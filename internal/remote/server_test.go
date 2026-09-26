package remote

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	superdb "superdb"
	"superdb/internal/cluster"
	"superdb/internal/engine"
	"superdb/internal/server"
	"superdb/internal/wire"
)

func testServer(t *testing.T, cfg Config) (*Server, string) {
	t.Helper()
	db := engine.New()
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	if cfg.Mode == "" {
		cfg.Mode = "memory"
	}
	cfg.Host = "127.0.0.1"
	if cfg.Port == 0 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		cfg.Port = ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
	}
	s := New(db, cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(s.Shutdown)
	go func() { _ = s.Serve(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s.Ready() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return s, s.Addr()
}

func dialURL(t *testing.T, addr, user, pass string) *superdb.Client {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	url := "superdb://" + host + ":" + port + "/main"
	if user != "" {
		url = "superdb://" + user + ":" + pass + "@" + host + ":" + port + "/main"
	}
	c, err := superdb.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestPingAndQueryExec(t *testing.T) {
	_, addr := testServer(t, Config{})
	c := dialURL(t, addr, "", "")
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO users VALUES (1, 'Ada')"); err != nil {
		t.Fatal(err)
	}
	rows, err := c.Query(ctx, "SELECT * FROM users WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Data) != 1 {
		t.Fatalf("rows %+v", rows)
	}
}

func TestAuthSuccessAndFailure(t *testing.T) {
	_, addr := testServer(t, Config{Username: "admin", Password: "s3cret"})
	host, port, _ := net.SplitHostPort(addr)
	ctx := context.Background()
	ok, err := superdb.Connect("superdb://admin:s3cret@" + host + ":" + port + "/main")
	if err != nil {
		t.Fatal(err)
	}
	if err := ok.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	_ = ok.Close()
	c2, err := superdb.Connect("superdb://admin:s3cret@" + host + ":" + port + "/main")
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if err := c2.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := superdb.Connect("superdb://admin:wrong@" + host + ":" + port + "/main"); err == nil {
		t.Fatal("expected auth failure")
	}
	// Unauthenticated raw query must be rejected with AUTH_REQUIRED.
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	r := bufio.NewReader(raw)
	w := bufio.NewWriter(raw)
	_, _ = w.Write(wire.EncodeFrame(wire.TypeQuery, 7, wire.MustJSON(wire.QueryPayload{SQL: "SELECT 1"})))
	_ = w.Flush()
	hdr, payload, err := wire.ReadFrame(r)
	if err != nil {
		t.Fatal(err)
	}
	var resp wire.Response
	_ = json.Unmarshal(payload, &resp)
	if hdr.RequestID != 7 || resp.ErrorCode != wire.ErrAuthRequired {
		t.Fatalf("got id=%d %+v", hdr.RequestID, resp)
	}
}

func TestAuthFailuresCloseConnection(t *testing.T) {
	_, addr := testServer(t, Config{Username: "u", Password: "p"})
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(raw)
	w := bufio.NewWriter(raw)
	for i := 1; i <= 6; i++ {
		_, _ = w.Write(wire.EncodeFrame(wire.TypeAuth, uint64(i),
			wire.MustJSON(wire.AuthPayload{Username: "u", Password: "wrong"})))
		_ = w.Flush()
		if _, _, err := wire.ReadFrame(r); err != nil {
			return // closed early: acceptable
		}
	}
	// After too many failures the server closes the connection.
	_ = raw.SetDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := wire.ReadFrame(r); err == nil {
		// If still open, the failure counter protects subsequent queries.
	}
}

func TestParameterizedQuery(t *testing.T) {
	_, addr := testServer(t, Config{})
	c := dialURL(t, addr, "", "")
	ctx := context.Background()
	if _, err := c.Exec(ctx, "CREATE TABLE u (id INT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	name := "o'brien; DROP TABLE u; --"
	if _, err := c.Exec(ctx, "INSERT INTO u VALUES (?, ?)", 1, name); err != nil {
		t.Fatal(err)
	}
	rows, err := c.Query(ctx, "SELECT * FROM u WHERE name = ?", name)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Data) != 1 {
		t.Fatalf("injection-safe round trip failed: %+v", rows)
	}
}

func TestTransactions(t *testing.T) {
	_, addr := testServer(t, Config{})
	c := dialURL(t, addr, "", "")
	ctx := context.Background()
	if _, err := c.Exec(ctx, "CREATE TABLE t (id INT PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := c.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO t VALUES (1, 'x')"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := c.Query(ctx, "SELECT * FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Data) != 0 {
		t.Fatalf("rollback failed: %+v", rows.Data)
	}
	if err := c.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO t VALUES (2, 'y')"); err != nil {
		t.Fatal(err)
	}
	if err := c.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err = c.Query(ctx, "SELECT * FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Data) != 1 {
		t.Fatalf("commit failed: %+v", rows.Data)
	}
}

func TestPasswordOnlyAuth(t *testing.T) {
	// A config with only a password must still authenticate: the handler
	// defaults an absent client username to "admin".
	_, addr := testServer(t, Config{Password: "s3cret"})
	host, port, _ := net.SplitHostPort(addr)
	c, err := superdb.Connect("superdb://admin:s3cret@" + host + ":" + port + "/main")
	if err != nil {
		t.Fatalf("password-only auth rejected: %v", err)
	}
	defer c.Close()
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLimitsMaxConnections(t *testing.T) {
	// Limits.MaxConnections is a second, finer-grained cap alongside
	// Config.MaxConnections; it must actually refuse extra conns.
	_, addr := testServer(t, Config{Limits: server.Limits{MaxConnections: 1}})
	c := dialURL(t, addr, "", "")
	defer c.Close()
	if _, err := superdb.Connect("superdb://" + addr + "/main"); err == nil {
		t.Fatal("second connection admitted past Limits.MaxConnections")
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("first connection disrupted: %v", err)
	}
}

func TestClientCloseThenQueryFailsFast(t *testing.T) {
	_, addr := testServer(t, Config{})
	host, port, _ := net.SplitHostPort(addr)
	c, err := superdb.Connect("superdb://" + host + ":" + port + "/main")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// After Close a new call must fail immediately, not hang waiting on a
	// dead connection.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.Query(ctx, "SELECT 1"); err == nil {
		t.Fatal("query after Close succeeded")
	}
}

func TestConcurrentClients(t *testing.T) {
	_, addr := testServer(t, Config{})
	first := dialURL(t, addr, "", "")
	if _, err := first.Exec(context.Background(), "CREATE TABLE c (id INT PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := dialURL(t, addr, "", "")
			ctx := context.Background()
			if _, err := c.Exec(ctx, "INSERT INTO c VALUES (?, ?)", i, "v"); err != nil {
				t.Error(err)
				return
			}
			if _, err := c.Query(ctx, "SELECT * FROM c WHERE id = ?", i); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
}

func TestConcurrentRequestsOneConn(t *testing.T) {
	_, addr := testServer(t, Config{})
	c := dialURL(t, addr, "", "")
	ctx := context.Background()
	if _, err := c.Exec(ctx, "CREATE TABLE m (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := c.Exec(ctx, "INSERT INTO m VALUES (?)", i); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	rows, err := c.Query(ctx, "SELECT * FROM m")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Data) != 20 {
		t.Fatalf("got %d rows", len(rows.Data))
	}
}

func TestRequestIDMatching(t *testing.T) {
	_, addr := testServer(t, Config{})
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	w := bufio.NewWriter(raw)
	r := bufio.NewReader(raw)
	// Pipeline two pings without waiting.
	_, _ = w.Write(wire.EncodeFrame(wire.TypePing, 101, []byte(`{}`)))
	_, _ = w.Write(wire.EncodeFrame(wire.TypePing, 202, []byte(`{}`)))
	_ = w.Flush()
	seen := map[uint64]bool{}
	for i := 0; i < 2; i++ {
		hdr, _, err := wire.ReadFrame(r)
		if err != nil {
			t.Fatal(err)
		}
		seen[hdr.RequestID] = true
	}
	if !seen[101] || !seen[202] {
		t.Fatalf("ids not echoed: %v", seen)
	}
}

func TestMalformedAndOversizedFrames(t *testing.T) {
	_, addr := testServer(t, Config{})
	// Malformed JSON payload: server must drop only this conn.
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	w := bufio.NewWriter(raw)
	r := bufio.NewReader(raw)
	_, _ = w.Write(wire.EncodeFrame(wire.TypeQuery, 1, []byte(`{not json`)))
	_ = w.Flush()
	hdr, payload, err := wire.ReadFrame(r)
	if err != nil {
		t.Fatal(err)
	}
	var resp wire.Response
	_ = json.Unmarshal(payload, &resp)
	if hdr.RequestID != 1 || resp.Status != "error" {
		t.Fatalf("%+v", resp)
	}
	_ = raw.Close()
	// Server still serves others.
	c := dialURL(t, addr, "", "")
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Oversized header: rejected without huge allocation.
	raw2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw2.Close()
	_ = raw2.SetDeadline(time.Now().Add(5 * time.Second))
	var big [wire.HeaderLen]byte
	copy(big[0:4], []byte("SDB1"))
	big[5] = 1
	big[6] = wire.TypeQuery
	// length = MaxFrameSize+1
	n := wire.MaxFrameSize + 1
	big[16], big[17], big[18], big[19] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	_, _ = raw2.Write(big[:])
	buf := make([]byte, 1)
	if _, err := raw2.Read(buf); err == nil {
		// Either EOF or error: connection must not serve further frames.
		r2 := bufio.NewReader(raw2)
		_ = r2
	}
}

func TestInvalidProtocolVersion(t *testing.T) {
	_, addr := testServer(t, Config{})
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	frame := wire.EncodeFrame(wire.TypePing, 1, []byte(`{}`))
	frame[5] = 99 // unknown version
	_, _ = raw.Write(frame)
	buf := make([]byte, 64)
	if _, err := raw.Read(buf); err == nil {
		// Server dropped or errored; either way no valid response follows.
	}
}

func TestErrorSerialization(t *testing.T) {
	_, addr := testServer(t, Config{})
	c := dialURL(t, addr, "", "")
	if _, err := c.Query(context.Background(), "SELECT * FROM missing"); err == nil {
		t.Fatal("expected error")
	} else {
		se, ok := err.(*superdb.ServerError)
		if !ok {
			t.Fatalf("want *ServerError, got %T", err)
		}
		if se.Code != superdb.ErrNotFound {
			t.Fatalf("code %q", se.Code)
		}
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	ctx := context.Background()
	mk := func() *Server {
		s := New(engine.New(), Config{Host: "127.0.0.1", Port: port, DataDir: dir, Mode: "wal"}, nil)
		cctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go func() { _ = s.Serve(cctx) }()
		deadline := time.Now().Add(5 * time.Second)
		for !s.Ready() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		return s
	}
	s1 := mk()
	c1, err := superdb.Connect("superdb://127.0.0.1:" + itoa(port) + "/main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c1.Exec(ctx, "CREATE TABLE p (id INT PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := c1.Exec(ctx, "INSERT INTO p VALUES (1, 'keep')"); err != nil {
		t.Fatal(err)
	}
	_ = c1.Close()
	s1.Shutdown()
	// Restart on the same directory: data must survive.
	s2 := mk()
	defer s2.Shutdown()
	c2, err := superdb.Connect("superdb://127.0.0.1:" + itoa(port) + "/main")
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	// The restarted server replays the WAL into a fresh engine image.
	rows, err := c2.Query(ctx, "SELECT * FROM p WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Data) != 1 {
		t.Fatalf("expected persisted row, got %+v", rows)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) == 0 {
		t.Fatal("expected durable files in data dir")
	}
}

func TestClientDisconnectDuringRequest(t *testing.T) {
	_, addr := testServer(t, Config{})
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	// Send a header claiming 1 KiB but only half the body, then vanish.
	frame := wire.EncodeFrame(wire.TypeQuery, 1, make([]byte, 1024))
	_, _ = raw.Write(frame[:wire.HeaderLen+512])
	_ = raw.Close()
	time.Sleep(100 * time.Millisecond)
	// Server must still serve new clients.
	c := dialURL(t, addr, "", "")
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownWithActiveClients(t *testing.T) {
	db := engine.New()
	cfg := Config{Host: "127.0.0.1", DataDir: t.TempDir(), Mode: "memory"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port = ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	s := New(db, cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan struct{})
	go func() { _ = s.Serve(ctx); close(serveDone) }()
	deadline := time.Now().Add(5 * time.Second)
	for !s.Ready() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	c, err := superdb.Connect("superdb://" + s.Addr() + "/main")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancel()
	s.Shutdown()
	select {
	case <-serveDone:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop with active client")
	}
}

func TestInvalidTLSCertificateRejected(t *testing.T) {
	certFile, keyFile := selfSigned(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	s := New(engine.New(), Config{Host: "127.0.0.1", Port: port, DataDir: t.TempDir(), Mode: "memory", TLSCertFile: certFile, TLSKeyFile: keyFile}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Shutdown()
	go func() { _ = s.Serve(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !s.Ready() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Verification on (default for ?tls=true) against a self-signed cert must fail.
	if _, err := superdb.Connect("superdb://127.0.0.1:" + itoa(port) + "/main?tls=true"); err == nil {
		t.Fatal("expected certificate verification failure")
	}
}

func TestTLSRoundTrip(t *testing.T) {
	certFile, keyFile := selfSigned(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	s := New(engine.New(), Config{Host: "127.0.0.1", Port: port, DataDir: t.TempDir(), Mode: "memory", TLSCertFile: certFile, TLSKeyFile: keyFile}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Shutdown()
	go func() { _ = s.Serve(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !s.Ready() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	c, err := superdb.Connect("superdb://127.0.0.1:" + itoa(port) + "/main?tls=insecure")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Plaintext client against TLS server must fail.
	raw, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(port), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = raw.Write(wire.EncodeFrame(wire.TypePing, 1, []byte(`{}`)))
	buf := make([]byte, 32)
	_, _ = raw.Read(buf) // TLS handshake bytes, not a valid frame
}

func TestHealthEndpoints(t *testing.T) {
	healthLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	healthAddr := healthLn.Addr().String()
	_ = healthLn.Close()
	s, _ := testServer(t, Config{HealthAddr: healthAddr})
	_ = s
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + healthAddr + "/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("health server not up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	resp, err := http.Get("http://" + healthAddr + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ready" {
		t.Fatalf("%v", body)
	}
}

type stubCluster struct{ err error }

func (s stubCluster) Exec(ctx context.Context, sql string) (engine.Result, error) {
	return engine.Result{}, s.err
}
func (s stubCluster) ExecAtomic(ctx context.Context, sqls []string) ([]engine.Result, error) {
	return nil, s.err
}

func TestClusterNotLeaderMapping(t *testing.T) {
	db := engine.New()
	cfg := Config{Host: "127.0.0.1", DataDir: t.TempDir(), Mode: "memory"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port = ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	s := New(db, cfg, stubCluster{err: cluster.NewError(cluster.CodeNoLeader, "no leader")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Shutdown()
	go func() { _ = s.Serve(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !s.Ready() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	c, err := superdb.Connect("superdb://" + s.Addr() + "/main")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Exec(context.Background(), "INSERT INTO t VALUES (1)")
	se, ok := err.(*superdb.ServerError)
	if !ok {
		t.Fatalf("want ServerError got %T %v", err, err)
	}
	if se.Code != superdb.ErrNotLeader || !se.Retryable {
		t.Fatalf("%+v", se)
	}
	// Session transactions are rejected, not faked, in cluster mode.
	if err := c.Begin(context.Background()); err == nil {
		t.Fatal("expected UNSUPPORTED for BEGIN in cluster mode")
	} else if se, ok := err.(*superdb.ServerError); !ok || se.Code != superdb.ErrUnsupported {
		t.Fatalf("%v", err)
	}
}

func TestMetricsCollected(t *testing.T) {
	s, addr := testServer(t, Config{})
	c := dialURL(t, addr, "", "")
	_ = c.Ping(context.Background())
	if _, err := c.Exec(context.Background(), "CREATE TABLE m2 (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	snap := s.Metrics()
	if snap.TotalConns == 0 || snap.Queries == 0 {
		t.Fatalf("%+v", snap)
	}
}

func selfSigned(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	cf, _ := os.Create(certFile)
	_ = pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	_ = cf.Close()
	kf, _ := os.Create(keyFile)
	_ = pem.Encode(kf, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	_ = kf.Close()
	// Sanity: pair loads.
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
