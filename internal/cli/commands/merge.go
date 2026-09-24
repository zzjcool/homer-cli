package commands

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/backup"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
	"github.com/zzjcool/homer-cli/internal/secretscan"
	syncx "github.com/zzjcool/homer-cli/internal/sync"
)

// MergeOptions controls conflict resolution. The two accept flags are
// mutually exclusive and are validated by both the dispatcher and RunMerge's
// command-layer entry point.
type MergeOptions struct {
	HomerHome    string
	Home         string
	JSON         bool
	AcceptLocal  bool
	AcceptRemote bool
}

type MergeStatus string

const (
	MergeStatusResolved    MergeStatus = "resolved"
	MergeStatusNoConflicts MergeStatus = "no-conflicts"
	MergeStatusAborted     MergeStatus = "aborted"
	MergeStatusError       MergeStatus = "error"
)

type MergeChoice string

const (
	MergeChoiceLocal  MergeChoice = "local"
	MergeChoiceRemote MergeChoice = "remote"
)

type MergeResolution struct {
	AdapterID string      `json:"adapterId"`
	Category  string      `json:"category"`
	RelPath   string      `json:"relPath"`
	KeyPaths  []string    `json:"keyPaths,omitempty"`
	Choice    MergeChoice `json:"choice"`
}

type MergeReport struct {
	OK          bool              `json:"ok"`
	Status      MergeStatus       `json:"status"`
	Resolutions []MergeResolution `json:"resolutions"`
	Applied     syncx.ApplyResult `json:"applied"`
	Commit      string            `json:"commit,omitempty"`
	Warnings    []string          `json:"warnings"`
	Errors      []string          `json:"errors"`
}

func newMergeCommandReport(status MergeStatus) MergeReport {
	return MergeReport{
		OK:          status == MergeStatusResolved || status == MergeStatusNoConflicts,
		Status:      status,
		Resolutions: []MergeResolution{},
		Applied:     emptyApplyResult(),
		Warnings:    []string{},
		Errors:      []string{},
	}
}

func (report MergeReport) ExitCode() int {
	if report.OK {
		return 0
	}
	return 1
}

const MERGE_USAGE = `用法: homer merge [options]

逐项裁决本地与远端的冲突，并把裁决结果写回工具目录（选择远端时先备份），
随后同步到 store 与远端。

裁决选项（互斥，二选一）:
  --accept-local      批量保留本地
  --accept-remote     批量采用远端

未给出裁决选项时逐项询问 Accept Local / Accept Remote（没有 skip）。
非交互环境下必须显式给出裁决选项，否则中止（零写入）。

其它选项:
  --home <dir>        homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --json              输出机器可读 JSON（只含路径 / 冲突键，不含文件内容）
  -h, --help          显示本帮助

退出码: resolved / no-conflicts → 0；aborted / error → 1。`

type mergeResolution struct {
	Action syncx.PullConflictAction
	Choice MergeChoice
}

func mergeIsConflict(action syncx.PullAction) bool { return action.Type == syncx.PullActionConflict }

func mergeTarget(action syncx.PullAction) string {
	return fileRef(syncx.FileRef{AdapterID: action.AdapterID, Category: action.Category, RelPath: action.RelPath})
}

func mergeOptions() []selectOption {
	return []selectOption{{Value: string(MergeChoiceLocal), Label: "Accept Local"}, {Value: string(MergeChoiceRemote), Label: "Accept Remote"}}
}

func mergeConflictPrompt(action syncx.PullAction) string {
	text := fmt.Sprintf("%s — %s", mergeTarget(action), mergeReasonText(action.Reason))
	if len(action.KeyPaths) > 0 {
		text += "（冲突键: " + strings.Join(action.KeyPaths, ", ") + "）"
	}
	return text
}

func mergeReasonText(reason string) string {
	switch reason {
	case "modify-vs-modify":
		return "双方各自修改了这个文件"
	case "local-delete-vs-remote-modify":
		return "本地删除了这个文件，远端修改了它"
	case "local-modify-vs-remote-delete":
		return "本地修改了这个文件，远端删除了它"
	case "merge-keys":
		return "同一个配置键被双方改成了不同值"
	default:
		return reason
	}
}

