package sync

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/manifest"
)

// SplitManifestActions removes manifest-kind actions from a pull plan and
// turns them into install tasks. A write uses its remote content from Content;
// a conflict uses RemoteContent. In both cases only IDs absent from the local
// list are installed, which gives manifest categories union semantics.
// Deletes are deliberately dropped because v1.2 never uninstalls.
func SplitManifestActions(config core.HomerConfig, plan PullPlan, local []core.AdapterSnapshot) (PullPlan, []manifest.Task, []string) {
	remaining := PullPlan{Actions: make([]PullAction, 0, len(plan.Actions))}
	tasks := make([]manifest.Task, 0)
	warnings := make([]string, 0)

	localByAdapter := indexByAdapter(local)
	for _, action := range plan.Actions {
		categoryConfig, ok := manifestCategory(config, action.AdapterID, action.Category)
		if !ok {
			remaining.Actions = append(remaining.Actions, action)
			continue
		}

		switch action.Type {
		case PullActionWrite:
			tasks = append(tasks, manifestTask(action, categoryConfig, localIDs(localByAdapter, action)))
		case PullActionConflict:
			// The remote side of a conflict is still an install source. Local-only
			// IDs are intentionally not removed: this is the D2 union rule.
			tasks = append(tasks, manifestTaskWithRemote(action, categoryConfig, action.RemoteContent, localIDs(localByAdapter, action)))
		case PullActionDelete:
			warnings = append(warnings, fmt.Sprintf("忽略 manifest 删除动作（不执行卸载）: %s/%s", action.AdapterID, action.Category))
		default:
			remaining.Actions = append(remaining.Actions, action)
		}
	}
	return remaining, tasks, warnings
}

// SupplementManifestInstalls installs center IDs the machine does not
// currently list, even when the file drift looks like a local deletion.
// A dispatch records the manifest as the new baseline after `pi install`
// fails or never runs. The next dispatch then sees "the machine removed
// these" and skips them, while `pi list` is still empty.
func SupplementManifestInstalls(config core.HomerConfig, local, remote []core.AdapterSnapshot, tasks []manifest.Task) []manifest.Task {
	type taskKey struct {
		adapter  string
		category string
	}
	index := make(map[taskKey]int, len(tasks))
	for i, task := range tasks {
		index[taskKey{task.AdapterID, task.Category}] = i
	}
	localByAdapter := indexByAdapter(local)
	remoteByAdapter := indexByAdapter(remote)
	adapterIDs := make([]string, 0, len(config.Adapters))
	for adapterID := range config.Adapters {
		adapterIDs = append(adapterIDs, adapterID)
	}
	sort.Strings(adapterIDs)
	for _, adapterID := range adapterIDs {
		remoteSnapshot, ok := remoteByAdapter[adapterID]
		if !ok {
			continue
		}
		adapter := config.Adapters[adapterID]
		names := make([]string, 0, len(adapter.Categories))
		for name := range adapter.Categories {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			category := adapter.Categories[name]
			if !category.IsManifest() {
				continue
			}
			content := manifestContent(&remoteSnapshot, name)
			if content == "" {
				continue
			}
			present := make(map[string]struct{})
			for _, id := range localIDs(localByAdapter, PullAction{AdapterID: adapterID, Category: name}) {
				present[id] = struct{}{}
			}
			missing := make([]string, 0)
			for _, id := range manifest.IDsOf(content) {
				if _, ok := present[id]; ok {
					continue
				}
				missing = append(missing, id)
			}
			if len(missing) == 0 {
				continue
			}
			key := taskKey{adapterID, name}
			if at, ok := index[key]; ok {
				tasks[at].IDs = sortedUniqueManifestIDs(append(tasks[at].IDs, missing...))
				continue
			}
			tasks = append(tasks, manifest.Task{
				AdapterID: adapterID,
				Category:  name,
				IDs:       sortedUniqueManifestIDs(missing),
				ListCmd:   category.ListCmd,
				ApplyCmd:  category.ApplyCmd,
			})
			index[key] = len(tasks) - 1
		}
	}
	return tasks
}

func manifestContent(snapshot *core.AdapterSnapshot, category string) string {
	found := findCategory(snapshot, category)
	if found == nil {
		return ""
	}
	entry, ok := found.Files[manifest.VirtualFileName(category)]
	if !ok {
		return ""
	}
	return entry.Content
}

func manifestCategory(config core.HomerConfig, adapterID, category string) (core.CategoryConfig, bool) {
	adapterConfig, ok := config.Adapters[adapterID]
	if !ok {
		return core.CategoryConfig{}, false
	}
	categoryConfig, ok := adapterConfig.Categories[category]
	if !ok || !categoryConfig.IsManifest() {
		return core.CategoryConfig{}, false
	}
	return categoryConfig, true
}

func manifestTask(action PullAction, categoryConfig core.CategoryConfig, local []string) manifest.Task {
	return manifestTaskWithRemote(action, categoryConfig, action.Content, local)
}

func manifestTaskWithRemote(action PullAction, categoryConfig core.CategoryConfig, remoteContent string, local []string) manifest.Task {
	localSet := make(map[string]struct{}, len(local))
	for _, id := range local {
		localSet[id] = struct{}{}
	}

	ids := make([]string, 0)
	seen := make(map[string]struct{})
	for _, id := range manifest.IDsOf(remoteContent) {
		if _, exists := localSet[id]; exists {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return manifest.Task{
		AdapterID: action.AdapterID,
		Category:  action.Category,
		IDs:       ids,
		ListCmd:   categoryConfig.ListCmd,
		ApplyCmd:  categoryConfig.ApplyCmd,
	}
}

func localIDs(localByAdapter map[string]core.AdapterSnapshot, action PullAction) []string {
	adapter, ok := localByAdapter[action.AdapterID]
	if !ok {
		return nil
	}
	category := findCategory(&adapter, action.Category)
	if category == nil {
		return nil
	}
	entry, ok := category.Files[manifest.VirtualFileName(action.Category)]
	if !ok && action.RelPath != "" {
		// The virtual key is frozen, but accepting the action's key makes this
		// splitter safe for hand-built plans and old fixtures.
		entry, ok = category.Files[action.RelPath]
	}
	if !ok {
		return nil
	}
	return manifest.IDsOf(entry.Content)
}

// BuildManifestPreview renders install tasks for pull/home confirmation. It
// emits at most 20 preview lines before a summary line for omitted IDs.
func BuildManifestPreview(tasks []manifest.Task) string {
	const maxLines = 20
	lines := make([]string, 0)
	omitted := 0
	for _, task := range tasks {
		ids := sortedUniqueManifestIDs(task.IDs)
		if len(ids) == 0 {
			continue
		}
		if len(lines) < maxLines {
			lines = append(lines, fmt.Sprintf("  安装扩展 %d 个（%s/%s）", len(ids), task.AdapterID, task.Category))
		} else {
			omitted += len(ids)
			continue
		}
		for _, id := range ids {
			if len(lines) >= maxLines {
				omitted++
				continue
			}
			lines = append(lines, "    + "+id)
		}
	}
	if omitted > 0 {
		lines = append(lines, fmt.Sprintf("… 其余 %d 条省略", omitted))
	}
	return strings.Join(lines, "\n")
}

func sortedUniqueManifestIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := append([]string(nil), ids...)
	sort.Strings(out)
	write := 0
	for _, id := range out {
		if write > 0 && out[write-1] == id {
			continue
		}
		out[write] = id
		write++
	}
	return out[:write]
}
