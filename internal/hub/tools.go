package hub

import "github.com/zzjcool/homer-cli/internal/toolctl"

const (
	// maxTools bounds how many programs one machine may list. The registry
	// of adapters is a handful; the cap only stops a confused or hostile
	// agent from filling the hub's memory.
	maxTools    = 16
	maxToolText = 64
	maxToolErr  = 200
)

// normalizeTools copies a reported tool list into registry-owned memory,
// drops entries without an ID, keeps the first of any repeated ID, and clips
// every string the console will render.
//
// A nil list means "this report did not mention tools" and stays nil. A
// non-nil list, even an empty one, is a statement and stays non-nil.
func normalizeTools(in []toolctl.Status) []toolctl.Status {
	if in == nil {
		return nil
	}
	out := make([]toolctl.Status, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, tool := range in {
		if len(out) >= maxTools {
			break
		}
		id := clipText(tool.ID, maxToolText)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, toolctl.Status{
			ID:         id,
			Adapter:    clipText(tool.Adapter, maxToolText),
			Label:      clipText(tool.Label, maxToolText),
			Version:    clipText(tool.Version, maxToolText),
			Error:      clipText(tool.Error, maxToolErr),
			Upgradable: tool.Upgradable,
		})
	}
	return out
}

// cloneTools copies a stored list so a caller cannot reach into the
// registry's memory. It keeps the nil / empty distinction.
func cloneTools(in []toolctl.Status) []toolctl.Status {
	if in == nil {
		return nil
	}
	return append(make([]toolctl.Status, 0, len(in)), in...)
}
