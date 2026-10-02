package sshkey

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHomerExampleKeyMaterial1234567890 homer"

func TestFetchUsesEnvironmentBase(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/e2euser.keys" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(sampleKey + "\n"))
	}))
	defer server.Close()
	if keysBaseURL != "" {
		t.Fatal("package base URL leaked into this test")
	}
	t.Setenv("HOMER_GITHUB_KEYS_BASE", server.URL)
	keys, err := FetchGitHubKeys(context.Background(), "e2euser")
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys = %#v err=%v", keys, err)
	}
}

func TestParseKeepsOnlyPublicKeys(t *testing.T) {
	text := "" +
		"# comment\n" +
		sampleKey + "\n" +
		"command=\"evil\" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHomerExampleKeyMaterial1234567890\n" +
		"not-a-key\n" +
		"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQexample\n"
	got := Parse(text)
	if len(got) != 2 {
		t.Fatalf("keys = %#v", got)
	}
	if got[0] != sampleKey {
		t.Fatalf("first = %q", got[0])
	}
}

func TestFetchGitHubKeys(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/zzjcool.keys":
			_, _ = w.Write([]byte(sampleKey + "\n"))
		case "/empty.keys":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	previous := keysBaseURL
	keysBaseURL = server.URL
	t.Cleanup(func() { keysBaseURL = previous })

	keys, err := FetchGitHubKeys(context.Background(), "zzjcool")
	if err != nil || len(keys) != 1 || keys[0] != sampleKey {
		t.Fatalf("keys = %#v err=%v", keys, err)
	}
	if _, err := FetchGitHubKeys(context.Background(), "missing"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("missing user err = %v", err)
	}
	if _, err := FetchGitHubKeys(context.Background(), "empty"); err == nil || !strings.Contains(err.Error(), "没有公钥") {
		t.Fatalf("empty user err = %v", err)
	}
	if _, err := FetchGitHubKeys(context.Background(), "../etc"); err == nil {
		t.Fatal("path-like username was accepted")
	}
}

func TestInstallReplacesOnlyTheMarkedBlock(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(sshDir, 0o755); err != nil {
		t.Fatal(err)
	}
	other := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAlreadyThereKeyMaterial1234567890 laptop"
	if err := os.WriteFile(filepath.Join(sshDir, "authorized_keys"), []byte(other+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	first := Install(home, "zzjcool", []string{sampleKey, "not-a-key"})
	if !first.OK || first.Installed != 1 {
		t.Fatalf("first = %+v", first)
	}
	info, err := os.Stat(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(sshDir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %o", dirInfo.Mode().Perm())
	}

	secondKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIReplacementKeyMaterial12345678901 zzjcool"
	second := Install(home, "zzjcool", []string{secondKey})
	if !second.OK {
		t.Fatalf("second = %+v", second)
	}
	body, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, other) {
		t.Fatalf("pre-existing key was dropped:\n%s", text)
	}
	if strings.Contains(text, sampleKey) {
		t.Fatalf("old managed key still present:\n%s", text)
	}
	if strings.Count(text, "# BEGIN homer github zzjcool") != 1 {
		t.Fatalf("block was duplicated:\n%s", text)
	}
	if !strings.Contains(text, secondKey) {
		t.Fatalf("new key missing:\n%s", text)
	}
}
