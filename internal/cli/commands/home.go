package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/agecrypto"
	"github.com/zzjcool/homer-cli/internal/backup"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gitx"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
	syncx "github.com/zzjcool/homer-cli/internal/sync"
)

// HomeOptions controls `homer home <repo-url>`. Home is kept as an alias for
// callers that use the shared flag field directly.
type HomeOptions struct {
	HomerHome string
	Home      string
	RepoURL   string
	RepoUrl   string // compatibility spelling for callers using Go's URL style
	JSON      bool
	Yes       bool
	Mode      syncx.FirstContactMode
}

type FirstContactMode = syncx.FirstContactMode
type HomeConflictSummary = syncx.PullConflictAction

type HomeStatus string

const (
	HomeStatusHomed   HomeStatus = "homed"
	HomeStatusAborted HomeStatus = "aborted"
	HomeStatusError   HomeStatus = "error"

	FirstContactPull  = syncx.FirstContactPull
	FirstContactMerge = syncx.FirstContactMerge
	FirstContactSkip  = syncx.FirstContactSkip
)

type HomeSecretsReport struct {
	Pulled  []string `json:"pulled"`
	Skipped []string `json:"skipped"`
	Errors  []string `json:"errors"`
}

type HomeFirstContactReport struct {
	Mode      syncx.FirstContactMode     `json:"mode"`
	Applied   syncx.ApplyResult          `json:"applied"`
	Conflicts []syncx.PullConflictAction `json:"conflicts"`
}

type HomeReport struct {
	OK           bool                    `json:"ok"`
	Status       HomeStatus              `json:"status"`
	Cloned       bool                    `json:"cloned"`
	AdapterIDs   []string                `json:"adapterIds"`
	FirstContact *HomeFirstContactReport `json:"firstContact,omitempty"`
	Secrets      HomeSecretsReport       `json:"secrets"`
	Doctor       *DoctorReport           `json:"doctor,omitempty"`
	Warnings     []string                `json:"warnings"`
	Errors       []string                `json:"errors"`
}

func newHomeReport(status HomeStatus) HomeReport {
	return HomeReport{
		OK:         status == HomeStatusHomed,
		Status:     status,
		AdapterIDs: []string{},
		Secrets: HomeSecretsReport{
			Pulled: []string{}, Skipped: []string{}, Errors: []string{},
		},
		Warnings: []string{},
		Errors:   []string{},
	}
}

func (report HomeReport) ExitCode() int {
	if report.OK {
		return 0
	}
	return 1
}

// HomeDeps are the Go command seam. Clone is deliberately an error-returning
// function (rather than a git port) so tests can provide a fake origin while
// all normal git behavior stays behind gitx.CloneRepo.
type HomeDeps struct {
	UI    any
	Age   agecrypto.AgeCryptoPort
	Git   any
	Clone func(repoURL, destDir string) error
}

const HOME_USAGE = `用法: homer home <repo-url> [options]

新机器一键归位：clone 配置仓库 → 应用各 adapter 分类配置 → 首次对接三选一
→ 用本机 age identity 解密密钥归位 → 体检。

选项:
  --home <dir>             目标工作区（默认 $HOMER_HOME 或 ~/.homer；须不存在或为空目录）
  --mode pull|merge|skip   首次对接模式（pull=远端覆盖 / merge=合并保留本地 / skip=暂不应用）
  --yes                    跳过交互确认；未给 --mode 时默认 merge
  --json                   输出机器可读 JSON（HomeReport）
  -h, --help               显示本帮助

退出码: homed → 0；aborted / error → 1（doctor 的 fail 不影响 home 退出码）。`

const homePreviewMaxActions = 20

func homeRepoURL(options HomeOptions) string {
	if options.RepoURL != "" {
		return options.RepoURL
	}
	return options.RepoUrl
}