// RunMerge implements the frozen merge pipeline. The report-returning shape
// ensures JSON output remains valid for precondition failures as well as for
// execution failures; no write is attempted before all preconditions pass.
func RunMerge(options MergeOptions, deps *MergeDeps) (report MergeReport) {
	report = newMergeCommandReport(MergeStatusError)
	defer func() {
		if recovered := recover(); recovered != nil {
			report = newMergeCommandReport(MergeStatusError)
			report.Errors = []string{fmt.Sprint(recovered)}
		}
	}()

	homerHome := options.HomerHome
	if homerHome == "" {
		homerHome = options.Home
	}
	paths := homeFor(homerHome)
	config, err := core.LoadConfig(paths)
	if err != nil {
		if os.IsNotExist(err) {
			report.Errors = []string{fmt.Sprintf("未找到 %s", paths.ConfigFile), "请先运行 `homer init` 生成配置（merge 需要 homer.json 才能映射冲突文件）。"}
		} else {
			report.Errors = errorLines(err)
		}
		return report
	}
	if options.AcceptLocal && options.AcceptRemote {
		report.Errors = []string{"不能同时指定 --accept-local 与 --accept-remote", "二者语义相反，请只保留一个（或都不给以进入逐项裁决）。"}
		return report
	}

	git := GitPort{}
	if deps != nil {
		git = gitPortFrom(deps.Git)
	}
	if err := mergePreconditions(paths, deps, git); err != nil {
		report.Errors = errorLines(err)
		return report
	}

	var sourceInput any
	if deps != nil {
		sourceInput = deps.Sources
	}
	// The precondition fetch has already happened. The injected-source path is
	// still accepted for focused command tests; real collection reads the
	// refreshed upstream without fetching a second time.
	sources := collectSources(sourceInput, paths, *config, false)
	warnings := append([]string{}, sources.Warnings...)
	errorsOut := append([]string{}, sources.Errors...)
	plan := syncx.PlanPull(*config, sources.Base, sources.Local, sources.Remote)
	conflicts := mergeConflictActions(plan)
	if len(conflicts) == 0 {
		report = newMergeCommandReport(MergeStatusNoConflicts)
		report.Warnings = warnings
		report.Errors = errorsOut
		return report
	}

	resolutions, aborted := chooseMergeResolutions(options, deps, conflicts)
	if aborted {
		warnings = append(warnings, "非交互环境无法逐项裁决：请显式指定 --accept-local 或 --accept-remote（本次未做任何写入）")
		report = newMergeCommandReport(MergeStatusAborted)
		report.Warnings = warnings
		report.Errors = errorsOut
		return report
	}

	progressFF := false
	progressState := false
	applied := emptyApplyResult()
	tryError := func(err error) MergeReport {
		result := newMergeCommandReport(MergeStatusError)
		result.Resolutions = mergeReportResolutions(resolutions)
		result.Applied = applied
		result.Warnings = warnings
		result.Errors = append(errorsOut, errorLines(err)...)
		if progressFF {
			if progressState {
				result.Warnings = append(result.Warnings, "store 已快进到 upstream 且 base 已前进（远端变更已被看见），但工具目录写入 / 同步未完成；请重跑 `homer merge` 或 `homer pull` 收敛")
			} else {
				result.Warnings = append(result.Warnings, "store 已快进到 upstream，但后续步骤未完成；请重跑 `homer merge` 或 `homer pull` 收敛")
			}
		}
		return result
	}

	// Re-check at the write boundary in case a user changed store during the
	// interactive resolution prompts.
	if err := git.requireCleanStore(paths); err != nil {
		return tryError(err)
	}
	if err := git.requireFastForwardable(paths); err != nil {
		return tryError(err)
	}
	ff := git.mergeFFUpstream(paths.Home)
	if !ff.OK {
		return tryError(fmt.Errorf("git merge --ff-only @{upstream} 失败: %s", firstLine(ff.Stderr)))
	}
	progressFF = true
	upstreamHead := git.headCommit(paths.Home)
	if upstreamHead == "" {
		warnings = append(warnings, "ff 后无法解析 HEAD，state.lastSyncCommit 未更新")
	} else if stateErr := nowState(paths, upstreamHead, "merge"); stateErr != nil {
		return tryError(stateErr)
	} else {
		progressState = true
	}

	subPlan := buildMergeResolutionPlan(plan, resolutions, *config, sources, &warnings)
	applied, err = syncx.ApplyPullActions(paths, *config, subPlan, syncx.ApplyPullActionsOptions{Backup: true, Command: "merge"})
	if err != nil {
		return tryError(err)
	}
	keep := backup.DefaultBackupKeep
	if config.Backup != nil && config.Backup.Keep != nil {
		keep = *config.Backup.Keep
	}
	if _, err := backup.PruneBackups(paths, keep); err != nil {
		return tryError(err)
	}

	localAfter := mergeRescanLocal(*config, sources.Remote, &errorsOut)
	safety := syncx.CheckPushSafety(*config, sources.Remote, localAfter, sources.Remote)
	if safety.Status != syncx.PushStatusOK {
		warnings = append(warnings, fmt.Sprintf("合并后重扫仍检测到远端未见的变更（%s），请运行 `homer status` 复核", safety.Status))
		result := newMergeCommandReport(MergeStatusResolved)
		result.Resolutions = mergeReportResolutions(resolutions)
		result.Applied = applied
		result.Warnings = warnings
		result.Errors = errorsOut
		return result
	}
	if len(safety.ChangedFiles) == 0 {
		result := newMergeCommandReport(MergeStatusResolved)
		result.Resolutions = mergeReportResolutions(resolutions)
		result.Applied = applied
		result.Warnings = warnings
		result.Errors = errorsOut
		return result
	}

	// Merge's push path uses exactly the push secret gate. It scans the bytes
	// that would be written to store (excluded local keys become placeholders),
	// then applies secrets.ignorePaths before permitting any store write.
	prepared := syncx.PrepareStoreSnapshot(localAfter, *config)
	findings := secretscan.FilterIgnored(secretscan.ScanSnapshots(prepared), secretIgnorePaths(config))
	if len(findings) > 0 {
		errorsOut = append(errorsOut, fmt.Sprintf("合并结果含 %d 处疑似密钥，已拒绝写入 store（未产生 commit、未推远端）", len(findings)))
		for _, finding := range findings {
			errorsOut = append(errorsOut, fmt.Sprintf("%s:%d [%s] %s", finding.Path, finding.Line, finding.PatternID, finding.Description))
		}
		errorsOut = append(errorsOut, "请移除密钥，或在 homer.json 的 secrets.ignorePaths 中显式豁免该路径，然后重跑 `homer merge`。")
		warnings = append(warnings, "store 已快进到 upstream 且 base 已前进（远端变更已被看见），但合并结果未写入 store；工具目录裁决已生效，请移除密钥后重跑 `homer merge` 或 `homer push` 收敛")
		result := newMergeCommandReport(MergeStatusError)
		result.Resolutions = mergeReportResolutions(resolutions)
		result.Applied = applied
		result.Warnings = warnings
		result.Errors = errorsOut
		return result
	}

	for _, snapshot := range prepared {
		if writeErr := core.WriteSnapshotToStore(paths, snapshot); writeErr != nil {
			return tryError(writeErr)
		}
	}
	if git.isStoreClean(paths.Home) {
		warnings = append(warnings, "重扫出的变更与 store 当前内容一致，无需新的提交")
		result := newMergeCommandReport(MergeStatusResolved)
		result.Resolutions = mergeReportResolutions(resolutions)
		result.Applied = applied
		result.Warnings = warnings
		result.Errors = errorsOut
		return result
	}

	commit := git.commitStore(paths, mergeCommitMessage(resolutions))
	if commit == "" {
		result := tryError(fmt.Errorf("工具目录裁决已生效且 store 已写入（%s），但未能生成 git commit（store 工作区仍脏）", paths.StoreDir))
		result.Errors = append(result.Errors, "请检查 git 是否可用与 user.name / user.email 配置，然后运行 `homer push` 完成提交与推送。")
		return result
	}

	if git.hasUpstream(paths.Home) {
		pushed := git.push(paths.Home)
		if !pushed.OK {
			warnings = append(warnings, fmt.Sprintf("git push 失败：本地合并提交已生成但未推送到远端（%s）；远端待重试，请运行 `homer push`", firstLine(pushed.Stderr)))
			if stateErr := nowState(paths, commit, "merge"); stateErr != nil {
				return tryError(stateErr)
			}
			result := newMergeCommandReport(MergeStatusResolved)
			result.Resolutions = mergeReportResolutions(resolutions)
			result.Applied = applied
			result.Commit = commit
			result.Warnings = warnings
			result.Errors = errorsOut
			return result
		}
	}
	if stateErr := nowState(paths, commit, "merge"); stateErr != nil {
		return tryError(stateErr)
	}
	result := newMergeCommandReport(MergeStatusResolved)
	result.Resolutions = mergeReportResolutions(resolutions)
	result.Applied = applied
	result.Commit = commit
	result.Warnings = warnings
	result.Errors = errorsOut
	return result
}

