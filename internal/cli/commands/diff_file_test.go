package commands

import (
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
)

func TestReadLocalFileReturnsMachineText(t *testing.T) {
	sources := DriftSources{Local: []core.AdapterSnapshot{{
		AdapterID: "pi",
		Categories: []core.CategorySnapshot{{
			Category: "agents",
			Files: core.SnapshotFiles{
				"send.md":        {Content: "left\n"},
				"settings.json":  {Content: "{\"theme\":\"dark\"}\n"},
				"npm:pi-lens.md": {Content: "keep\n"},
			},
		}},
	}}}
	got, err := ReadLocalFile(DiffOptions{Adapter: "pi", Category: "agents", Path: "agents/send.md"}, sources)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LocalOK || got.Local != "left\n" || got.Binary {
		t.Fatalf("send.md = %+v", got)
	}
	keyed, err := ReadLocalFile(DiffOptions{Adapter: "pi", Category: "agents", Path: "settings.json:theme"}, sources)
	if err != nil {
		t.Fatal(err)
	}
	if !keyed.LocalOK || !strings.Contains(keyed.Local, "dark") {
		t.Fatalf("key path = %+v", keyed)
	}
	named, err := ReadLocalFile(DiffOptions{Adapter: "pi", Category: "agents", Path: "npm:pi-lens.md"}, sources)
	if err != nil {
		t.Fatal(err)
	}
	if !named.LocalOK || named.Local != "keep\n" {
		t.Fatalf("colon name = %+v", named)
	}
	missing, err := ReadLocalFile(DiffOptions{Adapter: "pi", Category: "agents", Path: "gone.md"}, sources)
	if err != nil || missing.LocalOK {
		t.Fatalf("missing = %+v, err = %v", missing, err)
	}
	binary, err := ReadLocalFile(DiffOptions{Adapter: "pi", Category: "agents", Path: "send.md"}, DriftSources{Local: []core.AdapterSnapshot{{
		AdapterID: "pi",
		Categories: []core.CategorySnapshot{{
			Category: "agents",
			Files:    core.SnapshotFiles{"send.md": {Content: "a\x00b"}},
		}},
	}}})
	if err != nil || !binary.LocalOK || !binary.Binary || binary.Local != "" {
		t.Fatalf("binary = %+v, err = %v", binary, err)
	}
	if _, err := ReadLocalFile(DiffOptions{Adapter: "pi", Category: "agents", Path: "../secret"}, sources); err == nil {
		t.Fatal("parent path must be rejected")
	}
}
