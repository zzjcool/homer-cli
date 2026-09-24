package agecrypto

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"

	"github.com/zzjcool/homer-cli/internal/core"
)

const identityFileName = "age.txt"

// AgeIdentity is the local age X25519 identity and its corresponding public
// recipient. SecretKey is only kept in memory and in the identity file under
// the user's Homer home.
type AgeIdentity struct {
	SecretKey string
	Recipient string
}

// keysDirectory returns the configured keys directory. HomerPaths produced by
// core.GetHomerPaths always have KeysDir populated, while accepting a partially
// populated path value keeps this package convenient for focused tests and
// callers constructing paths manually.
func keysDirectory(p core.HomerPaths) string {
	if p.KeysDir != "" {
		return p.KeysDir
	}
	if p.Home != "" {
		return filepath.Join(p.Home, "keys")
	}
	return "keys"
}

// IdentityFilePath returns the path of the local age identity file. It does
// not create the directory or inspect the filesystem.
func IdentityFilePath(p core.HomerPaths) string {
	return filepath.Join(keysDirectory(p), identityFileName)
}

// GenerateIdentity makes a new X25519 identity in memory. The age library
// only fails here when the system random source fails; keep the frozen
// synchronous API and return the zero value in that exceptionally rare case.
func GenerateIdentity() AgeIdentity {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return AgeIdentity{}
	}
	return AgeIdentity{
		SecretKey: identity.String(),
		Recipient: identity.Recipient().String(),
	}
}

// KeyGen generates and persists a new identity. Existing identity files are
// never replaced because doing so would strand already-pushed vault files.
func KeyGen(p core.HomerPaths) (AgeIdentity, error) {
	identity := GenerateIdentity()
	if identity.SecretKey == "" || identity.Recipient == "" {
		return AgeIdentity{}, invalidIdentityError()
	}
	if err := WriteIdentityFile(p, identity); err != nil {
		return AgeIdentity{}, err
	}
	return identity, nil
}

// normalizeSecretKey accepts the all-lowercase spelling permitted by bech32
// and normalizes it to age's official all-uppercase identity spelling. Mixed
// case is deliberately rejected by the age format.
func normalizeSecretKey(secretKey string) (string, error) {
	if secretKey == "" || strings.TrimSpace(secretKey) != secretKey {
		return "", invalidIdentityError()
	}
	if secretKey != strings.ToLower(secretKey) && secretKey != strings.ToUpper(secretKey) {
		return "", invalidIdentityError()
	}
	candidate := secretKey
	if secretKey == strings.ToLower(secretKey) {
		candidate = strings.ToUpper(secretKey)
	}
	identity, err := age.ParseX25519Identity(candidate)
	if err != nil {
		return "", invalidIdentityError()
	}
	return identity.String(), nil
}

// parseSecretKey parses and derives an identity while ensuring all failures
// use a fixed error that cannot disclose the supplied private key.
func parseSecretKey(secretKey string) (*age.X25519Identity, error) {
	canonical, err := normalizeSecretKey(secretKey)
	if err != nil {
		return nil, err
	}
	identity, err := age.ParseX25519Identity(canonical)
	if err != nil {
		return nil, invalidIdentityError()
	}
	return identity, nil
}

func invalidIdentityError() error {
	return core.NewCliError("invalid age identity")
}

func duplicateIdentityError() error {
	return core.NewCliError("age identity already exists; refusing to overwrite")
}

// ParseIdentityFile parses the age CLI-compatible identity file format. It
// accepts one non-empty, non-comment line and rejects additional key lines.
// The returned identity is canonicalized to age's official spelling.
func ParseIdentityFile(content string) (AgeIdentity, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	var key string
	keyLines := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keyLines++
		if keyLines == 1 {
			key = line
		}
	}
	if keyLines == 0 || keyLines > 1 {
		return AgeIdentity{}, core.NewCliError(fmt.Sprintf("invalid age identity file: expected one key, found %d", keyLines))
	}

	identity, err := parseSecretKey(key)
	if err != nil {
		return AgeIdentity{}, err
	}
	return AgeIdentity{
		SecretKey: identity.String(),
		Recipient: identity.Recipient().String(),
	}, nil
}

// WriteIdentityFile atomically creates <keysDir>/age.txt with mode 0600. The
// target is created with a no-replace hard link from a completed temporary
// file, so a concurrent writer cannot turn this operation into an overwrite.
func WriteIdentityFile(p core.HomerPaths, identity AgeIdentity) error {
	file := IdentityFilePath(p)

	// Check existence before validating the new identity. Even an empty or
	// damaged old file is a reservation that keygen must not silently replace.
	if _, err := os.Lstat(file); err == nil {
		return duplicateIdentityError()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return core.NewCliError("cannot inspect age identity file")
	}

	parsed, err := parseSecretKey(identity.SecretKey)
	if err != nil {
		return err
	}
	serialized := []byte(parsed.String() + "\n")

	dir := keysDirectory(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return core.NewCliError("cannot create age keys directory")
	}
	// MkdirAll's mode is subject to umask and does not change an existing
	// directory. Explicit chmod makes the security invariant hold in both
	// cases.
	if err := os.Chmod(dir, 0o700); err != nil {
		return core.NewCliError("cannot secure age keys directory")
	}

	tmp, err := os.CreateTemp(dir, ".age.txt.tmp-")
	if err != nil {
		return core.NewCliError("cannot create temporary age identity file")
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
		return core.NewCliError("cannot secure temporary age identity file")
	}
	if _, err := tmp.Write(serialized); err != nil {
		_ = tmp.Close()
		return core.NewCliError("cannot write age identity file")
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return core.NewCliError("cannot flush age identity file")
	}
	if err := tmp.Close(); err != nil {
		return core.NewCliError("cannot close age identity file")
	}

	// Link is atomic and refuses to replace an existing target. Both paths are
	// in the same directory, so the operation is on one filesystem.
	if err := os.Link(tmpName, file); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return duplicateIdentityError()
		}
		return core.NewCliError("cannot finalize age identity file")
	}
	if err := os.Remove(tmpName); err != nil {
		return core.NewCliError("cannot finalize age identity file")
	}
	removeTemp = false
	return nil
}

// LoadIdentity reads the local identity. Missing, unreadable, or malformed
// files all return the zero identity and false; callers can decide whether to
// prompt for keygen or report a doctor failure.
func LoadIdentity(p core.HomerPaths) (AgeIdentity, bool) {
	content, err := os.ReadFile(IdentityFilePath(p))
	if err != nil {
		return AgeIdentity{}, false
	}
	identity, err := ParseIdentityFile(string(content))
	if err != nil {
		return AgeIdentity{}, false
	}
	return identity, true
}

// RecipientValid checks the frozen age recipient shape. Cryptographic callers
// additionally parse the recipient with filippo.io/age, which also verifies
// its bech32 checksum.
func RecipientValid(recipient string) bool {
	if len(recipient) != 62 || !strings.HasPrefix(recipient, "age1") {
		return false
	}
	for _, c := range recipient[4:] {
		if !strings.ContainsRune("023456789acdefghjklmnpqrstuvwxyz", c) {
			return false
		}
	}
	return true
}
