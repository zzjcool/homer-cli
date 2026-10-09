// Package resolutions persists staged conflict decisions on a machine
// (staged-resolution plan, Git-style phases): a recorded decision is
// consumed by a later dispatch instead of being applied immediately.
//
// The file lives at <home>/resolutions.json with 0600 permissions. Only
// this package may name the literal path (Path is the single source).
package resolutions

import (
	"errors"
)

const (
	// FileName is the on-disk name inside a homer home.
	FileName = "resolutions.json"
	// FileVersion is the only schema version this build understands.
	FileVersion = 1

	// Choices.
	ChoiceCenter = "center"
	ChoiceLocal  = "local"

	// Actions for the resolve-record task.
	ActionRecord = "record"
	ActionClear  = "clear"
	ActionList   = "list"

	// Outcomes for dispatch consumption reports.
	OutcomeApplied   = "applied"
	OutcomePublished = "published"
	OutcomeNoop      = "noop"
	OutcomeStale     = "stale"
	OutcomeFailed    = "failed"
)

var (
	// ErrCorrupt is returned by Load when the file exists but is not valid
	// JSON. Callers treat it as "no decisions" with a warning.
	ErrCorrupt = errors.New("resolutions: 文件损坏")
	// ErrUnsupportedVersion is returned by Save-side checks when the file
	// carries a schema this build does not know; writing is refused.
	ErrUnsupportedVersion = errors.New("resolutions: 不支持的版本")
)

// Entry is one staged decision, adapter level (v1).
type Entry struct {
	Adapter            string `json:"adapter"`
	Choice             string `json:"choice"`
	RecordedAt         string `json:"recordedAt"`
	GenerationAtRecord int    `json:"generationAtRecord"`
}

// File is the on-disk document.
type File struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// Outcome reports how one entry was treated during a dispatch.
type Outcome struct {
	Adapter string `json:"adapter"`
	Choice  string `json:"choice"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// Report is the resolve-record task's return body.
type Report struct {
	OK     bool   `json:"ok"`
	Action string `json:"action"`
	// Entries is the full post-operation list.
	Entries []Entry `json:"entries"`
	// Recorded lists freshly written entries.
	Recorded []Entry `json:"recorded,omitempty"`
	// Replaced lists entries a record overwrote (D2 last-write-wins).
	Replaced []Entry `json:"replaced,omitempty"`
	// Removed lists entries this operation deleted.
	Removed []Entry `json:"removed,omitempty"`
	// Pending is len(Entries) after the operation.
	Pending  int      `json:"pending"`
	Warnings []string `json:"warnings,omitempty"`
	Errors   []string `json:"errors"`
}
