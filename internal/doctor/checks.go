// Package doctor implements the read-only checks used by `homer doctor`.
//
// The package deliberately keeps the eight checks independent of command
// rendering.  A check may inspect the configured workspace and git state, but
// it never writes to disk and it never decides the process exit code.
package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zzjcool/homer-cli/internal/agecrypto"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gitx"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

// CheckStatus is the severity of one doctor check.
type CheckStatus string

const (
	CheckOK   CheckStatus = "ok"
	CheckWarn CheckStatus = "warn"
	CheckFail CheckStatus = "fail"
)

// DoctorCheckID is the stable id set and report order for the eight checks.
type DoctorCheckID string

const (
	CheckConfigID     DoctorCheckID = "config"
	CheckRepoID       DoctorCheckID = "repo"
	CheckStoreCleanID DoctorCheckID = "store-clean"
	CheckRemoteID     DoctorCheckID = "remote"
	CheckAdaptersID   DoctorCheckID = "adapters"
	CheckAgeID        DoctorCheckID = "age"
	CheckMachineID    DoctorCheckID = "machine"
	CheckRequiredID   DoctorCheckID = "required"
)

// DoctorCheck is one item in a DoctorReport. Details are omitted from JSON
// when empty, matching the TypeScript report shape where details is optional.
type DoctorCheck struct {
	ID      DoctorCheckID `json:"id"`
	Status  CheckStatus   `json:"status"`
	Message string        `json:"message"`
	Details []string      `json:"details,omitempty"`
}

// DoctorReport is the aggregate report. OK means that no check is fail;
// warnings are informational and do not change the doctor exit code.
type DoctorReport struct {
	Checks []DoctorCheck `json:"checks"`
	OK     bool          `json:"ok"`
}

// ExitCode maps a doctor report to the frozen process status.
func (report DoctorReport) ExitCode() int {
	if report.OK {
		return 0
	}
	return 1
}

// RemoteCheckTimeout is intentionally shorter than gitx's normal 15 second
// timeout: a diagnostic command should not appear to hang on a broken remote.
const RemoteCheckTimeout = 10 * time.Second

// REMOTE_CHECK_TIMEOUT is a migration-friendly alias used by focused tests.
const REMOTE_CHECK_TIMEOUT = RemoteCheckTimeout

// REMOTE_CHECK_TIMEOUT_MS preserves the millisecond spelling from the
// TypeScript plan while the Go implementation above uses time.Duration.
const REMOTE_CHECK_TIMEOUT_MS = 10_000

const maxStoreDetails = 20

// REQUIRED_PLACEHOLDER is the visible sentinel checked in store JSON.
const REQUIRED_PLACEHOLDER = "__REQUIRED__"

const requiredPlaceholder = REQUIRED_PLACEHOLDER

// RemoteCheckOptions controls checkRemote's network behaviour.
type RemoteCheckOptions struct {
	Offline bool
}

// RemoteOptions is the shorter spelling used by command callers.
type RemoteOptions = RemoteCheckOptions

// CheckConfig checks that homer.json exists and is a valid Homer config.
func CheckConfig(paths core.HomerPaths) DoctorCheck {
	data, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		message := fmt.Sprintf("无法读取 %s: %v", paths.ConfigFile, err)
		if errors.Is(err, os.ErrNotExist) {
			message = fmt.Sprintf("未找到 %s", paths.ConfigFile)
		}
		return DoctorCheck{
			ID:      CheckConfigID,
			Status:  CheckFail,
			Message: message,
			Details: []string{"先运行 `homer init` 生成 homer.json 与 store 快照。"},
		}
	}

	config, validationErrors := core.ValidateConfig(data)
	if config == nil {
		message := fmt.Sprintf("homer.json 配置无效（%d 处）", len(validationErrors))
		if len(validationErrors) == 1 && strings.Contains(validationErrors[0], "不是合法 JSON") {
			message = fmt.Sprintf("%s 不是合法 JSON", paths.ConfigFile)
		}
		return DoctorCheck{
			ID:      CheckConfigID,
			Status:  CheckFail,
			Message: message,
			Details: append([]string(nil), validationErrors...),
		}
	}

	adapterIDs := sortedKeys(config.Adapters)
	return DoctorCheck{
		ID:      CheckConfigID,
		Status:  CheckOK,
		Message: fmt.Sprintf("homer.json 有效（%d 个 adapter: %s）", len(adapterIDs), strings.Join(adapterIDs, ", ")),
	}
}

