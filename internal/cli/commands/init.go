package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/adapter/herdr"
	"github.com/zzjcool/homer-cli/internal/adapter/opencode"
	"github.com/zzjcool/homer-cli/internal/adapter/pi"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

// InitOptions controls the non-interactive scan performed by homer init.
type InitOptions struct {
	HomerHome string
	// Home is a compatibility alias for callers that use the flag spelling.
	Home     string
	Adapters []string
	JSON     bool
	Force    bool
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
}

// KNOWN_ADAPTERS is the P3 registration table.  The map is exported for
// focused tests and callers that need to inspect the built-in set; iteration
// order is controlled by knownAdapterOrder below.
var KNOWN_ADAPTERS = map[string]core.AdapterConfig{
	pi.PIAdapterID:             pi.DefaultPIAdapter,
	herdr.HerdrAdapterID:       herdr.DefaultHerdrAdapter,
	opencode.OpencodeAdapterID: opencode.DefaultOpencodeAdapter,
}

// KnownAdapters is the idiomatic alias for KNOWN_ADAPTERS.
var KnownAdapters = KNOWN_ADAPTERS

var knownAdapterOrder = []string{pi.PIAdapterID, herdr.HerdrAdapterID, opencode.OpencodeAdapterID}

const INIT_USAGE = `用法: homer init [options]

扫描已启用的 adapter，生成 homer.json 并把初始快照写入 store。

选项:
  --home <dir>          homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --adapters <ids>      只初始化指定 adapter（逗号分隔，可重复）
  --force               覆盖已存在的 homer.json
  --json                输出机器可读 JSON（InitReport）
  -h, --help            显示本帮助

注意: 已存在 homer.json 时会拒绝覆盖，除非显式给出 --force。`

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
			return nil, core.NewCliError(fmt.Sprintf("未知 adapter: %s；M1/P3 已知 adapter: pi, herdr, opencode", id))
		}
		selected[id] = config
	}
	return selected, nil
}

func selectedAdapterIDs(selected map[string]core.AdapterConfig) []string {
	ids := make([]string, 0, len(selected))
	for _, id := range knownAdapterOrder {
		if _, ok := selected[id]; ok {
			ids = append(ids, id)
		}
	}
	return ids
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
	runOpts := InitRunOptions{Force: opts.Force}
	if len(runOptions) > 0 {
		runOpts = runOptions[0]
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

	if _, err := os.Stat(paths.ConfigFile); err == nil && !runOpts.Force {
		return InitReport{}, core.NewCliError(fmt.Sprintf("已存在 homer 配置: %s；拒绝覆盖。如需重新初始化，请加 `--force`（会覆盖 homer.json 与 store 快照）。", paths.ConfigFile))
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return InitReport{}, err
	}

	selected, err := selectedAdapters(opts.Adapters)
	if err != nil {
		return InitReport{}, err
	}

	report := InitReport{
		HomerHome: paths.Home,
		Adapters:  make([]InitAdapterReport, 0, len(selected)),
		Errors:    []string{},
	}
	scan := deps.Scan
	if scan == nil {
		scan = adapter.ScanAdapter
	}
	writeSnapshot := deps.WriteSnapshot
	if writeSnapshot == nil {
		writeSnapshot = core.WriteSnapshotToStore
	}
	saveConfig := deps.SaveConfig
	if saveConfig == nil {
		saveConfig = saveInitConfig
	}

	for _, adapterID := range selectedAdapterIDs(selected) {
		adapterConfig := selected[adapterID]
		outcome := scan(adapterID, adapterConfig)
		if err := writeSnapshot(paths, outcome.Snapshot); err != nil {
			return InitReport{}, err
		}

		if len(outcome.Errors) > 0 {
			report.Errors = append(report.Errors, sourceErrorMessages(SnapshotSourceErrors{
				{
					AdapterID:      adapterID,
					RootUnreadable: rootUnreadable(outcome),
					Errors:         append([]adapter.ScanError(nil), outcome.Errors...),
				},
			})...)
		}

		adapterReport := InitAdapterReport{
			ID:         adapterID,
			Categories: make([]InitCategoryReport, 0, len(outcome.Snapshot.Categories)),
		}
		for _, category := range outcome.Snapshot.Categories {
			adapterReport.Categories = append(adapterReport.Categories, InitCategoryReport{
				Name:      category.Category,
				FileCount: len(category.Files),
			})
		}
		report.Adapters = append(report.Adapters, adapterReport)
	}

	config := core.HomerConfig{Version: 1, Adapters: selected}
	if err := saveConfig(paths, config); err != nil {
		return InitReport{}, err
	}
	return report, nil
}

// RunInitWithDefaults is a convenience spelling for callers that do not need
// the force option.
func RunInitWithDefaults(opts InitOptions) (InitReport, error) {
	return RunInit(opts, InitRunOptions{})
}

func runInit(opts InitOptions, runOptions ...InitRunOptions) (InitReport, error) {
	return RunInit(opts, runOptions...)
}
