package commands

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/backup"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/manifest"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
	syncx "github.com/zzjcool/homer-cli/internal/sync"
)

// PullOptions is the option set for the pull command. Home is retained as a
// compatibility alias for callers that use the flag spelling directly.
type PullOptions struct {
	HomerHome string
	Home      string
	JSON      bool
	Yes       bool
}

// PullDeps contains pull-specific injection points. Shared git behavior stays
// in GitPort, while manifest command execution belongs to this command's seam.
type PullDeps struct {
	UI       any
	Sources  any
	NoFetch  bool
	NoApply  bool
	Git      any
	Commands manifest.CommandPort
}

type PullStatus string

const (
	PullStatusApplied         PullStatus = "applied"
	PullStatusNoDrift         PullStatus = "no-drift"
	PullStatusAborted         PullStatus = "aborted"
	PullStatusConflictsRemain PullStatus = "conflicts-remain"
	PullStatusError           PullStatus = "error"
)

// ManifestApplyReport is shared by pull and home so both commands expose the
// same additive JSON shape for manifest installations.
type ManifestApplyReport struct {
	Installed []string `json:"installed"`
	Failed    []string `json:"failed"`
}

// PullReport is deliberately a value report rather than an error-returning
// API. This keeps --json parseable even when a precondition or an apply step
// fails: the dispatcher can render the same shape for every exit-1 outcome.
type PullReport struct {
	OK        bool                       `json:"ok"`
	Status    PullStatus                 `json:"status"`
	Applied   syncx.ApplyResult          `json:"applied"`
	Conflicts []syncx.PullConflictAction `json:"conflicts"`
	Manifest  *ManifestApplyReport       `json:"manifest,omitempty"`
	Commit    string                     `json:"commit,omitempty"`
	Warnings  []string                   `json:"warnings"`
	Errors    []string                   `json:"errors"`
}

func newPullCommandReport(status PullStatus) PullReport {
	return PullReport{
		OK:        status == PullStatusApplied || status == PullStatusNoDrift,
		Status:    status,
		Applied:   emptyApplyResult(),
		Conflicts: []syncx.PullConflictAction{},
		Warnings:  []string{},
		Errors:    []string{},
	}
}

func emptyManifestApplyReport() *ManifestApplyReport {
	return &ManifestApplyReport{Installed: []string{}, Failed: []string{}}
}

// applyManifestTasks preserves the manifest engine's continue-on-error
// behavior while adapting its result to the command report and warning
// contract. The engine's Failure intentionally carries only ID/message; the
// task list supplies the category context required by the CLI report.
func applyManifestTasks(tasks []manifest.Task, port manifest.CommandPort, warnings *[]string) *ManifestApplyReport {
	result := manifest.ApplyTasks(tasks, port)
	report := &ManifestApplyReport{
		Installed: append([]string{}, result.Installed...),
		Failed:    make([]string, 0, len(result.Failed)),
	}
	for _, failure := range result.Failed {
		entry := manifestFailureRef(tasks, failure)
		report.Failed = append(report.Failed, entry)
		if warnings != nil {
			*warnings = append(*warnings, "manifest 安装失败: "+entry)
		}
	}
	return report
}

func manifestFailureRef(tasks []manifest.Task, failure manifest.Failure) string {
	for _, task := range tasks {
		for _, id := range task.IDs {
			if id == failure.ID {
				return fmt.Sprintf("%s/%s:%s: %s", task.AdapterID, task.Category, failure.ID, failure.Message)
			}
		}
	}
	return fmt.Sprintf("%s: %s", failure.ID, failure.Message)
}

func (report PullReport) ExitCode() int {
	if report.OK {
		return 0
	}
	return 1
}

const PULL_USAGE = `用法: homer pull [options]

从 git upstream 拉取远端快照，三路判定后应用到本机工具目录。
应用前会把受影响的现有文件备份到 <home>/backups/<date>/<time>-pull/。

选项:
  --home <dir>       homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --yes              跳过交互确认；有冲突时保留本地并报告
  --json             输出机器可读 JSON（PullReport）
  -h, --help         显示本帮助

前置条件: 工作区是 git 仓库、已配置 upstream、store 干净且 HEAD 是 upstream 的祖先。
退出码: applied / no-drift → 0；aborted / conflicts-remain / error → 1。`

