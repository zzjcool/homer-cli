package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallDistBaseURLArchiveSmoke(t *testing.T) {
	binary := buildHomer(t)
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer Install Review\n\temail = homer-install-review@example.invalid\n")
	dist := filepath.Join(root, "dist")
	archiveRoot := filepath.Join(root, "archive")
	if err := os.MkdirAll(archiveRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	binaryInArchive := filepath.Join(archiveRoot, "homer")
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryInArchive, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dist, "homer_linux_amd64.tar.gz")
	tar := runProcess(t, repoRoot(), global, nil, "tar", "-czf", archive, "-C", archiveRoot, "homer")
	if tar.code != 0 {
		t.Fatalf("tar archive failed: %s", tar.stderr)
	}
	checksum := runProcess(t, repoRoot(), global, nil, "sha256sum", archive)
	if checksum.code != 0 {
		t.Fatalf("sha256sum failed: %s", checksum.stderr)
	}
	fields := strings.Fields(checksum.stdout)
	if len(fields) == 0 {
		t.Fatalf("sha256sum output = %q", checksum.stdout)
	}
	writeFile(t, filepath.Join(dist, "checksums.txt"), fields[0]+"  homer_linux_amd64.tar.gz\n")

	prefix := filepath.Join(root, "prefix")
	fakeHome := filepath.Join(root, "fake-home")
	result := runProcess(t, repoRoot(), global, []string{
		"HOME=" + fakeHome,
		"HOMER_INSTALL_BASE_URL=file://" + dist,
		"HOMER_INSTALL_PREFIX=" + prefix,
	}, "sh", filepath.Join(repoRoot(), "install.sh"))
	if result.code != 0 {
		t.Fatalf("dist base-url install failed (exit %d): %s\n%s", result.code, result.stdout, result.stderr)
	}
	installed := filepath.Join(prefix, "homer")
	help := runProcess(t, repoRoot(), global, []string{"HOME=" + fakeHome}, installed, "--help")
	if help.code != 0 || !strings.Contains(help.stdout, "homer") {
		t.Fatalf("installed dist binary --help failed: %#v", help)
	}
}
