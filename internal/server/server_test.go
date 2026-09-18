package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"testing"

	"superdb/internal/engine"
	"superdb/internal/storage"
)

// stubCluster routes through a plain engine while recording atomic batches.
type stubCluster struct {
	db       *engine.Database
	atomics  int
	singles  int
	failNext error
}

func (s *stubCluster) Exec(_ context.Context, sql string) (engine.Result, error) {
	s.singles++
	if s.failNext != nil {
		return engine.Result{}, s.failNext
	}
	return s.db.Exec(sql)
}

func (s *stubCluster) ExecAtomic(_ context.Context, sqls []string) ([]engine.Result, error) {
	s.atomics++
	if s.failNext != nil {
		return nil, s.failNext
	}
	clone, err := s.db.Clone()
	if err != nil {
		return nil, err
	}
	for _, sql := range sqls {
		if _, err := clone.Exec(sql); err != nil {
			return nil, err
		}
	}
	if err := s.db.ReplaceFrom(clone); err != nil {
		return nil, err
	}
	return []engine.Result{{Affected: len(sqls)}}, nil
}

func TestClusterAtomicBatch(t *testing.T) {
	stub := &stubCluster{db: engine.New()}
	s := &session{db: stub.db, cluster: stub}
	out := executeRequest(request{SQLs: []string{
		"CREATE TABLE t (id INT PRIMARY KEY)",
		"INSERT INTO t VALUES (1)",
	}, Atomic: true}, s, "memory", t.TempDir())
	results, ok := out.([]any)
	if !ok || len(results) != 1 || stub.atomics != 1 || stub.singles != 0 {
		t.Fatalf("atomic batch must take the single-commit path: %#v", out)
	}
}

func TestClusterRejectsTransactions(t *testing.T) {
	stub := &stubCluster{db: engine.New()}
	s := &session{db: stub.db, cluster: stub}
	out := executeSQL("BEGIN", s, "memory", t.TempDir())
	m, ok := out.(map[string]string)
	if !ok || m["error"] == "" {
		t.Fatalf("BEGIN must be rejected in cluster mode: %#v", out)
	}
}

func TestClusterErrorSurfaces(t *testing.T) {
	stub := &stubCluster{db: engine.New(), failNext: errors.New("no leader")}
	s := &session{db: stub.db, cluster: stub}
	out := executeSQL("INSERT INTO t VALUES (1)", s, "memory", t.TempDir())
	m, ok := out.(map[string]string)
	if !ok || m["error"] != "no leader" {
		t.Fatalf("cluster errors must surface: %#v", out)
	}
}

func TestLoadSnapshotRestoresDatabase(t *testing.T) {
	dir := t.TempDir()
	source := engine.New()
	if _, err := source.Exec("CREATE TABLE users (id INT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Exec("INSERT INTO users VALUES (1, 'Ada')"); err != nil {
		t.Fatal(err)
	}
	if err := storage.WriteSnapshot(storage.SnapshotPath(dir), source); err != nil {
		t.Fatal(err)
	}

	restored := engine.New()
	if err := LoadSnapshot(dir, restored); err != nil {
		t.Fatal(err)
	}
	r, err := restored.Exec("SELECT * FROM users WHERE id = 1")
	if err != nil || len(r.Rows) != 1 || r.Rows[0][1] != "Ada" {
		t.Fatalf("restored database mismatch: result=%+v err=%v", r, err)
	}
}

func TestHandleReturnsNormalJSON(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	db := engine.New()
	done := make(chan struct{})
	go func() {
		Handle(serverConn, db, "memory", t.TempDir())
		close(done)
	}()

	request, _ := json.Marshal(map[string]string{"sql": "CREATE TABLE users (id INT PRIMARY KEY, name TEXT)"})
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(request)))
	if _, err := clientConn.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := clientConn.Write(request); err != nil {
		t.Fatal(err)
	}
	if _, err := readFull(clientConn, header[:]); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, binary.BigEndian.Uint32(header[:]))
	if _, err := readFull(clientConn, response); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(response, &result); err != nil {
		t.Fatalf("response is not normal JSON: %v", err)
	}
	if result["message"] != "table created" {
		t.Fatalf("unexpected response: %#v", result)
	}
	clientConn.Close()
	<-done
}

func TestHandleBatchAndTransaction(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	db := engine.New()
	done := make(chan struct{})
	go func() {
		Handle(serverConn, db, "memory", t.TempDir())
		close(done)
	}()
	request, _ := json.Marshal(map[string]any{"sqls": []string{
		"BEGIN",
		"CREATE TABLE users (id INT PRIMARY KEY, name TEXT)",
		"INSERT INTO users VALUES (1, 'Ada'), (2, 'Grace')",
		"COMMIT",
	}})
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(request)))
	if _, err := clientConn.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := clientConn.Write(request); err != nil {
		t.Fatal(err)
	}
	if _, err := readFull(clientConn, header[:]); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, binary.BigEndian.Uint32(header[:]))
	if _, err := readFull(clientConn, response); err != nil {
		t.Fatal(err)
	}
	var results []any
	if err := json.Unmarshal(response, &results); err != nil || len(results) != 4 {
		t.Fatalf("batch response mismatch: results=%#v err=%v", results, err)
	}
	clientConn.Close()
	<-done
	result, err := db.Exec("SELECT COUNT(*) FROM users")
	if err != nil || result.Rows[0][0] != 2 {
		t.Fatalf("transaction did not commit: result=%+v err=%v", result, err)
	}
}

func readFull(r io.Reader, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := r.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func TestStoragePathsRemainStable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if storage.SnapshotPath(dir) != filepath.Join(dir, "snapshot.spdb") || storage.WALPath(dir) != filepath.Join(dir, "wal.spdb") {
		t.Fatal("unexpected storage path")
	}
}

func TestRecoverLatestRestoresNewestBackup(t *testing.T) {
	backupDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "data")
	db := engine.New()
	if _, err := db.Exec("CREATE TABLE users (id INT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO users VALUES (1, 'Ada')"); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateBackup(backupDir, db); err != nil {
		t.Fatal(err)
	}
	restored := engine.New()
	path, err := RecoverLatest(backupDir, dataDir)
	if err != nil || path == "" {
		t.Fatalf("recover failed: path=%q err=%v", path, err)
	}
	if err := LoadSnapshot(dataDir, restored); err != nil {
		t.Fatal(err)
	}
	result, err := restored.Exec("SELECT * FROM users WHERE id = 1")
	if err != nil || len(result.Rows) != 1 || result.Rows[0][1] != "Ada" {
		t.Fatalf("recovered database mismatch: result=%+v err=%v", result, err)
	}
}
