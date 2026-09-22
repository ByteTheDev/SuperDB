package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"superdb/internal/engine"
)

func TestLimiterConnCap(t *testing.T) {
	lim := NewLimiter(Limits{MaxConnections: 1})
	if !lim.AcquireConn() {
		t.Fatal("first connection must be admitted")
	}
	if lim.AcquireConn() {
		t.Fatal("second connection must be rejected")
	}
	if lim.RejectedConns() != 1 {
		t.Fatalf("RejectedConns=%d, want 1", lim.RejectedConns())
	}
	lim.ReleaseConn()
	if !lim.AcquireConn() {
		t.Fatal("slot must be reusable after release")
	}
	lim.ReleaseConn()
}

func TestLimiterQueryGate(t *testing.T) {
	lim := NewLimiter(Limits{MaxInflightQueries: 1, AcquireTimeout: 20 * time.Millisecond})
	if err := lim.AcquireQuery(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := lim.AcquireQuery(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("expected ErrBusy, got %v", err)
	}
	if time.Since(start) < 20*time.Millisecond {
		t.Fatal("acquire must wait up to AcquireTimeout before rejecting")
	}
	if lim.RejectedQueries() != 1 {
		t.Fatalf("RejectedQueries=%d, want 1", lim.RejectedQueries())
	}
	lim.ReleaseQuery()
	if err := lim.AcquireQuery(context.Background()); err != nil {
		t.Fatal(err)
	}
	lim.ReleaseQuery()
}

func TestLimiterQueryGateCtxCancel(t *testing.T) {
	lim := NewLimiter(Limits{MaxInflightQueries: 1, AcquireTimeout: time.Minute})
	if err := lim.AcquireQuery(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer lim.ReleaseQuery()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := lim.AcquireQuery(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestNilLimiter(t *testing.T) {
	var lim *Limiter
	if !lim.AcquireConn() {
		t.Fatal("nil limiter must admit connections")
	}
	if err := lim.AcquireQuery(context.Background()); err != nil {
		t.Fatal(err)
	}
	lim.ReleaseConn()
	lim.ReleaseQuery()
	if lim.RejectedConns() != 0 || lim.RejectedQueries() != 0 {
		t.Fatal("nil limiter must report zero rejections")
	}
}

func serveOnce(t *testing.T, opts HandleOptions) (net.Conn, <-chan struct{}) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		HandleWithOptions(serverConn, engine.New(), "memory", t.TempDir(), opts)
		close(done)
	}()
	return clientConn, done
}

func sendSQL(t *testing.T, c net.Conn, req any) map[string]any {
	t.Helper()
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	sendRaw(t, c, payload)
	var resp map[string]any
	if err := json.Unmarshal(readRaw(t, c), &resp); err != nil {
		t.Fatalf("response decode: %v", err)
	}
	return resp
}

func respError(resp map[string]any) string {
	if e, ok := resp["error"].(string); ok {
		return e
	}
	return ""
}

func TestConnectionLimitRejects(t *testing.T) {
	lim := NewLimiter(Limits{MaxConnections: 1})
	opts := HandleOptions{Limiter: lim}
	c1, done1 := serveOnce(t, opts)
	defer func() { c1.Close(); <-done1 }()
	// Round-trip on c1 to guarantee its handler acquired the conn slot
	// before the second connection is attempted.
	sendSQL(t, c1, map[string]string{"sql": "CREATE TABLE held (id INT PRIMARY KEY)"})

	c2, done2 := serveOnce(t, opts)
	// The rejection is written immediately on connect, before any request.
	var rejected map[string]any
	if err := json.Unmarshal(readRaw(t, c2), &rejected); err != nil {
		t.Fatalf("rejection decode: %v", err)
	}
	if !strings.Contains(respError(rejected), "connection") {
		t.Fatalf("expected connection-limit error, got %#v", rejected)
	}
	c2.Close()
	<-done2
	if lim.RejectedConns() != 1 {
		t.Fatalf("RejectedConns=%d, want 1", lim.RejectedConns())
	}
}

func TestQueryTimeout(t *testing.T) {
	c, done := serveOnce(t, HandleOptions{Limits: Limits{QueryTimeout: -1}})
	defer func() { c.Close(); <-done }()
	resp := sendSQL(t, c, map[string]string{"sql": "CREATE TABLE t (id INT PRIMARY KEY)"})
	if respError(resp) != "query timeout exceeded" {
		t.Fatalf("expected timeout error, got %#v", resp)
	}
}

func TestMaxResultRows(t *testing.T) {
	c, done := serveOnce(t, HandleOptions{Limits: Limits{MaxResultRows: 2}})
	defer func() { c.Close(); <-done }()
	sendSQL(t, c, map[string]string{"sql": "CREATE TABLE t (id INT PRIMARY KEY)"})
	sendSQL(t, c, map[string]string{"sql": "INSERT INTO t VALUES (1), (2), (3)"})
	resp := sendSQL(t, c, map[string]string{"sql": "SELECT * FROM t"})
	if !strings.Contains(respError(resp), "row limit") {
		t.Fatalf("expected row-limit error, got %#v", resp)
	}
	resp = sendSQL(t, c, map[string]string{"sql": "SELECT * FROM t LIMIT 2"})
	if respError(resp) != "" {
		t.Fatalf("at-cap query must succeed, got %#v", resp)
	}
}

func TestMaxResultBytes(t *testing.T) {
	c, done := serveOnce(t, HandleOptions{Limits: Limits{MaxResultBytes: 64}})
	defer func() { c.Close(); <-done }()
	sendSQL(t, c, map[string]string{"sql": "CREATE TABLE t (id INT PRIMARY KEY, v TEXT)"})
	sendSQL(t, c, map[string]string{"sql": "INSERT INTO t VALUES (1, 'a-longish-value-that-helps-exceed-the-cap')"})
	resp := sendSQL(t, c, map[string]string{"sql": "SELECT * FROM t"})
	if !strings.Contains(respError(resp), "maximum result size") {
		t.Fatalf("expected size-cap error, got %#v", resp)
	}
}

func TestMaxBatchStatements(t *testing.T) {
	c, done := serveOnce(t, HandleOptions{Limits: Limits{MaxBatchStatements: 2}})
	defer func() { c.Close(); <-done }()
	resp := sendSQL(t, c, map[string]any{"sqls": []string{"SELECT * FROM a", "SELECT * FROM b", "SELECT * FROM c"}})
	if !strings.Contains(respError(resp), "batch") {
		t.Fatalf("expected batch-limit error, got %#v", resp)
	}
}

func TestMaxRequestBytes(t *testing.T) {
	c, done := serveOnce(t, HandleOptions{Limits: Limits{MaxRequestBytes: 32}})
	defer func() { c.Close(); <-done }()
	payload, _ := json.Marshal(map[string]string{"sql": "SELECT * FROM some_table_with_a_long_name"})
	// Header and payload in one write: the server only reads the header
	// before rejecting, and net.Pipe would otherwise block on the body.
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(payload)))
	copy(frame[4:], payload)
	if _, err := c.Write(frame); err != nil {
		t.Fatal(err)
	}
	// Oversized request: server drops the connection without a response.
	var header [4]byte
	if _, err := readFull(c, header[:]); err == nil {
		t.Fatal("connection must be closed after oversized request")
	}
}

