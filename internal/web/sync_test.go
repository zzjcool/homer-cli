package web

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gens"
	"github.com/zzjcool/homer-cli/internal/gitx"
	"github.com/zzjcool/homer-cli/internal/keyring"
)

// Planner-frozen test names (MVP step 2). The sync API collapses
// push + fan-out pull into one manual action with human sentences only.

func syncPost(t *testing.T, handler http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/sync?"+query, nil)
	request.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestCategoryDestination(t *testing.T) {
	root := "~/.pi/agent"
	files := core.CategoryConfig{Paths: []string{"./"}, Mode: core.SyncModeMirror}
	if got := categoryDestination(root, files, "notes/todo.md"); got != "~/.pi/agent/notes/todo.md" {
		t.Fatalf("catch-all destination = %q", got)
	}
	skills := core.CategoryConfig{Paths: []string{"skills/"}, Mode: core.SyncModeMirror}
	if got := categoryDestination(root, skills, "foo/SKILL.md"); got != "~/.pi/agent/skills/foo/SKILL.md" {
		t.Fatalf("directory destination = %q", got)
	}
	settings := core.CategoryConfig{Paths: []string{"settings.json", "mcp.json"}, Mode: core.SyncModeMerge}
	if got := categoryDestination(root, settings, "mcp.json"); got != "~/.pi/agent/mcp.json" {
		t.Fatalf("file destination = %q", got)
	}
}

func TestStorageListsCredentialRules(t *testing.T) {
	// The keyring lives under the user's home, not HOMER_HOME. Point HOME at
	// an empty directory so a key the developer really created cannot leak in.
	t.Setenv("HOME", t.TempDir())
	fixture := makeFixture(t, "base\n", "local\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/storage", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("storage = %d %s", recorder.Code, recorder.Body)
	}
	var payload struct {
		Secrets []struct {
			Adapter     string `json:"adapter"`
			Name        string `json:"name"`
			Destination string `json:"destination"`
			Encrypted   bool   `json:"encrypted"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Secrets) != 3 || payload.Secrets[0].Name != "auth.json" || payload.Secrets[0].Encrypted || payload.Secrets[0].Destination != "~/.pi/agent/auth.json" {
		t.Fatalf("secrets = %+v", payload.Secrets)
	}
	if payload.Secrets[1].Name != "mcp-auth.json" || payload.Secrets[1].Adapter != "pi" {
		t.Fatalf("secrets = %+v", payload.Secrets)
	}
	if payload.Secrets[2].Adapter != "opencode" || payload.Secrets[2].Destination != "~/.local/share/opencode/auth.json" {
		t.Fatalf("secrets = %+v", payload.Secrets)
	}
}

func TestSyncRequiresConfirm(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	setGitIdentity(t, fixture.home)
	source := &sourceStub{}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := syncPost(t, server.Handler(), "direction=to-others")
	if response.Code != http.StatusConflict {
		t.Fatalf("sync without confirm = %d body=%s", response.Code, response.Body)
	}
	if !strings.Contains(response.Body.String(), `"aborted"`) {
		t.Fatalf("body = %s", response.Body)
	}
	if gitx.HeadCommit(fixture.home) != "" {
		t.Fatal("sync without confirm must not commit anything")
	}
	if len(source.pullValues) != 0 {
		t.Fatal("sync without confirm must not pull agents")
	}
}

func TestSyncToOthersPushesThenPullsOnlineAgents(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	setGitIdentity(t, fixture.home)
	source := &sourceStub{
		list: []AgentInfo{
			{AgentID: "online-1", Hostname: "box-online"},
			{AgentID: "offline-1", Hostname: "box-offline", Stale: true},
		},
		pullRaw: json.RawMessage(`{"ok":true}`),
	}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := syncPost(t, server.Handler(), "direction=to-others&confirm=true")
	if response.Code != http.StatusOK {
		t.Fatalf("sync = %d body=%s", response.Code, response.Body)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"status":"synced"`) || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("body = %s", body)
	}
	if len(source.pullValues) != 1 || !source.pullValues[0] {
		t.Fatalf("AgentPull confirm values = %v (want exactly one true)", source.pullValues)
	}
	if len(source.pulledAgent) != 1 || source.pulledAgent[0] != "online-1" {
		t.Fatalf("pulled agents = %v (stale machine must be skipped)", source.pulledAgent)
	}
	if len(source.pushValues) != 0 {
		t.Fatalf("AgentPush must not be called by sync, values = %v", source.pushValues)
	}
	if !strings.Contains(body, `"skipped":true`) {
		t.Fatalf("stale agent must be reported skipped: %s", body)
	}
}

func TestSyncToOthersDoesNotPullWhenPushRejected(t *testing.T) {
	fixture := makeFixture(t, "base\n", "sk-ant-abcdefghijklmnopqrstuvwxyz1234567890\n")
	setGitIdentity(t, fixture.home)
	source := &sourceStub{list: []AgentInfo{{AgentID: "online-1", Hostname: "box"}}}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := syncPost(t, server.Handler(), "direction=to-others&confirm=true")
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("secrets sync = %d body=%s", response.Code, response.Body)
	}
	body := response.Body.String()
	if !strings.Contains(body, "secrets-rejected") {
		t.Fatalf("body = %s", body)
	}
	if !strings.Contains(body, "检测到疑似密钥，已停止") {
		t.Fatalf("human sentence missing: %s", body)
	}
	if len(source.pullValues) != 0 {
		t.Fatal("agents must not be pulled when the push is rejected")
	}
	for _, gitWord := range []string{"commit", "merge", "git "} {
		if strings.Contains(body, gitWord) {
			t.Fatalf("git word %q leaked into sync response: %s", gitWord, body)
		}
	}
}

