package manifest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kballard/go-shellquote"

	"github.com/zzjcool/homer-cli/internal/core"
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
func ParseIDs(stdout []byte) []string {
	lines := strings.Split(string(stdout), "\n")
	var ids []string
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		id := strings.TrimSpace(line)
		if id == "" {
			continue
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
	return strings.Join(ids, "\n") + "\n"
}

var validIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func ValidID(id string) bool { return validIDPattern.MatchString(id) }

// ScanProblem is the manifest-side diagnostic; the adapter package converts
// it to adapter.ScanError{Path: Command, Message: ...}.
type ScanProblem struct {
	Command string
	Message string
}

// ScanCategory runs listCmd and produces the virtual-file snapshot entry.
// Failure (missing binary, non-zero exit, timeout) => empty Files + one problem.
func ScanCategory(adapterID, category string, cfg core.CategoryConfig, port CommandPort) (core.CategorySnapshot, []ScanProblem) {
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
		return snapshot, []ScanProblem{{Command: cfg.ListCmd, Message: err.Error()}}
	}
	ids := ParseIDs(stdout)
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

func run(argv []string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("command timed out: %w", ctx.Err())
	}
	if err != nil {
		message := err.Error()
		if text := strings.TrimSpace(stderr.String()); text != "" {
			message += ": " + text
		}
		return nil, errors.New(message)
	}
	return stdout, nil
}
