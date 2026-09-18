package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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
