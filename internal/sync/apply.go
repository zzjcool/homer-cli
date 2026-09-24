package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/backup"
	"github.com/zzjcool/homer-cli/internal/core"
)

// ApplyPullActionsOptions controls the side effects of ApplyPullActions.
// Backup accepts bool, *bool, or nil so callers can express the optional
// TypeScript field without changing the frozen command-facing shape. Command
// is used in the backup directory name and defaults to pull.
type ApplyPullActionsOptions struct {
	Backup  any
	Command string
}

type ApplyOptions = ApplyPullActionsOptions

// ApplyPullActions resolves and validates the entire action batch before any
// write. Existing write/delete targets are backed up in one directory, then
// writes/deletes are applied in plan order; conflicts never touch disk.
func ApplyPullActions(
	paths core.HomerPaths,
	config core.HomerConfig,
	plan PullPlan,
	options ...any,
) (ApplyResult, error) {
	backupEnabled, command, err := parseApplyOptions(options)
	if err != nil {
		return ApplyResult{}, err
	}

	result := ApplyResult{
		Written:   make([]FileRef, 0),
		Deleted:   make([]FileRef, 0),
		Conflicts: make([]FileRef, 0),
	}

	resolved := make([]resolvedAction, 0, len(plan.Actions))
	for _, action := range plan.Actions {
		item, err := resolveAction(config, action)
		if err != nil {
			return result, err
		}
		resolved = append(resolved, item)
	}

	if backupEnabled {
		targets := make([]backup.BackupTarget, 0)
		for _, item := range resolved {
			if item.kind == PullActionConflict {
				continue
			}
			if _, err := os.Stat(item.target); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return result, err
			}
			targets = append(targets, backup.BackupTarget{
				SourceAbs: item.target,
				Label:     actionLabel(item.action),
			})
		}
		if len(targets) > 0 {
			backed, err := backup.BackupFiles(paths, command, targets)
			if err != nil {
				return result, err
			}
			result.BackupDir = backed.BackupDir
		}
	}

	for _, item := range resolved {
		action := item.action
		ref := FileRef{AdapterID: action.AdapterID, Category: action.Category, RelPath: action.RelPath}
		switch item.kind {
		case PullActionConflict:
			result.Conflicts = append(result.Conflicts, ref)
		case PullActionWrite:
			if err := os.MkdirAll(filepath.Dir(item.target), 0o777); err != nil {
				return result, err
			}
			if err := os.WriteFile(item.target, []byte(action.Content), 0o666); err != nil {
				return result, err
			}
			result.Written = append(result.Written, ref)
		case PullActionDelete:
			if err := os.Remove(item.target); err != nil && !os.IsNotExist(err) {
				return result, err
			}
			if err := pruneEmptyParents(filepath.Dir(item.target), item.boundary); err != nil {
				return result, err
			}
			result.Deleted = append(result.Deleted, ref)
		}
	}

	return result, nil
}

func applyPullActions(paths core.HomerPaths, config core.HomerConfig, plan PullPlan, options ...any) (ApplyResult, error) {
	return ApplyPullActions(paths, config, plan, options...)
}

func parseApplyOptions(options []any) (bool, string, error) {
	backupEnabled := true
	command := "pull"
	if len(options) == 0 || options[0] == nil {
		return backupEnabled, command, nil
	}
	if len(options) > 1 {
		return false, "", fmt.Errorf("applyPullActions: opts 只能传一个")
	}

	setBackup := func(value any) error {
		switch typed := value.(type) {
		case nil:
			return nil
		case bool:
			backupEnabled = typed
		case *bool:
			if typed != nil {
				backupEnabled = *typed
			}
		default:
			return fmt.Errorf("applyPullActions: opts.backup 类型无效")
		}
		return nil
	}

	switch value := options[0].(type) {
	case bool, *bool:
		if err := setBackup(value); err != nil {
			return false, "", err
		}
	case string:
		if value != "" {
			command = value
		}
	case ApplyPullActionsOptions:
		if err := setBackup(value.Backup); err != nil {
			return false, "", err
		}
		if value.Command != "" {
			command = value.Command
		}
	case *ApplyPullActionsOptions:
		if value != nil {
			if err := setBackup(value.Backup); err != nil {
				return false, "", err
			}
			if value.Command != "" {
				command = value.Command
			}
		}
	default:
		return false, "", fmt.Errorf("applyPullActions: opts 类型无效")
	}
	return backupEnabled, command, nil
}

type resolvedAction struct {
	kind     string
	action   PullAction
	target   string
	boundary string
}

func resolveAction(config core.HomerConfig, action PullAction) (resolvedAction, error) {
	switch action.Type {
	case PullActionConflict:
		return resolvedAction{kind: PullActionConflict, action: action}, nil
	case PullActionWrite, PullActionDelete:
		// continue below
	default:
		return resolvedAction{}, fmt.Errorf("applyPullActions: 未知 action 类型 %q", action.Type)
	}

	adapterConfig, ok := config.Adapters[action.AdapterID]
	if !ok {
		return resolvedAction{}, fmt.Errorf("applyPullActions: %s/%s 不在 homer.json 中（plan 与 config 失配）", action.AdapterID, action.Category)
	}
	categoryConfig, ok := adapterConfig.Categories[action.Category]
	if !ok {
		return resolvedAction{}, fmt.Errorf("applyPullActions: %s/%s 不在 homer.json 中（plan 与 config 失配）", action.AdapterID, action.Category)
	}

	root := core.ExpandHome(adapterConfig.Root)
	target, err := adapter.ResolveCategoryFilePath(root, categoryConfig, action.RelPath)
	if err != nil {
		return resolvedAction{}, err
	}
	item := resolvedAction{kind: action.Type, action: action, target: target}
	if action.Type == PullActionDelete {
		item.boundary = categoryDirBoundary(root, categoryConfig, action.RelPath)
	}
	return item, nil
}

func actionLabel(action PullAction) string {
	return action.AdapterID + "/" + action.Category + "/" + action.RelPath
}

func isDirPath(path string) bool { return strings.HasSuffix(path, "/") }

// categoryDirBoundary returns the directory category root, never the root
// itself. An explicit single-file declaration that matches relPath disables
// pruning, matching adapter.ResolveCategoryFilePath's last-match rule.
func categoryDirBoundary(root string, category core.CategoryConfig, relPath string) string {
	boundary := ""
	for _, declared := range category.Paths {
		declared = strings.ReplaceAll(declared, "\\", "/")
		if isDirPath(declared) {
			dir := strings.TrimRight(declared, "/")
			if dir != "" {
				boundary = filepath.Join(root, filepath.FromSlash(dir))
			}
			continue
		}
		base := declared
		if slash := strings.LastIndexByte(base, '/'); slash >= 0 {
			base = base[slash+1:]
		}
		if base == relPath {
			boundary = ""
		}
	}
	return boundary
}

func pruneEmptyParents(from, boundary string) error {
	if boundary == "" {
		return nil
	}
	current := filepath.Clean(from)
	boundary = filepath.Clean(boundary)
	for current != boundary && isUnderOrEqual(current, boundary) {
		entries, err := os.ReadDir(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return nil // inaccessible means non-empty/conservative: leave it
		}
		if len(entries) != 0 {
			return nil
		}
		if err := os.Remove(current); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
	return nil
}

func isUnderOrEqual(child, parent string) bool {
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)
	if child == parent {
		return true
	}
	prefix := parent
	if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
		prefix += string(os.PathSeparator)
	}
	return strings.HasPrefix(child, prefix)
}