func assertHomeTargetEmpty(home string) error {
	info, err := os.Stat(home)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return core.NewCliError(fmt.Sprintf("目标路径不是目录: %s", home))
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return core.NewCliError(fmt.Sprintf("目标目录非空: %s（%d 项）\n请先备份并移走该目录中的内容，或用 `--home <dir>` 指定另一个工作区（homer 不覆盖已有目录）。", home, len(entries)))
	}
	return nil
}

func assertHomeCloneableURL(url string) error {
	if strings.HasPrefix(url, "-") {
		return core.NewCliError(fmt.Sprintf("非法 repo URL: %q\nrepo URL 不得以 \"-\" 开头（那会被 git 当成选项，例如 `--upload-pack`）。", url))
	}
	return nil
}

func homeClone(url, dest string, deps *HomeDeps) error {
	if deps != nil && deps.Clone != nil {
		return deps.Clone(url, dest)
	}
	return gitx.CloneRepo(url, dest, cloneTimeout)
}

const cloneTimeout = 60 * time.Second

// assertHomeNothingBeyondClone catches the clone-time TOCTOU case using git's
// own ignored/untracked view. The injected clone seam is intentionally exempt:
// its test fixture contract may create a local keys/ identity.
func assertHomeNothingBeyondClone(home string) error {
	result := gitx.Exec(home, []string{"status", "--porcelain", "--ignored", "--untracked-files=all"}, 0)
	if !result.OK {
		return nil
	}
	stray := make([]string, 0)
	for _, line := range strings.Split(result.Stdout, "\n") {
		if strings.TrimSpace(line) != "" {
			stray = append(stray, strings.TrimSpace(line))
		}
	}
	if len(stray) == 0 {
		return nil
	}
	shown := stray
	if len(shown) > homePreviewMaxActions {
		shown = shown[:homePreviewMaxActions]
	}
	message := fmt.Sprintf("clone 后的工作区含仓库之外的条目（%d 项，可能有其它进程在 clone 期间写入了该目录）", len(stray))
	message += "\n" + strings.Join(shown, "\n")
	if len(stray) > len(shown) {
		message += fmt.Sprintf("\n… 其余 %d 项省略", len(stray)-len(shown))
	}
	message += fmt.Sprintf("\nhomer 拒绝在来源不明的目录上继续；请移走这些条目后重试（或换一个 --home）。目录: %s", home)
	return core.NewCliError(message)
}