func mergePreconditions(paths core.HomerPaths, deps *MergeDeps, git GitPort) error {
	if !git.isGitRepo(paths.Home) {
		return core.NewCliError(syncx.NotAGitRepoMessage(paths.Home) + "\n" + syncx.NOT_A_REPO_HINT)
	}
	if !git.hasUpstream(paths.Home) {
		return core.NewCliError(syncx.NO_UPSTREAM_MESSAGE + "\n" + syncx.NO_UPSTREAM_HINT)
	}
	if err := git.requireCleanStore(paths); err != nil {
		return err
	}
	if deps == nil || !deps.NoFetch {
		fetched := git.fetch(paths.Home)
		if !fetched.OK {
			return core.NewCliError("git fetch 失败: " + firstLine(fetched.Stderr) + "\n远端状态不可确定，merge 已中止（未做任何写入）；请检查网络 / remote 后重试。")
		}
	}
	return git.requireFastForwardable(paths)
}

func chooseMergeResolutions(options MergeOptions, deps *MergeDeps, conflicts []syncx.PullConflictAction) ([]mergeResolution, bool) {
	batch := MergeChoice("")
	if options.AcceptRemote {
		batch = MergeChoiceRemote
	} else if options.AcceptLocal {
		batch = MergeChoiceLocal
	}
	if batch != "" {
		result := make([]mergeResolution, len(conflicts))
		for i, action := range conflicts {
			result[i] = mergeResolution{Action: action, Choice: batch}
		}
		return result, false
	}

	var ui any
	if deps != nil {
		ui = deps.UI
	}
	if ui == nil && !isTTY() {
		return nil, true
	}
	result := make([]mergeResolution, 0, len(conflicts))
	for _, action := range conflicts {
		answer := promptSelect(ui, mergeConflictPrompt(action), mergeOptions(), string(MergeChoiceLocal))
		choice := MergeChoiceLocal
		if answer == string(MergeChoiceRemote) {
			choice = MergeChoiceRemote
		}
		result = append(result, mergeResolution{Action: action, Choice: choice})
	}
	return result, false
}

