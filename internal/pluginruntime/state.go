// Package pluginruntime stores the hub's installed-plugin state.
package pluginruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
	"github.com/zzjcool/homer-cli/internal/pluginregistry"
)

var (
	ErrInvalidID        = errors.New("invalid plugin id")
	ErrInvalidPlugin    = errors.New("invalid plugin")
	ErrAlreadyInstalled = errors.New("plugin is already installed")
	ErrNotInstalled     = errors.New("plugin is not installed")
)

const schemaVersion = 1

type diskState struct {
	SchemaVersion int               `json:"schemaVersion"`
	Installed     []string          `json:"installed"`
	Custom        []json.RawMessage `json:"custom"`
}

// State is the concurrency-safe, persistent installed-plugin set for one hub
// process. Installed IDs retain installation order; official IDs are expanded
// from pluginregistry and custom adapter manifests are stored in full.
type State struct {
	mu        sync.RWMutex
	path      string
	installed []string
	custom    map[string]pluginregistry.Plugin
}

// New loads the plugin state from <home>/plugins.json. A missing or invalid
// state file produces an empty state; the next successful mutation rewrites it
// in the current schema.
func New(home string) *State {
	if strings.TrimSpace(home) == "" {
		home = core.GetHomerPaths(nil).Home
	}
	state := &State{
		path:   filepath.Join(home, "plugins.json"),
		custom: map[string]pluginregistry.Plugin{},
	}
	state.load()
	return state
}

