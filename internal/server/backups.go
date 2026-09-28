package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"superdb/internal/engine"
	"superdb/internal/storage"
)

type backupManifest struct {
	Version int    `json:"version"`
	Size    int64  `json:"size_bytes"`
	SHA256  string `json:"sha256"`
}

// BackupVerification describes the integrity checks performed on a backup.
type BackupVerification struct {
	Path             string
	SizeBytes        int64
	SHA256           string
	ChecksumVerified bool
}

// CreateBackup writes a timestamped, validated snapshot of the live database.
func CreateBackup(dir string, db *engine.Database) (string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("snapshot-%s.spdb", time.Now().UTC().Format("20060102-150405.000000000")))
	if err := storage.WriteSnapshot(path, db); err != nil {
		return "", err
	}
	if _, err := VerifyBackup(path); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("validate new backup: %w", err)
	}
	if err := writeBackupManifest(path); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("write backup checksum: %w", err)
	}
	return path, nil
}

// VerifyBackup validates the snapshot payload and, for backups created by
// current versions, checks its SHA-256 manifest. Older backups without a
// manifest are still accepted after full snapshot decoding.
func VerifyBackup(path string) (BackupVerification, error) {
	file, err := os.Open(path)
	if err != nil {
		return BackupVerification{}, err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return BackupVerification{}, copyErr
	}
	if closeErr != nil {
		return BackupVerification{}, closeErr
	}
	actualHash := hex.EncodeToString(hash.Sum(nil))
	db := engine.New()
	loaded, err := storage.ReadSnapshot(path, db)
	if err != nil {
		return BackupVerification{}, fmt.Errorf("decode snapshot: %w", err)
	}
	if !loaded {
		return BackupVerification{}, fmt.Errorf("backup disappeared: %s", path)
	}
	result := BackupVerification{Path: path, SizeBytes: size, SHA256: actualHash}
	manifestPath := backupManifestPath(path)
	manifestBytes, err := os.ReadFile(manifestPath)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return BackupVerification{}, fmt.Errorf("read backup checksum: %w", err)
	}
	var manifest backupManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return BackupVerification{}, fmt.Errorf("parse backup checksum: %w", err)
	}
	if manifest.Version != 1 {
		return BackupVerification{}, fmt.Errorf("unsupported backup checksum version %d", manifest.Version)
	}
	if manifest.Size != size || !strings.EqualFold(manifest.SHA256, actualHash) {
		return BackupVerification{}, fmt.Errorf("backup checksum mismatch")
	}
	result.ChecksumVerified = true
	return result, nil
}

func writeBackupManifest(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	manifest := backupManifest{Version: 1, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	manifestPath := backupManifestPath(path)
	tmpPath := manifestPath + ".tmp"
	if err := os.WriteFile(tmpPath, append(data, '\n'), 0600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, manifestPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func backupManifestPath(path string) string { return path + ".manifest.json" }

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
	if _, err := VerifyBackup(backup); err != nil {
		return "", fmt.Errorf("validate backup: %w", err)
	}
	db := engine.New()
	if _, err := storage.ReadSnapshot(backup, db); err != nil {
		return "", fmt.Errorf("load validated backup: %w", err)
	}
	if err := storage.WriteSnapshot(storage.SnapshotPath(dataDir), db); err != nil {
		return "", fmt.Errorf("restore backup: %w", err)
	}
	return backup, nil
}
