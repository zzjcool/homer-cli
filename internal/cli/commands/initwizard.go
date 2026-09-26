package commands

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/core"
)

// WizardOption is one stable value shown by a wizard MultiSelect.
type WizardOption struct {
	Value string
	Label string
}

// WizardPort is the only terminal dependency of the init wizard.  Returning
// an error means that the interaction was cancelled (Ctrl-C/EOF), so callers
// must abort the whole init without writing a config or snapshot.
type WizardPort interface {
	MultiSelect(message string, options []WizardOption, checked []string) ([]string, error)
}

// WizardEntry is a selectable first-level directory entry, or a file key when
// a caller supplies an explicitly drillable file category.
type WizardEntry struct {
	Key      string
	Label    string
	Included bool
}

// WizardCategory is the data presented at the category level. Entries is
// intentionally empty for ordinary/small categories; large directory
// categories are expanded to first-level entries by the state builder.
type WizardCategory struct {
	Name      string
	Enabled   bool
	FileCount int
	Entries   []WizardEntry
}

// WizardAdapter is the data presented at the adapter level.
type WizardAdapter struct {
	ID         string
	Enabled    bool
	FileCount  int
	Categories []WizardCategory
}

// WizardState is the immutable input to RunSelectionWizard.
type WizardState struct {
	Adapters []WizardAdapter
}

// WizardSelection is the complete checked subset returned by the three
// wizard levels. Maps contain stable IDs rather than presentation labels.
type WizardSelection struct {
	Adapters   map[string]bool
	Categories map[string]map[string]bool
	Excluded   map[string]map[string][]string
}

// WizardEntryDrillThreshold is the number of first-level directory entries at
// which the wizard offers a third, per-entry selection level.
const WizardEntryDrillThreshold = 8

// WizardBackValue is a sentinel option value injected into wizard prompts
// (except the top-level adapter question) so the user can go back one level:
// selecting it and pressing Enter restarts the previous question with the
// current choices preserved as defaults. The sentinel never reaches the
// resulting selection.
const WizardBackValue = "\x00back"

// backOptionLabel is how the back sentinel appears in the terminal.
const backOptionLabel = "< 返回上一级"

func backOption() WizardOption {
	return WizardOption{Value: WizardBackValue, Label: backOptionLabel}
}

// containsBack reports whether the user picked the back sentinel.
func containsBack(values []string) bool {
	for _, value := range values {
		if value == WizardBackValue {
			return true
		}
	}
	return false
}

