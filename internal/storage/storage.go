package storage

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	magic              = "SUPERDB1"
	version            = byte(1)
	snapshotKind       = byte('S')
	walKind            = byte('W')
	headerSize         = 20
	maxPayloadSize     = 256 << 20
	walFileName        = "wal.spdb"
	snapshotFileName   = "snapshot.spdb"
	legacyWALFileName  = "superdb.wal"
	legacySnapshotName = "superdb.snapshot"
)

// WriteSnapshot stores a JSON snapshot inside the versioned SUPERDB container.
func WriteSnapshot(path string, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	encoded, err := encode(snapshotKind, payload)
	if err != nil {
		return fmt.Errorf("encode snapshot: %w", err)
	}
	return atomicWrite(path, encoded)
}

// ReadSnapshot reads a SUPERDB snapshot, or the legacy plain JSON format.
// It returns false when the file does not exist.
func ReadSnapshot(path string, value any) (bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(b) >= len(magic) && string(b[:len(magic)]) == magic {
		payload, next, err := decodeFrame(b, 0, snapshotKind)
		if err != nil {
			return true, fmt.Errorf("decode snapshot: %w", err)
		}
		if next != len(b) {
			return true, errors.New("decode snapshot: trailing data")
		}
		b = payload
	}
	if err := json.Unmarshal(b, value); err != nil {
		return true, fmt.Errorf("parse snapshot: %w", err)
	}
	return true, nil
}

// AppendWAL appends one independently compressed SQL record to a SUPERDB WAL.
func AppendWAL(path, sql string) error {
	encoded, err := encode(walKind, []byte(sql))
	if err != nil {
		return fmt.Errorf("encode WAL record: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(encoded); err != nil {
		return err
	}
	return f.Sync()
}

// ReplayWAL replays SUPERDB framed records and falls back to legacy line WAL.
func ReplayWAL(path string, apply func(string) error) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(b) >= len(magic) && string(b[:len(magic)]) == magic {
		for offset, record := 0, 1; offset < len(b); record++ {
			payload, next, err := decodeFrame(b, offset, walKind)
			if err != nil {
				return fmt.Errorf("WAL record %d: %w", record, err)
			}
			if err := apply(string(payload)); err != nil {
				return fmt.Errorf("replay WAL record %d: %w", record, err)
			}
			offset = next
		}
		return nil
	}
	for lineNo, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if err := apply(line); err != nil {
			return fmt.Errorf("replay legacy WAL line %d: %w", lineNo+1, err)
		}
	}
	return nil
}

func encode(kind byte, payload []byte) ([]byte, error) {
	if len(payload) > maxPayloadSize {
		return nil, errors.New("payload exceeds SUPERDB size limit")
	}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write(payload); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if compressed.Len() > maxPayloadSize {
		return nil, errors.New("compressed payload exceeds SUPERDB size limit")
	}
	out := make([]byte, headerSize+compressed.Len())
	copy(out[:8], magic)
	out[8] = kind
	out[9] = version
	binary.BigEndian.PutUint32(out[12:16], uint32(len(payload)))
	binary.BigEndian.PutUint32(out[16:20], uint32(compressed.Len()))
	copy(out[headerSize:], compressed.Bytes())
	return out, nil
}

func decodeFrame(b []byte, offset int, expectedKind byte) ([]byte, int, error) {
	if len(b)-offset < headerSize {
		return nil, offset, errors.New("truncated SUPERDB header")
	}
	h := b[offset:]
	if string(h[:8]) != magic {
		return nil, offset, errors.New("invalid SUPERDB magic")
	}
	if h[8] != expectedKind {
		return nil, offset, fmt.Errorf("unexpected SUPERDB record kind %q", h[8])
	}
	if h[9] != version {
		return nil, offset, fmt.Errorf("unsupported SUPERDB version %d", h[9])
	}
	rawLen := binary.BigEndian.Uint32(h[12:16])
	compressedLen := binary.BigEndian.Uint32(h[16:20])
	if rawLen > maxPayloadSize || compressedLen > maxPayloadSize {
		return nil, offset, errors.New("SUPERDB payload exceeds size limit")
	}
	end := headerSize + int(compressedLen)
	if end < headerSize || end > len(h) {
		return nil, offset, errors.New("truncated SUPERDB payload")
	}
	zr, err := zlib.NewReader(bytes.NewReader(h[headerSize:end]))
	if err != nil {
		return nil, offset, fmt.Errorf("open compressed payload: %w", err)
	}
	payload, readErr := io.ReadAll(io.LimitReader(zr, int64(rawLen)+1))
	closeErr := zr.Close()
	if readErr != nil {
		return nil, offset, fmt.Errorf("read compressed payload: %w", readErr)
	}
	if closeErr != nil {
		return nil, offset, fmt.Errorf("close compressed payload: %w", closeErr)
	}
	if len(payload) != int(rawLen) {
		return nil, offset, fmt.Errorf("payload length mismatch: got %d, want %d", len(payload), rawLen)
	}
	return payload, offset + end, nil
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err == nil {
		return nil
	}
	// Windows cannot rename over an existing file. Removing only the intended
	// snapshot target keeps replacement compatible across platforms.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(tmpPath, path)
}

func SnapshotPath(dir string) string { return filepath.Join(dir, snapshotFileName) }

func WALPath(dir string) string { return filepath.Join(dir, walFileName) }

func LegacySnapshotPath(dir string) string { return filepath.Join(dir, legacySnapshotName) }

func LegacyWALPath(dir string) string { return filepath.Join(dir, legacyWALFileName) }
