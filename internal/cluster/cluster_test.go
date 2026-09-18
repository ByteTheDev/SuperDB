package cluster

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"superdb/internal/engine"
	"superdb/internal/storage"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func testConfig(dataDir, listen string, seeds ...string) Config {
	return Config{
		DataDir:           dataDir,
		ListenAddr:        listen,
		AdvertiseAddr:     listen,
		JoinAddrs:         seeds,
		HeartbeatInterval: 50 * time.Millisecond,
		PingTimeout:       500 * time.Millisecond,
		SuspectAfter:      300 * time.Millisecond,
	}
}

func startNode(t *testing.T, cfg Config) *Node {
	t.Helper()
	n := New(cfg, engine.New())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Shutdown)
	return n
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting: %s", msg)
}

func TestSingleNodeStart(t *testing.T) {
	n := startNode(t, testConfig(t.TempDir(), freeAddr(t)))
	st := n.Status()
	if st.ClusterID == "" || st.NodeID == "" {
		t.Fatal("missing identity")
	}
	if st.Health != HealthHealthy {
		t.Fatalf("health=%s", st.Health)
	}
	if len(st.Ranges) != 1 {
		t.Fatalf("expected 1 range, got %d", len(st.Ranges))
	}
	r := st.Ranges[0]
	if r.StartKey != "" || r.EndKey != "" {
		t.Fatalf("single range must cover full keyspace: %+v", r)
	}
	if r.Leader != st.NodeID || !r.HasReplica(st.NodeID) {
		t.Fatalf("single range must be led by self: %+v", r)
	}
	if len(st.Peers) != 1 {
		t.Fatalf("expected 1 member, got %d", len(st.Peers))
	}
}

func TestRestartPreservesIdentity(t *testing.T) {
	dir := t.TempDir()
	addr := freeAddr(t)
	n := startNode(t, testConfig(dir, addr))
	first := n.Identity()
	n.Shutdown()

	n2 := New(testConfig(dir, freeAddr(t)), engine.New())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := n2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer n2.Shutdown()
	second := n2.Identity()
	if first.NodeID != second.NodeID || first.ClusterID != second.ClusterID {
		t.Fatalf("identity changed: %+v -> %+v", first, second)
	}
}

func TestThreeNodeJoin(t *testing.T) {
	a1 := freeAddr(t)
	n1 := startNode(t, testConfig(t.TempDir(), a1))
	cid := n1.Identity().ClusterID

	n2 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))
	n3 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))

	if n2.Identity().ClusterID != cid || n3.Identity().ClusterID != cid {
		t.Fatal("joining nodes must adopt the seed cluster id")
	}
	waitFor(t, 5*time.Second, func() bool {
		return n1.Members().Count() == 3 && n2.Members().Count() == 3 && n3.Members().Count() == 3
	}, "every node sees 3 members")
}

func TestHealthChecks(t *testing.T) {
	a1 := freeAddr(t)
	n1 := startNode(t, testConfig(t.TempDir(), a1))
	n2 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))
	waitFor(t, 5*time.Second, func() bool { return n1.Members().Count() == 2 }, "join visible")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reply, err := n1.Ping(ctx, n2.AdvertiseAddr())
	if err != nil {
		t.Fatal(err)
	}
	if reply.NodeID != n2.Identity().NodeID || reply.ClusterID != n2.Identity().ClusterID {
		t.Fatalf("bad ping reply: %+v", reply)
	}
}

func TestRouteLocal(t *testing.T) {
	n := startNode(t, testConfig(t.TempDir(), freeAddr(t)))
	ctx := context.Background()
	if _, err := n.Exec(ctx, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Exec(ctx, "INSERT INTO users VALUES (1, 'Ada')"); err != nil {
		t.Fatal(err)
	}
	res, err := n.Exec(ctx, "SELECT * FROM users WHERE id = 1")
	if err != nil || len(res.Rows) != 1 || res.Rows[0][1] != "Ada" {
		t.Fatalf("local route mismatch: %+v err=%v", res, err)
	}
}

func TestForwardToRemote(t *testing.T) {
	a1 := freeAddr(t)
	n1 := startNode(t, testConfig(t.TempDir(), a1))
	n2 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))
	waitFor(t, 5*time.Second, func() bool { return n1.Members().Count() == 2 }, "join visible")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Remote execution over the internal transport (honest forwarding,
	// not fake replication): run DDL+write on n1 via n2's transport.
	if _, err := n2.Forward(ctx, a1, "CREATE TABLE kv (id INT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := n2.Forward(ctx, a1, "INSERT INTO kv VALUES (7, 'remote')"); err != nil {
		t.Fatal(err)
	}
	got, err := n1.DB().Exec("SELECT * FROM kv WHERE id = 7")
	if err != nil || len(got.Rows) != 1 || got.Rows[0][1] != "remote" {
		t.Fatalf("forwarded write missing on remote: %+v err=%v", got, err)
	}
}

func TestRouterMarksRemote(t *testing.T) {
	m := NewMembership("local", "127.0.0.1:1")
	m.Upsert("local", "127.0.0.1:1", StateAlive, 1)
	m.Upsert("peer", "127.0.0.1:2", StateAlive, 1)
	rs := NewRangeStore()
	rs.ReplaceBulk([]Range{{ID: 1, StartKey: "", EndKey: "", Replicas: []string{"local", "peer"}, Leader: "peer", Generation: 2}})
	r := NewRouter(rs, m, "local")
	route, err := r.RouteTable("users")
	if err != nil {
		t.Fatal(err)
	}
	if route.Local {
		t.Fatal("leader on peer must route remote")
	}
	if route.Addr != "127.0.0.1:2" {
		t.Fatalf("expected peer addr, got %q", route.Addr)
	}
}

