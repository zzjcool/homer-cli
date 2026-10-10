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
	syncx "github.com/zzjcool/homer-cli/internal/sync"
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
	Kind      string `json:"kind,omitempty"`
	Push      int    `json:"push"`
	Pull      int    `json:"pull"`
	Conflicts int    `json:"conflicts"`
	// Files carries the per-file view (path + status) so the console's
	// machine drawer can list WHICH extensions/skills/settings differ —
	// counts alone answer "how many", not "which". Manifest categories
	// list one entry per PACKAGE (the virtual manifest file is an
	// implementation detail the console must never show).
	Files []StatusFileReport `json:"files"`
}

// StatusFileReport is one file's drift state inside a category.
type StatusFileReport struct {
	Path   string `json:"path"`
	Status string `json:"status"`
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

// ConfigOutlineCategory is a category declaration from the loaded homer.json.
type ConfigOutlineCategory struct {
	Name  string   `json:"name"`
	Paths []string `json:"paths,omitempty"`
}

// ConfigOutlineAdapter is an adapter declaration from the loaded homer.json.
type ConfigOutlineAdapter struct {
	ID         string                  `json:"id"`
	Root       string                  `json:"root,omitempty"`
	Categories []ConfigOutlineCategory `json:"categories,omitempty"`
}

// StatusReport is the machine-readable result of homer status.  errors is
// always present (and is an empty array when clean); warnings is additive and
// omitted when there are no warnings, matching the TS report shape.
type StatusReport struct {
	Adapters []StatusAdapterReport `json:"adapters"`
	Errors   []string              `json:"errors"`
	Warnings []string              `json:"warnings,omitempty"`
	Disabled []string              `json:"disabled,omitempty"`
	// Resolutions carries this machine's recorded conflict decisions so the
	// hub can render choice state without a second round trip
	// (staged-resolution plan S2b; attached by agentd, not RunStatus).
	Resolutions   []StatusResolution      `json:"resolutions,omitempty"`
	ConfigOutline *[]ConfigOutlineAdapter `json:"configOutline,omitempty"`
}

// StatusResolution is one recorded decision in a status report. The shape is
// deliberately structural (commands must not import internal/resolutions).
type StatusResolution struct {
	Adapter            string `json:"adapter"`
	Choice             string `json:"choice"`
	RecordedAt         string `json:"recordedAt"`
	GenerationAtRecord int    `json:"generationAtRecord"`
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

// RootMissingMessages extracts the "adapter root does not exist yet" rows
// from the scan diagnostics. On a fresh machine waiting for its first
// dispatch this is a legal state, not a fault: the rows are reported as
// Warnings ("等待下发") instead of Errors so a new machine does not look
// broken in the console.
func RootMissingMessages(errors SnapshotSourceErrors) []string {
	messages := make([]string, 0)
	for _, entry := range errors {
		for _, scanError := range entry.Errors {
			if !scanError.RootMissing {
				continue
			}
			messages = append(messages, fmt.Sprintf("%s: 还没有 %s 目录，等待首次下发（同步时会自动创建）", entry.AdapterID, scanError.Path))
		}
	}
	return messages
}

// withoutRootMissing returns only the scan rows that are real faults.
func withoutRootMissing(errors SnapshotSourceErrors) SnapshotSourceErrors {
	out := make(SnapshotSourceErrors, 0, len(errors))
	for _, entry := range errors {
		filtered := entry
		filtered.Errors = nil
		for _, scanError := range entry.Errors {
			if scanError.RootMissing {
				continue
			}
			filtered.Errors = append(filtered.Errors, scanError)
		}
		if len(filtered.Errors) > 0 {
			out = append(out, filtered)
		}
	}
	return out
}

func rootUnreadable(outcome adapter.ScanOutcome) bool {
	return len(outcome.Snapshot.Categories) == 0 && len(outcome.Errors) > 0
}

func enabled(value *bool) bool { return value == nil || *value }

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
		for _, category := range adapter.CategoryOrder(adapterID, adapterConfig.Categories) {
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

// NormalizeHubRemote puts the center's snapshot on the same footing as the
// base and local snapshots: entry kinds follow each category's sync mode
// (a merge category's JSON is "json", not a raw "file") and excludeKeys are
// stripped. The wire format carries neither, so without this a merge file
// that is byte-identical on both sides is judged as a pending pull.
func NormalizeHubRemote(remote []core.AdapterSnapshot, config *core.HomerConfig) []core.AdapterSnapshot {
	if remote == nil || config == nil {
		return remote
	}
	out := make([]core.AdapterSnapshot, len(remote))
	for i, snapshot := range remote {
		adapterConfig, known := config.Adapters[snapshot.AdapterID]
		categories := make([]core.CategorySnapshot, len(snapshot.Categories))
		for j, category := range snapshot.Categories {
			category.Files = cloneSnapshotFiles(category.Files)
			if known {
				if categoryConfig, ok := adapterConfig.Categories[category.Category]; ok {
					category.Mode = categoryConfig.Mode
					for path, entry := range category.Files {
						entry.Kind = core.EntryKindFor(category.Mode, entry.Content)
						category.Files[path] = entry
					}
				}
			}
			categories[j] = category
		}
		snapshot.Categories = categories
		out[i] = engine.StripExcludeKeys(snapshot, excludeKeysByCategory(*config, snapshot.AdapterID))
	}
	return out
}

func cloneSnapshotFiles(files core.SnapshotFiles) core.SnapshotFiles {
	out := make(core.SnapshotFiles, len(files))
	for path, entry := range files {
		out[path] = entry
	}
	return out
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
	warnings := make([]string, 0)
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
		outcome := adapter.ScanAdapter(adapterID, syncx.WithEncryptedIgnores(paths.Home, adapterID, adapterConfig))
		warnings = append(warnings, outcome.Warnings...)
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
		files := make([]StatusFileReport, 0)
		if drift.Mode == core.SyncModeMirror {
			for _, op := range drift.Ops {
				files = append(files, StatusFileReport{Path: op.Path, Status: op.Type})
			}
		} else {
			for _, op := range drift.Ops {
				if op.Type == "" || op.Type == "noop" {
					continue
				}
				files = append(files, StatusFileReport{Path: op.Path, Status: op.Type})
			}
			for _, key := range drift.ChangedKeys {
				files = append(files, StatusFileReport{Path: key, Status: "changed"})
			}
			for _, conflict := range drift.MergeConflicts {
				path := mergeConflictPath(conflict)
				if path == "" {
					continue
				}
				upgraded := false
				for i := range files {
					if files[i].Path == path {
						files[i].Status = "conflict"
						upgraded = true
						break
					}
				}
				if !upgraded {
					files = append(files, StatusFileReport{Path: path, Status: "conflict"})
				}
			}
		}
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
			Files:     files,
		})
	}
	if len(warnings) > 0 {
		report.Warnings = append([]string(nil), warnings...)
	}
	return report
}

