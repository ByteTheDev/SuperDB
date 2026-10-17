package server

import (
	"fmt"
	"os"

	"superdb/internal/engine"
	"superdb/internal/storage"
)

func ReplayWAL(dir string, db *engine.Database) error {
	apply := func(sql string) error {
		if _, err := db.Exec(sql); err != nil {
			return fmt.Errorf("replay WAL: %w", err)
		}
		return nil
	}
	path := storage.WALPath(dir)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		path = storage.LegacyWALPath(dir)
	}
	return storage.ReplayWAL(path, apply)
}

func LoadSnapshot(dir string, db *engine.Database) error {
	loaded, err := storage.ReadSnapshot(storage.SnapshotPath(dir), db)
	if err != nil {
		return err
	}
	if !loaded {
		_, err = storage.ReadSnapshot(storage.LegacySnapshotPath(dir), db)
	}
	return err
}

func appendWALBatch(dir string, sqls []string) error {
	return storage.AppendWALBatch(storage.WALPath(dir), sqls)
}

func saveSnapshot(dir string, db *engine.Database) error {
	return storage.WriteSnapshot(storage.SnapshotPath(dir), db)
}

// Compact rewrites the snapshot from the fully replayed state (snapshot +
// WAL) and then removes the WAL. Every WAL record is folded into the new
// snapshot, so leaving the file in place would double-apply those writes
// on the next start — duplicate-key failures on primary-key tables and
// silently duplicated rows on synthetic-key tables.
func Compact(dir string, db *engine.Database) error {
	if err := LoadSnapshot(dir, db); err != nil {
		return err
	}
	if err := ReplayWAL(dir, db); err != nil {
		return err
	}
	if err := saveSnapshot(dir, db); err != nil {
		return err
	}
	return clearWAL(dir)
}

// clearWAL removes replayed WAL files after their contents are durably
// inside a snapshot.
func clearWAL(dir string) error {
	for _, wal := range []string{storage.WALPath(dir), storage.LegacyWALPath(dir)} {
		if err := os.Remove(wal); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove replayed WAL %s: %w", wal, err)
		}
	}
	return nil
}
