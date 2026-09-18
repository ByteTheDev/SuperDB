package cluster

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"
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
		PingTimeout:       2 * time.Second,
		SuspectAfter:      300 * time.Millisecond,
		Raft:              RaftTimeouts{Heartbeat: 50 * time.Millisecond, Election: 300 * time.Millisecond, Commit: 20 * time.Millisecond, SnapshotInterval: 5 * time.Second},
		SnapshotThreshold: 1 << 30,
	}
}

func startNode(t *testing.T, cfg Config) *Node {
	t.Helper()
	n := New(cfg, engine.New())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

func waitLeader(t *testing.T, nodes ...*Node) *Node {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			if n.IsLeader() {
				return n
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timeout waiting for leader")
	return nil
}

func countRows(t *testing.T, n *Node, sql string) int {
	t.Helper()
	res, err := n.DB().Exec(sql)
	if err != nil {
		t.Fatal(err)
	}
	return len(res.Rows)
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
	if len(st.Peers) != 1 {
		t.Fatalf("expected 1 member, got %d", len(st.Peers))
	}
	waitLeader(t, n)
	if st := n.Status(); st.Raft.Leader != n.Identity().NodeID {
		t.Fatalf("single node must lead itself: %+v", st.Raft)
	}
}

func TestRestartPreservesIdentity(t *testing.T) {
	dir := t.TempDir()
	n := startNode(t, testConfig(dir, freeAddr(t)))
	first := n.Identity()
	n.Shutdown()

	n2 := New(testConfig(dir, freeAddr(t)), engine.New())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

func TestRestartRestoresData(t *testing.T) {
	dir := t.TempDir()
	addr := freeAddr(t)
	n := startNode(t, testConfig(dir, addr))
	ctx := context.Background()
	if _, err := n.Exec(ctx, "CREATE TABLE keep (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Exec(ctx, "INSERT INTO keep VALUES (1, 41), (2, 42)"); err != nil {
		t.Fatal(err)
	}
	n.Shutdown()

	n2 := New(testConfig(dir, freeAddr(t)), engine.New())
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := n2.Start(cctx); err != nil {
		t.Fatal(err)
	}
	defer n2.Shutdown()
	res, err := n2.DB().Exec("SELECT * FROM keep")
	if err != nil || len(res.Rows) != 2 {
		t.Fatalf("data lost across restart: %+v err=%v", res, err)
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
	waitFor(t, 10*time.Second, func() bool {
		return n1.Members().Count() == 3 && n2.Members().Count() == 3 && n3.Members().Count() == 3
	}, "every node sees 3 members")
	waitFor(t, 10*time.Second, func() bool {
		for _, n := range []*Node{n1, n2, n3} {
			cfg, err := n.rn.Configuration()
			if err != nil {
				return false
			}
			voters := 0
			for _, s := range cfg.Servers {
				if string(s.ID) != "" && s.Suffrage == raft.Voter {
					voters++
				}
			}
			if voters != 3 {
				return false
			}
		}
		return true
	}, "raft configuration holds 3 voters")
}

func TestHealthChecks(t *testing.T) {
	a1 := freeAddr(t)
	n1 := startNode(t, testConfig(t.TempDir(), a1))
	n2 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))
	waitFor(t, 10*time.Second, func() bool { return n1.Members().Count() == 2 }, "join visible")
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
	waitLeader(t, n)
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

func TestReplicatedWriteConverges(t *testing.T) {
	a1 := freeAddr(t)
	n1 := startNode(t, testConfig(t.TempDir(), a1))
	n2 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))
	n3 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))
	waitLeader(t, n1, n2, n3)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Write through a follower: it must forward to the leader and the entry
	// must commit on a quorum before acknowledgement.
	var writer *Node
	for _, n := range []*Node{n1, n2, n3} {
		if !n.IsLeader() {
			writer = n
			break
		}
	}
	if _, err := writer.Exec(ctx, "CREATE TABLE kv (id INT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(ctx, "INSERT INTO kv VALUES (7, 'quorum')"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		for _, n := range []*Node{n1, n2, n3} {
			res, err := n.DB().Exec("SELECT * FROM kv WHERE id = 7")
			if err != nil || len(res.Rows) != 1 {
				return false
			}
		}
		return true
	}, "write visible on all replicas")
}