func TestSyncFromCenterDoesNotFanout(t *testing.T) {
	// No-git center: the hub's current generation carries the remote
	// content (fixture tool "base", store "base").
	fixture := makeFixture(t, "base\n", "base\n")
	if _, err := gens.New(fixture.home).Publish(map[string]map[string]string{
		"pi": {"settings/settings.json": "remote\n"},
	}, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	source := &sourceStub{list: []AgentInfo{{AgentID: "online-1", Hostname: "box"}}}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := syncPost(t, server.Handler(), "direction=from-center&confirm=true")
	if response.Code != http.StatusOK {
		t.Fatalf("from-center = %d body=%s", response.Code, response.Body)
	}
	tool, err := os.ReadFile(filepath.Join(fixture.tool, "settings.json"))
	if err != nil || string(tool) != "remote\n" {
		t.Fatalf("tool file = %q err=%v (want remote content applied)", tool, err)
	}
	if len(source.pullValues) != 0 {
		t.Fatal("from-center must not fan out to agents")
	}
}

func TestSyncRejectsBadDirection(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	for _, query := range []string{"", "direction=sideways&confirm=true", "confirm=true"} {
		response := syncPost(t, server.Handler(), query)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("query %q = %d body=%s", query, response.Code, response.Body)
		}
		if !strings.Contains(response.Body.String(), "direction 必须是 collect、dispatch、to-others 或 from-center") {
			t.Fatalf("query %q body = %s", query, response.Body)
		}
	}
}

