package pair

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectTailcatWithInjectedPATH(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "tailcat")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got, err := DetectTailcat(nil)
	if err != nil {
		t.Fatalf("DetectTailcat existing = %q, %v", got, err)
	}
	if got != binary {
		t.Fatalf("DetectTailcat path = %q, want %q", got, binary)
	}
}

func TestDetectTailcatMissingReportsInstallHint(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	got, err := DetectTailcat(nil)
	if got != "" || !errors.Is(err, ErrNoTailcat) {
		t.Fatalf("DetectTailcat missing = %q, %v", got, err)
	}
	if !strings.Contains(TailcatInstallHint, "https://github.com/tailscale/tailcat") || !strings.Contains(TailcatInstallHint, "homer secret push") || !strings.Contains(TailcatInstallHint, "homer secret pull") {
		t.Fatalf("TailcatInstallHint = %q", TailcatInstallHint)
	}
}

func TestDetectTailcatUsesInjectedResolver(t *testing.T) {
	want := "/custom/tailcat"
	got, err := DetectTailcat(func(name string) (string, error) {
		if name != "tailcat" {
			t.Fatalf("lookPath name = %q, want tailcat", name)
		}
		return want, nil
	})
	if err != nil || got != want {
		t.Fatalf("injected DetectTailcat = %q, %v", got, err)
	}
}
