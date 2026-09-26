package remote

import (
	"context"
	"testing"
	"time"

	superdb "superdb"
	"superdb/internal/engine"
	"superdb/internal/server"
)

func serverErrorCode(t *testing.T, err error) string {
	t.Helper()
	se, ok := err.(*superdb.ServerError)
	if !ok || se == nil {
		t.Fatalf("expected *ServerError, got %T %v", err, err)
	}
	return se.Code
}

func TestRemoteQueryTimeout(t *testing.T) {
	_, addr := testServer(t, Config{Limits: server.Limits{QueryTimeout: -1}})
	c := dialURL(t, addr, "", "")
	_, err := c.Query(context.Background(), "SELECT * FROM t")
	if code := serverErrorCode(t, err); code != superdb.ErrTimeout {
		t.Fatalf("expected TIMEOUT, got %q (%v)", code, err)
	}
}

func TestRemoteMaxResultRows(t *testing.T) {
	_, addr := testServer(t, Config{Limits: server.Limits{MaxResultRows: 2}})
	c := dialURL(t, addr, "", "")
	ctx := context.Background()
	if _, err := c.Exec(ctx, "CREATE TABLE t (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO t VALUES (1), (2), (3)"); err != nil {
		t.Fatal(err)
	}
	_, err := c.Query(ctx, "SELECT * FROM t")
	if code := serverErrorCode(t, err); code != superdb.ErrResultTooLarge {
		t.Fatalf("expected RESULT_TOO_LARGE, got %q (%v)", code, err)
	}
	rows, err := c.Query(ctx, "SELECT * FROM t LIMIT 2")
	if err != nil || len(rows.Data) != 2 {
		t.Fatalf("at-cap query must succeed: %+v err=%v", rows, err)
	}
}

func TestRemoteMaxResultBytes(t *testing.T) {
	_, addr := testServer(t, Config{Limits: server.Limits{MaxResultBytes: 96}})
	c := dialURL(t, addr, "", "")
	ctx := context.Background()
	if _, err := c.Exec(ctx, "CREATE TABLE t (id INT PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO t VALUES (1, 'a-longish-value-that-helps-exceed-the-cap')"); err != nil {
		t.Fatal(err)
	}
	_, err := c.Query(ctx, "SELECT * FROM t")
	if code := serverErrorCode(t, err); code != superdb.ErrResultTooLarge {
		t.Fatalf("expected RESULT_TOO_LARGE, got %q (%v)", code, err)
	}
}

func TestRemoteInflightBackpressure(t *testing.T) {
	s, addr := testServer(t, Config{Limits: server.Limits{
		MaxInflightQueries: 1,
		AcquireTimeout:     20 * time.Millisecond,
	}})
	c := dialURL(t, addr, "", "")
	// Occupy the single inflight slot so the client's query is rejected.
	if err := s.limiter.AcquireQuery(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := c.Exec(context.Background(), "CREATE TABLE t (id INT PRIMARY KEY)")
	if code := serverErrorCode(t, err); code != superdb.ErrBusy {
		t.Fatalf("expected SERVER_BUSY, got %q (%v)", code, err)
	}
	s.limiter.ReleaseQuery()
	if got := s.Metrics().RejectedQueries; got != 1 {
		t.Fatalf("RejectedQueries=%d, want 1", got)
	}
	// After release the server works normally again.
	if _, err := c.Exec(context.Background(), "CREATE TABLE t (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteMaxTxStatements(t *testing.T) {
	_, addr := testServer(t, Config{Limits: server.Limits{MaxTxStatements: 1}})
	c := dialURL(t, addr, "", "")
	ctx := context.Background()
	if _, err := c.Exec(ctx, "CREATE TABLE t (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := c.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO t VALUES (2)"); err == nil {
		t.Fatal("second buffered statement must abort the transaction")
	}
	if err := c.Commit(ctx); err == nil {
		t.Fatal("aborted transaction must not commit")
	}
}

func TestRemoteMaxTxDatabaseRows(t *testing.T) {
	_, addr := testServer(t, Config{Limits: server.Limits{MaxTxDatabaseRows: 1}})
	c := dialURL(t, addr, "", "")
	ctx := context.Background()
	if _, err := c.Exec(ctx, "CREATE TABLE t (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO t VALUES (1), (2)"); err != nil {
		t.Fatal(err)
	}
	err := c.Begin(ctx)
	if code := serverErrorCode(t, err); code != superdb.ErrBusy {
		t.Fatalf("expected SERVER_BUSY for oversized tx, got %q (%v)", code, err)
	}
}

func TestLoadConfigLimitEnv(t *testing.T) {
	t.Setenv("SUPERDB_QUERY_TIMEOUT", "2s")
	t.Setenv("SUPERDB_MAX_RESULT_ROWS", "1000")
	t.Setenv("SUPERDB_MAX_INFLIGHT_QUERIES", "8")
	cfg := LoadConfig(Config{})
	if cfg.Limits.QueryTimeout != 2*time.Second || cfg.Limits.MaxResultRows != 1000 || cfg.Limits.MaxInflightQueries != 8 {
		t.Fatalf("env limits not applied: %+v", cfg.Limits)
	}
	// Explicit config wins over env.
	cfg = LoadConfig(Config{Limits: server.Limits{MaxResultRows: 5}})
	if cfg.Limits.MaxResultRows != 5 || cfg.Limits.MaxInflightQueries != 8 {
		t.Fatalf("explicit limit must override env: %+v", cfg.Limits)
	}
}

type contextCheckingCluster struct {
	deadline bool
}

func (c *contextCheckingCluster) Exec(ctx context.Context, _ string) (engine.Result, error) {
	_, c.deadline = ctx.Deadline()
	return engine.Result{}, nil
}

func (*contextCheckingCluster) ExecAtomic(context.Context, []string) ([]engine.Result, error) {
	return nil, nil
}

func TestClusterQueryTimeoutDisabledByDefault(t *testing.T) {
	cluster := &contextCheckingCluster{}
	s := &session{cluster: cluster}
	if _, _, err := s.execOne(context.Background(), "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if cluster.deadline {
		t.Fatal("cluster query got an implicit timeout while QueryTimeout is disabled")
	}
}
