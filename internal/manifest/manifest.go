package manifest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/kballard/go-shellquote"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/shellenv"
)

// CommandPort abstracts external command execution. Production uses
// DefaultPort (shellquote argv split + os/exec, no shell); tests inject fakes.
type CommandPort interface {
	Output(command string) ([]byte, error) // run listCmd, capture stdout
	Apply(command string, id string) error // run applyCmd, id appended as last argv
}

type defaultPort struct{}

// DefaultPort returns the production command runner. Commands are parsed into
// argv without involving a shell, so shell metacharacters in configuration are
// never interpreted by a shell process.
func DefaultPort() CommandPort { return defaultPort{} }

const (
	ListTimeout  = 30 * time.Second
	ApplyTimeout = 120 * time.Second
)

// One manifest category == exactly one snapshot entry (virtual file).
func VirtualFileName(category string) string { return category + ".manifest.txt" }

// ParseIDs converts one-ID-per-line command output into its canonical list.
// Whitespace around each line is ignored, empty lines are dropped, and the
// first occurrence of an ID determines its position in the result.
// ParseIDs keeps every non-empty line (legacy behavior).
func ParseIDs(stdout []byte) []string {
	return ParseIDsWithPattern(stdout, "")
}

// ParseIDsWithPattern filters listCmd output to the ID lines. An empty
// pattern keeps every non-empty line; a pattern with one capture group
// extracts the group, and the trimmed full line otherwise.
func ParseIDsWithPattern(stdout []byte, pattern string) []string {
	lines := strings.Split(string(stdout), "\n")
	var ids []string
	seen := make(map[string]struct{}, len(lines))
	var compiled *regexp.Regexp
	if strings.TrimSpace(pattern) != "" {
		compiled = regexp.MustCompile(pattern)
	}
	for _, line := range lines {
		id := strings.TrimSpace(line)
		if id == "" {
			continue
		}
		if compiled != nil {
			match := compiled.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			if len(match) > 1 {
				id = strings.TrimSpace(match[1])
			}
			if id == "" {
				continue
			}
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

func IDsOf(content string) []string { return ParseIDs([]byte(content)) }

// ContentOf writes the canonical virtual-file representation: one ID per LF
// terminated line. Callers that need canonical IDs should use ParseIDs first.
func ContentOf(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	return strings.Join(sorted, "\n") + "\n"
}

var (
	validIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	validVersionedID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*@[0-9A-Za-z][0-9A-Za-z.+-]*$`)
	validNpmID       = regexp.MustCompile(`^npm:(@?[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)?)(?:@[0-9A-Za-z][0-9A-Za-z.+-]*)?$`)
)

// ValidID reports whether id can be passed as a single installer argument.
// Plain ids, VS Code publisher.name@version, and npm:name@version are
// allowed. Anything that could be a path escape or a second flag is not.
func ValidID(id string) bool {
	if strings.Contains(id, "..") {
		return false
	}
	return validIDPattern.MatchString(id) || validVersionedID.MatchString(id) || validNpmID.MatchString(id)
}

// ScanProblem is the manifest-side diagnostic; the adapter package converts
// it to adapter.ScanError{Path: Command, Message: ...}.
type ScanProblem struct {
	Command string
	Message string
	Err     error
}

// ScanCategory runs listCmd and produces the virtual-file snapshot entry.
// Failure (missing binary, non-zero exit, timeout) => empty Files + one problem.
func ScanCategory(adapterID, category string, cfg core.CategoryConfig, port CommandPort) (core.CategorySnapshot, []ScanProblem) {
	return ScanCategoryWithPattern(adapterID, category, cfg, port, cfg.IDPattern)
}

// ScanCategoryWithPattern runs listCmd and produces the virtual-file
// snapshot entry, filtering output lines through the optional ID pattern.
// Failure (missing binary, non-zero exit, timeout) => empty Files + one
// problem.
func ScanCategoryWithPattern(adapterID, category string, cfg core.CategoryConfig, port CommandPort, idPattern string) (core.CategorySnapshot, []ScanProblem) {
	if port == nil {
		port = DefaultPort()
	}
	snapshot := core.CategorySnapshot{
		AdapterID: adapterID,
		Category:  category,
		Mode:      cfg.Mode,
		Files:     make(core.SnapshotFiles),
	}
	stdout, err := port.Output(cfg.ListCmd)
	if err != nil {
		return snapshot, []ScanProblem{{Command: cfg.ListCmd, Message: err.Error(), Err: err}}
	}
	ids := ParseIDsWithPattern(stdout, idPattern)
	snapshot.Files[VirtualFileName(category)] = core.SnapshotEntry{
		Kind:    "file",
		Content: ContentOf(ids),
	}
	return snapshot, nil
}

// Task is one manifest category's set of IDs to install.
type Task struct {
	AdapterID string
	Category  string
	IDs       []string // sorted, deduped, to install
	ListCmd   string
	ApplyCmd  string
}

type Failure struct {
	ID      string
	Message string
}

type ApplyResult struct {
	Installed []string // "adapter/category:id"
	Failed    []Failure
}

// ApplyTasks runs applyCmd once per ID; a single failure is recorded and never
// blocks the remaining IDs. Invalid IDs are skipped with a Failure.
func ApplyTasks(tasks []Task, port CommandPort) ApplyResult {
	if port == nil {
		port = DefaultPort()
	}
	result := ApplyResult{
		Installed: make([]string, 0),
		Failed:    make([]Failure, 0),
	}
	for _, task := range tasks {
		for _, id := range sortedUnique(task.IDs) {
			if !ValidID(id) {
				result.Failed = append(result.Failed, Failure{ID: id, Message: "invalid manifest ID"})
				continue
			}
			if err := port.Apply(task.ApplyCmd, id); err != nil {
				message := err.Error()
				if message == "" {
					message = "manifest apply failed"
				}
				result.Failed = append(result.Failed, Failure{ID: id, Message: message})
				continue
			}
			result.Installed = append(result.Installed, task.AdapterID+"/"+task.Category+":"+id)
		}
	}
	return result
}

func sortedUnique(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := append([]string(nil), ids...)
	sort.Strings(out)
	write := 0
	for _, id := range out {
		if write > 0 && out[write-1] == id {
			continue
		}
		out[write] = id
		write++
	}
	return out[:write]
}

func splitCommand(command string) ([]string, error) {
	argv, err := shellquote.Split(command)
	if err != nil {
		return nil, fmt.Errorf("parse command: %w", err)
	}
	if len(argv) == 0 {
		return nil, errors.New("command is empty")
	}
	return argv, nil
}

func (defaultPort) Output(command string) ([]byte, error) {
	argv, err := splitCommand(command)
	if err != nil {
		return nil, err
	}
	return run(argv, ListTimeout)
}

func (defaultPort) Apply(command, id string) error {
	argv, err := splitCommand(command)
	if err != nil {
		return err
	}
	argv = append(argv, id)
	_, err = run(argv, ApplyTimeout)
	return err
}

const maxCommandOutputBytes = 16 * 1024 * 1024

type limitedReadResult struct {
	data     []byte
	exceeded bool
	err      error
}

func readLimited(reader io.Reader, limit int) limitedReadResult {
	data, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	exceeded := len(data) > limit
	if exceeded {
		data = data[:limit]
		// Keep draining the pipe after the cap so the child cannot block on a
		// full stdout pipe while the parent waits for it to exit.
		if _, drainErr := io.Copy(io.Discard, reader); err == nil {
			err = drainErr
		}
	}
	return limitedReadResult{data: data, exceeded: exceeded, err: err}
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	remaining := buffer.limit - buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			_, _ = buffer.Buffer.Write(data[:remaining])
			buffer.exceeded = true
		} else {
			_, _ = buffer.Buffer.Write(data)
		}
	} else if len(data) > 0 {
		buffer.exceeded = true
	}
	// Returning len(data) keeps os/exec draining stderr without retaining an
	// unbounded diagnostic. The command's exit status remains authoritative.
	return len(data), nil
}

func minimalCommandEnv() []string {
	path := shellenv.Path()
	keys := []string{"PATH", "HOME", "TERM", "LANG"}
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		value, _ := os.LookupEnv(key)
		if key == "PATH" {
			value = path
		}
		env = append(env, key+"="+value)
	}
	return env
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func run(argv []string, timeout time.Duration) ([]byte, error) {
	// argv comes from the user's own homer.json listCmd/applyCmd (trusted
	// local config, never remote input); the sandboxing below (timeout,
	// minimal env, output cap) is hygiene, not a security boundary.
	// Resolve PATH before the command timeout starts. A login shell can
	// take longer than a short command budget, and it is not the command.
	env := minimalCommandEnv()
	if resolved, err := shellenv.LookIn(argv[0], envValue(env, "PATH")); err == nil && resolved != "" {
		argv[0] = resolved
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	// Manifest commands may spawn descendants. Keep the command in its own
	// process group so timeout cancellation cannot leave a child holding the
	// stdout pipe open indefinitely.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second

	// Use an explicit os.Pipe instead of cmd.StdoutPipe: StdoutPipe's Wait
	// closes the read end while the reader goroutine may still be draining
	// it, racing the reader into "file already closed" and masking the
	// size-limit error. With our own pipe we control exactly who closes it.
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = stdoutWrite
	var stderr limitedBuffer
	stderr.limit = maxCommandOutputBytes
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		stdoutRead.Close()
		stdoutWrite.Close()
		return nil, err
	}
	// The child holds its own duplicate of the write end; closing ours here
	// lets the reader observe EOF as soon as the child (and any descendant
	// still sharing the fd) exits.
	stdoutWrite.Close()

	stdoutDone := make(chan limitedReadResult, 1)
	go func() {
		result := readLimited(stdoutRead, maxCommandOutputBytes)
		stdoutRead.Close()
		stdoutDone <- result
	}()
	waitErr := cmd.Wait()
	stdout := <-stdoutDone
	if ctx.Err() != nil {
		return nil, fmt.Errorf("command timed out: %w", ctx.Err())
	}
	if stdout.err != nil {
		return nil, stdout.err
	}
	if stdout.exceeded {
		return nil, fmt.Errorf("command output exceeds %d MiB limit", maxCommandOutputBytes/(1024*1024))
	}
	if waitErr != nil {
		message := waitErr.Error()
		if text := strings.TrimSpace(stderr.String()); text != "" {
			message += ": " + text
		}
		return nil, errors.New(message)
	}
	return stdout.data, nil
}
