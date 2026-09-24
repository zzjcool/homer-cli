package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/agecrypto"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gitx"
)

type secretTestEnv struct {
	root   string
	home   string
	origin string
	paths  core.HomerPaths
}

func newSecretTestEnv(t *testing.T) secretTestEnv {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	home := filepath.Join(root, "home")
	mustGit(t, root, "init", "--bare", "-b", "main", origin)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, home, "init", "-b", "main")
	mustGit(t, home, "config", "user.email", "homer-test@example.invalid")
	mustGit(t, home, "config", "user.name", "Homer Test")
	mustGit(t, home, "remote", "add", "origin", origin)
	paths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return home
		}
		return os.Getenv(name)
	})
	if err := os.MkdirAll(filepath.Join(home, "store", "pi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "store", "pi", "baseline.txt"), []byte("baseline\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if commit := gitx.CommitAllStore(home, "store baseline"); commit == "" {
		t.Fatal("store baseline did not commit")
	}
	mustGit(t, home, "push", "-u", "origin", "main")
	return secretTestEnv{root: root, home: home, origin: origin, paths: paths}
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	result := gitx.Exec(dir, args, 0)
	if !result.OK {
		t.Fatalf("git %s: %s", strings.Join(args, " "), result.Stderr)
	}
	return result.Stdout
}

func saveSecretsConfig(t *testing.T, env secretTestEnv, recipients []string, files map[string]string) {
	t.Helper()
	if err := core.SaveConfig(env.paths, core.HomerConfig{
		Version:  1,
		Adapters: map[string]core.AdapterConfig{},
		Secrets:  &core.SecretsConfig{Recipients: recipients, Files: files},
	}); err != nil {
		t.Fatal(err)
	}
}

func writeDestination(t *testing.T, name, contents string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return name
}

func modePerm(t *testing.T, name string) os.FileMode {
	t.Helper()
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func setupSecretPush(t *testing.T, second bool) (secretTestEnv, agecrypto.AgeIdentity, agecrypto.AgeIdentity, string, string) {
	t.Helper()
	env := newSecretTestEnv(t)
	a := agecrypto.GenerateIdentity()
	b := agecrypto.GenerateIdentity()
	if err := agecrypto.WriteIdentityFile(env.paths, a); err != nil {
		t.Fatal(err)
	}
	destA := writeDestination(t, filepath.Join(env.root, "targets", "a.env"), "API_TOKEN=HOMER-SECRET-FRAGMENT-123456789\n")
	destB := writeDestination(t, filepath.Join(env.root, "targets", "b.env"), "SECOND-SECRET-FRAGMENT-987654321\n")
	recipients := []string{a.Recipient}
	if second {
		recipients = append(recipients, b.Recipient)
	}
	saveSecretsConfig(t, env, recipients, map[string]string{"a-secret": destA, "b-secret": destB})
	return env, a, b, destA, destB
}

func TestSecretKeygenRecipientOnlyAndNoOverwrite(t *testing.T) {
	env := newSecretTestEnv(t)
	var out bytes.Buffer
	if code := ExecuteSecret("keygen", SecretCommandOptions{HomerHome: env.home, JSON: true}, nil, &out, &out); code != 0 {
		t.Fatalf("keygen exit = %d: %s", code, out.String())
	}
	if strings.Contains(out.String(), "AGE-SECRET-KEY") {
		t.Fatalf("private key leaked: %s", out.String())
	}
	var report SecretKeygenReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(report.IdentityFile)
	if err != nil {
		t.Fatal(err)
	}
	if modePerm(t, report.IdentityFile) != 0o600 || modePerm(t, filepath.Dir(report.IdentityFile)) != 0o700 {
		t.Fatalf("identity permissions = file %o dir %o", modePerm(t, report.IdentityFile), modePerm(t, filepath.Dir(report.IdentityFile)))
	}
	if code := ExecuteSecret("keygen", SecretCommandOptions{HomerHome: env.home}, nil, &out, &out); code != 1 {
		t.Fatalf("duplicate keygen exit = %d", code)
	}
	after, err := os.ReadFile(report.IdentityFile)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("identity changed after duplicate: %v", err)
	}
}

type stableSecretCrypto struct{}

func (stableSecretCrypto) Encrypt([]byte, []string) ([]byte, error) {
	return []byte("stable-age-ciphertext"), nil
}

func (stableSecretCrypto) Decrypt([]byte, agecrypto.AgeIdentity) ([]byte, error) {
	return nil, nil
}

func TestSecretPushIdempotentReportsNoNewCommit(t *testing.T) {
	env, _, _, _, _ := setupSecretPush(t, false)
	deps := &SecretDeps{Age: stableSecretCrypto{}}
	first := RunSecretPush(SecretPushOptions{HomerHome: env.home, Yes: true}, deps)
	if first.Status != SecretPushStatusPushed || first.Commit == "" {
		t.Fatalf("first secret push = %#v", first)
	}
	second := RunSecretPush(SecretPushOptions{HomerHome: env.home, Yes: true}, deps)
	if second.Status != SecretPushStatusPushed || second.Commit != "" {
		t.Fatalf("idempotent secret push = %#v", second)
	}
	if !strings.Contains(strings.Join(second.Warnings, "\n"), "vault 内容与 HEAD 一致，未产生新 commit") {
		t.Fatalf("idempotent warning = %#v", second.Warnings)
	}
}

func TestSecretPushMultiRecipientGitEvidenceAndScope(t *testing.T) {
	env, a, b, destA, _ := setupSecretPush(t, true)
	// Keep unrelated dirty files in the worktree. CommitPaths must not absorb
	// either store changes or an untracked note.
	if err := os.WriteFile(filepath.Join(env.home, "store", "pi", "dirty.txt"), []byte("store drift"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.home, "notes.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := RunSecretPush(SecretPushOptions{HomerHome: env.home, Yes: true}, &SecretDeps{Age: agecrypto.NewAgeCryptoPort()})
	if report.Status != SecretPushStatusPushed || !report.PushedToRemote || len(report.Encrypted) != 2 {
		t.Fatalf("push report = %#v", report)
	}
	files := strings.TrimSpace(mustGit(t, env.home, "show", "--name-only", "--format=", "HEAD"))
	if files != "secrets/a-secret.age\nsecrets/b-secret.age" {
		t.Fatalf("secret commit paths = %q", files)
	}
	for _, name := range []string{"a-secret", "b-secret"} {
		ciphertext, err := gitx.ReadVaultFileAtCommit(env.origin, "secrets/"+name+".age", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		for _, identity := range []agecrypto.AgeIdentity{a, b} {
			plaintext, decryptErr := agecrypto.DecryptWithIdentity(ciphertext, identity)
			if decryptErr != nil || len(plaintext) == 0 {
				t.Fatalf("%s decrypt with %s: %v", name, identity.Recipient, decryptErr)
			}
		}
	}
	if grep := gitx.Exec(env.origin, []string{"grep", "HOMER-SECRET-FRAGMENT-123456789", "HEAD"}, 0); grep.OK || grep.Stdout != "" {
		t.Fatalf("plaintext found in origin: %#v", grep)
	}
	if grep := gitx.Exec(env.origin, []string{"grep", "-I", "AGE-SECRET-KEY", "HEAD"}, 0); grep.OK || grep.Stdout != "" {
		t.Fatalf("private key found in origin: %#v", grep)
	}
	if got := strings.TrimSpace(mustGit(t, env.home, "status", "--porcelain", "--", "store/")); got == "" {
		t.Fatal("store drift was unexpectedly committed")
	}
	if got := strings.TrimSpace(mustGit(t, env.home, "status", "--porcelain", "--", "notes.txt")); got == "" {
		t.Fatal("unrelated note was unexpectedly committed")
	}
	if string(mustRead(t, destA)) == "" {
		t.Fatal("source disappeared")
	}
}

func TestSecretPushMissingSourceNoVaultAndNoIdentityRecipient(t *testing.T) {
	env, _, _, destA, destB := setupSecretPush(t, false)
	if err := os.Remove(destB); err != nil {
		t.Fatal(err)
	}
	report := RunSecretPush(SecretPushOptions{HomerHome: env.home, Yes: true}, nil)
	if report.Status != SecretPushStatusMissing || len(report.Encrypted) != 0 {
		t.Fatalf("missing source report = %#v", report)
	}
	if _, err := os.Stat(env.paths.SecretsDir); !os.IsNotExist(err) {
		t.Fatalf("vault directory after missing source: %v", err)
	}
	if _, err := os.Stat(destA); err != nil {
		t.Fatal(err)
	}

	noIdentity := newSecretTestEnv(t)
	dest := writeDestination(t, filepath.Join(noIdentity.root, "target"), "long-enough-secret-content\n")
	other := agecrypto.GenerateIdentity()
	saveSecretsConfig(t, noIdentity, []string{other.Recipient}, map[string]string{"s": dest})
	if got := RunSecretPush(SecretPushOptions{HomerHome: noIdentity.home, Yes: true}, nil); got.Status != SecretPushStatusNoIdentity {
		t.Fatalf("no identity = %#v", got)
	}
	identity := agecrypto.GenerateIdentity()
	if err := agecrypto.WriteIdentityFile(noIdentity.paths, identity); err != nil {
		t.Fatal(err)
	}
	saveSecretsConfig(t, noIdentity, nil, map[string]string{"s": dest})
	if got := RunSecretPush(SecretPushOptions{HomerHome: noIdentity.home, Yes: true}, nil); got.Status != SecretPushStatusNoRecipients {
		t.Fatalf("no recipients = %#v", got)
	}
}

func TestSecretPullUpstreamBackupPermissionsAndNoFF(t *testing.T) {
	env, _, _, destA, destB := setupSecretPush(t, true)
	pushed := RunSecretPush(SecretPushOptions{HomerHome: env.home, Yes: true}, nil)
	if pushed.Status != SecretPushStatusPushed {
		t.Fatalf("push = %#v", pushed)
	}
	oldHead := strings.TrimSpace(mustGit(t, env.home, "rev-parse", "HEAD~1"))
	mustGit(t, env.home, "reset", "--hard", "HEAD~1")
	if err := os.RemoveAll(env.paths.SecretsDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destA, []byte("OLD-A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(destB); err != nil {
		t.Fatal(err)
	}

	report := RunSecretPull(SecretPullOptions{HomerHome: env.home, Yes: true}, nil)
	if report.Status != SecretPullStatusApplied || len(report.Pulled) != 2 {
		t.Fatalf("pull = %#v", report)
	}
	if string(mustRead(t, destA)) != "API_TOKEN=HOMER-SECRET-FRAGMENT-123456789\n" || modePerm(t, destA) != 0o600 || modePerm(t, destB) != 0o600 {
		t.Fatalf("destination write or mode failed")
	}
	if report.BackupDir == "" {
		t.Fatal("missing backup directory")
	}
	backupFile := filepath.Join(report.BackupDir, "secret", "a-secret")
	if string(mustRead(t, backupFile)) != "OLD-A\n" || modePerm(t, backupFile) != 0o600 {
		t.Fatal("backup content or mode failed")
	}
	for _, dir := range []string{env.paths.BackupsDir, filepath.Dir(report.BackupDir), report.BackupDir} {
		if modePerm(t, dir) != 0o700 {
			t.Fatalf("backup dir %s mode = %o", dir, modePerm(t, dir))
		}
	}
	if got := strings.TrimSpace(mustGit(t, env.home, "rev-parse", "HEAD")); got != oldHead {
		t.Fatalf("secret pull unexpectedly fast-forwarded HEAD: %s != %s", got, oldHead)
	}
	if _, err := os.Stat(filepath.Join(env.paths.SecretsDir, "a-secret.age")); !os.IsNotExist(err) {
		t.Fatalf("pull unexpectedly wrote workspace vault: %v", err)
	}
}

func TestSecretPullSuccessOutputIncludesWorkspaceSyncHint(t *testing.T) {
	env, _, _, _, _ := setupSecretPush(t, false)
	if got := RunSecretPush(SecretPushOptions{HomerHome: env.home, Yes: true}, nil); got.Status != SecretPushStatusPushed {
		t.Fatalf("push = %#v", got)
	}

	hint := "git -C " + env.home + " fetch origin && git -C " + env.home + " merge --ff-only origin/master"
	var human bytes.Buffer
	if code := ExecuteSecret("pull", SecretCommandOptions{HomerHome: env.home, Yes: true}, nil, &human, &human); code != 0 {
		t.Fatalf("human pull exit = %d: %s", code, human.String())
	}
	if !strings.Contains(human.String(), "提示: vault 已更新") || !strings.Contains(human.String(), hint) {
		t.Fatalf("human pull hint = %q", human.String())
	}

	var machine bytes.Buffer
	if code := ExecuteSecret("pull", SecretCommandOptions{HomerHome: env.home, Yes: true, JSON: true}, nil, &machine, &machine); code != 0 {
		t.Fatalf("json pull exit = %d: %s", code, machine.String())
	}
	var parsed map[string]any
	if err := json.Unmarshal(machine.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	warnings, ok := parsed["warnings"].([]any)
	if !ok {
		t.Fatalf("json pull warnings field = %#v", parsed["warnings"])
	}
	found := false
	for _, warning := range warnings {
		if strings.Contains(warning.(string), hint) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("json pull hint missing: %#v", warnings)
	}
}

func TestSecretPullUndecryptableZeroWritesAndFetchRollbackGuard(t *testing.T) {
	env, a, _, destA, _ := setupSecretPush(t, false)
	if got := RunSecretPush(SecretPushOptions{HomerHome: env.home, Yes: true}, nil); got.Status != SecretPushStatusPushed {
		t.Fatalf("push = %#v", got)
	}
	before := mustRead(t, destA)
	other := agecrypto.GenerateIdentity()
	if err := os.Remove(agecrypto.IdentityFilePath(env.paths)); err != nil {
		t.Fatal(err)
	}
	if err := agecrypto.WriteIdentityFile(env.paths, other); err != nil {
		t.Fatal(err)
	}
	if got := RunSecretPull(SecretPullOptions{HomerHome: env.home, Yes: true}, nil); got.Status != SecretPullStatusUndecryptable || !bytes.Equal(before, mustRead(t, destA)) {
		t.Fatalf("undecryptable pull = %#v", got)
	}

	// Restore A and create the review-fixed fetch failure fork: origin/main has
	// the new ciphertext, while the worktree contains an older, different one.
	if err := os.Remove(agecrypto.IdentityFilePath(env.paths)); err != nil {
		t.Fatal(err)
	}
	if err := agecrypto.WriteIdentityFile(env.paths, a); err != nil {
		t.Fatal(err)
	}
	mustGit(t, env.home, "reset", "--hard", "HEAD~1")
	stale, err := agecrypto.EncryptToRecipients([]byte("STALE-OLD-CIPHERTEXT-CONTENT\n"), []string{a.Recipient})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(env.paths.SecretsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.paths.SecretsDir, "a-secret.age"), stale, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(env.paths.SecretsDir, "a-secret.age"), 0o600); err != nil {
		t.Fatal(err)
	}
	before = mustRead(t, destA)
	mustGit(t, env.home, "remote", "set-url", "origin", filepath.Join(env.root, "missing-origin.git"))
	got := RunSecretPull(SecretPullOptions{HomerHome: env.home, Yes: true}, nil)
	if got.Status != SecretPullStatusError || !strings.Contains(strings.Join(got.Errors, "\n"), "拒绝写旧值") || !bytes.Equal(before, mustRead(t, destA)) {
		t.Fatalf("rollback guard = %#v", got)
	}
}

func TestSecretListJSONShapeAndNoIdentityRequired(t *testing.T) {
	env := newSecretTestEnv(t)
	dest := writeDestination(t, filepath.Join(env.root, "dest"), "not used")
	saveSecretsConfig(t, env, nil, map[string]string{"z": dest, "a": filepath.Join(env.root, "missing")})
	if err := os.MkdirAll(env.paths.SecretsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.paths.SecretsDir, "z.age"), []byte("ciphertext"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := ExecuteSecret("list", SecretCommandOptions{HomerHome: env.home, JSON: true}, nil, &out, &out); code != 0 {
		t.Fatalf("list exit = %d: %s", code, out.String())
	}
	var parsed SecretListReport
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Secrets) != 2 || parsed.Secrets[0].Name != "a" || parsed.Secrets[1].VaultFile != "present" {
		t.Fatalf("list report = %#v", parsed)
	}
	if _, err := os.Stat(filepath.Join(env.paths.SecretsDir, "z.age")); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
