package web

import (
	"archive/tar"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestInstallScriptMismatchedPlatformDoesNotCrash reproduces the production
// incident: a darwin machine piping install.sh into macOS /bin/sh (bash 3.2
// without a UTF-8 locale) died with "GOARCH_ACTUAL...: unbound variable" on
// the platform-mismatch echo, because the bare variable reference was
// followed by a fullwidth comma. The mismatch branch must instead complete:
// cross-download the GitHub Release archive for the actual platform (stubbed
// here with a local server) and leave a working homer behind.
func TestInstallScriptMismatchedPlatformDoesNotCrash(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root: install.sh would write a real /etc/systemd/system unit")
	}
	root := t.TempDir()
	fakeHome := filepath.Join(root, "home")
	homerHome := filepath.Join(root, "homer-home")
	binDir := filepath.Join(fakeHome, ".local", "bin")
	for _, dir := range []string{binDir, homerHome} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// A release archive with an executable homer at the archive root,
	// laid out like the goreleaser archives on GitHub Releases.
	const stubBinary = "#!/bin/sh\nexit 0\n"
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zzjcool/homer-cli/releases/latest/download/homer_darwin_arm64.tar.gz" {
			http.NotFound(w, r)
			return
		}
		gz := gzip.NewWriter(w)
		tw := tar.NewWriter(gz)
		hdr := &tar.Header{Name: "homer", Mode: 0o755, Size: int64(len(stubBinary))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Error(err)
		}
		if _, err := tw.Write([]byte(stubBinary)); err != nil {
			t.Error(err)
		}
		_ = tw.Close()
		_ = gz.Close()
	}))
	t.Cleanup(releaseServer.Close)

	script := RenderInstallScript(releaseServer.URL, "linux", "amd64")
	script = strings.Replace(script,
		"https://github.com/zzjcool/homer-cli/releases/latest/download",
		releaseServer.URL+"/zzjcool/homer-cli/releases/latest/download", 1)
	scriptPath := filepath.Join(root, "install.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	// Stand in for a darwin/arm64 machine: shadow uname in PATH.
	shimDir := filepath.Join(root, "shim")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	uname := "#!/bin/sh\ncase \"$1\" in -m) echo arm64 ;; *) echo Darwin ;; esac\n"
	if err := os.WriteFile(filepath.Join(shimDir, "uname"), []byte(uname), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", scriptPath, "--token", "hr_mismatch-code")
	// XDG_RUNTIME_DIR / DBUS_SESSION_BUS_ADDRESS deliberately not passed so
	// the user-systemd branch cannot be taken on a systemd host.
	cmd.Env = []string{
		"HOME=" + fakeHome,
		"HOMER_HOME=" + homerHome,
		"PATH=" + shimDir + ":" + binDir + ":/usr/bin:/bin",
	}
	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() {
		out, runErr = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("install.sh did not return within 30s: %s", out)
	}
	output := string(out)
	if runErr != nil {
		t.Fatalf("install.sh failed on a mismatched platform: %v\n%s", runErr, output)
	}
	if strings.Contains(output, "unbound variable") {
		t.Fatalf("bash 3.2 unbound-variable crash reproduced:\n%s", output)
	}
	if !strings.Contains(output, "平台不匹配") {
		t.Fatalf("mismatch notice missing:\n%s", output)
	}
	if !strings.Contains(output, "homer_darwin_arm64.tar.gz") {
		t.Fatalf("cross-platform release download was not attempted:\n%s", output)
	}
	installed := filepath.Join(binDir, "homer")
	info, err := os.Stat(installed)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		t.Fatalf("homer must be installed executable from the release archive: %v", err)
	}
}
