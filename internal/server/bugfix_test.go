package server

import (
	"reflect"
	"testing"

	"superdb/internal/engine"
	"superdb/internal/storage"
)

func TestWriteStatementsDoesNotMutateInput(t *testing.T) {
	in := []string{
		"SELECT * FROM t",
		"INSERT INTO t VALUES (1)",
		"SELECT 1",
	}
	orig := append([]string(nil), in...)
	out := writeStatements(in)
	if !reflect.DeepEqual(in, orig) {
		t.Fatalf("input mutated: got %q want %q", in, orig)
	}
	if len(out) != 1 || out[0] != "INSERT INTO t VALUES (1)" {
		t.Fatalf("unexpected filter result: %q", out)
	}
}

// encoding/json binds struct fields case-insensitively, so {"SQL":...}
// would populate the real sql field on the fallback path. The fast parser
// must decline such keys instead of skipping them as unknown, or the same
// payload behaves differently depending on which parser runs it.
func TestParseRequestFastDeclinesCaseVariantKeys(t *testing.T) {
	variants := []string{
		`{"SQL":"SELECT 1"}`,
		`{"Sql":"SELECT 1"}`,
		`{"SQLS":["a"],"atomic":true}`,
		`{"Auth":"k","sql":"x"}`,
		`{"AUTH_KEY":"k","sql":"x"}`,
		`{"Ping":true}`,
		`{"Params":[1],"sql":"x"}`,
		`{"ATOMIC":true,"sqls":["a"]}`,
		`{"sql":"x","SQL":"y"}`,
	}
	for _, payload := range variants {
		if _, ok := parseRequestFast([]byte(payload)); ok {
			t.Fatalf("fast path must decline case-variant field in %q", payload)
		}
	}
	// Keys that merely share a prefix remain unknown fields to skip.
	q, ok := parseRequestFast([]byte(`{"sqlx":"y","sql":"SELECT 1"}`))
	if !ok || q.SQL != "SELECT 1" {
		t.Fatalf("non-variant unknown key must still skip: %+v ok=%v", q, ok)
	}
}

// compact folds the WAL into the snapshot, so the WAL must not survive it:
// replaying folded records over the rewritten snapshot double-applies every
// write — duplicate-key errors on primary-key tables and silently
// duplicated rows on synthetic-key tables — breaking the next startup.
func TestCompactClearsWAL(t *testing.T) {
	dir := t.TempDir()
	seed := engine.New()
	if _, err := seed.Exec("CREATE TABLE t (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec("INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec("CREATE TABLE log (v TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec("INSERT INTO log VALUES ('kept')"); err != nil {
		t.Fatal(err)
	}
	if err := storage.WriteSnapshot(storage.SnapshotPath(dir), seed); err != nil {
		t.Fatal(err)
	}
	// Later writes live only in the WAL, as in a running wal-mode server.
	for _, sql := range []string{
		"INSERT INTO t VALUES (2)",
		"INSERT INTO log VALUES ('tail')",
	} {
		if err := storage.AppendWAL(storage.WALPath(dir), sql); err != nil {
			t.Fatal(err)
		}
	}
	db := engine.New()
	if err := Compact(dir, db); err != nil {
		t.Fatalf("compact: %v", err)
	}
	// Simulate the next startup: snapshot plus WAL replay must reproduce
	// the folded state exactly once.
	restarted := engine.New()
	if err := LoadSnapshot(dir, restarted); err != nil {
		t.Fatal(err)
	}
	if err := ReplayWAL(dir, restarted); err != nil {
		t.Fatalf("post-compact WAL replay must not double-apply: %v", err)
	}
	r, err := restarted.Exec("SELECT COUNT(*) FROM t")
	if err != nil || r.Rows[0][0] != 2 {
		t.Fatalf("pk table after compact+restart: %+v %v", r, err)
	}
	r, err = restarted.Exec("SELECT COUNT(*) FROM log")
	if err != nil || r.Rows[0][0] != 2 {
		t.Fatalf("synthetic-key table must not duplicate rows: %+v %v", r, err)
	}
}
