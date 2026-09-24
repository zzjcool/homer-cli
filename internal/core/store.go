package core

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// StoreCompleteMarker is written last in each adapter directory. Its absence
// means a previous atomic write did not complete and the store must not be
// interpreted as an empty base.
const StoreCompleteMarker = ".homer-complete"

func assertSafeStorePath(value, what string) error {
	if value == "" {
		return fmt.Errorf("%s 不能为空", what)
	}
	if filepath.IsAbs(value) {
		return fmt.Errorf("%s 非法（不得为绝对路径或包含 '..'）: %s", what, value)
	}
	for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return fmt.Errorf("%s 非法（不得包含 '..' 段）: %s", what, value)
		}
	}
	return nil
}

// WriteSnapshotToStore replaces one adapter's store directory atomically. The
// adapter directory is cleared and rewritten as a whole, so stale files from a
// previous snapshot cannot survive an incremental-looking update.
func WriteSnapshotToStore(paths HomerPaths, snapshot AdapterSnapshot) error {
	if err := assertSafeStorePath(snapshot.AdapterID, "adapterId"); err != nil {
		return err
	}

	adapterDir := filepath.Join(paths.StoreDir, snapshot.AdapterID)
	tmpDir := fmt.Sprintf("%s.tmp-%d", adapterDir, os.Getpid())
	if err := os.MkdirAll(paths.StoreDir, 0o777); err != nil {
		return err
	}
	if err := os.RemoveAll(tmpDir); err != nil {
		return err
	}
	if err := os.MkdirAll(tmpDir, 0o777); err != nil {
		return err
	}

	cleanup := func() {
		_ = os.RemoveAll(tmpDir)
	}
	defer cleanup()

	for _, category := range snapshot.Categories {
		if err := assertSafeStorePath(category.Category, "category"); err != nil {
			return err
		}
		categoryDir := filepath.Join(tmpDir, category.Category)
		if err := os.MkdirAll(categoryDir, 0o777); err != nil {
			return err
		}

		pathsInCategory := make([]string, 0, len(category.Files))
		for relPath := range category.Files {
			pathsInCategory = append(pathsInCategory, relPath)
		}
		sort.Strings(pathsInCategory)
		for _, relPath := range pathsInCategory {
			if err := assertSafeStorePath(relPath, "relPath"); err != nil {
				return err
			}
			entry := category.Files[relPath]
			target := filepath.Join(categoryDir, filepath.FromSlash(relPath))
			if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
				return err
			}
			if err := os.WriteFile(target, []byte(entry.Content), 0o666); err != nil {
				return err
			}
		}
	}

	if err := os.WriteFile(filepath.Join(tmpDir, StoreCompleteMarker), nil, 0o666); err != nil {
		return err
	}
	if err := os.RemoveAll(adapterDir); err != nil {
		return err
	}
	if err := os.Rename(tmpDir, adapterDir); err != nil {
		return err
	}
	return nil
}

func writeSnapshotToStore(paths HomerPaths, snapshot AdapterSnapshot) error {
	return WriteSnapshotToStore(paths, snapshot)
}

func collectRelativeFiles(root string) ([]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return []string{}, nil
	}
	files := make([]string, 0)
	err = filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == root || entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func incompleteAdapterDirectory(adapterDir string) (bool, error) {
	info, err := os.Stat(adapterDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.IsDir() {
		return false, nil
	}
	markerInfo, markerErr := os.Stat(filepath.Join(adapterDir, StoreCompleteMarker))
	if markerErr == nil && !markerInfo.IsDir() {
		return false, nil
	}
	if markerErr != nil && !os.IsNotExist(markerErr) {
		return false, markerErr
	}
	return true, nil
}

// ReadSnapshotFromStore reads the enabled adapters/categories declared by
// config. Missing adapter/category directories are empty snapshots; an
// existing adapter without the completion marker is a user-facing error.
func ReadSnapshotFromStore(paths HomerPaths, config HomerConfig) ([]AdapterSnapshot, error) {
	adapterIDs := make([]string, 0, len(config.Adapters))
	for adapterID := range config.Adapters {
		adapterIDs = append(adapterIDs, adapterID)
	}
	sort.Strings(adapterIDs)

	snapshots := make([]AdapterSnapshot, 0, len(adapterIDs))
	for _, adapterID := range adapterIDs {
		adapter := config.Adapters[adapterID]
		if adapter.Enabled != nil && !*adapter.Enabled {
			continue
		}
		if err := assertSafeStorePath(adapterID, "adapterId"); err != nil {
			return nil, err
		}
		adapterDir := filepath.Join(paths.StoreDir, adapterID)
		incomplete, err := incompleteAdapterDirectory(adapterDir)
		if err != nil {
			return nil, err
		}
		if incomplete {
			return nil, NewCliError(fmt.Sprintf("store 不完整: %s 缺少 %s 标记（上次写入被中断？）", adapterDir, StoreCompleteMarker))
		}

		categoryNames := make([]string, 0, len(adapter.Categories))
		for name := range adapter.Categories {
			categoryNames = append(categoryNames, name)
		}
		sort.Strings(categoryNames)
		categories := make([]CategorySnapshot, 0, len(categoryNames))
		for _, categoryName := range categoryNames {
			category := adapter.Categories[categoryName]
			if category.Enabled != nil && !*category.Enabled {
				continue
			}
			if err := assertSafeStorePath(categoryName, "category"); err != nil {
				return nil, err
			}
			categoryDir := filepath.Join(adapterDir, categoryName)
			relPaths, err := collectRelativeFiles(categoryDir)
			if err != nil {
				return nil, err
			}
			files := make(SnapshotFiles, len(relPaths))
			for _, relPath := range relPaths {
				if relPath == StoreCompleteMarker {
					continue
				}
				if err := assertSafeStorePath(relPath, "relPath"); err != nil {
					return nil, err
				}
				content, err := os.ReadFile(filepath.Join(categoryDir, filepath.FromSlash(relPath)))
				if err != nil {
					return nil, err
				}
				text := string(content)
				files[relPath] = SnapshotEntry{Kind: entryKindFor(category.Mode, text), Content: text}
			}
			categories = append(categories, CategorySnapshot{
				AdapterID: adapterID,
				Category:  categoryName,
				Mode:      category.Mode,
				Files:     files,
			})
		}
		snapshots = append(snapshots, AdapterSnapshot{AdapterID: adapterID, Categories: categories})
	}
	return snapshots, nil
}

func readSnapshotFromStore(paths HomerPaths, config HomerConfig) ([]AdapterSnapshot, error) {
	return ReadSnapshotFromStore(paths, config)
}
