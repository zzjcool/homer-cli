package cli

import (
	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"io"
	"os"
	"strings"

	"github.com/zzjcool/homer-cli/internal/upgrade"
	"github.com/zzjcool/homer-cli/internal/web"
)

// version is populated by cmd/homer from the GoReleaser ldflag. Keeping the
// default here makes package-level CLI tests and local development builds
// print the explicit development marker.
var version = "dev"

// SetVersion is called once by the thin binary entry point. It is additive so
// callers embedding the cli package can still use the default dev version.
func SetVersion(value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	version = value
	// The hub health check and the agent heartbeat both read this. One
	// build stamp has to feed every place that says which program is running.
	web.Version = value
}

func currentVersion() string {
	if strings.TrimSpace(version) == "" {
		return "dev"
	}
	return version
}

func renderVersion() string {
	return "homer version: " + currentVersion()
}

// versionCheckOutcome summarises a version check for rendering. Failures are
// silent by design: version printing must never fail or hang on an offline
// machine.
type versionCheckOutcome struct {
	known      bool
	latest     string
	updateHint bool
}

func checkForUpdate(current string) versionCheckOutcome {
	if current == "dev" || strings.TrimSpace(current) == "" {
		return versionCheckOutcome{known: false}
	}
	// Explicit opt-out for CI, tests, and scripted environments.
	if os.Getenv("HOMER_NO_VERSION_CHECK") == "1" {
		return versionCheckOutcome{known: false}
	}
	client := upgrade.NewClient("")
	latest, err := client.FetchLatest()
	if err != nil {
		return versionCheckOutcome{known: false}
	}
	if upgrade.IsNewer(latest.TagName, current) {
		return versionCheckOutcome{known: true, latest: latest.TagName, updateHint: true}
	}
	return versionCheckOutcome{known: true, latest: latest.TagName}
}

func renderVersionChecked(current string, outcome versionCheckOutcome) string {
	lines := []string{"homer version: " + current}
	if outcome.known && outcome.updateHint {
		lines = append(lines, "")
		lines = append(lines, "有新版本可用: "+outcome.latest+"（当前 "+current+"）")
		lines = append(lines, "升级: homer upgrade")
	}
	return strings.Join(lines, "\n")
}

// runUpgrade implements `homer upgrade`. Two channels, hub first:
//
//  1. HUB channel (agent machines): pull the hub's OWN binary via
//     /dl/homer with the machine's per-agent secret (agent.json) or the
//     hub token (keys/hub-token). This is the fleet's self-update path —
//     the user story: "为什么升级这么麻烦，能不能增加一个命令，我执行
//     一下，自己就升级了".
//  2. GITHUB channel (no hub credential): the original release-based
//     self-update, unchanged.
func runUpgrade(force bool, connect, home string, out, errOut io.Writer) int {
	// Hub channel first: commands.RunUpgrade resolves the machine's own
	// credential (agent.json secret > keys/hub-token) and hub address.
	// It reports NoCredential when neither exists (GitHub-only installs).
	report := commands.RunUpgrade(commands.UpgradeOptions{
		HomerHome: home,
		Connect:   connect,
		Out:       out,
		ErrOut:    errOut,
	})
	if report.OK {
		return finishUpgrade(out, errOut, report.Binary, home)
	}
	if report.Status != "no-credential" {
		// An enrolled machine (credential exists) must NOT silently fall
		// back to GitHub: the release channel is far behind the hub
		// build — a "successful" fallback would DOWNGRADE the machine
		// and strip its features. Surface the hub error instead.
		writeLine(errOut, "homer upgrade: hub 通道失败: "+report.Note)
		writeLine(errOut, "homer upgrade: 接入机器不从 GitHub release 升级（会降级）；请检查 hub 可达后重试")
		return 1
	}

	current := currentVersion()
	client := upgrade.NewClient(os.Getenv("HOMER_INSTALL_BASE_URL"))
	latest, err := client.FetchLatest()
	if err != nil {
		writeLine(errOut, "homer upgrade: 无法获取最新版本信息（离线或网络受限）: "+err.Error())
		return 1
	}
	if !force && !upgrade.IsNewer(latest.TagName, current) {
		writeLine(out, "homer upgrade: 已是最新版本 "+latest.TagName)
		return 0
	}

	target, err := upgrade.SelfPath()
	if err != nil {
		writeLine(errOut, "homer upgrade: 无法定位当前二进制: "+err.Error())
		return 1
	}
	downloader := upgrade.NewDownloader(os.Getenv("HOMER_INSTALL_BASE_URL"))
	writeLine(out, "homer upgrade: "+current+" → "+latest.TagName+"（"+target+"）")
	if err := downloader.InstallArchive(latest.TagName, target); err != nil {
		writeLine(errOut, "homer upgrade: 升级失败: "+err.Error())
		return 1
	}
	writeLine(out, "✓ homer 升级完成（"+latest.TagName+"）")
	writeLine(out, "验证: homer version")
	return finishUpgrade(out, errOut, target, home)
}

// finishUpgrade restarts an agent that is already running so it loads the
// binary just written. The upgrade process is not the agent; the agent
// daemon restarts itself after a remote upgrade.
func finishUpgrade(out, errOut io.Writer, binary, home string) int {
	note, err := commands.RestartRunningAgents(binary, home)
	if note != "" {
		writeLine(out, note)
	}
	if err != nil {
		writeLine(errOut, "homer upgrade: "+err.Error())
		return 1
	}
	return 0
}