// CheckRepoAndStore returns the second and third checks in their frozen order.
func CheckRepoAndStore(paths core.HomerPaths) []DoctorCheck {
	return []DoctorCheck{checkRepo(paths), checkStoreClean(paths)}
}

func checkRepo(paths core.HomerPaths) DoctorCheck {
	if !gitx.IsGitRepo(paths.Home) {
		return DoctorCheck{
			ID:      CheckRepoID,
			Status:  CheckFail,
			Message: fmt.Sprintf("工作区不是 git 仓库: %s", paths.Home),
			Details: []string{"请先运行 `homer push` 建立 git 历史与 remote。"},
		}
	}

	ref := gitx.UpstreamRef(paths.Home)
	if ref == "" {
		return DoctorCheck{
			ID:      CheckRepoID,
			Status:  CheckWarn,
			Message: "git 仓库已就绪，但未配置 upstream（同步将停留在本地）",
			Details: []string{"配置远端：`git push -u <remote> <branch>`（或在 home 内 `git branch --set-upstream-to`）。"},
		}
	}
	return DoctorCheck{ID: CheckRepoID, Status: CheckOK, Message: fmt.Sprintf("git 仓库（upstream: %s）", ref)}
}

func checkStoreClean(paths core.HomerPaths) DoctorCheck {
	if !gitx.IsGitRepo(paths.Home) {
		return DoctorCheck{
			ID:      CheckStoreCleanID,
			Status:  CheckWarn,
			Message: "store 工作区状态不可判定（不是 git 仓库）",
		}
	}
	if gitx.IsStoreClean(paths.Home) {
		return DoctorCheck{ID: CheckStoreCleanID, Status: CheckOK, Message: "store 工作区干净"}
	}

	status := gitx.Exec(paths.Home, []string{"status", "--porcelain", "--untracked-files=all", "--", "store/"}, 0)
	lines := make([]string, 0)
	if status.OK {
		for _, line := range strings.Split(status.Stdout, "\n") {
			line = strings.TrimRight(line, "\r")
			if line != "" {
				lines = append(lines, line)
			}
		}
	}
	details := append([]string(nil), lines...)
	if len(details) > maxStoreDetails {
		details = details[:maxStoreDetails]
		details = append(details, fmt.Sprintf("… 其余 %d 条省略", len(lines)-maxStoreDetails))
	}
	if len(details) == 0 {
		details = append(details, "git status 无法判定（git 不可用？）")
	}
	details = append(details, "先运行 `homer push` 提交 store 改动，否则 pull / merge 会拒绝执行。")
	return DoctorCheck{
		ID:      CheckStoreCleanID,
		Status:  CheckWarn,
		Message: "store 工作区有未提交的改动",
		Details: details,
	}
}

// CheckRemote checks remote reachability. Offline is deliberately checked
// before repository discovery so --offline never invokes a network command.
func CheckRemote(paths core.HomerPaths, options RemoteCheckOptions) DoctorCheck {
	if options.Offline {
		return DoctorCheck{ID: CheckRemoteID, Status: CheckOK, Message: "远端可达性检查已跳过（--offline）"}
	}
	if !gitx.IsGitRepo(paths.Home) {
		return DoctorCheck{ID: CheckRemoteID, Status: CheckWarn, Message: "跳过远端检查：工作区不是 git 仓库"}
	}

	url := upstreamURL(paths.Home)
	if url == "" {
		return DoctorCheck{ID: CheckRemoteID, Status: CheckWarn, Message: "未配置 git upstream，跳过远端可达性检查"}
	}

	result := gitx.Exec(paths.Home, []string{"ls-remote", "--heads", "--", url}, RemoteCheckTimeout)
	if !result.OK {
		firstLine := ""
		for _, line := range strings.Split(result.Stderr, "\n") {
			if strings.TrimSpace(line) != "" {
				firstLine = strings.TrimSpace(line)
				break
			}
		}
		check := DoctorCheck{
			ID:      CheckRemoteID,
			Status:  CheckWarn,
			Message: fmt.Sprintf("远端不可达（离线？）: %s", url),
		}
		if firstLine != "" {
			check.Details = []string{firstLine}
		}
		return check
	}

	branches := 0
	if strings.TrimSpace(result.Stdout) != "" {
		for _, line := range strings.Split(result.Stdout, "\n") {
			if strings.TrimSpace(line) != "" {
				branches++
			}
		}
	}
	return DoctorCheck{
		ID:      CheckRemoteID,
		Status:  CheckOK,
		Message: fmt.Sprintf("远端可达: %s（%d 个分支）", url, branches),
	}
}

