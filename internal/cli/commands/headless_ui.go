package commands

// HeadlessUI is the command-layer UI seam for non-interactive callers such
// as the hub web server and agent executors. Its Confirm/Select always take
// the fallback value, which means unconfirmed push/pull run through the
// promptConfirm/promptSelect fallback path deterministically instead of
// probing the process stdout for a TTY: a server whose stdout happens to be
// a terminal (tmux, foreground use) must never block a request goroutine on
// an interactive prompt nobody can answer.
//
// It implements selectPrompter (and therefore confirmPrompter), which makes
// it valid as PushDeps.UI, and satisfies the duck-typed any seam used by
// PullDeps.UI.
type HeadlessUI struct{}

// Confirm returns the fallback. See the type comment for why.
func (HeadlessUI) Confirm(message string, fallback bool) bool { return fallback }

// Select returns the fallback. See the type comment for why.
func (HeadlessUI) Select(message string, options []selectOption, fallback string) string {
	return fallback
}
