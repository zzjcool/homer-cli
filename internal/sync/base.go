package sync

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gitx"
)

// CollectSyncSourcesOptions controls network access. Fetch defaults to true
// when no option is supplied; Fetch=false reads the currently available
// upstream ref and is intended for tests/offline callers.
type CollectSyncSourcesOptions struct {
	Fetch bool
}

type SyncSourcesOptions = CollectSyncSourcesOptions

// CollectSyncSources assembles raw base/local/remote snapshots. It never
// strips excluded keys: that belongs to PlanPull/CheckPushSafety. Environmental
// failures are recorded in Warnings/Errors and use the frozen safe fallbacks,
// rather than turning an unavailable adapter into mass deletion.
func CollectSyncSources(paths core.HomerPaths, config core.HomerConfig, options ...any) SyncSources {
	fetch, optionError := parseFetchOptions(options)
	warnings := make([]string, 0)
	errors := make([]string, 0)
	if optionError != nil {
		errors = append(errors, optionError.Error())
		fetch = true
	}

	base, mode, baseCommit := collectBase(paths, config, &warnings, &errors)
	local := collectLocal(config, base, &errors, &warnings)

	build := func(remote []core.AdapterSnapshot, remoteRef string) SyncSources {
		return SyncSources{
			Mode:       mode,
			Base:       base,
			Local:      local,
			Remote:     remote,
			BaseCommit: baseCommit,
			RemoteRef:  remoteRef,
			Warnings:   warnings,
			Errors:     errors,
		}
	}

	upstream := gitx.UpstreamRef(paths.Home)
	if upstream == "" {
		if gitx.IsGitRepo(paths.Home) {
			warnings = append(warnings, "未配置 git upstream，remote 视作 = base（M1 语义）")
		} else {
			warnings = append(warnings, "工作区不是 git 仓库，remote 视作 = base（M1 语义）")
		}
		return build(base, "")
	}

	if fetch {
		fetched := gitx.Fetch(paths.Home)
		if !fetched.OK {
			reason := strings.TrimSpace(fetched.Stderr)
			if reason == "" {
				reason = "未知错误"
			}
			warnings = append(warnings, fmt.Sprintf("git fetch 失败（远端变更不可见，remote 视作 = base）: %s", reason))
			return build(base, upstream)
		}
	}

	remoteCommit := revParse(paths, upstream)
	if remoteCommit == "" {
		warnings = append(warnings, fmt.Sprintf("git upstream %s 不可解析，remote 视作 = base", upstream))
		return build(base, upstream)
	}

	if !hasStoreTree(paths, remoteCommit) {
		if snapshotsHaveContent(base) {
			warnings = append(warnings, fmt.Sprintf("git upstream %s 中尚无 store 内容（首次同步？），remote 视作 = base（M1 语义）", upstream))
		}
		return build(base, upstream)
	}

	return build(gitx.ReadStoreSnapshotAtCommit(paths, &config, remoteCommit), upstream)
}

func collectSyncSources(paths core.HomerPaths, config core.HomerConfig, options ...any) SyncSources {
	return CollectSyncSources(paths, config, options...)
}

func parseFetchOptions(options []any) (bool, error) {
	if len(options) == 0 || options[0] == nil {
		return true, nil
	}
	if len(options) > 1 {
		return true, fmt.Errorf("collectSyncSources: opts 只能传一个")
	}
	switch value := options[0].(type) {
	case bool:
		return value, nil
	case CollectSyncSourcesOptions:
		return value.Fetch, nil
	case *CollectSyncSourcesOptions:
		if value == nil {
			return true, nil
		}
		return value.Fetch, nil
	default:
		return true, fmt.Errorf("collectSyncSources: opts 类型无效")
	}
}

