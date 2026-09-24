// Package gitx contains the deliberately small boundary between Homer and the
// git command line client.  Keeping this boundary in one package is important:
// callers can reason about git being unavailable, offline, or taking too long
// without having to handle process errors themselves.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ExecResult is the non-throwing result of a git subprocess.
type ExecResult struct {
	OK     bool
	Stdout string
	Stderr string
}

// GitDefaultTimeout is used when a caller does not provide a positive
// timeout.  It is intentionally a duration rather than an integer number of
// milliseconds so callers cannot accidentally pass a value in the wrong unit.
const GitDefaultTimeout = 15 * time.Second

// GIT_DEFAULT_TIMEOUT is retained as a plainly discoverable alias for tests
// and for callers migrating from the TypeScript constant.
const GIT_DEFAULT_TIMEOUT = GitDefaultTimeout

const (
	storePathspec = "store/"
	maxErrorBytes = 8 * 1024
)

var requiredGitignoreLines = [...]string{"state.json", "backups/", "keys/"}

// commandOutput keeps the byte form around for the two readers that need it.
type commandOutput struct {
	stdout   []byte
	stderr   []byte
	err      error
	timedOut bool
}

func effectiveTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return GitDefaultTimeout
	}
	return timeout
}

// executeGit is the only process-construction helper in this package.  It
// never invokes a shell: every caller supplies argv directly to exec.Command.
func executeGit(home string, args []string, timeout time.Duration) commandOutput {
	timeout = effectiveTimeout(timeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	if home != "" {
		cmd.Dir = home
	}
	// git aliases and hooks may spawn a shell child.  Killing only the git
	// parent on timeout leaves that child holding stdout/stderr open, which in
	// turn makes Cmd.Wait block until the child exits.  Put the command in its
	// own process group and terminate the whole group at the deadline.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Keep the timeout a hard upper bound even if a child process escapes the
	// process group and inherits one of the output descriptors.
	cmd.WaitDelay = time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	return commandOutput{
		stdout:   append([]byte(nil), stdout.Bytes()...),
		stderr:   append([]byte(nil), stderr.Bytes()...),
		err:      err,
		timedOut: errors.Is(ctx.Err(), context.DeadlineExceeded),
	}
}

func summarize(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= maxErrorBytes {
		return text
	}
	return text[:maxErrorBytes] + "…"
}

func failureText(output commandOutput, timeout time.Duration) string {
	parts := make([]string, 0, 2)
	if output.timedOut {
		// Put the timeout marker first so a very large stderr cannot truncate
		// away the fact that the process was killed by the deadline.
		parts = append(parts, fmt.Sprintf("git command timed out after %s", effectiveTimeout(timeout)))
	}
	if stderr := summarize(string(output.stderr)); stderr != "" {
		parts = append(parts, stderr)
	}
	if !output.timedOut && len(parts) == 0 && output.err != nil {
		if errors.Is(output.err, os.ErrNotExist) {
			parts = append(parts, fmt.Sprintf("git executable not found (ENOENT): %v", output.err))
		} else {
			parts = append(parts, output.err.Error())
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "git command failed")
	}
	return summarize(strings.Join(parts, "; "))
}

func resultFrom(output commandOutput, timeout time.Duration) ExecResult {
	if output.err == nil && !output.timedOut {
		return ExecResult{OK: true, Stdout: string(output.stdout)}
	}
	return ExecResult{
		OK:     false,
		Stdout: string(output.stdout),
		Stderr: failureText(output, timeout),
	}
}

// Exec executes git with home as its working directory.  It deliberately
// returns a value instead of an error: unavailable git, a non-repository, a
// failed command, and a timeout are all expected environmental outcomes for
// the higher layers.  No panic can escape this function.
func Exec(home string, args []string, timeout time.Duration) ExecResult {
	return resultFrom(executeGit(home, append([]string(nil), args...), timeout), timeout)
}

// GitExec is a descriptive alias for Exec for callers that use the gitx
// boundary name directly.
func GitExec(home string, args []string, timeout time.Duration) ExecResult {
	return Exec(home, args, timeout)
}

func gitFailure(operation string, result ExecResult) error {
	detail := summarize(result.Stderr)
	if detail == "" {
		detail = "git command failed"
	}
	return fmt.Errorf("%s: %s", operation, detail)
}

func canonicalPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(absolute)
}

