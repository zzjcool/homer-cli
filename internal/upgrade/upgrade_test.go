package upgrade

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsNewer(t *testing.T) {
	cases := []struct {
		candidate string
		current   string
		want      bool
	}{
		{"v1.3.1", "v1.3.0", true},
		{"v1.4", "v1.3.9", true},
		{"v2.0", "v1.9.9", true},
		{"v1.3.0", "v1.3.0", false},
		{"v1.3.0", "v1.3.1", false},
		{"1.3.1", "v1.3.0", true}, // v prefix is optional
		{"v1.3.0.1", "v1.3.0", true},
		{"v1.3.0", "v1.3.0.1", false},
		{"", "v1.3.0", false},
		{"v1.3.0", "", false},
		{"garbage", "v1.3.0", false},
	}
	for _, tc := range cases {
		if got := IsNewer(tc.candidate, tc.current); got != tc.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", tc.candidate, tc.current, got, tc.want)
		}
	}
}

// fetchRecorder serves canned files so InstallArchive can be tested without
// the network.
func fetchRecorder(files map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
}

func TestClientFetchLatestParsesTag(t *testing.T) {
	server := fetchRecorder(map[string]string{
		"/releases/latest": `{"tag_name":"v9.9.9","prerelease":false}`,
	})
	defer server.Close()
	client := &Client{HTTP: server.Client(), BaseURL: server.URL + "/releases/latest"}
	latest, err := client.FetchLatest()
	if err != nil {
		t.Fatalf("FetchLatest: %v", err)
	}
	if latest.TagName != "v9.9.9" {
		t.Fatalf("tag = %q", latest.TagName)
	}
}

func TestClientFetchLatestHandlesHTTPErrors(t *testing.T) {
	server := fetchRecorder(map[string]string{})
	defer server.Close()
	client := &Client{HTTP: server.Client(), BaseURL: server.URL + "/releases/latest"}
	if _, err := client.FetchLatest(); err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestArchiveNameMatchesGoreleaser(t *testing.T) {
	name := ArchiveName()
	if !strings.HasPrefix(name, "homer_") || !strings.HasSuffix(name, ".tar.gz") {
		t.Fatalf("archive name = %q", name)
	}
	if !strings.Contains(name, "_linux_") && !strings.Contains(name, "_darwin_") {
		t.Fatalf("archive name missing OS: %q", name)
	}
}

func TestDownloaderChecksumMismatchFails(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "payload")
	if err := os.WriteFile(archive, []byte("real archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	bogus := filepath.Join(dir, "bogus")
	if err := os.WriteFile(bogus, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	checksums := filepath.Join(dir, "checksums.txt")
	name := "homer_linux_amd64.tar.gz"
	// Entry deliberately does not match the tampered bytes.
	if err := os.WriteFile(checksums, []byte("0000000000000000000000000000000000000000000000000000000000000000  "+name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksum(bogus, checksums, name); err == nil {
		t.Fatal("expected checksum mismatch error")
	}
}

func TestVerifyChecksumMissingEntryFails(t *testing.T) {
	dir := t.TempDir()
	bogus := filepath.Join(dir, "bogus")
	if err := os.WriteFile(bogus, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	checksums := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(checksums, []byte("abc  other_archive.tar.gz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksum(bogus, checksums, "homer_linux_amd64.tar.gz"); err == nil {
		t.Fatal("expected missing-entry error")
	}
}
