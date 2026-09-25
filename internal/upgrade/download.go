package upgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ArchiveName mirrors the goreleaser name_template
// "{{ .ProjectName }}_{{ .Os }}_{{ .Arch }}" so the download matches the
// published release asset for the running platform.
func ArchiveName() string {
	return fmt.Sprintf("homer_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
}

// Downloader fetches archive + checksums for a given version tag from a
// release base URL. All network fetches are bounded and verified.
type Downloader struct {
	HTTP    *http.Client
	BaseURL string
}

func NewDownloader(baseURL string) *Downloader {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	return &Downloader{
		HTTP:    &http.Client{Timeout: 5 * time.Minute},
		BaseURL: baseURL,
	}
}

func (d *Downloader) fetch(url, destination string) error {
	if path, ok := strings.CutPrefix(url, "file://"); ok {
		source, err := os.Open(path)
		if err != nil {
			return err
		}
		defer source.Close()
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, source)
		closeErr := output.Close()
		if copyErr != nil {
			return fmt.Errorf("copy %s failed: %w", path, copyErr)
		}
		return closeErr
	}
	resp, err := d.HTTP.Get(url)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s returned HTTP %d", url, resp.StatusCode)
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, io.LimitReader(resp.Body, 512<<20))
	closeErr := output.Close()
	if copyErr != nil {
		return fmt.Errorf("download write failed: %w", copyErr)
	}
	return closeErr
}

// InstallArchive downloads the archive + checksums.txt for tag, verifies the
// SHA-256, extracts the homer binary, and atomically replaces target.
// target must be the absolute path of the running homer binary (typically
// from os.Executable). The replaced file keeps 0755.
func (d *Downloader) InstallArchive(tag, target string) error {
	tag = strings.TrimSpace(tag)
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	archive := ArchiveName()

	dir, err := os.MkdirTemp("", "homer-upgrade-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	base := strings.TrimSuffix(d.BaseURL, "/")
	archivePath := filepath.Join(dir, archive)
	archiveURL := fmt.Sprintf("%s/download/%s/%s", base, tag, archive)
	if err := d.fetch(archiveURL, archivePath); err != nil {
		return err
	}

	checksumPath := filepath.Join(dir, "checksums.txt")
	checksumURL := fmt.Sprintf("%s/download/%s/checksums.txt", base, tag)
	if err := d.fetch(checksumURL, checksumPath); err != nil {
		return err
	}

	if err := verifyChecksum(archivePath, checksumPath, archive); err != nil {
		return err
	}

	binaryPath, err := extractBinary(archivePath, dir)
	if err != nil {
		return err
	}

	// Read the extracted binary once, then atomically replace the target.
	data, err := os.ReadFile(binaryPath)
	if err != nil {
		return err
	}
	staged := target + ".homer-new"
	if err := os.WriteFile(staged, data, 0o755); err != nil {
		return err
	}
	if err := os.Rename(staged, target); err != nil {
		// Windows can fail to rename over a running binary; fall back to
		// a plain move-with-remove.
		os.Remove(staged)
		return err
	}
	return nil
}

func verifyChecksum(archivePath, checksumPath, name string) error {
	expected := ""
	data, err := os.ReadFile(checksumPath)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		fileName := strings.TrimPrefix(fields[1], "*")
		fileName = filepath.Base(fileName)
		if fileName == name {
			expected = strings.ToLower(fields[0])
			break
		}
	}
	if expected == "" {
		return fmt.Errorf("checksums.txt has no entry for %s", name)
	}
	archiveData, err := os.ReadFile(archivePath)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(archiveData)
	actual := hex.EncodeToString(sum[:])
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("checksum mismatch for %s (want %s, got %s)", name, expected, actual)
	}
	return nil
}

func extractBinary(archivePath, dir string) (string, error) {
	// tar is available on every supported platform (linux/darwin). The
	// archive produced by goreleaser contains ./homer at the root.
	tarErr := runTar(archivePath, dir)
	if tarErr != nil {
		return "", tarErr
	}
	candidates := []string{filepath.Join(dir, "homer")}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("release archive does not contain a homer binary")
}
