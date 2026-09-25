package commands

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/engine"
	"github.com/zzjcool/homer-cli/internal/gitx"
)

// StatusOptions is the read-only status command's option set.  JSON and
// Verbose affect presentation in the dispatcher; the computation is the same
// in either mode.
type StatusOptions struct {
	HomerHome string
	// Home is a compatibility alias for callers that use the CLI flag name.
	Home    string
	JSON    bool
	Verbose bool
}

// StatusCategoryReport is the JSON/text shape for one category.
type StatusCategoryReport struct {
	Name      string `json:"name"`
	Push      int    `json:"push"`
	Pull      int    `json:"pull"`
	Conflicts int    `json:"conflicts"`
}

// StatusAdapterReport is the JSON/text shape for one adapter.  Its fields
// intentionally mirror the archived TypeScript StatusReport.
type StatusAdapterReport struct {
	ID         string                 `json:"id"`
	Push       int                    `json:"push"`
	Pull       int                    `json:"pull"`
	Conflicts  int                    `json:"conflicts"`
	Categories []StatusCategoryReport `json:"categories"`
}

// StatusReport is the machine-readable result of homer status.  errors is
// always present (and is an empty array when clean); warnings is additive and
// omitted when there are no warnings, matching the TS report shape.
type StatusReport struct {
	Adapters []StatusAdapterReport `json:"adapters"`
	Errors   []string              `json:"errors"`
	Warnings []string              `json:"warnings,omitempty"`
	Disabled []string              `json:"disabled,omitempty"`
}

// SnapshotSourceError keeps scan diagnostics structured until the report
// layer.  RootUnreadable is the M-A guard: an empty snapshot from an
// unreadable root must not be interpreted as an intentional deletion.
type SnapshotSourceError struct {
	AdapterID      string
	RootUnreadable bool
	Errors         []adapter.ScanError
}

// SnapshotSourceErrors is the collection form used by DriftSources.
type SnapshotSourceErrors []SnapshotSourceError

// DriftSources are the three snapshots used by the engine.  A nil Remote is
// intentional and means remote == base; a non-nil empty Remote means a real
// empty remote snapshot.  ErrorMessages is useful for injected tests while
// ScanErrors is used by production collection and retains the M-A metadata.
type DriftSources struct {
	Base   []core.AdapterSnapshot
	Local  []core.AdapterSnapshot
	Remote []core.AdapterSnapshot
	// Errors is the simple injected-message seam.  ScanErrors retains the
	// structured production diagnostics and is rendered additively below.
	Errors        []string
	ErrorMessages []string // compatibility alias; prefer Errors in new callers
	ScanErrors    SnapshotSourceErrors
	Warnings      []string
}

// CliDriftSources is the name used by the renderer-facing API.
type CliDriftSources = DriftSources

const STATUS_USAGE = `用法: homer status [options]

显示本地（实时扫描）与 store 快照之间的漂移概览。
↑ = 本地相对 base 的变更（可 push）；↓ = 远端相对 base 的变更（可 pull）。

选项:
  --home <dir>       homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --json             输出机器可读 JSON（StatusReport）
  --verbose, -v      逐分类打印计数
  -h, --help         显示本帮助

漂移是信息而非错误：即使有漂移也以 0 退出；只有配置缺失等真错误才退出 1。`

// StatusUsage is the idiomatic alias for the archived constant spelling.
const StatusUsage = STATUS_USAGE

// sourceErrorMessages converts structured scan diagnostics into the additive
// report channel used by both status and diff.
func sourceErrorMessages(errors SnapshotSourceErrors) []string {
	messages := make([]string, 0)
	for _, entry := range errors {
		prefix := "扫描告警"
		if entry.RootUnreadable {
			prefix = "adapter root 不可读"
		}
		for _, scanError := range entry.Errors {
			messages = append(messages, fmt.Sprintf("%s: %s (%s: %s)", prefix, entry.AdapterID, scanError.Path, scanError.Message))
		}
	}
	return messages
}

// SourceErrorMessages is the exported spelling used by renderers and tests.
func SourceErrorMessages(errors SnapshotSourceErrors) []string {
	return sourceErrorMessages(errors)
}

func rootUnreadable(outcome adapter.ScanOutcome) bool {
	return len(outcome.Snapshot.Categories) == 0 && len(outcome.Errors) > 0
}

func enabled(value *bool) bool { return value == nil || *value }

