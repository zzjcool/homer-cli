package commands

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzjcool/homer-cli/internal/agecrypto"
	"github.com/zzjcool/homer-cli/internal/backup"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gitx"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

// SecretSubcommand is one of the four frozen secret command names.
type SecretSubcommand string

const (
	SecretKeygen SecretSubcommand = "keygen"
	SecretPush   SecretSubcommand = "push"
	SecretPull   SecretSubcommand = "pull"
	SecretList   SecretSubcommand = "list"
)

var SECRET_SUBCOMMANDS = []SecretSubcommand{SecretKeygen, SecretPush, SecretPull, SecretList}

// ParseSecretSubcommand splits argv after the `secret` command. An empty
// argument is represented by ok=false, while an unknown word is also false;
// the caller can then render SECRET_USAGE without touching the pipeline.
func ParseSecretSubcommand(argv []string) (SecretSubcommand, bool) {
	if len(argv) == 0 {
		return "", false
	}
	for _, command := range SECRET_SUBCOMMANDS {
		if string(command) == argv[0] {
			return command, true
		}
	}
	return "", false
}

// PromptPort is intentionally structural. The parent cli package owns the
// concrete TTY implementation; keeping the small interface here avoids an
// import cycle while allowing its implementation to be injected directly.
type PromptPort interface {
	Confirm(message string, fallback bool) bool
}

// SecretDeps contains the frozen command injection points. Tests can replace
// both the prompt and age implementation; git is deliberately exercised
// against a temporary repository so CommitPaths and fetch/ref semantics stay
// covered by the real git boundary.
type SecretDeps struct {
	UI  PromptPort
	Age agecrypto.AgeCryptoPort
}

const SECRET_USAGE = `用法: homer secret <keygen|push|pull|list> [options]

age 加密的密钥投递：明文从不进 store；密文走 secrets/<name>.age 入库，
本机私钥留在 <home>/keys/age.txt（0600，gitignored）。

子命令:
  keygen   生成本机 age X25519 identity（已存在则拒绝覆盖，exit 1）
  push     读取 secrets.files 的明文 → 用全部 recipients 加密 → 写 vault → commit
  pull     从远端读密文 → 用本机 identity 解密 → 写回目标路径（覆盖前备份）
  list     列出配置的密钥及其 vault 文件状态

选项:
  --home <dir>       homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --json             输出机器可读 JSON
  -h, --help         显示本帮助

    keygen: --home --json
    push  : --home --yes --no-push --json   （非 TTY 必须 --yes）
    pull  : --home --yes --json             （非 TTY 必须 --yes）
    list  : --home --json

退出码: keygen 成功 → 0（已存在 → 1）；push pushed/no-secrets → 0；pull applied/no-secrets → 0；list → 0；其余 → 1。

提示: 私钥永不回显；输出只含 recipient（公钥）。`

// SecretKeygenOptions are the keygen flags after parent-cli parsing.
type SecretKeygenOptions struct {
	HomerHome string
	JSON      bool
}

type SecretKeygenReport struct {
	OK           bool   `json:"ok"`
	IdentityFile string `json:"identityFile"`
	Recipient    string `json:"recipient"`
	Created      bool   `json:"created"`
}

func (report SecretKeygenReport) ExitCode() int {
	if report.OK {
		return 0
	}
	return 1
}

// RunSecretKeygen generates and atomically persists a new identity. The
// private key is returned by agecrypto only to the command's local stack and
// is never copied into the report or an error string.
func RunSecretKeygen(options SecretKeygenOptions) (SecretKeygenReport, error) {
	paths := resolveCommandPaths(options.HomerHome)
	identity, err := agecrypto.KeyGen(paths)
	if err != nil {
		return SecretKeygenReport{}, err
	}
	return SecretKeygenReport{
		OK:           true,
		IdentityFile: agecrypto.IdentityFilePath(paths),
		Recipient:    identity.Recipient,
		Created:      true,
	}, nil
}

// SecretPushOptions are the push flags after parent-cli parsing.
type SecretPushOptions struct {
	HomerHome string
	JSON      bool
	Yes       bool
	NoPush    bool
}

type SecretPushStatus string

const (
	SecretPushStatusPushed       SecretPushStatus = "pushed"
	SecretPushStatusNoSecrets    SecretPushStatus = "no-secrets"
	SecretPushStatusNoIdentity   SecretPushStatus = "no-identity"
	SecretPushStatusNoRecipients SecretPushStatus = "no-recipients"
	SecretPushStatusMissing      SecretPushStatus = "missing-source"
	SecretPushStatusAborted      SecretPushStatus = "aborted"
	SecretPushStatusError        SecretPushStatus = "error"
)