func TestFailoverContinuesServing(t *testing.T) {
	a1 := freeAddr(t)
	n1 := startNode(t, testConfig(t.TempDir(), a1))
	n2 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))
	n3 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))
	leader := waitLeader(t, n1, n2, n3)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := leader.Exec(ctx, "CREATE TABLE f (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := leader.Exec(ctx, "INSERT INTO f VALUES (1, 1)"); err != nil {
		t.Fatal(err)
	}
	leader.Kill()

	var survivors []*Node
	for _, n := range []*Node{n1, n2, n3} {
		if n != leader {
			survivors = append(survivors, n)
		}
	}
	newLeader := waitLeader(t, survivors...)
	if _, err := newLeader.Exec(ctx, "INSERT INTO f VALUES (2, 2)"); err != nil {
		t.Fatalf("write after failover failed: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		for _, n := range survivors {
			if countRows(t, n, "SELECT * FROM f") != 2 {
				return false
			}
		}
		return true
	}, "post-failover write on survivors")
}

func TestAtomicBatchRollback(t *testing.T) {
	n := startNode(t, testConfig(t.TempDir(), freeAddr(t)))
	waitLeader(t, n)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := n.Exec(ctx, "CREATE TABLE a (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatal(err)
	}
	_, err := n.ExecAtomic(ctx, []string{
		"INSERT INTO a VALUES (1, 1)",
		"INSERT INTO a VALUES (1, 2)",
	})
	if err == nil {
		t.Fatal("expected atomic batch to fail on duplicate key")
	}
	if got := countRows(t, n, "SELECT * FROM a"); got != 0 {
		t.Fatalf("atomic batch partially applied: %d rows", got)
	}
	if _, err := n.ExecAtomic(ctx, []string{
		"INSERT INTO a VALUES (1, 1)",
		"INSERT INTO a VALUES (2, 2)",
	}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, n, "SELECT * FROM a"); got != 2 {
		t.Fatalf("atomic batch not applied: %d rows", got)
	}
}

func TestSplitAndAssign(t *testing.T) {
	n := startNode(t, testConfig(t.TempDir(), freeAddr(t)))
	waitLeader(t, n)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := n.Exec(ctx, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Exec(ctx, "CREATE TABLE orders (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatal(err)
	}
	left, right, err := n.SplitRange(ctx, 1, "m", []string{"users"})
	if err != nil {
		t.Fatal(err)
	}
	if left.ID == right.ID || left.EndKey != "m" || right.StartKey != "m" {
		t.Fatalf("bad split: %+v %+v", left, right)
	}
	if err := n.AssignTable(ctx, "orders", right.ID); err != nil {
		t.Fatal(err)
	}
	route, err := n.Router().RouteTable("users")
	if err != nil || route.Range.ID != left.ID {
		t.Fatalf("users must route to left range: %+v err=%v", route, err)
	}
	route, err = n.Router().RouteTable("orders")
	if err != nil || route.Range.ID != right.ID {
		t.Fatalf("orders must route to right range: %+v err=%v", route, err)
	}
	// Writes still commit after the split.
	if _, err := n.Exec(ctx, "INSERT INTO users VALUES (1, 'Ada')"); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, n, "SELECT * FROM users"); got != 1 {
		t.Fatalf("post-split write missing: %d", got)
	}
}

func TestRebalanceConvergesDirectory(t *testing.T) {
	a1 := freeAddr(t)
	n1 := startNode(t, testConfig(t.TempDir(), a1))
	n2 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))
	n3 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))
	leader := waitLeader(t, n1, n2, n3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := leader.Rebalance(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		for _, n := range []*Node{n1, n2, n3} {
			r, err := n.Ranges().ByID(1)
			if err != nil || len(r.Replicas) != 3 {
				return false
			}
			lid, _ := n.rn.Leader()
			if r.Leader != lid {
				return false
			}
		}
		return true
	}, "directory converged on voters+leader")
}

