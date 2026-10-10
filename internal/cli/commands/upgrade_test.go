package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// nativeHeader returns the leading bytes of a binary the CURRENT platform
// can execute: a Mach-O header on darwin (with the right cputype), an ELF
// header on linux (with the right e_machine).
func nativeHeader() []byte {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return []byte{0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	}
	if runtime.GOOS == "darwin" {
		return []byte{0xcf, 0xfa, 0xed, 0xfe, 0x07, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	}
	// linux ELF64 header: magic, EI_CLASS=2 (64-bit), EI_DATA=1 (LE),
	// then padding up to e_type/e_machine at 16/18.
	header := []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0}
	if runtime.GOARCH == "amd64" {
		header[18] = 0x3e
	} else {
		header[18] = 0xb7
	}
	return header
}

// foreignHeader returns a well-formed header for a platform/arch the
// CURRENT machine cannot execute.
func foreignHeader() []byte {
	if runtime.GOOS == "darwin" {
		// An ELF on a Mac.
		header := []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0x3e, 0x00}
		return header
	}
	// A Mach-O on linux.
	return []byte{0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
}

// wrongArchHeader returns a header of the CURRENT OS but the OTHER CPU
// architecture (e.g. linux/amd64 ELF reaching linux/arm64): the hole a
// marker-less old hub leaves open and checkExecutable's arch check closes.
func wrongArchHeader() []byte {
	header := nativeHeader()
	if runtime.GOOS == "darwin" {
		if runtime.GOARCH == "arm64" { // amd64 Mach-O on an arm64 Mac
			header[4] = 0x07
		} else { // arm64 Mach-O on an amd64 Mac
			header[4] = 0x0c
		}
		return header
	}
	if runtime.GOARCH == "amd64" { // arm64 ELF on amd64 linux
		header[18] = 0xb7
	} else { // amd64 ELF on arm64 linux
		header[18] = 0x3e
	}
	return header
}

// biggerThanUpgradeFloor pads a header-prefixed payload past the
// minimum upgrade size so the size floor does not reject it (mirrors
// the production minUpgradeBytes constant).
func fillPad(header []byte, total int) []byte {
	payload := append([]byte{}, header...)
	for len(payload) < total {
		payload = append(payload, byte(len(payload)%251))
	}
	return payload
}

func targetBinary(t *testing.T) string {
	t.Helper()
	self := filepath.Join(t.TempDir(), "homer")
	if err := os.WriteFile(self, []byte("old binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return self
}

const upgradeFloorForTests = 1 << 20 // mirrors the production minUpgradeBytes floor

// TestDownloadAndReplaceHappyPath: a same-platform payload replaces the
// target and reports the size and hash of exactly what was served.
func TestDownloadAndReplaceHappyPath(t *testing.T) {
	self := targetBinary(t)
	payload := fillPad(nativeHeader(), upgradeFloorForTests+len(nativeHeader())+123)
	dataPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("goos"); got != runtime.GOOS {
			t.Errorf("request goos = %q, want %q", got, runtime.GOOS)
		}
		if got := r.URL.Query().Get("goarch"); got != runtime.GOARCH {
			t.Errorf("request goarch = %q, want %q", got, runtime.GOARCH)
		}
		_, _ = w.Write(payload)
	}))
	defer dataPlane.Close()

	report := DownloadAndReplace(context.Background(), dataPlane.Client(), dataPlane.URL, "secret", self)
	if !report.OK || report.Status != "upgraded" {
		t.Fatalf("report = %+v", report)
	}
	got, err := os.ReadFile(self)
	if err != nil || len(got) != len(payload) || string(got[:20]) != string(payload[:20]) {
		t.Fatalf("replaced binary differs: %d bytes err=%v", len(got), err)
	}
	sum := sha256.Sum256(got)
	if report.Hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("report.Hash = %s, want sha256 of the written file", report.Hash)
	}
	if report.SizeBytes != int64(len(payload)) {
		t.Fatalf("SizeBytes = %d, want %d", report.SizeBytes, len(payload))
	}
	if _, err := os.Stat(self + ".upgrade"); !os.IsNotExist(err) {
		t.Fatalf("staging file survived: %v", err)
	}
}

