package sync

import (
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/engine"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

// PullAction is the file-level plan produced by PlanPull and
// PlanFirstContact. Type is one of write, delete, or conflict. The remaining
// fields are populated according to Type; keeping one concrete Go shape is the
// closest representation of the frozen TypeScript discriminated union and
// makes plans easy for command/render layers to inspect.
type PullAction struct {
	Type          string   `json:"type"`
	AdapterID     string   `json:"adapterId"`
	Category      string   `json:"category"`
	RelPath       string   `json:"relPath"`
	Content       string   `json:"content,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	KeyPaths      []string `json:"keyPaths,omitempty"`
	LocalContent  string   `json:"localContent,omitempty"`
	RemoteContent string   `json:"remoteContent,omitempty"`
}

// These aliases retain the three names used by the frozen TypeScript types
// while the Go API uses the single concrete PullAction representation above.
type PullWriteAction = PullAction
type PullDeleteAction = PullAction
type PullConflictAction = PullAction

type PullPlan struct {
	Actions []PullAction `json:"actions"`
}

const (
	PullActionWrite    = "write"
	PullActionDelete   = "delete"
	PullActionConflict = "conflict"

	PullWrite    = PullActionWrite
	PullDelete   = PullActionDelete
	PullConflict = PullActionConflict
)

// FileRef is the common shape used for changed, remote-ahead, applied, and
// conflict file lists.
type FileRef struct {
	AdapterID string `json:"adapterId"`
	Category  string `json:"category"`
	RelPath   string `json:"relPath"`
}

type SyncFileRef = FileRef

type ApplyResult struct {
	Written   []FileRef `json:"written"`
	Deleted   []FileRef `json:"deleted"`
	Conflicts []FileRef `json:"conflicts"`
	BackupDir string    `json:"backupDir,omitempty"`
}

// SyncBaseMode identifies whether base came from a historical git commit or
// the current store worktree.
type SyncBaseMode string

const (
	SyncBaseModeGit   SyncBaseMode = "git"
	SyncBaseModeStore SyncBaseMode = "store"
)

type SyncSources struct {
	Mode       SyncBaseMode
	Base       []core.AdapterSnapshot
	Local      []core.AdapterSnapshot
	Remote     []core.AdapterSnapshot
	BaseCommit string
	RemoteRef  string
	Warnings   []string
	Errors     []string
}

// PushCheck is the pre-push safety decision. Status is one of ok,
// remote-ahead, or conflicts.
type PushCheck struct {
	Status           string               `json:"status"`
	ChangedFiles     []FileRef            `json:"changedFiles"`
	ConflictItems    []PullConflictAction `json:"conflictItems"`
	RemoteAheadFiles []FileRef            `json:"remoteAheadFiles"`
}

const (
	PushStatusOK          = "ok"
	PushStatusRemoteAhead = "remote-ahead"
	PushStatusConflicts   = "conflicts"
)

// PlanPull translates the three raw snapshots into the safe, file-level pull
// plan. Excluded keys are removed only for judgement; raw local/remote values
// are retained for conflict presentation and local excluded values are planted
// back into clean merge results.
func PlanPull(
	config core.HomerConfig,
	base, local, remote []core.AdapterSnapshot,
) PullPlan {
	raw := snapshotGroups{
		base:   indexByAdapter(base),
		local:  indexByAdapter(local),
		remote: indexByAdapter(remote),
	}
	stripped := snapshotGroups{
		base:   indexByAdapter(stripSnapshots(base, config)),
		local:  indexByAdapter(stripSnapshots(local, config)),
		remote: indexByAdapter(stripSnapshots(remote, config)),
	}

	actions := make([]PullAction, 0)
	for _, adapterID := range unionAdapterIDs(base, local, remote) {
		ctx := adapterContext{
			adapterID: adapterID,
			stripped:  snapshotTriplet{base: snapshotMapPtr(stripped.base, adapterID), local: snapshotMapPtr(stripped.local, adapterID), remote: snapshotMapPtr(stripped.remote, adapterID)},
			rawLocal:  snapshotMapPtr(raw.local, adapterID),
			rawRemote: snapshotMapPtr(raw.remote, adapterID),
			config:    config,
		}
		for _, category := range unionCategoryNames(ctx.stripped.base, ctx.stripped.local, ctx.stripped.remote) {
			actions = append(actions, planCategory(ctx, category)...)
		}
	}
	return PullPlan{Actions: actions}
}

// planPull is retained as the package-local spelling used by migration tests.
func planPull(config core.HomerConfig, base, local, remote []core.AdapterSnapshot) PullPlan {
	return PlanPull(config, base, local, remote)
}

type snapshotGroups struct {
	base   map[string]core.AdapterSnapshot
	local  map[string]core.AdapterSnapshot
	remote map[string]core.AdapterSnapshot
}

type snapshotTriplet struct {
	base   *core.AdapterSnapshot
	local  *core.AdapterSnapshot
	remote *core.AdapterSnapshot
}

type adapterContext struct {
	adapterID string
	stripped  snapshotTriplet
	rawLocal  *core.AdapterSnapshot
	rawRemote *core.AdapterSnapshot
	config    core.HomerConfig
}

func indexByAdapter(snapshots []core.AdapterSnapshot) map[string]core.AdapterSnapshot {
	out := make(map[string]core.AdapterSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		out[snapshot.AdapterID] = snapshot
	}
	return out
}

// unionAdapterIDs follows the source-slice order used by the TypeScript
// implementation. Callers that need a deterministic order should provide
// config-shaped snapshots (the store/adapter readers already sort those).
func snapshotMapPtr(snapshots map[string]core.AdapterSnapshot, adapterID string) *core.AdapterSnapshot {
	snapshot, ok := snapshots[adapterID]
	if !ok {
		return nil
	}
	return &snapshot
}

func unionAdapterIDs(groups ...[]core.AdapterSnapshot) []string {
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	for _, group := range groups {
		for _, snapshot := range group {
			if _, exists := seen[snapshot.AdapterID]; exists {
				continue
			}
			seen[snapshot.AdapterID] = struct{}{}
			ids = append(ids, snapshot.AdapterID)
		}
	}
	return ids
}

func unionCategoryNames(adapters ...*core.AdapterSnapshot) []string {
	seen := make(map[string]struct{})
	names := make([]string, 0)
	for _, adapter := range adapters {
		if adapter == nil {
			continue
		}
		for _, category := range adapter.Categories {
			if _, exists := seen[category.Category]; exists {
				continue
			}
			seen[category.Category] = struct{}{}
			names = append(names, category.Category)
		}
	}
	return names
}

func findCategory(adapter *core.AdapterSnapshot, name string) *core.CategorySnapshot {
	if adapter == nil {
		return nil
	}
	for index := range adapter.Categories {
		if adapter.Categories[index].Category == name {
			return &adapter.Categories[index]
		}
	}
	return nil
}

func categoryFiles(category *core.CategorySnapshot) core.SnapshotFiles {
	if category == nil || category.Files == nil {
		return core.SnapshotFiles{}
	}
	return category.Files
}

func planCategory(ctx adapterContext, category string) []PullAction {
	baseCategory := findCategory(ctx.stripped.base, category)
	localCategory := findCategory(ctx.stripped.local, category)
	remoteCategory := findCategory(ctx.stripped.remote, category)

	mode := core.SyncMode("")
	if localCategory != nil {
		mode = localCategory.Mode
	} else if baseCategory != nil {
		mode = baseCategory.Mode
	} else if remoteCategory != nil {
		mode = remoteCategory.Mode
	}
	if mode == "" {
		return nil
	}

	baseFiles := categoryFiles(baseCategory)
	localFiles := categoryFiles(localCategory)
	remoteFiles := categoryFiles(remoteCategory)
	rawLocalFiles := categoryFiles(findCategory(ctx.rawLocal, category))
	rawRemoteFiles := categoryFiles(findCategory(ctx.rawRemote, category))

	if mode == core.SyncModeMirror {
		actions := make([]PullAction, 0)
		for _, op := range engine.CompareCategory(baseFiles, localFiles, remoteFiles) {
			if action, ok := actionFromMirrorOp(op, ctx.adapterID, category,
				fileViews{judgedLocal: localFiles, judgedRemote: remoteFiles, displayLocal: rawLocalFiles, displayRemote: rawRemoteFiles}); ok {
				actions = append(actions, action)
			}
		}
		return actions
	}

	actions := make([]PullAction, 0)
	for _, relPath := range unionPaths(baseFiles, localFiles, remoteFiles) {
		action, ok := planMergeFile(ctx, category, relPath,
			fileTriplet{base: entryPtr(baseFiles, relPath), local: entryPtr(localFiles, relPath), remote: entryPtr(remoteFiles, relPath)},
			displayTriplet{local: entryPtr(rawLocalFiles, relPath), remote: entryPtr(rawRemoteFiles, relPath)})
		if ok {
			actions = append(actions, action)
		}
	}
	return actions
}

func unionPaths(fileSets ...core.SnapshotFiles) []string {
	seen := make(map[string]struct{})
	for _, files := range fileSets {
		for path := range files {
			seen[path] = struct{}{}
		}
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func entryPtr(files core.SnapshotFiles, path string) *core.SnapshotEntry {
	entry, ok := files[path]
	if !ok {
		return nil
	}
	copy := entry
	return &copy
}

type fileViews struct {
	judgedLocal   core.SnapshotFiles
	judgedRemote  core.SnapshotFiles
	displayLocal  core.SnapshotFiles
	displayRemote core.SnapshotFiles
}

func actionFromMirrorOp(op engine.MirrorOp, adapterID, category string, views fileViews) (PullAction, bool) {
	switch op.Type {
	case "pull":
		remote := entryPtr(views.judgedRemote, op.Path)
		if remote == nil {
			return PullAction{}, false
		}
		return PullAction{Type: PullActionWrite, AdapterID: adapterID, Category: category, RelPath: op.Path, Content: remote.Content}, true
	case "pull-delete":
		return PullAction{Type: PullActionDelete, AdapterID: adapterID, Category: category, RelPath: op.Path}, true
	case "conflict":
		return mirrorConflictAction(adapterID, category, op.Path, op.Reason, views), true
	case "push", "push-delete", "noop":
		return PullAction{}, false
	default:
		return PullAction{}, false
	}
}

func mirrorConflictAction(adapterID, category, relPath, reason string, views fileViews) PullAction {
	action := PullAction{Type: PullActionConflict, AdapterID: adapterID, Category: category, RelPath: relPath, Reason: reason}
	local := entryPtr(views.displayLocal, relPath)
	if local == nil {
		local = entryPtr(views.judgedLocal, relPath)
	}
	remote := safeRemoteDisplay(entryPtr(views.displayRemote, relPath), entryPtr(views.judgedRemote, relPath))
	if local != nil {
		action.LocalContent = local.Content
	}
	if remote != nil {
		action.RemoteContent = remote.Content
	}
	return action
}

func safeRemoteDisplay(display, judged *core.SnapshotEntry) *core.SnapshotEntry {
	if display == nil || judged == nil || display.Content != judged.Content {
		return nil
	}
	return display
}

type fileTriplet struct {
	base   *core.SnapshotEntry
	local  *core.SnapshotEntry
	remote *core.SnapshotEntry
}

type displayTriplet struct {
	local  *core.SnapshotEntry
	remote *core.SnapshotEntry
}

func planMergeFile(ctx adapterContext, category, relPath string, judged fileTriplet, display displayTriplet) (PullAction, bool) {
	if isDegraded(judged.base) || isDegraded(judged.local) || isDegraded(judged.remote) {
		views := fileViews{
			judgedLocal:   filesOf(judged.local, relPath),
			judgedRemote:  filesOf(judged.remote, relPath),
			displayLocal:  filesOf(firstNonNil(display.local, judged.local), relPath),
			displayRemote: filesOf(firstNonNil(display.remote, judged.remote), relPath),
		}
		op := engine.CompareFile(judged.base, judged.local, judged.remote, relPath)
		return actionFromMirrorOp(op, ctx.adapterID, category, views)
	}

	if judged.local == nil && judged.remote == nil {
		return PullAction{}, false
	}
	if !remoteChanged(judged.base, judged.remote) {
		return PullAction{}, false
	}

	if judged.base == nil {
		if judged.local == nil {
			content := ""
			if judged.remote != nil {
				content = judged.remote.Content
			}
			return PullAction{Type: PullActionWrite, AdapterID: ctx.adapterID, Category: category, RelPath: relPath, Content: content}, true
		}
		if judged.remote == nil || judged.local.Content == judged.remote.Content {
			return PullAction{}, false
		}
		action := PullAction{Type: PullActionConflict, AdapterID: ctx.adapterID, Category: category, RelPath: relPath, Reason: "modify-vs-modify"}
		local := firstNonNil(display.local, judged.local)
		if local != nil {
			action.LocalContent = local.Content
		}
		if remote := safeRemoteDisplay(display.remote, judged.remote); remote != nil {
			action.RemoteContent = remote.Content
		}
		return action, true
	}

	if judged.remote == nil {
		if judged.local == nil {
			return PullAction{}, false
		}
		baseValue, _ := parseEntry(judged.base)
		localValue, _ := parseEntry(judged.local)
		if diff := engine.DiffJSON(baseValue, localValue); len(diff.Keys) == 0 {
			return PullAction{Type: PullActionDelete, AdapterID: ctx.adapterID, Category: category, RelPath: relPath}, true
		}
		action := PullAction{Type: PullActionConflict, AdapterID: ctx.adapterID, Category: category, RelPath: relPath, Reason: "local-modify-vs-remote-delete"}
		if local := firstNonNil(display.local, judged.local); local != nil {
			action.LocalContent = local.Content
		}
		return action, true
	}

	if judged.local == nil {
		action := PullAction{Type: PullActionConflict, AdapterID: ctx.adapterID, Category: category, RelPath: relPath, Reason: "local-delete-vs-remote-modify"}
		if remote := safeRemoteDisplay(display.remote, judged.remote); remote != nil {
			action.RemoteContent = remote.Content
		}
		return action, true
	}

	baseValue, _ := parseEntry(judged.base)
	localValue, _ := parseEntry(judged.local)
	remoteValue, _ := parseEntry(judged.remote)
	merged := engine.MergeJSON(baseValue, localValue, remoteValue)
	if len(merged.Conflicts) > 0 {
		keyPaths := make([]string, 0, len(merged.Conflicts))
		for _, conflict := range merged.Conflicts {
			keyPaths = append(keyPaths, conflict.KeyPath)
		}
		return PullAction{Type: PullActionConflict, AdapterID: ctx.adapterID, Category: category, RelPath: relPath, Reason: "merge-keys", KeyPaths: keyPaths}, true
	}

	localRaw := firstNonNil(display.local, judged.local)
	var localRawValue orderedjson.Value
	if localRaw != nil {
		localRawValue, _ = parseEntry(localRaw)
	}
	planted := plantExcludedKeys(merged.Merged, localRawValue, excludedKeysFor(ctx.config, ctx.adapterID, category))
	if !orderedjson.DeepEqual(planted, localRawValue) {
		return PullAction{Type: PullActionWrite, AdapterID: ctx.adapterID, Category: category, RelPath: relPath, Content: string(orderedjson.SerializeFile(planted))}, true
	}
	return PullAction{}, false
}

func filesOf(entry *core.SnapshotEntry, relPath string) core.SnapshotFiles {
	if entry == nil {
		return core.SnapshotFiles{}
	}
	return core.SnapshotFiles{relPath: *entry}
}

func firstNonNil(values ...*core.SnapshotEntry) *core.SnapshotEntry {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func remoteChanged(base, remote *core.SnapshotEntry) bool {
	if base == nil {
		return remote != nil
	}
	if remote == nil {
		return true
	}
	return base.Content != remote.Content
}

func isDegraded(entry *core.SnapshotEntry) bool {
	if entry == nil {
		return false
	}
	if entry.Kind == "file" {
		return true
	}
	value, ok := parseEntry(entry)
	return !ok || !isJSONObject(value)
}

func parseEntry(entry *core.SnapshotEntry) (orderedjson.Value, bool) {
	if entry == nil {
		return nil, false
	}
	return parseJSONContent(entry.Content)
}

// CheckPushSafety applies the same excluded-key stripping and engine drift
// aggregation as status. Remote pull drift wins over conflicts; when clean,
// changedFiles describes exactly the store files that push would touch.
func CheckPushSafety(
	config core.HomerConfig,
	base, local, remote []core.AdapterSnapshot,
) PushCheck {
	strippedBase := stripSnapshots(base, config)
	strippedLocal := stripSnapshots(local, config)
	strippedRemote := stripSnapshots(remote, config)
	drifts := engine.ComputeDrift(strippedBase, strippedLocal, strippedRemote)

	totalPull, totalConflicts := 0, 0
	for _, category := range drifts {
		totalPull += category.Pull
		totalConflicts += category.Conflicts
	}

	if totalPull > 0 {
		plan := PlanPull(config, base, local, remote)
		return PushCheck{
			Status:           PushStatusRemoteAhead,
			ChangedFiles:     []FileRef{},
			ConflictItems:    []PullConflictAction{},
			RemoteAheadFiles: remoteDrivenFiles(plan.Actions),
		}
	}
	if totalConflicts > 0 {
		plan := PlanPull(config, base, local, remote)
		conflicts := make([]PullConflictAction, 0)
		for _, action := range plan.Actions {
			if action.Type == PullActionConflict {
				conflicts = append(conflicts, action)
			}
		}
		return PushCheck{
			Status:           PushStatusConflicts,
			ChangedFiles:     []FileRef{},
			ConflictItems:    conflicts,
			RemoteAheadFiles: []FileRef{},
		}
	}

	return PushCheck{
		Status:           PushStatusOK,
		ChangedFiles:     pushedFiles(drifts, [][]core.AdapterSnapshot{strippedBase, strippedLocal, strippedRemote}),
		ConflictItems:    []PullConflictAction{},
		RemoteAheadFiles: []FileRef{},
	}
}

func checkPushSafety(config core.HomerConfig, base, local, remote []core.AdapterSnapshot) PushCheck {
	return CheckPushSafety(config, base, local, remote)
}

func remoteDrivenFiles(actions []PullAction) []FileRef {
	direct := make([]FileRef, 0)
	for _, action := range actions {
		if action.Type == PullActionWrite || action.Type == PullActionDelete {
			direct = append(direct, fileRefOf(action))
		}
	}
	if len(direct) > 0 {
		return dedupFiles(direct)
	}
	fallback := make([]FileRef, 0)
	for _, action := range actions {
		if action.Type == PullActionConflict {
			fallback = append(fallback, fileRefOf(action))
		}
	}
	return dedupFiles(fallback)
}

func pushedFiles(drifts []engine.CategoryDrift, snapshotGroups [][]core.AdapterSnapshot) []FileRef {
	known := knownPathsIndex(snapshotGroups)
	out := make([]FileRef, 0)
	for _, category := range drifts {
		base := FileRef{AdapterID: category.AdapterID, Category: category.Category}
		paths := known[indexKey(category.AdapterID, category.Category)]
		for _, op := range category.Ops {
			if op.Type == "push" || op.Type == "push-delete" {
				out = append(out, FileRef{AdapterID: base.AdapterID, Category: base.Category, RelPath: op.Path})
			}
		}
		for _, changedKey := range category.ChangedKeys {
			if path := matchKnownPath(changedKey, paths); path != "" {
				out = append(out, FileRef{AdapterID: base.AdapterID, Category: base.Category, RelPath: path})
			}
		}
	}
	return dedupFiles(out)
}

func knownPathsIndex(groups [][]core.AdapterSnapshot) map[string][]string {
	index := make(map[string][]string)
	for _, group := range groups {
		for _, adapter := range group {
			for _, category := range adapter.Categories {
				key := indexKey(adapter.AdapterID, category.Category)
				paths := index[key]
				for path := range category.Files {
					found := false
					for _, existing := range paths {
						if existing == path {
							found = true
							break
						}
					}
					if !found {
						paths = append(paths, path)
					}
				}
				index[key] = paths
			}
		}
	}
	for key, paths := range index {
		sort.SliceStable(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
		index[key] = paths
	}
	return index
}

func matchKnownPath(changedKey string, paths []string) string {
	for _, path := range paths {
		if strings.HasPrefix(changedKey, path+":") {
			return path
		}
	}
	return ""
}

func indexKey(adapterID, category string) string { return adapterID + "\x00" + category }

func fileRefOf(action PullAction) FileRef {
	return FileRef{AdapterID: action.AdapterID, Category: action.Category, RelPath: action.RelPath}
}

func dedupFiles(refs []FileRef) []FileRef {
	seen := make(map[string]struct{}, len(refs))
	out := make([]FileRef, 0, len(refs))
	for _, ref := range refs {
		key := indexKey(ref.AdapterID, ref.Category) + "\x00" + ref.RelPath
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ref)
	}
	return out
}

// stripSnapshots is kept here rather than in the command layer so PlanPull,
// CheckPushSafety, and status all consume the same exclude-key semantics.
func stripSnapshots(snapshots []core.AdapterSnapshot, config core.HomerConfig) []core.AdapterSnapshot {
	out := make([]core.AdapterSnapshot, len(snapshots))
	for index, snapshot := range snapshots {
		out[index] = StripSnapshotExcludeKeys(snapshot, config)
	}
	return out
}
