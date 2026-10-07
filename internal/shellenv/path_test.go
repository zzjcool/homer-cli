package shellenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLoginPATHIgnoresProfileNoise(t *testing.T) {
	got := parseLoginPATH("some profile banner\n__HOMER_PATH__/home/linuxbrew/.linuxbrew/bin:/usr/bin\n")
	if got != "/home/linuxbrew/.linuxbrew/bin:/usr/bin" {
		t.Fatalf("PATH = %q", got)
	}
	if parseLoginPATH("no marker") != "" {
		t.Fatal("missing marker should yield an empty PATH")
	}
}

func TestCommandPATHFindsBrewOutsideTheAgentPath(t *testing.T) {
	home := "/home/user"
	brew := "/home/linuxbrew/.linuxbrew/bin"
	local := filepath.Join(home, ".local", "bin")
	exists := func(dir string) bool {
		return dir == brew || dir == local || dir == "/usr/bin" || dir == "/bin"
	}
	got := commandPATH("/usr/bin:/bin", home, "", exists)
	want := strings.Join([]string{brew, local, "/usr/bin", "/bin"}, string(os.PathListSeparator))
	if got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
	if again := commandPATH(want, home, "", exists); again != want {
		t.Fatalf("PATH duplicated: %q", again)
	}
}

func TestLookInPathUsesGivenPATH(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "pi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := LookIn("pi", dir)
	if err != nil || got != bin {
		t.Fatalf("LookIn = %q, %v", got, err)
	}
	if _, err := LookIn("pi", "/nonexistent"); err == nil {
		t.Fatal("expected a missing command")
	}
}

func TestPathReadsLoginPATHEveryCall(t *testing.T) {
	previous := ReadLoginPATH
	calls := 0
	ReadLoginPATH = func() string {
		calls++
		return "/usr/bin:/bin"
	}
	t.Cleanup(func() { ReadLoginPATH = previous })
	_ = Path()
	_ = Path()
	if calls != 2 {
		t.Fatalf("login PATH reads = %d, want 2", calls)
	}
}
