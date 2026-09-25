package commands

import (
	"fmt"
	"io"
	"strings"

	"github.com/zzjcool/homer-cli/internal/agecrypto"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
	"github.com/zzjcool/homer-cli/internal/pair"
)

// PairOptions controls the serve (empty Addr) or join (non-empty Addr) side
// of the pairing command. Protocol orchestration is added in P1; these fields
// are frozen now so the CLI seam does not change between waves.
type PairOptions struct {
	HomerHome string
	Addr      string
	Yes       bool
	JSON      bool
}

// PairDeps contains the command's future protocol injection points. A custom
// transport is also the explicit way for tests to bypass the optional tailcat
// binary during the scaffold phase.
type PairDeps struct {
	UI        PromptPort
	Transport pair.PairTransport
	Age       agecrypto.AgeCryptoPort
	OnAddr    func(addr string)
}

type PairPeerSummary struct {
	Hostname  string `json:"hostname"`
	Recipient string `json:"recipient"`
}

type PairServeStatus string

const (
	PairServeStatusNoTailcat  PairServeStatus = "no-tailcat"
	PairServeStatusNoConfig   PairServeStatus = "no-config"
	PairServeStatusNoSource   PairServeStatus = "missing-source"
	PairServeStatusAborted    PairServeStatus = "aborted"
	PairServeStatusPeerFailed PairServeStatus = "peer-failed"
	PairServeStatusPaired     PairServeStatus = "paired"
	PairServeStatusError      PairServeStatus = "error"
)

type PairServeReport struct {
	OK             bool             `json:"ok"`
	Status         PairServeStatus  `json:"status"`
	Addr           string           `json:"addr,omitempty"`
	Peer           *PairPeerSummary `json:"peer,omitempty"`
	Sent           []string         `json:"sent"`
	RecipientAdded bool             `json:"recipientAdded"`
	PairedCount    int              `json:"pairedCount"`
	Warnings       []string         `json:"warnings"`
	Errors         []string         `json:"errors"`
}

func (report PairServeReport) ExitCode() int {
	if report.OK {
		return 0
	}
	return 1
}

type PairJoinStatus string

const (
	PairJoinStatusNoTailcat     PairJoinStatus = "no-tailcat"
	PairJoinStatusNoConfig      PairJoinStatus = "no-config"
	PairJoinStatusNoIdentity    PairJoinStatus = "no-identity"
	PairJoinStatusDeclined      PairJoinStatus = "declined"
	PairJoinStatusUnknownSecret PairJoinStatus = "unknown-secret"
	PairJoinStatusUndecryptable PairJoinStatus = "undecryptable"
	PairJoinStatusPaired        PairJoinStatus = "paired"
	PairJoinStatusError         PairJoinStatus = "error"
)

type PairJoinReport struct {
	OK        bool             `json:"ok"`
	Status    PairJoinStatus   `json:"status"`
	Peer      *PairPeerSummary `json:"peer,omitempty"`
	Applied   []string         `json:"applied"`
	BackupDir string           `json:"backupDir,omitempty"`
	Warnings  []string         `json:"warnings"`
	Errors    []string         `json:"errors"`
}

func (report PairJoinReport) ExitCode() int {
	if report.OK {
		return 0
	}
	return 1
}

const PAIR_USAGE = `用法: homer pair [options]           （机器 A：serve，输出一次性地址并等待）
      homer pair <tc-addr> [options]  （机器 B：join，完成配对后退出）

两台机器在线时的密钥快车道：tailcat 建立 WireGuard 加密管道（可选外部
依赖），传输内容全部为 age 密文——即使地址泄露，拿到的只是密文。

流程:
  1. 机器 A 运行 homer pair，把输出的一次性地址通过安全信道（当面/
     私密消息）交给机器 B；切勿粘贴到 git、issue 或群聊。
  2. 机器 B 运行 homer pair <tc-addr>；A 端确认对方 hostname 与
     recipient 指纹后放行（非 TTY 需 --yes）。
  3. A 将全部密钥以 age 多 recipient 密文（A 现有 recipients + B）发送；
     B 解密验证后写入 secrets.files 登记的目标路径（0600，覆盖前备份），
     并同步写入自己的 secrets/ vault。

前置: 两端已完成 homer init / homer home；B 端已 homer secret keygen；
     两端 PATH 中有 tailcat（官方 INSTALL:
     https://github.com/tailscale/tailcat，建议 v0.7.0+）。

选项:
  --home <dir>       homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --yes              serve 端跳过对端确认（非 TTY 必须）；join 端无操作
  --json             输出机器可读 JSON
  -h, --help         显示本帮助

退出码: 配对成功 → 0；tailcat 缺失 / 用法错误 / 对端拒绝或传输失败 → 1。

安全: 地址即凭证（bearer secret）且进程结束即失效；无 tailcat 时请继续
使用 homer secret push / homer secret pull（age 密文走 git）。`

