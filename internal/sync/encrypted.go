package sync

import (
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/keyring"
)

// WithEncryptedIgnores returns the adapter config with every file this
// machine's keyring holds encrypted for that adapter added to its ignore
// list. Those files travel as ciphertext through the keyring adapter; the
// plaintext copy must not also be scanned, secret-checked, or counted as
// drift. The original config is not modified.
func WithEncryptedIgnores(homerHome, adapterID string, config core.AdapterConfig) core.AdapterConfig {
	extra := keyring.EncryptedIgnores(homerHome, adapterID, config.Root)
	if len(extra) == 0 {
		return config
	}
	merged := make([]string, 0, len(config.Ignore)+len(extra))
	merged = append(merged, config.Ignore...)
	merged = append(merged, extra...)
	config.Ignore = merged
	return config
}