func mergeConflictActions(plan syncx.PullPlan) []syncx.PullConflictAction {
	result := make([]syncx.PullConflictAction, 0)
	for _, action := range plan.Actions {
		if mergeIsConflict(action) {
			result = append(result, action)
		}
	}
	return result
}

func mergeReportResolutions(resolutions []mergeResolution) []MergeResolution {
	result := make([]MergeResolution, 0, len(resolutions))
	for _, resolution := range resolutions {
		item := MergeResolution{
			AdapterID: resolution.Action.AdapterID,
			Category:  resolution.Action.Category,
			RelPath:   resolution.Action.RelPath,
			Choice:    resolution.Choice,
		}
		if len(resolution.Action.KeyPaths) > 0 {
			item.KeyPaths = append([]string(nil), resolution.Action.KeyPaths...)
		}
		result = append(result, item)
	}
	return result
}

func buildMergeResolutionPlan(plan syncx.PullPlan, resolutions []mergeResolution, config core.HomerConfig, sources syncx.SyncSources, warnings *[]string) syncx.PullPlan {
	actions := make([]syncx.PullAction, 0, len(plan.Actions))
	for _, action := range plan.Actions {
		if action.Type != syncx.PullActionConflict {
			actions = append(actions, action)
		}
	}
	for _, resolution := range resolutions {
		if resolution.Choice != MergeChoiceRemote {
			continue
		}
		if adopted, ok := adoptRemoteMergeAction(resolution.Action, config, sources, warnings); ok {
			actions = append(actions, adopted)
		}
	}
	return syncx.PullPlan{Actions: actions}
}

