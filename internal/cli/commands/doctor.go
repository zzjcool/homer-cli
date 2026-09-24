package commands

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/zzjcool/homer-cli/internal/agecrypto"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/doctor"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

// DoctorOptions is the command-layer option shape. The command parser owned by
// internal/cli turns --home/--offline/--json into this value.
type DoctorOptions struct {
	HomerHome string
	JSON      bool
	Offline   bool
}

// DoctorDeps contains the one deliberately injectable doctor dependency. A
// nil Age port means that the production filippo.io/age port is used by the
// check itself.
type DoctorDeps struct {
	Age agecrypto.AgeCryptoPort
}

const DOCTOR_USAGE = `用法: homer doctor [options]

对本机 homer 环境做八项体检，逐项报告 ok / warn / fail：
  config       homer.json 存在且合法
  repo         home 是 git 仓库根
  store-clean  store 工作区干净（脏 → warn）
  remote       远端可达（--offline 跳过；不可达 → warn）
  adapters     各 enabled adapter root 存在（缺失 → warn「工具未安装？」）
  age          密钥同步配置与身份（未配置 → ok；identity 缺失 / 不可解 → fail）
  machine      state 与 HEAD 一致（缺失 / 领先 → warn）
  required     store 中的 __REQUIRED__ 占位符残留（有 → warn）

选项:
  --home <dir>       homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --offline          跳过远端可达性检查
  --json             输出机器可读 JSON（DoctorReport）
  -h, --help         显示本帮助

退出码: 无 fail → 0（含仅 warn）；有 fail → 1。`

// resolveCommandPaths is intentionally local to commands. The readonly W8
// render.go owns the eventual CLI helper, but command implementations must not
// import the parent cli package (that would create an import cycle).
func resolveCommandPaths(home string) core.HomerPaths {
	if home == "" {
		return core.GetHomerPaths(nil)
	}
	return core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return home
		}
		return os.Getenv(name)
	})
}

// RunDoctor executes the eight checks in the frozen order. A malformed config
// is still reported as config=fail; adapter/age/required are represented by
// explicit ok/"已跳过" entries so the JSON report always retains eight items.
func RunDoctor(options DoctorOptions, deps *DoctorDeps) DoctorReport {
	paths := resolveCommandPaths(options.HomerHome)
	checks := make([]doctor.DoctorCheck, 0, 8)

	configCheck := doctor.CheckConfig(paths)
	checks = append(checks, configCheck)

	config, configErr := core.LoadConfig(paths)
	if configErr != nil {
		config = nil
	}

	checks = append(checks, doctor.CheckRepoAndStore(paths)...)
	checks = append(checks, doctor.CheckRemote(paths, doctor.RemoteCheckOptions{Offline: options.Offline}))

	if config == nil {
		checks = append(checks,
			skippedDoctorCheck("adapters", "config 不可用"),
			skippedDoctorCheck("age", "config 不可用"),
		)
	} else {
		checks = append(checks, doctor.CheckAdapters(*config))
		var crypto agecrypto.AgeCryptoPort
		if deps != nil {
			crypto = deps.Age
		}
		checks = append(checks, doctor.CheckAge(paths, *config, crypto))
	}

	checks = append(checks, doctor.CheckMachine(paths))
	if config == nil {
		checks = append(checks, skippedDoctorCheck("required", "config 不可用"))
	} else {
		checks = append(checks, doctor.CheckRequiredPlaceholders(paths, *config))
	}

	ok := true
	for _, check := range checks {
		if check.Status == doctor.CheckFail {
			ok = false
			break
		}
	}
	return doctor.DoctorReport{Checks: checks, OK: ok}
}

func skippedDoctorCheck(id string, reason string) doctor.DoctorCheck {
	return doctor.DoctorCheck{
		ID:      doctor.DoctorCheckID(id),
		Status:  doctor.CheckOK,
		Message: fmt.Sprintf("已跳过（%s）", reason),
	}
}

