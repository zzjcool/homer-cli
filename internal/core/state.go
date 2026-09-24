package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

// HomerState records the last successfully synchronized repository revision.
// It is acceleration metadata, not synchronization content.
type HomerState struct {
	Version         int    `json:"version"`
	LastSyncCommit  string `json:"lastSyncCommit,omitempty"`
	LastSyncAt      string `json:"lastSyncAt,omitempty"`
	LastSyncCommand string `json:"lastSyncCommand,omitempty"`
}

func emptyState() HomerState { return HomerState{Version: 1} }

func validSyncCommand(command string) bool {
	return command == "push" || command == "pull" || command == "merge"
}

// LoadState treats every read, parse, and shape failure as an empty state.
// State is deliberately non-authoritative: callers can fall back to the
// workspace store when this acceleration file is absent or damaged.
func LoadState(paths HomerPaths) HomerState {
	data, err := os.ReadFile(paths.StateFile)
	if err != nil {
		return emptyState()
	}

	parsed, err := orderedjson.Parse(data)
	if err != nil {
		return emptyState()
	}
	object, ok := parsed.(*orderedjson.Object)
	if !ok || object == nil {
		return emptyState()
	}

	state := emptyState()
	if commit, ok := object.M["lastSyncCommit"].(string); ok && strings.TrimSpace(commit) != "" {
		state.LastSyncCommit = commit
	}
	if at, ok := object.M["lastSyncAt"].(string); ok && strings.TrimSpace(at) != "" {
		state.LastSyncAt = at
	}
	if command, ok := object.M["lastSyncCommand"].(string); ok && validSyncCommand(command) {
		state.LastSyncCommand = command
	}
	return state
}

func loadState(paths HomerPaths) HomerState { return LoadState(paths) }

func stateValue(state HomerState) orderedjson.Value {
	keys := []string{"version"}
	values := map[string]orderedjson.Value{
		"version": orderedStateNumber(state.Version),
	}
	if state.LastSyncCommit != "" {
		keys = append(keys, "lastSyncCommit")
		values["lastSyncCommit"] = state.LastSyncCommit
	}
	if state.LastSyncAt != "" {
		keys = append(keys, "lastSyncAt")
		values["lastSyncAt"] = state.LastSyncAt
	}
	if state.LastSyncCommand != "" {
		keys = append(keys, "lastSyncCommand")
		values["lastSyncCommand"] = state.LastSyncCommand
	}
	return &orderedjson.Object{Keys: keys, M: values}
}

func orderedStateNumber(value int) orderedjson.Value {
	parsed, err := orderedjson.Parse([]byte(fmt.Sprint(value)))
	if err != nil {
		return nil
	}
	return parsed
}

// SaveState atomically replaces state.json. The temporary file is in the same
// directory, so rename has the same-filesystem atomicity required by the
// frozen state contract.
func SaveState(paths HomerPaths, state HomerState) error {
	if err := os.MkdirAll(filepath.Dir(paths.StateFile), 0o777); err != nil {
		return err
	}
	if state.Version == 0 {
		state.Version = 1
	}

	data := orderedjson.SerializeFile(stateValue(state))
	tmp := fmt.Sprintf("%s.tmp-%d", paths.StateFile, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o666); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, paths.StateFile); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func saveState(paths HomerPaths, state HomerState) error { return SaveState(paths, state) }

// StateValue exposes the ordered representation for git/diagnostic workers
// without introducing a second JSON serialization implementation.
func StateValue(state HomerState) orderedjson.Value { return stateValue(state) }
