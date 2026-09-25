package cli

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

// Keep command result types available from the cli package as well as the
// commands subpackage.  The aliases avoid duplicate JSON contracts and make
// the renderer's public surface pleasant for package-level tests.
type StatusReport = commands.StatusReport
type StatusAdapterReport = commands.StatusAdapterReport
type StatusCategoryReport = commands.StatusCategoryReport
type InitReport = commands.InitReport
type InitAdapterReport = commands.InitAdapterReport
type InitCategoryReport = commands.InitCategoryReport
type CliDriftSources = commands.DriftSources
type SnapshotSourceError = commands.SnapshotSourceError
type SnapshotSourceErrors = commands.SnapshotSourceErrors
type CliError = core.CliError

// NewCliError preserves the renderer-level compatibility constructor while
// the actual type lives in core to avoid a dependency cycle.
var NewCliError = core.NewCliError

type RenderStatusOptions struct {
	Verbose bool
}

// ResolveHomerPaths is the renderer-facing path helper from the archived
// TypeScript module.  An omitted or empty argument follows HOMER_HOME/default
// resolution; a non-empty argument wins explicitly.
func ResolveHomerPaths(homerHome ...string) core.HomerPaths {
	value := ""
	if len(homerHome) > 0 {
		value = homerHome[0]
	}
	return core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" && value != "" {
			return value
		}
		return os.Getenv(key)
	})
}

// CollectSnapshotSources exposes the command collector at the renderer seam
// without making callers depend on the subpackage layout.
func CollectSnapshotSources(paths core.HomerPaths, config *core.HomerConfig) (CliDriftSources, error) {
	return commands.CollectSnapshotSources(paths, config)
}

// FormatScanError renders one scanner diagnostic as path + reason.
func FormatScanError(errorValue adapter.ScanError) string {
	return errorValue.Path + ": " + errorValue.Message
}

// SourceErrorMessages is the public renderer spelling for structured scan
// diagnostics.
func SourceErrorMessages(errors SnapshotSourceErrors) []string {
	return commands.SourceErrorMessages(errors)
}

