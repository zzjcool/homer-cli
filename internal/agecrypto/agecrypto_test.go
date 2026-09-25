package agecrypto

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
)

type goldenAgeVector struct {
	Input struct {
		Plaintext  string   `json:"plaintext"`
		Recipients []string `json:"recipients"`
	} `json:"input"`
	Output struct {
		Plaintext        string   `json:"plaintext"`
		CiphertextBase64 string   `json:"ciphertextBase64"`
		Recipients       []string `json:"recipients"`
		Identities       []string `json:"identities"`
	} `json:"output"`
}

func goldenAgeDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "golden", "age")
}

func TestGoldenAgeVectorsAndReverseRoundTrip(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(goldenAgeDir(t), "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	if len(files) != 4 {
		t.Fatalf("golden age vector count = %d, want 4", len(files))
	}

	for _, file := range files {
		file := file
		t.Run(filepath.Base(file), func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var vector goldenAgeVector
			if err := json.Unmarshal(data, &vector); err != nil {
				t.Fatal(err)
			}
			ciphertext, err := base64.StdEncoding.DecodeString(vector.Output.CiphertextBase64)
			if err != nil {
				t.Fatal(err)
			}
			want := []byte(vector.Output.Plaintext)
			if vector.Input.Plaintext != vector.Output.Plaintext {
				t.Fatalf("input/output plaintext mismatch")
			}
			if len(vector.Output.Identities) != len(vector.Output.Recipients) {
				t.Fatalf("identity/recipient count mismatch")
			}
			for i, secretKey := range vector.Output.Identities {
				identity, ok := mustParseIdentity(t, secretKey)
				if !ok {
					t.Fatal("golden identity did not parse")
				}
				if identity.Recipient != vector.Output.Recipients[i] {
					t.Fatalf("identity %d recipient = %s, want golden recipient", i, identity.Recipient)
				}
				got, err := DecryptWithIdentity(ciphertext, identity)
				if err != nil {
					t.Fatalf("Go decrypt of TS ciphertext: %v", err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("Go decrypt mismatch: got %x, want %x", got, want)
				}
			}

			// The TS implementation is not part of the Go test command. The
			// reverse direction is still checked with each throwaway vector
			// identity, proving that all recipients in a Go-produced header work.
			goCiphertext, err := EncryptToRecipients(want, vector.Output.Recipients)
			if err != nil {
				t.Fatalf("Go encrypt: %v", err)
			}
			for _, secretKey := range vector.Output.Identities {
				identity, ok := mustParseIdentity(t, secretKey)
				if !ok {
					t.Fatal("golden identity did not parse")
				}
				got, err := DecryptWithIdentity(goCiphertext, identity)
				if err != nil {
					t.Fatalf("Go decrypt of Go ciphertext: %v", err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("Go roundtrip mismatch: got %x, want %x", got, want)
				}
			}
		})
	}
}

func mustParseIdentity(t *testing.T, secretKey string) (AgeIdentity, bool) {
	t.Helper()
	identity, err := ParseIdentityFile(secretKey + "\n")
	if err != nil {
		return AgeIdentity{}, false
	}
	return identity, true
}

func TestKeyGenWriteLoadAndRejectOverwrite(t *testing.T) {
	home := t.TempDir()
	paths := core.GetHomerPaths(func(string) string { return home })

	identity, err := KeyGen(paths)
	if err != nil {
		t.Fatalf("KeyGen: %v", err)
	}
	loaded, ok := LoadIdentity(paths)
	if !ok || loaded != identity {
		t.Fatalf("LoadIdentity = %#v, %v; want %#v, true", loaded, ok, identity)
	}
	if mode := os.FileMode(statMode(t, IdentityFilePath(paths))); mode.Perm() != 0o600 {
		t.Fatalf("identity mode = %o, want 600", mode.Perm())
	}
	if mode := os.FileMode(statMode(t, paths.KeysDir)); mode.Perm() != 0o700 {
		t.Fatalf("keys directory mode = %o, want 700", mode.Perm())
	}
	before, err := os.ReadFile(IdentityFilePath(paths))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := KeyGen(paths); err == nil {
		t.Fatal("second KeyGen succeeded")
	} else if bytes.Contains([]byte(err.Error()), []byte(identity.SecretKey)) {
		t.Fatal("private key appeared in overwrite error")
	}
	after, err := os.ReadFile(IdentityFilePath(paths))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("overwrite changed the original identity")
	}
}