type SecretPushReport struct {
	OK             bool             `json:"ok"`
	Status         SecretPushStatus `json:"status"`
	Encrypted      []string         `json:"encrypted"`
	Commit         string           `json:"commit,omitempty"`
	PushedToRemote bool             `json:"pushedToRemote"`
	Warnings       []string         `json:"warnings"`
	Errors         []string         `json:"errors"`
}

func newPushReport(status SecretPushStatus) SecretPushReport {
	return SecretPushReport{
		OK:             status == SecretPushStatusPushed || status == SecretPushStatusNoSecrets,
		Status:         status,
		Encrypted:      []string{},
		PushedToRemote: false,
		Warnings:       []string{},
		Errors:         []string{},
	}
}

// ExitCode maps a push report to the frozen process status.
func (report SecretPushReport) ExitCode() int {
	if report.OK {
		return 0
	}
	return 1
}

// RunSecretPush implements the all-or-nothing source read, multi-recipient
// encryption, secrets-only commit, and optional remote push pipeline.
func RunSecretPush(options SecretPushOptions, deps *SecretDeps) SecretPushReport {
	paths := resolveCommandPaths(options.HomerHome)
	config, err := requireSecretConfig(paths)
	if err != nil {
		return pushErrorReport(err)
	}

	names := agecrypto.SecretNames(config)
	if len(names) == 0 {
		report := newPushReport(SecretPushStatusNoSecrets)
		report.Warnings = append(report.Warnings, "homer.json 的 secrets.files 为空，没有需要投递的密钥")
		return report
	}

	identity, identityOK := agecrypto.LoadIdentity(paths)
	if !identityOK {
		report := newPushReport(SecretPushStatusNoIdentity)
		report.Errors = []string{
			fmt.Sprintf("未找到本机 age identity: %s", agecrypto.IdentityFilePath(paths)),
			"请先运行 `homer secret keygen` 生成本机私钥，并把输出的 recipient 写进 homer.json 的 secrets.recipients。",
		}
		return report
	}
	_ = identity // Loading is the required existence/validity gate; the key stays private.

	recipients := []string{}
	if config.Secrets != nil {
		recipients = append(recipients, config.Secrets.Recipients...)
	}
	if len(recipients) == 0 {
		report := newPushReport(SecretPushStatusNoRecipients)
		report.Errors = []string{
			"homer.json 的 secrets.recipients 为空：没有加密目标",
			"把各机器的 recipient（`age1...`，由 `homer secret keygen` 输出）写进 secrets.recipients 后重试。",
		}
		return report
	}

	// Read every source before invoking EncryptSecretToFile. This is the
	// missing-source all-or-nothing boundary: a bad second source cannot leave
	// a first plaintext encrypted into secrets/.
	plaintexts := make(map[string][]byte, len(names))
	missing := make([]string, 0)
	for _, name := range names {
		destination, destinationErr := agecrypto.DestinationOf(config, name)
		if destinationErr != nil {
			missing = append(missing, fmt.Sprintf("%s → %s", name, safeError(destinationErr)))
			continue
		}
		plaintext, readErr := os.ReadFile(destination)
		if readErr != nil {
			missing = append(missing, fmt.Sprintf("%s → %s", name, destination))
			continue
		}
		plaintexts[name] = plaintext
	}
	if len(missing) > 0 {
		report := newPushReport(SecretPushStatusMissing)
		report.Errors = append(report.Errors, fmt.Sprintf("以下目标文件不可读（%d/%d），已中止且未写入任何 vault 文件：", len(missing), len(names)))
		report.Errors = append(report.Errors, missing...)
		return report
	}

	crypto := secretCrypto(deps)
	encrypted := make([]string, 0, len(names))
	for _, name := range names {
		if encryptErr := agecrypto.EncryptSecretToFile(crypto, paths, name, plaintexts[name], recipients); encryptErr != nil {
			report := newPushReport(SecretPushStatusError)
			report.Encrypted = append(report.Encrypted, encrypted...)
			report.Errors = errorLines(encryptErr)
			return report
		}
		encrypted = append(encrypted, name)
	}

	warnings := make([]string, 0)
	if !options.Yes {
		prompt := depsPrompt(deps)
		approved := prompt.Confirm(secretPreview(config, encrypted, "将加密并提交"), false)
		if !approved {
			if deps == nil || deps.UI == nil {
				warnings = append(warnings, "非交互环境，请加 --yes")
			}
			report := newPushReport(SecretPushStatusAborted)
			report.Encrypted = encrypted
			report.Warnings = warnings
			report.Errors = []string{"已取消：未确认投递（vault 未 commit，远端未推送）"}
			return report
		}
	}

	if ensureErr := gitx.EnsureGitRepo(paths.Home); ensureErr != nil {
		report := newPushReport(SecretPushStatusError)
		report.Encrypted = encrypted
		report.Warnings = warnings
		report.Errors = errorLines(ensureErr)
		return report
	}
	if !gitx.IsGitRepo(paths.Home) {
		report := newPushReport(SecretPushStatusError)
		report.Encrypted = encrypted
		report.Warnings = warnings
		report.Errors = []string{
			fmt.Sprintf("vault 已写入但 %s 不是 git 仓库（git 不可用？），无法提交", paths.Home),
			"请确认 git 已安装并可用，然后重试。",
		}
		return report
	}

	commit := gitx.CommitPaths(paths.Home, []string{"secrets/"}, fmt.Sprintf("homer secret push: 更新 %d 个密钥", len(encrypted)))
	if commit == "" && secretsDirty(paths.Home) {
		report := newPushReport(SecretPushStatusError)
		report.Encrypted = encrypted
		report.Warnings = warnings
		report.Errors = []string{
			fmt.Sprintf("vault 已写入但 git commit 失败: %s", paths.Home),
			"请检查 git 是否可用与 user.name / user.email 配置，然后重试。",
		}
		return report
	}

	if commit == "" {
		warnings = append(warnings, "vault 内容与 HEAD 一致，未产生新 commit")
	}
	report := newPushReport(SecretPushStatusPushed)
	report.Encrypted = encrypted
	report.Commit = commit
	report.Warnings = warnings
	if options.NoPush {
		report.Warnings = append(report.Warnings, "--no-push: 只做本地 commit，未推送远端")
		if gitx.HasRemote(paths.Home) {
			report.Warnings = append(report.Warnings, "如需推送请运行 `"+gitx.PushHint(paths.Home)+"`")
		}
		return report
	}
	if !gitx.HasPushTarget(paths.Home) {
		report.Warnings = append(report.Warnings, "未配置 git remote，仅本地 commit（local-only 模式；如需推送请先运行 `homer remote <url>`）")
		return report
	}

	pushed := gitx.Push(paths.Home)
	if !pushed.OK {
		reason := strings.TrimSpace(pushed.Stderr)
		if reason == "" {
			reason = "未知错误"
		}
		report := newPushReport(SecretPushStatusError)
		report.Encrypted = encrypted
		report.Commit = commit
		report.Warnings = append(warnings, "远端推送失败；请运行 `"+gitx.PushHint(paths.Home)+"`："+reason)
		report.Errors = []string{
			fmt.Sprintf("本地 commit 已成功%s，远端推送失败: %s", commitSuffix(commit), reason),
			"密钥已在本地提交，修复远端问题后重试 `homer secret push`。",
		}
		return report
	}
	report.PushedToRemote = true
	return report
}

