package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSnapshotRoundTripAndLegacyJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "superdb.snapshot")
	want := map[string]any{
		"name": "users",
		"rows": []any{
			map[string]any{"id": float64(1), "name": "Ada"},
			map[string]any{"id": float64(2), "name": "Grace"},
		},
	}
	if err := WriteSnapshot(path, want); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), magic) {
		t.Fatalf("snapshot does not have SUPERDB header: %q", b[:min(len(b), len(magic))])
	}
	var got map[string]any
	loaded, err := ReadSnapshot(path, &got)
	if err != nil || !loaded {
		t.Fatalf("load compressed snapshot: loaded=%v err=%v", loaded, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot mismatch: got %#v want %#v", got, want)
	}

	legacyPath := filepath.Join(dir, "legacy.snapshot")
	legacy, _ := json.Marshal(want)
	if err := os.WriteFile(legacyPath, legacy, 0644); err != nil {
		t.Fatal(err)
	}
	got = nil
	loaded, err = ReadSnapshot(legacyPath, &got)
	if err != nil || !loaded || !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy snapshot mismatch: loaded=%v err=%v got=%#v", loaded, err, got)
	}
}

func TestWALRoundTripAndLegacyLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "superdb.wal")
	want := []string{
		"CREATE TABLE users (id INT PRIMARY KEY, name TEXT)",
		"INSERT INTO users VALUES (1, 'Ada')",
	}
	for _, sql := range want {
		if err := AppendWAL(path, sql); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), magic) {
		t.Fatalf("WAL does not have SUPERDB header")
	}
	var got []string
	if err := ReplayWAL(path, func(sql string) error {
		got = append(got, sql)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WAL mismatch: got %#v want %#v", got, want)
	}

	legacyPath := filepath.Join(dir, "legacy.wal")
	if err := os.WriteFile(legacyPath, []byte(strings.Join(want, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got = nil
	if err := ReplayWAL(legacyPath, func(sql string) error {
		got = append(got, sql)
		return nil
	}); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy WAL mismatch: err=%v got=%#v", err, got)
	}
}

func TestCorruptSUPERDBDataFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "superdb.snapshot")
	if err := os.WriteFile(path, []byte(magic), 0644); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if _, err := ReadSnapshot(path, &value); err == nil {
		t.Fatal("expected truncated snapshot error")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