// TestDownloadAndReplaceHonorsContext: an expired or cancelled context
// must abort the download and leave the binary untouched — the hub has
// already reported the task as timed out; a late replace would swap the
// binary behind its back.
func TestDownloadAndReplaceHonorsContext(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		self := targetBinary(t)
		payload := fillPad(nativeHeader(), upgradeFloorForTests)
		dataPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(200 * time.Millisecond)
			_, _ = w.Write(payload)
		}))
		defer dataPlane.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		report := DownloadAndReplace(ctx, dataPlane.Client(), dataPlane.URL, "secret", self)
		if report.OK {
			t.Fatalf("an expired context must not upgrade: %+v", report)
		}
		got, err := os.ReadFile(self)
		if err != nil || string(got) != "old binary\n" {
			t.Fatalf("binary must be untouched: %q err=%v", got, err)
		}
		if _, err := os.Stat(self + ".upgrade"); !os.IsNotExist(err) {
			t.Fatalf("no staging file may survive: %v", err)
		}
	})
	t.Run("cancelled mid-download", func(t *testing.T) {
		self := targetBinary(t)
		payload := fillPad(nativeHeader(), minUpgradeBytes)
		dataPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(payload[:1000])
			// Hold the rest back until the client goes away.
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}))
		defer dataPlane.Close()

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(150 * time.Millisecond)
			cancel()
		}()
		start := time.Now()
		report := DownloadAndReplace(ctx, dataPlane.Client(), dataPlane.URL, "secret", self)
		if report.OK {
			t.Fatalf("a cancelled context must not upgrade: %+v", report)
		}
		if elapsed := time.Since(start); elapsed > 4*time.Second {
			t.Fatalf("cancellation took %v; the download must abort promptly", elapsed)
		}
		got, err := os.ReadFile(self)
		if err != nil || string(got) != "old binary\n" {
			t.Fatalf("binary must be untouched: %q err=%v", got, err)
		}
		if _, err := os.Stat(self + ".upgrade"); !os.IsNotExist(err) {
			t.Fatalf("no staging file may survive: %v", err)
		}
	})
}

// TestDownloadAndReplaceRefusesMarkerMismatch: the X-Homer-Platform marker
// naming another platform must leave the binary untouched (defense 1).
func TestDownloadAndReplaceRefusesMarkerMismatch(t *testing.T) {
	self := targetBinary(t)
	dataPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Homer-Platform", "windows/386")
		_, _ = w.Write(fillPad(foreignHeader(), minUpgradeBytes))
	}))
	defer dataPlane.Close()

	report := DownloadAndReplace(context.Background(), dataPlane.Client(), dataPlane.URL, "secret", self)
	if report.OK || report.Status != "platform-mismatch" {
		t.Fatalf("report = %+v, want platform-mismatch", report)
	}
	got, err := os.ReadFile(self)
	if err != nil || string(got) != "old binary\n" {
		t.Fatalf("binary must be untouched: %q err=%v", got, err)
	}
	if _, err := os.Stat(self + ".upgrade"); !os.IsNotExist(err) {
		t.Fatalf("no staging file may survive a refusal: %v", err)
	}
}

// TestDownloadAndReplaceRefusesForeignMagic: an OLD hub ignores the platform
// query and streams its own binary with no marker — the exact 2026-10-10
// Mac brick shape. Defense 2 (payload header) must refuse it even without
// the marker.
func TestDownloadAndReplaceRefusesForeignMagic(t *testing.T) {
	self := targetBinary(t)
	dataPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No X-Homer-Platform: exactly what the incident hub served.
		_, _ = w.Write(fillPad(foreignHeader(), minUpgradeBytes))
	}))
	defer dataPlane.Close()

	report := DownloadAndReplace(context.Background(), dataPlane.Client(), dataPlane.URL, "secret", self)
	if report.OK || report.Status != "platform-mismatch" {
		t.Fatalf("report = %+v, want platform-mismatch against a marker-less wrong binary", report)
	}
	if !strings.Contains(report.Note, "拒绝替换") {
		t.Fatalf("note should explain the refusal: %q", report.Note)
	}
	got, err := os.ReadFile(self)
	if err != nil || string(got) != "old binary\n" {
		t.Fatalf("binary must be untouched: %q err=%v", got, err)
	}
	if _, err := os.Stat(self + ".upgrade"); !os.IsNotExist(err) {
		t.Fatalf("no staging file may survive a refusal: %v", err)
	}
}

// TestDownloadAndReplaceRefusesWrongArch: a marker-less binary of the SAME
// OS but the WRONG CPU architecture is the incident shape rotated to the
// arch axis; the header's architecture field must refuse it.
func TestDownloadAndReplaceRefusesWrongArch(t *testing.T) {
	self := targetBinary(t)
	dataPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fillPad(wrongArchHeader(), minUpgradeBytes))
	}))
	defer dataPlane.Close()

	report := DownloadAndReplace(context.Background(), dataPlane.Client(), dataPlane.URL, "secret", self)
	if report.OK || report.Status != "platform-mismatch" {
		t.Fatalf("report = %+v, want platform-mismatch for a same-OS wrong-arch binary", report)
	}
	got, err := os.ReadFile(self)
	if err != nil || string(got) != "old binary\n" {
		t.Fatalf("binary must be untouched: %q err=%v", got, err)
	}
}