// SecretPullOptions are the pull flags after parent-cli parsing.
type SecretPullOptions struct {
	HomerHome string
	JSON      bool
	Yes       bool
}

type SecretPullStatus string

const (
	SecretPullStatusApplied       SecretPullStatus = "applied"
	SecretPullStatusNoSecrets     SecretPullStatus = "no-secrets"
	SecretPullStatusNoIdentity    SecretPullStatus = "no-identity"
	SecretPullStatusMissingVault  SecretPullStatus = "missing-vault"
	SecretPullStatusUndecryptable SecretPullStatus = "undecryptable"
	SecretPullStatusAborted       SecretPullStatus = "aborted"
	SecretPullStatusError         SecretPullStatus = "error"
)

type SecretPullReport struct {
	OK        bool             `json:"ok"`
	Status    SecretPullStatus `json:"status"`
	Pulled    []string         `json:"pulled"`
	BackupDir string           `json:"backupDir,omitempty"`
	Warnings  []string         `json:"warnings"`
	Errors    []string         `json:"errors"`
}

func newPullReport(status SecretPullStatus) SecretPullReport {
	return SecretPullReport{
		OK:       status == SecretPullStatusApplied || status == SecretPullStatusNoSecrets,
		Status:   status,
		Pulled:   []string{},
		Warnings: []string{},
		Errors:   []string{},
	}
}

