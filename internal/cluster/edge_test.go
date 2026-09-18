package cluster

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"superdb/internal/engine"
)

func newEngineForDebug() *engine.Database { return engine.New() }

// Poison entries (valid log, failing SQL) must surface errors without
// diverging replicas or wedging the log: later writes still commit.
func TestPoisonEntryDoesNotDiverge(t *testing.T) {
	a1 := freeAddr(t)
	n1 := startNode(t, testConfig(t.TempDir(), a1))
	n2 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))
	n3 := startNode(t, testConfig(t.TempDir(), freeAddr(t), a1))
	waitLeader(t, n1, n2, n3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := n1.Exec(ctx, "CREATE TABLE p (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := n2.Exec(ctx, "INSERT INTO p VALUES ('not-an-int')"); err == nil {
		t.Fatal("expected type error")
	}
	if _, err := n3.Exec(ctx, "INSERT INTO p VALUES (1)"); err != nil {
		t.Fatalf("log wedged after poison entry: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		for _, n := range []*Node{n1, n2, n3} {
			res, err := n.DB().Exec("SELECT * FROM p")
			if err != nil || len(res.Rows) != 1 {
				return false
			}
		}
		return true
	}, "replicas converged after poison entry")
}

// Empty atomic batches are rejected before touching the log.
func TestEmptyAtomicRejected(t *testing.T) {
	n := startNode(t, testConfig(t.TempDir(), freeAddr(t)))
	waitLeader(t, n)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := n.ExecAtomic(ctx, nil); err == nil {
		t.Fatal("expected empty batch error")
	} else if ce, ok := AsError(err); !ok || ce.Code != CodeInvalidArgument {
		t.Fatalf("expected invalid_argument, got %v", err)
	}
}

// Two restarts in a row must preserve data and identity.
func TestDoubleRestart(t *testing.T) {
	dir := t.TempDir()
	n := startNode(t, testConfig(dir, freeAddr(t)))
	waitLeader(t, n)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := n.Exec(ctx, "CREATE TABLE d (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Exec(ctx, "INSERT INTO d VALUES (5)"); err != nil {
		t.Fatal(err)
	}
	want := n.Identity()
	n.Shutdown()
	for i := 0; i < 2; i++ {
		r := New(testConfig(dir, freeAddr(t)), newEngineForDebug())
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := r.Start(cctx)
		ccancel()
		if err != nil {
			t.Fatalf("restart %d failed: %v", i, err)
		}
		if got := r.Identity(); got != want {
			r.Shutdown()
			t.Fatalf("identity changed on restart %d: want %+v got %+v", i, want, got)
		}
		res, err := r.DB().Exec("SELECT * FROM d")
		if err != nil || len(res.Rows) != 1 {
			r.Shutdown()
			t.Fatalf("data lost on restart %d: %+v err=%v", i, res, err)
		}
		r.Shutdown()
	}
}

// Kill is idempotent and safe to mix with Shutdown.
func TestKillShutdownIdempotent(t *testing.T) {
	n := startNode(t, testConfig(t.TempDir(), freeAddr(t)))
	n.Kill()
	n.Kill()
	n.Shutdown()
	if got := n.Status().Health; got != HealthShutdown {
		t.Fatalf("health=%s", got)
	}
}

// A corrupt Raft store must fail fast with a structured error, never hang.
func TestCorruptRaftStoreFailsFast(t *testing.T) {
	dir := t.TempDir()
	n := startNode(t, testConfig(dir, freeAddr(t)))
	waitLeader(t, n)
	n.Shutdown()
	if err := os.WriteFile(filepath.Join(dir, "raft", "raft.db"), []byte("garbage-not-bolt"), 0644); err != nil {
		t.Fatal(err)
	}
	r := New(testConfig(dir, freeAddr(t)), newEngineForDebug())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			r.Shutdown()
			t.Fatal("expected corrupt store error")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Start hung on corrupt store")
	}
}
