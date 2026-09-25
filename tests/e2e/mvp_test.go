package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/core"
)

type machine struct {
	name      string
	fakeHome  string
	homerHome string
	gitGlobal string
}

type processResult struct {
	code   int
	stdout string
	stderr string
}

type e2eWorld struct {
	binary      string
	origin      string
	root        string
	gitGlobal   string
	a           machine
	b           machine
	initReport  map[string]any
	keygenA     map[string]any
	secretPushA map[string]any
	pushA       map[string]any
	homeReport  map[string]any
	keygenB     map[string]any
	secretPushB map[string]any
	secretPullB map[string]any
}

const (
	secretName      = "a-secret"
	secretDest      = "~/.secrets/a.env"
	secretFragment  = "W11-E2E-PLAINTEXT-FRAGMENT-3f91c7"
	secretPlaintext = "API_TOKEN=" + secretFragment + "\nsecond-line-body-long-enough\n"
)

func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

func buildHomer(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "homer")
	command := exec.Command("go", "build", "-o", binary, "./cmd/homer")
	command.Dir = repoRoot()
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("go build ./cmd/homer: %v\n%s", err, stderr.String())
	}
	return binary
}

func baseEnv(global string) []string {
	env := os.Environ()
	env = setEnv(env, "GIT_CONFIG_GLOBAL", global)
	env = setEnv(env, "GIT_CONFIG_NOSYSTEM", "1")
	return env
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	filtered := make([]string, 0, len(env)+1)
	for _, item := range env {
		if !strings.HasPrefix(item, prefix) {
			filtered = append(filtered, item)
		}
	}
	return append(filtered, prefix+value)
}

