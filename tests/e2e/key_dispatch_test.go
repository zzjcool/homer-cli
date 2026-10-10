package e2e

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	keyDispatchPassword    = "key-dispatch-password"
	keyDispatchDestination = "~/.pi/agent/models.json"

	keyDispatchInitialPlaintext = `{
  "dispatchMarker": "key-dispatch-initial-5da1",
  "models": {"openai": {"id": "gpt-initial"}}
}
`
	keyDispatchUpdatedPlaintext = `{
  "dispatchMarker": "key-dispatch-updated-0b8f",
  "models": {"openai": {"id": "gpt-updated"}}
}
`
	keyDispatchConflictPlaintext = `{
  "dispatchMarker": "key-dispatch-machine-b-local-39e4",
  "models": {"openai": {"id": "gpt-local"}}
}
`
	keyDispatchCenterSettings = "{\n  \"theme\": \"center-updated\",\n  \"keep\": 1\n}\n"
	keyDispatchLocalSettings  = "{\n  \"theme\": \"machine-b-local\",\n  \"keep\": 1\n}\n"
)

type keyDispatchWorld struct {
	binary     string
	serverHome string
	hubURL     string
	auth       map[string]string
	machineA   machine
	machineB   machine
	hubProc    *exec.Cmd
}

// TestKeyDispatchUnlocksOnTarget exercises the console dispatch through a
// real hub and two real WebSocket-connected agents. The target must receive
// both the encrypted keyring item and the plaintext file written by unlock.
func TestKeyDispatchUnlocksOnTarget(t *testing.T) {
	world := setupKeyDispatchWorld(t)

	collectCode, collectBody := collectKeyDispatch(t, world)
	requireKeyDispatchOK(t, world, "collect machine-a", collectCode, collectBody)

	dispatchCode, dispatchBody := pullKeyDispatch(t, world)
	requireKeyDispatchOK(t, world, "dispatch to machine-b", dispatchCode, dispatchBody)

	assertKeyDispatchManifest(t, world, dispatchBody)
	assertKeyDispatchFile(t, world, keyDispatchInitialPlaintext, dispatchBody)
}

// TestKeyDispatchConflictStillUnlocks verifies that a partial dispatch still
// opens the delivered keyring item when another file remains conflicted. The
// center's models.json is applied while the conflicting local settings.json
// stays untouched, and the response continues to report the conflict.
func TestKeyDispatchConflictStillUnlocks(t *testing.T) {
	world := setupKeyDispatchWorld(t)

	collectCode, collectBody := collectKeyDispatch(t, world)
	requireKeyDispatchOK(t, world, "initial collect machine-a", collectCode, collectBody)
	firstHead, err := os.ReadFile(filepath.Join(world.serverHome, "HEAD"))
	if err != nil {
		t.Fatalf("read center HEAD after initial collect: %v", err)
	}

	firstDispatchCode, firstDispatchBody := pullKeyDispatch(t, world)
	requireKeyDispatchOK(t, world, "initial dispatch to machine-b", firstDispatchCode, firstDispatchBody)
	assertKeyDispatchManifest(t, world, firstDispatchBody)
	assertKeyDispatchFile(t, world, keyDispatchInitialPlaintext, firstDispatchBody)

	// Move the center forward with both new ciphertext for the key-bound file
	// and a conflicting edit to a tracked pi settings file. models.json is
	// intentionally keyring-ignored by the config sync path, so its local edit
	// alone cannot create a pull conflict.
	modelsA := filepath.Join(piRoot(world.machineA), "models.json")
	writeFile(t, modelsA, keyDispatchUpdatedPlaintext)
	writeFile(t, filepath.Join(piRoot(world.machineA), "settings.json"), keyDispatchCenterSettings)
	encrypt := runHomer(t, world.binary, world.machineA,
		"key", "encrypt", "--id", "pi", "--file", "models", "--path", keyDispatchDestination,
		"--adapter", "pi", "--password", keyDispatchPassword, "--json")
	if encrypt.code != 0 {
		t.Fatalf("re-encrypt machine-a models.json failed: stdout=%s stderr=%s", encrypt.stdout, encrypt.stderr)
	}

	secondCollectCode, secondCollectBody := collectKeyDispatch(t, world)
	requireKeyDispatchOK(t, world, "second collect machine-a", secondCollectCode, secondCollectBody)
	secondHead, err := os.ReadFile(filepath.Join(world.serverHome, "HEAD"))
	if err != nil {
		t.Fatalf("read center HEAD after second collect: %v", err)
	}
	if strings.TrimSpace(string(secondHead)) == strings.TrimSpace(string(firstHead)) {
		t.Fatalf("second collect did not advance center HEAD: first=%q second=%q body=%s\nhub output: %s",
			firstHead, secondHead, secondCollectBody, procOutput[world.hubProc])
	}

	modelsB := filepath.Join(piRoot(world.machineB), "models.json")
	writeFile(t, modelsB, keyDispatchConflictPlaintext)
	settingsB := filepath.Join(piRoot(world.machineB), "settings.json")
	writeFile(t, settingsB, keyDispatchLocalSettings)

	secondDispatchCode, secondDispatchBody := pullKeyDispatch(t, world)
	status, _ := jsonPath(t, secondDispatchBody, "status").(string)
	ok, _ := jsonPath(t, secondDispatchBody, "ok").(bool)
	if secondDispatchCode != http.StatusUnprocessableEntity || ok || status != "conflicts-remain" {
		t.Fatalf("second dispatch should report conflicts-remain (HTTP 422, ok=false): HTTP %d body=%s\nhub output: %s",
			secondDispatchCode, secondDispatchBody, procOutput[world.hubProc])
	}
	assertKeyDispatchManifest(t, world, secondDispatchBody)
	assertKeyDispatchConflict(t, secondDispatchBody)
	assertKeyDispatchBlobApplied(t, world, secondDispatchBody)

	if strings.Contains(secondDispatchBody, "口令不正确") {
		t.Fatalf("second dispatch reported an incorrect password: %s\nhub output: %s",
			secondDispatchBody, procOutput[world.hubProc])
	}
	settingsAfter, err := os.ReadFile(settingsB)
	if err != nil || string(settingsAfter) != keyDispatchLocalSettings {
		t.Fatalf("machine-b settings.json = %q err=%v; conflict must keep B's local content; response=%s\nhub output: %s",
			settingsAfter, err, secondDispatchBody, procOutput[world.hubProc])
	}
	assertKeyDispatchFile(t, world, keyDispatchUpdatedPlaintext, secondDispatchBody)
	t.Logf("machine-b settings.json remains local (%d bytes)", len(settingsAfter))
	t.Logf("partial conflict: HTTP %d ok=%v status=%q; settings.json stayed local and the center's models.json was unlocked; response confirms the keyring item was applied",
		secondDispatchCode, ok, status)
}

