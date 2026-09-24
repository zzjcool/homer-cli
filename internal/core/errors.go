package core

import "fmt"

// CliError is a user-facing error carrying the process exit code selected by
// the command layer. Code defaults to 1 when constructed with NewCliError.
type CliError struct {
	Msg  string
	Code int
}

func (e CliError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return "homer command failed"
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

// AsCliError makes it convenient for callers to preserve a non-Cli error's
// text while giving it the standard user-error code.
func AsCliError(err error) CliError {
	if err == nil {
		return CliError{Code: 1}
	}
	if value, ok := err.(CliError); ok {
		if value.Code == 0 {
			value.Code = 1
		}
		return value
	}
	if pointer, ok := err.(*CliError); ok && pointer != nil {
		value := *pointer
		if value.Code == 0 {
			value.Code = 1
		}
		return value
	}
	return NewCliError(err.Error())
}

// FormatCliError is a small compatibility helper for command renderers. It
// deliberately does not expose implementation details beyond the message.
func FormatCliError(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprint(err)
}