const (
	PREVIEW_MAX_LINES            = 20
	PREVIEW_DIFF_MAX_INPUT_LINES = 2000
	pullPreviewMaxLines          = PREVIEW_MAX_LINES
	pullPreviewMaxInputLines     = PREVIEW_DIFF_MAX_INPUT_LINES
)

func pullIsConflict(action syncx.PullAction) bool { return action.Type == syncx.PullActionConflict }
func pullIsWrite(action syncx.PullAction) bool    { return action.Type == syncx.PullActionWrite }
func pullIsDelete(action syncx.PullAction) bool   { return action.Type == syncx.PullActionDelete }

func pullTarget(action syncx.PullAction) string {
	return fileRef(syncx.FileRef{AdapterID: action.AdapterID, Category: action.Category, RelPath: action.RelPath})
}

func snapshotContent(snapshots []core.AdapterSnapshot, adapterID, category, relPath string) string {
	for _, snapshot := range snapshots {
		if snapshot.AdapterID != adapterID {
			continue
		}
		for _, item := range snapshot.Categories {
			if item.Category != category {
				continue
			}
			if entry, ok := item.Files[relPath]; ok {
				return entry.Content
			}
		}
	}
	return ""
}

func pullSplitLinesCount(text string) int {
	if text == "" {
		return 0
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		return len(lines) - 1
	}
	return len(lines)
}

func pullTruncateLines(lines []string) []string {
	if len(lines) <= pullPreviewMaxLines {
		return append([]string(nil), lines...)
	}
	kept := append([]string(nil), lines[:pullPreviewMaxLines]...)
	kept = append(kept, fmt.Sprintf("… 省略 %d 行", len(lines)-pullPreviewMaxLines))
	return kept
}

func pullPreviewDiff(before, after string) []string {
	beforeLines := pullSplitLinesCount(before)
	afterLines := pullSplitLinesCount(after)
	if beforeLines > pullPreviewMaxInputLines || afterLines > pullPreviewMaxInputLines {
		return []string{fmt.Sprintf("… 文件过大（%d → %d 行），跳过逐行预览", beforeLines, afterLines)}
	}
	return pullTruncateLines(diffLines(before, after))
}

// BuildPullPreview creates the count + line-level preview used by the
// confirmation gate. It never prints file contents except as diff lines, and
// callers only invoke it before the first write.
func BuildPullPreview(sources syncx.SyncSources, plan syncx.PullPlan) string {
	writes, deletes, conflicts := 0, 0, 0
	for _, action := range plan.Actions {
		switch action.Type {
		case syncx.PullActionWrite:
			writes++
		case syncx.PullActionDelete:
			deletes++
		case syncx.PullActionConflict:
			conflicts++
		}
	}

	lines := []string{fmt.Sprintf("预览: 写入 %d  删除 %d  冲突 %d", writes, deletes, conflicts)}
	indent := func(values []string) {
		for _, value := range values {
			lines = append(lines, "    "+value)
		}
	}
	for _, action := range plan.Actions {
		switch action.Type {
		case syncx.PullActionWrite:
			lines = append(lines, "  写入 "+pullTarget(action))
			before := snapshotContent(sources.Local, action.AdapterID, action.Category, action.RelPath)
			indent(pullPreviewDiff(before, action.Content))
		case syncx.PullActionDelete:
			lines = append(lines, "  删除 "+pullTarget(action))
			before := snapshotContent(sources.Local, action.AdapterID, action.Category, action.RelPath)
			indent(pullPreviewDiff(before, ""))
		case syncx.PullActionConflict:
			lines = append(lines, fmt.Sprintf("  冲突 %s (%s)", pullTarget(action), action.Reason))
			if len(action.KeyPaths) > 0 {
				lines = append(lines, "    冲突键: "+strings.Join(action.KeyPaths, ", "))
			}
			local := action.LocalContent
			if local == "" {
				local = snapshotContent(sources.Local, action.AdapterID, action.Category, action.RelPath)
			}
			remote := action.RemoteContent
			if remote == "" {
				remote = snapshotContent(sources.Remote, action.AdapterID, action.Category, action.RelPath)
			}
			indent(pullPreviewDiff(local, remote))
		}
	}
	return strings.Join(lines, "\n")
}

