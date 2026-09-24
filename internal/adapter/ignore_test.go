package adapter_test

import (
	"testing"

	"github.com/zzjcool/homer-cli/internal/adapter"
)

func TestGlobMatchFrozenTable(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		path     string
		expected bool
	}{
		{name: "literal exact", pattern: "auth.json", path: "auth.json", expected: true},
		{name: "literal not nested", pattern: "auth.json", path: "sub/auth.json", expected: false},
		{name: "literal full", pattern: "auth.json", path: "auth.jsonx", expected: false},
		{name: "star suffix", pattern: "*.bak", path: "models.json.bak", expected: true},
		{name: "star does not cross slash", pattern: "*.bak", path: "a/b.bak", expected: false},
		{name: "two stars", pattern: "*.bak-*", path: "models.json.bak-predirect", expected: true},
		{name: "missing suffix", pattern: "*.bak-*", path: "models.json.bak", expected: false},
		{name: "star both sides", pattern: "*cache*", path: "mycache.json", expected: true},
		{name: "nested cache not a single path", pattern: "*cache*", path: "keep/index.js", expected: false},
		{name: "directory prefix child", pattern: "sessions/", path: "sessions/x", expected: true},
		{name: "directory prefix deep child", pattern: "sessions/", path: "sessions/a/b/c", expected: true},
		{name: "directory itself", pattern: "sessions/", path: "sessions", expected: true},
		{name: "directory boundary", pattern: "sessions/", path: "mysessions/x", expected: false},
		{name: "prefix star", pattern: "s*", path: "settings.json", expected: true},
		{name: "prefix star does not cross slash", pattern: "s*", path: "skills/foo", expected: false},
		{name: "multi-segment star", pattern: "s*/foo", path: "skills/foo", expected: true},
		{name: "question is literal", pattern: "a?b", path: "a?b", expected: true},
		{name: "question is not wildcard", pattern: "a?b", path: "axb", expected: false},
		{name: "double star is still one segment", pattern: "**", path: "ab", expected: true},
		{name: "double star does not cross slash", pattern: "**", path: "a/b", expected: false},
		{name: "double star nested one level", pattern: "**/x", path: "deep/x", expected: true},
		{name: "double star nested two levels", pattern: "**/x", path: "a/b/x", expected: false},
		{name: "empty pattern", pattern: "", path: "anything", expected: false},
		{name: "leading dot slash", pattern: "sessions/", path: "./sessions/x", expected: true},
		{name: "leading slash", pattern: "sessions/", path: "/sessions/x", expected: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := adapter.GlobMatch(test.path, test.pattern); got != test.expected {
				t.Fatalf("GlobMatch(%q, %q) = %v, want %v", test.path, test.pattern, got, test.expected)
			}
		})
	}
}

func TestMatchesIgnoreAndBareGlobGuard(t *testing.T) {
	patterns := []string{"auth.json", "sessions/", "*.bak-*", "*.log"}
	cases := map[string]bool{
		"auth.json":                 true,
		"sessions/2026/foo.jsonl":   true,
		"models.json.bak-predirect": true,
		"pi-tui-crash.log":          true,
		"nested/pi-tui-crash.log":   false,
		"sessions.json":             false,
		"auth.json.example":         false,
		"settings.json":             false,
		"skills/foo/SKILL.md":       false,
		"models.json.bak":           false,
	}
	for path, want := range cases {
		if got := adapter.MatchesIgnore(path, patterns); got != want {
			t.Errorf("MatchesIgnore(%q) = %v, want %v", path, got, want)
		}
	}
	if adapter.MatchesIgnore("anything", nil) {
		t.Fatal("nil ignore patterns must not match")
	}

	for _, pattern := range []string{"*", "**", "*/", "**/", "./*", "/*", ".//*", "/**/"} {
		if !adapter.IsBareGlob(pattern) {
			t.Errorf("IsBareGlob(%q) = false, want true", pattern)
		}
	}
	for _, pattern := range []string{"skills/*", "skills/*/inner", "skills/agent-browser", "a*/", ""} {
		if adapter.IsBareGlob(pattern) {
			t.Errorf("IsBareGlob(%q) = true, want false", pattern)
		}
	}
}
