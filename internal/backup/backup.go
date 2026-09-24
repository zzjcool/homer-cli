// Package backup implements the on-disk backup tree used before applying
// remote changes to an adapter. Backups are deliberately outside the store.
package backup

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zzjcool/homer-cli/internal/core"
)

// BackupTarget maps an existing source path to a controlled, store-relative
// label in the backup directory.
type BackupTarget struct {
	SourceAbs string
	Label     string
}

// BackupResult reports the exact backup directory and target outcomes.
type BackupResult struct {
	BackupDir string
	BackedUp  []string
	Skipped   []string
}

// BackupFileModes optionally tightens permissions on the backup tree. A zero
// field means "do not change that kind of entry"; this makes the ordinary
// (unspecified) path preserve the platform's default mode and umask behavior.
type BackupFileModes struct {
	Dir  fs.FileMode
	File fs.FileMode
}

// mode is the package-local spelling used by the frozen opts.mode notation.
// Its fields mirror BackupFileModes; it is intentionally not exported because
// callers outside this package should use the explicit type above.
type mode struct {
	dir  fs.FileMode
	file fs.FileMode
}

// BackupFilesOptions is accepted as an optional argument to BackupFiles. Mode
// may be either BackupFileModes or *BackupFileModes; accepting both keeps call
// sites concise while retaining an explicit opt-in for secret backups.
type BackupFilesOptions struct {
	Mode any
	mode any // package-local compatibility spelling; exported callers use Mode
}

const DefaultBackupKeep = 7

// DEFAULT_BACKUP_KEEP preserves the migration-facing constant spelling.
const DEFAULT_BACKUP_KEEP = DefaultBackupKeep

// BackupMode and BackupOptions are concise aliases for callers that use the
// frozen opts.mode terminology.
type BackupMode = BackupFileModes
type BackupOptions = BackupFilesOptions

var dateDirectoryPattern = regexp.MustCompile(`^\d{8}$`)

func twoDigits(value int) string {
	if value < 10 {
		return "0" + fmt.Sprint(value)
	}
	return fmt.Sprint(value)
}

func dateStamp(now time.Time) string {
	return fmt.Sprintf("%04d%s%s", now.Year(), twoDigits(int(now.Month())), twoDigits(now.Day()))
}

func timeStamp(now time.Time) string {
	return twoDigits(now.Hour()) + twoDigits(now.Minute()) + twoDigits(now.Second())
}

func assertSafeSegment(value, label string) error {
	if value == "" {
		return fmt.Errorf("%s 不能为空", label)
	}
	if filepath.IsAbs(value) {
		return fmt.Errorf("%s 非法（不得为绝对路径或包含 '..'）: %s", label, value)
	}
	for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return fmt.Errorf("%s 非法（不得包含 '..' 段）: %s", label, value)
		}
	}
	return nil
}

func modeFromOptions(options []any) (BackupFileModes, bool, error) {
	if len(options) == 0 || options[0] == nil {
		return BackupFileModes{}, false, nil
	}
	if len(options) > 1 {
		return BackupFileModes{}, false, fmt.Errorf("backupFiles: opts 只能传一个")
	}
	switch value := options[0].(type) {
	case BackupFilesOptions:
		if value.Mode != nil {
			return modeFromOptions([]any{value.Mode})
		}
		return modeFromOptions([]any{value.mode})
	case *BackupFilesOptions:
		if value == nil {
			return BackupFileModes{}, false, nil
		}
		if value.Mode != nil {
			return modeFromOptions([]any{value.Mode})
		}
		return modeFromOptions([]any{value.mode})
	case mode:
		return BackupFileModes{Dir: value.dir, File: value.file}, true, nil
	case *mode:
		if value == nil {
			return BackupFileModes{}, false, nil
		}
		return BackupFileModes{Dir: value.dir, File: value.file}, true, nil
	case BackupFileModes:
		return value, true, nil
	case *BackupFileModes:
		if value == nil {
			return BackupFileModes{}, false, nil
		}
		return *value, true, nil
	default:
		return BackupFileModes{}, false, fmt.Errorf("backupFiles: opts.mode 类型无效")
	}
}

func chmodIfPresent(name string, mode fs.FileMode) error {
	if mode == 0 {
		return nil
	}
	if err := os.Chmod(name, mode.Perm()); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return nil
}

func applyModes(root string, modes BackupFileModes) error {
	if err := chmodIfPresent(root, modes.Dir); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == root {
			return nil
		}
		if entry.IsDir() {
			return chmodIfPresent(name, modes.Dir)
		}
		return chmodIfPresent(name, modes.File)
	})
}

