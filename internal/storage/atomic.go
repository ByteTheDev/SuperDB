package storage

import (
	"errors"
	"os"
	"path/filepath"
)

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
	if n, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	} else if n != len(data) {
		tmp.Close()
		return errors.New("short snapshot write")
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