func lookupMergeEntry(snapshots []core.AdapterSnapshot, ref syncx.PullAction) (core.SnapshotEntry, bool) {
	for _, snapshot := range snapshots {
		if snapshot.AdapterID != ref.AdapterID {
			continue
		}
		for _, category := range snapshot.Categories {
			if category.Category != ref.Category {
				continue
			}
			entry, ok := category.Files[ref.RelPath]
			return entry, ok
		}
	}
	return core.SnapshotEntry{}, false
}

func adoptRemoteMergeAction(action syncx.PullConflictAction, config core.HomerConfig, sources syncx.SyncSources, warnings *[]string) (syncx.PullAction, bool) {
	remoteEntry, ok := lookupMergeEntry(sources.Remote, action)
	if !ok {
		return syncx.PullAction{Type: syncx.PullActionDelete, AdapterID: action.AdapterID, Category: action.Category, RelPath: action.RelPath}, true
	}
	keys := syncx.ExcludedKeysFor(config, action.AdapterID, action.Category)
	content := action.RemoteContent
	if len(keys) > 0 {
		content = mergeRemoteContentWithExcluded(action, config, sources, remoteEntry.Content, keys, warnings)
	} else if content == "" {
		content = remoteEntry.Content
	}
	return syncx.PullAction{Type: syncx.PullActionWrite, AdapterID: action.AdapterID, Category: action.Category, RelPath: action.RelPath, Content: content}, true
}

func mergeRemoteContentWithExcluded(action syncx.PullConflictAction, _ core.HomerConfig, sources syncx.SyncSources, remoteContent string, keys []string, warnings *[]string) string {
	remoteValue, ok := syncx.ParseJSONContent(remoteContent)
	if !ok || !syncx.IsJSONObject(remoteValue) {
		return remoteContent
	}
	localValue := orderedjson.Value(nil)
	if localEntry, exists := lookupMergeEntry(sources.Local, action); exists {
		localValue, _ = syncx.ParseJSONContent(localEntry.Content)
	}
	planted := syncx.PlantExcludedKeys(remoteValue, localValue, keys)
	object, ok := planted.(*orderedjson.Object)
	if !ok || object == nil {
		return remoteContent
	}
	deleteSet := make(map[string]struct{})
	for _, key := range keys {
		if value, exists := object.M[key]; exists {
			if text, isString := value.(string); isString && text == syncx.REQUIRED_PLACEHOLDER {
				deleteSet[key] = struct{}{}
				*warnings = append(*warnings, fmt.Sprintf("%s 的必填项 %q 在 store 中为占位符且本地缺失，已从结果中移除；请重新填写后再 push", mergeTarget(action), key))
			}
		}
	}
	if len(deleteSet) > 0 {
		trimmed := &orderedjson.Object{Keys: make([]string, 0, len(object.Keys)), M: make(map[string]orderedjson.Value, len(object.M)-len(deleteSet))}
		for _, key := range object.Keys {
			if _, remove := deleteSet[key]; remove {
				continue
			}
			if value, exists := object.M[key]; exists {
				trimmed.Keys = append(trimmed.Keys, key)
				trimmed.M[key] = value
			}
		}
		object = trimmed
	}
	return syncx.SerializeJSONContent(object)
}

func mergeRescanLocal(config core.HomerConfig, fallback []core.AdapterSnapshot, errorsOut *[]string) []core.AdapterSnapshot {
	ids := make([]string, 0, len(config.Adapters))
	for id := range config.Adapters {
		ids = append(ids, id)
	}
	// core config's map is intentionally not an order contract; sort for stable
	// reports and deterministic re-scan order.
	sort.Strings(ids)
	local := make([]core.AdapterSnapshot, 0, len(ids))
	for _, id := range ids {
		cfg := config.Adapters[id]
		if cfg.Enabled != nil && !*cfg.Enabled {
			continue
		}
		outcome := adapter.ScanAdapter(id, cfg)
		problems := make([]core.ScanProblem, len(outcome.Errors))
		for i, item := range outcome.Errors {
			problems[i] = core.ScanProblem{Path: item.Path, Message: item.Message}
		}
		like := core.ScanOutcomeLike{Snapshot: outcome.Snapshot, Errors: problems}
		*errorsOut = append(*errorsOut, core.ScanWarningMessages(id, like)...)
		if core.IsRootUnreadable(like) {
			for _, snapshot := range fallback {
				if snapshot.AdapterID == id {
					local = append(local, snapshot)
					goto nextAdapter
				}
			}
		}
		local = append(local, outcome.Snapshot)
	nextAdapter:
	}
	return local
}