func TestUnreachableThenRecover(t *testing.T) {
	a1 := freeAddr(t)
	n1 := startNode(t, testConfig(t.TempDir(), a1))
	dir2 := t.TempDir()
	a2 := freeAddr(t)
	n2 := startNode(t, testConfig(dir2, a2, a1))
	waitFor(t, 5*time.Second, func() bool { return n1.Members().Count() == 2 }, "join visible")

	// Simulate an unclean crash: stop n2 with no leave announcement.
	n2.Kill()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := n1.Ping(ctx, a2); err == nil {
		t.Fatal("expected ping to fail while peer is down")
	} else if ce, ok := AsError(err); !ok || !ce.Retryable {
		t.Fatalf("unreachable error must be retryable: %v", err)
	}
	n1.Members().MarkSuspect(n2.Identity().NodeID)
	if mb, ok := n1.Members().Get(n2.Identity().NodeID); !ok || mb.State != StateSuspect {
		t.Fatalf("member should be suspect, not deleted: %+v", mb)
	}
	if n1.Members().Count() != 2 {
		t.Fatal("suspect members must remain listed; never delete on transient failure")
	}

	// Recover: restart n2 with the same identity, ping must succeed.
	n2r := New(testConfig(dir2, a2), engine.New())
	cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ccancel()
	if err := n2r.Start(cctx); err != nil {
		t.Fatal(err)
	}
	defer n2r.Shutdown()
	waitFor(t, 5*time.Second, func() bool {
		_, err := n1.Ping(ctx, a2)
		return err == nil
	}, "ping after recovery")
	if mb, ok := n1.Members().Get(n2.Identity().NodeID); !ok || mb.State != StateAlive {
		t.Fatalf("member should recover to alive: %+v", mb)
	}
}

func TestGracefulShutdown(t *testing.T) {
	n := New(testConfig(t.TempDir(), freeAddr(t)), engine.New())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	n.Shutdown()
	n.Shutdown() // must be idempotent
	if got := n.Status().Health; got != HealthShutdown {
		t.Fatalf("health=%s", got)
	}
}

func TestCorruptMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, identityFileName), []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	n := New(testConfig(dir, freeAddr(t)), engine.New())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := n.Start(ctx)
	if err == nil {
		n.Shutdown()
		t.Fatal("expected corrupt metadata error")
	}
	if ce, ok := AsError(err); !ok || ce.Code != CodeCorruptMetadata {
		t.Fatalf("expected corrupt_metadata, got %v", err)
	}
}

func TestQuorumWritesRejected(t *testing.T) {
	n := startNode(t, testConfig(t.TempDir(), freeAddr(t)))
	err := n.repl.Replicate(context.Background(), 1, "SELECT 1", WriteConcern{RequiredAcks: 2})
	if ce, ok := AsError(err); !ok || ce.Code != CodeNoQuorum {
		t.Fatalf("quorum write must be explicitly rejected: %v", err)
	}
}

func TestConcurrentRequests(t *testing.T) {
	n := startNode(t, testConfig(t.TempDir(), freeAddr(t)))
	ctx := context.Background()
	if _, err := n.Exec(ctx, "CREATE TABLE c (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := n.Exec(ctx, "SELECT * FROM c")
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := n.StatsSnapshot().Requests; got < 64 {
		t.Fatalf("requests=%d", got)
	}
}

func TestWALRecoveryUnaffected(t *testing.T) {
	dir := t.TempDir()
	n := startNode(t, testConfig(dir, freeAddr(t)))
	ctx := context.Background()
	if _, err := n.Exec(ctx, "CREATE TABLE w (id INT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Exec(ctx, "INSERT INTO w VALUES (1, 'Ada')"); err != nil {
		t.Fatal(err)
	}
	// The pre-existing WAL format replays beside cluster.json untouched.
	walDir := t.TempDir()
	if err := storage.AppendWALBatch(storage.WALPath(walDir), []string{
		"CREATE TABLE w (id INT PRIMARY KEY, name TEXT)",
		"INSERT INTO w VALUES (2, 'Grace')",
	}); err != nil {
		t.Fatal(err)
	}
	fresh := engine.New()
	if err := storage.ReplayWAL(storage.WALPath(walDir), func(sql string) error {
		_, err := fresh.Exec(sql)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res, err := fresh.Exec("SELECT * FROM w WHERE id = 2")
	if err != nil || len(res.Rows) != 1 || res.Rows[0][1] != "Grace" {
		t.Fatalf("WAL replay mismatch: %+v err=%v", res, err)
	}
	// Cluster identity coexists with engine data in the same directory.
	if _, err := os.Stat(filepath.Join(dir, identityFileName)); err != nil {
		t.Fatalf("cluster identity missing: %v", err)
	}
}

func TestObservability(t *testing.T) {
	n := startNode(t, testConfig(t.TempDir(), freeAddr(t)))
	ctx := context.Background()
	_, _ = n.Exec(ctx, "CREATE TABLE o (id INT PRIMARY KEY)")
	_, _ = n.Exec(ctx, "SELECT * FROM o")
	st := n.Status()
	if st.Stats.Requests < 2 || st.Stats.Writes < 1 || st.Stats.Reads < 1 {
		t.Fatalf("bad counters: %+v", st.Stats)
	}
	if st.UptimeSec < 0 || st.NodeID == "" || st.ClusterID == "" {
		t.Fatalf("bad status: %+v", st)
	}
}
