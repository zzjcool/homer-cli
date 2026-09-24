package adapter

import "strings"

// normalizeGlobPath applies the path normalization frozen by the TypeScript
// adapter.  Adapter paths are POSIX-style paths even when the process runs on
// a platform whose native separator is different.  Leading ./ and / are
// ignored; a trailing / is deliberately retained because it means "directory
// prefix".
func normalizeGlobPath(input string) string {
	out := input
	for strings.HasPrefix(out, "./") {
		out = strings.TrimPrefix(out, "./")
	}
	for strings.HasPrefix(out, "/") {
		out = strings.TrimPrefix(out, "/")
	}
	return out
}

// GlobMatch reports whether candidate matches pattern using the adapter glob
// grammar:
//
//   - all characters other than * are literals;
//   - * matches zero or more characters other than /;
//   - a trailing / turns the pattern into a directory-prefix match;
//   - ? and ** have no special meaning (two stars are simply two * tokens).
//
// This intentionally is not filepath.Match or a general purpose glob
// implementation.  In particular, a star never crosses a path separator.
func GlobMatch(candidate, pattern string) bool {
	path := normalizeGlobPath(candidate)
	pat := normalizeGlobPath(pattern)
	if pat == "" {
		return false
	}

	if strings.HasSuffix(pat, "/") {
		prefix := strings.TrimSuffix(pat, "/")
		if prefix == "" {
			return false
		}
		if globMatchExact(path, prefix) {
			return true
		}
		// The TypeScript implementation is equivalent to
		// ^body(?:/.*)?$.  Try every candidate slash as the optional
		// suffix boundary.  globMatchExact still prevents * from
		// consuming a slash in the prefix.
		for i := 0; i < len(path); i++ {
			if path[i] == '/' && globMatchExact(path[:i], prefix) {
				return true
			}
		}
		return false
	}

	return globMatchExact(path, pat)
}

// MatchesIgnore reports whether any pattern matches relPath.
func MatchesIgnore(relPath string, patterns []string) bool {
	for _, pattern := range patterns {
		if GlobMatch(relPath, pattern) {
			return true
		}
	}
	return false
}

// globMatchExact matches one complete path string.  The implementation is a
// small wildcard matcher rather than a regexp compiler, so literals can never
// accidentally acquire regexp meaning.  The only wildcard is * and it is
// constrained to a single path segment.
func globMatchExact(candidate, pattern string) bool {
	// A standard two-pointer wildcard matcher is sufficient here.  Since * is
	// not allowed to consume '/', each star can backtrack only within the
	// current segment.
	ci, pi := 0, 0
	star := -1
	starCandidate := -1

	for ci < len(candidate) {
		if pi < len(pattern) && pattern[pi] != '*' && pattern[pi] == candidate[ci] {
			ci++
			pi++
			continue
		}
		if pi < len(pattern) && pattern[pi] == '*' {
			star = pi
			starCandidate = ci
			pi++
			continue
		}
		if star >= 0 && starCandidate < len(candidate) && candidate[starCandidate] != '/' {
			starCandidate++
			ci = starCandidate
			pi = star + 1
			continue
		}
		return false
	}

	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// IsBareGlob reports the four broad allowEscape spellings rejected by config
// validation in the W4 core worker.  The scan itself intentionally does not
// reject them: matching semantics belong here, while policy validation belongs
// to config.go.
func IsBareGlob(pattern string) bool {
	switch normalizeGlobPath(pattern) {
	case "*", "*/", "**", "**/":
		return true
	default:
		return false
	}
}

// Lower-case aliases keep the package convenient for tests in package adapter
// while the exported forms are used by the other adapter packages.
func globMatch(candidate, pattern string) bool { return GlobMatch(candidate, pattern) }
func matchesIgnore(relPath string, patterns []string) bool {
	return MatchesIgnore(relPath, patterns)
}
