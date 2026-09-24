package adapter

import (
	"path/filepath"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
)

func TestResolveCategoryFilePathInverseRules(t *testing.T) {
	root := filepath.Join(t.TempDir(), "agent")
	tests := []struct {
		name     string
		category core.CategoryConfig
		relPath  string
		want     string
	}{
		{
			name:     "directory declaration",
			category: core.CategoryConfig{Paths: []string{"skills/"}},
			relPath:  "alpha/SKILL.md",
			want:     filepath.Join(root, "skills", "alpha", "SKILL.md"),
		},
		{
			name:     "single file basename",
			category: core.CategoryConfig{Paths: []string{"settings.json"}},
			relPath:  "settings.json",
			want:     filepath.Join(root, "settings.json"),
		},
		{
			name:     "nested single file keeps declaration path",
			category: core.CategoryConfig{Paths: []string{"config/settings.json"}},
			relPath:  "settings.json",
			want:     filepath.Join(root, "config", "settings.json"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveCategoryFilePath(root, test.category, test.relPath)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("path = %q, want %q", got, test.want)
			}
		})
	}
}

func TestResolveCategoryFilePathRejectsEscapeAndUnknown(t *testing.T) {
	category := core.CategoryConfig{Paths: []string{"skills/"}}
	for _, relPath := range []string{"../outside", "skills/../outside", "/absolute"} {
		t.Run(relPath, func(t *testing.T) {
			if _, err := ResolveCategoryFilePath("/tmp/agent", category, relPath); err == nil {
				t.Fatalf("ResolveCategoryFilePath(%q) unexpectedly succeeded", relPath)
			}
		})
	}
	if _, err := ResolveCategoryFilePath("/tmp/agent", core.CategoryConfig{Paths: []string{"settings.json"}}, "unknown.txt"); err == nil {
		t.Fatal("unknown single-file basename unexpectedly succeeded")
	}
}
