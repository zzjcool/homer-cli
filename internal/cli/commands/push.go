package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gitx"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
	"github.com/zzjcool/homer-cli/internal/secretscan"
	syncx "github.com/zzjcool/homer-cli/internal/sync"
)

// GitPort is the command-layer git seam.  Production calls use the real
// gitx package; tests may replace any individual operation without having to
// create a repository.  The fields are function values rather than methods so
// a partially populated port remains useful and safe.
//
// The command files share this type because push, pull, and merge must make
// exactly the same precondition and fast-forward decisions.
type GitPort struct {
	Exec                   func(home string, args []string, timeout time.Duration) gitx.ExecResult
	IsGitRepo              func(home string) bool
	HasUpstream            func(home string) bool
	HasPushTarget          func(home string) bool
	HasRemote              func(home string) bool
	PushHint               func(home string) string
	UpstreamRef            func(home string) string
	Fetch                  func(home string) gitx.ExecResult
	GitFetch               func(home string) gitx.ExecResult
	Push                   func(home string) gitx.ExecResult
	GitPush                func(home string) gitx.ExecResult
	HeadCommit             func(home string) string
	IsStoreClean           func(home string) bool
	IsPushClean            func(home string) bool
	IsAncestorOf           func(home, ancestor, descendant string) bool
	MergeFFUpstream        func(home string) gitx.ExecResult
	EnsureGitRepo          func(home string) error
	CommitAllStore         func(home, message string) string
	CommitStoreIfNeeded    func(paths core.HomerPaths, message string) string
	RequireCleanStore      func(paths core.HomerPaths) error
	RequireFastForwardable func(paths core.HomerPaths) error
}

// confirmPrompter and selectPrompter are the command-layer UI seams. They
// deliberately use the command's private option type so callers cannot rely
// on reflection or accidentally pass a structurally incompatible selector.
type confirmPrompter interface {
	Confirm(message string, fallback bool) bool
}

type selectPrompter interface {
	confirmPrompter
	Select(message string, options []selectOption, fallback string) string
}

type selectOption struct {
	Value string
	Label string
}

// PushDeps contains the push injection points. Sources accepts either a
// sync.SyncSources value, *sync.SyncSources, or func() sync.SyncSources.  The
// value form keeps hand-built fixtures concise while the function form avoids
// taking a snapshot before a test has finished setting up its repository.
type PushDeps struct {
	UI      selectPrompter
	Sources any
	Git     any
}

type PullDeps struct {
	UI      any
	Sources any
	NoFetch bool
	NoApply bool
	Git     any
}

type MergeDeps struct {
	UI      any
	Sources any
	NoFetch bool
	Git     any
}

// These aliases make the shared command-layer seam discoverable under the
// per-command names used by the migration plan, without creating three
// subtly different git contracts.
type PullGitPort = GitPort
type MergeGitPort = GitPort
type ResolvedGitPort = GitPort

func gitPortFrom(value any) GitPort {
	switch port := value.(type) {
	case GitPort:
		return port
	case *GitPort:
		if port != nil {
			return *port
		}
	}
	return GitPort{}
}

func (p GitPort) exec(home string, args []string) gitx.ExecResult {
	if p.Exec != nil {
		return p.Exec(home, args, 0)
	}
	return gitx.Exec(home, args, 0)
}

func (p GitPort) isGitRepo(home string) bool {
	if p.IsGitRepo != nil {
		return p.IsGitRepo(home)
	}
	return gitx.IsGitRepo(home)
}

func (p GitPort) hasUpstream(home string) bool {
	if p.HasUpstream != nil {
		return p.HasUpstream(home)
	}
	return gitx.HasUpstream(home)
}

func (p GitPort) hasPushTarget(home string) bool {
	if p.HasPushTarget != nil {
		return p.HasPushTarget(home)
	}
	return gitx.HasPushTarget(home)
}

