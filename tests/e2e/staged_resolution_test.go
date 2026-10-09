package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStagedResolutionStory walks the staged-resolution flow end-to-end
// against a real hub and real agents (staged-resolution plan S3b-2):
//
//  1. record center → nothing applied yet, decisions persisted 0600
//  2. dispatch consumes → machine rewritten, entries cleared
//  3. record local → dispatch publishes the machine's content to the center
//  4. staleness → a newer generation invalidates a pending decision
//
// The legacy immediate path (record=true) is covered by the unit and UI
// suites; this test pins the staged phases themselves.
func TestStagedResolutionStory(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer E2E\n\temail = homer@example.invalid\n")
	binary := binaryOf(t)

	// Machine A: configured, provides the center's baseline.
	machineA := makeMachine(t, root, "A", global, true)
	if result := runHomer(t, binary, machineA, "init", "--json"); result.code != 0 {
		t.Fatalf("init A: %s%s", result.stdout, result.stderr)
	}
	serverHome := filepath.Join(root, "server-home")
	if err := os.MkdirAll(serverHome, 0o700); err != nil {
		t.Fatal(err)
	}
	token := "staged-resolution-token"
	hubPort := freePort(t)
	hubProc := startHomer(t, binary, machineA, global,
		"serve", "--addr", fmt.Sprintf("127.0.0.1:%d", hubPort), "--token", token, "--home", serverHome)
	defer stopProc(t, hubProc)
	hubURL := fmt.Sprintf("http://127.0.0.1:%d", hubPort)
	auth := map[string]string{"Authorization": "Bearer " + token}

	// The hub is a pure server; machine A needs its own agent to be
	// collectable.
	agentA := startHomer(t, binary, machineA, global,
		"agent", "--hub", hubURL, "--token", token, "--id", "agent-a", "--home", machineA.homerHome)
	defer stopProc(t, agentA)
	waitRegistered(t, hubURL, auth, hubProc, "agent-a")

	// Collect A so the center has a generation.
	collect := apiPost(t, hubURL+"/api/sync?direction=collect&agent=agent-a&confirm=true", auth)
	if !strings.Contains(collect, `"status":"synced"`) {
		t.Fatalf("collect A = %s", collect)
	}

	// Machine B: empty machine, boots from the center, then diverges so a
	// conflict exists.
	machineB := makeMachine(t, root, "B", global, false)
	agentB := startHomer(t, binary, machineB, global,
		"agent", "--hub", hubURL, "--token", token, "--id", "agent-b", "--home", machineB.homerHome)
	defer stopProc(t, agentB)
	waitRegistered(t, hubURL, auth, hubProc, "agent-b")

	dispatchAll := apiPost(t, hubURL+"/api/sync?direction=dispatch&confirm=true", auth)
	if !strings.Contains(dispatchAll, `"status":"synced"`) {
		t.Fatalf("bootstrap dispatch B = %s", dispatchAll)
	}
	// B now has A's settings.json; change it locally AND change the center
	// through A so the next dispatch reports a conflict on pi.
	bSettings := filepath.Join(piRoot(machineB), "settings.json")
	if err := os.WriteFile(bSettings, []byte("{\n  \"theme\": \"machine-b\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	collectB := apiPost(t, hubURL+"/api/sync?direction=collect&agent=agent-b&confirm=true", auth)
	if !strings.Contains(collectB, `"status"`) {
		t.Fatalf("collect B (baseline) = %s", collectB)
	}
	// Diverge BOTH sides after B's baseline: B changes its local file again
	// (so local != base) and A changes the center (so remote != base). That
	// three-way split is a real conflict, not just pending pulls.
	if err := os.WriteFile(bSettings, []byte("{\n  \"theme\": \"machine-b-diverged\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	aSettings := filepath.Join(piRoot(machineA), "settings.json")
	if err := os.WriteFile(aSettings, []byte("{\n  \"theme\": \"center-new\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if collect := apiPost(t, hubURL+"/api/sync?direction=collect&agent=agent-a&confirm=true", auth); !strings.Contains(collect, `"status"`) {
		t.Fatalf("collect A (diverge) = %s", collect)
	}

	// Wait until B reports the pi conflict.
	waitForCondition(t, 45*time.Second, func() bool {
		status := apiPost(t, hubURL+"/api/agents/agent-b/status", auth)
		if agentReportsPiConflict(status) {
			return true
		}
		t.Logf("waiting for pi conflict, status=%s", status)
		return false
	})

	resolutionsFile := filepath.Join(machineB.homerHome, "resolutions.json")

	// Phase 1: record a center decision for pi. Nothing is applied yet.
	recordStatus, record := apiPostJSON(t, hubURL+"/api/resolve/record?choice=center&agent=agent-b&confirm=true", auth, map[string]any{"adapters": []string{"pi"}})
	if recordStatus != 200 || !strings.Contains(record, `"status":"recorded"`) {
		t.Fatalf("record center = %s", record)
	}
	data, err := os.ReadFile(resolutionsFile)
	if err != nil {
		t.Fatalf("resolutions.json missing after record: %v", err)
	}
	if !strings.Contains(string(data), `"choice":"center"`) || !strings.Contains(string(data), `"adapter":"pi"`) {
		t.Fatalf("resolutions.json = %s", data)
	}
	if info, err := os.Stat(resolutionsFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("resolutions.json perm: %v %v", info, err)
	}
	// The machine is unchanged: the conflict is still there.
	status := apiPost(t, hubURL+"/api/agents/agent-b/status", auth)
	if !strings.Contains(status, `"conflicts":1`) {
		t.Fatalf("conflict must survive a pure record: %s", status)
	}

	// Phase 2: a scoped dispatch consumes the decision.
	dispatchBStatus, dispatchB := apiPostJSON(t, hubURL+"/api/agents/agent-b/pull?confirm=true", auth, map[string]any{"adapters": []string{"pi"}})
	if dispatchBStatus != 200 || !strings.Contains(dispatchB, `"ok":true`) {
		t.Fatalf("dispatch B consuming decision = %s", dispatchB)
	}
	content, err := os.ReadFile(bSettings)
	if err != nil || !strings.Contains(string(content), "center-new") {
		t.Fatalf("B settings after center decision = %q err=%v", content, err)
	}
	// The consumed entry is gone.
	waitForCondition(t, 10*time.Second, func() bool {
		data, err := os.ReadFile(resolutionsFile)
		return err != nil || !strings.Contains(string(data), `"adapter":"pi"`)
	})

	// Phase 3: diverge again and record a LOCAL decision. The dispatch must
	// keep the machine's file and publish it to the center. Both sides move:
	// B rewrites locally and A moves the center, so the next status is a
	// genuine three-way conflict again.
	if err := os.WriteFile(bSettings, []byte("{\n  \"theme\": \"machine-b-local\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aSettings, []byte("{\n  \"theme\": \"center-moved\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if collect := apiPost(t, hubURL+"/api/sync?direction=collect&agent=agent-a&confirm=true", auth); !strings.Contains(collect, `"status"`) {
		t.Fatalf("collect A (phase 3) = %s", collect)
	}
	waitForCondition(t, 45*time.Second, func() bool {
		status := apiPost(t, hubURL+"/api/agents/agent-b/status", auth)
		return agentReportsPiConflict(status)
	})
	recordLocalStatus, recordLocal := apiPostJSON(t, hubURL+"/api/resolve/record?choice=local&agent=agent-b&confirm=true", auth, map[string]any{"adapters": []string{"pi"}})
	if recordLocalStatus != 200 || !strings.Contains(recordLocal, `"status":"recorded"`) {
		t.Fatalf("record local = %s", recordLocal)
	}
	dispatchLocalStatus, dispatchLocal := apiPostJSON(t, hubURL+"/api/agents/agent-b/pull?confirm=true", auth, map[string]any{"adapters": []string{"pi"}})
	if dispatchLocalStatus != 200 || !strings.Contains(dispatchLocal, `"ok":true`) {
		t.Fatalf("dispatch consuming local decision = %s", dispatchLocal)
	}
	content, err = os.ReadFile(bSettings)
	if err != nil || !strings.Contains(string(content), "machine-b-local") {
		t.Fatalf("local decision must keep the machine's file: %q err=%v", content, err)
	}
	// The center now carries the machine's content (closed loop). The hub
	// home's HEAD generation is the source of truth.
	head, err := os.ReadFile(filepath.Join(serverHome, "HEAD"))
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	centerFile := filepath.Join(serverHome, "generations", strings.TrimSpace(string(head)),
		"store", "pi", "settings", "settings.json")
	center, err := os.ReadFile(centerFile)
	if err != nil || !strings.Contains(string(center), "machine-b-local") {
		t.Fatalf("center generation must hold the machine's content: %q err=%v", center, err)
	}

	// Phase 4: staleness. Record a decision, then publish a new generation
	// from the other machine; the pending decision must not apply. B moves
	// locally too so a conflict exists to record against.
	if err := os.WriteFile(bSettings, []byte("{\n  \"theme\": \"machine-b-stale\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aSettings, []byte("{\n  \"theme\": \"center-even-newer\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if collect := apiPost(t, hubURL+"/api/sync?direction=collect&agent=agent-a&confirm=true", auth); !strings.Contains(collect, `"status"`) {
		t.Fatalf("collect A (phase 4) = %s", collect)
	}
	waitForCondition(t, 45*time.Second, func() bool {
		status := apiPost(t, hubURL+"/api/agents/agent-b/status", auth)
		return agentReportsPiConflict(status)
	})
	recordStaleStatus, recordStale := apiPostJSON(t, hubURL+"/api/resolve/record?choice=center&agent=agent-b&confirm=true", auth, map[string]any{"adapters": []string{"pi"}})
	if recordStaleStatus != 200 || !strings.Contains(recordStale, `"status":"recorded"`) {
		t.Fatalf("record (future stale) = %s", recordStale)
	}
	// Advance the center generation with a fresh collect from A. A's
	// content must CHANGE again, otherwise the collect is a no-drift
	// no-op and the generation (and the decision's freshness) stays.
	if err := os.WriteFile(aSettings, []byte("{\n  \"theme\": \"center-after-record\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if collect := apiPost(t, hubURL+"/api/sync?direction=collect&agent=agent-a&confirm=true", auth); !strings.Contains(collect, `"status"`) {
		t.Fatalf("collect A (advance generation) = %s", collect)
	}
	// The choices view must now mark the decision stale and keep the
	// adapter disabled.
	choices := apiGet(t, hubURL+"/api/sync/choices?direction=dispatch&agent=agent-b", auth, hubProc)
	if !strings.Contains(choices, `"stale":true`) {
		t.Fatalf("choices must mark the decision stale: resolution view = %s",
			strings.Join([]string{choices[strings.Index(choices, `"id":"pi"`):min(len(choices), strings.Index(choices, `"id":"pi"`)+1200)]}, ""))
	}
	// A dispatch does not apply the stale decision: the conflict stays.
	staleDispatchStatus, staleDispatch := apiPostJSON(t, hubURL+"/api/agents/agent-b/pull?confirm=true", auth, map[string]any{"adapters": []string{"pi"}})
	if staleDispatchStatus == 200 && strings.Contains(staleDispatch, `"ok":true`) {
		content, _ := os.ReadFile(bSettings)
		if strings.Contains(string(content), "center-even-newer") {
			t.Fatalf("stale decision was applied: %s", content)
		}
	}
	// The stale entry survives for a fresh decision.
	if data, err := os.ReadFile(resolutionsFile); err != nil || !strings.Contains(string(data), `"adapter":"pi"`) {
		t.Fatalf("stale entry must survive: %s err=%v", data, err)
	}
}

// agentReportsPiConflict parses an agent status report and reports whether
// the pi adapter currently has at least one conflict. The report nests the
// adapters under "report" (the agent task payload), not at the top level.
func agentReportsPiConflict(status string) bool {
	var parsed struct {
		Report struct {
			Adapters []struct {
				ID        string `json:"id"`
				Conflicts int    `json:"conflicts"`
			} `json:"adapters"`
		} `json:"report"`
	}
	if err := json.Unmarshal([]byte(status), &parsed); err != nil {
		return false
	}
	for _, adapter := range parsed.Report.Adapters {
		if adapter.ID == "pi" && adapter.Conflicts > 0 {
			return true
		}
	}
	return false
}

// waitForCondition polls testFn until it holds or the deadline expires.
func waitForCondition(t *testing.T, timeout time.Duration, testFn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if testFn() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// min avoids importing builtin min in older Go toolchains used by CI.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestStagedResolutionSurvivesAgentRestart re-starts the agent between the
// record and the dispatch: decisions are machine state, not session state.
func TestStagedResolutionSurvivesAgentRestart(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer E2E\n\temail = homer@example.invalid\n")
	binary := binaryOf(t)

	machineA := makeMachine(t, root, "A", global, true)
	if result := runHomer(t, binary, machineA, "init", "--json"); result.code != 0 {
		t.Fatalf("init A: %s%s", result.stdout, result.stderr)
	}
	serverHome := filepath.Join(root, "server-home")
	if err := os.MkdirAll(serverHome, 0o700); err != nil {
		t.Fatal(err)
	}
	token := "staged-restart-token"
	hubPort := freePort(t)
	hubProc := startHomer(t, binary, machineA, global,
		"serve", "--addr", fmt.Sprintf("127.0.0.1:%d", hubPort), "--token", token, "--home", serverHome)
	defer stopProc(t, hubProc)
	hubURL := fmt.Sprintf("http://127.0.0.1:%d", hubPort)
	auth := map[string]string{"Authorization": "Bearer " + token}
	agentA := startHomer(t, binary, machineA, global,
		"agent", "--hub", hubURL, "--token", token, "--id", "agent-a", "--home", machineA.homerHome)
	defer stopProc(t, agentA)
	waitRegistered(t, hubURL, auth, hubProc, "agent-a")
	if collect := apiPost(t, hubURL+"/api/sync?direction=collect&agent=agent-a&confirm=true", auth); !strings.Contains(collect, `"status"`) {
		t.Fatalf("collect A = %s", collect)
	}

	machineB := makeMachine(t, root, "B", global, false)
	agentB := startHomer(t, binary, machineB, global,
		"agent", "--hub", hubURL, "--token", token, "--id", "agent-b", "--home", machineB.homerHome)
	waitRegistered(t, hubURL, auth, hubProc, "agent-b")
	if dispatch := apiPost(t, hubURL+"/api/sync?direction=dispatch&confirm=true", auth); !strings.Contains(dispatch, `"status"`) {
		t.Fatalf("bootstrap dispatch = %s", dispatch)
	}
	bSettings := filepath.Join(piRoot(machineB), "settings.json")
	if err := os.WriteFile(bSettings, []byte("{\n  \"theme\": \"b-local\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if collect := apiPost(t, hubURL+"/api/sync?direction=collect&agent=agent-b&confirm=true", auth); !strings.Contains(collect, `"status"`) {
		t.Fatalf("collect B = %s", collect)
	}
	// B diverges again after its baseline so the A-side change creates a
	// genuine three-way conflict.
	if err := os.WriteFile(bSettings, []byte("{\n  \"theme\": \"b-diverged\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	aSettings := filepath.Join(piRoot(machineA), "settings.json")
	if err := os.WriteFile(aSettings, []byte("{\n  \"theme\": \"a-new\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if collect := apiPost(t, hubURL+"/api/sync?direction=collect&agent=agent-a&confirm=true", auth); !strings.Contains(collect, `"status"`) {
		t.Fatalf("collect A = %s", collect)
	}
	waitForCondition(t, 45*time.Second, func() bool {
		status := apiPost(t, hubURL+"/api/agents/agent-b/status", auth)
		return agentReportsPiConflict(status)
	})

	recordStatus, record := apiPostJSON(t, hubURL+"/api/resolve/record?choice=center&agent=agent-b&confirm=true", auth, map[string]any{"adapters": []string{"pi"}})
	if recordStatus != 200 || !strings.Contains(record, `"status":"recorded"`) {
		t.Fatalf("record = %s", record)
	}

	// Restart the agent: stop, start again, wait for it to be back.
	stopProc(t, agentB)
	time.Sleep(500 * time.Millisecond)
	agentB2 := startHomer(t, binary, machineB, global,
		"agent", "--hub", hubURL, "--token", token, "--id", "agent-b", "--home", machineB.homerHome)
	defer stopProc(t, agentB2)
	waitRegistered(t, hubURL, auth, hubProc, "agent-b")

	dispatchStatus, dispatch := apiPostJSON(t, hubURL+"/api/agents/agent-b/pull?confirm=true", auth, map[string]any{"adapters": []string{"pi"}})
	if dispatchStatus != 200 || !strings.Contains(dispatch, `"ok":true`) {
		t.Fatalf("dispatch after restart = %s", dispatch)
	}
	content, err := os.ReadFile(bSettings)
	if err != nil || !strings.Contains(string(content), "a-new") {
		t.Fatalf("decision must survive the agent restart: %q err=%v", content, err)
	}
}
