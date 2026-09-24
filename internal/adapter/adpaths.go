package adapter

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zzjcool/homer-cli/internal/core"
)

var windowsAbsolutePath = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

func isDeclaredDir(path string) bool { return strings.HasSuffix(path, "/") }

func stripTrailingSlashes(path string) string {
	return strings.TrimRight(path, "/")
}

func assertFileRelPath(relPath string) error {
	if relPath == "" {
		return fmt.Errorf("resolve category file path: relPath 不能为空")
	}
	if filepath.IsAbs(relPath) || strings.HasPrefix(relPath, "/") || windowsAbsolutePath.MatchString(relPath) {
		return fmt.Errorf("resolve category file path: relPath 非法（不得为绝对路径）: %s", relPath)
	}
	for _, segment := range strings.FieldsFunc(relPath, func(r rune) bool { return r == '/' || r == '\\' }) {
		if segment == ".." {
			return fmt.Errorf("resolve category file path: relPath 非法（不得包含 '..' 段）: %s", relPath)
		}
	}
	if strings.HasSuffix(relPath, "/") {
		return fmt.Errorf("resolve category file path: relPath 非法（不得以 '/' 结尾，那不是文件路径）: %s", relPath)
	}
	return nil
}

// ResolveCategoryFilePath maps a store-side category relPath back to the
// adapter's absolute file path.  It is the inverse of scan's two relPath
// rules: directory declarations retain a path below that directory, while
// single-file declarations use only their basename.
func ResolveCategoryFilePath(root string, category core.CategoryConfig, relPath string) (string, error) {
	if err := assertFileRelPath(relPath); err != nil {
		return "", err
	}

	// The last matching declaration wins, matching scan's map overwrite
	// behavior when a category declares overlapping paths.
	matched := ""
	matchedDir := false
	for _, declared := range category.Paths {
		declared = normalizeRel(declared)
		if isDeclaredDir(declared) {
			if stripTrailingSlashes(declared) == "" {
				continue
			}
			matched = declared
			matchedDir = true
			continue
		}
		if basenameOf(declared) == relPath {
			matched = declared
			matchedDir = false
		}
	}
	if matched == "" {
		return "", fmt.Errorf(
			"resolve category file path: relPath %q 无法映射到 category.paths %v 中的任何一项",
			relPath, category.Paths,
		)
	}
	if !matchedDir {
		return filepath.Join(root, filepath.FromSlash(matched)), nil
	}
	return filepath.Join(root, filepath.FromSlash(stripTrailingSlashes(matched)), filepath.FromSlash(relPath)), nil
}

// ResolveFilePath is a short alias for callers that do not need the longer
// category-oriented name.
func ResolveFilePath(root string, category core.CategoryConfig, relPath string) (string, error) {
	return ResolveCategoryFilePath(root, category, relPath)
}