func setupKeyDispatchWorld(t *testing.T) *keyDispatchWorld {
	t.Helper()
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer Key Dispatch E2E\n\temail = homer-key-dispatch@example.invalid\n")

	binary := binaryOf(t)
	machineA := makeMachine(t, root, "A", global, true)
	machineB := makeMachine(t, root, "B", global, false)
	// In these two tests HOMER_HOME is the conventional ~/.homer directory,
	// so machine.homerHome/keyring is also the keyring adapter's real root.
	machineA.homerHome = filepath.Join(machineA.fakeHome, ".homer")
	machineB.homerHome = filepath.Join(machineB.fakeHome, ".homer")
	if result := runHomer(t, binary, machineA, "init", "--json"); result.code != 0 {
		t.Fatalf("init machine-a failed: stdout=%s stderr=%s", result.stdout, result.stderr)
	}

	modelsA := filepath.Join(piRoot(machineA), "models.json")
	writeFile(t, modelsA, keyDispatchInitialPlaintext)
	if result := runHomer(t, binary, machineA,
		"key", "create", "--id", "pi", "--name", "pi", "--password", keyDispatchPassword, "--json"); result.code != 0 {
		t.Fatalf("create pi key failed: stdout=%s stderr=%s", result.stdout, result.stderr)
	}
	if result := runHomer(t, binary, machineA,
		"key", "encrypt", "--id", "pi", "--file", "models", "--path", keyDispatchDestination,
		"--adapter", "pi", "--password", keyDispatchPassword, "--json"); result.code != 0 {
		t.Fatalf("encrypt machine-a models.json failed: stdout=%s stderr=%s", result.stdout, result.stderr)
	}

	serverHome := filepath.Join(root, "server-home")
	if err := os.MkdirAll(serverHome, 0o755); err != nil {
		t.Fatalf("mkdir bare server home: %v", err)
	}
	token := "key-dispatch-e2e-token"
	hubPort := freePort(t)
	var hubProc *exec.Cmd
	// startHomer records procOutput from its cleanup. Register this earlier
	// so a failed test logs that captured output after the process is stopped.
	t.Cleanup(func() {
		if t.Failed() && hubProc != nil {
			t.Logf("hub output: %s", procOutput[hubProc])
		}
	})
	hubProc = startHomer(t, binary, machineA, global,
		"serve", "--addr", fmt.Sprintf("127.0.0.1:%d", hubPort), "--token", token, "--home", serverHome)
	t.Cleanup(func() { stopProc(t, hubProc) })

	hubURL := fmt.Sprintf("http://127.0.0.1:%d", hubPort)
	auth := map[string]string{"Authorization": "Bearer " + token}
	agentA := startHomer(t, binary, machineA, global,
		"agent", "--hub", hubURL, "--token", token, "--id", "machine-a", "--home", machineA.homerHome)
	t.Cleanup(func() { stopProc(t, agentA) })
	agentB := startHomer(t, binary, machineB, global,
		"agent", "--hub", hubURL, "--token", token, "--id", "machine-b", "--home", machineB.homerHome)
	t.Cleanup(func() { stopProc(t, agentB) })
	waitRegistered(t, hubURL, auth, hubProc, "machine-a")
	waitRegistered(t, hubURL, auth, hubProc, "machine-b")

	return &keyDispatchWorld{
		binary: binary, serverHome: serverHome,
		hubURL: hubURL, auth: auth, machineA: machineA, machineB: machineB,
		hubProc: hubProc,
	}
}