func TestMoveLeader(t *testing.T) {
	a1 := freeAddr(t)
	n1 := startNode(t, testConfig(t.TempDir(), a1))
	n2 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))
	// Wait for leadership to settle after join churn.
	var first *Node
	stable := 0
	waitFor(t, 15*time.Second, func() bool {
		l := waitLeaderOnce(n1, n2)
		if l == nil || l != first {
			first, stable = l, 0
			return false
		}
		stable++
		return stable >= 5
	}, "stable leader")
	time.Sleep(500 * time.Millisecond)
	if l := waitLeaderOnce(n1, n2); l != first {
		t.Fatal("leader unstable before transfer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := first.MoveLeader(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, func() bool {
		l := waitLeaderOnce(n1, n2)
		return l != nil && l != first
	}, "leadership moved")
}

func waitLeaderOnce(nodes ...*Node) *Node {
	for _, n := range nodes {
		if n.IsLeader() {
			return n
		}
	}
	return nil
}

func TestMoveRange(t *testing.T) {
	a1 := freeAddr(t)
	n1 := startNode(t, testConfig(t.TempDir(), a1))
	n2 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))
	waitLeader(t, n1, n2)
	before, err := n1.Ranges().ByID(1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	moved, err := n1.MoveRange(ctx, 1, []string{n2.Identity().NodeID})
	if err != nil {
		t.Fatal(err)
	}
	if moved.Generation != before.Generation+1 {
		t.Fatalf("move must bump generation: %+v", moved)
	}
	if len(moved.Replicas) != 1 || moved.Replicas[0] != n2.Identity().NodeID {
		t.Fatalf("bad replicas: %+v", moved)
	}
}

func TestQuorumConcernEnforced(t *testing.T) {
	n := startNode(t, testConfig(t.TempDir(), freeAddr(t)))
	waitLeader(t, n)
	// Single voter: concern of 2 is unsatisfiable and must fail loudly.
	err := n.repl.Replicate(context.Background(), 1, "SELECT 1", WriteConcern{RequiredAcks: 2})
	if ce, ok := AsError(err); !ok || ce.Code != CodeNoQuorum {
		t.Fatalf("expected no_quorum, got %v", err)
	}
}

func TestQuorumWriteAck(t *testing.T) {
	a1 := freeAddr(t)
	n1 := startNode(t, testConfig(t.TempDir(), a1))
	n2 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))
	n3 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))
	waitLeader(t, n1, n2, n3)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := n1.Exec(ctx, "CREATE TABLE q (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	// Concern 2 on 3 voters: ack implies a second replica durably stored it.
	if err := n1.repl.Replicate(ctx, 1, "INSERT INTO q VALUES (9)", WriteConcern{RequiredAcks: 2}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		seen := 0
		for _, n := range []*Node{n1, n2, n3} {
			if countRows(t, n, "SELECT * FROM q WHERE id = 9") == 1 {
				seen++
			}
		}
		return seen >= 2
	}, "write on a quorum of replicas")
}

func TestForwardToRemote(t *testing.T) {
	a1 := freeAddr(t)
	n1 := startNode(t, testConfig(t.TempDir(), a1))
	n2 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))
	waitFor(t, 10*time.Second, func() bool { return n1.Members().Count() == 2 }, "join visible")
	waitLeader(t, n1, n2)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := n2.Forward(ctx, a1, "CREATE TABLE kv (id INT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := n2.Forward(ctx, a1, "INSERT INTO kv VALUES (7, 'remote')"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		got, err := n1.DB().Exec("SELECT * FROM kv WHERE id = 7")
		return err == nil && len(got.Rows) == 1
	}, "forwarded write committed")
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
	cfg2 := testConfig(dir2, a2, a1)
	n2 := startNode(t, cfg2)
	waitFor(t, 10*time.Second, func() bool { return n1.Members().Count() == 2 }, "join visible")

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

	n2r := New(testConfig(dir2, a2, a1), engine.New())
	cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ccancel()
	if err := n2r.Start(cctx); err != nil {
		t.Fatal(err)
	}
	defer n2r.Shutdown()
	waitFor(t, 10*time.Second, func() bool {
		_, err := n1.Ping(ctx, a2)
		return err == nil
	}, "ping after recovery")
	if mb, ok := n1.Members().Get(n2.Identity().NodeID); !ok || mb.State != StateAlive {
		t.Fatalf("member should recover to alive: %+v", mb)
	}
}