// RunPairServe is a P0 scaffold. It performs only dependency/configuration
// gates and deliberately does not start a partial pairing protocol.
func RunPairServe(options PairOptions, deps *PairDeps) PairServeReport {
	if !pairTailcatAvailable(deps) {
		return noTailcatServeReport()
	}
	if err := pairConfigReady(options.HomerHome); err != nil {
		return noConfigServeReport(err)
	}
	report := newPairServeReport(PairServeStatusError)
	report.Errors = append(report.Errors, "pair 协议尚未实现")
	return report
}

// RunPairJoin is the join-side counterpart of the P0 scaffold.
func RunPairJoin(options PairOptions, deps *PairDeps) PairJoinReport {
	if !pairTailcatAvailable(deps) {
		return noTailcatJoinReport()
	}
	if err := pairConfigReady(options.HomerHome); err != nil {
		return noConfigJoinReport(err)
	}
	report := newPairJoinReport(PairJoinStatusError)
	report.Errors = append(report.Errors, "pair 协议尚未实现")
	return report
}

// ExecutePair dispatches to the scaffold's serve/join preflight and renders a
// stable human or ordered-JSON skeleton. Full signal/transport orchestration
// is intentionally reserved for the frozen P1/P2 waves.
func ExecutePair(options PairOptions, deps *PairDeps, out, errOut io.Writer) int {
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}

	if options.Addr == "" {
		report := RunPairServe(options, deps)
		if options.JSON {
			_ = writeOrderedJSON(out, pairServeValue(report))
		} else {
			_, _ = fmt.Fprintln(out, RenderPairServeReport(report))
		}
		writePairPreflightErrors(report.Status == PairServeStatusNoTailcat || report.Status == PairServeStatusNoConfig, report.Errors, errOut)
		return report.ExitCode()
	}

	report := RunPairJoin(options, deps)
	if options.JSON {
		_ = writeOrderedJSON(out, pairJoinValue(report))
	} else {
		_, _ = fmt.Fprintln(out, RenderPairJoinReport(report))
	}
	writePairPreflightErrors(report.Status == PairJoinStatusNoTailcat || report.Status == PairJoinStatusNoConfig, report.Errors, errOut)
	return report.ExitCode()
}

