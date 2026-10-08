package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
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

func TestLocalAtomicBatch(t *testing.T) {
	db := engine.New()
	s := &session{db: db}
	out := executeRequest(request{SQLs: []string{
		"CREATE TABLE t (id INT PRIMARY KEY)",
		"INSERT INTO t VALUES (1)",
		"INSERT INTO t VALUES (2)",
	}, Atomic: true}, s, "memory", t.TempDir())
	results, ok := out.([]any)
	if !ok || len(results) != 3 {
		t.Fatalf("atomic batch results: %#v", out)
	}
	r, err := db.Exec("SELECT COUNT(*) FROM t")
	if err != nil || r.Rows[0][0] != 2 {
		t.Fatalf("atomic batch not applied: %+v %v", r, err)
	}
}

func TestLocalAtomicBatchRollsBackOnFailure(t *testing.T) {
	db := engine.New()
	if _, err := db.Exec("CREATE TABLE t (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	s := &session{db: db}
	out := executeRequest(request{SQLs: []string{
		"INSERT INTO t VALUES (1)",
		"INSERT INTO t VALUES (1)", // duplicate primary key: must fail
		"INSERT INTO t VALUES (3)",
	}, Atomic: true}, s, "memory", t.TempDir())
	m, ok := out.(map[string]string)
	if !ok || m["error"] == "" {
		t.Fatalf("atomic batch failure must surface: %#v", out)
	}
	r, err := db.Exec("SELECT COUNT(*) FROM t")
	if err != nil || r.Rows[0][0] != 0 {
		t.Fatalf("atomic batch partially applied: %+v %v", r, err)
	}
}

func TestLocalAtomicBatchRejectsInsideTransaction(t *testing.T) {
	db := engine.New()
	s := &session{db: db}
	out := executeSQL("BEGIN", s, "memory", t.TempDir())
	if m, ok := out.(map[string]string); !ok || m["error"] != "" {
		t.Fatalf("BEGIN failed: %#v", out)
	}
	out = executeRequest(request{SQLs: []string{"SELECT 1"}, Atomic: true}, s, "memory", t.TempDir())
	m, ok := out.(map[string]string)
	if !ok || m["error"] == "" {
		t.Fatalf("atomic batch inside tx must be rejected: %#v", out)
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

func TestBackupChecksumVerificationAndLegacyBackup(t *testing.T) {
	dir := t.TempDir()
	db := engine.New()
	if _, err := db.Exec("CREATE TABLE checks (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	path, err := CreateBackup(dir, db)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyBackup(path)
	if err != nil || !verified.ChecksumVerified || verified.SizeBytes == 0 || verified.SHA256 == "" {
		t.Fatalf("verify new backup: result=%+v err=%v", verified, err)
	}

	corrupt, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corrupt[len(corrupt)-1] ^= 0xff
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBackup(path); err == nil {
		t.Fatal("expected modified backup to fail verification")
	}

	legacyPath := filepath.Join(dir, "snapshot-20000101-000000.000000000.spdb")
	if err := storage.WriteSnapshot(legacyPath, db); err != nil {
		t.Fatal(err)
	}
	legacy, err := VerifyBackup(legacyPath)
	if err != nil || legacy.ChecksumVerified {
		t.Fatalf("legacy backup should decode without manifest: result=%+v err=%v", legacy, err)
	}
}

func mustNoErr(t *testing.T, out any) {
	t.Helper()
	if m, ok := out.(map[string]string); ok && m["error"] != "" {
		t.Fatalf("request failed: %s", m["error"])
	}
}

func TestHandleParamsBindEndToEnd(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	db := engine.New()
	done := make(chan struct{})
	go func() {
		Handle(serverConn, db, "memory", t.TempDir())
		close(done)
	}()
	roundTrip := func(req any) map[string]any {
		payload, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
		if _, err := clientConn.Write(header[:]); err != nil {
			t.Fatal(err)
		}
		if _, err := clientConn.Write(payload); err != nil {
			t.Fatal(err)
		}
		if _, err := readFull(clientConn, header[:]); err != nil {
			t.Fatal(err)
		}
		response := make([]byte, binary.BigEndian.Uint32(header[:]))
		if _, err := readFull(clientConn, response); err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(response, &out); err != nil {
			t.Fatalf("response is not JSON: %v", err)
		}
		return out
	}
	mustNoErr(t, roundTrip(map[string]any{"sql": "CREATE TABLE users (id INT PRIMARY KEY, name TEXT)"}))
	// Quotes inside params must be escaped by the binder, not by callers.
	mustNoErr(t, roundTrip(map[string]any{
		"sql":    "INSERT INTO users VALUES (?, ?)",
		"params": []any{1, "O'Brien"},
	}))
	sel := roundTrip(map[string]any{
		"sql":    "SELECT * FROM users WHERE name = ?",
		"params": []any{"O'Brien"},
	})
	rows, ok := sel["rows"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("param select returned %v", sel)
	}
	if row := rows[0].([]any); row[0] != float64(1) || row[1] != "O'Brien" {
		t.Fatalf("bound row mismatch: %v", row)
	}
	missing := roundTrip(map[string]any{"sql": "SELECT * FROM users WHERE id = ?"})
	if !strings.Contains(fmt.Sprint(missing["error"]), "missing params") {
		t.Fatalf("placeholder without params must fail: %v", missing)
	}
	tooMany := roundTrip(map[string]any{
		"sql":    "SELECT * FROM users WHERE id = ?",
		"params": []any{1, 2},
	})
	if !strings.Contains(fmt.Sprint(tooMany["error"]), "too many params") {
		t.Fatalf("extra params must fail: %v", tooMany)
	}
	batch := roundTrip(map[string]any{"sqls": []string{"SELECT 1"}, "params": []any{1}})
	if !strings.Contains(fmt.Sprint(batch["error"]), "params are only supported") {
		t.Fatalf("params with sqls must be rejected: %v", batch)
	}
	// A '?' inside a quoted literal is text, not a placeholder.
	literal := roundTrip(map[string]any{"sql": "SELECT * FROM users WHERE name = '?'"})
	if literal["error"] != nil {
		t.Fatalf("quoted placeholder must not bind: %v", literal)
	}
	clientConn.Close()
	<-done
	res, err := db.Exec("SELECT name FROM users WHERE id = 1")
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "O'Brien" {
		t.Fatalf("bound insert missing: %+v %v", res, err)
	}
}

func TestParamsPersistBoundSQLToWAL(t *testing.T) {
	dir := t.TempDir()
	db := engine.New()
	s := &session{db: db}
	mustNoErr(t, executeRequest(request{SQL: "CREATE TABLE users (id INT PRIMARY KEY, name TEXT)"}, s, "wal", dir))
	mustNoErr(t, executeRequest(request{SQL: "INSERT INTO users VALUES (?, ?)", Params: []any{1, "O'Brien"}}, s, "wal", dir))
	replayed := engine.New()
	if err := ReplayWAL(dir, replayed); err != nil {
		t.Fatalf("WAL must contain bound literals, not placeholders: %v", err)
	}
	res, err := replayed.Exec("SELECT name FROM users WHERE id = 1")
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "O'Brien" {
		t.Fatalf("replayed bound row mismatch: %+v %v", res, err)
	}
}

func TestParamsInsideTransactionPersistBound(t *testing.T) {
	dir := t.TempDir()
	db := engine.New()
	s := &session{db: db}
	mustNoErr(t, executeRequest(request{SQL: "CREATE TABLE t (id INT PRIMARY KEY, v TEXT)"}, s, "wal", dir))
	mustNoErr(t, executeRequest(request{SQL: "BEGIN"}, s, "wal", dir))
	mustNoErr(t, executeRequest(request{SQL: "INSERT INTO t VALUES (?, ?)", Params: []any{1, "x'y"}}, s, "wal", dir))
	mustNoErr(t, executeRequest(request{SQL: "COMMIT"}, s, "wal", dir))
	replayed := engine.New()
	if err := ReplayWAL(dir, replayed); err != nil {
		t.Fatalf("committed params must replay as literals: %v", err)
	}
	res, err := replayed.Exec("SELECT v FROM t WHERE id = 1")
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "x'y" {
		t.Fatalf("replayed tx row mismatch: %+v %v", res, err)
	}
}

func TestParamsErrors(t *testing.T) {
	db := engine.New()
	s := &session{db: db}
	wantErr := func(q request, substr string) {
		out := executeRequest(q, s, "memory", t.TempDir())
		m, ok := out.(map[string]string)
		if !ok || !strings.Contains(m["error"], substr) {
			t.Fatalf("want error containing %q, got %#v", substr, out)
		}
	}
	wantErr(request{SQL: "SELECT * FROM t WHERE id = ?"}, "missing params")
	wantErr(request{SQL: "SELECT * FROM t WHERE id = ?", Params: []any{1, 2}}, "too many params")
	wantErr(request{SQL: "SELECT ? FROM t", Params: []any{}}, "missing params")
	wantErr(request{SQLs: []string{"SELECT 1"}, Params: []any{1}}, "params are only supported")
	wantErr(request{SQLs: []string{"SELECT 1"}, Atomic: true, Params: []any{1}}, "params are only supported")
}