// mergeConflictPath is the leaf the console can open. A key conflict is
// settings.json:theme; a whole-file conflict is just the file.
func mergeConflictPath(conflict engine.MergeConflict) string {
	file := strings.TrimSpace(conflict.File)
	key := strings.TrimSpace(conflict.KeyPath)
	if file == "" {
		return key
	}
	if key == "" || key == file {
		return file
	}
	return file + ":" + key
}

// applyManifestView rewrites manifest categories' file lists from the
// virtual manifest file (one opaque blob) into one entry per package
// name: "which extensions are installed" reads as a package list, not
// "packages.manifest.txt". kinds maps adapter→category→kind; locals
// maps adapter→category→virtual file content.
func applyManifestView(report *StatusReport, kinds map[string]map[string]string, locals map[string]map[string]string) {
	for i := range report.Adapters {
		adapter := &report.Adapters[i]
		for j := range adapter.Categories {
			category := &adapter.Categories[j]
			isManifest := kinds[adapter.ID] != nil && kinds[adapter.ID][category.Name] == "manifest"
			if !isManifest {
				continue
			}
			category.Kind = "manifest"
			content := ""
			if locals[adapter.ID] != nil {
				content = locals[adapter.ID][category.Name]
			}
			packages := make([]StatusFileReport, 0)
			for _, line := range strings.Split(content, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				packages = append(packages, StatusFileReport{Path: line, Status: "installed"})
			}
			if len(packages) > 0 {
				category.Files = packages
			}
		}
	}
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
			// A machine without homer.json still HAS live configuration
			// worth showing: the built-in adapters (pi/herdr/opencode/
			// vscode) are compiled into the binary. Fall back to the
			// defaults for this READ-ONLY scan — "查看" on a fresh
			// machine shows what is installed instead of refusing.
			// (Write paths — push baseline, store — never fall back.)
			config := &core.HomerConfig{Version: 1, Adapters: defaultAdaptersForScan()}
			report, err := runStatusWithConfig(opts, paths, config, injected...)
			if err != nil {
				return StatusReport{}, err
			}
			// The marker rides in Errors (not Warnings) on purpose:
			// driftSummary keys the console's "新机器 · 等待下发" badge
			// on it — a fresh machine must not read as "↑32 项未收取"
			// (its local files are not drift until a baseline exists).
			report.Errors = append(report.Errors, fmt.Sprintf(
				"未找到 homer 配置: %s；请先运行 `homer init` 生成 homer.json 与 store 快照。", paths.ConfigFile))
			report.Warnings = append([]string{fmt.Sprintf(
				"这台机器还没有接入配置基线。以下为内置适配器的实况扫描；执行一次「下发」或 `homer init` 后进入正式同步。")}, report.Warnings...)
			return report, nil
		}
		return StatusReport{}, err
	}

	report, err := runStatusWithConfig(opts, paths, config, injected...)
	if err != nil {
		return StatusReport{}, err
	}
	report.ConfigOutline = configOutlineFromConfig(config)
	return report, nil
}

