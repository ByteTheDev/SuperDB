package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestVersion(t *testing.T) {
	if got := Version("v0.1.1"); got != "0.1.1" {
		t.Fatalf("Version() = %q", got)
	}
}

func TestVerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "release.tar.gz")
	contents := []byte("superdb release")
	if err := os.WriteFile(archive, contents, 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(contents)
	checksums := filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(checksums, []byte(hex.EncodeToString(hash[:])+"  release.tar.gz\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksum(archive, checksums, "release.tar.gz"); err != nil {
		t.Fatalf("verifyChecksum() error = %v", err)
	}
	if err := os.WriteFile(checksums, []byte("bad  release.tar.gz\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksum(archive, checksums, "release.tar.gz"); err == nil {
		t.Fatal("verifyChecksum() accepted an invalid checksum")
	}
}

func TestExtractTarAndInstall(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "release.tar.gz")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	writer := tar.NewWriter(compressed)
	for _, item := range []struct {
		name string
		data []byte
	}{
		{name: "superdb_0.1.1_linux_amd64/superdb", data: []byte("server")},
		{name: "superdb_0.1.1_linux_amd64/superdb-cli", data: []byte("cli")},
	} {
		if err := writer.WriteHeader(&tar.Header{Name: item.name, Mode: 0755, Size: int64(len(item.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(item.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	stage := filepath.Join(dir, "stage")
	if err := os.Mkdir(stage, 0755); err != nil {
		t.Fatal(err)
	}
	if err := extract(archive, stage, false); err != nil {
		t.Fatal(err)
	}
	install := filepath.Join(dir, "install")
	if err := installUnix(stage, install); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name string
		want string
	}{
		{name: "superdb", want: "server"},
		{name: "superdb-cli", want: "cli"},
	} {
		got, err := os.ReadFile(filepath.Join(install, item.name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, []byte(item.want)) {
			t.Fatalf("%s = %q, want %q", item.name, got, item.want)
		}
	}
}

func TestExtractZip(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "release.zip")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for _, item := range []struct {
		name string
		data string
	}{
		{name: "superdb_0.1.1_windows_amd64/superdb.exe", data: "server"},
		{name: "superdb_0.1.1_windows_amd64/superdb-cli.exe", data: "cli"},
	} {
		entry, err := writer.Create(item.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(item.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(dir, "stage")
	if err := os.Mkdir(stage, 0755); err != nil {
		t.Fatal(err)
	}
	if err := extract(archive, stage, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stage, "superdb.exe")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stage, "superdb-cli.exe")); err != nil {
		t.Fatal(err)
	}
}