// upstreamURL resolves the configured remote from branch configuration first,
// avoiding ambiguity when a remote name itself contains a slash.
func upstreamURL(home string) string {
	ref := gitx.UpstreamRef(home)
	branchResult := gitx.Exec(home, []string{"symbolic-ref", "--short", "HEAD"}, 0)
	branch := ""
	if branchResult.OK {
		branch = strings.TrimSpace(branchResult.Stdout)
	}

	remote := ""
	if branch != "" {
		configured := gitx.Exec(home, []string{"config", "--get", "branch." + branch + ".remote"}, 0)
		if configured.OK {
			remote = strings.TrimSpace(configured.Stdout)
		}
	}
	if remote == "" && ref != "" {
		if slash := strings.IndexByte(ref, '/'); slash > 0 {
			remote = ref[:slash]
		}
	}
	if remote == "" {
		return ""
	}

	result := gitx.Exec(home, []string{"remote", "get-url", remote}, 0)
	if !result.OK {
		return ""
	}
	return strings.TrimSpace(result.Stdout)
}

// CheckAdapters reports missing enabled adapter roots as warnings. A missing
// tool is expected on a machine that does not use that adapter, so it is never
// a fail.
func CheckAdapters(config core.HomerConfig) DoctorCheck {
	missing := make([]string, 0)
	checked := 0
	for _, adapterID := range sortedKeys(config.Adapters) {
		adapter := config.Adapters[adapterID]
		if adapter.Enabled != nil && !*adapter.Enabled {
			continue
		}
		checked++
		root := core.ExpandHome(adapter.Root)
		info, err := os.Stat(root)
		if err != nil {
			missing = append(missing, fmt.Sprintf("%s: %s（%s）", adapterID, adapter.Root, root))
			continue
		}
		if !info.IsDir() {
			missing = append(missing, fmt.Sprintf("%s: %s（%s）不是目录", adapterID, adapter.Root, root))
		}
	}

	if checked == 0 {
		return DoctorCheck{ID: CheckAdaptersID, Status: CheckOK, Message: "没有启用的 adapter"}
	}
	if len(missing) == 0 {
		return DoctorCheck{ID: CheckAdaptersID, Status: CheckOK, Message: fmt.Sprintf("%d 个 adapter root 均存在", checked)}
	}
	return DoctorCheck{
		ID:      CheckAdaptersID,
		Status:  CheckWarn,
		Message: fmt.Sprintf("%d/%d 个 adapter root 缺失（工具未安装？）", len(missing), checked),
		Details: missing,
	}
}

const rePushHint = "若本机是新设备：在旧机把本机 recipient 加入 secrets.recipients，重新 `homer secret push` 后再 `homer secret pull`。"