func RenderPairServeReport(report PairServeReport) string {
	lines := []string{fmt.Sprintf("homer pair: %s", report.Status)}
	if report.Addr != "" {
		lines = append(lines, "  地址: "+report.Addr)
	}
	if report.Peer != nil {
		lines = append(lines, fmt.Sprintf("  对端: %s（%s）", report.Peer.Hostname, report.Peer.Recipient))
	}
	lines = append(lines, fmt.Sprintf("  已发送: %d 个密钥", len(report.Sent)))
	if report.PairedCount > 0 {
		lines = append(lines, fmt.Sprintf("  已配对设备: %d", report.PairedCount))
	}
	for _, warning := range report.Warnings {
		lines = append(lines, "  ⚠ "+warning)
	}
	for _, err := range report.Errors {
		for _, line := range strings.Split(err, "\n") {
			if line != "" {
				lines = append(lines, "  ✗ "+line)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func RenderPairJoinReport(report PairJoinReport) string {
	lines := []string{fmt.Sprintf("homer pair: %s", report.Status)}
	if report.Peer != nil {
		lines = append(lines, fmt.Sprintf("  对端: %s（%s）", report.Peer.Hostname, report.Peer.Recipient))
	}
	lines = append(lines, fmt.Sprintf("  已归位: %d 个密钥", len(report.Applied)))
	if report.BackupDir != "" {
		lines = append(lines, "  备份目录: "+report.BackupDir)
	}
	for _, warning := range report.Warnings {
		lines = append(lines, "  ⚠ "+warning)
	}
	for _, err := range report.Errors {
		for _, line := range strings.Split(err, "\n") {
			if line != "" {
				lines = append(lines, "  ✗ "+line)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func newPairServeReport(status PairServeStatus) PairServeReport {
	return PairServeReport{
		OK:       status == PairServeStatusPaired,
		Status:   status,
		Sent:     []string{},
		Warnings: []string{},
		Errors:   []string{},
	}
}

func newPairJoinReport(status PairJoinStatus) PairJoinReport {
	return PairJoinReport{
		OK:       status == PairJoinStatusPaired,
		Status:   status,
		Applied:  []string{},
		Warnings: []string{},
		Errors:   []string{},
	}
}

func noTailcatServeReport() PairServeReport {
	report := newPairServeReport(PairServeStatusNoTailcat)
	report.Errors = append(report.Errors, pair.TailcatInstallHint)
	return report
}

func noTailcatJoinReport() PairJoinReport {
	report := newPairJoinReport(PairJoinStatusNoTailcat)
	report.Errors = append(report.Errors, pair.TailcatInstallHint)
	return report
}

func noConfigServeReport(err error) PairServeReport {
	report := newPairServeReport(PairServeStatusNoConfig)
	report.Errors = append(report.Errors, safeError(err))
	return report
}

func noConfigJoinReport(err error) PairJoinReport {
	report := newPairJoinReport(PairJoinStatusNoConfig)
	report.Errors = append(report.Errors, safeError(err))
	return report
}

func pairTailcatAvailable(deps *PairDeps) bool {
	// An injected transport is the test seam and intentionally does not need
	// the optional production binary. The default path will be replaced by
	// NewTailcatTransport when the P2 adapter lands.
	if deps != nil && deps.Transport != nil {
		return true
	}
	_, err := pair.DetectTailcat(nil)
	return err == nil
}

func pairConfigReady(home string) error {
	paths := resolveCommandPaths(home)
	config, err := core.LoadConfig(paths)
	if err != nil || config == nil {
		return core.NewCliError(fmt.Sprintf("未找到 homer 配置: %s\n请先运行 `homer init` 生成 homer.json 与 store 快照。", paths.ConfigFile))
	}
	if config.Secrets == nil || len(config.Secrets.Files) == 0 {
		return core.NewCliError("homer.json 的 secrets.files 为空：没有可配对的密钥。")
	}
	return nil
}

func writePairPreflightErrors(enabled bool, errors []string, out io.Writer) {
	if !enabled || out == nil {
		return
	}
	for _, message := range errors {
		for _, line := range strings.Split(message, "\n") {
			if strings.TrimSpace(line) != "" {
				_, _ = fmt.Fprintln(out, line)
			}
		}
	}
}

func pairServeValue(report PairServeReport) orderedjson.Value {
	keys := []string{"ok", "status"}
	values := map[string]orderedjson.Value{
		"ok":             report.OK,
		"status":         string(report.Status),
		"sent":           orderedStringArray(report.Sent),
		"recipientAdded": report.RecipientAdded,
		"pairedCount":    report.PairedCount,
		"warnings":       orderedStringArray(report.Warnings),
		"errors":         orderedStringArray(report.Errors),
	}
	if report.Addr != "" {
		keys = append(keys, "addr")
		values["addr"] = report.Addr
	}
	if report.Peer != nil {
		keys = append(keys, "peer")
		values["peer"] = pairPeerValue(report.Peer)
	}
	keys = append(keys, "sent", "recipientAdded", "pairedCount", "warnings", "errors")
	return &orderedjson.Object{Keys: keys, M: values}
}

func pairJoinValue(report PairJoinReport) orderedjson.Value {
	keys := []string{"ok", "status"}
	values := map[string]orderedjson.Value{
		"ok":       report.OK,
		"status":   string(report.Status),
		"applied":  orderedStringArray(report.Applied),
		"warnings": orderedStringArray(report.Warnings),
		"errors":   orderedStringArray(report.Errors),
	}
	if report.Peer != nil {
		keys = append(keys, "peer")
		values["peer"] = pairPeerValue(report.Peer)
	}
	if report.BackupDir != "" {
		keys = append(keys, "backupDir")
		values["backupDir"] = report.BackupDir
	}
	keys = append(keys, "applied", "warnings", "errors")
	return &orderedjson.Object{Keys: keys, M: values}
}

func pairPeerValue(peer *PairPeerSummary) orderedjson.Value {
	if peer == nil {
		return nil
	}
	return &orderedjson.Object{
		Keys: []string{"hostname", "recipient"},
		M: map[string]orderedjson.Value{
			"hostname":  peer.Hostname,
			"recipient": peer.Recipient,
		},
	}
}