func statMode(t *testing.T, path string) uint32 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return uint32(info.Mode())
}

func TestParseIdentityAndLoadCorruptionAreSafe(t *testing.T) {
	identity := GenerateIdentity()
	parsed, err := ParseIdentityFile("# generated\r\n\r\n" + strings.ToLower(identity.SecretKey) + "\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if parsed != identity {
		t.Fatalf("parsed identity = %#v, want %#v", parsed, identity)
	}
	tamperedLast := "A"
	if identity.SecretKey[len(identity.SecretKey)-1] == 'A' {
		tamperedLast = "C"
	}
	for _, content := range []string{"", "garbage", identity.SecretKey + "\n" + identity.SecretKey + "\n", identity.SecretKey[:len(identity.SecretKey)-1] + tamperedLast} {
		_, err := ParseIdentityFile(content)
		if err == nil {
			t.Fatal("invalid identity parsed successfully")
		}
		if _, ok := err.(core.CliError); !ok {
			t.Fatalf("error type = %T, want core.CliError", err)
		}
		for start := 0; start+12 <= len(identity.SecretKey); start++ {
			if strings.Contains(err.Error(), identity.SecretKey[start:start+12]) {
				t.Fatalf("private key window appeared in error: %q", err)
			}
		}
	}

	home := t.TempDir()
	paths := core.GetHomerPaths(func(string) string { return home })
	if err := os.MkdirAll(paths.KeysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(IdentityFilePath(paths), []byte("broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := LoadIdentity(paths); ok || got != (AgeIdentity{}) {
		t.Fatalf("corrupt LoadIdentity = %#v, %v; want zero, false", got, ok)
	}
}

func TestVaultMultiRecipientRoundTripAndAtomicLayout(t *testing.T) {
	homeA := t.TempDir()
	pathsA := core.GetHomerPaths(func(string) string { return homeA })
	a := GenerateIdentity()
	b := GenerateIdentity()
	if _, err := KeyGen(pathsA); err != nil {
		t.Fatal(err)
	}
	// Keep the generated local identity deterministic for the vault read by
	// replacing only the temporary test setup with a's identity.
	if err := os.Remove(IdentityFilePath(pathsA)); err != nil {
		t.Fatal(err)
	}
	if err := WriteIdentityFile(pathsA, a); err != nil {
		t.Fatal(err)
	}

	plaintext := []byte("shared secret across two recipients")
	if err := EncryptSecretToFile(NewAgeCryptoPort(), pathsA, "shared", plaintext, []string{a.Recipient, b.Recipient}); err != nil {
		t.Fatal(err)
	}
	ciphertext, err := os.ReadFile(filepath.Join(pathsA.SecretsDir, "shared.age"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, plaintext) || len(ciphertext) == len(plaintext) && bytes.Equal(ciphertext, plaintext) {
		t.Fatal("vault contains plaintext")
	}
	got, err := DecryptSecretFromFile(NewAgeCryptoPort(), pathsA, "shared")
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("recipient A decrypt = %q, %v", got, err)
	}

	homeB := t.TempDir()
	pathsB := core.GetHomerPaths(func(string) string { return homeB })
	if err := WriteIdentityFile(pathsB, b); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pathsB.SecretsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pathsB.SecretsDir, "shared.age"), ciphertext, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = DecryptSecretFromFile(NewAgeCryptoPort(), pathsB, "shared")
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("recipient B decrypt = %q, %v", got, err)
	}
	entries := ListSecrets(pathsA, &core.HomerConfig{Secrets: &core.SecretsConfig{Files: map[string]string{"shared": "~/shared", "missing": "/tmp/missing"}}})
	if len(entries) != 2 || entries[0].Name != "missing" || entries[0].VaultFile != "missing" || entries[1].VaultFile != "present" {
		t.Fatalf("ListSecrets = %#v", entries)
	}
}

