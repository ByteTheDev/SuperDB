package server

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"testing"

	"superdb/internal/engine"
)

func sendRaw(t *testing.T, c net.Conn, payload []byte) {
	t.Helper()
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := c.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(payload); err != nil {
		t.Fatal(err)
	}
}

func readRaw(t *testing.T, c net.Conn) []byte {
	t.Helper()
	var header [4]byte
	if _, err := readFull(c, header[:]); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, binary.BigEndian.Uint32(header[:]))
	if _, err := readFull(c, resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAuthRequired(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	db := engine.New()
	done := make(chan struct{})
	go func() {
		HandleWithOptions(serverConn, db, "memory", t.TempDir(), HandleOptions{AuthKey: "secret"})
		close(done)
	}()
	defer func() { clientConn.Close(); <-done }()

	// No key -> rejected, table must not be created.
	payload, _ := json.Marshal(map[string]string{"sql": "CREATE TABLE t (id INT PRIMARY KEY)"})
	sendRaw(t, clientConn, payload)
	var errResp map[string]string
	if err := json.Unmarshal(readRaw(t, clientConn), &errResp); err != nil || errResp["error"] == "" {
		t.Fatalf("missing key must be rejected: %s err=%v", string(readRaw(t, clientConn)), err)
	}
	if _, err := db.Exec("SELECT * FROM t"); err == nil {
		t.Fatal("rejected request must not execute")
	}

	// Wrong key -> rejected.
	payload, _ = json.Marshal(map[string]string{"sql": "CREATE TABLE t (id INT PRIMARY KEY)", "auth": "wrong"})
	sendRaw(t, clientConn, payload)
	if err := json.Unmarshal(readRaw(t, clientConn), &errResp); err != nil || errResp["error"] == "" {
		t.Fatalf("wrong key must be rejected: %#v err=%v", errResp, err)
	}

	// Correct key (auth_key alias) -> executes.
	payload, _ = json.Marshal(map[string]string{"sql": "CREATE TABLE t (id INT PRIMARY KEY)", "auth_key": "secret"})
	sendRaw(t, clientConn, payload)
	var okResp map[string]any
	if err := json.Unmarshal(readRaw(t, clientConn), &okResp); err != nil || okResp["message"] != "table created" {
		t.Fatalf("correct key must succeed: %#v err=%v", okResp, err)
	}
}

func TestAuthDisabledByDefault(t *testing.T) {
	if !checkAuth("", "") || !checkAuth("anything", "") {
		t.Fatal("empty server key must allow all requests")
	}
	if checkAuth("", "secret") || checkAuth("wrong", "secret") || !checkAuth("secret", "secret") {
		t.Fatal("key comparison mismatch")
	}
}

func TestParseRequestFastAuth(t *testing.T) {
	q, ok := parseRequestFast([]byte(`{"sql":"SELECT 1","auth":"s3cr3t"}`))
	if !ok || q.SQL != "SELECT 1" || q.Auth != "s3cr3t" {
		t.Fatalf("fast path must parse auth: %+v ok=%v", q, ok)
	}
	q, ok = parseRequestFast([]byte(`{"sqls":["a"],"auth_key":"k"}`))
	if !ok || q.AuthKey != "k" {
		t.Fatalf("fast path must parse auth_key: %+v ok=%v", q, ok)
	}
}
