package agecrypto

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"

	"github.com/zzjcool/homer-cli/internal/core"
)

const (
	minimumLeakFragmentBytes = 17
	leakSampleBytes          = 64
)

// SecretNameValid is the flat vault-name policy. Because slash and dot-dot
// names are excluded by the first-character rule and the allowed alphabet,
// every accepted name maps to one file directly under secrets/.
func SecretNameValid(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		if i == 0 {
			if !isASCIIAlpha(c) && !isASCIIDigit(c) {
				return false
			}
			continue
		}
		if !isASCIIAlpha(c) && !isASCIIDigit(c) && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func isASCIIAlpha(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isASCIIDigit(c rune) bool { return c >= '0' && c <= '9' }

func secretsDirectory(p core.HomerPaths) string {
	if p.SecretsDir != "" {
		return p.SecretsDir
	}
	if p.Home != "" {
		return filepath.Join(p.Home, "secrets")
	}
	return "secrets"
}

// SecretFilePath maps a validated name to <secretsDir>/<name>.age. Invalid
// names produce a CliError rather than allowing a caller to construct a path
// outside the vault.
func SecretFilePath(p core.HomerPaths, name string) (string, error) {
	if !SecretNameValid(name) {
		return "", core.NewCliError("invalid secret name")
	}
	return filepath.Join(secretsDirectory(p), name+".age"), nil
}

// SecretNames returns the configured vault names in stable lexical order.
func SecretNames(config *core.HomerConfig) []string {
	if config == nil || config.Secrets == nil || len(config.Secrets.Files) == 0 {
		return nil
	}
	names := make([]string, 0, len(config.Secrets.Files))
	for name := range config.Secrets.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// DestinationOf returns the configured destination for a vault name. It is a
// small shared read helper for command layers; no destination is inferred for
// an unknown name.
func DestinationOf(config *core.HomerConfig, name string) (string, error) {
	if config == nil || config.Secrets == nil || config.Secrets.Files == nil {
		return "", core.NewCliError("secret is not configured")
	}
	destination, ok := config.Secrets.Files[name]
	if !ok {
		return "", core.NewCliError("secret is not configured")
	}
	return filepath.Clean(core.ExpandHome(destination)), nil
}

func plaintextSamples(plaintext []byte) [][]byte {
	if len(plaintext) < minimumLeakFragmentBytes {
		return nil
	}
	length := len(plaintext)
	if length > leakSampleBytes {
		length = leakSampleBytes
	}
	starts := []int{0, (len(plaintext) - length) / 2, len(plaintext) - length}
	samples := make([][]byte, 0, len(starts))
	seen := make(map[string]struct{}, len(starts))
	for _, start := range starts {
		sample := plaintext[start : start+length]
		key := string(sample)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		samples = append(samples, sample)
	}
	return samples
}

// CiphertextLooksSafe applies the vault self-check before any bytes reach
// disk. The samples catch partial leakage at the beginning, middle, and end;
// the full byte equality check is the explicit fallback for short plaintexts,
// for which rejecting every short substring would create random-match false
// positives. It is exported for pair's preflight path; the behavior is kept
// identical to the original vault write guard.
func CiphertextLooksSafe(ciphertext, plaintext []byte) bool {
	if len(ciphertext) == 0 {
		return false
	}
	if bytes.Equal(ciphertext, plaintext) {
		return false
	}
	for _, sample := range plaintextSamples(plaintext) {
		if bytes.Contains(ciphertext, sample) {
			return false
		}
	}
	return true
}

// Keep the package-local spelling used by the pre-v1.3 write path so its
// behavior and call sites remain unchanged.
func ciphertextLooksSafe(ciphertext, plaintext []byte) bool {
	return CiphertextLooksSafe(ciphertext, plaintext)
}

func integrityError() error {
	return core.NewCliError("age ciphertext failed integrity check")
}

// EncryptSecretToFile encrypts one secret to all supplied recipients and
// atomically replaces its .age file. The crypto port is injectable so command
// and doctor tests can use a fake without coupling vault behavior to age.
func EncryptSecretToFile(crypto AgeCryptoPort, p core.HomerPaths, name string, plaintext []byte, recipients []string) error {
	file, err := SecretFilePath(p, name)
	if err != nil {
		return err
	}
	if len(recipients) == 0 {
		return core.NewCliError("no age recipients configured")
	}
	if crypto == nil {
		crypto = NewAgeCryptoPort()
	}
	ciphertext, err := crypto.Encrypt(plaintext, recipients)
	if err != nil {
		return err
	}
	if !ciphertextLooksSafe(ciphertext, plaintext) {
		return integrityError()
	}
	return atomicWriteVaultFile(file, ciphertext)
}

// WriteVaultCiphertext atomically stores an already-encrypted payload in the
// local vault. The payload is intentionally not re-encrypted or parsed here:
// the join side has already verified it before asking the vault to persist it.
func WriteVaultCiphertext(p core.HomerPaths, name string, ciphertext []byte) error {
	file, err := SecretFilePath(p, name)
	if err != nil {
		return err
	}
	return atomicWriteVaultFile(file, ciphertext)
}

func atomicWriteVaultFile(file string, content []byte) error {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return core.NewCliError("cannot create secrets directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return core.NewCliError("cannot secure secrets directory")
	}

	tmp, err := os.CreateTemp(dir, ".vault-*.tmp")
	if err != nil {
		return core.NewCliError("cannot create temporary vault file")
	}
	tmpName := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return core.NewCliError("cannot secure temporary vault file")
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return core.NewCliError("cannot write vault file")
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return core.NewCliError("cannot flush vault file")
	}
	if err := tmp.Close(); err != nil {
		return core.NewCliError("cannot close vault file")
	}
	if err := os.Rename(tmpName, file); err != nil {
		return core.NewCliError("cannot finalize vault file")
	}
	removeTemp = false
	return nil
}

// DecryptSecretFromFile reads a local .age file and decrypts it with the local
// identity. Identity loading is deliberately inside this helper to preserve
// the frozen command-layer signature and to keep all writes out of the read
// path.
func DecryptSecretFromFile(crypto AgeCryptoPort, p core.HomerPaths, name string) ([]byte, error) {
	file, err := SecretFilePath(p, name)
	if err != nil {
		return nil, err
	}
	ciphertext, err := os.ReadFile(file)
	if err != nil {
		return nil, core.NewCliError("vault file is missing")
	}
	identity, ok := LoadIdentity(p)
	if !ok {
		return nil, core.NewCliError("no usable age identity")
	}
	if crypto == nil {
		crypto = NewAgeCryptoPort()
	}
	return crypto.Decrypt(ciphertext, identity)
}

// VaultEntryStatus describes whether a configured vault file exists. The
// destination is intentionally the configured value (rather than an inferred
// filesystem path), matching the config/report shape used by the command
// layer.
type VaultEntryStatus struct {
	Name        string `json:"name"`
	Destination string `json:"destination"`
	VaultFile   string `json:"vaultFile"`
}

// ListSecrets is a pure inventory operation: it does not require an identity,
// decrypt files, or touch configured destination paths.
func ListSecrets(p core.HomerPaths, config *core.HomerConfig) []VaultEntryStatus {
	names := SecretNames(config)
	if len(names) == 0 {
		return []VaultEntryStatus{}
	}
	result := make([]VaultEntryStatus, 0, len(names))
	for _, name := range names {
		destination := ""
		if config != nil && config.Secrets != nil {
			destination = config.Secrets.Files[name]
		}
		file, err := SecretFilePath(p, name)
		status := "missing"
		if err == nil {
			if _, statErr := os.Stat(file); statErr == nil {
				status = "present"
			}
		}
		result = append(result, VaultEntryStatus{
			Name:        name,
			Destination: destination,
			VaultFile:   status,
		})
	}
	return result
}
