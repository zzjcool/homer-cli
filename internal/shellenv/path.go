// Package shellenv resolves the command PATH for a long-running homer
// process. The login shell PATH is cached for 30 seconds to avoid repeatedly
// starting a shell for each command lookup. This trades freshness for speed:
// newly installed tools become visible after the TTL expires or when
// Invalidate is called.
package shellenv

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ReadLoginPATH is called when the cached login PATH is missing or expired.
// Tests replace it. An empty result (including a failed read) is not cached.
// A nil func skips the login shell and uses the process environment.
var ReadLoginPATH = readLoginPATH

// LoginPATHTTL is how long a successful login-shell PATH is cached.
var LoginPATHTTL = 30 * time.Second

// loginPATHNow is kept injectable so cache expiry can be tested without sleep.
var loginPATHNow = time.Now

type loginPATHFlight struct {
	done       chan struct{}
	generation uint64
	path       string
}

var loginPATHCache struct {
	sync.Mutex
	path       string
	expiresAt  time.Time
	valid      bool
	generation uint64
	flight     *loginPATHFlight
}

// Path is the PATH a command should use right now.
func Path() string {
	home, _ := os.UserHomeDir()
	base := os.Getenv("PATH")
	if read := ReadLoginPATH; read != nil {
		if fresh := cachedLoginPATH(read); fresh != "" {
			base = fresh
		}
	}
	return commandPATH(base, home, os.Getenv("HOMEBREW_PREFIX"), dirExists)
}

// Invalidate discards the cached login PATH. The next Path call will read the
// login shell again. An in-progress read may finish for its existing callers,
// but cannot repopulate the cache after invalidation.
func Invalidate() {
	loginPATHCache.Lock()
	loginPATHCache.path = ""
	loginPATHCache.expiresAt = time.Time{}
	loginPATHCache.valid = false
	loginPATHCache.generation++
	loginPATHCache.flight = nil
	loginPATHCache.Unlock()
}

func cachedLoginPATH(read func() string) string {
	now := loginPATHNow()
	loginPATHCache.Lock()
	if LoginPATHTTL > 0 && loginPATHCache.valid && now.Before(loginPATHCache.expiresAt) {
		path := loginPATHCache.path
		loginPATHCache.Unlock()
		return path
	}
	loginPATHCache.valid = false
	if flight := loginPATHCache.flight; flight != nil {
		loginPATHCache.Unlock()
		<-flight.done
		return flight.path
	}
	flight := &loginPATHFlight{
		done:       make(chan struct{}),
		generation: loginPATHCache.generation,
	}
	loginPATHCache.flight = flight
	loginPATHCache.Unlock()

	var path string
	defer func() {
		// Even if a replacement reader panics, wake callers waiting on this
		// singleflight. The panic still propagates to the caller that read.
		readAt := loginPATHNow()
		loginPATHCache.Lock()
		if path != "" && LoginPATHTTL > 0 && loginPATHCache.generation == flight.generation && loginPATHCache.flight == flight {
			loginPATHCache.path = path
			loginPATHCache.expiresAt = readAt.Add(LoginPATHTTL)
			loginPATHCache.valid = true
		}
		flight.path = path
		if loginPATHCache.flight == flight {
			loginPATHCache.flight = nil
		}
		close(flight.done)
		loginPATHCache.Unlock()
	}()
	path = strings.TrimSpace(read())
	return path
}

// Look resolves file against Path. A path that already contains a slash is
// returned unchanged. Go's exec.LookPath only sees the process PATH.
func Look(file string) (string, error) {
	return LookIn(file, Path())
}

// LookIn resolves file against an explicit PATH string.
func LookIn(file, path string) (string, error) {
	if file == "" || strings.Contains(file, "/") {
		return file, nil
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, file)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("exec: %q: executable file not found in $PATH", file)
}

// Resolve finds file and returns the environment the child should inherit,
// with PATH replaced by the freshly read value.
func Resolve(file string, env []string) (string, []string, error) {
	path := Path()
	bin, err := LookIn(file, path)
	if err != nil {
		return "", nil, err
	}
	return bin, setEnv(env, "PATH", path), nil
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			if !replaced {
				out = append(out, prefix+value)
				replaced = true
			}
			continue
		}
		out = append(out, entry)
	}
	if !replaced {
		out = append(out, prefix+value)
	}
	return out
}

func readLoginPATH() string {
	// Test binaries import testing, which registers test.v. A login shell
	// on every git call would dominate the suite; production has no such flag.
	if flag.Lookup("test.v") != nil {
		return ""
	}
	shell := strings.TrimSpace(os.Getenv("SHELL"))
	if shell == "" {
		shell = "/bin/bash"
	}
	if _, err := os.Stat(shell); err != nil {
		return ""
	}
	script := `printf '%s\n' "__HOMER_PATH__$PATH"`
	args := []string{"-lic", script}
	if strings.Contains(strings.ToLower(filepath.Base(shell)), "fish") {
		args = []string{"-l", "-c", script}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, args...)
	home, _ := os.UserHomeDir()
	cmd.Env = []string{
		"HOME=" + home,
		"USER=" + os.Getenv("USER"),
		"LOGNAME=" + os.Getenv("LOGNAME"),
		"SHELL=" + shell,
		"PATH=" + os.Getenv("PATH"),
		"TERM=dumb",
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return parseLoginPATH(string(out))
}

func parseLoginPATH(output string) string {
	const marker = "__HOMER_PATH__"
	index := strings.LastIndex(output, marker)
	if index < 0 {
		return ""
	}
	value := output[index+len(marker):]
	if newline := strings.IndexByte(value, '\n'); newline >= 0 {
		value = value[:newline]
	}
	return strings.TrimSpace(value)
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func commandPATH(base, home, brewPrefix string, exists func(string) bool) string {
	if exists == nil {
		exists = dirExists
	}
	candidates := make([]string, 0, 6)
	if brewPrefix != "" {
		candidates = append(candidates, filepath.Join(brewPrefix, "bin"))
	}
	candidates = append(candidates, "/home/linuxbrew/.linuxbrew/bin", "/opt/homebrew/bin")
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".linuxbrew", "bin"),
			filepath.Join(home, ".local", "share", "mise", "shims"),
			filepath.Join(home, ".local", "bin"),
		)
	}
	seen := map[string]struct{}{}
	var dirs []string
	add := func(dir string) {
		if dir == "" {
			return
		}
		if _, ok := seen[dir]; ok {
			return
		}
		seen[dir] = struct{}{}
		if !exists(dir) {
			return
		}
		dirs = append(dirs, dir)
	}
	for _, dir := range candidates {
		add(dir)
	}
	for _, dir := range filepath.SplitList(base) {
		add(dir)
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}
