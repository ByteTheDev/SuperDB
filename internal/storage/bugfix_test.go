package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWALWriterAppendAfterCloseErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.spdb")
	w, err := OpenWALWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Append([]string{"SELECT 1"}); err == nil {
		t.Fatal("expected closed-writer error, got nil")
	}
}

func TestAppendWALBatchEmptyCreatesNoFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.spdb")
	if err := AppendWALBatch(path, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("empty batch must not create WAL file, stat err=%v", err)
	}
}

func TestEncodeFrameRejectsInvalidCombos(t *testing.T) {
	if _, err := encodeFrame(snapshotKind, walVersion, []byte("{}"), true); err == nil {
		t.Fatal("expected snapshot v2 rejection")
	}
	if _, err := encodeFrame(walKind, checksumVersion, []byte("x"), true); err == nil {
		t.Fatal("expected v3 WAL compressed rejection")
	}
	if _, err := encodeFrame(walKind, walVersion, []byte("x"), true); err == nil {
		t.Fatal("expected v2 WAL compressed rejection")
	}
	if _, err := encodeFrame(walKind, version, []byte("x"), false); err == nil {
		t.Fatal("expected v1 WAL raw rejection")
	}
	// Valid combos still encode.
	if _, err := encode(snapshotKind, []byte("{}")); err != nil {
		t.Fatalf("snapshot encode: %v", err)
	}
}