// CheckAge checks identity/configuration and tries every configured vault. It
// reads a workspace vault first and falls back to the local upstream commit;
// this is important because secret pull intentionally does not fast-forward
// the whole Homer repository.
func CheckAge(paths core.HomerPaths, config core.HomerConfig, crypto agecrypto.AgeCryptoPort) (check DoctorCheck) {
	defer func() {
		check = appendPairedAgeDisplay(paths, check)
	}()
	files := map[string]string{}
	recipients := []string{}
	if config.Secrets != nil {
		if config.Secrets.Files != nil {
			files = config.Secrets.Files
		}
		recipients = config.Secrets.Recipients
	}
	names := sortedKeys(files)
	if len(names) == 0 && len(recipients) == 0 {
		return DoctorCheck{ID: CheckAgeID, Status: CheckOK, Message: "未配置密钥同步（secrets 段为空）"}
	}

	identity, identityOK := agecrypto.LoadIdentity(paths)
	if !identityOK {
		return DoctorCheck{
			ID:      CheckAgeID,
			Status:  CheckFail,
			Message: fmt.Sprintf("未找到可用的 age identity: %s", agecrypto.IdentityFilePath(paths)),
			Details: []string{"先运行 `homer secret keygen`。", rePushHint},
		}
	}
	if len(recipients) == 0 {
		return DoctorCheck{
			ID:      CheckAgeID,
			Status:  CheckFail,
			Message: "secrets.recipients 为空：secret push 没有加密目标",
			Details: []string{"在 homer.json 的 secrets.recipients 里至少配置一个 `age1...` 公钥。"},
		}
	}
	if len(names) == 0 {
		return DoctorCheck{
			ID:      CheckAgeID,
			Status:  CheckOK,
			Message: fmt.Sprintf("identity 就绪（%s），secrets.files 未配置密钥", identity.Recipient),
		}
	}

	statusByName := make(map[string]string)
	for _, entry := range agecrypto.ListSecrets(paths, &config) {
		statusByName[entry.Name] = entry.VaultFile
	}
	upstream := gitx.UpstreamRef(paths.Home)
	failed := make([]string, 0)
	missing := make([]string, 0)
	for _, name := range names {
		workspacePresent := statusByName[name] == "present"
		var upstreamBytes []byte
		if !workspacePresent && upstream != "" {
			if bytes, err := gitx.ReadVaultFileAtCommit(paths.Home, secretRelativePath(name), upstream); err == nil {
				upstreamBytes = bytes
			}
		}
		if !workspacePresent && upstreamBytes == nil {
			missing = append(missing, fmt.Sprintf("%s（%s.age 不存在）", name, name))
			continue
		}

		var workspaceErr error
		var upstreamErr error
		if crypto == nil {
			crypto = agecrypto.NewAgeCryptoPort()
		}
		if workspacePresent {
			file, err := agecrypto.SecretFilePath(paths, name)
			if err != nil {
				workspaceErr = err
			} else if ciphertext, readErr := os.ReadFile(file); readErr != nil {
				workspaceErr = readErr
			} else if _, decryptErr := crypto.Decrypt(ciphertext, identity); decryptErr == nil {
				continue
			} else {
				workspaceErr = decryptErr
			}
		}
		if upstreamBytes == nil && upstream != "" {
			if bytes, err := gitx.ReadVaultFileAtCommit(paths.Home, secretRelativePath(name), upstream); err == nil {
				upstreamBytes = bytes
			}
		}
		if upstreamBytes != nil {
			if _, decryptErr := crypto.Decrypt(upstreamBytes, identity); decryptErr == nil {
				continue
			} else {
				upstreamErr = decryptErr
			}
		}

		reason := "未知错误"
		if workspaceErr != nil {
			reason = safeCheckError(workspaceErr, identity.SecretKey)
		} else if upstreamErr != nil {
			reason = safeCheckError(upstreamErr, identity.SecretKey)
		}
		if workspaceErr != nil && upstreamErr != nil {
			failed = append(failed, fmt.Sprintf("%s: 工作区与 %s 均无法解密（%s）", name, upstream, reason))
		} else {
			failed = append(failed, fmt.Sprintf("%s: %s", name, reason))
		}
	}

	if len(failed) > 0 {
		return DoctorCheck{
			ID:      CheckAgeID,
			Status:  CheckFail,
			Message: fmt.Sprintf("%d 个密钥无法解密（本机 identity 不是 recipient？）", len(failed)),
			Details: append(append([]string(nil), failed...), rePushHint),
		}
	}
	if len(missing) > 0 {
		return DoctorCheck{
			ID:      CheckAgeID,
			Status:  CheckWarn,
			Message: fmt.Sprintf("%d 个 vault 文件缺失（尚未 secret push？）", len(missing)),
			Details: missing,
		}
	}
	return DoctorCheck{ID: CheckAgeID, Status: CheckOK, Message: fmt.Sprintf("%d 个密钥均可解密", len(names))}
}

func appendPairedAgeDisplay(paths core.HomerPaths, check DoctorCheck) DoctorCheck {
	devices := core.PairedDisplay(core.LoadState(paths))
	if len(devices) == 0 {
		return check
	}
	check.Message += fmt.Sprintf("；已配对 %d 台设备", len(devices))
	details := append([]string(nil), check.Details...)
	limit := len(devices)
	if limit > 5 {
		limit = 5
	}
	details = append(details, devices[:limit]...)
	if len(devices) > limit {
		details = append(details, fmt.Sprintf("… 其余 %d 台", len(devices)-limit))
	}
	check.Details = details
	return check
}

func secretRelativePath(name string) string {
	return filepath.ToSlash(filepath.Join("secrets", name+".age"))
}

func safeCheckError(err error, privateKey string) string {
	if err == nil {
		return "未知错误"
	}
	message := err.Error()
	if privateKey != "" {
		message = strings.ReplaceAll(message, privateKey, "[redacted]")
	}
	if strings.TrimSpace(message) == "" {
		return "解密失败"
	}
	return message
}

