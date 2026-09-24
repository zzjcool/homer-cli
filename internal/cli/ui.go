package cli

import (
	"github.com/AlecAivazis/survey/v2"
)

// SelectOption is one choice exposed by PromptPort.  Keeping the value
// separate from the label lets command code use stable machine values while
// presenting a useful description to a person at a terminal.
type SelectOption struct {
	Value string
	Label string
}

// PromptPort is the only prompt dependency visible to command code.  The
// methods are deliberately synchronous: commands are synchronous too, and a
// prompt is a small, bounded interaction at the edge of the process.
type PromptPort interface {
	Confirm(msg string, fallback bool) bool
	Select(msg string, opts []SelectOption, fallback string) string
}

// nonInteractivePromptPort never reads stdin.  This is important for pipes,
// CI, and tests: a command must not hang waiting for a prompt that can never
// be answered.  An explicit fallback is returned as-is; an empty select
// fallback means "use the first option", matching the TypeScript port's
// missing-fallback behavior.
type nonInteractivePromptPort struct{}

// NonInteractive is the exported name for the safe fallback implementation.
// It has no state and can also be embedded by command-level tests.
type NonInteractive = nonInteractivePromptPort

func (nonInteractivePromptPort) Confirm(_ string, fallback bool) bool { return fallback }

func (nonInteractivePromptPort) Select(_ string, opts []SelectOption, fallback string) string {
	if fallback != "" {
		return fallback
	}
	if len(opts) == 0 {
		return ""
	}
	return opts[0].Value
}

var nonInteractivePort PromptPort = nonInteractivePromptPort{}

// NewNonInteractivePromptPort returns the stateless non-TTY implementation.
// A shared instance is safe and makes it easy for callers/tests to recognize
// the fallback implementation without any process-global mutable state.
func NewNonInteractivePromptPort() PromptPort { return nonInteractivePort }

// surveyPromptPort adapts the lightweight survey terminal UI to PromptPort.
// Construction does not perform any I/O; prompts happen only when a command
// explicitly calls Confirm or Select.
type surveyPromptPort struct{}

func (surveyPromptPort) Confirm(message string, fallback bool) bool {
	answer := fallback
	prompt := &survey.Confirm{Message: message, Default: fallback}
	if err := survey.AskOne(prompt, &answer); err != nil {
		// Ctrl-C, EOF, and all other prompt failures are safe refusals.  A
		// failed terminal interaction must never turn into approval.
		return false
	}
	return answer
}

func (surveyPromptPort) Select(message string, opts []SelectOption, fallback string) string {
	if len(opts) == 0 {
		return fallback
	}

	labels := make([]string, len(opts))
	index := 0
	foundFallback := false
	for i, option := range opts {
		labels[i] = option.Label
		if option.Value == fallback {
			index = i
			foundFallback = true
		}
	}
	if !foundFallback {
		index = 0
	}

	selected := labels[index]
	prompt := &survey.Select{
		Message: message,
		Options: labels,
		Default: index,
	}
	if err := survey.AskOne(prompt, &selected); err != nil {
		return fallbackOrFirst(opts, fallback)
	}
	for _, option := range opts {
		if option.Label == selected {
			return option.Value
		}
	}
	// survey should only return one of Options.  Keep a fail-safe if a custom
	// prompt implementation ever violates that contract.
	return fallbackOrFirst(opts, fallback)
}

func fallbackOrFirst(opts []SelectOption, fallback string) string {
	if fallback != "" {
		return fallback
	}
	if len(opts) == 0 {
		return ""
	}
	return opts[0].Value
}

// NewDefaultPromptPort selects a real terminal prompt only when isTTY is
// true.  Callers that have --yes must not call this constructor at all: the
// no-port rule is enforced by the command dispatch layer, not by this factory.
func NewDefaultPromptPort(isTTY bool) PromptPort {
	if !isTTY {
		return nonInteractivePort
	}
	return surveyPromptPort{}
}