// stripBack removes the sentinel from a value list.
func stripBack(values []string) []string {
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if value != WizardBackValue {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

func withBackOption(options []WizardOption) []WizardOption {
	return append(append([]WizardOption(nil), options...), backOption())
}

// RunSelectionWizard orchestrates adapter -> category -> entry MultiSelects.
// It is deliberately pure apart from calls to the injected port, making all
// cancellation and selection cases testable without a terminal.
// Every question below the adapter level offers a "< 返回上一级" sentinel;
// picking it restarts the previous question with the user's in-progress
// choices preserved as the new defaults.
func RunSelectionWizard(port WizardPort, state WizardState) (WizardSelection, error) {
	if port == nil {
		port = identityWizardPort{}
	}
	selection := WizardSelection{
		Adapters:   make(map[string]bool, len(state.Adapters)),
		Categories: make(map[string]map[string]bool, len(state.Adapters)),
		Excluded:   make(map[string]map[string][]string),
	}

	adapterOptions := make([]WizardOption, 0, len(state.Adapters))
	adapterChecked := make([]string, 0, len(state.Adapters))
	for _, item := range state.Adapters {
		adapterOptions = append(adapterOptions, WizardOption{
			Value: item.ID,
			Label: wizardCountLabel(item.ID, item.FileCount, "文件"),
		})
		if item.Enabled {
			adapterChecked = append(adapterChecked, item.ID)
		}
	}

	// The adapter level has no parent to go back to: no sentinel is offered.
	checkedAdapters, err := port.MultiSelect("选择要初始化的 adapter（空格勾选，Enter 确认）", adapterOptions, adapterChecked)
	if err != nil {
		return WizardSelection{}, err
	}
	selectedAdapters := optionSet(adapterOptions, checkedAdapters)

	// lastChoice carries in-progress selections per level so a back
	// navigation restores them as checked defaults instead of starting over.
	lastCategoryChoice := make(map[string][]string)
	lastEntryChoice := make(map[string]map[string][]string)

	for _, adapterState := range state.Adapters {
		adapterSelected := selectedAdapters[adapterState.ID]
		selection.Adapters[adapterState.ID] = adapterSelected
		categorySelection := make(map[string]bool, len(adapterState.Categories))
		selection.Categories[adapterState.ID] = categorySelection

		categoryOptions := make([]WizardOption, 0, len(adapterState.Categories))
		categoryChecked := make([]string, 0, len(adapterState.Categories))
		for _, category := range adapterState.Categories {
			categoryOptions = append(categoryOptions, WizardOption{
				Value: category.Name,
				Label: wizardCountLabel(category.Name, category.FileCount, "文件"),
			})
			if category.Enabled {
				categoryChecked = append(categoryChecked, category.Name)
			}
		}

		if !adapterSelected {
			for _, category := range adapterState.Categories {
				categorySelection[category.Name] = false
			}
			continue
		}

		categoryDefault := categoryChecked
		if previous, ok := lastCategoryChoice[adapterState.ID]; ok {
			categoryDefault = previous
		}

	categoryLoop:
		for {
			checkedCategories, err := port.MultiSelect(
				"选择 "+adapterState.ID+" 的分类（空格勾选，Enter 确认；选 "+backOptionLabel+" 回到 adapter 选择）",
				withBackOption(categoryOptions),
				categoryDefault,
			)
			if err != nil {
				return WizardSelection{}, err
			}
			if containsBack(checkedCategories) {
				// Go back to the adapter question, preserving in-progress state.
				lastCategoryChoice[adapterState.ID] = stripBack(categoryDefault)
				checkedAdapters, err = port.MultiSelect("选择要初始化的 adapter（空格勾选，Enter 确认）", adapterOptions, checkedAdapters)
				if err != nil {
					return WizardSelection{}, err
				}
				selectedAdapters = optionSet(adapterOptions, checkedAdapters)
				adapterSelected = selectedAdapters[adapterState.ID]
				selection.Adapters[adapterState.ID] = adapterSelected
				if !adapterSelected {
					for _, category := range adapterState.Categories {
						categorySelection[category.Name] = false
					}
					break categoryLoop
				}
				continue
			}
			categoryDefault = stripBack(checkedCategories)

			selectedCategories := optionSet(categoryOptions, categoryDefault)
			for _, category := range adapterState.Categories {
				selected := selectedCategories[category.Name]
				categorySelection[category.Name] = selected
				// The state builder emits Entries only for categories above the
				// threshold. A caller may also populate Entries explicitly to mark
				// a small directory as drillable, so the orchestration keys off the
				// non-empty marker rather than reapplying the threshold here.
				if !selected || len(category.Entries) == 0 {
					continue
				}

				entryOptions := make([]WizardOption, 0, len(category.Entries))
				entryChecked := make([]string, 0, len(category.Entries))
				for _, entry := range category.Entries {
					if entry.Key == "" {
						continue
					}
					label := entry.Label
					if label == "" {
						label = entry.Key
					}
					entryOptions = append(entryOptions, WizardOption{Value: entry.Key, Label: label})
					if entry.Included {
						entryChecked = append(entryChecked, entry.Key)
					}
				}
				if len(entryOptions) == 0 {
					continue
				}

				entryDefault := entryChecked
				if previous, ok := lastEntryChoice[adapterState.ID][category.Name]; ok {
					entryDefault = previous
				}

			entryLoop:
				for {
					checkedEntries, err := port.MultiSelect(
						"选择 "+adapterState.ID+"/"+category.Name+" 的目录条目（空格勾选，Enter 确认；选 "+backOptionLabel+" 回到分类选择）",
						withBackOption(entryOptions),
						entryDefault,
					)
					if err != nil {
						return WizardSelection{}, err
					}
					if containsBack(checkedEntries) {
						lastEntryChoice[adapterState.ID] = map[string][]string{category.Name: stripBack(entryDefault)}
						continue categoryLoop
					}
					entryDefault = stripBack(checkedEntries)
					selectedEntries := optionSet(entryOptions, entryDefault)
					for _, entry := range category.Entries {
						if entry.Key == "" || selectedEntries[entry.Key] {
							continue
						}
						appendWizardExclude(&selection, adapterState.ID, category.Name, entry.Key)
					}
					break entryLoop
				}
			}
			break categoryLoop
		}
	}
	return selection, nil
}

func wizardCountLabel(name string, count int, noun string) string {
	return name + " (" + formatWizardCount(count) + " " + noun + ")"
}

func formatWizardCount(count int) string {
	// Counts are deliberately formatted without locale-specific punctuation so
	// labels remain stable for both the survey adapter and fake ports.
	if count < 0 {
		return "0"
	}
	if count == 0 {
		return "0"
	}
	var digits [32]byte
	index := len(digits)
	for count > 0 {
		index--
		digits[index] = byte('0' + count%10)
		count /= 10
	}
	return string(digits[index:])
}

func optionSet(options []WizardOption, checked []string) map[string]bool {
	allowed := make(map[string]struct{}, len(options))
	for _, option := range options {
		allowed[option.Value] = struct{}{}
	}
	result := make(map[string]bool, len(checked))
	for _, value := range checked {
		if _, ok := allowed[value]; ok {
			result[value] = true
		}
	}
	return result
}

func appendWizardExclude(selection *WizardSelection, adapterID, category, key string) {
	if selection.Excluded == nil {
		selection.Excluded = make(map[string]map[string][]string)
	}
	byAdapter := selection.Excluded[adapterID]
	if byAdapter == nil {
		byAdapter = make(map[string][]string)
		selection.Excluded[adapterID] = byAdapter
	}
	for _, existing := range byAdapter[category] {
		if existing == key {
			return
		}
	}
	byAdapter[category] = append(byAdapter[category], key)
}

// ApplySelectionToConfig mutates only enabled/exclude selection fields. It
// never removes existing patterns or touches roots, paths, modes, ignores,
// allowEscape, backup, or secrets.
func ApplySelectionToConfig(config *core.HomerConfig, selection WizardSelection) {
	if config == nil {
		return
	}
	for adapterID, adapterConfig := range config.Adapters {
		keepAdapter, hasAdapter := selection.Adapters[adapterID]
		if hasAdapter {
			setEnabledSelection(&adapterConfig.Enabled, keepAdapter)
		}
		categoryChoices := selection.Categories[adapterID]
		excludedCategories := selection.Excluded[adapterID]
		for categoryName, categoryConfig := range adapterConfig.Categories {
			keepCategory, hasCategory := categoryChoices[categoryName]
			if hasCategory {
				setEnabledSelection(&categoryConfig.Enabled, keepCategory)
			}
			for _, key := range excludedCategories[categoryName] {
				if key == "" || selectionPatternMatches(key, categoryConfig.Exclude) {
					continue
				}
				categoryConfig.Exclude = append(categoryConfig.Exclude, key)
			}
			adapterConfig.Categories[categoryName] = categoryConfig
		}
		config.Adapters[adapterID] = adapterConfig
	}
}

func setEnabledSelection(target **bool, wanted bool) {
	if wanted {
		if *target == nil || **target {
			return
		}
		value := true
		*target = &value
		return
	}
	if *target != nil && !**target {
		return
	}
	value := false
	*target = &value
}

// FilterSnapshotsBySelection applies the same enabled/exclude semantics used
// by ApplySelectionToConfig. Directory categories compare the first path
// segment; file/manifest categories compare the snapshot key/basename.
func FilterSnapshotsBySelection(snapshots []core.AdapterSnapshot, config core.HomerConfig) []core.AdapterSnapshot {
	filtered := make([]core.AdapterSnapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		adapterConfig, hasAdapter := config.Adapters[snapshot.AdapterID]
		if hasAdapter && !enabledSelection(adapterConfig.Enabled) {
			continue
		}
		copySnapshot := snapshot
		copySnapshot.Categories = make([]core.CategorySnapshot, 0, len(snapshot.Categories))
		for _, category := range snapshot.Categories {
			categoryConfig, hasCategory := adapterConfig.Categories[category.Category]
			if hasAdapter && hasCategory && !enabledSelection(categoryConfig.Enabled) {
				continue
			}
			copyCategory := category
			if category.Files != nil {
				copyCategory.Files = make(core.SnapshotFiles, len(category.Files))
				for key, entry := range category.Files {
					if hasAdapter && hasCategory && selectionPatternMatchesSnapshot(key, categoryConfig) {
						continue
					}
					copyCategory.Files[key] = entry
				}
			}
			copySnapshot.Categories = append(copySnapshot.Categories, copyCategory)
		}
		filtered = append(filtered, copySnapshot)
	}
	return filtered
}

func enabledSelection(value *bool) bool {
	return value == nil || *value
}

func selectionPatternMatchesSnapshot(key string, config core.CategoryConfig) bool {
	candidates := []string{key}
	if isDirectoryCategory(config) {
		// Directory selections are first-level entries, not individual files.
		// Keep this comparison intentionally narrow: a pattern matching a
		// nested file must not silently hide its whole first-level entry.
		candidates = []string{firstPathSegment(key)}
	} else {
		candidates = append(candidates, filepath.Base(filepath.FromSlash(key)))
	}
	for _, candidate := range candidates {
		if candidate != "" && selectionPatternMatches(candidate, config.Exclude) {
			return true
		}
	}
	return false
}

func selectionPatternMatches(key string, patterns []string) bool {
	for _, pattern := range patterns {
		if adapter.MatchesIgnore(key, []string{pattern}) {
			return true
		}
	}
	return false
}

func isDirectoryCategory(config core.CategoryConfig) bool {
	if config.Kind != nil {
		return *config.Kind == core.CategoryKindDir
	}
	for _, path := range config.Paths {
		if strings.HasSuffix(strings.ReplaceAll(path, "\\", "/"), "/") {
			return true
		}
	}
	return false
}

func firstPathSegment(path string) string {
	path = strings.Trim(strings.ReplaceAll(path, "\\", "/"), "/")
	if path == "" {
		return ""
	}
	if index := strings.IndexByte(path, '/'); index >= 0 {
		return path[:index]
	}
	return path
}

// buildWizardState creates a stable state from the config and pre-wizard scan
// outcomes. Missing categories remain visible with zero files so a disabled or
// missing-root category can still be selected/re-enabled.
func buildWizardState(config core.HomerConfig, outcomes []adapter.ScanOutcome) WizardState {
	outcomeByAdapter := make(map[string]core.AdapterSnapshot, len(outcomes))
	for _, outcome := range outcomes {
		snapshot := outcome.Snapshot
		outcomeByAdapter[snapshot.AdapterID] = snapshot
	}
	adapterIDs := orderedConfigIDs(config.Adapters)
	state := WizardState{Adapters: make([]WizardAdapter, 0, len(adapterIDs))}
	for _, adapterID := range adapterIDs {
		adapterConfig := config.Adapters[adapterID]
		snapshot := outcomeByAdapter[adapterID]
		categoriesByName := make(map[string]core.CategorySnapshot, len(snapshot.Categories))
		for _, category := range snapshot.Categories {
			categoriesByName[category.Category] = category
		}
		categoryNames := make([]string, 0, len(adapterConfig.Categories))
		seenCategories := make(map[string]struct{}, len(adapterConfig.Categories))
		for _, category := range snapshot.Categories {
			if _, seen := seenCategories[category.Category]; seen {
				continue
			}
			categoryNames = append(categoryNames, category.Category)
			seenCategories[category.Category] = struct{}{}
		}
		remaining := make([]string, 0, len(adapterConfig.Categories))
		for categoryName := range adapterConfig.Categories {
			if _, seen := seenCategories[categoryName]; !seen {
				remaining = append(remaining, categoryName)
			}
		}
		sort.Strings(remaining)
		categoryNames = append(categoryNames, remaining...)

		adapterState := WizardAdapter{
			ID:         adapterID,
			Enabled:    enabledSelection(adapterConfig.Enabled),
			Categories: make([]WizardCategory, 0, len(categoryNames)),
		}
		for _, categoryName := range categoryNames {
			categoryConfig := adapterConfig.Categories[categoryName]
			snapshotCategory := categoriesByName[categoryName]
			categoryState := WizardCategory{
				Name:      categoryName,
				Enabled:   enabledSelection(categoryConfig.Enabled),
				FileCount: len(snapshotCategory.Files),
			}
			adapterState.FileCount += categoryState.FileCount
			categoryState.Entries = buildWizardEntries(categoryConfig, snapshotCategory)
			adapterState.Categories = append(adapterState.Categories, categoryState)
		}
		state.Adapters = append(state.Adapters, adapterState)
	}
	return state
}

func buildWizardEntries(config core.CategoryConfig, snapshot core.CategorySnapshot) []WizardEntry {
	if !isDirectoryCategory(config) {
		return nil
	}
	keys := make(map[string]struct{})
	for key := range snapshot.Files {
		first := firstPathSegment(key)
		if first != "" {
			keys[first] = struct{}{}
		}
	}
	if len(keys) <= WizardEntryDrillThreshold {
		return nil
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	entries := make([]WizardEntry, 0, len(ordered))
	for _, key := range ordered {
		entries = append(entries, WizardEntry{
			Key:      key,
			Label:    key,
			Included: !selectionPatternMatches(key, config.Exclude),
		})
	}
	return entries
}

func orderedConfigIDs(adapters map[string]core.AdapterConfig) []string {
	ids := make([]string, 0, len(adapters))
	seen := make(map[string]struct{}, len(adapters))
	for _, id := range knownAdapterOrder {
		if _, ok := adapters[id]; ok {
			ids = append(ids, id)
			seen[id] = struct{}{}
		}
	}
	extra := make([]string, 0, len(adapters)-len(ids))
	for id := range adapters {
		if _, ok := seen[id]; !ok {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	return append(ids, extra...)
}
