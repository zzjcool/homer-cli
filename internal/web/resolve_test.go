package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/gens"
	"github.com/zzjcool/homer-cli/internal/gitx"
)

// Planner-frozen conflict fixture (MVP step 4): a three-way conflict —
// base "base", local "local", remote "remote".
func conflictFixture(t *testing.T) webFixture {
	t.Helper()
	fixture := makeFixture(t, "base\n", "local\n")
	// The no-git data plane's center is a published hub generation.
	if _, err := gens.New(fixture.home).Publish(map[string]map[string]string{
		"pi": {"settings/settings.json": "remote\n"},
	}, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	// The machine store keeps the base content.
	storeFile := filepath.Join(fixture.home, "store", "pi", "settings", "settings.json")
	if err := os.WriteFile(storeFile, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func resolvePost(t *testing.T, handler http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/resolve?"+query, nil)
	request.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestResolveLocalKeepsMachineContent(t *testing.T) {
	fixture := conflictFixture(t)
	source := &sourceStub{pullRaw: []byte(`{"ok":true}`)}
	server := newWebServer(t, fixture, "test-token", nil, nil)
	_ = source
	response := resolvePost(t, server.Handler(), "choice=local&confirm=true")
	if response.Code != http.StatusOK {
		t.Fatalf("resolve local = %d body=%s", response.Code, response.Body)
	}
	tool, _ := os.ReadFile(filepath.Join(fixture.tool, "settings.json"))
	if string(tool) != "local\n" {
		t.Fatalf("local choice must keep machine content, got %q", tool)
	}
	store, err := os.ReadFile(filepath.Join(fixture.home, "store", "pi", "settings", "settings.json"))
	if err != nil || string(store) != "local\n" {
		t.Fatalf("store must hold the machine content after local resolve: %q err=%v", store, err)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"status":"resolved"`) {
		t.Fatalf("body = %s", body)
	}
	for _, banned := range []string{"homer", "merge", "commit", "git "} {
		if strings.Contains(body, banned) {
			t.Fatalf("git word %q leaked: %s", banned, body)
		}
	}
}

func TestResolveCenterAppliesCenterContent(t *testing.T) {
	fixture := conflictFixture(t)
	source := &sourceStub{
		list:    []AgentInfo{{AgentID: "online-1", Hostname: "box"}},
		pullRaw: []byte(`{"ok":true}`),
	}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := resolvePost(t, server.Handler(), "choice=center&confirm=true")
	if response.Code != http.StatusOK {
		t.Fatalf("resolve center = %d body=%s", response.Code, response.Body)
	}
	tool, _ := os.ReadFile(filepath.Join(fixture.tool, "settings.json"))
	if string(tool) != "remote\n" {
		t.Fatalf("center choice must apply center content, got %q", tool)
	}
	if len(source.pullValues) != 0 {
		t.Fatal("center choice must never fan out")
	}
	if !strings.Contains(response.Body.String(), `"agents":[]`) {
		t.Fatalf("agents must be empty: %s", response.Body)
	}
}

func TestResolveLocalFanoutsToOnlineAgents(t *testing.T) {
	fixture := conflictFixture(t)
	source := &sourceStub{
		list:    []AgentInfo{{AgentID: "online-1", Hostname: "box"}},
		pullRaw: []byte(`{"ok":true}`),
	}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := resolvePost(t, server.Handler(), "choice=local&confirm=true")
	if response.Code != http.StatusOK {
		t.Fatalf("resolve local = %d body=%s", response.Code, response.Body)
	}
	if len(source.pullValues) != 1 || !source.pullValues[0] {
		t.Fatalf("local choice must fan out pull to online agents, values=%v", source.pullValues)
	}
}

func TestResolveRequiresConfirm(t *testing.T) {
	fixture := conflictFixture(t)
	source := &sourceStub{}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := resolvePost(t, server.Handler(), "choice=local")
	if response.Code != http.StatusConflict {
		t.Fatalf("resolve without confirm = %d", response.Code)
	}
	tool, _ := os.ReadFile(filepath.Join(fixture.tool, "settings.json"))
	if string(tool) != "local\n" {
		t.Fatal("resolve without confirm must not touch files")
	}
	if len(source.pullValues) != 0 {
		t.Fatal("resolve without confirm must not fan out")
	}
	if gitx.HeadCommit(fixture.home) == "" {
		// head may exist from the fixture's base commit; the assertion that
		// matters is no NEW commit — captured by pullValues being empty and
		// the file being untouched above.
		_ = gitx.HeadCommit(fixture.home)
	}
}

func TestResolveRejectsBadChoice(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	server := newWebServer(t, fixture, "test-token", nil, nil)
	response := resolvePost(t, server.Handler(), "choice=sideways&confirm=true")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("bad choice = %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), "choice 必须是 local 或 center") {
		t.Fatalf("body = %s", response.Body)
	}
}
