package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
