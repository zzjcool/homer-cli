package e2e

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// extractInstallURLs pulls every raw.githubusercontent.com install.sh URL out
// of the given documentation file. Branch-name drift between the default
// branch (master) and the documented URL once shipped a 404 to every new
// user, so the docs are now guarded by TestDocsInstallURLReachable below.
var installURLPattern = regexp.MustCompile(`https://raw\.githubusercontent\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/install\.sh`)

func extractInstallURLs(t *testing.T, filename string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), filename))
	if err != nil {
		t.Fatalf("read %s: %v", filename, err)
	}
	matches := installURLPattern.FindAllString(string(data), -1)
	if len(matches) == 0 {
		t.Fatalf("%s contains no raw.githubusercontent.com install.sh URL; guard is void", filename)
	}
	return matches
}

// TestDocsInstallURLReachable verifies that every documented install.sh URL
// actually resolves on the default branch. It needs network access and is
// skipped with -short.
func TestDocsInstallURLReachable(t *testing.T) {
	if testing.Short() {
		t.Skip("network check skipped in -short mode")
	}
	files := []string{"README.md", "npm/README.md", "npm/bin/homer.js"}
	client := &http.Client{Timeout: 30 * time.Second}
	seen := map[string]bool{}
	for _, filename := range files {
		for _, url := range extractInstallURLs(t, filename) {
			if seen[url] {
				continue
			}
			seen[url] = true
			resp, err := client.Head(url)
			if err != nil {
				t.Fatalf("HEAD %s (from %s): %v", url, filename, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("documented install URL returns %d (from %s): %s", resp.StatusCode, filename, url)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("no install URLs found to check")
	}
}

// TestInstallScriptLocaleRobustness replays the installer under a hostile
// locale: a full-width punctuation byte adjacent to $TARGET once made macOS
// /bin/sh treat the multibyte sequence as part of the variable name and die
// with "unbound variable" after the binary was already installed.
func TestInstallScriptLocaleRobustness(t *testing.T) {
	binary := buildHomer(t)
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer Install Review\n\temail = homer-install-review@example.invalid\n")
	dist := filepath.Join(root, "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}

	// Stage the real binary as a goreleaser-shaped archive plus checksums.
	staging := filepath.Join(root, "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	binaryData, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "homer"), binaryData, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dist, "homer_linux_amd64.tar.gz")
	tar := runProcess(t, repoRoot(), global, nil, "tar", "-czf", archive, "-C", staging, "homer")
	if tar.code != 0 {
		t.Fatalf("tar archive failed: %s", tar.stderr)
	}
	sum := runProcess(t, repoRoot(), global, nil, "sha256sum", archive)
	if sum.code != 0 {
		t.Fatalf("sha256sum failed: %s", sum.stderr)
	}
	fields := strings.Fields(sum.stdout)
	if len(fields) == 0 {
		t.Fatalf("sha256sum output = %q", sum.stdout)
	}
	writeFile(t, filepath.Join(dist, "checksums.txt"), fields[0]+"  homer_linux_amd64.tar.gz\n")

	result := runProcess(t, repoRoot(), global, []string{
		"HOME=" + filepath.Join(root, "fake-home"),
		"LC_ALL=C",
		"LANG=C",
		"HOMER_INSTALL_BASE_URL=file://" + dist,
		"HOMER_INSTALL_PREFIX=" + filepath.Join(root, "prefix"),
	}, "sh", filepath.Join(repoRoot(), "install.sh"))
	if result.code != 0 {
		t.Fatalf("installer failed under LC_ALL=C (exit %d):\nstdout: %s\nstderr: %s", result.code, result.stdout, result.stderr)
	}
	if !strings.Contains(result.stdout, "homer") {
		t.Fatalf("installer produced no output under LC_ALL=C: %q", result.stdout)
	}
}