// configOutlineFromConfig converts the loaded declaration to the stable,
// JSON-only status outline. Sorting map keys makes the wire representation
// deterministic; Paths retain their declared order.
func configOutlineFromConfig(config *core.HomerConfig) *[]ConfigOutlineAdapter {
	if config == nil {
		return nil
	}

	adapterIDs := make([]string, 0, len(config.Adapters))
	for id := range config.Adapters {
		adapterIDs = append(adapterIDs, id)
	}
	sort.Strings(adapterIDs)

	outline := make([]ConfigOutlineAdapter, 0, len(adapterIDs))
	for _, id := range adapterIDs {
		adapterConfig := config.Adapters[id]
		categoryNames := make([]string, 0, len(adapterConfig.Categories))
		for name := range adapterConfig.Categories {
			categoryNames = append(categoryNames, name)
		}
		sort.Strings(categoryNames)

		categories := make([]ConfigOutlineCategory, 0, len(categoryNames))
		for _, name := range categoryNames {
			categoryConfig := adapterConfig.Categories[name]
			categories = append(categories, ConfigOutlineCategory{
				Name:  name,
				Paths: append([]string(nil), categoryConfig.Paths...),
			})
		}
		outline = append(outline, ConfigOutlineAdapter{
			ID:         id,
			Root:       adapterConfig.Root,
			Categories: categories,
		})
	}
	return &outline
}

// runStatusWithConfig is RunStatus's body with the config already
// resolved (real config, or the built-in defaults fallback).
func runStatusWithConfig(opts StatusOptions, paths core.HomerPaths, config *core.HomerConfig, injected ...DriftSources) (StatusReport, error) {
	var sources DriftSources
	if provided, ok := sourceFromArgs(injected); ok {
		sources = provided
	} else {
		var err error
		sources, err = CollectSnapshotSources(paths, config)
		if err != nil {
			return StatusReport{}, err
		}
	}

	errorMessages := append([]string(nil), sources.Errors...)
	errorMessages = append(errorMessages, sources.ErrorMessages...)
	warnings := append([]string(nil), sources.Warnings...)
	warnings = append(warnings, RootMissingMessages(sources.ScanErrors)...)
	errorMessages = append(errorMessages, sourceErrorMessages(withoutRootMissing(sources.ScanErrors))...)
	report := BuildStatusReport(engine.ComputeDrift(sources.Base, sources.Local, sources.Remote), errorMessages, warnings)
	report.Disabled = DisabledSummaries(*config)
	// Manifest categories surface package names, never the virtual file.
	kinds := map[string]map[string]string{}
	locals := map[string]map[string]string{}
	for adapterID, adapter := range config.Adapters {
		for name, category := range adapter.Categories {
			if !category.IsManifest() {
				continue
			}
			if kinds[adapterID] == nil {
				kinds[adapterID] = map[string]string{}
				locals[adapterID] = map[string]string{}
			}
			kinds[adapterID][name] = "manifest"
			if entry, ok := findLocalEntry(sources.Local, adapterID, name); ok {
				locals[adapterID][name] = entry
			}
		}
	}
	applyManifestView(&report, kinds, locals)
	return report, nil
}

// findLocalEntry pulls one local snapshot entry's content by adapter and
// category (first file wins — manifest categories hold a single virtual
// file).
func findLocalEntry(snapshots []core.AdapterSnapshot, adapterID, category string) (string, bool) {
	for _, adapter := range snapshots {
		if adapter.AdapterID != adapterID {
			continue
		}
		for _, cat := range adapter.Categories {
			if cat.Category != category {
				continue
			}
			for _, entry := range cat.Files {
				return entry.Content, true
			}
		}
	}
	return "", false
}

// runStatus is kept as an internal compatibility spelling for package-local
// tests ported from the TypeScript command module.
func runStatus(opts StatusOptions, injected ...DriftSources) (StatusReport, error) {
	return RunStatus(opts, injected...)
}

// defaultAdaptersForScan clones the compiled-in adapter defaults for the
// read-only fresh-machine fallback (never persisted, never a baseline).
func defaultAdaptersForScan() map[string]core.AdapterConfig {
	selected := make(map[string]core.AdapterConfig, len(knownAdapterOrder))
	for _, id := range knownAdapterOrder {
		selected[id] = cloneAdapterConfig(KNOWN_ADAPTERS[id])
	}
	return selected
}
