package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzjcool/homer-cli/internal/orderedjson"
)

// PairedDevice records the public identity of a machine that has completed
// an online key pairing. It is metadata only; the private identity remains
// outside the repository.
type PairedDevice struct {
	Hostname  string `json:"hostname"`
	Recipient string `json:"recipient"`
	PairedAt  string `json:"pairedAt"`
}

// HomerState records the last successfully synchronized repository revision.
// It is acceleration metadata, not synchronization content.
type HomerState struct {
	Version         int            `json:"version"`
	LastSyncCommit  string         `json:"lastSyncCommit,omitempty"`
	LastSyncAt      string         `json:"lastSyncAt,omitempty"`
	LastSyncCommand string         `json:"lastSyncCommand,omitempty"`
	Paired          []PairedDevice `json:"paired,omitempty"`
}

// AddPairedDevice returns a copy with device merged into the recorded list.
// Recipient is the stable deduplication key: updating a known recipient
// preserves its position while replacing its hostname and pairing time.
func (s HomerState) AddPairedDevice(device PairedDevice) HomerState {
	merged := s
	merged.Paired = append([]PairedDevice(nil), s.Paired...)
	for index := range merged.Paired {
		if merged.Paired[index].Recipient != device.Recipient {
			continue
		}
		merged.Paired[index].Hostname = device.Hostname
		merged.Paired[index].PairedAt = device.PairedAt
		return merged
	}
	merged.Paired = append(merged.Paired, device)
	return merged
}

// PairedDisplay renders the deliberately abbreviated recipient form used in
// human-facing reports. State is non-authoritative, so malformed entries are
// ignored rather than allowed to produce an unsafe or misleading line.
func PairedDisplay(state HomerState) []string {
	displays := make([]string, 0, len(state.Paired))
	for _, device := range state.Paired {
		if !pairedDeviceDisplayValid(device) {
			continue
		}
		recipient := device.Recipient
		fingerprint := recipient[:12] + "…" + recipient[len(recipient)-4:]
		displays = append(displays, device.Hostname+"（"+fingerprint+"）")
	}
	if len(displays) == 0 {
		return nil
	}
	return displays
}

func pairedDeviceDisplayValid(device PairedDevice) bool {
	if strings.TrimSpace(device.Hostname) == "" || strings.TrimSpace(device.Recipient) == "" || strings.TrimSpace(device.PairedAt) == "" {
		return false
	}
	// The display needs enough bytes for the frozen first-12/last-4
	// fingerprint. Recipients are ASCII, so byte slicing is intentional.
	return len(device.Recipient) > 16
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
	if paired, ok := parsePaired(object.M["paired"]); ok {
		state.Paired = paired
	}
	return state
}

// parsePaired applies the deliberately narrow shape check for the additive
// paired field. A single malformed element invalidates the whole field, but
// never invalidates the older last-sync metadata in the same state object.
func parsePaired(value orderedjson.Value) ([]PairedDevice, bool) {
	items, ok := value.([]orderedjson.Value)
	if !ok {
		return nil, false
	}
	paired := make([]PairedDevice, 0, len(items))
	for _, item := range items {
		object, ok := item.(*orderedjson.Object)
		if !ok || object == nil {
			return nil, false
		}
		hostname, hostnameOK := object.M["hostname"].(string)
		recipient, recipientOK := object.M["recipient"].(string)
		pairedAt, pairedAtOK := object.M["pairedAt"].(string)
		if !hostnameOK || !recipientOK || !pairedAtOK {
			return nil, false
		}
		paired = append(paired, PairedDevice{
			Hostname:  hostname,
			Recipient: recipient,
			PairedAt:  pairedAt,
		})
	}
	return paired, true
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
	if len(state.Paired) > 0 {
		keys = append(keys, "paired")
		items := make([]orderedjson.Value, 0, len(state.Paired))
		for _, device := range state.Paired {
			items = append(items, &orderedjson.Object{
				Keys: []string{"hostname", "recipient", "pairedAt"},
				M: map[string]orderedjson.Value{
					"hostname":  device.Hostname,
					"recipient": device.Recipient,
					"pairedAt":  device.PairedAt,
				},
			})
		}
		values["paired"] = items
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
