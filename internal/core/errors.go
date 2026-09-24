package core

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