// RenderDoctorReport renders a stable human-oriented report. It is pure and
// therefore also useful to callers embedding the command layer.
func RenderDoctorReport(report doctor.DoctorReport) string {
	lines := []string{fmt.Sprintf("homer doctor: %s", ternary(report.OK, "通过", "存在问题"))}
	marks := map[doctor.CheckStatus]string{
		doctor.CheckOK:   "✓",
		doctor.CheckWarn: "⚠",
		doctor.CheckFail: "✗",
	}
	counts := map[doctor.CheckStatus]int{}
	for _, check := range report.Checks {
		counts[check.Status]++
		lines = append(lines, fmt.Sprintf("  %s %s  %s", marks[check.Status], check.ID, check.Message))
		for _, detail := range check.Details {
			lines = append(lines, "      - "+detail)
		}
	}
	lines = append(lines, fmt.Sprintf("  合计: ok %d  warn %d  fail %d", counts[doctor.CheckOK], counts[doctor.CheckWarn], counts[doctor.CheckFail]))
	if !report.OK {
		lines = append(lines, "  有 fail 项：修复后重跑 `homer doctor`（warn 不影响退出码）。")
	}
	return strings.Join(lines, "\n")
}

// ExecuteDoctor is the command-facing renderer used by the frozen CLI
// dispatcher. It returns the doctor exit code: warnings are successful, fail
// checks return 1.
func ExecuteDoctor(options DoctorOptions, deps *DoctorDeps, out io.Writer) int {
	report := RunDoctor(options, deps)
	if options.JSON {
		_ = writeOrderedJSON(out, doctorReportValue(report))
	} else {
		_, _ = fmt.Fprintln(out, RenderDoctorReport(report))
	}
	return report.ExitCode()
}

// RunDoctorCommand is a descriptive alias for W8's dispatcher integration.
func RunDoctorCommand(options DoctorOptions, deps *DoctorDeps, out io.Writer) int {
	return ExecuteDoctor(options, deps, out)
}

// Package-local spellings mirror the TypeScript command names and make the
// implementation convenient to test without weakening the exported API.
func runDoctor(options DoctorOptions, deps *DoctorDeps) DoctorReport {
	return RunDoctor(options, deps)
}

func renderDoctorReport(report DoctorReport) string { return RenderDoctorReport(report) }

func doctorReportValue(report doctor.DoctorReport) orderedjson.Value {
	checks := make([]orderedjson.Value, 0, len(report.Checks))
	for _, check := range report.Checks {
		keys := []string{"id", "status", "message"}
		values := map[string]orderedjson.Value{
			"id":      string(check.ID),
			"status":  string(check.Status),
			"message": check.Message,
		}
		if len(check.Details) > 0 {
			keys = append(keys, "details")
			values["details"] = orderedStringArray(check.Details)
		}
		checks = append(checks, &orderedjson.Object{Keys: keys, M: values})
	}
	return &orderedjson.Object{
		Keys: []string{"checks", "ok"},
		M: map[string]orderedjson.Value{
			"checks": checks,
			"ok":     report.OK,
		},
	}
}

func orderedStringArray(items []string) orderedjson.Value {
	values := make([]orderedjson.Value, len(items))
	for index, item := range items {
		values[index] = item
	}
	return values
}

func writeOrderedJSON(out io.Writer, value orderedjson.Value) error {
	if out == nil {
		return nil
	}
	_, err := fmt.Fprintln(out, string(orderedjson.Serialize(value)))
	return err
}

func ternary[T any](condition bool, yes, no T) T {
	if condition {
		return yes
	}
	return no
}

// Re-export the report/check types from the command package so callers do not
// need to know whether a check came from core/doctor or command orchestration.
type CheckStatus = doctor.CheckStatus
type DoctorCheckID = doctor.DoctorCheckID
type DoctorCheck = doctor.DoctorCheck
type DoctorReport = doctor.DoctorReport