// CheckMachine compares the acceleration state with HEAD. Missing or stale
// state is a warning because the sync layer has safe fallbacks.
func CheckMachine(paths core.HomerPaths) DoctorCheck {
	state := core.LoadState(paths)
	head := gitx.HeadCommit(paths.Home)
	if state.LastSyncCommit == "" {
		return DoctorCheck{
			ID:      CheckMachineID,
			Status:  CheckWarn,
			Message: "state.json 未记录同步状态（lastSyncCommit 缺失）",
			Details: []string{fmt.Sprintf("state 文件: %s", paths.StateFile), "首次同步（`homer push` / `homer pull`）会自动写入；仅影响三方判定的基准。"},
		}
	}
	if head == "" {
		return DoctorCheck{
			ID:      CheckMachineID,
			Status:  CheckWarn,
			Message: "无法解析 HEAD（仓库没有 commit？），state 与 HEAD 的关系不可判定",
			Details: []string{fmt.Sprintf("state.lastSyncCommit: %s", shortCommit(state.LastSyncCommit))},
		}
	}
	if state.LastSyncCommit != head {
		return DoctorCheck{
			ID:      CheckMachineID,
			Status:  CheckWarn,
			Message: "本地有未同步 commit（state.lastSyncCommit 落后于 HEAD）",
			Details: []string{fmt.Sprintf("state: %s", shortCommit(state.LastSyncCommit)), fmt.Sprintf("HEAD:  %s", shortCommit(head))},
		}
	}
	return DoctorCheck{ID: CheckMachineID, Status: CheckOK, Message: fmt.Sprintf("state 与 HEAD 一致（%s）", shortCommit(head))}
}

// CheckRequiredPlaceholders scans only top-level JSON keys in store entries.
// Malformed JSON and non-JSON entries are outside this check's scope.
func CheckRequiredPlaceholders(paths core.HomerPaths, config core.HomerConfig) DoctorCheck {
	snapshots, err := core.ReadSnapshotFromStore(paths, config)
	if err != nil {
		return DoctorCheck{
			ID:      CheckRequiredID,
			Status:  CheckWarn,
			Message: fmt.Sprintf("store 快照不可读，跳过占位符检查: %v", err),
		}
	}

	details := make([]string, 0)
	for _, snapshot := range snapshots {
		for _, category := range snapshot.Categories {
			pathsInCategory := make([]string, 0, len(category.Files))
			for relPath := range category.Files {
				pathsInCategory = append(pathsInCategory, relPath)
			}
			sort.Strings(pathsInCategory)
			for _, relPath := range pathsInCategory {
				entry := category.Files[relPath]
				if entry.Kind != "json" {
					continue
				}
				value, parseErr := orderedjson.Parse([]byte(entry.Content))
				if parseErr != nil {
					continue
				}
				object, ok := value.(*orderedjson.Object)
				if !ok || object == nil {
					continue
				}
				keys := make([]string, 0)
				for key, item := range object.M {
					if text, isString := item.(string); isString && text == requiredPlaceholder {
						keys = append(keys, key)
					}
				}
				sort.Strings(keys)
				if len(keys) > 0 {
					details = append(details, fmt.Sprintf("%s/%s/%s: %s", snapshot.AdapterID, category.Category, relPath, strings.Join(keys, ", ")))
				}
			}
		}
	}
	if len(details) == 0 {
		return DoctorCheck{ID: CheckRequiredID, Status: CheckOK, Message: "未发现 __REQUIRED__ 占位符残留"}
	}
	return DoctorCheck{
		ID:      CheckRequiredID,
		Status:  CheckWarn,
		Message: fmt.Sprintf("%d 个文件残留 __REQUIRED__ 占位符（本机需补全这些必填项）", len(details)),
		Details: details,
	}
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func shortCommit(commit string) string {
	if len(commit) <= 7 {
		return commit
	}
	return commit[:7]
}

// Lower-case aliases keep package-local migration tests concise while all
// public callers use the Go-exported names above.
func checkConfig(paths core.HomerPaths) DoctorCheck         { return CheckConfig(paths) }
func checkRepoAndStore(paths core.HomerPaths) []DoctorCheck { return CheckRepoAndStore(paths) }
func checkRemote(paths core.HomerPaths, options RemoteCheckOptions) DoctorCheck {
	return CheckRemote(paths, options)
}
func checkAdapters(config core.HomerConfig) DoctorCheck { return CheckAdapters(config) }
func checkAge(paths core.HomerPaths, config core.HomerConfig, crypto agecrypto.AgeCryptoPort) DoctorCheck {
	return CheckAge(paths, config, crypto)
}
func checkMachine(paths core.HomerPaths) DoctorCheck { return CheckMachine(paths) }
func checkRequiredPlaceholders(paths core.HomerPaths, config core.HomerConfig) DoctorCheck {
	return CheckRequiredPlaceholders(paths, config)
}
