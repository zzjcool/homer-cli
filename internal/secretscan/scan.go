package secretscan

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/core"
)

// SecretFinding is one line-level detector hit. Path is the store-relative
// adapter/category/path form used by push reports and ignorePaths.
type SecretFinding struct {
	PatternID   string `json:"patternId"`
	Description string `json:"description"`
	Path        string `json:"path"`
	Line        int    `json:"line"`
	Excerpt     string `json:"excerpt"`
}

const maskKeep = 4

// maskSecret keeps four leading and trailing characters and replaces the
// middle with stars. Detectors all find substantially longer values, but the
// short-value branch is intentionally total for direct callers.
func maskSecret(secret string) string {
	if len(secret) <= maskKeep*2 {
		return strings.Repeat("*", len(secret))
	}
	return secret[:maskKeep] + strings.Repeat("*", len(secret)-maskKeep*2) + secret[len(secret)-maskKeep:]
}

// MaskSecret is the exported defensive helper used by tests and renderers.
func MaskSecret(secret string) string { return maskSecret(secret) }

func maskLine(line, secret string) string {
	return strings.ReplaceAll(line, secret, maskSecret(secret))
}

// scanContent scans text line by line. Pattern order is significant and each
// line contributes at most one finding, matching the TypeScript scanner's
// "first detector wins" contract.
func scanContent(content, path string) []SecretFinding {
	if content == "" {
		return []SecretFinding{}
	}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	lines := strings.Split(content, "\n")
	findings := make([]SecretFinding, 0)
	for index, line := range lines {
		if line == "" {
			continue
		}
		for _, detector := range SECRET_PATTERNS {
			matched := detector.Regex.FindString(line)
			if matched == "" {
				continue
			}
			findings = append(findings, SecretFinding{
				PatternID:   detector.ID,
				Description: detector.Description,
				Path:        path,
				Line:        index + 1,
				Excerpt:     maskLine(line, matched),
			})
			break
		}
	}
	return findings
}

// ScanContent is the exported scanner entry point.
func ScanContent(content, path string) []SecretFinding { return scanContent(content, path) }

// scanSnapshots scans adapter snapshots in caller-provided adapter/category
// order and sorts each category's file names to make output deterministic
// regardless of map insertion order.
func scanSnapshots(snapshots []core.AdapterSnapshot) []SecretFinding {
	findings := make([]SecretFinding, 0)
	for _, snapshot := range snapshots {
		for _, category := range snapshot.Categories {
			paths := make([]string, 0, len(category.Files))
			for relPath := range category.Files {
				paths = append(paths, relPath)
			}
			sort.Strings(paths)
			for _, relPath := range paths {
				entry, ok := category.Files[relPath]
				if !ok {
					continue
				}
				storePath := filepath.ToSlash(filepath.Join(snapshot.AdapterID, category.Category, relPath))
				findings = append(findings, scanContent(entry.Content, storePath)...)
			}
		}
	}
	return findings
}

// ScanSnapshots is the exported snapshot scanner.
func ScanSnapshots(snapshots []core.AdapterSnapshot) []SecretFinding { return scanSnapshots(snapshots) }

func normalizeGlob(input string) string {
	for strings.HasPrefix(input, "./") {
		input = strings.TrimPrefix(input, "./")
	}
	for strings.HasPrefix(input, "/") {
		input = strings.TrimPrefix(input, "/")
	}
	return input
}

func globBody(pattern string) string {
	var builder strings.Builder
	for _, character := range pattern {
		if character == '*' {
			builder.WriteString("[^/]*")
			continue
		}
		if strings.ContainsRune(`\\.+?^$(){}|[]`, character) {
			builder.WriteByte('\\')
		}
		builder.WriteRune(character)
	}
	return builder.String()
}

// globMatch implements the frozen small glob language: literals plus *, where
// * never crosses '/', and a trailing slash means a recursive directory
// prefix. ** is intentionally just two single-star wildcards, also not
// recursive across slash boundaries.
func globMatch(candidate, pattern string) bool {
	candidate = normalizeGlob(candidate)
	pattern = normalizeGlob(pattern)
	if pattern == "" {
		return false
	}
	if strings.HasSuffix(pattern, "/") {
		prefix := strings.TrimSuffix(pattern, "/")
		if prefix == "" {
			return false
		}
		return globMatchExact(candidate, prefix) || globMatchPrefix(candidate, prefix)
	}
	return globMatchExact(candidate, pattern)
}

func globMatchExact(candidate, pattern string) bool {
	// This compact dynamic matcher avoids importing a general-purpose glob and
	// exactly models [^/]* (including an empty match).
	candidateRunes := []rune(candidate)
	patternRunes := []rune(pattern)
	memo := make(map[[2]int]bool)
	seen := make(map[[2]int]bool)
	var match func(int, int) bool
	match = func(candidateIndex, patternIndex int) bool {
		key := [2]int{candidateIndex, patternIndex}
		if seen[key] {
			return memo[key]
		}
		seen[key] = true
		var result bool
		switch {
		case patternIndex == len(patternRunes):
			result = candidateIndex == len(candidateRunes)
		case patternRunes[patternIndex] == '*':
			result = match(candidateIndex, patternIndex+1)
			if !result && candidateIndex < len(candidateRunes) && candidateRunes[candidateIndex] != '/' {
				result = match(candidateIndex+1, patternIndex)
			}
		case candidateIndex < len(candidateRunes) && patternRunes[patternIndex] == candidateRunes[candidateIndex]:
			result = match(candidateIndex+1, patternIndex+1)
		default:
			result = false
		}
		memo[key] = result
		return result
	}
	return match(0, 0)
}

func globMatchPrefix(candidate, prefix string) bool {
	candidateRunes := []rune(candidate)
	prefixRunes := []rune(prefix)
	if len(candidateRunes) < len(prefixRunes) {
		return false
	}
	for end := range candidateRunes {
		if candidateRunes[end] != '/' {
			continue
		}
		if globMatchExact(string(candidateRunes[:end]), prefix) {
			return true
		}
	}
	return false
}

func matchesIgnore(path string, patterns []string) bool {
	for _, pattern := range patterns {
		if globMatch(path, pattern) {
			return true
		}
	}
	return false
}

// filterIgnored drops findings whose complete store-relative path matches any
// configured ignore glob. It always returns a new slice.
func filterIgnored(findings []SecretFinding, ignorePaths []string) []SecretFinding {
	filtered := make([]SecretFinding, 0, len(findings))
	for _, finding := range findings {
		if !matchesIgnore(finding.Path, ignorePaths) {
			filtered = append(filtered, finding)
		}
	}
	return filtered
}

// FilterIgnored is the exported ignore filter.
func FilterIgnored(findings []SecretFinding, ignorePaths []string) []SecretFinding {
	return filterIgnored(findings, ignorePaths)
}
