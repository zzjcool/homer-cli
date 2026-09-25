package cli

import (
	"io"
	"os"
	"strings"

	"github.com/zzjcool/homer-cli/internal/upgrade"
)

// version is populated by cmd/homer from the GoReleaser ldflag. Keeping the
// default here makes package-level CLI tests and local development builds
// print the explicit development marker.
var version = "dev"

// SetVersion is called once by the thin binary entry point. It is additive so
// callers embedding the cli package can still use the default dev version.
func SetVersion(value string) {
	if strings.TrimSpace(value) != "" {
		version = value
	}
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

// runUpgrade implements `homer upgrade`: check the latest release and, when
// it is newer (or --force is given), download, verify and replace this
// binary. Non-interactive; the user asked for the upgrade explicitly.
func runUpgrade(force bool, out, errOut io.Writer) int {
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
	return 0
}