func trimLine(text string) string {
	return strings.TrimRight(text, "\r\n")
}

// IsGitRepo reports whether home itself is the root of a git work tree.  A
// directory nested inside another repository is intentionally not accepted:
// Homer must never accidentally commit into an enclosing project.
func IsGitRepo(home string) bool {
	result := Exec(home, []string{"rev-parse", "--show-toplevel"}, 0)
	if !result.OK {
		return false
	}
	top := trimLine(result.Stdout)
	if top == "" {
		return false
	}
	return canonicalPath(top) == canonicalPath(home)
}

func ensureGitignore(home string) error {
	filename := filepath.Join(home, ".gitignore")
	contents, err := os.ReadFile(filename)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", filename, err)
	}

	existing := string(contents)
	present := make(map[string]struct{}, len(requiredGitignoreLines))
	for _, line := range strings.Split(existing, "\n") {
		present[strings.TrimSpace(line)] = struct{}{}
	}

	missing := make([]string, 0, len(requiredGitignoreLines))
	for _, line := range requiredGitignoreLines {
		if _, ok := present[line]; !ok {
			missing = append(missing, line)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	next := existing
	if next != "" && !strings.HasSuffix(next, "\n") {
		next += "\n"
	}
	next += strings.Join(missing, "\n") + "\n"
	if err := os.WriteFile(filename, []byte(next), 0o666); err != nil {
		return fmt.Errorf("write %s: %w", filename, err)
	}
	return nil
}

// EnsureGitRepo initializes home when necessary and maintains the three
// private/local-only .gitignore entries.  Existing user rules and bytes are
// preserved; repeated calls are byte-for-byte idempotent.
func EnsureGitRepo(home string) error {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return fmt.Errorf("create git home %s: %w", home, err)
	}

	if !IsGitRepo(home) {
		result := Exec(home, []string{"init"}, 0)
		if !result.OK {
			return gitFailure("git init", result)
		}
		if !IsGitRepo(home) {
			return fmt.Errorf("git init completed but %s is not a git work-tree", home)
		}
	}

	return ensureGitignore(home)
}

// UpstreamRef resolves the current branch's upstream, returning an empty
// string when no upstream is configured or the ref is unavailable locally.
func UpstreamRef(home string) string {
	result := Exec(home, []string{
		"rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}",
	}, 0)
	if !result.OK {
		return ""
	}
	return strings.TrimSpace(result.Stdout)
}

// HasUpstream reports whether @{upstream} resolves in the local repository.
func HasUpstream(home string) bool {
	return UpstreamRef(home) != ""
}

// HasPushTarget is deliberately a little broader than HasUpstream.  A branch
// can have branch.<name>.remote configured while its remote-tracking ref is
// temporarily absent (for example after a failed fetch).  S3 requires that
// configuration alone to count as a push target.
func HasPushTarget(home string) bool {
	if HasUpstream(home) {
		return true
	}

	branchResult := Exec(home, []string{"symbolic-ref", "--quiet", "--short", "HEAD"}, 0)
	if !branchResult.OK {
		return false
	}
	branch := strings.TrimSpace(branchResult.Stdout)
	if branch == "" {
		return false
	}

	remoteResult := Exec(home, []string{"config", "--get", "branch." + branch + ".remote"}, 0)
	return remoteResult.OK && strings.TrimSpace(remoteResult.Stdout) != ""
}

// Fetch runs git fetch using the repository's configured remotes and refspecs.
func Fetch(home string) ExecResult {
	return Exec(home, []string{"fetch"}, 0)
}

// GitFetch is the explicit git-prefixed spelling used by command-layer ports.
func GitFetch(home string) ExecResult { return Fetch(home) }

// Push runs git push using the current branch's configured push target.
func Push(home string) ExecResult {
	return Exec(home, []string{"push"}, 0)
}

// GitPush is the explicit git-prefixed spelling used by command-layer ports.
func GitPush(home string) ExecResult { return Push(home) }

