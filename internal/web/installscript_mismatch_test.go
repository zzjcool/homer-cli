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

// TestInstallScriptMismatchedPlatformDoesNotCrash runs the platform-mismatch
// branch end-to-end (a darwin/arm64 machine simulated via a PATH-shadowed
// uname) against a stubbed GitHub Releases server. It cannot reproduce the
// bash 3.2 parsing bug itself — that is guarded statically by
// TestRenderedInstallScriptNoBareVarBeforeMultibyte — but it verifies the
// branch completes, cross-downloads the release archive without ever
// sending the hub token, and leaves an executable homer behind.
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
	var sawAuthHeader bool
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zzjcool/homer-cli/releases/latest/download/homer_darwin_arm64.tar.gz" {
			http.NotFound(w, r)
			return
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			sawAuthHeader = true
			t.Errorf("public release download must not send credentials, got Authorization: %s", auth)
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
	const githubReleaseBase = "https://github.com/zzjcool/homer-cli/releases/latest/download"
	if !strings.Contains(script, githubReleaseBase) {
		t.Fatalf("template no longer embeds %s; update this test", githubReleaseBase)
	}
	script = strings.Replace(script, githubReleaseBase,
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
	if sawAuthHeader {
		t.Fatal("the hub token must never reach the public release download")
	}
	if _, err := os.Stat(filepath.Join(binDir, "homer-rel.tar.gz")); err == nil {
		t.Fatal("the release archive must be cleaned up after installation")
	}
}

// The fallback path: when the release download 404s (no archive for the
// platform) or the archive is unusable, the script must still complete with
// the source-build guidance instead of crashing.
func TestInstallScriptMismatchFallsBackToSourceBuild(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root: install.sh would write a real /etc/systemd/system unit")
	}
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "no archive for platform (404)",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.NotFound(w, r)
			},
		},
		{
			name: "corrupt archive",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("this is not gzip data"))
			},
		},
		{
			name: "archive without a root homer entry",
			handler: func(w http.ResponseWriter, r *http.Request) {
				gz := gzip.NewWriter(w)
				tw := tar.NewWriter(gz)
				hdr := &tar.Header{Name: "nested/homer", Mode: 0o755, Size: 4}
				if err := tw.WriteHeader(hdr); err != nil {
					t.Error(err)
				}
				if _, err := tw.Write([]byte("stub")); err != nil {
					t.Error(err)
				}
				_ = tw.Close()
				_ = gz.Close()
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fakeHome := filepath.Join(root, "home")
			homerHome := filepath.Join(root, "homer-home")
			binDir := filepath.Join(fakeHome, ".local", "bin")
			for _, dir := range []string{binDir, homerHome} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// A stale binary from an earlier install must not be mistaken
			// for a successful cross-download.
			stale := filepath.Join(binDir, "homer")
			if err := os.WriteFile(stale, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(tc.handler)
			t.Cleanup(server.Close)

			script := RenderInstallScript(server.URL, "linux", "amd64")
			const githubReleaseBase = "https://github.com/zzjcool/homer-cli/releases/latest/download"
			if !strings.Contains(script, githubReleaseBase) {
				t.Fatalf("template no longer embeds %s; update this test", githubReleaseBase)
			}
			script = strings.Replace(script, githubReleaseBase,
				server.URL+"/zzjcool/homer-cli/releases/latest/download", 1)
			scriptPath := filepath.Join(root, "install.sh")
			if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			shimDir := filepath.Join(root, "shim")
			if err := os.MkdirAll(shimDir, 0o755); err != nil {
				t.Fatal(err)
			}
			uname := "#!/bin/sh\ncase \"$1\" in -m) echo arm64 ;; *) echo Darwin ;; esac\n"
			if err := os.WriteFile(filepath.Join(shimDir, "uname"), []byte(uname), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", scriptPath, "--token", "hr_mismatch-code")
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
				t.Fatalf("install.sh must complete (exit 0) even when the release is unusable: %v\n%s", runErr, output)
			}
			if strings.Contains(output, "unbound variable") {
				t.Fatalf("bash 3.2 unbound-variable crash reproduced:\n%s", output)
			}
			if !strings.Contains(output, "平台不匹配") {
				t.Fatalf("mismatch notice missing:\n%s", output)
			}
			if !strings.Contains(output, "请从源码构建") {
				t.Fatalf("fallback guidance missing:\n%s", output)
			}
			if strings.Contains(output, "已从 GitHub Releases 安装") {
				t.Fatalf("a broken release download must not be reported as installed:\n%s", output)
			}
			if _, err := os.Stat(filepath.Join(binDir, "homer-rel.tar.gz")); err == nil {
				t.Fatal("the failed release archive must be cleaned up")
			}
		})
	}
}
