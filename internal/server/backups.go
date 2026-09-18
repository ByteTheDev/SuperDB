package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"superdb/internal/engine"
	"superdb/internal/storage"
)

// CreateBackup writes a timestamped, validated snapshot of the live database.
func CreateBackup(dir string, db *engine.Database) (string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("snapshot-%s.spdb", time.Now().UTC().Format("20060102-150405.000000000")))
	if err := storage.WriteSnapshot(path, db); err != nil {
		return "", err
	}
	return path, nil
}

func LatestBackup(dir string) (string, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "snapshot-*.spdb"))
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", fmt.Errorf("no timestamped backups found in %s", dir)
	}
	sort.Strings(paths)
	return paths[len(paths)-1], nil
}

// RecoverLatest validates the newest backup before replacing the active snapshot.
func RecoverLatest(backupDir, dataDir string) (string, error) {
	backup, err := LatestBackup(backupDir)
	if err != nil {
		return "", err
	}
	db := engine.New()
	loaded, err := storage.ReadSnapshot(backup, db)
	if err != nil {
		return "", fmt.Errorf("validate backup: %w", err)
	}
	if !loaded {
		return "", fmt.Errorf("backup disappeared: %s", backup)
	}
	if err := storage.WriteSnapshot(storage.SnapshotPath(dataDir), db); err != nil {
		return "", fmt.Errorf("restore backup: %w", err)
	}
	return backup, nil
}