// ExitCode maps a pull report to the frozen process status.
func (report SecretPullReport) ExitCode() int {
	if report.OK {
		return 0
	}
	return 1
}

// RunSecretPull fetches/refers to an upstream vault when available, falls back
// symmetrically to the workspace, decrypts all files before writing anything,
// and only then backs up/writes destination files.
func RunSecretPull(options SecretPullOptions, deps *SecretDeps) SecretPullReport {
	paths := resolveCommandPaths(options.HomerHome)
	config, err := requireSecretConfig(paths)
	if err != nil {
		return pullErrorReport(err)
	}

	names := agecrypto.SecretNames(config)
	if len(names) == 0 {
		report := newPullReport(SecretPullStatusNoSecrets)
		report.Warnings = append(report.Warnings, "homer.json 的 secrets.files 为空，没有需要归位的密钥")
		return report
	}

	identity, identityOK := agecrypto.LoadIdentity(paths)
	if !identityOK {
		report := newPullReport(SecretPullStatusNoIdentity)
		report.Errors = []string{
			fmt.Sprintf("未找到本机 age identity: %s", agecrypto.IdentityFilePath(paths)),
			"先在本机运行 `homer secret keygen`；若要从旧机迁移：在旧机把本机 recipient 加入 homer.json 的 secrets.recipients 后重新 `homer secret push`，再回到本机 `homer secret pull`。",
		}
		return report
	}

	fetched := gitx.Fetch(paths.Home)
	configured := configuredUpstream(paths.Home)
	upstream := gitx.UpstreamRef(paths.Home)
	upstreamName := upstream
	if upstreamName == "" {
		upstreamName = configured
	}
	upstreamReadable := upstream != "" && readableRef(paths.Home, upstream)
	warnings := make([]string, 0)
	unverifiableRollback := false

	if !fetched.OK {
		reason := strings.TrimSpace(fetched.Stderr)
		if reason == "" {
			reason = "未知错误"
		}
		switch {
		case upstreamName == "":
			warnings = append(warnings, fmt.Sprintf("git fetch 失败（%s）：当前分支无 upstream，回落读取工作区 vault", reason))
		case upstreamReadable:
			warnings = append(warnings, fmt.Sprintf("git fetch 失败（%s）：回落读取工作区 vault（并与本地 %s 比对）", reason, upstream))
		default:
			unverifiableRollback = true
			warnings = append(warnings, fmt.Sprintf("git fetch 失败（%s）且本地无 %s 可读引用，无法确认工作区密文是否为最新：将回滚到本地旧版密文", reason, upstreamName))
		}
	} else if upstream == "" {
		warnings = append(warnings, "当前分支无 upstream：读取工作区 vault")
	}

	ciphertexts := make(map[string][]byte, len(names))
	missing := make([]string, 0)
	diverged := make([]string, 0)
	for _, name := range names {
		rel := secretRelativePath(name)
		var upstreamBytes []byte
		if upstreamReadable {
			if bytes, readErr := gitx.ReadVaultFileAtCommit(paths.Home, rel, upstream); readErr == nil {
				upstreamBytes = bytes
			}
		}
		workspaceBytes := readWorkspaceVault(paths, name)
		if !fetched.OK && upstreamBytes != nil && workspaceBytes != nil && !bytesEqual(upstreamBytes, workspaceBytes) {
			diverged = append(diverged, fmt.Sprintf("%s（工作区 ≠ %s）", rel, upstream))
		}

		value := upstreamBytes
		if value == nil {
			value = workspaceBytes
		}
		if value == nil {
			if upstreamReadable {
				missing = append(missing, fmt.Sprintf("%s（工作区与 %s）", rel, upstream))
			} else {
				missing = append(missing, fmt.Sprintf("%s（工作区）", rel))
			}
			continue
		}
		ciphertexts[name] = value
	}

	if len(diverged) > 0 {
		report := newPullReport(SecretPullStatusError)
		report.Warnings = warnings
		report.Errors = append([]string{fmt.Sprintf("git fetch 失败且工作区密文与本地 %s 不一致（%d/%d）：拒绝写旧值", upstream, len(diverged), len(names))}, diverged...)
		report.Errors = append(report.Errors, "无法确认远端最新密文（工作区可能是旧版本）：请恢复网络后重跑 `homer secret pull`；确需使用本地版本请先 `homer secret push`（或用 `git pull` 手工对齐）。")
		return report
	}
	if len(missing) > 0 {
		report := newPullReport(SecretPullStatusMissingVault)
		report.Warnings = warnings
		report.Errors = append([]string{fmt.Sprintf("以下 vault 文件缺失（%d/%d），已中止且未写入任何目标文件：", len(missing), len(names))}, missing...)
		report.Errors = append(report.Errors, "确认旧机已 `homer secret push`（密文进 git）后重试 `homer secret pull`。")
		return report
	}

	crypto := secretCrypto(deps)
	plaintexts := make(map[string][]byte, len(names))
	failures := make([]string, 0)
	for _, name := range names {
		plaintext, decryptErr := crypto.Decrypt(ciphertexts[name], identity)
		if decryptErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", name, safeErrorWithPrivate(decryptErr, identity.SecretKey)))
			continue
		}
		plaintexts[name] = plaintext
	}
	if len(failures) > 0 {
		report := newPullReport(SecretPullStatusUndecryptable)
		report.Warnings = warnings
		report.Errors = append([]string{fmt.Sprintf("以下密钥无法用本机 identity 解密（%d/%d），已中止且未写入任何目标文件：", len(failures), len(names))}, failures...)
		report.Errors = append(report.Errors, "本机 recipient 可能不在旧机的 secrets.recipients 里：请在旧机追加本机 recipient 后重新 `homer secret push`。")
		return report
	}

	if unverifiableRollback && options.Yes {
		report := newPullReport(SecretPullStatusError)
		report.Warnings = warnings
		report.Errors = []string{
			"git fetch 失败且无法比对本地与远端密文（无可用 remote-tracking 引用）：`--yes` 下拒绝回滚到旧版密文",
			"请恢复网络后重跑 `homer secret pull`；若确认本地就是最新版，请先 `homer secret push` 把本地密文推到远端。",
		}
		return report
	}

	if !options.Yes {
		prompt := depsPrompt(deps)
		preview := secretPreview(config, names, "将归位")
		if unverifiableRollback {
			preview += "\n⚠ 警告：fetch 失败且无法比对远端，本次将回滚到本地旧版密文（旧值可能已废弃）。"
		}
		if !prompt.Confirm(preview, false) {
			if deps == nil || deps.UI == nil {
				warnings = append(warnings, "非交互环境，请加 --yes")
			}
			report := newPullReport(SecretPullStatusAborted)
			report.Warnings = warnings
			report.Errors = []string{"已取消：未确认归位（未写入任何目标文件）"}
			return report
		}
	}

	backupDir, backupErr := backupSecretDestinations(paths, config, names)
	if backupErr != nil {
		report := newPullReport(SecretPullStatusError)
		report.Warnings = warnings
		report.Errors = errorLines(backupErr)
		return report
	}
	for _, name := range names {
		destination, destinationErr := agecrypto.DestinationOf(config, name)
		if destinationErr != nil {
			report := newPullReport(SecretPullStatusError)
			report.Warnings = warnings
			report.Errors = errorLines(destinationErr)
			return report
		}
		if writeErr := writeSecretDestination(destination, plaintexts[name]); writeErr != nil {
			report := newPullReport(SecretPullStatusError)
			report.Warnings = warnings
			report.Errors = errorLines(writeErr)
			return report
		}
	}

	report := newPullReport(SecretPullStatusApplied)
	report.Pulled = append(report.Pulled, names...)
	report.BackupDir = backupDir
	report.Warnings = append(warnings, secretPullWorkspaceHint(paths.Home))
	return report
}

