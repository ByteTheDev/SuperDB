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

func appendWAL(dir, sql string) error {
	return appendWALBatch(dir, []string{sql})
}

func appendWALBatch(dir string, sqls []string) error {
	return storage.AppendWALBatch(storage.WALPath(dir), sqls)
}

func saveSnapshot(dir string, db *engine.Database) error {
	return storage.WriteSnapshot(storage.SnapshotPath(dir), db)
}
