package sync

import (
	"sort"

	"github.com/zzjcool/homer-cli/internal/core"
)

// FirstContactMode is the home/first-contact three-way choice.
type FirstContactMode string

const (
	FirstContactPull  FirstContactMode = "pull"
	FirstContactMerge FirstContactMode = "merge"
	FirstContactSkip  FirstContactMode = "skip"
)

// FirstContactPlan mirrors PullPlan while retaining the selected mode for the
// home command's report.
type FirstContactPlan struct {
	Mode    FirstContactMode
	Actions []PullAction
}

// EmptyBaseSnapshots creates a config-shaped base with an empty file map for
// every enabled adapter/category. Map-backed Go config is sorted so the result
// is deterministic even though JSON object insertion order is not retained by
// the frozen Go config structs.
func EmptyBaseSnapshots(config core.HomerConfig) []core.AdapterSnapshot {
	adapterIDs := make([]string, 0, len(config.Adapters))
	for adapterID := range config.Adapters {
		adapterIDs = append(adapterIDs, adapterID)
	}
	sort.Strings(adapterIDs)

	out := make([]core.AdapterSnapshot, 0, len(adapterIDs))
	for _, adapterID := range adapterIDs {
		adapterConfig := config.Adapters[adapterID]
		if adapterConfig.Enabled != nil && !*adapterConfig.Enabled {
			continue
		}
		categoryNames := make([]string, 0, len(adapterConfig.Categories))
		for category := range adapterConfig.Categories {
			categoryNames = append(categoryNames, category)
		}
		sort.Strings(categoryNames)
		categories := make([]core.CategorySnapshot, 0, len(categoryNames))
		for _, categoryName := range categoryNames {
			categoryConfig := adapterConfig.Categories[categoryName]
			if categoryConfig.Enabled != nil && !*categoryConfig.Enabled {
				continue
			}
			categories = append(categories, core.CategorySnapshot{
				AdapterID: adapterID,
				Category:  categoryName,
				Mode:      categoryConfig.Mode,
				Files:     core.SnapshotFiles{},
			})
		}
		out = append(out, core.AdapterSnapshot{AdapterID: adapterID, Categories: categories})
	}
	return out
}

func emptyBaseSnapshots(config core.HomerConfig) []core.AdapterSnapshot {
	return EmptyBaseSnapshots(config)
}

// PlanFirstContact translates pull, merge, and skip into actions. Pull uses an
// empty local side so every remote file is a write and no local-only file can
// be interpreted as a deletion. Merge uses an empty-object base for shared
// JSON-object files: this is the W5 ruling that preserves key-level union and
// merge-keys paths for first contact rather than losing them in base-missing
// file-level mirror fallback.
func PlanFirstContact(
	config core.HomerConfig,
	local, remote []core.AdapterSnapshot,
	mode FirstContactMode,
) FirstContactPlan {
	if mode == FirstContactSkip || mode == "skip" {
		return FirstContactPlan{Mode: mode, Actions: []PullAction{}}
	}
	if mode == FirstContactPull || mode == "pull" {
		base := orderBaseLike(EmptyBaseSnapshots(config), remote)
		return FirstContactPlan{Mode: mode, Actions: PlanPull(config, base, nil, remote).Actions}
	}
	if mode != FirstContactMerge && mode != "merge" {
		// The TypeScript union prevents this at compile time. Treat a malformed
		// runtime value as skip rather than applying an unsafe implicit pull.
		return FirstContactPlan{Mode: mode, Actions: []PullAction{}}
	}
	base := orderBaseLike(mergeBaseSnapshots(config, local, remote), local, remote)
	return FirstContactPlan{Mode: mode, Actions: PlanPull(config, base, local, remote).Actions}
}

func planFirstContact(config core.HomerConfig, local, remote []core.AdapterSnapshot, mode FirstContactMode) FirstContactPlan {
	return PlanFirstContact(config, local, remote, mode)
}

