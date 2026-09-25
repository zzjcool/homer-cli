package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/adapter/herdr"
	"github.com/zzjcool/homer-cli/internal/adapter/opencode"
	"github.com/zzjcool/homer-cli/internal/adapter/pi"
	"github.com/zzjcool/homer-cli/internal/adapter/vscode"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gitx"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

// InitOptions controls the non-interactive scan performed by homer init.
type InitOptions struct {
	HomerHome string
	// Home is a compatibility alias for callers that use the flag spelling.
	Home     string
	Adapters []string
	JSON     bool
	All      bool
	Force    bool
	Remote   string
}

// InitRunOptions contains command-only switches that are not part of the
// report computation.  Force is intentionally separate so the normal
// InitOptions shape remains compatible with the frozen TS API.
type InitRunOptions struct {
	Force bool
}

// InitDeps is the init orchestration seam.  Production leaves all fields nil;
// focused tests can provide a hand-made scan outcome without touching a real
// adapter root, while store/config writes remain the real implementations.
type InitDeps struct {
	Scan          func(adapterID string, config core.AdapterConfig) adapter.ScanOutcome
	WriteSnapshot func(paths core.HomerPaths, snapshot core.AdapterSnapshot) error
	SaveConfig    func(paths core.HomerPaths, config core.HomerConfig) error
	Wizard        WizardPort
}

// InitCategoryReport is the per-category file count in InitReport.
type InitCategoryReport struct {
	Name      string `json:"name"`
	FileCount int    `json:"fileCount"`
}

// InitAdapterReport is the per-adapter portion of InitReport.
type InitAdapterReport struct {
	ID         string               `json:"id"`
	Categories []InitCategoryReport `json:"categories"`
}

// InitReport is the machine-readable and presentation-neutral init result.
type InitReport struct {
	HomerHome string              `json:"homerHome"`
	Adapters  []InitAdapterReport `json:"adapters"`
	Errors    []string            `json:"errors"`
	Warnings  []string            `json:"warnings,omitempty"`
}

// KNOWN_ADAPTERS is the P3 registration table.  The map is exported for
// focused tests and callers that need to inspect the built-in set; iteration
// order is controlled by knownAdapterOrder below.
var KNOWN_ADAPTERS = map[string]core.AdapterConfig{
	pi.PIAdapterID:             pi.DefaultPIAdapter,
	herdr.HerdrAdapterID:       herdr.DefaultHerdrAdapter,
	opencode.OpencodeAdapterID: opencode.DefaultOpencodeAdapter,
	vscode.VSCodeAdapterID:     vscode.DefaultVSCodeAdapter,
}

// KnownAdapters is the idiomatic alias for KNOWN_ADAPTERS.
var KnownAdapters = KNOWN_ADAPTERS

var knownAdapterOrder = []string{pi.PIAdapterID, herdr.HerdrAdapterID, opencode.OpencodeAdapterID, vscode.VSCodeAdapterID}

const INIT_USAGE = `用法: homer init [options]

扫描已启用的 adapter，生成 homer.json 并把初始快照写入 store。

选项:
  --home <dir>          homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --adapters <ids>      只初始化指定 adapter（逗号分隔，可重复）
  --all                 显式初始化全部内置 adapter（跳过交互向导）
  --force               覆盖已存在的 homer.json
  --remote <url>        初始化 git、绑定 origin，并建立/推送首个同步基线
  --json                输出机器可读 JSON（InitReport）
  -h, --help            显示本帮助

注意: 已存在 homer.json 时会拒绝覆盖，除非显式给出 --force；TTY 下可交互调整向导选择。`

// InitUsage is the idiomatic alias for the archived constant spelling.
const InitUsage = INIT_USAGE

func selectedAdapters(ids []string) (map[string]core.AdapterConfig, error) {
	if len(ids) == 0 {
		selected := make(map[string]core.AdapterConfig, len(knownAdapterOrder))
		for _, id := range knownAdapterOrder {
			selected[id] = KNOWN_ADAPTERS[id]
		}
		return selected, nil
	}

	selected := make(map[string]core.AdapterConfig, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		config, ok := KNOWN_ADAPTERS[id]
		if !ok {
			return nil, core.NewCliError(fmt.Sprintf("未知 adapter: %s；已知 adapter: pi, herdr, opencode, vscode；自定义 adapter 请直接编辑 homer.json", id))
		}
		selected[id] = config
	}
	return selected, nil
}