// renderDisabledStatus keeps the machine-facing paths in StatusReport while
// giving the human renderer a compact per-adapter summary.  A disabled
// adapter is already a complete summary; category paths are grouped by their
// adapter so the common case reads, for example, "pi: 2 类未启用（extensions,
// agents）".  Verbose mode deliberately switches to one path per line.
func renderDisabledStatus(disabled []string, verbose bool) []string {
	if len(disabled) == 0 {
		return nil
	}
	if verbose {
		lines := make([]string, 0, len(disabled))
		for _, item := range disabled {
			lines = append(lines, "  "+item)
		}
		return lines
	}

	type disabledGroup struct {
		adapterDisabled bool
		categories      []string
	}
	groups := make(map[string]*disabledGroup)
	order := make([]string, 0, len(disabled))
	for _, item := range disabled {
		adapterID, category, hasCategory := strings.Cut(item, "/")
		if adapterID == "" {
			continue
		}
		group, ok := groups[adapterID]
		if !ok {
			group = &disabledGroup{}
			groups[adapterID] = group
			order = append(order, adapterID)
		}
		if !hasCategory || category == "" {
			group.adapterDisabled = true
			continue
		}
		group.categories = append(group.categories, category)
	}

	lines := make([]string, 0, len(order))
	for _, adapterID := range order {
		group := groups[adapterID]
		if group.adapterDisabled {
			lines = append(lines, adapterID+": adapter 未启用")
			continue
		}
		if len(group.categories) == 0 {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %d 类未启用（%s）", adapterID, len(group.categories), strings.Join(group.categories, ", ")))
	}
	return lines
}

// RenderStatus prints warnings first, then adapter totals and optional
// category totals.  Drift is informational: all-zero output ends with the
// same Chinese no-drift marker as the archived CLI.
func RenderStatus(report StatusReport, options ...RenderStatusOptions) string {
	verbose := false
	if len(options) > 0 {
		verbose = options[0].Verbose
	}

	lines := make([]string, 0, len(report.Errors)+len(report.Warnings)+len(report.Adapters)+1)
	for _, message := range report.Errors {
		lines = append(lines, "⚠ "+message)
	}
	for _, message := range report.Warnings {
		lines = append(lines, "⚠ "+message)
	}
	if len(report.Adapters) == 0 {
		lines = append(lines, "（homer.json 中没有启用的 adapter）")
		lines = append(lines, renderDisabledStatus(report.Disabled, verbose)...)
		return strings.Join(lines, "\n")
	}

	totalPush, totalPull, totalConflicts := 0, 0, 0
	for _, adapter := range report.Adapters {
		totalPush += adapter.Push
		totalPull += adapter.Pull
		totalConflicts += adapter.Conflicts
		line := adapter.ID + "  ↑" + strconv.Itoa(adapter.Push) + " ↓" + strconv.Itoa(adapter.Pull)
		if adapter.Conflicts > 0 {
			line += "  冲突" + strconv.Itoa(adapter.Conflicts)
		}
		lines = append(lines, line)
		if verbose {
			for _, category := range adapter.Categories {
				line := "  " + category.Name + "  ↑" + strconv.Itoa(category.Push) + " ↓" + strconv.Itoa(category.Pull)
				if category.Conflicts > 0 {
					line += "  冲突" + strconv.Itoa(category.Conflicts)
				}
				lines = append(lines, line)
			}
		}
	}
	lines = append(lines, renderDisabledStatus(report.Disabled, verbose)...)
	if totalPush == 0 && totalPull == 0 && totalConflicts == 0 {
		lines = append(lines, "无漂移")
	}
	return strings.Join(lines, "\n")
}

// RenderInit prints the workspace and file counts.  Scan diagnostics remain
// visible even when an adapter root is absent; a missing tool must not look
// like a successful empty scan.
func RenderInit(report InitReport) string {
	lines := []string{"homer init: " + report.HomerHome}
	for _, message := range report.Errors {
		lines = append(lines, "  ⚠ "+message)
	}
	if len(report.Adapters) == 0 {
		lines = append(lines, "（没有要初始化的 adapter）")
		return strings.Join(lines, "\n")
	}
	for _, adapter := range report.Adapters {
		total := 0
		for _, category := range adapter.Categories {
			total += category.FileCount
		}
		lines = append(lines, "  "+adapter.ID+": "+strconv.Itoa(total)+" 个文件")
		for _, category := range adapter.Categories {
			lines = append(lines, "    "+category.Name+": "+strconv.Itoa(category.FileCount))
		}
	}
	return strings.Join(lines, "\n")
}

// FormatValue is used for merge key rows.  Strings intentionally do not get
// quotes; all other JSON values use compact, ordered JSON.
func FormatValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		return typed
	case *orderedjson.Object:
		return compactJSON(typed)
	case []orderedjson.Value:
		return compactJSON(typed)
	case bool:
		return strconv.FormatBool(typed)
	case fmt.Stringer:
		// orderedjson parses numbers as json.Number.  Reparse through the
		// ordered boundary so JS number normalization (for example 1.0 -> 1)
		// remains identical to JSON.stringify rather than leaking the source
		// spelling into a human diff.
		if parsed, err := orderedjson.Parse([]byte(typed.String())); err == nil {
			return compactJSON(parsed)
		}
		return typed.String()
	case int:
		return strconv.Itoa(typed)
	case int8:
		return strconv.FormatInt(int64(typed), 10)
	case int16:
		return strconv.FormatInt(int64(typed), 10)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	case int64:
		return strconv.FormatInt(typed, 10)
	case uint:
		return strconv.FormatUint(uint64(typed), 10)
	case uint8:
		return strconv.FormatUint(uint64(typed), 10)
	case uint16:
		return strconv.FormatUint(uint64(typed), 10)
	case uint32:
		return strconv.FormatUint(uint64(typed), 10)
	case uint64:
		return strconv.FormatUint(typed, 10)
	case float32:
		return strconv.FormatFloat(float64(typed), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	default:
		return fmt.Sprint(value)
	}
}