func (p GitPort) hasRemote(home string) bool {
	if p.HasRemote != nil {
		return p.HasRemote(home)
	}
	return gitx.HasRemote(home)
}

func (p GitPort) pushHint(home string) string {
	if p.PushHint != nil {
		return p.PushHint(home)
	}
	return gitx.PushHint(home)
}

func (p GitPort) upstreamRef(home string) string {
	if p.UpstreamRef != nil {
		return p.UpstreamRef(home)
	}
	return gitx.UpstreamRef(home)
}

func (p GitPort) fetch(home string) gitx.ExecResult {
	if p.Fetch != nil {
		return p.Fetch(home)
	}
	if p.GitFetch != nil {
		return p.GitFetch(home)
	}
	return gitx.Fetch(home)
}

func (p GitPort) push(home string) gitx.ExecResult {
	if p.Push != nil {
		return p.Push(home)
	}
	if p.GitPush != nil {
		return p.GitPush(home)
	}
	return gitx.Push(home)
}

func (p GitPort) headCommit(home string) string {
	if p.HeadCommit != nil {
		return p.HeadCommit(home)
	}
	return gitx.HeadCommit(home)
}

func (p GitPort) isStoreClean(home string) bool {
	if p.IsStoreClean != nil {
		return p.IsStoreClean(home)
	}
	return gitx.IsStoreClean(home)
}

func (p GitPort) isPushClean(home string) bool {
	if p.IsPushClean != nil {
		return p.IsPushClean(home)
	}
	return gitx.IsPushClean(home)
}

func (p GitPort) isAncestorOf(home, ancestor, descendant string) bool {
	if p.IsAncestorOf != nil {
		return p.IsAncestorOf(home, ancestor, descendant)
	}
	return gitx.IsAncestorOf(home, ancestor, descendant)
}

func (p GitPort) mergeFFUpstream(home string) gitx.ExecResult {
	if p.MergeFFUpstream != nil {
		return p.MergeFFUpstream(home)
	}
	return gitx.MergeFFUpstream(home)
}

func (p GitPort) ensureGitRepo(home string) error {
	if p.EnsureGitRepo != nil {
		return p.EnsureGitRepo(home)
	}
	return gitx.EnsureGitRepo(home)
}

func (p GitPort) commitStore(paths core.HomerPaths, message string) string {
	if p.CommitStoreIfNeeded != nil {
		return p.CommitStoreIfNeeded(paths, message)
	}
	if p.CommitAllStore != nil {
		return p.CommitAllStore(paths.Home, message)
	}
	return gitx.CommitAllStore(paths.Home, message)
}

func (p GitPort) requireCleanStore(paths core.HomerPaths) error {
	if p.RequireCleanStore != nil {
		return p.RequireCleanStore(paths)
	}
	return syncx.RequireCleanStore(paths)
}

func (p GitPort) requireFastForwardable(paths core.HomerPaths) error {
	if p.RequireFastForwardable != nil {
		return p.RequireFastForwardable(paths)
	}
	return syncx.RequireFastForwardable(paths)
}

func sourceValue(value any) (syncx.SyncSources, bool) {
	switch source := value.(type) {
	case syncx.SyncSources:
		return source, true
	case *syncx.SyncSources:
		if source != nil {
			return *source, true
		}
	case func() syncx.SyncSources:
		if source != nil {
			return source(), true
		}
	case func() *syncx.SyncSources:
		if source != nil {
			if result := source(); result != nil {
				return *result, true
			}
		}
	}
	return syncx.SyncSources{}, false
}

func collectSources(value any, paths core.HomerPaths, config core.HomerConfig, fetch bool) syncx.SyncSources {
	if source, ok := sourceValue(value); ok {
		return source
	}
	return syncx.CollectSyncSources(paths, config, syncx.CollectSyncSourcesOptions{Fetch: fetch})
}

func nowState(paths core.HomerPaths, commit, command string) error {
	state := core.LoadState(paths)
	state.Version = 1
	state.LastSyncCommit = commit
	state.LastSyncAt = time.Now().UTC().Format(time.RFC3339Nano)
	state.LastSyncCommand = command
	return core.SaveState(paths, state)
}