func mergeCommitMessage(resolutions []mergeResolution) string {
	remote := 0
	for _, resolution := range resolutions {
		if resolution.Choice == MergeChoiceRemote {
			remote++
		}
	}
	return fmt.Sprintf("homer merge: resolve %d conflict(s) (remote %d, local %d)", len(resolutions), remote, len(resolutions)-remote)
}

func RenderMergeReport(report MergeReport) string {
	lines := []string{fmt.Sprintf("homer merge: %s", report.Status)}
	for _, resolution := range report.Resolutions {
		prefix := "← 保留本地"
		if resolution.Choice == MergeChoiceRemote {
			prefix = "→ 采用远端"
		}
		keys := ""
		if len(resolution.KeyPaths) > 0 {
			keys = " [" + strings.Join(resolution.KeyPaths, ", ") + "]"
		}
		lines = append(lines, fmt.Sprintf("  %s  %s%s", prefix, fileRef(syncx.FileRef{AdapterID: resolution.AdapterID, Category: resolution.Category, RelPath: resolution.RelPath}), keys))
	}
	lines = append(lines, fmt.Sprintf("  写入: %d  删除: %d", len(report.Applied.Written), len(report.Applied.Deleted)))
	if report.Applied.BackupDir != "" {
		lines = append(lines, "  备份目录: "+report.Applied.BackupDir)
	}
	if report.Commit != "" {
		lines = append(lines, "  同步提交: "+report.Commit)
	}
	if report.Status == MergeStatusNoConflicts {
		lines = append(lines, "  无冲突：非冲突的远端变更请用 `homer pull` 应用（merge 只处理冲突）。")
	}
	if report.Status == MergeStatusAborted {
		lines = append(lines, "  本次未做任何写入；请显式指定 --accept-local / --accept-remote，或在 TTY 下逐项裁决。")
	}
	for _, warning := range report.Warnings {
		lines = append(lines, "  ⚠ "+warning)
	}
	for _, item := range report.Errors {
		lines = append(lines, "  ✗ "+item)
	}
	return strings.Join(lines, "\n")
}

func RenderMergeJSON(report MergeReport) string {
	resolutions := make([]orderedjson.Value, 0, len(report.Resolutions))
	for _, resolution := range report.Resolutions {
		keys := []string{"adapterId", "category", "relPath"}
		values := map[string]orderedjson.Value{
			"adapterId": resolution.AdapterID, "category": resolution.Category, "relPath": resolution.RelPath,
		}
		if len(resolution.KeyPaths) > 0 {
			keys = append(keys, "keyPaths")
			values["keyPaths"] = stringArrayValue(resolution.KeyPaths)
		}
		keys = append(keys, "choice")
		values["choice"] = string(resolution.Choice)
		resolutions = append(resolutions, &orderedjson.Object{Keys: keys, M: values})
	}
	keys := []string{"ok", "status", "resolutions", "applied"}
	values := map[string]orderedjson.Value{
		"ok": report.OK, "status": string(report.Status), "resolutions": resolutions, "applied": applyResultValue(report.Applied),
	}
	if report.Commit != "" {
		keys = append(keys, "commit")
		values["commit"] = report.Commit
	}
	keys = append(keys, "warnings", "errors")
	values["warnings"] = stringArrayValue(report.Warnings)
	values["errors"] = stringArrayValue(report.Errors)
	return string(orderedjson.Serialize(&orderedjson.Object{Keys: keys, M: values}))
}

func runMerge(options MergeOptions, deps *MergeDeps) MergeReport { return RunMerge(options, deps) }
func renderMergeReport(report MergeReport) string                { return RenderMergeReport(report) }