func orderedObjectKeys(object *orderedjson.Object) []string {
	if object == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(object.Keys))
	keys := make([]string, 0, len(object.Keys))
	for _, key := range object.Keys {
		if _, ok := object.M[key]; !ok {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	extra := make([]string, 0)
	for key := range object.M {
		if _, ok := seen[key]; !ok {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	return append(keys, extra...)
}

func compactJSON(value orderedjson.Value) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case *orderedjson.Object:
		if typed == nil || len(typed.M) == 0 {
			return "{}"
		}
		parts := make([]string, 0, len(typed.M))
		for _, key := range orderedObjectKeys(typed) {
			parts = append(parts, string(orderedjson.Serialize(key))+":"+compactJSON(typed.M[key]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []orderedjson.Value:
		parts := make([]string, len(typed))
		for i, item := range typed {
			parts[i] = compactJSON(item)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case string:
		return string(orderedjson.Serialize(typed))
	case bool:
		return strconv.FormatBool(typed)
	default:
		return string(orderedjson.Serialize(typed))
	}
}

// SplitLines and DiffLines are pure, dependency-free helpers used by diff and
// directly useful to renderer tests.
func SplitLines(text string) []string {
	if text == "" {
		return []string{}
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func DiffLines(before, after string) []string {
	if before == after {
		return []string{}
	}
	a := SplitLines(before)
	b := SplitLines(after)
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	result := make([]string, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			result = append(result, " "+a[i])
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			result = append(result, "-"+a[i])
			i++
		} else {
			result = append(result, "+"+b[j])
			j++
		}
	}
	for i < len(a) {
		result = append(result, "-"+a[i])
		i++
	}
	for j < len(b) {
		result = append(result, "+"+b[j])
		j++
	}
	return result
}

func stringsJSON(values []string) []orderedjson.Value {
	result := make([]orderedjson.Value, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
}

func orderedNumber(text string) orderedjson.Value {
	value, err := orderedjson.Parse([]byte(text))
	if err != nil {
		return text
	}
	return value
}

func orderedObject(keys []string, values map[string]orderedjson.Value) orderedjson.Value {
	return &orderedjson.Object{Keys: keys, M: values}
}

// RenderStatusJSON serializes StatusReport through orderedjson rather than
// encoding/json maps, preserving the stable field order of the TS output.
func RenderStatusJSON(report StatusReport) string {
	adapters := make([]orderedjson.Value, 0, len(report.Adapters))
	for _, adapter := range report.Adapters {
		categories := make([]orderedjson.Value, 0, len(adapter.Categories))
		for _, category := range adapter.Categories {
			categories = append(categories, orderedObject(
				[]string{"name", "push", "pull", "conflicts"},
				map[string]orderedjson.Value{
					"name": category.Name, "push": orderedNumber(strconv.Itoa(category.Push)), "pull": orderedNumber(strconv.Itoa(category.Pull)), "conflicts": orderedNumber(strconv.Itoa(category.Conflicts)),
				},
			))
		}
		adapters = append(adapters, orderedObject(
			[]string{"id", "push", "pull", "conflicts", "categories"},
			map[string]orderedjson.Value{
				"id": adapter.ID, "push": orderedNumber(strconv.Itoa(adapter.Push)), "pull": orderedNumber(strconv.Itoa(adapter.Pull)), "conflicts": orderedNumber(strconv.Itoa(adapter.Conflicts)), "categories": categories,
			},
		))
	}
	keys := []string{"errors", "adapters"}
	values := map[string]orderedjson.Value{
		"errors": stringsJSON(report.Errors), "adapters": adapters,
	}
	if len(report.Warnings) > 0 {
		keys = append(keys, "warnings")
		values["warnings"] = stringsJSON(report.Warnings)
	}
	if len(report.Disabled) > 0 {
		keys = append(keys, "disabled")
		values["disabled"] = stringsJSON(report.Disabled)
	}
	return string(orderedjson.Serialize(orderedObject(keys, values)))
}

// RenderInitJSON serializes InitReport through the same ordered boundary.
func RenderInitJSON(report InitReport) string {
	adapters := make([]orderedjson.Value, 0, len(report.Adapters))
	for _, adapter := range report.Adapters {
		categories := make([]orderedjson.Value, 0, len(adapter.Categories))
		for _, category := range adapter.Categories {
			categories = append(categories, orderedObject(
				[]string{"name", "fileCount"},
				map[string]orderedjson.Value{"name": category.Name, "fileCount": orderedNumber(strconv.Itoa(category.FileCount))},
			))
		}
		adapters = append(adapters, orderedObject(
			[]string{"id", "categories"},
			map[string]orderedjson.Value{"id": adapter.ID, "categories": categories},
		))
	}
	return string(orderedjson.Serialize(orderedObject(
		[]string{"homerHome", "adapters", "errors"},
		map[string]orderedjson.Value{"homerHome": report.HomerHome, "adapters": adapters, "errors": stringsJSON(report.Errors)},
	)))
}

// Lower-case aliases keep package-local tests that mirror the archived
// TypeScript helper names concise while the exported functions remain the
// supported cross-package API.
func renderStatus(report StatusReport, options ...RenderStatusOptions) string {
	return RenderStatus(report, options...)
}

func renderInit(report InitReport) string { return RenderInit(report) }

func formatValue(value any) string { return FormatValue(value) }

func splitLines(text string) []string { return SplitLines(text) }

func diffLines(before, after string) []string { return DiffLines(before, after) }

func resolveHomerPaths(homerHome ...string) core.HomerPaths {
	return ResolveHomerPaths(homerHome...)
}