// SecretListOptions are the list flags after parent-cli parsing.
type SecretListOptions struct {
	HomerHome string
	JSON      bool
}

type SecretListReport struct {
	Secrets []agecrypto.VaultEntryStatus `json:"secrets"`
}

func (SecretListReport) ExitCode() int { return 0 }

// VaultEntryStatus is re-exported from the agecrypto vault boundary so the
// command report has one authoritative inventory type.
type VaultEntryStatus = agecrypto.VaultEntryStatus

// RunSecretList is pure inventory: it does not require identity or decrypt a
// vault and never creates secrets/.
func RunSecretList(options SecretListOptions) (SecretListReport, error) {
	paths := resolveCommandPaths(options.HomerHome)
	config, err := requireSecretConfig(paths)
	if err != nil {
		return SecretListReport{}, err
	}
	return SecretListReport{Secrets: agecrypto.ListSecrets(paths, config)}, nil
}

func requireSecretConfig(paths core.HomerPaths) (*core.HomerConfig, error) {
	config, err := core.LoadConfig(paths)
	if err != nil {
		return nil, core.NewCliError(fmt.Sprintf("未找到 homer 配置: %s\n请先运行 `homer init` 生成 homer.json 与 store 快照。", paths.ConfigFile))
	}
	return config, nil
}