func pullConfirmationPreview(sources syncx.SyncSources, plan syncx.PullPlan, tasks []manifest.Task) string {
	return pullConfirmationPreviewWithWarnings(sources, plan, tasks, nil)
}

func pullConfirmationPreviewWithWarnings(sources syncx.SyncSources, plan syncx.PullPlan, tasks []manifest.Task, manifestCommandWarnings []string) string {
	preview := BuildPullPreview(sources, plan)
	manifestPreview := syncx.BuildManifestPreview(tasks)
	if manifestPreview != "" {
		preview += "\n" + manifestPreview
	}
	if len(manifestCommandWarnings) > 0 {
		preview += "\n\n" + strings.Join(manifestCommandWarnings, "\n")
	}
	return preview
}

type manifestCommandDeclaration struct {
	Kind     core.CategoryKind
	ListCmd  string
	ApplyCmd string
}

func manifestCommandDeclarations(config core.HomerConfig) map[string]manifestCommandDeclaration {
	declarations := make(map[string]manifestCommandDeclaration)
	for adapterID, adapterConfig := range config.Adapters {
		for categoryName, category := range adapterConfig.Categories {
			declaration := manifestCommandDeclaration{ListCmd: category.ListCmd, ApplyCmd: category.ApplyCmd}
			if category.Kind != nil {
				declaration.Kind = *category.Kind
			}
			if declaration.Kind == "" && declaration.ListCmd == "" && declaration.ApplyCmd == "" {
				continue
			}
			declarations[adapterID+"/"+categoryName] = declaration
		}
	}
	return declarations
}

func manifestCommandDeclarationText(declaration manifestCommandDeclaration, present bool) string {
	if !present {
		return "<缺省>"
	}
	kind := string(declaration.Kind)
	if kind == "" {
		kind = "<缺省>"
	}
	return fmt.Sprintf("kind=%s, listCmd=%q, applyCmd=%q", kind, declaration.ListCmd, declaration.ApplyCmd)
}

