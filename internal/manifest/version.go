package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zzjcool/homer-cli/internal/core"
)

// VersionedListCommand asks a tool to print versions when its stock list
// command omits them. pi list has no such flag; npm versions are pinned
// afterwards from the installed package.json.
func VersionedListCommand(command string) string {
	if strings.TrimSpace(command) == "code --list-extensions" {
		return "code --list-extensions --show-versions"
	}
	return command
}

var npmSpecPattern = regexp.MustCompile(`^(@?[^@/]+(?:/[^@/]+)?)(?:@(.+))?$`)

// PinNpmVersions rewrites npm:name lines in a manifest snapshot to
// npm:name@version using the package.json next to the installed copy.
// An id that already names a version, or a package with no readable
// package.json, is left unchanged.
func PinNpmVersions(modulesDir string, snapshot core.CategorySnapshot) {
	if modulesDir == "" || snapshot.Files == nil {
		return
	}
	name := VirtualFileName(snapshot.Category)
	entry, ok := snapshot.Files[name]
	if !ok || strings.TrimSpace(entry.Content) == "" {
		return
	}
	ids := ParseIDs([]byte(entry.Content))
	pinned := make([]string, 0, len(ids))
	for _, id := range ids {
		pinned = append(pinned, pinNpmID(modulesDir, id))
	}
	entry.Content = ContentOf(pinned)
	snapshot.Files[name] = entry
}

func pinNpmID(modulesDir, id string) string {
	if !strings.HasPrefix(id, "npm:") {
		return id
	}
	spec := strings.TrimPrefix(id, "npm:")
	match := npmSpecPattern.FindStringSubmatch(spec)
	if match == nil {
		return id
	}
	name := match[1]
	if name == "" || strings.Contains(name, "..") {
		return id
	}
	version := readNpmPackageVersion(modulesDir, name)
	if version == "" {
		if match[2] != "" {
			return "npm:" + name + "@" + match[2]
		}
		return id
	}
	return "npm:" + name + "@" + version
}

func readNpmPackageVersion(modulesDir, name string) string {
	pkgPath := filepath.Join(modulesDir, filepath.FromSlash(name), "package.json")
	rel, err := filepath.Rel(modulesDir, filepath.Dir(pkgPath))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		return ""
	}
	var meta struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &meta) != nil {
		return ""
	}
	version := strings.TrimSpace(meta.Version)
	if !validVersionedID.MatchString("a@" + version) {
		return ""
	}
	return version
}
