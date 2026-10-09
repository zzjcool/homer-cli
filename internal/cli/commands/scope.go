package commands

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/core"
	syncx "github.com/zzjcool/homer-cli/internal/sync"
)

func adapterSelectionError(config core.HomerConfig, ids []string) error {
	unknown, disabled := splitAdapterAvailability(config, ids)
	if len(unknown) > 0 {
		return fmt.Errorf("没有适配器 %s，无法收取", strings.Join(unknown, "、"))
	}
	if len(disabled) > 0 {
		return fmt.Errorf("适配器 %s 未启用，无法收取", strings.Join(disabled, "、"))
	}
	return nil
}

func pullSelectionError(config core.HomerConfig, remote []core.AdapterSnapshot, ids []string) error {
	if missing := syncx.MissingAdapterIDs(remote, ids); len(missing) > 0 {
		return fmt.Errorf("中心没有适配器 %s，无法下发", strings.Join(missing, "、"))
	}
	unknown, disabled := splitAdapterAvailability(config, ids)
	if len(unknown) > 0 {
		return fmt.Errorf("没有适配器 %s，无法下发", strings.Join(unknown, "、"))
	}
	if len(disabled) > 0 {
		return fmt.Errorf("适配器 %s 未启用，无法下发", strings.Join(disabled, "、"))
	}
	return nil
}

func splitAdapterAvailability(config core.HomerConfig, ids []string) (unknown, disabled []string) {
	for _, id := range ids {
		adapterConfig, ok := config.Adapters[id]
		if !ok {
			unknown = append(unknown, id)
			continue
		}
		if adapterConfig.Enabled != nil && !*adapterConfig.Enabled {
			disabled = append(disabled, id)
		}
	}
	return unknown, disabled
}

// preferRemotePlan turns conflicts into the center's content. A missing
// remote file becomes a delete; everything else is a write of that content.
func preferRemotePlan(plan syncx.PullPlan, remote []core.AdapterSnapshot) syncx.PullPlan {
	return preferRemotePlanFor(plan, remote, nil)
}

// preferRemotePlanFor turns selected adapters' conflicts into the center's
// content. A nil adapterIDs selects every adapter; a non-nil empty slice
// selects none. A missing remote file becomes a delete.
func preferRemotePlanFor(plan syncx.PullPlan, remote []core.AdapterSnapshot, adapterIDs []string) syncx.PullPlan {
	selected := make(map[string]struct{}, len(adapterIDs))
	for _, adapterID := range adapterIDs {
		selected[adapterID] = struct{}{}
	}

	actions := make([]syncx.PullAction, len(plan.Actions))
	copy(actions, plan.Actions)
	for index, action := range actions {
		if action.Type != syncx.PullActionConflict {
			continue
		}
		if adapterIDs != nil {
			if _, ok := selected[action.AdapterID]; !ok {
				continue
			}
		}

		content := action.RemoteContent
		if content == "" {
			content = snapshotContent(remote, action.AdapterID, action.Category, action.RelPath)
		}
		if content == "" {
			actions[index].Type = syncx.PullActionDelete
			actions[index].Content = ""
			continue
		}
		actions[index].Type = syncx.PullActionWrite
		actions[index].Content = content
	}
	return syncx.PullPlan{Actions: actions}
}

func snapshotFileRefs(snapshots []core.AdapterSnapshot) []syncx.FileRef {
	refs := make([]syncx.FileRef, 0)
	for _, snapshot := range snapshots {
		for _, category := range snapshot.Categories {
			paths := make([]string, 0, len(category.Files))
			for path := range category.Files {
				paths = append(paths, path)
			}
			sort.Strings(paths)
			for _, path := range paths {
				refs = append(refs, syncx.FileRef{
					AdapterID: snapshot.AdapterID,
					Category:  category.Category,
					RelPath:   path,
				})
			}
		}
	}
	return refs
}
