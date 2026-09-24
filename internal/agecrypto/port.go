package agecrypto

import (
	"bytes"
	"io"

	"filippo.io/age"

	"github.com/zzjcool/homer-cli/internal/core"
)

// AgeCryptoPort is the seam consumed by doctor, secret, and home command
// layers. Tests can inject a fake port without importing filippo.io/age.
type AgeCryptoPort interface {
	Encrypt(plaintext []byte, recipients []string) ([]byte, error)
	Decrypt(ciphertext []byte, identity AgeIdentity) ([]byte, error)
}

type defaultAgeCryptoPort struct{}

// NewAgeCryptoPort returns the production filippo.io/age implementation.
func NewAgeCryptoPort() AgeCryptoPort { return defaultAgeCryptoPort{} }

func (defaultAgeCryptoPort) Encrypt(plaintext []byte, recipients []string) ([]byte, error) {
	return EncryptToRecipients(plaintext, recipients)
}

func (defaultAgeCryptoPort) Decrypt(ciphertext []byte, identity AgeIdentity) ([]byte, error) {
	return DecryptWithIdentity(ciphertext, identity)
}

func invalidRecipientError() error {
	return core.NewCliError("invalid age recipient")
}

func encryptionError() error { return core.NewCliError("age encryption failed") }

func decryptionError() error { return core.NewCliError("age decryption failed") }

// EncryptToRecipients produces a binary age-v1 ciphertext. Every recipient is
// added to the same age header, so each corresponding identity can decrypt the
// resulting bytes independently.
func EncryptToRecipients(plaintext []byte, recipients []string) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, core.NewCliError("no age recipients configured")
	}

	parsed := make([]age.Recipient, 0, len(recipients))
	for _, recipient := range recipients {
		if !RecipientValid(recipient) {
			return nil, invalidRecipientError()
		}
		value, err := age.ParseX25519Recipient(recipient)
		if err != nil {
			// Do not return the package error: it can echo an untrusted
			// recipient string, and a misconfigured value might be a private key.
			return nil, invalidRecipientError()
		}
		parsed = append(parsed, value)
	}

	var ciphertext bytes.Buffer
	writer, err := age.Encrypt(&ciphertext, parsed...)
	if err != nil {
		return nil, encryptionError()
	}
	if _, err := writer.Write(plaintext); err != nil {
		_ = writer.Close()
		return nil, encryptionError()
	}
	if err := writer.Close(); err != nil {
		return nil, encryptionError()
	}
	return ciphertext.Bytes(), nil
}

// DecryptWithIdentity decrypts a binary age-v1 ciphertext using one local
// X25519 identity. All parse and cryptographic failures are intentionally
// collapsed to a fixed CliError so no private key or ciphertext detail can
// enter user-facing error output.
func DecryptWithIdentity(ciphertext []byte, identity AgeIdentity) ([]byte, error) {
	parsed, err := parseSecretKey(identity.SecretKey)
	if err != nil {
		return nil, decryptionError()
	}
	reader, err := age.Decrypt(bytes.NewReader(ciphertext), parsed)
	if err != nil {
		return nil, decryptionError()
	}
	plaintext, err := io.ReadAll(reader)
	if err != nil {
		return nil, decryptionError()
	}
	return plaintext, nil
}