func selectedAdapterIDs(selected map[string]core.AdapterConfig) []string {
	return orderedConfigIDs(selected)
}

// saveInitConfig preserves the known adapter registration order in the JSON
// object.  core.HomerConfig intentionally uses a Go map, so its generic
// serializer sorts map keys; init has a frozen TS registration order that is
// useful to humans and observable by ordered JSON consumers.
func saveInitConfig(paths core.HomerPaths, config core.HomerConfig) error {
	value := core.ConfigValue(config)
	root, ok := value.(*orderedjson.Object)
	if !ok || root == nil {
		return fmt.Errorf("无法构造 homer 配置 JSON")
	}
	adapters, ok := root.M["adapters"].(*orderedjson.Object)
	if !ok || adapters == nil {
		return fmt.Errorf("无法构造 adapters JSON")
	}
	keys := make([]string, 0, len(config.Adapters))
	seen := make(map[string]struct{}, len(config.Adapters))
	for _, id := range knownAdapterOrder {
		if _, exists := config.Adapters[id]; exists {
			keys = append(keys, id)
			seen[id] = struct{}{}
		}
	}
	extra := make([]string, 0)
	for id := range config.Adapters {
		if _, exists := seen[id]; !exists {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	adapters.Keys = append(keys, extra...)

	raw := orderedjson.Serialize(value)
	if validated, validationErrors := core.ValidateConfig(raw); validated == nil {
		return fmt.Errorf("拒绝写入非法配置:\n  - %s", strings.Join(validationErrors, "\n  - "))
	}
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o777); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d", paths.ConfigFile, os.Getpid())
	if err := os.WriteFile(tmp, orderedjson.SerializeFile(value), 0o666); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, paths.ConfigFile); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// RunInit performs scan -> store snapshot -> config write.  Existing
// homer.json is never overwritten unless runOpts.Force is true.  The option is
// variadic so the natural RunInit(opts) form and the force-aware form both
// remain convenient for focused tests.
func RunInit(opts InitOptions, runOptions ...InitRunOptions) (InitReport, error) {
	return runInitWithDeps(opts, InitDeps{}, runOptions...)
}

// RunInitWithDeps is the hand-snapshot orchestration seam used by tests.  The
// optional run option keeps force behavior available without changing the
// normal one-argument call.
func RunInitWithDeps(opts InitOptions, deps InitDeps, runOptions ...InitRunOptions) (InitReport, error) {
	return runInitWithDeps(opts, deps, runOptions...)
}

func runInitWithDeps(opts InitOptions, deps InitDeps, runOptions ...InitRunOptions) (InitReport, error) {
	force := opts.Force
	if len(runOptions) > 0 {
		force = runOptions[0].Force
	}
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

	_, statErr := os.Stat(paths.ConfigFile)
	configExists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return InitReport{}, statErr
	}
	interactive := initWizardActive(opts, deps)
	if configExists && !force && !interactive {
		return InitReport{}, core.NewCliError(fmt.Sprintf(
			"已存在 homer 配置: %s；拒绝覆盖。如需重新初始化，请加 `--force`（会覆盖 homer.json 与 store 快照）。TTY 下可交互调整选择。",
			paths.ConfigFile,
		))
	}
	if opts.Remote != "" {
		if err := gitx.AssertCloneableRepoURL(opts.Remote); err != nil {
			return InitReport{}, core.NewCliError("remote URL 不得以 \"-\" 开头: " + err.Error())
		}
	}

	scan := deps.Scan
	if scan == nil {
		scan = func(adapterID string, config core.AdapterConfig) adapter.ScanOutcome {
			return adapter.ScanAdapter(adapterID, config)
		}
	}
	writeSnapshot := deps.WriteSnapshot
	if writeSnapshot == nil {
		writeSnapshot = core.WriteSnapshotToStore
	}
	saveConfig := deps.SaveConfig
	if saveConfig == nil {
		saveConfig = saveInitConfig
	}

	var existing *core.HomerConfig
	if configExists {
		loaded, err := core.LoadConfig(paths)
		if err != nil {
			return InitReport{}, err
		}
		existing = loaded
	}

	if interactive {
		return runInteractiveInit(opts, force, configExists, existing, paths, deps, scan, writeSnapshot, saveConfig)
	}

	config, err := nonInteractiveInitConfig(opts, force, existing)
	if err != nil {
		return InitReport{}, err
	}
	outcomes := scanInitAdapters(config, selectedAdapterIDs(config.Adapters), scan, false)
	for _, outcome := range outcomes {
		if err := writeSnapshot(paths, outcome.Snapshot); err != nil {
			return InitReport{}, err
		}
	}
	report := buildInitReport(outcomes, outcomesToSnapshots(outcomes), config, paths.Home)
	if err := saveConfig(paths, config); err != nil {
		return InitReport{}, err
	}
	if opts.Remote != "" {
		if err := initializeRemote(paths, opts.Remote); err != nil {
			return InitReport{}, err
		}
	}
	return report, nil
}

