package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const DefaultRepository = "ByteTheDev/SuperDB"

var ErrUpdateScheduled = errors.New("update scheduled; restart SuperDB to use the new version")

type Release struct {
	TagName string `json:"tag_name"`
}

type client struct {
	http *http.Client
}

func Latest(ctx context.Context, repository string) (Release, error) {
	if repository == "" {
		repository = DefaultRepository
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repository+"/releases/latest", nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("User-Agent", "SuperDB-Updater")
	resp, err := (&client{http: &http.Client{Timeout: 30 * time.Second}}).http.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("latest release: %s", resp.Status)
	}
	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return Release{}, err
	}
	if release.TagName == "" {
		return Release{}, errors.New("latest release has no version tag")
	}
	return release, nil
}

func Version(tag string) string {
	return strings.TrimPrefix(strings.TrimSpace(tag), "v")
}

func Update(ctx context.Context, repository, version, installDir string) error {
	if repository == "" {
		repository = DefaultRepository
	}
	version = Version(version)
	if version == "" {
		release, err := Latest(ctx, repository)
		if err != nil {
			return err
		}
		version = Version(release.TagName)
	}
	osName, arch, extension, err := target()
	if err != nil {
		return err
	}
	archive := fmt.Sprintf("superdb_%s_%s_%s%s", version, osName, arch, extension)
	baseURL := fmt.Sprintf("https://github.com/%s/releases/download/v%s", repository, version)
	workDir, err := os.MkdirTemp("", "superdb-update-")
	if err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		defer os.RemoveAll(workDir)
	}

	httpClient := &client{http: &http.Client{Timeout: 2 * time.Minute}}
	archivePath := filepath.Join(workDir, archive)
	if err := httpClient.download(ctx, baseURL+"/"+archive, archivePath); err != nil {
		return err
	}
	checksumsPath := filepath.Join(workDir, "SHA256SUMS")
	if err := httpClient.download(ctx, baseURL+"/SHA256SUMS", checksumsPath); err != nil {
		return err
	}
	if err := verifyChecksum(archivePath, checksumsPath, archive); err != nil {
		return err
	}
	stageDir := filepath.Join(workDir, "package")
	if err := os.MkdirAll(stageDir, 0755); err != nil {
		return err
	}
	if err := extract(archivePath, stageDir, extension == ".zip"); err != nil {
		return err
	}
	packageDir := stageDir
	if installDir == "" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		installDir = filepath.Dir(executable)
	}
	if runtime.GOOS == "windows" {
		return scheduleWindowsInstall(workDir, packageDir, installDir)
	}
	return installUnix(packageDir, installDir)
}

func target() (osName, arch, extension string, err error) {
	switch runtime.GOOS {
	case "linux", "darwin":
		osName = runtime.GOOS
		extension = ".tar.gz"
	case "windows":
		osName = "windows"
		extension = ".zip"
	default:
		return "", "", "", fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}
	switch runtime.GOARCH {
	case "amd64", "arm64":
		arch = runtime.GOARCH
	default:
		return "", "", "", fmt.Errorf("unsupported architecture: %s", runtime.GOARCH)
	}
	return osName, arch, extension, nil
}

func (c *client) download(ctx context.Context, url, destination string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "SuperDB-Updater")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", filepath.Base(destination), resp.Status)
	}
	file, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(file, resp.Body)
	return err
}

func verifyChecksum(archivePath, checksumsPath, archive string) error {
	checksums, err := os.Open(checksumsPath)
	if err != nil {
		return err
	}
	defer checksums.Close()
	var expected string
	data, err := io.ReadAll(checksums)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.TrimPrefix(fields[len(fields)-1], "*") == archive {
			expected = fields[0]
			break
		}
	}
	if expected == "" {
		return fmt.Errorf("checksum missing for %s", archive)
	}
	hash := sha256.New()
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(hash, file); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, expected) {
		return errors.New("downloaded archive checksum verification failed")
	}
	return nil
}

func extract(archivePath, destination string, isZip bool) error {
	if isZip {
		return extractZip(archivePath, destination)
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer compressed.Close()
	return extractTar(tar.NewReader(compressed), destination)
}

func extractTar(reader *tar.Reader, destination string) error {
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Base(header.Name)
		if name != "superdb" && name != "superdb-cli" {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return fmt.Errorf("unexpected archive entry type for %s", name)
		}
		if err := writeExtracted(reader, filepath.Join(destination, name), header.Mode); err != nil {
			return err
		}
	}
}

func extractZip(archivePath, destination string) error {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	for _, entry := range archive.File {
		name := path.Base(entry.Name)
		if name != "superdb.exe" && name != "superdb-cli.exe" {
			continue
		}
		if entry.FileInfo().IsDir() {
			return fmt.Errorf("unexpected directory archive entry for %s", name)
		}
		input, err := entry.Open()
		if err != nil {
			return err
		}
		err = writeExtracted(input, filepath.Join(destination, name), 0755)
		input.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeExtracted(input io.Reader, destination string, mode int64) error {
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(mode)|0700)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(file, input)
	return err
}

func installUnix(packageDir, installDir string) error {
	if err := os.MkdirAll(installDir, 0755); err != nil {
		return err
	}
	for _, name := range []string{"superdb", "superdb-cli"} {
		if err := replaceFile(filepath.Join(packageDir, name), filepath.Join(installDir, name)); err != nil {
			return err
		}
	}
	return nil
}

func replaceFile(source, destination string) error {
	temp, err := os.CreateTemp(filepath.Dir(destination), ".superdb-update-")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	input, err := os.Open(source)
	if err != nil {
		temp.Close()
		return err
	}
	if _, err := io.Copy(temp, input); err != nil {
		input.Close()
		temp.Close()
		return err
	}
	input.Close()
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tempName, 0755); err != nil {
		return err
	}
	return os.Rename(tempName, destination)
}

func scheduleWindowsInstall(workDir, packageDir, installDir string) error {
	scriptPath := filepath.Join(workDir, "apply-update.ps1")
	script := `param([int]$ParentPid, [string]$PackageDir, [string]$InstallDir, [string]$WorkDir)
$ErrorActionPreference = "Stop"
Wait-Process -Id $ParentPid -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
foreach ($name in @("superdb.exe", "superdb-cli.exe")) {
    $source = Join-Path $PackageDir $name
    $destination = Join-Path $InstallDir $name
    $updated = $false
    for ($attempt = 0; $attempt -lt 60; $attempt++) {
        try {
            Copy-Item -LiteralPath $source -Destination $destination -Force
            $updated = $true
            break
        } catch {
            Start-Sleep -Seconds 1
        }
    }
    if (-not $updated) { throw "Could not replace $name; stop SuperDB and retry the update." }
}
Remove-Item -LiteralPath $WorkDir -Recurse -Force -ErrorAction SilentlyContinue
`
	if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		return err
	}
	command := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath,
		"-ParentPid", strconv.Itoa(os.Getpid()), "-PackageDir", packageDir, "-InstallDir", installDir, "-WorkDir", workDir)
	if err := command.Start(); err != nil {
		return err
	}
	return ErrUpdateScheduled
}