type leakingPort struct{ data []byte }

func (p leakingPort) Encrypt([]byte, []string) ([]byte, error) {
	return append([]byte(nil), p.data...), nil
}
func (p leakingPort) Decrypt(ciphertext []byte, _ AgeIdentity) ([]byte, error) {
	return append([]byte(nil), ciphertext...), nil
}

func TestCiphertextLooksSafeAndWriteVaultCiphertext(t *testing.T) {
	home := t.TempDir()
	paths := core.GetHomerPaths(func(string) string { return home })
	identity := GenerateIdentity()
	plaintext := []byte("a sufficiently long secret for the exported vault self-check")
	ciphertext, err := EncryptToRecipients(plaintext, []string{identity.Recipient})
	if err != nil {
		t.Fatal(err)
	}
	if !CiphertextLooksSafe(ciphertext, plaintext) {
		t.Fatal("valid age ciphertext was rejected")
	}
	if CiphertextLooksSafe(plaintext, plaintext) {
		t.Fatal("plaintext passthrough was accepted")
	}
	leaking := append([]byte("prefix"), plaintext...)
	if CiphertextLooksSafe(leaking, plaintext) {
		t.Fatal("ciphertext containing the plaintext sample was accepted")
	}

	if err := WriteVaultCiphertext(paths, "received", ciphertext); err != nil {
		t.Fatal(err)
	}
	file, err := SecretFilePath(paths, "received")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, ciphertext) {
		t.Fatalf("stored ciphertext = %x, want %x", stored, ciphertext)
	}
	if mode := os.FileMode(statMode(t, file)); mode.Perm() != 0o600 {
		t.Fatalf("vault mode = %o, want 600", mode.Perm())
	}
	if mode := os.FileMode(statMode(t, paths.SecretsDir)); mode.Perm() != 0o700 {
		t.Fatalf("secrets dir mode = %o, want 700", mode.Perm())
	}
	if err := WriteVaultCiphertext(paths, "../escape", ciphertext); err == nil {
		t.Fatal("invalid vault name was accepted")
	} else if _, ok := err.(core.CliError); !ok {
		t.Fatalf("invalid name error = %T, want core.CliError", err)
	}
}

func TestVaultCiphertextSelfCheckCatchesShortAndTailLeaks(t *testing.T) {
	home := t.TempDir()
	paths := core.GetHomerPaths(func(string) string { return home })
	short := []byte("SHORT-KEY!")
	if err := EncryptSecretToFile(leakingPort{data: short}, paths, "short", short, []string{"test-recipient"}); err == nil {
		t.Fatal("short passthrough was accepted")
	}
	if _, err := os.Stat(paths.SecretsDir); !os.IsNotExist(err) {
		t.Fatalf("secrets directory after rejected short plaintext: %v", err)
	}

	plaintext := bytes.Repeat([]byte{'-'}, 400)
	marker := []byte("TAIL-SEGMENT-MARKER-0123456789")
	copy(plaintext[len(plaintext)-len(marker):], marker)
	ciphertext := append([]byte(nil), plaintext...)
	for i := 0; i < 100; i++ {
		ciphertext[i] = 'z'
	}
	if err := EncryptSecretToFile(leakingPort{data: ciphertext}, paths, "tail", plaintext, []string{"test-recipient"}); err == nil {
		t.Fatal("tail-leaking ciphertext was accepted")
	}
}