func runProcess(t *testing.T, dir, global string, env []string, name string, args ...string) processResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.Env = baseEnv(global)
	for _, item := range env {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) == 2 {
			command.Env = setEnv(command.Env, parts[0], parts[1])
		}
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			code = -1
		}
	}
	if ctx.Err() != nil {
		code = -1
		stderr.WriteString("context deadline exceeded")
	}
	return processResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func runGit(t *testing.T, global, dir string, args ...string) string {
	t.Helper()
	result := runProcess(t, dir, global, nil, "git", args...)
	if result.code != 0 {
		t.Fatalf("git %s failed (exit %d):\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), result.code, result.stdout, result.stderr)
	}
	return result.stdout
}

func runHomer(t *testing.T, binary string, m machine, args ...string) processResult {
	t.Helper()
	return runProcess(t, repoRoot(), m.gitGlobal, []string{
		"HOME=" + m.fakeHome,
		"HOMER_HOME=" + m.homerHome,
	}, binary, args...)
}

func mustJSON(t *testing.T, result processResult, command string) map[string]any {
	t.Helper()
	if result.code != 0 {
		t.Fatalf("homer %s failed (exit %d):\nstdout:\n%s\nstderr:\n%s", command, result.code, result.stdout, result.stderr)
	}
	if result.stderr != "" {
		t.Fatalf("homer %s wrote stderr:\n%s", command, result.stderr)
	}
	value := map[string]any{}
	if err := json.Unmarshal([]byte(result.stdout), &value); err != nil {
		t.Fatalf("homer %s did not produce JSON: %v\n%s", command, err, result.stdout)
	}
	return value
}

func mustJSONHomer(t *testing.T, binary string, m machine, args ...string) map[string]any {
	t.Helper()
	return mustJSON(t, runHomer(t, binary, m, args...), strings.Join(args, " "))
}

func writeFile(t *testing.T, filename, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(filename), err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
}

func listFiles(root string) []string {
	files := []string{}
	_ = filepath.WalkDir(root, func(filename string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if filename == root || entry.IsDir() {
			return nil
		}
		if entry.Type().IsRegular() {
			rel, relErr := filepath.Rel(root, filename)
			if relErr == nil {
				files = append(files, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	sort.Strings(files)
	return files
}

func piRoot(m machine) string       { return filepath.Join(m.fakeHome, ".pi", "agent") }
func herdrRoot(m machine) string    { return filepath.Join(m.fakeHome, ".config", "herdr") }
func opencodeRoot(m machine) string { return filepath.Join(m.fakeHome, ".config", "opencode") }

func makeMachine(t *testing.T, root, name, global string, fixtures bool) machine {
	t.Helper()
	fakeHome := filepath.Join(root, name, "home")
	homerHome := filepath.Join(root, name, "homer")
	if err := os.MkdirAll(fakeHome, 0o755); err != nil {
		t.Fatalf("mkdir fake home: %v", err)
	}
	m := machine{name: name, fakeHome: fakeHome, homerHome: homerHome, gitGlobal: global}
	if fixtures {
		writeFixtures(t, m)
	} else {
		for _, root := range []string{piRoot(m), herdrRoot(m), opencodeRoot(m)} {
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatalf("mkdir adapter root %s: %v", root, err)
			}
		}
	}
	return m
}

func writeFixtures(t *testing.T, m machine) {
	t.Helper()
	pi := piRoot(m)
	writeFile(t, filepath.Join(pi, "settings.json"), "{\n  \"theme\": \"light\",\n  \"keep\": 1\n}\n")
	writeFile(t, filepath.Join(pi, "models.json"), "{\n  \"apiKeys\": {\"openai\": \"sk-a\"},\n  \"models\": {\"openai\": {\"id\": \"gpt\"}}\n}\n")
	writeFile(t, filepath.Join(pi, "skills", "alpha", "SKILL.md"), "# alpha\n")
	writeFile(t, filepath.Join(pi, "skills", "beta", "SKILL.md"), "# beta\n")
	writeFile(t, filepath.Join(pi, "auth.json"), "{\"token\":\"ignored\"}\n")
	writeFile(t, filepath.Join(pi, "trust.json"), "{\"trusted\":[]}\n")
	writeFile(t, filepath.Join(pi, "sessions", "s1.jsonl"), "{\"turn\":1}\n")
	writeFile(t, filepath.Join(pi, "run-history.jsonl"), "{}\n")

	external := filepath.Join(filepath.Dir(m.fakeHome), "external", "browser")
	writeFile(t, filepath.Join(external, "SKILL.md"), "# agent-browser (outside root)\n")
	writeFile(t, filepath.Join(external, "sub", "a.ts"), "export const a = 1;\n")
	if err := os.Symlink(external, filepath.Join(pi, "skills", "agent-browser")); err != nil {
		t.Fatalf("symlink escape fixture: %v", err)
	}

	herdr := herdrRoot(m)
	writeFile(t, filepath.Join(herdr, "config.toml"), "[ui]\ntheme = \"dark\"\n")
	writeFile(t, filepath.Join(herdr, "session.json"), "{\"panes\":[]}\n")
	writeFile(t, filepath.Join(herdr, "herdr.sock"), "")
	writeFile(t, filepath.Join(herdr, "herdr.log"), "debug\n")
	writeFile(t, filepath.Join(herdr, "release-notes.json"), "{\"version\":\"1.2.3\"}\n")
	writeFile(t, filepath.Join(herdr, ".plugins.lock"), "{}\n")

	opencode := opencodeRoot(m)
	writeFile(t, filepath.Join(opencode, "opencode.json"), "{\n  \"provider\": {\"anthropic\": {\"options\": {\"apiKey\": \"ccrb\"}}}\n}\n")
	writeFile(t, filepath.Join(opencode, "package.json"), "{\n  \"name\": \"opencode-plugins\",\n  \"dependencies\": {\"plugin\": \"1.0.0\"}\n}\n")
	writeFile(t, filepath.Join(opencode, "package-lock.json"), "{\n  \"name\": \"opencode-plugins\",\n  \"lockfileVersion\": 3\n}\n")
	writeFile(t, filepath.Join(opencode, "node_modules", "plugin", "index.js"), "module.exports={}\n")
	writeFile(t, filepath.Join(opencode, ".plugins.lock"), "{}\n")
	writeFile(t, filepath.Join(opencode, "opencode.log"), "log\n")
	writeFile(t, filepath.Join(opencode, ".gitignore"), "node_modules\n")
}

func setupWorld(t *testing.T, binary string) *e2eWorld {
	t.Helper()
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer W11 E2E\n\temail = homer-w11@example.invalid\n")
	origin := filepath.Join(root, "origin.git")
	runGit(t, global, root, "init", "--bare", "-b", "main", origin)

	a := makeMachine(t, root, "A", global, true)
	runGit(t, global, root, "clone", origin, a.homerHome)
	initReport := mustJSONHomer(t, binary, a, "init", "--json")
	keygenA := mustJSONHomer(t, binary, a, "secret", "keygen", "--json")
	patchConfig(t, a, func(config map[string]any) {
		config["secrets"] = map[string]any{
			"recipients": []any{keygenA["recipient"].(string)},
			"files":      map[string]any{secretName: secretDest},
		}
		adapters := config["adapters"].(map[string]any)
		pi := adapters["pi"].(map[string]any)
		pi["allowEscape"] = []any{"skills/agent-browser"}
	})
	writeFile(t, filepath.Join(a.fakeHome, ".secrets", "a.env"), secretPlaintext)
	secretPushA := mustJSONHomer(t, binary, a, "secret", "push", "--yes", "--json")
	pushA := mustJSONHomer(t, binary, a, "push", "--yes", "--json")

	b := makeMachine(t, root, "B", global, false)
	homeReport := mustJSONHomer(t, binary, b, "home", origin, "--yes", "--json")
	keygenB := mustJSONHomer(t, binary, b, "secret", "keygen", "--json")
	patchConfig(t, a, func(config map[string]any) {
		secrets := config["secrets"].(map[string]any)
		recipients := secrets["recipients"].([]any)
		recipients = append(recipients, keygenB["recipient"].(string))
		secrets["recipients"] = recipients
	})
	secretPushB := mustJSONHomer(t, binary, a, "secret", "push", "--yes", "--json")
	secretPullB := mustJSONHomer(t, binary, b, "secret", "pull", "--yes", "--json")

	return &e2eWorld{
		binary: binary, origin: origin, root: root, gitGlobal: global,
		a: a, b: b,
		initReport: initReport, keygenA: keygenA, secretPushA: secretPushA,
		pushA: pushA, homeReport: homeReport, keygenB: keygenB,
		secretPushB: secretPushB, secretPullB: secretPullB,
	}
}

func patchConfig(t *testing.T, m machine, mutate func(map[string]any)) {
	t.Helper()
	filename := filepath.Join(m.homerHome, "homer.json")
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read homer.json: %v", err)
	}
	config := map[string]any{}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("parse homer.json: %v", err)
	}
	mutate(config)
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatalf("marshal homer.json: %v", err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(filename, encoded, 0o644); err != nil {
		t.Fatalf("write homer.json: %v", err)
	}
	runGit(t, m.gitGlobal, m.homerHome, "add", "homer.json")
	runGit(t, m.gitGlobal, m.homerHome, "commit", "--no-gpg-sign", "-m", "chore(e2e): update homer.json")
}

func findCheck(t *testing.T, report map[string]any, id string) map[string]any {
	t.Helper()
	checks, ok := report["checks"].([]any)
	if !ok {
		t.Fatalf("doctor report has no checks: %#v", report)
	}
	for _, item := range checks {
		check := item.(map[string]any)
		if check["id"] == id {
			return check
		}
	}
	t.Fatalf("doctor report has no check %q: %#v", id, report)
	return nil
}

func stringSlice(value any) []string {
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		result = append(result, item.(string))
	}
	return result
}

func expandRoot(root, fakeHome string) string {
	if root == "~" {
		return fakeHome
	}
	if strings.HasPrefix(root, "~/") {
		return filepath.Join(fakeHome, filepath.FromSlash(root[2:]))
	}
	return root
}

type storeEntry struct {
	rel      string
	content  []byte
	target   string
	category core.CategoryConfig
}

func storeEntries(t *testing.T, source, target machine, config *core.HomerConfig) []storeEntry {
	t.Helper()
	entries := []storeEntry{}
	store := filepath.Join(source.homerHome, "store")
	for _, rel := range listFiles(store) {
		if filepath.Base(rel) == core.StoreCompleteMarker {
			continue
		}
		parts := strings.Split(rel, "/")
		if len(parts) < 3 {
			t.Fatalf("unexpected store path: %s", rel)
		}
		adapterConfig, ok := config.Adapters[parts[0]]
		if !ok {
			t.Fatalf("store path has unknown adapter: %s", rel)
		}
		category, ok := adapterConfig.Categories[parts[1]]
		if !ok {
			t.Fatalf("store path has unknown category: %s", rel)
		}
		pathInCategory := strings.Join(parts[2:], "/")
		targetPath, err := adapter.ResolveCategoryFilePath(expandRoot(adapterConfig.Root, target.fakeHome), category, pathInCategory)
		if err != nil {
			t.Fatalf("resolve %s: %v", rel, err)
		}
		content, err := os.ReadFile(filepath.Join(store, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read store %s: %v", rel, err)
		}
		entries = append(entries, storeEntry{rel: rel, content: content, target: targetPath, category: category})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	return entries
}

func normalizedWithoutExcluded(t *testing.T, content []byte, keys []string) []byte {
	t.Helper()
	value := map[string]any{}
	if err := json.Unmarshal(content, &value); err != nil {
		t.Fatalf("expected JSON content: %v", err)
	}
	for _, key := range keys {
		delete(value, key)
	}
	result, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("normalize JSON: %v", err)
	}
	return result
}

func assertToolPlacement(t *testing.T, world *e2eWorld, config *core.HomerConfig) {
	t.Helper()
	entries := storeEntries(t, world.a, world.b, config)
	expected := map[string]struct{}{}
	for _, entry := range entries {
		expected[entry.target] = struct{}{}
		actual, err := os.ReadFile(entry.target)
		if err != nil {
			t.Fatalf("home did not place %s at %s: %v", entry.rel, entry.target, err)
		}
		if len(entry.category.ExcludeKeys) == 0 {
			if !bytes.Equal(actual, entry.content) {
				t.Fatalf("byte mismatch for %s: store=%q target=%q", entry.rel, entry.content, actual)
			}
			continue
		}
		if bytes.Contains(actual, []byte("__REQUIRED__")) {
			t.Fatalf("excluded placeholder leaked into target %s: %s", entry.rel, actual)
		}
		if !bytes.Equal(normalizedWithoutExcluded(t, actual, entry.category.ExcludeKeys), normalizedWithoutExcluded(t, entry.content, entry.category.ExcludeKeys)) {
			t.Fatalf("excluded-key JSON mismatch for %s", entry.rel)
		}
	}

	actualFiles := map[string]struct{}{}
	for _, root := range []string{piRoot(world.b), herdrRoot(world.b), opencodeRoot(world.b)} {
		for _, rel := range listFiles(root) {
			actualFiles[filepath.Join(root, filepath.FromSlash(rel))] = struct{}{}
		}
	}
	if len(expected) != len(actualFiles) {
		t.Fatalf("home placed file count = %d, want %d\nactual=%v\nexpected=%v", len(actualFiles), len(expected), actualFiles, expected)
	}
	for filename := range actualFiles {
		if _, ok := expected[filename]; !ok {
			t.Fatalf("unexpected file after home: %s", filename)
		}
	}
}

func originTree(t *testing.T, world *e2eWorld) string {
	t.Helper()
	return runGit(t, world.gitGlobal, world.root, "--git-dir", world.origin, "ls-tree", "-r", "--name-only", "HEAD")
}

func assertOriginHas(t *testing.T, world *e2eWorld, path string) {
	t.Helper()
	for _, item := range strings.Fields(originTree(t, world)) {
		if item == path {
			return
		}
	}
	t.Fatalf("origin tree does not contain %s:\n%s", path, originTree(t, world))
}

func assertOriginDoesNotContain(t *testing.T, world *e2eWorld, needle string) {
	t.Helper()
	revisions := strings.Fields(runGit(t, world.gitGlobal, world.root, "--git-dir", world.origin, "rev-list", "--all"))
	for _, revision := range revisions {
		result := runProcess(t, world.root, world.gitGlobal, nil, "git", "--git-dir", world.origin, "grep", "-n", "-e", needle, revision)
		if result.code == 0 {
			t.Fatalf("origin contains forbidden %q in %s:\n%s", needle, revision, result.stdout)
		}
		if result.code != 1 {
			t.Fatalf("git grep for %q failed with exit %d: %s", needle, result.code, result.stderr)
		}
	}
}

func TestMVPSevenGroups(t *testing.T) {
	binary := buildHomer(t)
	world := setupWorld(t, binary)

	t.Run("01-machine-A-assembly", func(t *testing.T) {
		adapters, ok := world.initReport["adapters"].([]any)
		if !ok || len(adapters) != 4 {
			t.Fatalf("init adapters = %#v, want four", world.initReport["adapters"])
		}
		ids := []string{}
		for _, item := range adapters {
			ids = append(ids, item.(map[string]any)["id"].(string))
		}
		if got := strings.Join(ids, ","); got != "pi,herdr,opencode,vscode" {
			t.Fatalf("init adapter order = %s", got)
		}
		if world.secretPushA["status"] != "pushed" || world.pushA["status"] != "pushed" {
			t.Fatalf("A push reports: secret=%#v push=%#v", world.secretPushA, world.pushA)
		}
		if world.secretPushA["pushedToRemote"] != true || world.pushA["pushedToRemote"] != true {
			t.Fatalf("A pushes did not reach origin: secret=%#v push=%#v", world.secretPushA, world.pushA)
		}
		config, err := core.LoadConfig(core.GetHomerPaths(func(key string) string {
			if key == "HOMER_HOME" {
				return world.a.homerHome
			}
			return ""
		}))
		if err != nil {
			t.Fatalf("load A config: %v", err)
		}
		if len(config.Adapters) != 4 || config.Secrets == nil || config.Secrets.Files[secretName] != secretDest {
			t.Fatalf("A config missing adapters/secrets: %#v", config)
		}
		if _, err := os.Stat(filepath.Join(world.a.homerHome, "secrets", secretName+".age")); err != nil {
			t.Fatalf("secret vault missing: %v", err)
		}
		if len(storeEntries(t, world.a, world.a, config)) != 10 {
			t.Fatalf("store file count = %d, want 10", len(storeEntries(t, world.a, world.a, config)))
		}
		assertOriginHas(t, world, "homer.json")
		assertOriginHas(t, world, "secrets/"+secretName+".age")
		assertOriginHas(t, world, "store/pi/skills/agent-browser/SKILL.md")
		assertOriginDoesNotContain(t, world, secretFragment)
		assertOriginDoesNotContain(t, world, "AGE-SECRET-KEY-")
	})

	t.Run("02-machine-B-home-restore", func(t *testing.T) {
		if world.homeReport["ok"] != true || world.homeReport["status"] != "homed" || world.homeReport["cloned"] != true {
			t.Fatalf("home report = %#v", world.homeReport)
		}
		secrets := world.homeReport["secrets"].(map[string]any)
		if got := stringSlice(secrets["skipped"]); len(got) != 1 || got[0] != secretName {
			t.Fatalf("home secret skip = %#v", secrets)
		}
		firstContact := world.homeReport["firstContact"].(map[string]any)
		applied := firstContact["applied"].(map[string]any)
		if written := len(applied["written"].([]any)); written != 10 {
			t.Fatalf("home writes = %d, want 10", written)
		}
		config, err := core.LoadConfig(core.GetHomerPaths(func(key string) string {
			if key == "HOMER_HOME" {
				return world.a.homerHome
			}
			return ""
		}))
		if err != nil {
			t.Fatalf("load config for placement: %v", err)
		}
		assertToolPlacement(t, world, config)
	})

	t.Run("03-device-rotation", func(t *testing.T) {
		if world.keygenB["recipient"] == world.keygenA["recipient"] || world.secretPushB["status"] != "pushed" || world.secretPullB["status"] != "applied" {
			t.Fatalf("rotation reports: keygen=%#v push=%#v pull=%#v", world.keygenB, world.secretPushB, world.secretPullB)
		}
		result, err := os.ReadFile(filepath.Join(world.b.fakeHome, ".secrets", "a.env"))
		if err != nil {
			t.Fatalf("B secret missing: %v", err)
		}
		if string(result) != secretPlaintext {
			t.Fatalf("B secret mismatch: %q", result)
		}
		info, err := os.Stat(filepath.Join(world.b.fakeHome, ".secrets", "a.env"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("B secret permissions = %v, want 0600 (err=%v)", info.Mode().Perm(), err)
		}
		identityInfo, err := os.Stat(filepath.Join(world.b.homerHome, "keys", "age.txt"))
		if err != nil || identityInfo.Mode().Perm() != 0o600 {
			t.Fatalf("B identity permissions = %v, want 0600 (err=%v)", identityInfo.Mode().Perm(), err)
		}
		oldDevice := mustJSONHomer(t, world.binary, world.a, "secret", "pull", "--yes", "--json")
		if oldDevice["status"] != "applied" {
			t.Fatalf("A could not decrypt re-encrypted vault: %#v", oldDevice)
		}
		assertOriginDoesNotContain(t, world, secretFragment)
	})

	t.Run("04-required-placeholder-doctor", func(t *testing.T) {
		report := mustJSONHomer(t, world.binary, world.b, "doctor", "--json")
		if report["ok"] != true {
			t.Fatalf("doctor has fail: %#v", report)
		}
		required := findCheck(t, report, "required")
		if required["status"] != "warn" {
			t.Fatalf("required check = %#v", required)
		}
		details := stringSlice(required["details"])
		if len(details) != 1 || details[0] != "pi/models/models.json: apiKeys" {
			t.Fatalf("required details = %#v", details)
		}
		for _, item := range report["checks"].([]any) {
			if item.(map[string]any)["status"] == "fail" {
				t.Fatalf("doctor fail check: %#v", item)
			}
		}
	})

	t.Run("05-allowEscape-snapshot", func(t *testing.T) {
		errors := stringSlice(world.initReport["errors"])
		foundEscapeWarning := false
		for _, item := range errors {
			if strings.Contains(item, "pi") && strings.Contains(item, "逃逸") {
				foundEscapeWarning = true
			}
		}
		if !foundEscapeWarning {
			t.Fatalf("initial scan did not report denied escape: %#v", errors)
		}
		for _, rel := range []string{"store/pi/skills/agent-browser/SKILL.md", "store/pi/skills/agent-browser/sub/a.ts"} {
			content, err := os.ReadFile(filepath.Join(world.a.homerHome, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatalf("allowed escape file %s missing: %v", rel, err)
			}
			if len(content) == 0 {
				t.Fatalf("allowed escape file %s is empty", rel)
			}
		}
		status := mustJSONHomer(t, world.binary, world.b, "status", "--json")
		if len(status["errors"].([]any)) != 0 {
			t.Fatalf("B status errors after allowEscape = %#v", status)
		}
	})

	t.Run("06-install-sh-smoke", func(t *testing.T) {
		syntax := runProcess(t, repoRoot(), world.gitGlobal, nil, "sh", "-n", filepath.Join(repoRoot(), "install.sh"))
		if syntax.code != 0 {
			t.Fatalf("sh -n install.sh failed: %s", syntax.stderr)
		}
		prefix := t.TempDir()
		fakeHome := t.TempDir()
		env := []string{
			"HOME=" + fakeHome,
			"HOMER_INSTALL_PACKAGE=" + world.binary,
			"HOMER_INSTALL_PREFIX=" + prefix,
		}
		first := runProcess(t, repoRoot(), world.gitGlobal, env, "sh", filepath.Join(repoRoot(), "install.sh"))
		if first.code != 0 {
			t.Fatalf("local install failed: %s", first.stderr)
		}
		second := runProcess(t, repoRoot(), world.gitGlobal, env, "sh", filepath.Join(repoRoot(), "install.sh"))
		if second.code != 0 {
			t.Fatalf("idempotent local install failed: %s", second.stderr)
		}
		installed := filepath.Join(prefix, "homer")
		if _, err := os.Stat(installed); err != nil {
			t.Fatalf("installed homer missing: %v", err)
		}
		help := runProcess(t, repoRoot(), world.gitGlobal, []string{"HOME=" + fakeHome}, installed, "--help")
		if help.code != 0 || !strings.Contains(help.stdout, "homer") {
			t.Fatalf("installed --help failed: %#v", help)
		}
	})

	t.Run("07-isolated-read-only-smoke", func(t *testing.T) {
		root := t.TempDir()
		fakeHome := filepath.Join(root, "fake-home")
		for _, path := range []string{
			filepath.Join(fakeHome, ".pi", "agent"),
			filepath.Join(fakeHome, ".config", "herdr"),
			filepath.Join(fakeHome, ".config", "opencode"),
		} {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatalf("mkdir readonly fixture: %v", err)
			}
		}
		m := machine{name: "readonly", fakeHome: fakeHome, homerHome: filepath.Join(root, "homer"), gitGlobal: world.gitGlobal}
		init := mustJSONHomer(t, world.binary, m, "init", "--json")
		if len(init["adapters"].([]any)) != 4 {
			t.Fatalf("isolated init adapters = %#v", init["adapters"])
		}
		result := runHomer(t, world.binary, m, "doctor", "--offline", "--json")
		if result.code == 0 {
			t.Fatalf("doctor should report repo fail before git init")
		}
		if result.stderr != "" {
			t.Fatalf("doctor readonly stderr: %s", result.stderr)
		}
		report := map[string]any{}
		if err := json.Unmarshal([]byte(result.stdout), &report); err != nil {
			t.Fatalf("doctor readonly JSON: %v\n%s", err, result.stdout)
		}
		if len(report["checks"].([]any)) != 8 {
			t.Fatalf("doctor check count = %d", len(report["checks"].([]any)))
		}
		if age := findCheck(t, report, "age"); age["status"] != "ok" {
			t.Fatalf("unconfigured age check = %#v", age)
		}
	})
}
