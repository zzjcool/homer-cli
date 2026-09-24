package adapter

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/core"
)

// ScanError is a non-fatal diagnostic emitted while collecting a snapshot.
// A missing configured file is not an error; errors are reserved for an
// unreadable root/path, a failed directory read, or a denied symlink escape.
type ScanError struct {
	Path    string
	Message string
}

// ScanOutcome contains the snapshot and diagnostics from one adapter scan.
type ScanOutcome struct {
	Snapshot core.AdapterSnapshot
	Errors   []ScanError
}

const maxWalkDepth = 32

// scanState is per category.  A category has its own visited set so that a
// directory declared by two categories is independently represented in each
// category, while a symlink loop inside one category is cut off.
type scanState struct {
	Ignore      []string
	Exclude     []string
	AllowEscape []string
	Errors      *[]ScanError
	Files       *[]string
	RootReal    string
	RootRelBase string
	Visited     map[string]struct{}
}

func expandHome(input string) string {
	if input != "~" && !strings.HasPrefix(input, "~/") && !strings.HasPrefix(input, `~\`) {
		return input
	}

	home := os.Getenv("HOME")
	if home == "" {
		if resolved, err := os.UserHomeDir(); err == nil {
			home = resolved
		}
	}
	if home == "" {
		return input
	}
	if input == "~" {
		return home
	}
	return filepath.Join(home, filepath.FromSlash(input[2:]))
}

func isDirPath(path string) bool { return strings.HasSuffix(path, "/") }

func normalizeRel(path string) string {
	return filepath.ToSlash(path)
}

func basenameOf(rel string) string {
	rel = normalizeRel(rel)
	if idx := strings.LastIndexByte(rel, '/'); idx >= 0 {
		return rel[idx+1:]
	}
	return rel
}

func joinRel(rootRelBase, rel string) string {
	rootRelBase = strings.Trim(rootRelBase, "/")
	rel = strings.Trim(rel, "/")
	switch {
	case rootRelBase == "":
		return rel
	case rel == "":
		return rootRelBase
	default:
		return rootRelBase + "/" + rel
	}
}

func isIgnored(rel string, ignore []string) bool {
	return MatchesIgnore(normalizeRel(rel), ignore)
}

// isExcluded preserves the TypeScript category-exclude rule: a pattern is
// tried against the whole category-relative path, its basename, and every
// path segment.  This makes *cache* prune both cache-like files and entire
// cache-like directories while adapter-level ignore remains path-strict.
func isExcluded(rel string, exclude []string) bool {
	if len(exclude) == 0 {
		return false
	}
	rel = normalizeRel(rel)
	if MatchesIgnore(rel, exclude) || MatchesIgnore(basenameOf(rel), exclude) {
		return true
	}
	for _, segment := range strings.Split(rel, "/") {
		if segment != "" && MatchesIgnore(segment, exclude) {
			return true
		}
	}
	return false
}

func isEscapeAllowed(rootRel string, allowEscape []string) bool {
	for _, pattern := range allowEscape {
		// Config validation owns the user-facing error, but the scanner also
		// keeps an unsafe value fail-closed when called directly (for example
		// by an embedding caller that constructed AdapterConfig by hand).
		if IsBareGlob(pattern) {
			continue
		}
		if GlobMatch(rootRel, pattern) {
			return true
		}
	}
	return false
}

func tryRealpath(path string) (string, bool) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	return real, true
}

func isWithinRoot(child, root string) bool {
	child = filepath.Clean(child)
	root = filepath.Clean(root)
	if child == root {
		return true
	}
	// Keep an existing volume/root separator (for example "/" or "C:\\")
	// instead of appending a second one.
	prefix := root
	if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
		prefix += string(os.PathSeparator)
	}
	return strings.HasPrefix(child, prefix)
}

func addScanError(errors *[]ScanError, path, message string) {
	*errors = append(*errors, ScanError{Path: path, Message: message})
}

// visitSymlink applies the containment policy before following a symlink.
// Dangling links are intentionally silent.  A denied escape is diagnosable,
// but never contributes target contents to the snapshot.
func visitSymlink(abs, rel string, depth int, state *scanState) {
	real, ok := tryRealpath(abs)
	if !ok {
		return
	}

	rootRel := joinRel(state.RootRelBase, normalizeRel(rel))
	if !isWithinRoot(real, state.RootReal) && !isEscapeAllowed(rootRel, state.AllowEscape) {
		addScanError(state.Errors, abs, fmt.Sprintf("symlink 逃逸 adapter root: %s", real))
		return
	}

	info, err := os.Stat(real)
	if err != nil {
		// A target can disappear between realpath and stat.  Treat it as a
		// dangling path, matching the scan's best-effort semantics.
		return
	}
	if info.IsDir() {
		walk(abs, rel, depth+1, state)
	} else if info.Mode().IsRegular() {
		*state.Files = append(*state.Files, normalizeRel(rel))
	}
}

func walk(absBase, relBase string, depth int, state *scanState) {
	if depth > maxWalkDepth {
		return
	}

	realBase, ok := tryRealpath(absBase)
	if !ok {
		return
	}
	if _, seen := state.Visited[realBase]; seen {
		return
	}
	state.Visited[realBase] = struct{}{}

	entries, err := os.ReadDir(absBase)
	if err != nil {
		addScanError(state.Errors, absBase, err.Error())
		return
	}

	for _, entry := range entries {
		name := entry.Name()
		rel := name
		if relBase != "" {
			rel = relBase + "/" + name
		}
		rel = normalizeRel(rel)
		if isIgnored(rel, state.Ignore) || isExcluded(rel, state.Exclude) {
			continue
		}

		abs := filepath.Join(absBase, filepath.FromSlash(name))
		if entry.Type()&os.ModeSymlink != 0 {
			visitSymlink(abs, rel, depth, state)
			continue
		}
		if entry.IsDir() {
			walk(abs, rel, depth+1, state)
			continue
		}
		if entry.Type().IsRegular() {
			*state.Files = append(*state.Files, rel)
		}
	}
}

func entryKind(mode core.SyncMode, content string) string {
	return core.EntryKindFor(mode, content)
}

func readEntry(abs string, mode core.SyncMode, errors *[]ScanError) (core.SnapshotEntry, bool) {
	content, err := os.ReadFile(abs)
	if err != nil {
		addScanError(errors, abs, err.Error())
		return core.SnapshotEntry{}, false
	}
	return core.SnapshotEntry{Kind: entryKind(mode, string(content)), Content: string(content)}, true
}

type configuredPath struct {
	Real string
	Info os.FileInfo
}

// resolveConfiguredPath handles a symlink when the declared category path
// itself is a link.  A missing path and a dangling link both mean "no file";
// an escape is the one case that is reported.
func resolveConfiguredPath(abs, rootReal, rootRel string, allowEscape []string, errors *[]ScanError) (configuredPath, bool) {
	// lstat first distinguishes a genuinely absent path from a path that
	// races with deletion.  EvalSymlinks then resolves the entire path, not
	// just its final component, so a configured "dir/file" cannot smuggle an
	// outside file through an escaping symlink in dir.
	if _, err := os.Lstat(abs); err != nil {
		return configuredPath{}, false
	}
	real, ok := tryRealpath(abs)
	if !ok {
		return configuredPath{}, false
	}
	if !isWithinRoot(real, rootReal) && !isEscapeAllowed(rootRel, allowEscape) {
		addScanError(errors, abs, fmt.Sprintf("symlink 逃逸 adapter root: %s", real))
		return configuredPath{}, false
	}
	info, err := os.Stat(real)
	if err != nil {
		return configuredPath{}, false
	}
	return configuredPath{Real: real, Info: info}, true
}

func scanCategory(root, rootReal, category string, cfg core.CategoryConfig, ignore, allowEscape []string, errors *[]ScanError) core.CategorySnapshot {
	files := make(core.SnapshotFiles)

	for _, declared := range cfg.Paths {
		declared = normalizeRel(declared)
		if isDirPath(declared) {
			relDir := strings.TrimRight(declared, "/")
			if relDir == "" || isIgnored(relDir, ignore) || isExcluded(relDir, cfg.Exclude) {
				continue
			}

			absDir := filepath.Join(root, filepath.FromSlash(relDir))
			resolved, ok := resolveConfiguredPath(absDir, rootReal, relDir, allowEscape, errors)
			if !ok || !resolved.Info.IsDir() {
				continue
			}

			collected := make([]string, 0)
			state := scanState{
				Ignore:      ignore,
				Exclude:     cfg.Exclude,
				AllowEscape: allowEscape,
				Errors:      errors,
				Files:       &collected,
				RootReal:    rootReal,
				RootRelBase: relDir,
				Visited:     make(map[string]struct{}),
			}
			walk(resolved.Real, "", 0, &state)
			for _, rel := range collected {
				entry, ok := readEntry(filepath.Join(resolved.Real, filepath.FromSlash(rel)), cfg.Mode, errors)
				if ok {
					files[normalizeRel(rel)] = entry
				}
			}
			continue
		}

		if isIgnored(declared, ignore) || isExcluded(declared, cfg.Exclude) {
			continue
		}
		abs := filepath.Join(root, filepath.FromSlash(declared))
		resolved, ok := resolveConfiguredPath(abs, rootReal, declared, allowEscape, errors)
		if !ok || !resolved.Info.Mode().IsRegular() {
			continue
		}
		entry, ok := readEntry(resolved.Real, cfg.Mode, errors)
		if ok {
			// A single-file category is keyed by basename, not by the
			// declaration's directory prefix.
			files[basenameOf(declared)] = entry
		}
	}

	return core.CategorySnapshot{
		AdapterID: "",
		Category:  category,
		Mode:      cfg.Mode,
		Files:     files,
	}
}

// categoryOrder makes snapshots deterministic despite CategoryConfig being a
// map in the frozen Go interface.  For the three built-in adapters it follows
// the insertion order of the frozen TypeScript defaults; custom categories are
// appended in lexical order.
func categoryOrder(adapterID string, categories map[string]core.CategoryConfig) []string {
	preferred := map[string][]string{
		"pi":       {"settings", "skills", "extensions", "agents", "models", "prompts", "themes"},
		"herdr":    {"config"},
		"opencode": {"config", "plugins", "locks"},
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

// ScanAdapter reads one adapter root without writing to the tool directory.
// Missing roots produce an empty snapshot and one diagnostic; missing
// configured files/directories produce empty category entries without errors.
func ScanAdapter(adapterID string, config core.AdapterConfig) ScanOutcome {
	outcome := ScanOutcome{
		Snapshot: core.AdapterSnapshot{AdapterID: adapterID, Categories: []core.CategorySnapshot{}},
		Errors:   make([]ScanError, 0),
	}
	if config.Enabled != nil && !*config.Enabled {
		return outcome
	}

	root := expandHome(config.Root)
	rootInfo, err := os.Stat(root)
	if err != nil {
		addScanError(&outcome.Errors, root, err.Error())
		outcome.Snapshot.Categories = []core.CategorySnapshot{}
		return outcome
	}
	if !rootInfo.IsDir() {
		addScanError(&outcome.Errors, root, "not a directory")
		outcome.Snapshot.Categories = []core.CategorySnapshot{}
		return outcome
	}

	rootReal, ok := tryRealpath(root)
	if !ok {
		rootReal = root
	}

	for _, category := range categoryOrder(adapterID, config.Categories) {
		cfg := config.Categories[category]
		if cfg.Enabled != nil && !*cfg.Enabled {
			continue
		}
		cat := scanCategory(root, rootReal, category, cfg, config.Ignore, config.AllowEscape, &outcome.Errors)
		cat.AdapterID = adapterID
		outcome.Snapshot.Categories = append(outcome.Snapshot.Categories, cat)
	}
	return outcome
}

// Lower-case aliases are useful to tests kept in package adapter and mirror
// the original TypeScript naming without duplicating implementation.
func scanAdapter(adapterID string, config core.AdapterConfig) ScanOutcome {
	return ScanAdapter(adapterID, config)
}