func collectKeyDispatch(t *testing.T, world *keyDispatchWorld) (int, string) {
	t.Helper()
	return apiPostJSON(t, world.hubURL+"/api/sync?direction=collect&agent=machine-a&confirm=true", world.auth,
		map[string]any{"adapters": []string{"pi", "keyring"}})
}

func pullKeyDispatch(t *testing.T, world *keyDispatchWorld) (int, string) {
	t.Helper()
	return apiPostJSON(t, world.hubURL+"/api/agents/machine-b/pull?confirm=true", world.auth,
		map[string]any{
			"adapters": []string{"pi", "keyring"},
			"unlocks":  []map[string]string{{"id": "pi", "password": keyDispatchPassword}},
		})
}

func requireKeyDispatchOK(t *testing.T, world *keyDispatchWorld, action string, code int, body string) {
	t.Helper()
	if code != http.StatusOK || jsonPath(t, body, "ok") != true {
		t.Fatalf("%s failed: HTTP %d body=%s\nhub output: %s", action, code, body, procOutput[world.hubProc])
	}
	t.Logf("%s response: HTTP %d body=%s", action, code, body)
}

func assertKeyDispatchManifest(t *testing.T, world *keyDispatchWorld, responseBody string) {
	t.Helper()
	manifest := filepath.Join(world.machineB.homerHome, "keyring", "items", "pi", "manifest.json")
	info, err := os.Stat(manifest)
	if err != nil {
		t.Fatalf("machine-b keyring manifest is missing (%s): %v; response=%s\nhub output: %s",
			manifest, err, responseBody, procOutput[world.hubProc])
	}
	t.Logf("machine-b keyring manifest exists: %s (%d bytes)", manifest, info.Size())
}

func assertKeyDispatchConflict(t *testing.T, responseBody string) {
	t.Helper()
	conflicts, ok := jsonPath(t, responseBody, "conflicts").([]any)
	if !ok {
		t.Fatalf("second dispatch has no conflicts list: %s", responseBody)
	}
	for _, item := range conflicts {
		conflict, ok := item.(map[string]any)
		if ok && conflict["adapterId"] == "pi" && conflict["relPath"] == "settings.json" {
			t.Logf("second dispatch reports pi/settings.json conflict (%v)", conflict["reason"])
			return
		}
	}
	t.Fatalf("second dispatch did not report the expected pi/settings.json conflict: %s", responseBody)
}

func assertKeyDispatchBlobApplied(t *testing.T, world *keyDispatchWorld, responseBody string) {
	t.Helper()
	written, ok := jsonPath(t, responseBody, "applied", "written").([]any)
	if !ok {
		t.Fatalf("second dispatch has no applied.written list: response=%s\nhub output: %s", responseBody, procOutput[world.hubProc])
	}
	for _, item := range written {
		entry, ok := item.(map[string]any)
		if ok && entry["adapterId"] == "keyring" && entry["relPath"] == "pi/files/models.age" {
			t.Logf("second dispatch applied updated ciphertext: keyring/pi/files/models.age")
			return
		}
	}
	t.Fatalf("second dispatch did not apply the changed keyring ciphertext: response=%s\nhub output: %s", responseBody, procOutput[world.hubProc])
}

func assertKeyDispatchFile(t *testing.T, world *keyDispatchWorld, expected, responseBody string) {
	t.Helper()
	modelsB := filepath.Join(piRoot(world.machineB), "models.json")
	actual, err := os.ReadFile(modelsB)
	if err != nil {
		t.Fatalf("read machine-b models.json (%s): %v; response=%s\nhub output: %s",
			modelsB, err, responseBody, procOutput[world.hubProc])
	}
	if string(actual) != expected {
		t.Fatalf("machine-b models.json = %q, want plaintext %q; response=%s\nhub output: %s",
			actual, expected, responseBody, procOutput[world.hubProc])
	}
	t.Logf("machine-b models.json matches expected plaintext (%d bytes)", len(actual))
}