func TestGracefulShutdown(t *testing.T) {
	n := New(testConfig(t.TempDir(), freeAddr(t)), engine.New())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	n.Shutdown()
	n.Shutdown()
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

func TestConsistentRead(t *testing.T) {
	a1 := freeAddr(t)
	n1 := startNode(t, testConfig(t.TempDir(), a1))
	n2 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))
	leader := waitLeader(t, n1, n2)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := leader.Exec(ctx, "CREATE TABLE cr (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := leader.Exec(ctx, "INSERT INTO cr VALUES (1, 100)"); err != nil {
		t.Fatal(err)
	}
	var follower *Node
	for _, n := range []*Node{n1, n2} {
		if n != leader {
			follower = n
		}
	}
	res, err := follower.ExecConsistent(ctx, "SELECT * FROM cr WHERE id = 1")
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("consistent read failed: %+v err=%v", res, err)
	}
}

func TestRegionPlacement(t *testing.T) {
	a1 := freeAddr(t)
	c1 := testConfig(t.TempDir(), a1)
	c1.Region = "east"
	n1 := startNode(t, c1)
	c2 := testConfig(t.TempDir(), freeAddr(t), a1)
	c2.Region = "east"
	n2 := startNode(t, c2)
	c3 := testConfig(t.TempDir(), freeAddr(t), a1)
	c3.Region = "west"
	n3 := startNode(t, c3)
	waitLeader(t, n1, n2, n3)
	waitFor(t, 10*time.Second, func() bool { return n1.Members().Count() == 3 }, "members visible")
	p := n1.Placement()
	if p.ByRegion["east"] != 2 || p.ByRegion["west"] != 1 {
		t.Fatalf("bad region spread: %+v", p)
	}
	if st := n3.Status(); st.Region != "west" {
		t.Fatalf("region missing from status: %+v", st.Region)
	}
}

func TestSnapshotRestore(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir, freeAddr(t))
	cfg.SnapshotThreshold = 8
	cfg.Raft.SnapshotInterval = 300 * time.Millisecond
	n := startNode(t, cfg)
	waitLeader(t, n)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := n.Exec(ctx, "CREATE TABLE s (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Exec(ctx, "INSERT INTO s VALUES (1, 1)"); err != nil {
		t.Fatal(err)
	}
	// Committed-but-rejected entries still advance the log past the snapshot
	// threshold, exercising snapshot + restore.
	for i := 0; i < 20; i++ {
		_, _ = n.Exec(ctx, "INSERT INTO s VALUES (1, 1)")
	}
	waitFor(t, 10*time.Second, func() bool {
		entries, err := os.ReadDir(filepath.Join(dir, "raft", "snapshots"))
		return err == nil && len(entries) > 0
	}, "raft snapshot written")
	n.Shutdown()
	// State must survive a snapshot restore + log replay.
	n2 := New(testConfig(dir, freeAddr(t)), engine.New())
	cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ccancel()
	if err := n2.Start(cctx); err != nil {
		t.Fatal(err)
	}
	defer n2.Shutdown()
	if got := countRows(t, n2, "SELECT * FROM s"); got != 1 {
		t.Fatalf("snapshot restore lost data: %d", got)
	}
}

func TestConcurrentRequests(t *testing.T) {
	n := startNode(t, testConfig(t.TempDir(), freeAddr(t)))
	waitLeader(t, n)
	ctx := context.Background()
	if _, err := n.Exec(ctx, "CREATE TABLE c (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := n.Exec(ctx, "SELECT * FROM c"); err != nil {
				errs <- err
			}
		}()
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
	waitLeader(t, n)
	ctx := context.Background()
	if _, err := n.Exec(ctx, "CREATE TABLE w (id INT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Exec(ctx, "INSERT INTO w VALUES (1, 'Ada')"); err != nil {
		t.Fatal(err)
	}
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
	if _, err := os.Stat(filepath.Join(dir, identityFileName)); err != nil {
		t.Fatalf("cluster identity missing: %v", err)
	}
}

func TestObservability(t *testing.T) {
	n := startNode(t, testConfig(t.TempDir(), freeAddr(t)))
	waitLeader(t, n)
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
	if st.Raft.State == "" || st.Raft.Leader == "" {
		t.Fatalf("raft status missing: %+v", st.Raft)
	}
}