func copyPath(source, destination string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.MkdirAll(destination, 0o777); err != nil {
			return err
		}
		return filepath.WalkDir(source, func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(source, name)
			if err != nil {
				return err
			}
			if relative == "." {
				return nil
			}
			target := filepath.Join(destination, relative)
			if entry.IsDir() {
				return os.MkdirAll(target, 0o777)
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("不支持备份非普通文件: %s", name)
			}
			return copyRegularFile(name, target)
		})
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("不支持备份非普通文件: %s", source)
	}
	return copyRegularFile(source, destination)
}

func copyRegularFile(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o777); err != nil {
		return err
	}
	return os.WriteFile(destination, data, 0o666)
}

// BackupFiles creates <backups>/<YYYYMMDD>/<HHmmss>-<command>/<label> and
// copies all existing targets. Missing sources are reported in Skipped.
// Validation is completed before the first mkdir, so invalid labels leave no
// partial backup tree. The function returns an error for I/O and path errors.
func BackupFiles(paths core.HomerPaths, command string, targets []BackupTarget, options ...any) (BackupResult, error) {
	if err := assertSafeSegment(command, "command"); err != nil {
		return BackupResult{}, err
	}
	for _, target := range targets {
		if err := assertSafeSegment(target.Label, "label"); err != nil {
			return BackupResult{}, err
		}
	}
	modes, useModes, err := modeFromOptions(options)
	if err != nil {
		return BackupResult{}, err
	}

	now := time.Now()
	backupDir := filepath.Join(paths.BackupsDir, dateStamp(now), timeStamp(now)+"-"+command)
	if err := os.MkdirAll(backupDir, 0o777); err != nil {
		return BackupResult{}, err
	}

	result := BackupResult{BackupDir: backupDir, BackedUp: make([]string, 0, len(targets)), Skipped: make([]string, 0)}
	for _, target := range targets {
		if _, err := os.Stat(target.SourceAbs); err != nil {
			if os.IsNotExist(err) {
				result.Skipped = append(result.Skipped, target.Label)
				continue
			}
			return BackupResult{}, err
		}
		if err := copyPath(target.SourceAbs, filepath.Join(backupDir, filepath.FromSlash(target.Label))); err != nil {
			return BackupResult{}, err
		}
		result.BackedUp = append(result.BackedUp, target.Label)
	}

	if useModes {
		if modes.Dir != 0 {
			if err := chmodIfPresent(paths.BackupsDir, modes.Dir); err != nil {
				return BackupResult{}, err
			}
			if err := chmodIfPresent(filepath.Dir(backupDir), modes.Dir); err != nil {
				return BackupResult{}, err
			}
		}
		if err := applyModes(backupDir, modes); err != nil {
			return BackupResult{}, err
		}
	}
	return result, nil
}

// backupFiles is the package-local spelling used by migration-oriented tests.
func backupFiles(paths core.HomerPaths, command string, targets []BackupTarget, options ...any) (BackupResult, error) {
	return BackupFiles(paths, command, targets, options...)
}

// PruneResult contains date-directory names relative to BackupsDir.
type PruneResult struct {
	Removed []string
	Kept    []string
}

// PruneBackups retains the newest keep date directories. If keep is omitted,
// seven dates are retained. Non-date entries are intentionally untouched.
func PruneBackups(paths core.HomerPaths, keep ...int) (PruneResult, error) {
	retention := DefaultBackupKeep
	if len(keep) > 1 {
		return PruneResult{}, fmt.Errorf("pruneBackups: keep 只能传一个")
	}
	if len(keep) == 1 {
		retention = keep[0]
	}
	if retention < 0 {
		return PruneResult{}, fmt.Errorf("pruneBackups: keep 必须是非负整数（当前: %d）", retention)
	}

	entries, err := os.ReadDir(paths.BackupsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return PruneResult{Removed: []string{}, Kept: []string{}}, nil
		}
		return PruneResult{}, err
	}
	names := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() && dateDirectoryPattern.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	kept := append([]string(nil), names[:min(retention, len(names))]...)
	removed := append([]string(nil), names[min(retention, len(names)):]...)
	for _, name := range removed {
		if err := os.RemoveAll(filepath.Join(paths.BackupsDir, name)); err != nil {
			return PruneResult{}, err
		}
	}
	return PruneResult{Removed: removed, Kept: kept}, nil
}

func pruneBackups(paths core.HomerPaths, keep ...int) (PruneResult, error) {
	return PruneBackups(paths, keep...)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