// The no-git data plane's transfer protocol: agents upload snapshots to
// the hub and pull the hub's current snapshot over authenticated HTTP.
func TestSnapshotRoundTrip(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	handler := server.Handler()

	// Upload a snapshot (agent push): two files across one adapter.
	upload := request(t, handler, http.MethodPost, "/api/snapshot",
		`{"homerJson":"{\"version\":1}","store":{"pi":{"settings.json":"one\n","agents/designer.md":"hi\n"}}}`)
	if upload.Code != http.StatusOK {
		t.Fatalf("upload = %d body=%s", upload.Code, upload.Body)
	}
	var uploadBody struct {
		Generation int `json:"generation"`
	}
	if err := json.Unmarshal(upload.Body.Bytes(), &uploadBody); err != nil || uploadBody.Generation < 1 {
		t.Fatalf("upload body = %s (%v)", upload.Body, err)
	}

	// Download the current snapshot (agent pull): same content back.
	download := request(t, handler, http.MethodGet, "/api/snapshot", "")
	if download.Code != http.StatusOK {
		t.Fatalf("download = %d body=%s", download.Code, download.Body)
	}
	var snapshot struct {
		Generation int                          `json:"generation"`
		HomerJSON  string                       `json:"homerJson"`
		Store      map[string]map[string]string `json:"store"`
	}
	if err := json.Unmarshal(download.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("download parse: %v %s", err, download.Body)
	}
	if snapshot.Generation != uploadBody.Generation {
		t.Fatalf("generation = %d, uploaded %d", snapshot.Generation, uploadBody.Generation)
	}
	if snapshot.Store["pi"]["settings.json"] != "one\n" {
		t.Fatalf("store = %v", snapshot.Store)
	}
}

// 4c: the hub's own sync publishes locally (gens) — no git repository is
// ever needed inside the hub home, and no HTTP self-loop either.
func TestSyncToOthersPublishesGenerationLocally(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	response := syncPost(t, server.Handler(), "direction=to-others&confirm=true")
	if response.Code != http.StatusOK {
		t.Fatalf("sync = %d body=%s", response.Code, response.Body)
	}
	head, ok := gens.New(fixture.home).Read()
	if !ok || head.Generation < 1 {
		t.Fatalf("hub did not publish a generation: ok=%v head=%#v", ok, head)
	}
	tool, err := os.ReadFile(filepath.Join(head.StoreDir, "pi", "settings", "settings.json"))
	if err != nil || string(tool) != "local\n" {
		t.Fatalf("generation store = %q err=%v (want the pushed content)", tool, err)
	}
}

// An uninitialized hub home carries no store baseline; sync to-others
// bootstraps it (first contact semantics: everything is new).
func TestSyncToOthersBootstrapsEmptyHome(t *testing.T) {
	fixture := makeFixtureAtHome(t, filepath.Join(t.TempDir(), "hub"), "base\n")
	setGitIdentity(t, fixture.home)
	source := &sourceStub{pullRaw: []byte(`{"ok":true}`)}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := syncPost(t, server.Handler(), "direction=to-others&confirm=true")
	if response.Code != http.StatusOK {
		t.Fatalf("bootstrap sync = %d body=%s", response.Code, response.Body)
	}
	head, ok := gens.New(fixture.home).Read()
	if !ok || head.Generation < 1 {
		t.Fatalf("bootstrap generation = %#v ok=%v", head, ok)
	}
}

// ---------- pure-server console (server ≠ any machine) ----------