// manifestCommandChangeWarnings compares the checked-out homer.json with the
// upstream blob before fast-forward. The new command is not executed here;
// the warning is retained in the report and shown in the confirmation preview.
func manifestCommandChangeWarnings(home string, config core.HomerConfig, git GitPort) []string {
	ref := git.upstreamRef(home)
	if ref == "" {
		return nil
	}
	remote := git.exec(home, []string{"show", ref + ":homer.json"})
	if !remote.OK || strings.TrimSpace(remote.Stdout) == "" {
		return nil
	}
	remoteConfig, validationErrors := core.ValidateConfig([]byte(remote.Stdout))
	if remoteConfig == nil || len(validationErrors) > 0 {
		return nil
	}

	localDeclarations := manifestCommandDeclarations(config)
	remoteDeclarations := manifestCommandDeclarations(*remoteConfig)
	keys := make([]string, 0, len(localDeclarations)+len(remoteDeclarations))
	seen := make(map[string]struct{}, len(localDeclarations)+len(remoteDeclarations))
	for key := range localDeclarations {
		keys = append(keys, key)
		seen[key] = struct{}{}
	}
	for key := range remoteDeclarations {
		if _, exists := seen[key]; !exists {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	warnings := make([]string, 0)
	for _, key := range keys {
		local, localOK := localDeclarations[key]
		remote, remoteOK := remoteDeclarations[key]
		if localOK == remoteOK && local == remote {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"⚠ 远端配置变更了 manifest 命令：%s：%s → %s",
			key,
			manifestCommandDeclarationText(local, localOK),
			manifestCommandDeclarationText(remote, remoteOK),
		))
	}
	return warnings
}

// RunPull implements the frozen pull sequence. In particular, all four
// repository/store checks happen before collection or any tool-directory
// write, and a pull leaving conflicts deliberately keeps state at preFfHead
// (S4) so a later merge still sees the original three-way base.
func RunPull(options PullOptions, deps *PullDeps) (report PullReport) {
	report = newPullCommandReport(PullStatusError)
	defer func() {
		if recovered := recover(); recovered != nil {
			report = newPullCommandReport(PullStatusError)
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
			report.Errors = []string{fmt.Sprintf("未找到 %s", paths.ConfigFile), "请先运行 `homer init` 生成 homer.json 与 store 快照。"}
		} else {
			report.Errors = errorLines(err)
		}
		return report
	}

	git := GitPort{}
	if deps != nil {
		git = gitPortFrom(deps.Git)
	}

	// Frozen precondition order: isGitRepo → hasUpstream → clean store →
	// fetch → fast-forwardable. Fetch is the only expected repository write;
	// it is still completed before source collection and tool writes.
	if !git.isGitRepo(paths.Home) {
		report.Errors = []string{syncx.NotAGitRepoMessage(paths.Home), syncx.NOT_A_REPO_HINT}
		return report
	}
	if !git.hasUpstream(paths.Home) {
		report.Errors = []string{syncx.NO_UPSTREAM_MESSAGE, syncx.NO_UPSTREAM_HINT}
		return report
	}
	if err := git.requireCleanStore(paths); err != nil {
		report.Errors = errorLines(err)
		return report
	}
	if deps == nil || !deps.NoFetch {
		fetched := git.fetch(paths.Home)
		if !fetched.OK {
			report.Errors = []string{"git fetch 失败: " + firstLine(fetched.Stderr), "请检查网络与 remote 配置后重试。"}
			return report
		}
	}
	if err := git.requireFastForwardable(paths); err != nil {
		report.Errors = errorLines(err)
		return report
	}

	var sourceInput any
	if deps != nil {
		sourceInput = deps.Sources
	}
	// The precondition fetch above has already refreshed the remote-tracking
	// ref. Reading with fetch=false avoids a second network operation. Compare
	// homer.json before ff so a newly introduced manifest command is visible
	// before any future scan can use the fast-forwarded config.
	manifestCommandWarnings := manifestCommandChangeWarnings(paths.Home, *config, git)
	sources := collectSources(sourceInput, paths, *config, false)
	warnings := append([]string{}, sources.Warnings...)
	warnings = append(warnings, manifestCommandWarnings...)
	sourceErrors := append([]string{}, sources.Errors...)
	plan := syncx.PlanPull(*config, sources.Base, sources.Local, sources.Remote)
	remainingPlan, manifestTasks, manifestWarnings := syncx.SplitManifestActions(*config, plan, sources.Local)
	warnings = append(warnings, manifestWarnings...)
	if len(remainingPlan.Actions) == 0 && len(manifestTasks) == 0 && len(manifestCommandWarnings) == 0 {
		report = newPullCommandReport(PullStatusNoDrift)
		report.Warnings = warnings
		report.Errors = sourceErrors
		return report
	}

	manifestReport := (*ManifestApplyReport)(nil)
	if len(manifestTasks) > 0 {
		manifestReport = emptyManifestApplyReport()
	}
	if !options.Yes {
		confirmed := promptConfirm(depsUI(deps), pullConfirmationPreviewWithWarnings(sources, remainingPlan, manifestTasks, manifestCommandWarnings)+"\n\n以上变更将应用到本机工具目录（受影响文件会先备份）。是否继续？", false)
		if !confirmed {
			if depsUI(deps) == nil && !isTTY() {
				warnings = append(warnings, "非交互环境无法确认，已中止；如需自动应用请加 --yes")
			}
			report = newPullCommandReport(PullStatusAborted)
			report.Manifest = manifestReport
			report.Warnings = warnings
			report.Errors = sourceErrors
			return report
		}
	}

	noApply := deps != nil && deps.NoApply
	applied := emptyApplyResult()
	commit := ""
	if !noApply {
		applied, err = syncx.ApplyPullActions(paths, *config, remainingPlan, syncx.ApplyPullActionsOptions{Backup: true, Command: "pull"})
		if err != nil {
			report = newPullCommandReport(PullStatusError)
			report.Applied = applied
			report.Conflicts = pullConflicts(remainingPlan)
			report.Manifest = manifestReport
			report.Warnings = warnings
			report.Errors = append(sourceErrors, errorLines(err)...)
			return report
		}
		keep := backup.DefaultBackupKeep
		if config.Backup != nil && config.Backup.Keep != nil {
			keep = *config.Backup.Keep
		}
		if len(manifestTasks) > 0 {
			var commands manifest.CommandPort
			if deps != nil {
				commands = deps.Commands
			}
			manifestReport = applyManifestTasks(manifestTasks, commands, &warnings)
		}
		if _, pruneErr := backup.PruneBackups(paths, keep); pruneErr != nil {
			report = newPullCommandReport(PullStatusError)
			report.Applied = applied
			report.Conflicts = pullConflicts(remainingPlan)
			report.Manifest = manifestReport
			report.Warnings = warnings
			report.Errors = append(sourceErrors, errorLines(pruneErr)...)
			return report
		}

		// Capture the old HEAD before ff. With residual conflicts this is the
		// base that merge must use on its next invocation (S4).
		preFfHead := git.headCommit(paths.Home)
		ff := git.mergeFFUpstream(paths.Home)
		if !ff.OK {
			report = newPullCommandReport(PullStatusError)
			report.Applied = applied
			report.Conflicts = pullConflicts(remainingPlan)
			report.Manifest = manifestReport
			report.Warnings = warnings
			report.Errors = append(sourceErrors, fmt.Sprintf("git merge --ff-only 失败（工具目录已应用，store 未前移）: %s", firstLine(ff.Stderr)))
			return report
		}
		commit = git.headCommit(paths.Home)
		conflicts := pullConflicts(remainingPlan)
		nextBase := commit
		if len(conflicts) > 0 {
			nextBase = preFfHead
		}
		if nextBase != "" {
			if stateErr := nowState(paths, nextBase, "pull"); stateErr != nil {
				report = newPullCommandReport(PullStatusError)
				report.Applied = applied
				report.Conflicts = conflicts
				report.Manifest = manifestReport
				report.Commit = commit
				report.Warnings = warnings
				report.Errors = append(sourceErrors, errorLines(stateErr)...)
				return report
			}
		} else {
			warnings = append(warnings, "ff 后无法解析 HEAD，state.lastSyncCommit 未更新")
		}
	}

	conflicts := pullConflicts(remainingPlan)
	errorsOut := append([]string{}, sourceErrors...)
	if len(conflicts) > 0 {
		errorsOut = append(errorsOut, fmt.Sprintf("检测到 %d 个冲突（已保留本地）：请运行 `homer merge` 逐项裁决。", len(conflicts)))
	}
	status := PullStatusApplied
	if len(conflicts) > 0 {
		status = PullStatusConflictsRemain
	}
	report = newPullCommandReport(status)
	report.OK = len(conflicts) == 0
	report.Applied = applied
	report.Conflicts = conflicts
	report.Manifest = manifestReport
	report.Commit = commit
	report.Warnings = warnings
	report.Errors = errorsOut
	return report
}

func depsUI(deps *PullDeps) any {
	if deps == nil {
		return nil
	}
	return deps.UI
}

func pullConflicts(plan syncx.PullPlan) []syncx.PullConflictAction {
	conflicts := make([]syncx.PullConflictAction, 0)
	for _, action := range plan.Actions {
		if pullIsConflict(action) {
			conflicts = append(conflicts, action)
		}
	}
	return conflicts
}

// RenderPullReport is the stable human renderer; every displayed count comes
// from the same slices exposed in PullReport and therefore agrees with JSON.
func RenderPullReport(report PullReport) string {
	lines := []string{fmt.Sprintf("homer pull: %s", report.Status)}
	lines = append(lines, fmt.Sprintf("  写入: %d  删除: %d  冲突: %d", len(report.Applied.Written), len(report.Applied.Deleted), len(report.Conflicts)))
	if report.Applied.BackupDir != "" {
		lines = append(lines, "  备份目录: "+report.Applied.BackupDir)
	}
	if report.Commit != "" {
		lines = append(lines, "  同步提交: "+report.Commit)
	}
	if report.Manifest != nil {
		lines = append(lines, fmt.Sprintf("  安装扩展: %d 成功 / %d 失败", len(report.Manifest.Installed), len(report.Manifest.Failed)))
		for _, failure := range report.Manifest.Failed {
			lines = append(lines, "  ✗ manifest: "+failure)
		}
	}
	for _, conflict := range report.Conflicts {
		lines = append(lines, fmt.Sprintf("  ⚡ 冲突: %s (%s)", pullTarget(conflict), conflict.Reason))
	}
	for _, warning := range report.Warnings {
		lines = append(lines, "  ⚠ "+warning)
	}
	for _, item := range report.Errors {
		lines = append(lines, "  ✗ "+item)
	}
	return strings.Join(lines, "\n")
}

func RenderPullJSON(report PullReport) string {
	keys := []string{"ok", "status", "applied", "conflicts"}
	values := map[string]orderedJSONValue{
		"ok":        report.OK,
		"status":    string(report.Status),
		"applied":   applyResultValue(report.Applied),
		"conflicts": pullActionsValue(report.Conflicts),
	}
	if report.Manifest != nil {
		keys = append(keys, "manifest")
		values["manifest"] = manifestApplyReportValue(report.Manifest)
	}
	if report.Commit != "" {
		keys = append(keys, "commit")
		values["commit"] = report.Commit
	}
	keys = append(keys, "warnings", "errors")
	values["warnings"] = stringArrayValue(report.Warnings)
	values["errors"] = stringArrayValue(report.Errors)
	return orderedObjectJSON(keys, values)
}

// These aliases keep pull's JSON builder independent of encoding/json while
// allowing the shared ordered-json helpers in push.go to remain private to the
// command package.
type orderedJSONValue = orderedjson.Value

func manifestApplyReportValue(report *ManifestApplyReport) orderedjson.Value {
	return &orderedjson.Object{
		Keys: []string{"installed", "failed"},
		M: map[string]orderedjson.Value{
			"installed": stringArrayValue(report.Installed),
			"failed":    stringArrayValue(report.Failed),
		},
	}
}

func applyResultValue(result syncx.ApplyResult) orderedjson.Value {
	keys := []string{"written", "deleted", "conflicts"}
	values := map[string]orderedjson.Value{
		"written":   fileRefsValue(result.Written),
		"deleted":   fileRefsValue(result.Deleted),
		"conflicts": fileRefsValue(result.Conflicts),
	}
	if result.BackupDir != "" {
		keys = append(keys, "backupDir")
		values["backupDir"] = result.BackupDir
	}
	return &orderedjson.Object{Keys: keys, M: values}
}

func pullActionsValue(actions []syncx.PullConflictAction) orderedjson.Value {
	items := make([]orderedjson.Value, 0, len(actions))
	for _, action := range actions {
		keys := []string{"type", "adapterId", "category", "relPath", "reason"}
		values := map[string]orderedjson.Value{
			"type": action.Type, "adapterId": action.AdapterID, "category": action.Category,
			"relPath": action.RelPath, "reason": action.Reason,
		}
		if len(action.KeyPaths) > 0 {
			keys = append(keys, "keyPaths")
			values["keyPaths"] = stringArrayValue(action.KeyPaths)
		}
		if action.LocalContent != "" {
			keys = append(keys, "localContent")
			values["localContent"] = action.LocalContent
		}
		if action.RemoteContent != "" {
			keys = append(keys, "remoteContent")
			values["remoteContent"] = action.RemoteContent
		}
		items = append(items, &orderedjson.Object{Keys: keys, M: values})
	}
	return items
}

func orderedObjectJSON(keys []string, values map[string]orderedJSONValue) string {
	return string(orderedjson.Serialize(&orderedjson.Object{Keys: keys, M: values}))
}