func collectBase(paths core.HomerPaths, config core.HomerConfig, warnings, errors *[]string) ([]core.AdapterSnapshot, SyncBaseMode, string) {
	state := core.LoadState(paths)
	if state.LastSyncCommit == "" {
		return readStoreBase(paths, config, errors)
	}
	if !isReadableCommit(paths, state.LastSyncCommit) {
		*warnings = append(*warnings, fmt.Sprintf("state.lastSyncCommit=%s 在 git 历史中不可读，base 回落到 store 工作区", state.LastSyncCommit))
		return readStoreBase(paths, config, errors)
	}
	return gitx.ReadStoreSnapshotAtCommit(paths, &config, state.LastSyncCommit), SyncBaseModeGit, state.LastSyncCommit
}

func readStoreBase(paths core.HomerPaths, config core.HomerConfig, errors *[]string) ([]core.AdapterSnapshot, SyncBaseMode, string) {
	base, err := core.ReadSnapshotFromStore(paths, config)
	if err != nil {
		*errors = append(*errors, fmt.Sprintf("读取 store 快照失败: %v", err))
		return EmptyBaseSnapshots(config), SyncBaseModeStore, ""
	}
	return base, SyncBaseModeStore, ""
}

func revParse(paths core.HomerPaths, ref string) string {
	result := gitx.Exec(paths.Home, []string{"rev-parse", "--verify", ref + "^{commit}"}, 0)
	if !result.OK {
		return ""
	}
	return strings.TrimSpace(result.Stdout)
}

func isReadableCommit(paths core.HomerPaths, commit string) bool {
	return gitx.Exec(paths.Home, []string{"cat-file", "-e", commit + "^{commit}"}, 0).OK
}

func collectLocal(config core.HomerConfig, base []core.AdapterSnapshot, errors, warnings *[]string) []core.AdapterSnapshot {
	adapterIDs := make([]string, 0, len(config.Adapters))
	for adapterID := range config.Adapters {
		adapterIDs = append(adapterIDs, adapterID)
	}
	sort.Strings(adapterIDs)

	local := make([]core.AdapterSnapshot, 0, len(adapterIDs))
	for _, adapterID := range adapterIDs {
		adapterConfig := config.Adapters[adapterID]
		if adapterConfig.Enabled != nil && !*adapterConfig.Enabled {
			continue
		}
		outcome := adapter.ScanAdapter(adapterID, adapterConfig)
		if warnings != nil {
			*warnings = append(*warnings, outcome.Warnings...)
		}
		problems := make([]core.ScanProblem, len(outcome.Errors))
		for index, problem := range outcome.Errors {
			problems[index] = core.ScanProblem{Path: problem.Path, Message: problem.Message}
		}
		like := core.ScanOutcomeLike{Snapshot: outcome.Snapshot, Errors: problems}
		*errors = append(*errors, core.ScanWarningMessages(adapterID, like)...)
		if core.IsRootUnreadable(like) {
			if snapshot, ok := findAdapterSnapshot(base, adapterID); ok {
				local = append(local, snapshot)
				continue
			}
		}
		local = append(local, outcome.Snapshot)
	}
	return local
}

func findAdapterSnapshot(snapshots []core.AdapterSnapshot, adapterID string) (core.AdapterSnapshot, bool) {
	for _, snapshot := range snapshots {
		if snapshot.AdapterID == adapterID {
			return snapshot, true
		}
	}
	return core.AdapterSnapshot{}, false
}

func hasStoreTree(paths core.HomerPaths, commit string) bool {
	result := gitx.Exec(paths.Home, []string{"ls-tree", "-r", "--name-only", "-z", commit, "--", "store/"}, 0)
	return result.OK && strings.Trim(result.Stdout, " \t\r\n\x00") != ""
}

func snapshotsHaveContent(snapshots []core.AdapterSnapshot) bool {
	for _, snapshot := range snapshots {
		for _, category := range snapshot.Categories {
			if len(category.Files) != 0 {
				return true
			}
		}
	}
	return false
}
