package gitx

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/core"
)

const storeTreePrefix = "store/"

func listStoreFiles(paths core.HomerPaths, commitish string) []string {
	result := Exec(paths.Home, []string{
		"ls-tree", "-r", "--name-only", "-z", commitish, "--", storeTreePrefix,
	}, 0)
	if !result.OK || result.Stdout == "" {
		return []string{}
	}

	files := make([]string, 0)
	for _, name := range strings.Split(result.Stdout, "\x00") {
		if name != "" {
			files = append(files, name)
		}
	}
	return files
}

func readBlob(paths core.HomerPaths, commitish, storePath string) (string, bool) {
	result := Exec(paths.Home, []string{"show", commitish + ":" + storePath}, 0)
	if !result.OK {
		return "", false
	}
	return result.Stdout, true
}

func sortedMapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func enabled(value *bool) bool {
	return value == nil || *value
}

func snapshotEntryKind(mode core.SyncMode, content string) string {
	// Delegate validity to core's single entry-kind implementation so the
	// working-tree scanner and this historical reader cannot drift apart.
	return core.EntryKindFor(mode, content)
}

// ReadStoreSnapshotAtCommit reads the store tree from a git object without
// consulting the working tree.  It returns a config-shaped result even when
// the commit/ref is unavailable: each enabled category then has an empty
// SnapshotFiles map, which is the frozen missing-base/missing-remote
// semantics.
func ReadStoreSnapshotAtCommit(paths core.HomerPaths, cfg *core.HomerConfig, commitish string) []core.AdapterSnapshot {
	if cfg == nil {
		return []core.AdapterSnapshot{}
	}

	storeFiles := listStoreFiles(paths, commitish)
	adapters := make([]core.AdapterSnapshot, 0, len(cfg.Adapters))

	for _, adapterID := range sortedMapKeys(cfg.Adapters) {
		adapter := cfg.Adapters[adapterID]
		if !enabled(adapter.Enabled) {
			continue
		}

		categories := make([]core.CategorySnapshot, 0, len(adapter.Categories))
		for _, categoryName := range sortedMapKeys(adapter.Categories) {
			category := adapter.Categories[categoryName]
			if !enabled(category.Enabled) {
				continue
			}

			prefix := storeTreePrefix + adapterID + "/" + categoryName + "/"
			matched := make(map[string]string)
			for _, storePath := range storeFiles {
				if !strings.HasPrefix(storePath, prefix) {
					continue
				}
				relPath := strings.TrimPrefix(storePath, prefix)
				if relPath == "" || relPath == ".homer-complete" {
					continue
				}
				matched[relPath] = storePath
			}

			files := make(core.SnapshotFiles, len(matched))
			for _, relPath := range sortedMapKeys(matched) {
				storePath := matched[relPath]
				content, ok := readBlob(paths, commitish, storePath)
				if !ok {
					continue
				}
				files[relPath] = core.SnapshotEntry{
					Kind:    snapshotEntryKind(category.Mode, content),
					Content: content,
				}
			}

			categories = append(categories, core.CategorySnapshot{
				AdapterID: adapterID,
				Category:  categoryName,
				Mode:      category.Mode,
				Files:     files,
			})
		}

		adapters = append(adapters, core.AdapterSnapshot{
			AdapterID:  adapterID,
			Categories: categories,
		})
	}

	return adapters
}

// ReadVaultFileAtCommit reads one git blob as bytes.  This path intentionally
// bypasses Exec's string conversion: age ciphertext is binary and invalid
// UTF-8 must not be replaced.  A committed empty blob returns a non-nil,
// zero-length slice, while any missing object returns an error.
func ReadVaultFileAtCommit(home, relPath, commitish string) ([]byte, error) {
	output := executeGit(home, []string{"show", commitish + ":" + relPath}, 0)
	if output.err != nil || output.timedOut {
		return nil, fmt.Errorf("git show %s: %s", relPath, failureText(output, GitDefaultTimeout))
	}

	contents := make([]byte, len(output.stdout))
	copy(contents, output.stdout)
	return contents, nil
}