func homeEnabledAdapterIDs(config core.HomerConfig) []string {
	ids := make([]string, 0, len(config.Adapters))
	for id, item := range config.Adapters {
		if item.Enabled == nil || *item.Enabled {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func homeScanLocal(config core.HomerConfig, remote []core.AdapterSnapshot, warnings *[]string) []core.AdapterSnapshot {
	ids := homeEnabledAdapterIDs(config)
	local := make([]core.AdapterSnapshot, 0, len(ids))
	for _, id := range ids {
		cfg := config.Adapters[id]
		outcome := adapter.ScanAdapter(id, cfg)
		problems := make([]core.ScanProblem, len(outcome.Errors))
		for i, item := range outcome.Errors {
			problems[i] = core.ScanProblem{Path: item.Path, Message: item.Message}
		}
		like := core.ScanOutcomeLike{Snapshot: outcome.Snapshot, Errors: problems}
		*warnings = append(*warnings, core.ScanWarningMessages(id, like)...)
		if core.IsRootUnreadable(like) {
			for _, snapshot := range remote {
				if snapshot.AdapterID == id {
					local = append(local, snapshot)
					goto next
				}
			}
		}
		local = append(local, outcome.Snapshot)
	next:
	}
	return local
}

type homeRootEnsureResult struct {
	Retried  bool
	Warnings []string
}

// ensureHomeAdapterRoots creates missing enabled adapter roots before the
// first-contact plan is applied. A missing root is otherwise treated as
// unreadable by homeScanLocal and safely substituted with the remote snapshot;
// creating it and rescanning lets the normal write actions materialize the
// configuration without weakening the root-unreadable guard elsewhere.
func ensureHomeAdapterRoots(config core.HomerConfig) homeRootEnsureResult {
	result := homeRootEnsureResult{Warnings: []string{}}
	for _, id := range homeEnabledAdapterIDs(config) {
		root := core.ExpandHome(config.Adapters[id].Root)
		if _, err := os.Stat(root); err == nil || !os.IsNotExist(err) {
			continue
		}

		result.Retried = true
		if err := os.MkdirAll(root, 0o777); err != nil {
			like := core.ScanOutcomeLike{Errors: []core.ScanProblem{{
				Path:    root,
				Message: "创建 adapter root 失败: " + err.Error(),
			}}}
			result.Warnings = append(result.Warnings, core.ScanWarningMessages(id, like)...)
		}
	}
	return result
}

func homeMode(options HomeOptions, deps *HomeDeps) (syncx.FirstContactMode, bool) {
	if options.Mode != "" {
		return options.Mode, true
	}
	if options.Yes {
		return syncx.FirstContactMerge, true
	}
	var ui any
	if deps != nil {
		ui = deps.UI
	}
	if ui == nil && !isTTY() {
		return "", false
	}
	selected := promptSelect(ui, "选择首次对接方式（远端配置 vs 本机现状）", []selectOption{
		{Value: string(syncx.FirstContactPull), Label: "Pull：远端覆盖本机（本机独有文件保留，冲突项先备份）"},
		{Value: string(syncx.FirstContactMerge), Label: "Merge：合并，冲突保留本地（推荐）"},
		{Value: string(syncx.FirstContactSkip), Label: "Skip：暂不应用，只 clone 配置仓库"},
	}, string(syncx.FirstContactMerge))
	return syncx.FirstContactMode(selected), selected != ""
}

func homeActionRef(action syncx.PullAction) string {
	return fileRef(syncx.FileRef{AdapterID: action.AdapterID, Category: action.Category, RelPath: action.RelPath})
}

func BuildHomePreview(config core.HomerConfig, plan syncx.FirstContactPlan, secretNames []string) string {
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
	lines := []string{fmt.Sprintf("首次对接（%s）: 写入 %d  冲突 %d  删除 %d", plan.Mode, writes, conflicts, deletes)}
	for i, action := range plan.Actions {
		if i >= homePreviewMaxActions {
			break
		}
		switch action.Type {
		case syncx.PullActionWrite:
			lines = append(lines, "  写入 "+homeActionRef(action))
		case syncx.PullActionDelete:
			lines = append(lines, "  删除 "+homeActionRef(action))
		case syncx.PullActionConflict:
			keys := ""
			if len(action.KeyPaths) > 0 {
				keys = "（冲突键: " + strings.Join(action.KeyPaths, ", ") + "）"
			}
			lines = append(lines, fmt.Sprintf("  冲突 %s [%s]%s", homeActionRef(action), action.Reason, keys))
		}
	}
	if len(plan.Actions) > homePreviewMaxActions {
		lines = append(lines, fmt.Sprintf("  … 其余 %d 条省略", len(plan.Actions)-homePreviewMaxActions))
	}
	if len(secretNames) > 0 {
		lines = append(lines, fmt.Sprintf("密钥归位: %d 个（%s）", len(secretNames), strings.Join(secretNames, ", ")))
		for _, name := range secretNames {
			destination, err := agecrypto.DestinationOf(&config, name)
			if err != nil {
				destination = "[invalid destination]"
			}
			lines = append(lines, fmt.Sprintf("  %s → %s", name, destination))
		}
	}
	lines = append(lines, "以上将应用到本机工具目录（受影响文件先备份）与密钥目标路径。是否继续？")
	return strings.Join(lines, "\n")
}

const homeSecretHint = "两步补齐：① 本机 `homer secret keygen`；② 在旧机把输出的 recipient 加入 homer.json 的 secrets.recipients 后 `homer secret push`；③ 回本机 `homer secret pull`。"

func homePlaceSecrets(paths core.HomerPaths, config core.HomerConfig, crypto agecrypto.AgeCryptoPort, report *HomeSecretsReport, warnings *[]string) error {
	names := agecrypto.SecretNames(&config)
	if len(names) == 0 {
		return nil
	}
	identity, ok := agecrypto.LoadIdentity(paths)
	if !ok {
		report.Skipped = append(report.Skipped, names...)
		*warnings = append(*warnings, fmt.Sprintf("未找到本机 age identity，%d 个密钥未归位: %s", len(names), agecrypto.IdentityFilePath(paths)), homeSecretHint)
		return nil
	}
	ciphertexts := make(map[string][]byte, len(names))
	missing := make([]string, 0)
	for _, name := range names {
		file, err := agecrypto.SecretFilePath(paths, name)
		if err != nil {
			missing = append(missing, name)
			continue
		}
		data, readErr := os.ReadFile(file)
		if readErr != nil {
			missing = append(missing, filepath.ToSlash(filepath.Join("secrets", name+".age")))
			continue
		}
		ciphertexts[name] = data
	}
	if len(missing) > 0 {
		report.Errors = append(report.Errors, fmt.Sprintf("以下 vault 文件缺失（%d/%d），密钥未归位且未写入任何目标文件: %s", len(missing), len(names), strings.Join(missing, ", ")))
		*warnings = append(*warnings, "仓库里缺少密钥密文：请在旧机确认已 `homer secret push` 后回到本机 `homer secret pull`。")
		return nil
	}

	if crypto == nil {
		crypto = agecrypto.NewAgeCryptoPort()
	}
	plaintexts := make(map[string][]byte, len(names))
	failures := make([]string, 0)
	for _, name := range names {
		plaintext, err := crypto.Decrypt(ciphertexts[name], identity)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", name, safeErrorWithPrivate(err, identity.SecretKey)))
			continue
		}
		plaintexts[name] = plaintext
	}
	if len(failures) > 0 {
		report.Errors = append(report.Errors, fmt.Sprintf("以下密钥无法用本机 identity 解密（%d/%d），密钥未归位且未写入任何目标文件:", len(failures), len(names)))
		report.Errors = append(report.Errors, failures...)
		*warnings = append(*warnings, "本机 recipient 可能不在旧机的 secrets.recipients 里：请在旧机追加本机 recipient 后重新 `homer secret push`，再 `homer secret pull`。")
		return nil
	}

	backupDir, err := homeBackupSecretDestinations(paths, config, names)
	if err != nil {
		return err
	}
	_ = backupDir // HomeReport intentionally exposes only pulled/skipped/errors.
	for _, name := range names {
		destination, err := agecrypto.DestinationOf(&config, name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o777); err != nil {
			return err
		}
		if err := os.WriteFile(destination, plaintexts[name], 0o600); err != nil {
			return err
		}
		if err := os.Chmod(destination, 0o600); err != nil {
			return err
		}
	}
	report.Pulled = append(report.Pulled, names...)
	return nil
}

func homeBackupSecretDestinations(paths core.HomerPaths, config core.HomerConfig, names []string) (string, error) {
	targets := make([]backup.BackupTarget, 0, len(names))
	for _, name := range names {
		destination, err := agecrypto.DestinationOf(&config, name)
		if err != nil {
			return "", err
		}
		if _, statErr := os.Stat(destination); statErr == nil {
			targets = append(targets, backup.BackupTarget{SourceAbs: destination, Label: "secret/" + name})
		} else if !os.IsNotExist(statErr) {
			return "", statErr
		}
	}
	if len(targets) == 0 {
		return "", nil
	}
	result, err := backup.BackupFiles(paths, "home", targets, backup.BackupFilesOptions{Mode: backup.BackupFileModes{Dir: 0o700, File: 0o600}})
	if err != nil {
		return "", err
	}
	return result.BackupDir, nil
}

// RunHome runs all ten frozen steps. Early setup failures are represented as
// a structured error report rather than a second output channel, so --json is
// parseable even when the target is non-empty or clone/config validation fails.
func RunHome(options HomeOptions, deps *HomeDeps) (report HomeReport) {
	report = newHomeReport(HomeStatusError)
	defer func() {
		if recovered := recover(); recovered != nil {
			report = newHomeReport(HomeStatusError)
			report.Errors = []string{fmt.Sprint(recovered)}
		}
	}()

	homerHome := options.HomerHome
	if homerHome == "" {
		homerHome = options.Home
	}
	paths := homeFor(homerHome)
	warnings := []string{}
	url := homeRepoURL(options)
	if err := assertHomeTargetEmpty(paths.Home); err != nil {
		report.Errors = errorLines(err)
		return report
	}
	if err := assertHomeCloneableURL(url); err != nil {
		report.Errors = errorLines(err)
		return report
	}
	if err := homeClone(url, paths.Home, deps); err != nil {
		report.Errors = errorLines(err)
		return report
	}
	report.Cloned = true
	if deps == nil || deps.Clone == nil {
		if err := assertHomeNothingBeyondClone(paths.Home); err != nil {
			report.Errors = errorLines(err)
			return report
		}
	}

	config, err := core.LoadConfig(paths)
	if err != nil {
		report.Errors = []string{
			"该仓库不是 homer 配置中心: " + err.Error(),
			"根目录缺少 homer.json（若该仓库由旧版 homer 推送，请在机器 A 重新 homer push 以纳入配置文件）。",
			fmt.Sprintf("恢复步骤：在机器 A 运行 `homer push --yes` 纳入 homer.json；回到本机移走失败的目标目录 %s，再重跑 `homer home <repo-url> --yes`。请确认 URL 指向的是 homer 配置仓库。", paths.Home),
		}
		return report
	}
	report.AdapterIDs = homeEnabledAdapterIDs(*config)
	remote, err := core.ReadSnapshotFromStore(paths, *config)
	if err != nil {
		report.Errors = errorLines(err)
		return report
	}
	scanWarnings := []string{}
	local := homeScanLocal(*config, remote, &scanWarnings)
	warnings = append(warnings, scanWarnings...)
	mode, modeOK := homeMode(options, deps)
	if !modeOK {
		report.Errors = []string{"非交互环境无法选择首次对接模式", "请显式给出 `--mode pull|merge|skip`，或加 `--yes`（未给 --mode 时默认 merge）。"}
		report.Warnings = warnings
		return report
	}

	// A missing root is conservatively represented as local=remote by the
	// scanner. For an actual first-contact apply, create the root first and
	// rescan so that planFirstContact produces the expected remote writes.
	if mode == syncx.FirstContactPull || mode == syncx.FirstContactMerge {
		rootResult := ensureHomeAdapterRoots(*config)
		if rootResult.Retried {
			warnings = []string{}
			refreshedWarnings := []string{}
			local = homeScanLocal(*config, remote, &refreshedWarnings)
			warnings = append(warnings, refreshedWarnings...)
		}
		for _, warning := range rootResult.Warnings {
			seen := false
			for _, existing := range warnings {
				if existing == warning {
					seen = true
					break
				}
			}
			if !seen {
				warnings = append(warnings, warning)
			}
		}
	}

	plan := syncx.PlanFirstContact(*config, local, remote, mode)
	names := agecrypto.SecretNames(config)
	if !options.Yes && (len(plan.Actions) > 0 || len(names) > 0) {
		if !promptConfirm(depsHomeUI(deps), BuildHomePreview(*config, plan, names), false) {
			if depsHomeUI(deps) == nil && !isTTY() {
				warnings = append(warnings, "非交互环境无法确认，已中止；如需自动归位请加 --yes")
			}
			report = newHomeReport(HomeStatusAborted)
			report.Cloned = true
			report.AdapterIDs = homeEnabledAdapterIDs(*config)
			report.FirstContact = &HomeFirstContactReport{Mode: mode, Applied: emptyApplyResult(), Conflicts: []syncx.PullConflictAction{}}
			report.Warnings = warnings
			report.Errors = []string{"已取消：未确认归位（未写入任何工具目录 / 密钥目标，未更新 state）"}
			return report
		}
	}

	applied := emptyApplyResult()
	secretReport := HomeSecretsReport{Pulled: []string{}, Skipped: []string{}, Errors: []string{}}
	if len(plan.Actions) > 0 {
		applied, err = syncx.ApplyPullActions(paths, *config, syncx.PullPlan{Actions: plan.Actions}, syncx.ApplyPullActionsOptions{Backup: true, Command: "home"})
		if err != nil {
			report.Errors = errorLines(err)
			return report
		}
	}
	head := GitPort{}
	if deps != nil {
		head = gitPortFrom(deps.Git)
	}
	commit := head.headCommit(paths.Home)
	if commit != "" {
		if stateErr := nowState(paths, commit, "pull"); stateErr != nil {
			report.Errors = errorLines(stateErr)
			return report
		}
	} else {
		warnings = append(warnings, fmt.Sprintf("无法解析 HEAD（仓库没有 commit？），state.lastSyncCommit 未写入: %s", paths.Home))
		state := core.LoadState(paths)
		state.Version = 1
		state.LastSyncAt = nowTimestamp()
		state.LastSyncCommand = "pull"
		if stateErr := core.SaveState(paths, state); stateErr != nil {
			report.Errors = errorLines(stateErr)
			return report
		}
	}

	crypto := agecrypto.NewAgeCryptoPort()
	if deps != nil && deps.Age != nil {
		crypto = deps.Age
	}
	if err := homePlaceSecrets(paths, *config, crypto, &secretReport, &warnings); err != nil {
		report.Errors = errorLines(err)
		report.FirstContact = &HomeFirstContactReport{Mode: mode, Applied: applied, Conflicts: homePlanConflicts(plan)}
		report.Secrets = secretReport
		report.Warnings = warnings
		return report
	}

	var doctorDeps *DoctorDeps
	if deps != nil && deps.Age != nil {
		doctorDeps = &DoctorDeps{Age: deps.Age}
	}
	doctorReport := RunDoctor(DoctorOptions{HomerHome: paths.Home}, doctorDeps)
	report = newHomeReport(HomeStatusHomed)
	report.Cloned = true
	report.AdapterIDs = homeEnabledAdapterIDs(*config)
	report.FirstContact = &HomeFirstContactReport{Mode: mode, Applied: applied, Conflicts: homePlanConflicts(plan)}
	report.Secrets = secretReport
	report.Doctor = &doctorReport
	report.Warnings = warnings
	return report
}

func depsHomeUI(deps *HomeDeps) any {
	if deps == nil {
		return nil
	}
	return deps.UI
}

func homePlanConflicts(plan syncx.FirstContactPlan) []syncx.PullConflictAction {
	result := make([]syncx.PullConflictAction, 0)
	for _, action := range plan.Actions {
		if action.Type == syncx.PullActionConflict {
			result = append(result, action)
		}
	}
	return result
}

func nowTimestamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func RenderHomeReport(report HomeReport) string {
	lines := []string{fmt.Sprintf("homer home: %s", report.Status)}
	lines = append(lines, fmt.Sprintf("  已 clone: %s", ternary(report.Cloned, "是", "否")))
	if len(report.AdapterIDs) > 0 {
		lines = append(lines, "  adapter: "+strings.Join(report.AdapterIDs, ", "))
	}
	if report.FirstContact != nil {
		lines = append(lines, fmt.Sprintf("  首次对接: %s（写入 %d / 删除 %d / 冲突 %d）", report.FirstContact.Mode, len(report.FirstContact.Applied.Written), len(report.FirstContact.Applied.Deleted), len(report.FirstContact.Conflicts)))
	}
	if len(report.Secrets.Pulled) > 0 {
		lines = append(lines, fmt.Sprintf("  密钥归位: %d 个（%s）", len(report.Secrets.Pulled), strings.Join(report.Secrets.Pulled, ", ")))
	}
	if len(report.Secrets.Skipped) > 0 {
		lines = append(lines, fmt.Sprintf("  密钥跳过: %d 个（%s）", len(report.Secrets.Skipped), strings.Join(report.Secrets.Skipped, ", ")))
	}
	if report.Doctor != nil {
		lines = append(lines, fmt.Sprintf("  体检: %s（详见 `homer doctor`）", ternary(report.Doctor.OK, "通过", "存在问题")))
	}
	for _, warning := range report.Warnings {
		lines = append(lines, "  ⚠ "+warning)
	}
	for _, item := range report.Errors {
		lines = append(lines, "  ✗ "+item)
	}
	if report.Status == HomeStatusHomed && report.FirstContact != nil && len(report.FirstContact.Conflicts) > 0 {
		lines = append(lines, "  提示: 冲突项已保留本地（远端已被看见），可用 `homer status` 查看待 push 漂移。")
	}
	return strings.Join(lines, "\n")
}

func RenderHomeJSON(report HomeReport) string {
	keys := []string{"ok", "status", "cloned", "adapterIds"}
	values := map[string]orderedjson.Value{
		"ok": report.OK, "status": string(report.Status), "cloned": report.Cloned, "adapterIds": stringArrayValue(report.AdapterIDs),
	}
	if report.FirstContact != nil {
		conflicts := make([]orderedjson.Value, 0, len(report.FirstContact.Conflicts))
		for _, action := range report.FirstContact.Conflicts {
			itemKeys := []string{"type", "adapterId", "category", "relPath", "reason"}
			itemValues := map[string]orderedjson.Value{
				"type": action.Type, "adapterId": action.AdapterID, "category": action.Category, "relPath": action.RelPath, "reason": action.Reason,
			}
			if len(action.KeyPaths) > 0 {
				itemKeys = append(itemKeys, "keyPaths")
				itemValues["keyPaths"] = stringArrayValue(action.KeyPaths)
			}
			conflicts = append(conflicts, &orderedjson.Object{Keys: itemKeys, M: itemValues})
		}
		values["firstContact"] = &orderedjson.Object{
			Keys: []string{"mode", "applied", "conflicts"},
			M: map[string]orderedjson.Value{
				"mode": string(report.FirstContact.Mode), "applied": applyResultValue(report.FirstContact.Applied), "conflicts": conflicts,
			},
		}
		keys = append(keys, "firstContact")
	}
	values["secrets"] = &orderedjson.Object{
		Keys: []string{"pulled", "skipped", "errors"},
		M: map[string]orderedjson.Value{
			"pulled": stringArrayValue(report.Secrets.Pulled), "skipped": stringArrayValue(report.Secrets.Skipped), "errors": stringArrayValue(report.Secrets.Errors),
		},
	}
	keys = append(keys, "secrets")
	if report.Doctor != nil {
		values["doctor"] = doctorReportValue(*report.Doctor)
		keys = append(keys, "doctor")
	}
	keys = append(keys, "warnings", "errors")
	values["warnings"] = stringArrayValue(report.Warnings)
	values["errors"] = stringArrayValue(report.Errors)
	return string(orderedjson.Serialize(&orderedjson.Object{Keys: keys, M: values}))
}

func runHome(options HomeOptions, deps *HomeDeps) HomeReport { return RunHome(options, deps) }
func renderHomeReport(report HomeReport) string              { return RenderHomeReport(report) }
