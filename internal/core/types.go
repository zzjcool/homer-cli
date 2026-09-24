package core

// SyncMode selects the merge strategy for a category.
type SyncMode string

const (
	SyncModeMerge  SyncMode = "merge"
	SyncModeMirror SyncMode = "mirror"
)

// CategoryConfig describes the files belonging to one adapter category.
type CategoryConfig struct {
	Paths       []string `json:"paths"`
	Mode        SyncMode `json:"mode"`
	Enabled     *bool    `json:"enabled,omitempty"`
	Exclude     []string `json:"exclude,omitempty"`
	ExcludeKeys []string `json:"excludeKeys,omitempty"`
}

// AdapterConfig describes one tool/adapter and its categories.
type AdapterConfig struct {
	Root        string                    `json:"root"`
	Enabled     *bool                     `json:"enabled,omitempty"`
	Categories  map[string]CategoryConfig `json:"categories"`
	Ignore      []string                  `json:"ignore,omitempty"`
	AllowEscape []string                  `json:"allowEscape,omitempty"`
}

// BackupConfig controls date-directory retention for backups.
type BackupConfig struct {
	Keep *int `json:"keep,omitempty"`
}

// SecretsConfig controls secret scanning and age-vault destinations.
type SecretsConfig struct {
	IgnorePaths []string          `json:"ignorePaths,omitempty"`
	Recipients  []string          `json:"recipients,omitempty"`
	Files       map[string]string `json:"files,omitempty"`
}

// HomerConfig is the on-disk homer.json shape.
type HomerConfig struct {
	Version  int                      `json:"version"`
	Adapters map[string]AdapterConfig `json:"adapters"`
	Backup   *BackupConfig            `json:"backup,omitempty"`
	Secrets  *SecretsConfig           `json:"secrets,omitempty"`
}

// SnapshotEntry is a raw UTF-8 file in a category snapshot. Kind is either
// "json" or "file"; entryKindFor is the single source of truth for deriving it.
type SnapshotEntry struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

// SnapshotFiles maps a category-relative POSIX path to its raw entry.
type SnapshotFiles map[string]SnapshotEntry

// CategorySnapshot is one adapter category's store/local snapshot.
type CategorySnapshot struct {
	AdapterID string        `json:"adapterId"`
	Category  string        `json:"category"`
	Mode      SyncMode      `json:"mode"`
	Files     SnapshotFiles `json:"files"`
}

// AdapterSnapshot contains all enabled categories for one adapter.
type AdapterSnapshot struct {
	AdapterID  string             `json:"adapterId"`
	Categories []CategorySnapshot `json:"categories"`
}