func secretCrypto(deps *SecretDeps) agecrypto.AgeCryptoPort {
	if deps != nil && deps.Age != nil {
		return deps.Age
	}
	return agecrypto.NewAgeCryptoPort()
}

type fallbackPrompt struct{}

func (fallbackPrompt) Confirm(_ string, fallback bool) bool { return fallback }

func depsPrompt(deps *SecretDeps) PromptPort {
	if deps != nil && deps.UI != nil {
		return deps.UI
	}
	return fallbackPrompt{}
}

func secretPreview(config *core.HomerConfig, names []string, verb string) string {
	lines := make([]string, 0, len(names)+1)
	for _, name := range names {
		destination, err := agecrypto.DestinationOf(config, name)
		if err != nil {
			destination = "[invalid destination]"
		}
		lines = append(lines, fmt.Sprintf("  %s → %s", name, destination))
	}
	return fmt.Sprintf("%s %d 个密钥：\n%s\n继续？", verb, len(names), strings.Join(lines, "\n"))
}

func secretsDirty(home string) bool {
	result := gitx.Exec(home, []string{"status", "--porcelain", "--", "secrets/"}, 0)
	return result.OK && strings.TrimSpace(result.Stdout) != ""
}

func configuredUpstream(home string) string {
	branchResult := gitx.Exec(home, []string{"symbolic-ref", "--quiet", "--short", "HEAD"}, 0)
	if !branchResult.OK {
		return gitx.UpstreamRef(home)
	}
	branch := strings.TrimSpace(branchResult.Stdout)
	remoteResult := gitx.Exec(home, []string{"config", "--get", "branch." + branch + ".remote"}, 0)
	if !remoteResult.OK {
		return gitx.UpstreamRef(home)
	}
	remote := strings.TrimSpace(remoteResult.Stdout)
	if remote == "" {
		return gitx.UpstreamRef(home)
	}
	mergeResult := gitx.Exec(home, []string{"config", "--get", "branch." + branch + ".merge"}, 0)
	if mergeResult.OK {
		merge := strings.TrimSpace(mergeResult.Stdout)
		merge = strings.TrimPrefix(merge, "refs/heads/")
		if merge != "" {
			return remote + "/" + merge
		}
	}
	return gitx.UpstreamRef(home)
}

func readableRef(home, ref string) bool {
	if ref == "" {
		return false
	}
	return gitx.Exec(home, []string{"rev-parse", "--verify", ref + "^{commit}"}, 0).OK
}

func readWorkspaceVault(paths core.HomerPaths, name string) []byte {
	file, err := agecrypto.SecretFilePath(paths, name)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	return data
}

func backupSecretDestinations(paths core.HomerPaths, config *core.HomerConfig, names []string) (string, error) {
	targets := make([]backup.BackupTarget, 0, len(names))
	for _, name := range names {
		destination, err := agecrypto.DestinationOf(config, name)
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
	result, err := backup.BackupFiles(paths, "secret", targets, backup.BackupFilesOptions{
		Mode: backup.BackupFileModes{Dir: 0o700, File: 0o600},
	})
	if err != nil {
		return "", err
	}
	return result.BackupDir, nil
}

func writeSecretDestination(destination string, plaintext []byte) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o777); err != nil {
		return err
	}
	if err := os.WriteFile(destination, plaintext, 0o600); err != nil {
		return err
	}
	return os.Chmod(destination, 0o600)
}

func secretPullWorkspaceHint(home string) string {
	return fmt.Sprintf("提示: vault 已更新。若随后要 homer push，请先同步工作区：git -C %s fetch origin && git -C %s merge --ff-only origin/master", home, home)
}

func secretRelativePath(name string) string {
	return filepath.ToSlash(filepath.Join("secrets", name+".age"))
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func pushErrorReport(err error) SecretPushReport {
	report := newPushReport(SecretPushStatusError)
	report.Errors = errorLines(err)
	return report
}

func pullErrorReport(err error) SecretPullReport {
	report := newPullReport(SecretPullStatusError)
	report.Errors = errorLines(err)
	return report
}

func errorLines(err error) []string {
	if err == nil {
		return []string{}
	}
	message := safeError(err)
	lines := make([]string, 0)
	for _, line := range strings.Split(message, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return []string{"homer command failed"}
	}
	return lines
}

func safeError(err error) string {
	return safeErrorWithPrivate(err, "")
}

func safeErrorWithPrivate(err error, privateKey string) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if privateKey != "" {
		message = strings.ReplaceAll(message, privateKey, "[redacted]")
	}
	return message
}