func emptyApplyResult() syncx.ApplyResult {
	return syncx.ApplyResult{
		Written:   []syncx.FileRef{},
		Deleted:   []syncx.FileRef{},
		Conflicts: []syncx.FileRef{},
	}
}

func homeFor(optionsHome string) core.HomerPaths {
	return resolveCommandPaths(optionsHome)
}

func promptConfirm(value any, message string, fallback bool) bool {
	if value != nil {
		if prompt, ok := value.(confirmPrompter); ok {
			return prompt.Confirm(message, fallback)
		}
	}
	if !isTTY() {
		return fallback
	}
	answer := fallback
	if err := survey.AskOne(&survey.Confirm{Message: message, Default: fallback}, &answer); err != nil {
		return false
	}
	return answer
}

func promptSelect(value any, message string, options []selectOption, fallback string) string {
	if value != nil {
		if prompt, ok := value.(selectPrompter); ok {
			return prompt.Select(message, options, fallback)
		}
	}
	if !isTTY() {
		return fallback
	}
	labels := make([]string, len(options))
	defaultIndex := 0
	for i, option := range options {
		labels[i] = option.Label
		if option.Value == fallback {
			defaultIndex = i
		}
	}
	selected := labels[defaultIndex]
	if err := survey.AskOne(&survey.Select{Message: message, Options: labels, Default: defaultIndex}, &selected); err != nil {
		return fallback
	}
	for _, option := range options {
		if option.Label == selected {
			return option.Value
		}
	}
	return fallback
}

func isTTY() bool {
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func fileRef(ref syncx.FileRef) string {
	return ref.AdapterID + "/" + ref.Category + "/" + ref.RelPath
}

func conflictRef(action syncx.PullAction) string {
	keys := ""
	if len(action.KeyPaths) > 0 {
		keys = " [" + strings.Join(action.KeyPaths, ", ") + "]"
	}
	return fileRef(syncx.FileRef{AdapterID: action.AdapterID, Category: action.Category, RelPath: action.RelPath}) + " (" + action.Reason + ")" + keys
}

func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return "未知错误"
}

func strictlyAhead(home string, git GitPort) bool {
	ref := git.upstreamRef(home)
	if ref == "" {
		return false
	}
	head := git.headCommit(home)
	if head == "" || git.isStoreClean(home) == false {
		return false
	}
	resolved := git.exec(home, []string{"rev-parse", "--verify", ref + "^{commit}"})
	if !resolved.OK {
		return false
	}
	upstream := strings.TrimSpace(resolved.Stdout)
	return upstream != "" && upstream != head && git.isAncestorOf(home, upstream, head)
}

func changedCount(files []syncx.FileRef) int { return len(files) }

// -------------------------------------------------------------------------
// push
// -------------------------------------------------------------------------

type PushOptions struct {
	HomerHome string
	Home      string
	JSON      bool
	Yes       bool
	NoPush    bool
}

type PushStatus string

const (
	PushStatusPushed          PushStatus = "pushed"
	PushStatusNoDrift         PushStatus = "no-drift"
	PushStatusSecretsRejected PushStatus = "secrets-rejected"
	PushStatusRemoteAhead     PushStatus = "remote-ahead"
	PushStatusConflicts       PushStatus = "conflicts"
	PushStatusAborted         PushStatus = "aborted"
	PushStatusError           PushStatus = "error"
)

// SecretFinding is re-exported for command consumers.
type SecretFinding = secretscan.SecretFinding

type PushReport struct {
	OK             bool            `json:"ok"`
	Status         PushStatus      `json:"status"`
	Secrets        []SecretFinding `json:"secrets"`
	ChangedFiles   []syncx.FileRef `json:"changedFiles"`
	Commit         string          `json:"commit,omitempty"`
	PushedToRemote bool            `json:"pushedToRemote"`
	Warnings       []string        `json:"warnings"`
	Errors         []string        `json:"errors"`
}