// HeadCommit returns HEAD's object name, or an empty string for an unborn HEAD,
// a non-repository, or any other git failure.
func HeadCommit(home string) string {
	result := Exec(home, []string{"rev-parse", "HEAD"}, 0)
	if !result.OK {
		return ""
	}
	return strings.TrimSpace(result.Stdout)
}

func pathspecStatus(home string, pathspecs []string) (bool, bool) {
	if len(pathspecs) == 0 {
		return false, true
	}
	args := make([]string, 0, 3+len(pathspecs))
	args = append(args, "status", "--porcelain", "--")
	args = append(args, pathspecs...)
	result := Exec(home, args, 0)
	if !result.OK {
		return false, false
	}
	return strings.TrimSpace(result.Stdout) != "", true
}

// CommitAllStore stages and commits only store/.  An empty or unobservable
// store change returns an empty string and never creates an empty commit.
func CommitAllStore(home, message string) string {
	changed, statusOK := pathspecStatus(home, []string{storePathspec})
	if !statusOK || !changed {
		return ""
	}

	add := Exec(home, []string{"add", "-A", "--", storePathspec}, 0)
	if !add.OK {
		return ""
	}
	commit := Exec(home, []string{"commit", "-m", message, "--", storePathspec}, 0)
	if !commit.OK {
		return ""
	}
	return HeadCommit(home)
}

// CommitPaths is the path-scoped commit primitive used by the secrets
// pipeline.  It intentionally shares no pathspec with CommitAllStore.
func CommitPaths(home string, pathspecs []string, message string) string {
	changed, statusOK := pathspecStatus(home, pathspecs)
	if !statusOK || !changed {
		return ""
	}

	addArgs := make([]string, 0, 3+len(pathspecs))
	addArgs = append(addArgs, "add", "-A", "--")
	addArgs = append(addArgs, pathspecs...)
	if result := Exec(home, addArgs, 0); !result.OK {
		return ""
	}

	commitArgs := make([]string, 0, 4+len(pathspecs))
	commitArgs = append(commitArgs, "commit", "-m", message, "--")
	commitArgs = append(commitArgs, pathspecs...)
	if result := Exec(home, commitArgs, 0); !result.OK {
		return ""
	}
	return HeadCommit(home)
}

// MergeFFUpstream only permits a fast-forward, leaving a divergent HEAD
// untouched and returning git's stderr for the caller to report.
func MergeFFUpstream(home string) ExecResult {
	return Exec(home, []string{"merge", "--ff-only", "@{upstream}"}, 0)
}

// IsStoreClean checks only store/.  state.json, backups/, and unrelated files
// do not participate in this gate.
func IsStoreClean(home string) bool {
	changed, statusOK := pathspecStatus(home, []string{storePathspec})
	return statusOK && !changed
}

// IsAncestorOf delegates the graph relation to git.  Invalid refs and all
// other git failures are simply false.
func IsAncestorOf(home, ancestor, descendant string) bool {
	result := Exec(home, []string{"merge-base", "--is-ancestor", ancestor, descendant}, 0)
	return result.OK
}

// AssertCloneableRepoURL is the second, explicit defence against git option
// injection.  CloneRepo also passes -- before the untrusted URL, but callers
// should not have to rely on that implementation detail.
func AssertCloneableRepoURL(url string) error {
	if strings.HasPrefix(url, "-") {
		return fmt.Errorf("repository URL must not start with '-'")
	}
	return nil
}

// CloneRepo invokes git clone with the frozen argv shape
// ["clone", "--", url, dest].  It uses the same timeout/error-summary policy
// as Exec while returning an error because cloning is a user-facing setup
// operation rather than a probe.
func CloneRepo(url, dest string, timeout time.Duration) error {
	if err := AssertCloneableRepoURL(url); err != nil {
		return err
	}
	if parent := filepath.Dir(dest); parent != "." && parent != "" {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("prepare clone destination %s: %w", parent, err)
		}
	}

	args := []string{"clone", "--", url, dest}
	output := executeGit("", args, timeout)
	if output.err != nil || output.timedOut {
		return fmt.Errorf("git clone: %s", failureText(output, timeout))
	}
	return nil
}
