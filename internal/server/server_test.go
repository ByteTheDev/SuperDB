package server

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"testing"

	"superdb/internal/engine"
	"superdb/internal/storage"
)

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