func commitSuffix(commit string) string {
	if commit == "" {
		return ""
	}
	short := commit
	if len(short) > 7 {
		short = short[:7]
	}
	return "（" + short + "）"
}

// RenderSecretKeygenReport intentionally has no field that could contain a
// private key. The recipient is the only key material shown to a user.
func RenderSecretKeygenReport(report SecretKeygenReport) string {
	lines := []string{
		fmt.Sprintf("homer secret keygen: %s", ternary(report.Created, "已生成", "已存在")),
		"  identity 文件: " + report.IdentityFile,
		"  recipient: " + report.Recipient,
		"  提示: 把上面这行 recipient 写入 homer.json 的 secrets.recipients（在旧机上），随后 `homer secret push`。",
	}
	return strings.Join(lines, "\n")
}

func RenderSecretPushReport(report SecretPushReport) string {
	lines := []string{fmt.Sprintf("homer secret push: %s", report.Status), fmt.Sprintf("  已加密: %d 个密钥", len(report.Encrypted))}
	for _, name := range report.Encrypted {
		lines = append(lines, "    "+name)
	}
	if report.Commit != "" {
		lines = append(lines, "  本地提交: "+report.Commit)
	}
	lines = append(lines, fmt.Sprintf("  远端推送: %s", ternary(report.PushedToRemote, "已推送", "未推送")))
	for _, warning := range report.Warnings {
		lines = append(lines, "  ⚠ "+warning)
	}
	for _, err := range report.Errors {
		lines = append(lines, "  ✗ "+err)
	}
	return strings.Join(lines, "\n")
}

func RenderSecretPullReport(report SecretPullReport) string {
	lines := []string{fmt.Sprintf("homer secret pull: %s", report.Status), fmt.Sprintf("  已归位: %d 个密钥", len(report.Pulled))}
	for _, name := range report.Pulled {
		lines = append(lines, "    "+name)
	}
	if report.BackupDir != "" {
		lines = append(lines, "  备份目录: "+report.BackupDir)
	}
	for _, warning := range report.Warnings {
		lines = append(lines, "  ⚠ "+warning)
	}
	for _, err := range report.Errors {
		lines = append(lines, "  ✗ "+err)
	}
	return strings.Join(lines, "\n")
}

func RenderSecretListReport(report SecretListReport) string {
	if len(report.Secrets) == 0 {
		return "homer secret list: 未配置密钥（homer.json 的 secrets.files 为空）"
	}
	lines := []string{"homer secret list:"}
	for _, entry := range report.Secrets {
		lines = append(lines, fmt.Sprintf("  %s  →  %s  [vault %s]", entry.Name, entry.Destination, entry.VaultFile))
	}
	return strings.Join(lines, "\n")
}

// SecretCommandOptions is the common option set used by ExecuteSecret. It is
// intentionally separate from the per-subcommand option structs so W8 can
// dispatch one parsed flag object without touching this command's pipeline.
type SecretCommandOptions struct {
	HomerHome string
	JSON      bool
	Yes       bool
	NoPush    bool
}

// ExecuteSecret renders one subcommand and maps its report status to the
// frozen process exit code. It is the independent entry point that W8's
// run.go dispatcher can call without changing this worker's files.
func ExecuteSecret(subcommand string, options SecretCommandOptions, deps *SecretDeps, out, errOut io.Writer) int {
	writeError := func(err error) {
		if errOut == nil {
			errOut = out
		}
		for _, line := range errorLines(err) {
			_, _ = fmt.Fprintln(errOut, line)
		}
	}
	switch SecretSubcommand(subcommand) {
	case SecretKeygen:
		report, err := RunSecretKeygen(SecretKeygenOptions{HomerHome: options.HomerHome, JSON: options.JSON})
		if err != nil {
			writeError(err)
			return 1
		}
		if options.JSON {
			_ = writeOrderedJSON(out, secretKeygenValue(report))
		} else {
			_, _ = fmt.Fprintln(out, RenderSecretKeygenReport(report))
		}
		return 0
	case SecretPush:
		report := RunSecretPush(SecretPushOptions{HomerHome: options.HomerHome, JSON: options.JSON, Yes: options.Yes, NoPush: options.NoPush}, deps)
		if options.JSON {
			_ = writeOrderedJSON(out, secretPushValue(report))
		} else {
			_, _ = fmt.Fprintln(out, RenderSecretPushReport(report))
		}
		return report.ExitCode()
	case SecretPull:
		report := RunSecretPull(SecretPullOptions{HomerHome: options.HomerHome, JSON: options.JSON, Yes: options.Yes}, deps)
		if options.JSON {
			_ = writeOrderedJSON(out, secretPullValue(report))
		} else {
			_, _ = fmt.Fprintln(out, RenderSecretPullReport(report))
		}
		return report.ExitCode()
	case SecretList:
		report, err := RunSecretList(SecretListOptions{HomerHome: options.HomerHome, JSON: options.JSON})
		if err != nil {
			writeError(err)
			return 1
		}
		if options.JSON {
			_ = writeOrderedJSON(out, secretListValue(report))
		} else {
			_, _ = fmt.Fprintln(out, RenderSecretListReport(report))
		}
		return 0
	default:
		writeError(core.NewCliError(fmt.Sprintf("未知 secret 子命令: %s", subcommand)))
		return 1
	}
}