func initWizardActive(opts InitOptions, deps InitDeps) bool {
	if opts.JSON || opts.All || len(opts.Adapters) > 0 {
		return false
	}
	// An injected wizard is the package-level fake-port seam. It also lets
	// tests exercise the TTY branch without making the test process itself a
	// terminal; production calls with a nil Wizard still require a real TTY.
	return isTTY() || deps.Wizard != nil
}

func nonInteractiveInitConfig(opts InitOptions, force bool, existing *core.HomerConfig) (core.HomerConfig, error) {
	selected, err := selectedAdapters(opts.Adapters)
	if err != nil {
		return core.HomerConfig{}, err
	}
	if !force {
		return core.HomerConfig{Version: 1, Adapters: cloneAdapterMap(selected)}, nil
	}
	config := core.HomerConfig{Version: 1, Adapters: cloneAdapterMap(selected)}
	if existing == nil {
		return config, nil
	}
	for adapterID, adapterConfig := range existing.Adapters {
		if builtinConfig, builtin := KNOWN_ADAPTERS[adapterID]; builtin {
			// --adapters narrows newly registered built-ins, but --force still
			// must not delete an adapter that already exists. Existing built-ins
			// are reset to their defaults; non-existing ones stay omitted.
			if _, selected := config.Adapters[adapterID]; !selected {
				config.Adapters[adapterID] = cloneAdapterConfig(builtinConfig)
			}
			continue
		}
		config.Adapters[adapterID] = cloneAdapterConfig(adapterConfig)
	}
	config.Backup = cloneBackupConfig(existing.Backup)
	config.Secrets = cloneSecretsConfig(existing.Secrets)
	return config, nil
}

func runInteractiveInit(
	opts InitOptions,
	force bool,
	configExists bool,
	existing *core.HomerConfig,
	paths core.HomerPaths,
	deps InitDeps,
	scan func(adapterID string, config core.AdapterConfig) adapter.ScanOutcome,
	writeSnapshot func(paths core.HomerPaths, snapshot core.AdapterSnapshot) error,
	saveConfig func(paths core.HomerPaths, config core.HomerConfig) error,
) (InitReport, error) {
	var config core.HomerConfig
	reInit := configExists && !force
	if reInit {
		if existing == nil {
			return InitReport{}, fmt.Errorf("无法加载已有 homer 配置")
		}
		config = cloneHomerConfig(*existing)
	} else {
		var err error
		config, err = nonInteractiveInitConfig(opts, force, existing)
		if err != nil {
			return InitReport{}, err
		}
	}

	// Scan an enabled view even when the current config has an adapter/category
	// disabled. This lets re-init show the complete three-level choice tree and
	// lets a user re-enable a previously disabled category without editing JSON.
	scanConfig := cloneHomerConfig(config)
	enableWizardScan(&scanConfig)
	ids := selectedAdapterIDs(scanConfig.Adapters)
	outcomes := scanInitAdapters(scanConfig, ids, scan, true)
	state := buildWizardState(config, outcomes, reInit)
	port := deps.Wizard
	if port == nil {
		port = NewDefaultWizardPort(isTTY())
	}
	selection, err := RunSelectionWizard(port, state)
	if err != nil {
		return InitReport{}, core.NewCliError("已取消初始化")
	}
	beforeSelection := cloneHomerConfig(config)
	ApplySelectionToConfig(&config, selection)
	filtered := FilterSnapshotsBySelection(outcomesToSnapshots(outcomes), config)
	report := buildInitReport(outcomes, filtered, config, paths.Home)
	if reInit {
		if !reflect.DeepEqual(beforeSelection, config) {
			if err := saveConfig(paths, config); err != nil {
				return InitReport{}, err
			}
		}
		report.Warnings = append(report.Warnings, "已更新选择；store 快照未重写，`homer push` 提交当前本地状态")
		if opts.Remote != "" {
			if err := initializeRemote(paths, opts.Remote); err != nil {
				return InitReport{}, err
			}
		}
		return report, nil
	}

	for _, snapshot := range filtered {
		if err := writeSnapshot(paths, snapshot); err != nil {
			return InitReport{}, err
		}
	}
	if err := saveConfig(paths, config); err != nil {
		return InitReport{}, err
	}
	if opts.Remote != "" {
		if err := initializeRemote(paths, opts.Remote); err != nil {
			return InitReport{}, err
		}
	}
	return report, nil
}

