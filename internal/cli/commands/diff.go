package commands

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/engine"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

// DiffOptions controls the read-only detailed diff command.
type DiffOptions struct {
	HomerHome string
	// Home is a compatibility alias for callers that use the CLI flag name.
	Home     string
	Adapter  string
	Category string
}

const DIFF_USAGE = "用法: homer diff [options]\n\n显示漂移的详细差异：\n  - merge 文件：按键行输出 `key: old → new`\n  - mirror 文件：按行输出 `+` / `-`（LCS diff）\n\n选项:\n  --home <dir>       homer 工作区（默认 $HOMER_HOME 或 ~/.homer）\n  --adapter <id>     只看某个 adapter\n  --category <name>  只看某个分类\n  -h, --help         显示本帮助\n\n无漂移时输出为空（exit 0）。"

// DiffUsage is the idiomatic alias for the archived constant spelling.
const DiffUsage = DIFF_USAGE

type jsonSlot struct {
	has   bool
	value orderedjson.Value
}

type keyRow struct {
	path      string
	before    orderedjson.Value
	after     orderedjson.Value
	beforeHas bool
	afterHas  bool
}

func entryPointer(files core.SnapshotFiles, path string) *core.SnapshotEntry {
	entry, ok := files[path]
	if !ok {
		return nil
	}
	copy := entry
	return &copy
}

func findFiles(snapshots []core.AdapterSnapshot, adapterID, category string) core.SnapshotFiles {
	for _, snapshot := range snapshots {
		if snapshot.AdapterID != adapterID {
			continue
		}
		for _, item := range snapshot.Categories {
			if item.Category == category {
				if item.Files == nil {
					return core.SnapshotFiles{}
				}
				return item.Files
			}
		}
	}
	return core.SnapshotFiles{}
}

// slotOf returns valid=false for a declared JSON entry whose bytes cannot be
// parsed.  The caller then follows the engine's degraded-file path instead of
// inventing a key-level diff for malformed content.
func slotOf(entry *core.SnapshotEntry) (*jsonSlot, bool) {
	if entry == nil {
		return &jsonSlot{has: false}, true
	}
	if entry.Kind != "json" {
		return nil, false
	}
	value, err := orderedjson.Parse([]byte(entry.Content))
	if err != nil {
		return nil, false
	}
	return &jsonSlot{has: true, value: value}, true
}

func isJSONEntry(entry *core.SnapshotEntry) bool {
	return entry == nil || entry.Kind == "json"
}

func objectValue(value orderedjson.Value) (*orderedjson.Object, bool) {
	object, ok := value.(*orderedjson.Object)
	if !ok || object == nil {
		return nil, false
	}
	return object, true
}

func objectKeys(object *orderedjson.Object) []string {
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

func childSlot(parent *jsonSlot, key string) *jsonSlot {
	if parent == nil || !parent.has {
		return &jsonSlot{has: false}
	}
	object, ok := objectValue(parent.value)
	if !ok {
		return &jsonSlot{has: false}
	}
	value, ok := object.M[key]
	if !ok {
		return &jsonSlot{has: false}
	}
	return &jsonSlot{has: true, value: value}
}

func slotsEqual(a, b *jsonSlot) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.has != b.has {
		return false
	}
	if !a.has {
		return true
	}
	return orderedjson.DeepEqual(a.value, b.value)
}

func collectKeyDiffs(base, local *jsonSlot, prefix string, out *[]keyRow) {
	if base == nil || local == nil || (!base.has && !local.has) {
		return
	}
	baseObject, baseIsObject := objectValue(base.value)
	localObject, localIsObject := objectValue(local.value)
	if base.has && local.has && baseIsObject && localIsObject {
		seen := make(map[string]struct{})
		keys := make([]string, 0, len(baseObject.M)+len(localObject.M))
		for _, key := range append(objectKeys(baseObject), objectKeys(localObject)...) {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			collectKeyDiffs(childSlot(base, key), childSlot(local, key), path, out)
		}
		return
	}
	if slotsEqual(base, local) {
		return
	}
	path := prefix
	if path == "" {
		path = "$"
	}
	row := keyRow{path: path, beforeHas: base.has, afterHas: local.has}
	if base.has {
		row.before = base.value
	}
	if local.has {
		row.after = local.value
	}
	*out = append(*out, row)
}

func formatValueMissing(has bool, value orderedjson.Value) string {
	if !has {
		return "(无)"
	}
	if text, ok := value.(string); ok {
		return text
	}
	return compactJSON(value)
}