func mergeBaseSnapshots(config core.HomerConfig, local, remote []core.AdapterSnapshot) []core.AdapterSnapshot {
	localIndex := indexByAdapter(local)
	remoteIndex := indexByAdapter(remote)
	base := EmptyBaseSnapshots(config)

	for adapterIndex := range base {
		adapter := &base[adapterIndex]
		for categoryIndex := range adapter.Categories {
			category := &adapter.Categories[categoryIndex]
			if category.Mode != core.SyncModeMerge {
				continue
			}
			localCategory := findCategory(ptrSnapshot(localIndex[adapter.AdapterID]), category.Category)
			remoteCategory := findCategory(ptrSnapshot(remoteIndex[adapter.AdapterID]), category.Category)
			if localCategory == nil || remoteCategory == nil {
				continue
			}
			files := make(core.SnapshotFiles)
			for relPath, localEntry := range localCategory.Files {
				remoteEntry, exists := remoteCategory.Files[relPath]
				if !exists || !isMergeableEntry(localEntry) || !isMergeableEntry(remoteEntry) {
					continue
				}
				files[relPath] = core.SnapshotEntry{Kind: "json", Content: "{}"}
			}
			if len(files) > 0 {
				category.Files = files
			}
		}
	}
	return base
}

func ptrSnapshot(snapshot core.AdapterSnapshot) *core.AdapterSnapshot {
	if snapshot.AdapterID == "" && snapshot.Categories == nil {
		return nil
	}
	return &snapshot
}

func orderBaseLike(base []core.AdapterSnapshot, preferred ...[]core.AdapterSnapshot) []core.AdapterSnapshot {
	adapterOrder := make([]string, 0)
	seenAdapters := make(map[string]struct{})
	categoryOrder := make(map[string][]string)
	seenCategories := make(map[string]map[string]struct{})
	for _, group := range preferred {
		for _, adapter := range group {
			if _, seen := seenAdapters[adapter.AdapterID]; !seen {
				seenAdapters[adapter.AdapterID] = struct{}{}
				adapterOrder = append(adapterOrder, adapter.AdapterID)
			}
			seen, ok := seenCategories[adapter.AdapterID]
			if !ok {
				seen = make(map[string]struct{})
				seenCategories[adapter.AdapterID] = seen
			}
			for _, category := range adapter.Categories {
				if _, exists := seen[category.Category]; exists {
					continue
				}
				seen[category.Category] = struct{}{}
				categoryOrder[adapter.AdapterID] = append(categoryOrder[adapter.AdapterID], category.Category)
			}
		}
	}
	byID := make(map[string]core.AdapterSnapshot, len(base))
	for _, adapter := range base {
		byID[adapter.AdapterID] = adapter
	}
	out := make([]core.AdapterSnapshot, 0, len(base))
	seen := make(map[string]struct{}, len(base))
	appendAdapter := func(adapter core.AdapterSnapshot) {
		if _, exists := seen[adapter.AdapterID]; exists {
			return
		}
		seen[adapter.AdapterID] = struct{}{}
		preferredCategories := categoryOrder[adapter.AdapterID]
		if len(preferredCategories) == 0 {
			out = append(out, adapter)
			return
		}
		categoriesByName := make(map[string]core.CategorySnapshot, len(adapter.Categories))
		for _, category := range adapter.Categories {
			categoriesByName[category.Category] = category
		}
		categories := make([]core.CategorySnapshot, 0, len(adapter.Categories))
		used := make(map[string]struct{}, len(adapter.Categories))
		for _, name := range preferredCategories {
			if category, exists := categoriesByName[name]; exists {
				categories = append(categories, category)
				used[name] = struct{}{}
			}
		}
		for _, category := range adapter.Categories {
			if _, exists := used[category.Category]; !exists {
				categories = append(categories, category)
			}
		}
		adapter.Categories = categories
		out = append(out, adapter)
	}
	for _, adapterID := range adapterOrder {
		if adapter, exists := byID[adapterID]; exists {
			appendAdapter(adapter)
		}
	}
	for _, adapter := range base {
		appendAdapter(adapter)
	}
	return out
}

func isMergeableEntry(entry core.SnapshotEntry) bool {
	if entry.Kind == "file" {
		return false
	}
	value, ok := ParseJSONContent(entry.Content)
	return ok && IsJSONObject(value)
}

// EmptyJSONObjectEntry is useful to callers building a first-contact fixture;
// the normal planner creates an independent entry internally.
func EmptyJSONObjectEntry() core.SnapshotEntry {
	return core.SnapshotEntry{Kind: "json", Content: "{}"}
}