// List returns installed plugins in installation order. Returned plugins are
// detached from the state and can be modified by the caller.
func (s *State) List() []pluginregistry.Plugin {
	if s == nil {
		return []pluginregistry.Plugin{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	plugins := make([]pluginregistry.Plugin, 0, len(s.installed))
	for _, id := range s.installed {
		if plugin, ok := pluginregistry.Builtin(id); ok {
			plugins = append(plugins, plugin)
			continue
		}
		if plugin, ok := s.custom[id]; ok {
			plugins = append(plugins, clonePlugin(plugin))
		}
	}
	return plugins
}

// IsInstalled reports whether id is in the current installed set.
func (s *State) IsInstalled(id string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return contains(s.installed, id)
}

// Install adds an official plugin or a validated third-party adapter. The
// state is committed in memory only after the new file has been atomically
// written, so persistence failures do not produce a split view.
func (s *State) Install(plugin pluginregistry.Plugin) error {
	if s == nil {
		return ErrInvalidPlugin
	}
	if !core.ValidAdapterID(plugin.ID) {
		return fmt.Errorf("%w: %q", ErrInvalidID, plugin.ID)
	}

	custom := true
	if builtin, ok := pluginregistry.Builtin(plugin.ID); ok {
		plugin = builtin
		custom = false
	} else {
		if err := validateCustomPlugin(plugin); err != nil {
			return err
		}
		if plugin.Name == "" {
			plugin.Name = plugin.ID
		}
	}
	plugin = clonePlugin(plugin)

	s.mu.Lock()
	defer s.mu.Unlock()
	if contains(s.installed, plugin.ID) {
		return fmt.Errorf("%w: %s", ErrAlreadyInstalled, plugin.ID)
	}

	nextInstalled := append(append([]string(nil), s.installed...), plugin.ID)
	nextCustom := cloneCustom(s.custom)
	if custom {
		nextCustom[plugin.ID] = plugin
	}
	if err := s.persist(nextInstalled, nextCustom); err != nil {
		return err
	}
	s.installed = nextInstalled
	s.custom = nextCustom
	return nil
}

// Uninstall removes an installed plugin and persists the change.
func (s *State) Uninstall(id string) error {
	if s == nil {
		return ErrNotInstalled
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	for i, installedID := range s.installed {
		if installedID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("%w: %s", ErrNotInstalled, id)
	}

	nextInstalled := make([]string, 0, len(s.installed)-1)
	nextInstalled = append(nextInstalled, s.installed[:index]...)
	nextInstalled = append(nextInstalled, s.installed[index+1:]...)
	nextCustom := cloneCustom(s.custom)
	delete(nextCustom, id)
	if err := s.persist(nextInstalled, nextCustom); err != nil {
		return err
	}
	s.installed = nextInstalled
	s.custom = nextCustom
	return nil
}

func (s *State) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var saved diskState
	if json.Unmarshal(data, &saved) != nil || saved.SchemaVersion != schemaVersion {
		return
	}

	custom := make(map[string]pluginregistry.Plugin, len(saved.Custom))
	for _, raw := range saved.Custom {
		var plugin pluginregistry.Plugin
		if json.Unmarshal(raw, &plugin) != nil || plugin.ID == "" {
			continue
		}
		if _, builtin := pluginregistry.Builtin(plugin.ID); builtin {
			continue
		}
		if err := validateCustomPlugin(plugin); err != nil {
			continue
		}
		if plugin.Name == "" {
			plugin.Name = plugin.ID
		}
		if _, exists := custom[plugin.ID]; !exists {
			custom[plugin.ID] = clonePlugin(plugin)
		}
	}

	installed := make([]string, 0, len(saved.Installed)+len(custom))
	seen := make(map[string]struct{}, len(saved.Installed)+len(custom))
	for _, id := range saved.Installed {
		if !core.ValidAdapterID(id) {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		if _, builtin := pluginregistry.Builtin(id); !builtin {
			if _, customPlugin := custom[id]; !customPlugin {
				continue
			}
		}
		installed = append(installed, id)
		seen[id] = struct{}{}
	}
	// Older schema-1 files may have stored custom manifests without listing
	// their IDs in installed. Keep them usable and append them deterministically
	// in the order they appeared in the file.
	for _, raw := range saved.Custom {
		var plugin pluginregistry.Plugin
		if json.Unmarshal(raw, &plugin) != nil {
			continue
		}
		if _, ok := custom[plugin.ID]; !ok {
			continue
		}
		if _, duplicate := seen[plugin.ID]; duplicate {
			continue
		}
		installed = append(installed, plugin.ID)
		seen[plugin.ID] = struct{}{}
	}
	s.installed = installed
	s.custom = custom
}

func (s *State) persist(installed []string, custom map[string]pluginregistry.Plugin) error {
	saved := diskState{
		SchemaVersion: schemaVersion,
		Installed:     append([]string(nil), installed...),
		Custom:        make([]json.RawMessage, 0, len(custom)),
	}
	for _, id := range installed {
		plugin, ok := custom[id]
		if !ok {
			continue
		}
		data, err := json.Marshal(plugin)
		if err != nil {
			return fmt.Errorf("marshal custom plugin %q: %w", id, err)
		}
		saved.Custom = append(saved.Custom, data)
	}
	data, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal plugin state: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create plugin state directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".plugins-*.tmp")
	if err != nil {
		return fmt.Errorf("create plugin state temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set plugin state file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write plugin state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync plugin state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close plugin state: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("publish plugin state: %w", err)
	}
	return nil
}

func validateCustomPlugin(plugin pluginregistry.Plugin) error {
	if !core.ValidAdapterID(plugin.ID) {
		return fmt.Errorf("%w: invalid ID %q", ErrInvalidPlugin, plugin.ID)
	}
	if plugin.Role != pluginregistry.RoleAdapter || plugin.Adapter == nil || plugin.Action != nil {
		return fmt.Errorf("%w: custom plugins must be adapter plugins with an adapter config", ErrInvalidPlugin)
	}
	config := core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			plugin.ID: *plugin.Adapter,
		},
	}
	if validated, problems := core.ValidateConfig(orderedjson.Serialize(core.ConfigValue(config))); validated == nil {
		return fmt.Errorf("%w: %s", ErrInvalidPlugin, strings.Join(problems, "; "))
	}
	return nil
}

func clonePlugin(plugin pluginregistry.Plugin) pluginregistry.Plugin {
	data, err := json.Marshal(plugin)
	if err != nil {
		return plugin
	}
	var cloned pluginregistry.Plugin
	if json.Unmarshal(data, &cloned) != nil {
		return plugin
	}
	return cloned
}

func cloneCustom(custom map[string]pluginregistry.Plugin) map[string]pluginregistry.Plugin {
	cloned := make(map[string]pluginregistry.Plugin, len(custom))
	for id, plugin := range custom {
		cloned[id] = clonePlugin(plugin)
	}
	return cloned
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