// The console's hero must speak about the SERVER's storage, not about a
// machine's workspace: it reports the current generation and the fleet's
// drift relative to that storage.
func TestConsoleStorageView(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	if _, err := gens.New(home).Publish(map[string]map[string]string{
		"pi": {"settings/settings.json": "remote\n"},
	}, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	server := newWebServer(t, fixture, "test-token", nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/console", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("console = %d body=%s", recorder.Code, recorder.Body)
	}
	var payload struct {
		OK         bool `json:"ok"`
		Generation int  `json:"generation"`
		Published  bool `json:"published"`
		Machines   []struct {
			AgentID  string `json:"agentId"`
			Hostname string `json:"hostname"`
			Stale    bool   `json:"stale"`
			Drift    struct {
				Push      int `json:"push"`
				Pull      int `json:"pull"`
				Conflicts int `json:"conflicts"`
			} `json:"drift"`
		} `json:"machines"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.OK || !payload.Published || payload.Generation != 1 {
		t.Fatalf("storage view = ok:%v published:%v generation:%d", payload.OK, payload.Published, payload.Generation)
	}
}

// An empty server (no generation ever published) must answer with
// published:false and an empty machine list — the UI's first-run state.
func TestConsoleStorageViewEmpty(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/console", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("console = %d body=%s", recorder.Code, recorder.Body)
	}
	var payload struct {
		OK         bool `json:"ok"`
		Generation int  `json:"generation"`
		Published  bool `json:"published"`
		Machines   []struct {
			AgentID string `json:"agentId"`
		} `json:"machines"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.OK || payload.Published || payload.Generation != 0 || len(payload.Machines) != 0 {
		t.Fatalf("empty storage view = %#v", payload)
	}
}

// ---------- collect / dispatch (pure-server sync semantics) ----------

// collectFromMachine: the hub asks one machine to push its content into
// the storage. The hub itself runs NO local push.
func TestSyncCollectFromMachine(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	source := &sourceStub{
		list:    []AgentInfo{{AgentID: "box-a", Hostname: "box-a"}},
		pushRaw: json.RawMessage(`{"ok":true,"status":"synced"}`),
	}
	// The machine's own executor uploads into the hub storage (the real
	// agent posts /api/snapshot; the stub publishes directly).
	source.onPush = func() {
		if _, err := gens.New(home).Publish(map[string]map[string]string{
			"pi": {"settings/settings.json": "machine content\n"},
		}, []byte("{}")); err != nil {
			t.Fatal(err)
		}
	}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := syncPost(t, server.Handler(), "direction=collect&agent=box-a&confirm=true")
	if response.Code != http.StatusOK {
		t.Fatalf("collect = %d body=%s", response.Code, response.Body)
	}
	if len(source.pushValues) != 1 {
		t.Fatal("collect must delegate the push to the machine (hub runs no local push)")
	}
	// The storage must now hold a generation the machine uploaded.
	if head, ok := gens.New(home).Read(); !ok || head.Generation < 1 {
		t.Fatalf("collect published no generation: %#v", head)
	}
}

func TestSyncCollectSecretsAskBeforeWriting(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	source := &sourceStub{
		list:    []AgentInfo{{AgentID: "box-a", Hostname: "box-a"}},
		pushRaw: json.RawMessage(`{"ok":false,"status":"secrets-rejected","secrets":[{"path":"pi/models/models.json","patternId":"generic-secret-assignment"}]}`),
	}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := request(t, server.Handler(), http.MethodPost, "/api/sync?direction=collect&agent=box-a&confirm=true", `{"adapters":["pi"]}`)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("collect = %d body=%s", response.Code, response.Body)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"status":"secrets-rejected"`) || !strings.Contains(body, "pi/models/models.json") || !strings.Contains(body, "仍然写入") {
		t.Fatalf("body = %s", body)
	}
	confirmed := request(t, server.Handler(), http.MethodPost, "/api/sync?direction=collect&agent=box-a&confirm=true", `{"adapters":["pi"],"allowSecrets":true}`)
	if confirmed.Code != http.StatusUnprocessableEntity {
		t.Fatalf("confirmed collect = %d body=%s", confirmed.Code, confirmed.Body)
	}
	if len(source.pushScopes) != 2 || source.pushScopes[0].AllowSecrets || !source.pushScopes[1].AllowSecrets {
		t.Fatalf("scopes = %#v", source.pushScopes)
	}
}

// dispatchToMachines: the hub asks every ONLINE machine to pull from the
// storage. Offline machines are skipped, not failed.
func TestSyncDispatchSkipsOffline(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	if _, err := gens.New(home).Publish(map[string]map[string]string{
		"pi": {"settings/settings.json": "remote\n"},
	}, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	source := &sourceStub{
		list: []AgentInfo{
			{AgentID: "on-1", Hostname: "on-1"},
			{AgentID: "off-1", Hostname: "off-1", Stale: true},
		},
		pullRaw: []byte(`{"ok":true,"status":"applied"}`),
	}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := syncPost(t, server.Handler(), "direction=dispatch&confirm=true")
	if response.Code != http.StatusOK {
		t.Fatalf("dispatch = %d body=%s", response.Code, response.Body)
	}
	if len(source.pullValues) != 1 {
		t.Fatalf("dispatch must pull online machines only, pulled %d", len(source.pullValues))
	}
}

// ---------- resolve on the machine (pure-server semantics) ----------

// Resolve must execute on the MACHINE that has the conflict — the server
// only relays the choice. With ?agent= given, the hub must NOT run a
// local merge.
func TestResolveDelegatesToMachine(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	// Center content exists; the machine reports its own conflict state.
	if _, err := gens.New(home).Publish(map[string]map[string]string{
		"pi": {"settings/settings.json": "center\n"},
	}, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	mergedOnAgent := false
	source := &sourceStub{
		list:    []AgentInfo{{AgentID: "box-c", Hostname: "box-c", Drift: &AgentDrift{Conflicts: 1}}},
		pushRaw: json.RawMessage(`{"ok":true,"status":"resolved"}`),
		pullRaw: json.RawMessage(`{"ok":true,"status":"applied"}`),
	}
	source.onPush = func() { mergedOnAgent = true }
	server := newWebServer(t, fixture, "test-token", source, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/resolve?choice=local&agent=box-c&confirm=true", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("resolve = %d body=%s", recorder.Code, recorder.Body)
	}
	if !mergedOnAgent {
		t.Fatal("resolve must delegate the merge to the machine (hub runs no local merge)")
	}
}

// "以这台机器为准" runs the machine's push. When the secret scanner stops
// it, the console must be told (with the files) so it can ask to write
// anyway; a vague "没有应用这次裁决" leaves the button looking dead.
func TestResolveLocalSurfacesSecretsRejected(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	source := &sourceStub{
		list:    []AgentInfo{{AgentID: "box-c", Hostname: "box-c", Drift: &AgentDrift{Conflicts: 1}}},
		pushRaw: json.RawMessage(`{"ok":false,"status":"secrets-rejected","secrets":[{"path":"pi/files/web-search.json"}]}`),
	}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := request(t, server.Handler(), http.MethodPost, "/api/resolve?choice=local&agent=box-c&confirm=true", `{"adapters":["pi"]}`)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("resolve = %d body=%s", response.Code, response.Body)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"status":"secrets-rejected"`) || !strings.Contains(body, "pi/files/web-search.json") {
		t.Fatalf("body = %s", body)
	}
}

// A real machine's snapshot is far beyond the console's 64KB JSON body
// limit — the snapshot endpoint must accept multi-MB payloads.
func TestSnapshotUploadAcceptsLargePayload(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	// ~1MB of content across many files.
	store := map[string]map[string]string{"pi": {}}
	big := strings.Repeat("x", 4096)
	for i := 0; i < 300; i++ {
		store["pi"][fmt.Sprintf("settings/file-%d.json", i)] = big
	}
	body, err := json.Marshal(snapshotPayload{Store: store, HomerJSON: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/snapshot", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("large snapshot upload = %d body=%s", recorder.Code, recorder.Body)
	}
}

// A one-time enrollment code must download the binary BEFORE redemption —
// that is its whole purpose on a fresh machine. (The 401 the user hit.)
func TestEnrollCodeDownloadsBinary(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	server, err := NewServer(ServeOptions{
		Addr:       "127.0.0.1:0",
		HomerHome:  fixture.home,
		Token:      "hub-token-value",
		Enrollment: testEnrollmentService{},
	})
	if err != nil {
		t.Fatal(err)
	}
	code, err := server.opts.Enrollment.Mint(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// serveSelfBinary streams the test process's own executable — any
	// readable binary satisfies the authorization assertion.
	request := httptest.NewRequest(http.MethodGet, "/dl/homer", nil)
	request.Header.Set("Authorization", "Bearer "+code)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("enroll-code download = %d body=%s", recorder.Code, recorder.Body)
	}
}

// testEnrollmentService is a minimal EnrollmentService for web tests
// (web must not import hub).
type testEnrollmentService struct {
	code string
}

func (t testEnrollmentService) Mint(ttl time.Duration) (string, error) {
	return "hr_test-code", nil
}
func (t testEnrollmentService) Revoke(agentID string) bool { return false }
func (t testEnrollmentService) ValidCode(code string) bool {
	return code == "hr_test-code"
}

// Storage view: the console must show what the server HOLDS, not just
// generation numbers — adapters/categories/files with sizes.
func TestStorageListing(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	// publish a generation through the upload endpoint
	store := map[string]map[string]string{
		"pi": {"settings/settings.json": "{\"theme\":\"dark\"}"},
	}
	body, _ := json.Marshal(snapshotPayload{Store: store, HomerJSON: "{}"})
	request := httptest.NewRequest(http.MethodPost, "/api/snapshot", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("upload = %d", recorder.Code)
	}

	// listing
	request = httptest.NewRequest(http.MethodGet, "/api/storage", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("storage listing = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var listing struct {
		Generation int `json:"generation"`
		Adapters   []struct {
			ID         string `json:"id"`
			Categories []struct {
				Name  string `json:"name"`
				Files []struct {
					Path string `json:"path"`
					Size int    `json:"size"`
				} `json:"files"`
			} `json:"categories"`
		} `json:"adapters"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Generation != 1 {
		t.Fatalf("generation = %d want 1", listing.Generation)
	}
	if len(listing.Adapters) != 1 || listing.Adapters[0].ID != "pi" {
		t.Fatalf("adapters = %+v", listing.Adapters)
	}
	if len(listing.Adapters[0].Categories) != 1 || len(listing.Adapters[0].Categories[0].Files) != 1 {
		t.Fatalf("categories/files = %+v", listing.Adapters[0].Categories)
	}
	if listing.Adapters[0].Categories[0].Files[0].Path != "settings/settings.json" {
		t.Fatalf("file path = %q", listing.Adapters[0].Categories[0].Files[0].Path)
	}
}

// Plugin lists are one level the user can read. The virtual file
// packages.manifest.txt must not appear beside the real files.
func TestStorageListingShowsPlugins(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	store := map[string]map[string]string{
		"pi": {
			"packages/packages.manifest.txt": "npm:pi-sop\nnpm:@zzjcool/pi-herdr-subagents\nnpm:pi-lens\n",
			"agents/advisor.md":              "---\nname: advisor\n",
		},
	}
	body, _ := json.Marshal(snapshotPayload{Store: store, HomerJSON: "{}"})
	request := httptest.NewRequest(http.MethodPost, "/api/snapshot", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("upload = %d", recorder.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/storage", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("listing = %d body=%s", recorder.Code, recorder.Body.String())
	}
	raw := recorder.Body.String()
	if strings.Contains(raw, "manifest.txt") {
		t.Fatalf("listing still names the virtual manifest file: %s", raw)
	}
	var listing struct {
		Adapters []struct {
			ID         string `json:"id"`
			Categories []struct {
				Name  string `json:"name"`
				Label string `json:"label"`
				Kind  string `json:"kind"`
				Files []struct {
					Path string `json:"path"`
				} `json:"files"`
			} `json:"categories"`
		} `json:"adapters"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	var plugins, agents []string
	for _, cat := range listing.Adapters[0].Categories {
		switch cat.Name {
		case "packages":
			if cat.Kind != "manifest" || cat.Label != "插件" {
				t.Fatalf("packages category = %+v", cat)
			}
			for _, file := range cat.Files {
				plugins = append(plugins, file.Path)
			}
		case "agents":
			for _, file := range cat.Files {
				agents = append(agents, file.Path)
			}
		}
	}
	wantPlugins := []string{"npm:@zzjcool/pi-herdr-subagents", "npm:pi-lens", "npm:pi-sop"}
	if !reflect.DeepEqual(plugins, wantPlugins) {
		t.Fatalf("plugins = %#v", plugins)
	}
	if !reflect.DeepEqual(agents, []string{"agents/advisor.md"}) {
		t.Fatalf("agents = %#v", agents)
	}
}

// Storage file content: click a file in the drawer → see what the
// server actually holds.
func TestStorageFileContent(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	store := map[string]map[string]string{
		"pi": {"settings/settings.json": "{\"theme\":\"dark\"}"},
	}
	body, _ := json.Marshal(snapshotPayload{Store: store, HomerJSON: "{}"})
	request := httptest.NewRequest(http.MethodPost, "/api/snapshot", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("upload = %d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/storage/file?adapter=pi&path=settings/settings.json", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("file content = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Content != "{\"theme\":\"dark\"}" {
		t.Fatalf("content = %q", payload.Content)
	}
}

// AppleDouble sidecars (._foo) from older generations are binary. The
// storage drawer must not list them or render their bytes as text.
func TestStorageListingSkipsAppleDouble(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	store := map[string]map[string]string{
		"pi": {
			"agents/advisor.md":             "---\nname: advisor\n",
			"agents/._advisor.md":           "\x00\x05Mac OS X",
			"extensions/anotify/index.ts":   "export {}\n",
			"extensions/anotify/._index.ts": "\x00junk",
		},
	}
	body, _ := json.Marshal(snapshotPayload{Store: store, HomerJSON: "{}"})
	request := httptest.NewRequest(http.MethodPost, "/api/snapshot", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("upload = %d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/storage", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("listing = %d body=%s", recorder.Code, recorder.Body.String())
	}
	listing := recorder.Body.String()
	if strings.Contains(listing, "._") {
		t.Fatalf("listing still contains AppleDouble names: %s", listing)
	}
	if !strings.Contains(listing, "agents/advisor.md") || !strings.Contains(listing, "extensions/anotify/index.ts") {
		t.Fatalf("listing dropped real files: %s", listing)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/storage/file?adapter=pi&path=agents/._advisor.md", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "Mac OS X") {
		t.Fatalf("binary preview = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var preview struct {
		Binary  bool   `json:"binary"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if !preview.Binary || preview.Content != "" {
		t.Fatalf("preview = %+v", preview)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/storage/file?adapter=pi&path=agents/advisor.md", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "name: advisor") {
		t.Fatalf("text preview = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

// A per-agent secret must authorize the binary download too — the
// machine's OWN upgrade path (`homer upgrade`) presents its enrollment
// secret, not the shared hub token. (User hit 401: keys/hub-token holds
// a burned hr_ code after enrollment.)
func TestAgentSecretDownloadsBinary(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	server, err := NewServer(ServeOptions{
		Addr:       "127.0.0.1:0",
		HomerHome:  fixture.home,
		Token:      "hub-token-value",
		Enrollment: testEnrollmentService{},
		AgentEndpointAuthorized: func(r *http.Request) bool {
			return strings.HasPrefix(r.Header.Get("Authorization"), "Bearer agent-secret-")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/dl/homer", nil)
	request.Header.Set("Authorization", "Bearer agent-secret-xyz")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("agent-secret download = %d body=%s", recorder.Code, recorder.Body)
	}
}

// /dl/homer.gz is the same executable, small enough to finish on a slow
// tunnel, and it honors Range so a dropped connection can resume.
func TestSelfBinaryGzipRoundTripAndRange(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	server, err := NewServer(ServeOptions{Addr: "127.0.0.1:0", HomerHome: fixture.home, Token: "hub-token-value"})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	rawReq := httptest.NewRequest(http.MethodGet, "/dl/homer", nil)
	rawReq.Header.Set("Authorization", "Bearer hub-token-value")
	rawRec := httptest.NewRecorder()
	handler.ServeHTTP(rawRec, rawReq)
	if rawRec.Code != http.StatusOK || len(rawRec.Body.Bytes()) < 1024 {
		t.Fatalf("raw = %d size=%d", rawRec.Code, rawRec.Body.Len())
	}
	gzReq := httptest.NewRequest(http.MethodGet, "/dl/homer.gz", nil)
	gzReq.Header.Set("Authorization", "Bearer hub-token-value")
	gzRec := httptest.NewRecorder()
	handler.ServeHTTP(gzRec, gzReq)
	if gzRec.Code != http.StatusOK {
		t.Fatalf("gzip = %d body=%s", gzRec.Code, gzRec.Body.String())
	}
	if gzRec.Body.Len() >= rawRec.Body.Len() {
		t.Fatalf("gzip %d is not smaller than raw %d", gzRec.Body.Len(), rawRec.Body.Len())
	}
	reader, err := gzip.NewReader(bytes.NewReader(gzRec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, rawRec.Body.Bytes()) {
		t.Fatal("gunzip did not restore the executable")
	}
	rangeReq := httptest.NewRequest(http.MethodGet, "/dl/homer.gz", nil)
	rangeReq.Header.Set("Authorization", "Bearer hub-token-value")
	rangeReq.Header.Set("Range", "bytes=0-3")
	rangeRec := httptest.NewRecorder()
	handler.ServeHTTP(rangeRec, rangeReq)
	if rangeRec.Code != http.StatusPartialContent || rangeRec.Body.Len() != 4 {
		t.Fatalf("range = %d size=%d", rangeRec.Code, rangeRec.Body.Len())
	}
	if !bytes.Equal(rangeRec.Body.Bytes(), gzRec.Body.Bytes()[:4]) {
		t.Fatal("range bytes are not the start of the gzip body")
	}
}

// "Sync to others" pushes this hub's content and then writes every online
// machine from the center. An adapter with a key bound to it cannot be
// delivered whole by that fan-out (it has no per-machine password step), so
// the fan-out is refused instead of stranding the key on every machine.
func TestSyncToOthersRefusesWhenAdapterHasBoundKey(t *testing.T) {
	// The keyring lives under the adapter root resolved from $HOME. Without
	// this the test would create a key in the developer's real ~/.homer.
	t.Setenv("HOME", t.TempDir())
	fixture := makeFixture(t, "base\n", "local\n")
	setGitIdentity(t, fixture.home)
	secretFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(secretFile, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := keyring.Apply(fixture.home, keyring.Command{Action: "create", ID: "pi", Name: "pi", Password: "long-password", WorkFactor: 14}); !r.OK {
		t.Fatalf("create = %#v", r)
	}
	if r := keyring.Apply(fixture.home, keyring.Command{Action: "encrypt", ID: "pi", Path: secretFile, Adapter: "pi", Password: "long-password", WorkFactor: 14}); !r.OK {
		t.Fatalf("encrypt = %#v", r)
	}
	source := &sourceStub{
		list:    []AgentInfo{{AgentID: "online-1", Hostname: "box-online"}},
		pullRaw: json.RawMessage(`{"ok":true}`),
	}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := syncPost(t, server.Handler(), "direction=to-others&confirm=true")
	if response.Code != http.StatusUnprocessableEntity || len(source.pullValues) != 0 {
		t.Fatalf("to-others = %d pulls=%d body=%s (want refused before any machine is written)", response.Code, len(source.pullValues), response.Body)
	}
	if !strings.Contains(response.Body.String(), "pi") || !strings.Contains(response.Body.String(), "密钥") {
		t.Fatalf("refusal should name the adapter and the key: %s", response.Body)
	}
}
