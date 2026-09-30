package core

import (
	"errors"
	"fmt"
)

// CliError is a user-facing error carrying the process exit code selected by
// the command layer. Code defaults to 1 when constructed with NewCliError.
type CliError struct {
	Msg  string
	Code int
	// Cause lets a CLI error carry a sentinel (e.g.
	// ErrConfigNotInitialized) so downstream consumers can match
	// structurally with errors.Is instead of message substrings.
	Cause error
}

func (e CliError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return "homer command failed"
}

// Unwrap propagates the sentinel for errors.Is chains.
func (e CliError) Unwrap() error { return e.Cause }

// NewCliErrorWithCause is the sentinel-carrying constructor.
func NewCliErrorWithCause(cause error, message string, code ...int) CliError {
	wrapped := NewCliError(message, code...)
	wrapped.Cause = cause
	return wrapped
}

// NewCliError constructs the normal exit-1 user error. An optional code is
// accepted for command-specific future use while preserving Code=1 by default.
func NewCliError(message string, code ...int) CliError {
	status := 1
	if len(code) > 0 && code[0] != 0 {
		status = code[0]
	}
	return CliError{Msg: message, Code: status}
}

// ExitCode returns Code, normalizing zero-value CliError values to the frozen
// default exit code.
func (e CliError) ExitCode() int {
	if e.Code == 0 {
		return 1
	}
	return e.Code
}

// ErrConfigNotInitialized is the sentinel for "this machine has no
// homer.json yet" — a legal machine state (fresh machine awaiting
// dispatch), NOT a task failure. Callers that degrade this state into
// a friendly shape (agentd executor, web handleStatus) must match on
// errors.Is, never on message substrings.
var ErrConfigNotInitialized = errors.New("homer 配置未初始化")

// WrapConfigNotInitialized marks an underlying os.ErrNotExist-style
// miss as the fresh-machine sentinel while keeping the human message.
func WrapConfigNotInitialized(err error, message string) error {
	return fmt.Errorf("%w: %s", ErrConfigNotInitialized, message)
}

// IsConfigNotInitialized is the structured probe.
func IsConfigNotInitialized(err error) bool {
	return errors.Is(err, ErrConfigNotInitialized)
}