// TestDownloadAndReplaceRefusesTinyPayloads: a valid header with nothing
// behind it (or a 4-byte magic-only body) would write a file the machine
// cannot boot; the size floor and Content-Length checks refuse it.
func TestDownloadAndReplaceRefusesTinyPayloads(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"magic only", nativeHeader()},
		{"header plus crumbs", append(append([]byte{}, nativeHeader()...), []byte("tail")...)},
		{"html login page", []byte("<html>login</html>")},
		{"short body", []byte("ab")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			self := targetBinary(t)
			dataPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(tc.body)
			}))
			defer dataPlane.Close()

			report := DownloadAndReplace(context.Background(), dataPlane.Client(), dataPlane.URL, "secret", self)
			if report.OK {
				t.Fatalf("a %d-byte payload must never upgrade: %+v", len(tc.body), report)
			}
			got, err := os.ReadFile(self)
			if err != nil || string(got) != "old binary\n" {
				t.Fatalf("binary must be untouched: %q err=%v", got, err)
			}
			if _, err := os.Stat(self + ".upgrade"); !os.IsNotExist(err) {
				t.Fatalf("no staging file may survive: %v", err)
			}
		})
	}
}

// TestDownloadAndReplaceTruncatedBodyKeepsOldBinary: a server that promises
// more than it sends must fail the Content-Length check, not rename a
// half-written binary into place.
func TestDownloadAndReplaceTruncatedBodyKeepsOldBinary(t *testing.T) {
	self := targetBinary(t)
	payload := fillPad(nativeHeader(), upgradeFloorForTests)
	dataPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Promise far more than we send: the Content-Length check must
		// fail the upgrade instead of renaming a half-written binary.
		w.Header().Set("Content-Length", "999999999")
		_, _ = w.Write(payload)
	}))
	defer dataPlane.Close()

	report := DownloadAndReplace(context.Background(), dataPlane.Client(), dataPlane.URL, "secret", self)
	if report.OK || report.Status != "error" {
		t.Fatalf("report = %+v, want a size mismatch error", report)
	}
	got, err := os.ReadFile(self)
	if err != nil || string(got) != "old binary\n" {
		t.Fatalf("binary must be untouched: %q err=%v", got, err)
	}
	if _, err := os.Stat(self + ".upgrade"); !os.IsNotExist(err) {
		t.Fatalf("no staging file may survive: %v", err)
	}
}

// TestDownloadAndReplaceSurfacesHTTPErrors: a refused download reports the
// status code instead of touching the target.
func TestDownloadAndReplaceSurfacesHTTPErrors(t *testing.T) {
	self := targetBinary(t)
	dataPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer dataPlane.Close()

	report := DownloadAndReplace(context.Background(), dataPlane.Client(), dataPlane.URL, "secret", self)
	if report.OK || report.Status != "error" || !strings.Contains(report.Note, "401") {
		t.Fatalf("report = %+v, want an error carrying HTTP 401", report)
	}
	got, err := os.ReadFile(self)
	if err != nil || string(got) != "old binary\n" {
		t.Fatalf("binary must be untouched: %q err=%v", got, err)
	}
}

// TestCheckExecutable pins the format and architecture tables for both
// supported platforms regardless of the host running the test.
func TestCheckExecutable(t *testing.T) {
	// darwin thin Mach-O headers, per endianness and bitness.
	darwinThin := []struct {
		name   string
		header []byte
	}{
		{"macho-64-le", []byte{0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
		{"macho-64-le-amd64", []byte{0xcf, 0xfa, 0xed, 0xfe, 0x07, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
	}
	for _, tc := range darwinThin {
		if err := checkExecutable("darwin", "arm64", tc.header); err != nil {
			if tc.name == "macho-64-le" {
				t.Errorf("darwin arm64 %s: %v", tc.name, err)
			}
		}
	}
	// An amd64 Mach-O on an arm64 darwin must fail the arch check.
	amd64MachO := []byte{0xcf, 0xfa, 0xed, 0xfe, 0x07, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if err := checkExecutable("darwin", "arm64", amd64MachO); err == nil {
		t.Error("an amd64 Mach-O must not pass as darwin/arm64")
	}
	if err := checkExecutable("darwin", "amd64", amd64MachO); err != nil {
		t.Errorf("amd64 Mach-O on amd64 darwin: %v", err)
	}
	// Fat/universal containers pass the container-format check.
	fat := []byte{0xca, 0xfe, 0xba, 0xbe, 0, 0, 0, 1, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0}
	if err := checkExecutable("darwin", "arm64", fat); err != nil {
		t.Errorf("fat Mach-O: %v", err)
	}
	// linux ELF.
	elfAmd64 := []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0x3e, 0x00}
	if err := checkExecutable("linux", "amd64", elfAmd64); err != nil {
		t.Errorf("linux amd64 elf: %v", err)
	}
	if err := checkExecutable("linux", "arm64", elfAmd64); err == nil {
		t.Error("an amd64 ELF must not pass as linux/arm64")
	}
	// Cross-format and garbage.
	if err := checkExecutable("darwin", "arm64", elfAmd64); err == nil {
		t.Error("an ELF must not pass as darwin-executable")
	}
	if err := checkExecutable("linux", "amd64", amd64MachO); err == nil {
		t.Error("a Mach-O must not pass as linux-executable")
	}
	if err := checkExecutable("darwin", "arm64", []byte{1, 2, 3}); err == nil {
		t.Error("a short read must not pass")
	}
}
