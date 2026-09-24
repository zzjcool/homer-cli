package commands

import (
	"fmt"
	"strings"

	"github.com/zzjcool/homer-cli/internal/gitx"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

// RemoteOptions controls the small remote-wiring command. It deliberately does
// not push: users can review the exact first-push command before sending their
// configuration to a remote.
type RemoteOptions struct {
	HomerHome string
	Home      string
	URL       string
	RepoURL   string
	JSON      bool
}

type RemoteStatus string

const (
	RemoteStatusConfigured RemoteStatus = "configured"
	RemoteStatusError      RemoteStatus = "error"
)

type RemoteReport struct {
	OK          bool         `json:"ok"`
	Status      RemoteStatus `json:"status"`
	HomerHome   string       `json:"homerHome"`
	Remote      string       `json:"remote"`
	PushCommand string       `json:"pushCommand"`
	Warnings    []string     `json:"warnings"`
	Errors      []string     `json:"errors"`
}

func newRemoteReport(status RemoteStatus) RemoteReport {
	return RemoteReport{
		OK:       status == RemoteStatusConfigured,
		Status:   status,
		Warnings: []string{},
		Errors:   []string{},
	}
}

func (report RemoteReport) ExitCode() int {
	if report.OK {
		return 0
	}
	return 1
}

const REMOTE_USAGE = `用法: homer remote <url> [options]

确保 ~/.homer 是 git 仓库，把 origin 添加或切换到给定 URL；不自动推送。

选项:
  --home <dir>       homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --json             输出机器可读 JSON（RemoteReport）
  -h, --help         显示本帮助

下一步: 按输出的 git -C <home> push -u origin <branch> 命令首次推送。`

func remoteURL(options RemoteOptions) string {
	if options.URL != "" {
		return options.URL
	}
	return options.RepoURL
}

// RunRemote implements `homer remote <url>`: ensureGitRepo, add/set-url
// origin, and return an exact first-push instruction.
func RunRemote(options RemoteOptions) RemoteReport {
	homerHome := options.HomerHome
	if homerHome == "" {
		homerHome = options.Home
	}
	paths := homeFor(homerHome)
	remote := remoteURL(options)
	if remote == "" {
		report := newRemoteReport(RemoteStatusError)
		report.HomerHome = paths.Home
		report.Errors = []string{"缺少 remote URL", "用法: homer remote <url> [--home <dir>]"}
		return report
	}
	if err := gitx.AssertCloneableRepoURL(remote); err != nil {
		report := newRemoteReport(RemoteStatusError)
		report.HomerHome = paths.Home
		report.Remote = remote
		report.Errors = []string{fmt.Sprintf("remote URL 不得以 \"-\" 开头: %v", err)}
		return report
	}
	if err := gitx.EnsureGitRepo(paths.Home); err != nil {
		report := newRemoteReport(RemoteStatusError)
		report.HomerHome = paths.Home
		report.Remote = remote
		report.Errors = errorLines(err)
		return report
	}
	if err := gitx.AddOrSetRemote(paths.Home, remote); err != nil {
		report := newRemoteReport(RemoteStatusError)
		report.HomerHome = paths.Home
		report.Remote = remote
		report.Errors = errorLines(err)
		return report
	}

	report := newRemoteReport(RemoteStatusConfigured)
	report.HomerHome = paths.Home
	report.Remote = remote
	report.PushCommand = gitx.PushHint(paths.Home)
	report.Warnings = append(report.Warnings,
		"origin 已配置；homer remote 不自动推送配置，请先确认后运行 `"+report.PushCommand+"`",
	)
	return report
}

func RenderRemoteReport(report RemoteReport) string {
	if report.Status == RemoteStatusError {
		lines := []string{"homer remote: 失败"}
		for _, item := range report.Errors {
			lines = append(lines, "  ✗ "+item)
		}
		return strings.Join(lines, "\n")
	}
	lines := []string{
		"homer remote: 已配置",
		"  工作区: " + report.HomerHome,
		"  origin: " + report.Remote,
		"  首次推送: " + report.PushCommand,
	}
	for _, warning := range report.Warnings {
		lines = append(lines, "  ⚠ "+warning)
	}
	return strings.Join(lines, "\n")
}

func RenderRemoteJSON(report RemoteReport) string {
	keys := []string{"ok", "status", "homerHome", "remote", "pushCommand", "warnings", "errors"}
	values := map[string]orderedjson.Value{
		"ok":          report.OK,
		"status":      string(report.Status),
		"homerHome":   report.HomerHome,
		"remote":      report.Remote,
		"pushCommand": report.PushCommand,
		"warnings":    stringArrayValue(report.Warnings),
		"errors":      stringArrayValue(report.Errors),
	}
	return string(orderedjson.Serialize(&orderedjson.Object{Keys: keys, M: values}))
}