func scanInitAdapters(config core.HomerConfig, ids []string, scan func(adapterID string, config core.AdapterConfig) adapter.ScanOutcome, wizard bool) []adapter.ScanOutcome {
	outcomes := make([]adapter.ScanOutcome, 0, len(ids))
	for _, adapterID := range ids {
		adapterConfig := config.Adapters[adapterID]
		if wizard {
			adapterConfig = wizardScanAdapterConfig(adapterConfig)
		}
		outcome := scan(adapterID, adapterConfig)
		outcome.Snapshot.AdapterID = adapterID
		for index := range outcome.Snapshot.Categories {
			outcome.Snapshot.Categories[index].AdapterID = adapterID
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}

func wizardScanAdapterConfig(config core.AdapterConfig) core.AdapterConfig {
	config.Enabled = boolPointer(true)
	for categoryName, categoryConfig := range config.Categories {
		categoryConfig.Enabled = boolPointer(true)
		config.Categories[categoryName] = categoryConfig
	}
	return config
}

func enableWizardScan(config *core.HomerConfig) {
	if config == nil {
		return
	}
	for adapterID, adapterConfig := range config.Adapters {
		config.Adapters[adapterID] = wizardScanAdapterConfig(adapterConfig)
	}
}

func outcomesToSnapshots(outcomes []adapter.ScanOutcome) []core.AdapterSnapshot {
	snapshots := make([]core.AdapterSnapshot, 0, len(outcomes))
	for _, outcome := range outcomes {
		snapshots = append(snapshots, outcome.Snapshot)
	}
	return snapshots
}

func buildInitReport(outcomes []adapter.ScanOutcome, snapshots []core.AdapterSnapshot, config core.HomerConfig, homerHome string) InitReport {
	report := InitReport{
		HomerHome: homerHome,
		Adapters:  make([]InitAdapterReport, 0, len(snapshots)),
		Errors:    make([]string, 0),
	}
	for _, outcome := range outcomes {
		if len(outcome.Errors) > 0 {
			report.Errors = append(report.Errors, sourceErrorMessages(SnapshotSourceErrors{
				{
					AdapterID:      outcome.Snapshot.AdapterID,
					RootUnreadable: rootUnreadable(outcome),
					Errors:         append([]adapter.ScanError(nil), outcome.Errors...),
				},
			})...)
		}
	}
	filteredByID := make(map[string]core.AdapterSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		filteredByID[snapshot.AdapterID] = snapshot
	}
	for _, adapterID := range selectedAdapterIDs(config.Adapters) {
		adapterConfig, ok := config.Adapters[adapterID]
		if !ok || !enabledSelection(adapterConfig.Enabled) {
			continue
		}
		snapshot, ok := filteredByID[adapterID]
		if !ok {
			continue
		}
		adapterReport := InitAdapterReport{ID: adapterID, Categories: make([]InitCategoryReport, 0, len(snapshot.Categories))}
		for _, category := range snapshot.Categories {
			adapterReport.Categories = append(adapterReport.Categories, InitCategoryReport{
				Name:      category.Category,
				FileCount: len(category.Files),
			})
		}
		report.Adapters = append(report.Adapters, adapterReport)
	}
	return report
}

func boolPointer(value bool) *bool { return &value }

func cloneHomerConfig(config core.HomerConfig) core.HomerConfig {
	return core.HomerConfig{
		Version:  config.Version,
		Adapters: cloneAdapterMap(config.Adapters),
		Backup:   cloneBackupConfig(config.Backup),
		Secrets:  cloneSecretsConfig(config.Secrets),
	}
}

func cloneAdapterMap(adapters map[string]core.AdapterConfig) map[string]core.AdapterConfig {
	if adapters == nil {
		return nil
	}
	result := make(map[string]core.AdapterConfig, len(adapters))
	for id, config := range adapters {
		result[id] = cloneAdapterConfig(config)
	}
	return result
}

func cloneAdapterConfig(config core.AdapterConfig) core.AdapterConfig {
	clone := core.AdapterConfig{
		Root:        config.Root,
		Enabled:     cloneBoolPointer(config.Enabled),
		Categories:  make(map[string]core.CategoryConfig, len(config.Categories)),
		Ignore:      cloneStringSlice(config.Ignore),
		AllowEscape: cloneStringSlice(config.AllowEscape),
	}
	for name, category := range config.Categories {
		categoryCopy := category
		categoryCopy.Paths = cloneStringSlice(category.Paths)
		categoryCopy.Kind = cloneKindPointer(category.Kind)
		categoryCopy.Enabled = cloneBoolPointer(category.Enabled)
		categoryCopy.Exclude = cloneStringSlice(category.Exclude)
		categoryCopy.ExcludeKeys = cloneStringSlice(category.ExcludeKeys)
		clone.Categories[name] = categoryCopy
	}
	return clone
}

func cloneBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneKindPointer(value *core.CategoryKind) *core.CategoryKind {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneBackupConfig(value *core.BackupConfig) *core.BackupConfig {
	if value == nil {
		return nil
	}
	return &core.BackupConfig{Keep: cloneIntPointer(value.Keep)}
}

func cloneSecretsConfig(value *core.SecretsConfig) *core.SecretsConfig {
	if value == nil {
		return nil
	}
	clone := &core.SecretsConfig{
		IgnorePaths: cloneStringSlice(value.IgnorePaths),
		Recipients:  cloneStringSlice(value.Recipients),
	}
	if value.Files != nil {
		clone.Files = make(map[string]string, len(value.Files))
		for name, path := range value.Files {
			clone.Files[name] = path
		}
	}
	return clone
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneStringSlice(value []string) []string {
	if value == nil {
		return nil
	}
	clone := make([]string, len(value))
	copy(clone, value)
	return clone
}

// initializeRemote is the opt-in init one-liner: create the repository,
// configure origin, commit the complete configuration center, persist the
// local sync state, and establish the upstream with one push.
func initializeRemote(paths core.HomerPaths, remote string) error {
	if err := gitx.EnsureGitRepo(paths.Home); err != nil {
		return err
	}
	if err := gitx.AddOrSetRemote(paths.Home, remote); err != nil {
		return err
	}
	commit := gitx.CommitAllStore(paths.Home, "homer init: 建立同步基线")
	if commit == "" && !gitx.IsPushClean(paths.Home) {
		return core.NewCliError(fmt.Sprintf("已配置 origin，但初始 git commit 失败: %s", paths.Home))
	}
	if commit == "" {
		commit = gitx.HeadCommit(paths.Home)
	}
	if commit == "" {
		return core.NewCliError("无法确定初始同步基线 commit；请检查 git user.name / user.email 后重试 `homer push --yes`。")
	}
	if err := nowState(paths, commit, "push"); err != nil {
		return err
	}
	if pushed := gitx.Push(paths.Home); !pushed.OK {
		return core.NewCliError(fmt.Sprintf(
			"初始同步基线已提交但未推送到 origin: %s\n请运行 `%s`。",
			firstLine(pushed.Stderr), gitx.PushHint(paths.Home),
		))
	}
	return nil
}

// RunInitWithDefaults is a convenience spelling for callers that do not need
// the force option.
func RunInitWithDefaults(opts InitOptions) (InitReport, error) {
	return RunInit(opts, InitRunOptions{})
}

func runInit(opts InitOptions, runOptions ...InitRunOptions) (InitReport, error) {
	return RunInit(opts, runOptions...)
}