func (report PushReport) ExitCode() int {
	if report.OK {
		return 0
	}
	return 1
}

const PUSH_USAGE = `用法: homer push [options]

把本地快照推送到 store 并提交到 git 仓库（有 push target 时一并推送远端）。
推送前强制执行密钥扫描，命中即拒推（exit 1）。

选项:
  --home <dir>       homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --yes              跳过交互确认（非 TTY 环境必须显式给出）
  --no-push          只本地 commit，不推送远端
  --json             输出机器可读 JSON（PushReport）
  -h, --help         显示本帮助

退出码: pushed / no-drift → 0；其余状态 → 1。`

func newPushCommandReport(status PushStatus) PushReport {
	return PushReport{
		OK:           status == PushStatusPushed || status == PushStatusNoDrift,
		Status:       status,
		Secrets:      []SecretFinding{},
		ChangedFiles: []syncx.FileRef{},
		Warnings:     []string{},
		Errors:       []string{},
	}
}

// RunPush executes the complete write path and always returns a structured
// report, including precondition/config failures.  This guarantees that
// `--json` remains parseable at every command exit.
func RunPush(options PushOptions, deps *PushDeps) (report PushReport) {
	report = newPushCommandReport(PushStatusError)
	defer func() {
		if recovered := recover(); recovered != nil {
			report = newPushCommandReport(PushStatusError)
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
		if errors.Is(err, os.ErrNotExist) {
			report.Errors = []string{fmt.Sprintf("未找到 homer 配置: %s", paths.ConfigFile), "请先运行 `homer init` 生成 homer.json 与 store 快照。"}
		} else {
			report.Errors = errorLines(err)
		}
		return report
	}

	git := GitPort{}
	if deps != nil {
		git = gitPortFrom(deps.Git)
	}
	sourceInput := any(nil)
	if deps != nil {
		sourceInput = deps.Sources
	}
	sources := collectSources(sourceInput, paths, *config, true)
	warnings := append([]string{}, sources.Warnings...)
	warnings = append(warnings, sources.Errors...)

	prepared := syncx.PrepareStoreSnapshot(sources.Local, *config)
	findings := secretscan.FilterIgnored(secretscan.ScanSnapshots(prepared), secretIgnorePaths(config))
	if len(findings) > 0 {
		report = newPushCommandReport(PushStatusSecretsRejected)
		report.Secrets = findings
		report.Warnings = warnings
		report.Errors = []string{
			fmt.Sprintf("检测到 %d 处疑似密钥，已拒绝推送（store 未写入，未产生 commit）", len(findings)),
			"请移除密钥，或在 homer.json 的 secrets.ignorePaths 中显式豁免该路径。",
		}
		return report
	}

	check := syncx.CheckPushSafety(*config, sources.Base, sources.Local, sources.Remote)
	localAhead := strictlyAhead(paths.Home, git)
	if check.Status == syncx.PushStatusRemoteAhead && localAhead {
		warnings = append(warnings, "本地严格领先 upstream（上次推送未成功），remote-ahead 是 fetch 陈旧导致的假阳性；已跳过拦截，直接重试推送")
		check = syncx.CheckPushSafety(*config, sources.Base, sources.Local, sources.Base)
	}
	if check.Status == syncx.PushStatusRemoteAhead {
		for _, item := range check.RemoteAheadFiles {
			warnings = append(warnings, "远端变更: "+fileRef(item))
		}
		report = newPushCommandReport(PushStatusRemoteAhead)
		report.Warnings = warnings
		report.Errors = []string{fmt.Sprintf("远端有 %d 处本地未见的变更，已拒绝推送（请先运行 `homer pull`）", len(check.RemoteAheadFiles))}
		return report
	}
	if check.Status == syncx.PushStatusConflicts {
		for _, conflict := range check.ConflictItems {
			warnings = append(warnings, "冲突: "+conflictRef(conflict))
		}
		report = newPushCommandReport(PushStatusConflicts)
		report.Warnings = warnings
		report.Errors = []string{fmt.Sprintf("本地与远端有 %d 处冲突，已拒绝推送（请运行 `homer merge` 逐项裁决）", len(check.ConflictItems))}
		return report
	}

	changedFiles := append([]syncx.FileRef(nil), check.ChangedFiles...)
	// A first push is itself a sync operation even when the adapter snapshot has
	// no drift: the repository, .gitignore, homer.json, and store need a
	// portable baseline. This is the v1.1 upgrade from the old M1 no-op for a
	// non-git ~/.homer. An unborn repository and an existing repository with an
	// uncommitted configuration-center path use the same baseline path.
	needsBaseline := !git.isGitRepo(paths.Home) || git.headCommit(paths.Home) == "" || !git.isPushClean(paths.Home)
	if len(changedFiles) == 0 && !needsBaseline {
		if localAhead && !options.NoPush {
			if retry, ok := retryPush(paths.Home, git, warnings); ok {
				return retry
			}
		}
		report = newPushCommandReport(PushStatusNoDrift)
		report.Warnings = warnings
		return report
	}
	if len(changedFiles) == 0 {
		warnings = append(warnings, "本地快照已在 store 中，本次不重写 store 内容；将提交 store + homer.json + .gitignore，建立同步基线")
	}
	if !git.isGitRepo(paths.Home) {
		warnings = append(warnings, "~/.homer 尚未建立 git 仓库，本次将自动 git init 并建立同步基线")
	}

	var ui selectPrompter
	if deps != nil {
		ui = deps.UI
	}
	if !options.Yes {
		preview := pushPreview(changedFiles)
		if !promptConfirm(ui, preview, false) {
			if ui == nil && !isTTY() {
				warnings = append(warnings, "非交互环境，请加 --yes")
			}
			report = newPushCommandReport(PushStatusAborted)
			report.ChangedFiles = changedFiles
			report.Warnings = warnings
			report.Errors = []string{"已取消：未确认推送（store 未写入，未产生 commit）"}
			return report
		}
	}

	for _, snapshot := range prepared {
		if err := core.WriteSnapshotToStore(paths, snapshot); err != nil {
			report = newPushCommandReport(PushStatusError)
			report.ChangedFiles = changedFiles
			report.Warnings = warnings
			report.Errors = errorLines(err)
			return report
		}
	}
	if err := git.ensureGitRepo(paths.Home); err != nil {
		report = newPushCommandReport(PushStatusError)
		report.ChangedFiles = changedFiles
		report.Warnings = warnings
		report.Errors = errorLines(err)
		return report
	}
	commit := git.commitStore(paths, pushCommitMessage(len(changedFiles)))
	if commit == "" && !git.isPushClean(paths.Home) {
		report = newPushCommandReport(PushStatusError)
		report.ChangedFiles = changedFiles
		report.Warnings = warnings
		report.Errors = []string{
			fmt.Sprintf("store 已写入但 git commit 失败: %s", paths.Home),
			"请检查 git 是否可用与 user.name / user.email 配置，然后重试。",
		}
		return report
	}
	if commit == "" {
		commit = git.headCommit(paths.Home)
	}
	if commit == "" {
		report = newPushCommandReport(PushStatusError)
		report.ChangedFiles = changedFiles
		report.Warnings = warnings
		report.Errors = []string{fmt.Sprintf("无法确定 HEAD commit: %s（store 已写入）", paths.Home)}
		return report
	}
	if err := nowState(paths, commit, "push"); err != nil {
		report = newPushCommandReport(PushStatusError)
		report.ChangedFiles = changedFiles
		report.Commit = commit
		report.Warnings = warnings
		report.Errors = errorLines(err)
		return report
	}
	if !options.NoPush && git.hasPushTarget(paths.Home) {
		pushed := git.push(paths.Home)
		if !pushed.OK {
			reason := firstLine(pushed.Stderr)
			hint := git.pushHint(paths.Home)
			report = newPushCommandReport(PushStatusError)
			report.ChangedFiles = changedFiles
			report.Commit = commit
			report.Warnings = append(warnings, "远端未推送；请运行 `"+hint+"`")
			report.Errors = []string{fmt.Sprintf("本地 commit 已成功（%s），远端推送失败: %s", shortCommit(commit), reason)}
			return report
		}
		report = newPushCommandReport(PushStatusPushed)
		report.ChangedFiles = changedFiles
		report.Commit = commit
		report.PushedToRemote = true
		report.Warnings = warnings
		return report
	}

	report = newPushCommandReport(PushStatusPushed)
	report.ChangedFiles = changedFiles
	report.Commit = commit
	report.Warnings = warnings
	if options.NoPush {
		report.Warnings = append(report.Warnings, "--no-push: 只做本地 commit，未推送远端")
		if git.hasRemote(paths.Home) {
			report.Warnings = append(report.Warnings, "如需推送请运行 `"+git.pushHint(paths.Home)+"`")
		}
	} else if git.hasRemote(paths.Home) {
		report.Warnings = append(report.Warnings, "已配置 remote 但本次未推送；请运行 `"+git.pushHint(paths.Home)+"`")
	} else if !git.hasPushTarget(paths.Home) {
		report.Warnings = append(report.Warnings, "未配置 git remote，仅本地 commit（local-only 模式；如需推送请先运行 `homer remote <url>`）")
	}
	return report
}

func secretIgnorePaths(config *core.HomerConfig) []string {
	if config == nil || config.Secrets == nil {
		return nil
	}
	return config.Secrets.IgnorePaths
}

func pushCommitMessage(changedCount int) string {
	if changedCount == 0 {
		return "homer push: 建立同步基线（store 首次入库）"
	}
	return fmt.Sprintf("homer push: 同步 %d 个变更文件", changedCount)
}

func retryPush(home string, git GitPort, warnings []string) (PushReport, bool) {
	if !git.hasPushTarget(home) {
		return PushReport{}, false
	}
	head := git.headCommit(home)
	if head == "" {
		return PushReport{}, false
	}
	pushed := git.push(home)
	if !pushed.OK {
		report := newPushCommandReport(PushStatusError)
		report.Commit = head
		report.Warnings = append(warnings, "远端未推送；请运行 `"+git.pushHint(home)+"`")
		report.Errors = []string{fmt.Sprintf("本地 commit 已成立（%s），重试推送远端仍失败: %s", shortCommit(head), firstLine(pushed.Stderr))}
		return report, true
	}
	warnings = append(warnings, "本地提交已推送到远端（上次推送失败的补推送）")
	report := newPushCommandReport(PushStatusPushed)
	report.Commit = head
	report.PushedToRemote = true
	report.Warnings = warnings
	return report, true
}

func pushPreview(files []syncx.FileRef) string {
	limit := len(files)
	if limit > 5 {
		limit = 5
	}
	names := make([]string, 0, limit)
	for _, file := range files[:limit] {
		names = append(names, fileRef(file))
	}
	more := ""
	if len(files) > limit {
		more = fmt.Sprintf(" 等 %d 个", len(files))
	}
	return fmt.Sprintf("将写入 store 并提交 %d 个变更文件（%s%s），继续？", len(files), strings.Join(names, ", "), more)
}

func shortCommit(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}

// RenderPushReport is the stable human renderer. Every count printed here is
// sourced from the same slices exposed in PushReport, so text and JSON cannot
// disagree about the result.
func RenderPushReport(report PushReport) string {
	titles := map[PushStatus]string{
		PushStatusPushed:          "已推送",
		PushStatusNoDrift:         "无漂移，无需推送",
		PushStatusSecretsRejected: "密钥命中，已拒绝推送",
		PushStatusRemoteAhead:     "远端有本地未见的变更，已拒绝推送",
		PushStatusConflicts:       "存在冲突，已拒绝推送",
		PushStatusAborted:         "已取消",
		PushStatusError:           "失败",
	}
	lines := []string{fmt.Sprintf("homer push: %s（%s）", report.Status, titles[report.Status])}
	lines = append(lines, fmt.Sprintf("  变更文件: %d", len(report.ChangedFiles)))
	for _, file := range report.ChangedFiles {
		lines = append(lines, "    "+fileRef(file))
	}
	if report.Commit != "" {
		lines = append(lines, "  本地提交: "+shortCommit(report.Commit))
	}
	lines = append(lines, fmt.Sprintf("  远端推送: %s", ternary(report.PushedToRemote, "已推送", "未推送")))
	lines = append(lines, fmt.Sprintf("  密钥命中: %d", len(report.Secrets)))
	for _, finding := range report.Secrets {
		lines = append(lines, fmt.Sprintf("    %s:%d [%s] %s", finding.Path, finding.Line, finding.PatternID, finding.Description))
		lines = append(lines, "      摘录: "+finding.Excerpt)
	}
	for _, warning := range report.Warnings {
		lines = append(lines, "  ⚠ "+warning)
	}
	for _, err := range report.Errors {
		lines = append(lines, "  ✗ "+err)
	}
	return strings.Join(lines, "\n")
}

func RenderPushJSON(report PushReport) string {
	keys := []string{"ok", "status", "secrets", "changedFiles"}
	values := map[string]orderedjson.Value{
		"ok":           report.OK,
		"status":       string(report.Status),
		"secrets":      pushFindingsValue(report.Secrets),
		"changedFiles": fileRefsValue(report.ChangedFiles),
	}
	if report.Commit != "" {
		keys = append(keys, "commit")
		values["commit"] = report.Commit
	}
	keys = append(keys, "pushedToRemote", "warnings", "errors")
	values["pushedToRemote"] = report.PushedToRemote
	values["warnings"] = stringArrayValue(report.Warnings)
	values["errors"] = stringArrayValue(report.Errors)
	return string(orderedjson.Serialize(&orderedjson.Object{Keys: keys, M: values}))
}

func pushFindingsValue(findings []SecretFinding) orderedjson.Value {
	items := make([]orderedjson.Value, 0, len(findings))
	for _, finding := range findings {
		items = append(items, &orderedjson.Object{
			Keys: []string{"patternId", "description", "path", "line", "excerpt"},
			M: map[string]orderedjson.Value{
				"patternId": finding.PatternID, "description": finding.Description, "path": finding.Path,
				"line": orderedNumber(fmt.Sprint(finding.Line)), "excerpt": finding.Excerpt,
			},
		})
	}
	return items
}

func fileRefsValue(files []syncx.FileRef) orderedjson.Value {
	items := make([]orderedjson.Value, 0, len(files))
	for _, file := range files {
		items = append(items, &orderedjson.Object{
			Keys: []string{"adapterId", "category", "relPath"},
			M:    map[string]orderedjson.Value{"adapterId": file.AdapterID, "category": file.Category, "relPath": file.RelPath},
		})
	}
	return items
}

func stringArrayValue(values []string) orderedjson.Value {
	items := make([]orderedjson.Value, len(values))
	for i, value := range values {
		items[i] = value
	}
	return items
}

func orderedNumber(text string) orderedjson.Value {
	value, err := orderedjson.Parse([]byte(text))
	if err != nil {
		return text
	}
	return value
}

// io.Writer helper used by the command dispatcher and unit tests.
func writeReportJSON(out io.Writer, text string) error {
	if out == nil {
		return nil
	}
	_, err := fmt.Fprintln(out, text)
	return err
}

// Keep a package-local spelling matching the archived command modules.
func runPush(options PushOptions, deps *PushDeps) PushReport { return RunPush(options, deps) }
