package storage

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFrameStoredLen(b []byte, frameStart int) uint32 {
	return binary.BigEndian.Uint32(b[frameStart+16 : frameStart+20])
}

func TestSnapshotFramesCarryChecksum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.spdb")
	want := map[string]any{"name": "users"}
	if err := WriteSnapshot(path, want); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < checksumHeaderSize {
		t.Fatalf("snapshot too short for checksum header: %d bytes", len(b))
	}
	if b[9] != checksumVersion {
		t.Fatalf("snapshot frame version = %d, want %d", b[9], checksumVersion)
	}
	if b[10] != 0 || b[11] != 0 {
		t.Fatalf("checksum frame padding must stay zero: %d %d", b[10], b[11])
	}
	stored := b[checksumHeaderSize:]
	if got, field := binary.BigEndian.Uint32(b[20:24]), checksumStored(stored); got != field {
		t.Fatalf("snapshot checksum = %08x, want %08x", got, field)
	}
	if storedLen := readFrameStoredLen(b, 0); storedLen != uint32(len(stored)) {
		t.Fatalf("stored length = %d, want %d", storedLen, len(stored))
	}
	var got map[string]any
	loaded, err := ReadSnapshot(path, &got)
	if err != nil || !loaded {
		t.Fatalf("checksummed snapshot reload: loaded=%v err=%v", loaded, err)
	}
}

func TestWALFramesCarryChecksum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.spdb")
	want := []string{"CREATE TABLE t (id INT PRIMARY KEY)", "INSERT INTO t VALUES (1)", "INSERT INTO t VALUES (2)"}
	if err := AppendWALBatch(path, want); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	frameStart := 0
	for i := range want {
		if b[frameStart+9] != checksumVersion {
			t.Fatalf("WAL record %d frame version = %d, want %d", i+1, b[frameStart+9], checksumVersion)
		}
		stored := b[frameStart+checksumHeaderSize:]
		if remaining := readFrameStoredLen(b, frameStart); int(remaining) <= len(stored) {
			stored = stored[:remaining]
		}
		if got, field := binary.BigEndian.Uint32(b[frameStart+20:frameStart+24]), checksumStored(stored); got != field {
			t.Fatalf("WAL record %d checksum = %08x, want %08x", i+1, got, field)
		}
		frameStart += checksumHeaderSize + int(readFrameStoredLen(b, frameStart))
	}
	if frameStart != len(b) {
		t.Fatalf("WAL frames cover %d bytes of %d", frameStart, len(b))
	}
	var got []string
	if err := ReplayWAL(path, func(sql string) error {
		got = append(got, sql)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("WAL replay mismatch: got %q want %q", got, want)
	}
}

func TestSnapshotChecksumDetectsCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.spdb")
	if err := WriteSnapshot(path, map[string]any{"name": "users"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[checksumHeaderSize+3] ^= 0xFF
	if err := os.WriteFile(path, b, 0644); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	_, err = ReadSnapshot(path, &value)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
}

func TestWALChecksumDetectsCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.spdb")
	if err := AppendWALBatch(path, []string{"INSERT INTO t VALUES (1)", "INSERT INTO t VALUES (2)"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	frame2 := checksumHeaderSize + int(readFrameStoredLen(b, 0))
	b[frame2+checksumHeaderSize+2] ^= 0x40
	if err := os.WriteFile(path, b, 0644); err != nil {
		t.Fatal(err)
	}
	err = ReplayWAL(path, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "WAL record 2") || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected record 2 checksum mismatch, got %v", err)
	}
}

func TestChecksumFieldTamperingRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.spdb")
	if err := WriteSnapshot(path, map[string]any{"name": "users"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[20] ^= 0x01
	if err := os.WriteFile(path, b, 0644); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if _, err := ReadSnapshot(path, &value); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
}

func TestTruncatedChecksumHeaderRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.spdb")
	if err := WriteSnapshot(path, map[string]any{"name": "users"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b[:22], 0644); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if _, err := ReadSnapshot(path, &value); err == nil {
		t.Fatal("expected truncated checksum header error")
	}
}

func TestLegacyFramedVersionsStillDecode(t *testing.T) {
	dir := t.TempDir()

	snapshotPayload := []byte(`{"name":"users"}`)
	legacySnapshot, err := encodeFrame(snapshotKind, version, snapshotPayload, true)
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(dir, "legacy-v1.spdb")
	if err := os.WriteFile(legacyPath, legacySnapshot, 0644); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	loaded, err := ReadSnapshot(legacyPath, &value)
	if err != nil || !loaded || value["name"] != "users" {
		t.Fatalf("legacy v1 framed snapshot: loaded=%v err=%v value=%v", loaded, err, value)
	}

	walPath := filepath.Join(dir, "mixed.wal")
	var mixed []byte
	legacyRaw, err := encodeFrame(walKind, walVersion, []byte("INSERT INTO t VALUES (1)"), false)
	if err != nil {
		t.Fatal(err)
	}
	mixed = append(mixed, legacyRaw...)
	legacyCompressed, err := encodeFrame(walKind, version, []byte("INSERT INTO t VALUES (2)"), true)
	if err != nil {
		t.Fatal(err)
	}
	mixed = append(mixed, legacyCompressed...)
	if err := os.WriteFile(walPath, mixed, 0644); err != nil {
		t.Fatal(err)
	}
	if err := AppendWAL(walPath, "INSERT INTO t VALUES (3)"); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := ReplayWAL(walPath, func(sql string) error {
		got = append(got, sql)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"INSERT INTO t VALUES (1)", "INSERT INTO t VALUES (2)", "INSERT INTO t VALUES (3)"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("mixed-version WAL replay: got %q want %q", got, want)
	}
}
