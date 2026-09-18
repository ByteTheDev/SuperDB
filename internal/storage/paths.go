package storage

import "path/filepath"

const (
	walFileName        = "wal.spdb"
	snapshotFileName   = "snapshot.spdb"
	legacyWALFileName  = "superdb.wal"
	legacySnapshotName = "superdb.snapshot"
)

func SnapshotPath(dir string) string { return filepath.Join(dir, snapshotFileName) }

func WALPath(dir string) string { return filepath.Join(dir, walFileName) }

func LegacySnapshotPath(dir string) string { return filepath.Join(dir, legacySnapshotName) }

func LegacyWALPath(dir string) string { return filepath.Join(dir, legacyWALFileName) }