func TestMaxTxStatementsAbortsTransaction(t *testing.T) {
	c, done := serveOnce(t, HandleOptions{Limits: Limits{MaxTxStatements: 1}})
	defer func() { c.Close(); <-done }()
	sendSQL(t, c, map[string]string{"sql": "CREATE TABLE t (id INT PRIMARY KEY)"})
	if r := sendSQL(t, c, map[string]string{"sql": "BEGIN"}); r["message"] != "BEGIN ok" {
		t.Fatalf("BEGIN failed: %#v", r)
	}
	sendSQL(t, c, map[string]string{"sql": "INSERT INTO t VALUES (1)"})
	resp := sendSQL(t, c, map[string]string{"sql": "INSERT INTO t VALUES (2)"})
	if !strings.Contains(respError(resp), "transaction aborted") {
		t.Fatalf("expected tx abort, got %#v", resp)
	}
	// The transaction was aborted, so COMMIT reports no active tx.
	resp = sendSQL(t, c, map[string]string{"sql": "COMMIT"})
	if !strings.Contains(respError(resp), "no transaction") {
		t.Fatalf("expected aborted tx, got %#v", resp)
	}
}

func TestMaxTxDatabaseRowsRejectsBegin(t *testing.T) {
	db := engine.New()
	if _, err := db.Exec("CREATE TABLE t (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := db.Exec(fmt.Sprintf("INSERT INTO t VALUES (%d)", i)); err != nil {
			t.Fatal(err)
		}
	}
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		HandleWithOptions(serverConn, db, "memory", t.TempDir(), HandleOptions{Limits: Limits{MaxTxDatabaseRows: 3}})
		close(done)
	}()
	defer func() { clientConn.Close(); <-done }()
	resp := sendSQL(t, clientConn, map[string]string{"sql": "BEGIN"})
	if !strings.Contains(respError(resp), "transaction rejected") {
		t.Fatalf("expected tx rejection, got %#v", resp)
	}
}

func TestQueryGateBusy(t *testing.T) {
	lim := NewLimiter(Limits{MaxInflightQueries: 1, AcquireTimeout: 20 * time.Millisecond})
	if err := lim.AcquireQuery(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer lim.ReleaseQuery()
	c, done := serveOnce(t, HandleOptions{Limiter: lim})
	defer func() { c.Close(); <-done }()
	resp := sendSQL(t, c, map[string]string{"sql": "SELECT * FROM t"})
	if !strings.Contains(respError(resp), "busy") {
		t.Fatalf("expected busy rejection, got %#v", resp)
	}
}
