package commands

import (
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/engine"
)

// The hub's wire snapshot knows neither a category's sync mode nor its
// excludeKeys. A merge-mode JSON file that is identical on every side must
// not be judged as a pending pull just because the center copy arrived as a
// raw "file" entry.
func TestNormalizeHubRemoteMergeFileIsNotDrift(t *testing.T) {
	const content = "{\n  \"providers\": {\n    \"a\": {\"apiKey\": \"x\"}\n  }\n}\n"
	config := &core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"pi": {Categories: map[string]core.CategoryConfig{
				"models": {Paths: []string{"models.json"}, Mode: core.SyncModeMerge, ExcludeKeys: []string{"apiKeys"}},
			}},
		},
	}
	entry := func(kind string) core.SnapshotFiles {
		return core.SnapshotFiles{"models.json": {Kind: kind, Content: content}}
	}
	snap := func(kind string, mode core.SyncMode) []core.AdapterSnapshot {
		return []core.AdapterSnapshot{{AdapterID: "pi", Categories: []core.CategorySnapshot{
			{AdapterID: "pi", Category: "models", Mode: mode, Files: entry(kind)},
		}}}
	}
	base := stripExcludedSnapshots(snap("json", core.SyncModeMerge), *config)
	local := base
	// What the agent used to compare against: the raw wire form.
	raw := snap("file", core.SyncModeMirror)

	count := func(remote []core.AdapterSnapshot) (pull, conflicts int) {
		for _, drift := range engine.ComputeDrift(base, local, remote) {
			pull += drift.Pull
			conflicts += drift.Conflicts
		}
		return
	}
	if pull, _ := count(raw); pull == 0 {
		t.Skip("raw wire form no longer drifts; the normalization is not needed")
	}
	if pull, conflicts := count(NormalizeHubRemote(raw, config)); pull != 0 || conflicts != 0 {
		t.Fatalf("identical merge file drifted after normalization: pull=%d conflicts=%d", pull, conflicts)
	}
}