func RunSecretCommand(subcommand string, options SecretCommandOptions, deps *SecretDeps, out, errOut io.Writer) int {
	return ExecuteSecret(subcommand, options, deps, out, errOut)
}

func runSecretKeygen(options SecretKeygenOptions) (SecretKeygenReport, error) {
	return RunSecretKeygen(options)
}

func runSecretPush(options SecretPushOptions, deps *SecretDeps) SecretPushReport {
	return RunSecretPush(options, deps)
}

func runSecretPull(options SecretPullOptions, deps *SecretDeps) SecretPullReport {
	return RunSecretPull(options, deps)
}

func runSecretList(options SecretListOptions) (SecretListReport, error) {
	return RunSecretList(options)
}

func renderSecretKeygenReport(report SecretKeygenReport) string {
	return RenderSecretKeygenReport(report)
}
func renderSecretPushReport(report SecretPushReport) string { return RenderSecretPushReport(report) }
func renderSecretPullReport(report SecretPullReport) string { return RenderSecretPullReport(report) }
func renderSecretListReport(report SecretListReport) string { return RenderSecretListReport(report) }

func secretKeygenValue(report SecretKeygenReport) orderedjson.Value {
	return &orderedjson.Object{
		Keys: []string{"ok", "identityFile", "recipient", "created"},
		M: map[string]orderedjson.Value{
			"ok":           report.OK,
			"identityFile": report.IdentityFile,
			"recipient":    report.Recipient,
			"created":      report.Created,
		},
	}
}

func secretPushValue(report SecretPushReport) orderedjson.Value {
	keys := []string{"ok", "status", "encrypted"}
	values := map[string]orderedjson.Value{
		"ok":             report.OK,
		"status":         string(report.Status),
		"encrypted":      orderedStringArray(report.Encrypted),
		"pushedToRemote": report.PushedToRemote,
		"warnings":       orderedStringArray(report.Warnings),
		"errors":         orderedStringArray(report.Errors),
	}
	if report.Commit != "" {
		keys = append(keys, "commit")
		values["commit"] = report.Commit
	}
	keys = append(keys, "pushedToRemote", "warnings", "errors")
	return &orderedjson.Object{Keys: keys, M: values}
}

func secretPullValue(report SecretPullReport) orderedjson.Value {
	keys := []string{"ok", "status", "pulled"}
	values := map[string]orderedjson.Value{
		"ok":       report.OK,
		"status":   string(report.Status),
		"pulled":   orderedStringArray(report.Pulled),
		"warnings": orderedStringArray(report.Warnings),
		"errors":   orderedStringArray(report.Errors),
	}
	if report.BackupDir != "" {
		keys = append(keys, "backupDir")
		values["backupDir"] = report.BackupDir
	}
	keys = append(keys, "warnings", "errors")
	return &orderedjson.Object{Keys: keys, M: values}
}

func secretListValue(report SecretListReport) orderedjson.Value {
	entries := make([]orderedjson.Value, 0, len(report.Secrets))
	for _, entry := range report.Secrets {
		entries = append(entries, &orderedjson.Object{
			Keys: []string{"name", "destination", "vaultFile"},
			M: map[string]orderedjson.Value{
				"name":        entry.Name,
				"destination": entry.Destination,
				"vaultFile":   entry.VaultFile,
			},
		})
	}
	return &orderedjson.Object{
		Keys: []string{"secrets"},
		M:    map[string]orderedjson.Value{"secrets": entries},
	}
}