// compactJSON is the renderer-only compact counterpart to orderedjson's
// pretty file serializer.  It intentionally keeps ordered object keys and
// delegates string/number escaping/formatting to orderedjson for JS parity.
func compactJSON(value orderedjson.Value) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case *orderedjson.Object:
		if typed == nil || len(typed.M) == 0 {
			return "{}"
		}
		keys := objectKeys(typed)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			keyText := string(orderedjson.Serialize(key))
			parts = append(parts, keyText+":"+compactJSON(typed.M[key]))
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

func splitLines(text string) []string {
	if text == "" {
		return []string{}
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// diffLines is an LCS line diff with the exact +/-/context prefixes used by
// the TypeScript renderer.
func diffLines(before, after string) []string {
	if before == after {
		return []string{}
	}
	a := splitLines(before)
	b := splitLines(after)
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
		switch {
		case a[i] == b[j]:
			result = append(result, " "+a[i])
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			result = append(result, "-"+a[i])
			i++
		default:
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

func conflictMark(conflict bool) string {
	if conflict {
		return "⚡ "
	}
	return ""
}

func renderFileDiff(out *[]string, relPath string, base, local, remote *core.SnapshotEntry, conflict bool) bool {
	lines := []string{}
	switch {
	case base == nil && local != nil:
		lines = diffLines("", local.Content)
	case base != nil && local == nil:
		lines = diffLines(base.Content, "")
	case base != nil && local != nil && base.Content != local.Content:
		lines = diffLines(base.Content, local.Content)
	}

	remoteChanged := false
	if base == nil {
		remoteChanged = remote != nil
	} else {
		remoteChanged = remote == nil || remote.Content != base.Content
	}
	remoteLines := []string{}
	if remoteChanged {
		before := ""
		if base != nil {
			before = base.Content
		}
		after := ""
		if remote != nil {
			after = remote.Content
		}
		remoteLines = diffLines(before, after)
	}
	if len(lines) == 0 && len(remoteLines) == 0 {
		return false
	}
	*out = append(*out, "  "+conflictMark(conflict)+relPath)
	*out = append(*out, lines...)
	if len(remoteLines) > 0 {
		*out = append(*out, "    远端(↓)")
		*out = append(*out, remoteLines...)
	}
	return true
}

func renderMirrorCategory(out *[]string, adapterID string, drift engine.CategoryDrift, base, local, remote core.SnapshotFiles) {
	headerWritten := false
	for _, op := range drift.Ops {
		if op.Type == "noop" {
			continue
		}
		if !headerWritten {
			*out = append(*out, adapterID+"/"+drift.Category)
			headerWritten = true
		}
		renderFileDiff(out, op.Path, entryPointer(base, op.Path), entryPointer(local, op.Path), entryPointer(remote, op.Path), op.Type == "conflict")
	}
}

func renderMergeCategory(out *[]string, adapterID string, drift engine.CategoryDrift, base, local, remote core.SnapshotFiles) {
	degraded := make(map[string]struct{})
	for _, op := range drift.Ops {
		if op.Type != "noop" {
			degraded[op.Path] = struct{}{}
		}
	}
	conflictKeys := make(map[string]struct{}, len(drift.MergeConflicts))
	for _, conflict := range drift.MergeConflicts {
		conflictKeys[conflict.KeyPath] = struct{}{}
	}
	conflictOpPaths := make(map[string]struct{})
	for _, op := range drift.Ops {
		if op.Type == "conflict" {
			conflictOpPaths[op.Path] = struct{}{}
		}
	}
	fileConflict := func(path string) bool {
		_, key := conflictKeys[path]
		_, op := conflictOpPaths[path]
		return key || op
	}
	keyConflict := func(path string) bool {
		_, ok := conflictKeys[path]
		return ok
	}

	headerWritten := false
	head := func() {
		if !headerWritten {
			*out = append(*out, adapterID+"/"+drift.Category)
			headerWritten = true
		}
	}
	for _, op := range drift.Ops {
		if op.Type == "noop" {
			continue
		}
		head()
		renderFileDiff(out, op.Path, entryPointer(base, op.Path), entryPointer(local, op.Path), entryPointer(remote, op.Path), op.Type == "conflict")
	}

	paths := make(map[string]struct{}, len(base)+len(local)+len(remote))
	for path := range base {
		paths[path] = struct{}{}
	}
	for path := range local {
		paths[path] = struct{}{}
	}
	for path := range remote {
		paths[path] = struct{}{}
	}
	orderedPaths := make([]string, 0, len(paths))
	for path := range paths {
		orderedPaths = append(orderedPaths, path)
	}
	sort.Strings(orderedPaths)
	for _, relPath := range orderedPaths {
		if _, ok := degraded[relPath]; ok {
			continue
		}
		baseEntry := entryPointer(base, relPath)
		localEntry := entryPointer(local, relPath)
		remoteEntry := entryPointer(remote, relPath)
		if !isJSONEntry(baseEntry) || !isJSONEntry(localEntry) || !isJSONEntry(remoteEntry) {
			continue
		}
		baseSlot, baseOK := slotOf(baseEntry)
		localSlot, localOK := slotOf(localEntry)
		remoteSlot, remoteOK := slotOf(remoteEntry)
		if !baseOK || !localOK || !remoteOK {
			continue
		}
		rows := make([]keyRow, 0)
		collectKeyDiffs(baseSlot, localSlot, "", &rows)
		remoteRows := make([]keyRow, 0)
		remoteChanged := false
		if baseEntry == nil {
			remoteChanged = remoteEntry != nil
		} else {
			remoteChanged = remoteEntry == nil || remoteEntry.Content != baseEntry.Content
		}
		if remoteChanged {
			collectKeyDiffs(baseSlot, remoteSlot, "", &remoteRows)
		}
		if len(rows) == 0 && len(remoteRows) == 0 {
			continue
		}
		head()
		*out = append(*out, "  "+conflictMark(fileConflict(relPath))+relPath)
		for _, row := range rows {
			*out = append(*out, "    "+conflictMark(keyConflict(row.path))+row.path+": "+formatValueMissing(row.beforeHas, row.before)+" → "+formatValueMissing(row.afterHas, row.after))
		}
		for _, row := range remoteRows {
			*out = append(*out, "    ↓ "+conflictMark(keyConflict(row.path))+row.path+": "+formatValueMissing(row.beforeHas, row.before)+" → "+formatValueMissing(row.afterHas, row.after))
		}
	}
}

func emptyDrift(drift engine.CategoryDrift) bool {
	return drift.Push == 0 && drift.Pull == 0 && drift.Conflicts == 0
}

// RunDiff returns the detailed diff text.  Empty text is the successful
// no-drift result; callers should not add a placeholder line.
func RunDiff(opts DiffOptions, injected ...DriftSources) (string, error) {
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
			return "", core.NewCliError(fmt.Sprintf("未找到 homer 配置: %s；请先运行 `homer init` 生成 homer.json 与 store 快照。", paths.ConfigFile))
		}
		return "", err
	}

	var sources DriftSources
	if provided, ok := sourceFromArgs(injected); ok {
		sources = provided
	} else {
		sources, err = CollectSnapshotSources(paths, config)
		if err != nil {
			return "", err
		}
	}

	drifts := engine.ComputeDrift(sources.Base, sources.Local, sources.Remote)
	remoteSnapshots := sources.Remote
	if remoteSnapshots == nil {
		remoteSnapshots = sources.Base
	}
	out := make([]string, 0)
	errorMessages := append([]string(nil), sources.Errors...)
	errorMessages = append(errorMessages, sources.ErrorMessages...)
	errorMessages = append(errorMessages, sourceErrorMessages(sources.ScanErrors)...)
	for _, message := range errorMessages {
		out = append(out, "⚠ "+message)
	}
	for _, warning := range sources.Warnings {
		out = append(out, "⚠ "+warning)
	}

	for _, drift := range drifts {
		if opts.Adapter != "" && opts.Adapter != drift.AdapterID {
			continue
		}
		if emptyDrift(drift) {
			continue
		}
		if opts.Category != "" && opts.Category != drift.Category {
			continue
		}
		base := findFiles(sources.Base, drift.AdapterID, drift.Category)
		local := findFiles(sources.Local, drift.AdapterID, drift.Category)
		remote := findFiles(remoteSnapshots, drift.AdapterID, drift.Category)
		if drift.Mode == core.SyncModeMirror {
			renderMirrorCategory(&out, drift.AdapterID, drift, base, local, remote)
		} else {
			renderMergeCategory(&out, drift.AdapterID, drift, base, local, remote)
		}
	}
	return strings.Join(out, "\n"), nil
}

func runDiff(opts DiffOptions, injected ...DriftSources) (string, error) {
	return RunDiff(opts, injected...)
}