// statusCategoryOrder keeps disabled summaries in the same stable order as
// adapter scans.  CategoryConfig is a map in the frozen config shape, so
// built-in categories need an explicit preference and custom names fall back
// to lexical order.
func statusCategoryOrder(adapterID string, categories map[string]core.CategoryConfig) []string {
	preferred := map[string][]string{
		"pi":       {"settings", "skills", "extensions", "agents", "models", "prompts", "themes"},
		"herdr":    {"config"},
		"opencode": {"config", "plugins", "locks"},
		"vscode":   {"settings", "keybindings", "extensions"},
	}[adapterID]

	ordered := make([]string, 0, len(categories))
	seen := make(map[string]struct{}, len(categories))
	for _, name := range preferred {
		if _, ok := categories[name]; ok {
			ordered = append(ordered, name)
			seen[name] = struct{}{}
		}
	}
	rest := make([]string, 0, len(categories)-len(ordered))
	for name := range categories {
		if _, ok := seen[name]; !ok {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append(ordered, rest...)
}

// DisabledSummaries returns the disabled adapter/category paths used by the
// status report.  A disabled adapter is represented by just its adapter ID;
// its disabled categories are intentionally not repeated because the adapter
// switch already suppresses all of them.
func DisabledSummaries(config core.HomerConfig) []string {
	adapterIDs := make([]string, 0, len(config.Adapters))
	for adapterID := range config.Adapters {
		adapterIDs = append(adapterIDs, adapterID)
	}
	sort.Strings(adapterIDs)

	disabled := make([]string, 0)
	for _, adapterID := range adapterIDs {
		adapterConfig := config.Adapters[adapterID]
		if !enabled(adapterConfig.Enabled) {
			disabled = append(disabled, adapterID)
			continue
		}
		for _, category := range statusCategoryOrder(adapterID, adapterConfig.Categories) {
			if !enabled(adapterConfig.Categories[category].Enabled) {
				disabled = append(disabled, adapterID+"/"+category)
			}
		}
	}
	return disabled
}

func excludeKeysByCategory(config core.HomerConfig, adapterID string) map[string][]string {
	adapterConfig, ok := config.Adapters[adapterID]
	if !ok {
		return nil
	}
	result := make(map[string][]string)
	for category, categoryConfig := range adapterConfig.Categories {
		if len(categoryConfig.ExcludeKeys) == 0 {
			continue
		}
		result[category] = append([]string(nil), categoryConfig.ExcludeKeys...)
	}
	return result
}

func stripExcludedSnapshots(snapshots []core.AdapterSnapshot, config core.HomerConfig) []core.AdapterSnapshot {
	result := make([]core.AdapterSnapshot, len(snapshots))
	for i, snapshot := range snapshots {
		result[i] = engine.StripExcludeKeys(snapshot, excludeKeysByCategory(config, snapshot.AdapterID))
	}
	return result
}

// CollectSnapshotSources gathers store(base), live adapter(local), and the
// optional git upstream(remote) snapshots.  The implementation deliberately
// keeps all filesystem/git work here so status/diff tests can inject hand-made
// snapshots without importing sync.
func CollectSnapshotSources(paths core.HomerPaths, config *core.HomerConfig) (DriftSources, error) {
	if config == nil {
		return DriftSources{}, errors.New("homer 配置为空")
	}

	base, err := core.ReadSnapshotFromStore(paths, *config)
	if err != nil {
		return DriftSources{}, err
	}
	base = stripExcludedSnapshots(base, *config)

	local := make([]core.AdapterSnapshot, 0, len(config.Adapters))
	scanErrors := make(SnapshotSourceErrors, 0)
	adapterIDs := make([]string, 0, len(config.Adapters))
	for adapterID := range config.Adapters {
		adapterIDs = append(adapterIDs, adapterID)
	}
	sort.Strings(adapterIDs)
	for _, adapterID := range adapterIDs {
		adapterConfig := config.Adapters[adapterID]
		if !enabled(adapterConfig.Enabled) {
			continue
		}
		outcome := adapter.ScanAdapter(adapterID, adapterConfig)
		if len(outcome.Errors) > 0 {
			scanErrors = append(scanErrors, SnapshotSourceError{
				AdapterID:      adapterID,
				RootUnreadable: rootUnreadable(outcome),
				Errors:         append([]adapter.ScanError(nil), outcome.Errors...),
			})
		}

		if rootUnreadable(outcome) {
			// Do not turn an unreadable root into a full push-delete.  Keep the
			// base view for that adapter and expose the reason in ScanErrors.
			for _, snapshot := range base {
				if snapshot.AdapterID == adapterID {
					local = append(local, snapshot)
					break
				}
			}
			continue
		}
		local = append(local, engine.StripExcludeKeys(outcome.Snapshot, excludeKeysByCategory(*config, adapterID)))
	}

	warnings := make([]string, 0)
	if gitx.IsGitRepo(paths.Home) && !gitx.IsStoreClean(paths.Home) {
		warnings = append(warnings, "store 工作区有未提交的改动（漂移计数以 git 基线为准，可能未反映刚直改的内容）；如需提交请运行 `homer push`")
	}

	var remote []core.AdapterSnapshot
	if gitx.IsGitRepo(paths.Home) {
		ref := gitx.UpstreamRef(paths.Home)
		if ref != "" {
			fetched := gitx.Fetch(paths.Home)
			if !fetched.OK {
				reason := strings.TrimSpace(fetched.Stderr)
				if reason == "" {
					reason = "未知错误"
				}
				warnings = append(warnings, fmt.Sprintf("git fetch 失败（远端变更不可见，↓ 计数可能偏小）: %s", reason))
			} else {
				resolved := gitx.Exec(paths.Home, []string{"rev-parse", "--verify", ref + "^{commit}"}, 0)
				commit := strings.TrimSpace(resolved.Stdout)
				if resolved.OK && commit != "" {
					remote = stripExcludedSnapshots(gitx.ReadStoreSnapshotAtCommit(paths, config, commit), *config)
				}
			}
		}
	}

	return DriftSources{
		Base:       base,
		Local:      local,
		Remote:     remote,
		ScanErrors: scanErrors,
		Warnings:   warnings,
	}, nil
}

func sourceFromArgs(args []DriftSources) (DriftSources, bool) {
	if len(args) == 0 {
		return DriftSources{}, false
	}
	return args[0], true
}

// BuildStatusReport aggregates the engine's flattened category drifts into the
// adapter/category JSON shape frozen by the TypeScript implementation.
func BuildStatusReport(drifts []engine.CategoryDrift, extra ...[]string) StatusReport {
	var errors, warnings []string
	if len(extra) > 0 {
		errors = extra[0]
	}
	if len(extra) > 1 {
		warnings = extra[1]
	}
	report := StatusReport{
		Adapters: make([]StatusAdapterReport, 0),
		Errors:   append([]string(nil), errors...),
	}
	if report.Errors == nil {
		report.Errors = []string{}
	}

	adapterIndex := make(map[string]int)
	for _, drift := range drifts {
		index, ok := adapterIndex[drift.AdapterID]
		if !ok {
			index = len(report.Adapters)
			adapterIndex[drift.AdapterID] = index
			report.Adapters = append(report.Adapters, StatusAdapterReport{
				ID:         drift.AdapterID,
				Categories: make([]StatusCategoryReport, 0),
			})
		}
		adapterReport := &report.Adapters[index]
		adapterReport.Push += drift.Push
		adapterReport.Pull += drift.Pull
		adapterReport.Conflicts += drift.Conflicts
		adapterReport.Categories = append(adapterReport.Categories, StatusCategoryReport{
			Name:      drift.Category,
			Push:      drift.Push,
			Pull:      drift.Pull,
			Conflicts: drift.Conflicts,
		})
	}
	if len(warnings) > 0 {
		report.Warnings = append([]string(nil), warnings...)
	}
	return report
}

// RunStatus computes a status report.  Supplying one DriftSources value is a
// test seam; with no injected value the real store/scan/git collector is used.
func RunStatus(opts StatusOptions, injected ...DriftSources) (StatusReport, error) {
	homerHome := opts.HomerHome
	if homerHome == "" {
		homerHome = opts.Home
	}
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" && homerHome != "" {
			return homerHome
		}
		return os.Getenv(key)
	})
	config, err := core.LoadConfig(paths)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return StatusReport{}, core.NewCliError(fmt.Sprintf("未找到 homer 配置: %s；请先运行 `homer init` 生成 homer.json 与 store 快照。", paths.ConfigFile))
		}
		return StatusReport{}, err
	}

	var sources DriftSources
	if provided, ok := sourceFromArgs(injected); ok {
		sources = provided
	} else {
		sources, err = CollectSnapshotSources(paths, config)
		if err != nil {
			return StatusReport{}, err
		}
	}

	errorMessages := append([]string(nil), sources.Errors...)
	errorMessages = append(errorMessages, sources.ErrorMessages...)
	errorMessages = append(errorMessages, sourceErrorMessages(sources.ScanErrors)...)
	report := BuildStatusReport(engine.ComputeDrift(sources.Base, sources.Local, sources.Remote), errorMessages, sources.Warnings)
	report.Disabled = DisabledSummaries(*config)
	return report, nil
}

// runStatus is kept as an internal compatibility spelling for package-local
// tests ported from the TypeScript command module.
func runStatus(opts StatusOptions, injected ...DriftSources) (StatusReport, error) {
	return RunStatus(opts, injected...)
}
