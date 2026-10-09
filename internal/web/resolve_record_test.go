package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/gens"
	"github.com/zzjcool/homer-cli/internal/resolutions"
)

type resolutionRouteStub struct {
	sourceStub
	recordRaw json.RawMessage
	recordErr error
	records   []ResolveRecordRequest
	events    []string
}

func (s *resolutionRouteStub) AgentStatus(ctx context.Context, agentID string) (json.RawMessage, error) {
	s.events = append(s.events, "status")
	return s.sourceStub.AgentStatus(ctx, agentID)
}

func (s *resolutionRouteStub) AgentResolveRecord(_ context.Context, _ string, req ResolveRecordRequest) (json.RawMessage, error) {
	s.events = append(s.events, "record")
	req.Adapters = append([]string(nil), req.Adapters...)
	s.records = append(s.records, req)
	if s.recordErr != nil {
		return nil, s.recordErr
	}
	return append(json.RawMessage(nil), s.recordRaw...), nil
}

func (s *resolutionRouteStub) AgentPush(ctx context.Context, agentID string, confirm bool, scope SyncScope) (json.RawMessage, error) {
	s.events = append(s.events, "push")
	return s.sourceStub.AgentPush(ctx, agentID, confirm, scope)
}

func (s *resolutionRouteStub) AgentPull(ctx context.Context, agentID string, confirm bool, scope SyncScope) (json.RawMessage, error) {
	s.events = append(s.events, "pull")
	return s.sourceStub.AgentPull(ctx, agentID, confirm, scope)
}

func publishResolutionCenter(t *testing.T, fixture webFixture, adapters ...string) int {
	t.Helper()
	store := make(map[string]map[string]string, len(adapters))
	for _, adapter := range adapters {
		store[adapter] = map[string]string{"settings/settings.json": "center\n"}
	}
	generation, err := gens.New(fixture.home).Publish(store, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

func resolutionRecordRaw(generation int) json.RawMessage {
	return resolutionRecordRawForChoice(generation, resolutions.ChoiceLocal)
}

func resolutionRecordRawForChoice(generation int, choice string) json.RawMessage {
	entry := resolutions.Entry{
		Adapter: "pi", Choice: choice,
		RecordedAt: "2026-10-20T08:00:00Z", GenerationAtRecord: generation,
	}
	data, _ := json.Marshal(resolutions.Report{
		OK: true, Action: resolutions.ActionRecord,
		Entries: []resolutions.Entry{entry}, Recorded: []resolutions.Entry{entry},
		Pending: 1, Errors: []string{},
	})
	return data
}

func TestResolveRecordOnlyPersistsConflictedAdapters(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	generation := publishResolutionCenter(t, fixture, "pi", "herdr")
	stub := &resolutionRouteStub{
		sourceStub: sourceStub{statusRaw: []byte(`{"adapters":[{"id":"pi","conflicts":2},{"id":"herdr","push":1}],"errors":[]}`)},
		recordRaw:  resolutionRecordRaw(generation),
	}
	server := newWebServer(t, fixture, "test-token", stub, nil)
	response := request(t, server.Handler(), http.MethodPost,
		"/api/resolve/record?agent=box&choice=local&confirm=true", `{"adapters":["pi","herdr"]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("record = %d %s", response.Code, response.Body)
	}
	if len(stub.pushValues) != 0 || len(stub.pullValues) != 0 {
		t.Fatalf("record must not execute a write: pushes=%v pulls=%v", stub.pushValues, stub.pullValues)
	}
	if len(stub.records) != 1 {
		t.Fatalf("record calls = %+v", stub.records)
	}
	req := stub.records[0]
	if req.Action != resolutions.ActionRecord || req.Choice != resolutions.ChoiceLocal || req.CenterGeneration != generation || len(req.Adapters) != 1 || req.Adapters[0] != "pi" {
		t.Fatalf("record request = %+v", req)
	}
	var report resolveRecordReport
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Recorded) != 1 || report.Recorded[0].Adapter != "pi" || len(report.Skipped) != 1 || report.Skipped[0].Adapter != "herdr" {
		t.Fatalf("record response = %+v", report)
	}
}

func TestResolveRecordErrorPaths(t *testing.T) {
	t.Run("not confirmed", func(t *testing.T) {
		fixture := makeFixture(t, "base\n", "local\n")
		publishResolutionCenter(t, fixture, "pi")
		stub := &resolutionRouteStub{}
		server := newWebServer(t, fixture, "test-token", stub, nil)
		response := request(t, server.Handler(), http.MethodPost, "/api/resolve/record?agent=box&choice=center", `{"adapters":["pi"]}`)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"status":"aborted"`) || len(stub.records) != 0 {
			t.Fatalf("unconfirmed = %d %s", response.Code, response.Body)
		}
	})
	t.Run("bad choice", func(t *testing.T) {
		fixture := makeFixture(t, "base\n", "local\n")
		publishResolutionCenter(t, fixture, "pi")
		server := newWebServer(t, fixture, "test-token", &resolutionRouteStub{}, nil)
		response := request(t, server.Handler(), http.MethodPost, "/api/resolve/record?agent=box&choice=wrong&confirm=true", `{"adapters":["pi"]}`)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("bad choice = %d %s", response.Code, response.Body)
		}
	})
	t.Run("missing adapters", func(t *testing.T) {
		fixture := makeFixture(t, "base\n", "local\n")
		publishResolutionCenter(t, fixture, "pi")
		server := newWebServer(t, fixture, "test-token", &resolutionRouteStub{}, nil)
		response := request(t, server.Handler(), http.MethodPost, "/api/resolve/record?agent=box&choice=center&confirm=true", `{}`)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("missing adapters = %d %s", response.Code, response.Body)
		}
	})
	t.Run("no snapshot", func(t *testing.T) {
		fixture := makeFixture(t, "base\n", "local\n")
		server := newWebServer(t, fixture, "test-token", &resolutionRouteStub{}, nil)
		response := request(t, server.Handler(), http.MethodPost, "/api/resolve/record?agent=box&choice=center&confirm=true", `{"adapters":["pi"]}`)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"no-snapshot"`) {
			t.Fatalf("no snapshot = %d %s", response.Code, response.Body)
		}
	})
	t.Run("agents disabled", func(t *testing.T) {
		fixture := makeFixture(t, "base\n", "local\n")
		publishResolutionCenter(t, fixture, "pi")
		server := newWebServer(t, fixture, "test-token", &sourceStub{}, nil)
		response := request(t, server.Handler(), http.MethodPost, "/api/resolve/record?agent=box&choice=center&confirm=true", `{"adapters":["pi"]}`)
		if response.Code != http.StatusNotImplemented {
			t.Fatalf("missing source = %d %s", response.Code, response.Body)
		}
	})
	t.Run("outdated agent", func(t *testing.T) {
		fixture := makeFixture(t, "base\n", "local\n")
		publishResolutionCenter(t, fixture, "pi")
		stub := &resolutionRouteStub{
			sourceStub: sourceStub{statusRaw: []byte(`{"adapters":[{"id":"pi","conflicts":1}],"errors":[]}`)},
			recordErr:  &AgentError{Code: "agent-outdated", Status: http.StatusConflict, Err: errors.New("old agent")},
		}
		server := newWebServer(t, fixture, "test-token", stub, nil)
		response := request(t, server.Handler(), http.MethodPost, "/api/resolve/record?agent=box&choice=center&confirm=true", `{"adapters":["pi"]}`)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "这台机器的 homer 版本太旧") {
			t.Fatalf("outdated = %d %s", response.Code, response.Body)
		}
	})
}

func TestResolveRecordNothingToRecord(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	publishResolutionCenter(t, fixture, "pi")
	stub := &resolutionRouteStub{sourceStub: sourceStub{statusRaw: []byte(`{"adapters":[{"id":"pi","pull":1}],"errors":[]}`)}}
	server := newWebServer(t, fixture, "test-token", stub, nil)
	response := request(t, server.Handler(), http.MethodPost, "/api/resolve/record?agent=box&choice=center&confirm=true", `{"adapters":["pi"]}`)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), `"status":"nothing-to-record"`) || len(stub.records) != 0 {
		t.Fatalf("nothing to record = %d %s", response.Code, response.Body)
	}
}

func TestClientCannotForgeGeneration(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	generation := publishResolutionCenter(t, fixture, "pi")
	stub := &resolutionRouteStub{
		sourceStub: sourceStub{statusRaw: []byte(`{"adapters":[{"id":"pi","conflicts":1}],"errors":[]}`)},
		recordRaw:  resolutionRecordRaw(generation),
	}
	server := newWebServer(t, fixture, "test-token", stub, nil)
	response := request(t, server.Handler(), http.MethodPost,
		"/api/resolve/record?agent=box&choice=local&confirm=true", `{"adapters":["pi"],"centerGeneration":999}`)
	if response.Code != http.StatusOK || len(stub.records) != 1 || stub.records[0].CenterGeneration != generation {
		t.Fatalf("client-controlled generation = %d request=%+v body=%s", response.Code, stub.records, response.Body)
	}
}

func TestResolveWithRecordTrueRecordsThenRelaysWithClear(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	generation := publishResolutionCenter(t, fixture, "pi")
	stub := &resolutionRouteStub{
		recordRaw:  resolutionRecordRaw(generation),
		sourceStub: sourceStub{pullRaw: []byte(`{"ok":true,"status":"applied"}`)},
	}
	server := newWebServer(t, fixture, "test-token", stub, nil)
	response := request(t, server.Handler(), http.MethodPost,
		"/api/resolve?choice=center&agent=box&confirm=true&record=true", `{"adapters":["pi"]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("record then relay = %d %s", response.Code, response.Body)
	}
	if strings.Join(stub.events, ",") != "record,pull" || len(stub.records) != 1 || len(stub.pullScopes) != 1 {
		t.Fatalf("call order = %v, records=%d pulls=%d", stub.events, len(stub.records), len(stub.pullScopes))
	}
	scope := stub.pullScopes[0]
	if !scope.ClearResolutions || !scope.Explicit || len(scope.Adapters) != 1 || scope.Adapters[0] != "pi" {
		t.Fatalf("relay scope = %+v", scope)
	}
	var report resolveReport
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Recorded) != 1 || report.Recorded[0] != "pi" || len(report.Pending) != 0 {
		t.Fatalf("immediate resolution report = %+v", report)
	}
}

func TestResolveWithRecordLocalFanoutNeverConsumes(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	generation := publishResolutionCenter(t, fixture, "pi")
	stub := &resolutionRouteStub{
		sourceStub: sourceStub{
			list:    []AgentInfo{{AgentID: "box", Hostname: "box"}},
			pushRaw: []byte(`{"ok":true,"status":"pushed"}`),
			pullRaw: []byte(`{"ok":true,"status":"applied"}`),
		},
		recordRaw: resolutionRecordRaw(generation),
	}
	server := newWebServer(t, fixture, "test-token", stub, nil)
	response := request(t, server.Handler(), http.MethodPost,
		"/api/resolve?choice=local&agent=box&confirm=true&record=true", `{"adapters":["pi"]}`)
	if response.Code != http.StatusOK || len(stub.pushScopes) != 1 || len(stub.pullScopes) != 1 {
		t.Fatalf("local immediate resolve = %d pushScopes=%+v pullScopes=%+v body=%s", response.Code, stub.pushScopes, stub.pullScopes, response.Body)
	}
	if !stub.pushScopes[0].ClearResolutions {
		t.Fatalf("immediate local relay did not clear its own decision: %+v", stub.pushScopes[0])
	}
	if stub.pullScopes[0].ApplyResolutions || stub.pullScopes[0].ClearResolutions || stub.pullScopes[0].CenterGeneration != 0 {
		t.Fatalf("local fallback fanout consumed staged decision: %+v", stub.pullScopes[0])
	}
}

func TestResolveWithRecordFailureDoesNotRelay(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	publishResolutionCenter(t, fixture, "pi")
	stub := &resolutionRouteStub{
		recordRaw:  json.RawMessage(`{"ok":false,"action":"record","entries":[],"errors":["写入失败"]}`),
		sourceStub: sourceStub{pullRaw: []byte(`{"ok":true,"status":"applied"}`)},
	}
	server := newWebServer(t, fixture, "test-token", stub, nil)
	response := request(t, server.Handler(), http.MethodPost,
		"/api/resolve?choice=center&agent=box&confirm=true&record=true", `{"adapters":["pi"]}`)
	if response.Code != http.StatusUnprocessableEntity || len(stub.records) != 1 || len(stub.pullValues) != 0 {
		t.Fatalf("record failure = %d records=%d pulls=%d body=%s", response.Code, len(stub.records), len(stub.pullValues), response.Body)
	}
}

func TestResolveWithRecordRelayFailureKeepsPending(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	generation := publishResolutionCenter(t, fixture, "pi")
	stub := &resolutionRouteStub{
		recordRaw:  resolutionRecordRaw(generation),
		sourceStub: sourceStub{pullErr: &AgentError{Code: "agent-unreachable", Status: http.StatusBadGateway, Err: errors.New("offline during pull")}},
	}
	server := newWebServer(t, fixture, "test-token", stub, nil)
	response := request(t, server.Handler(), http.MethodPost,
		"/api/resolve?choice=center&agent=box&confirm=true&record=true", `{"adapters":["pi"]}`)
	if response.Code != http.StatusBadGateway || strings.Join(stub.events, ",") != "record,pull" {
		t.Fatalf("relay failure = %d events=%v body=%s", response.Code, stub.events, response.Body)
	}
	var report resolveReport
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.OK || len(report.Recorded) != 1 || len(report.Pending) != 1 || report.Pending[0] != "pi" {
		t.Fatalf("failed relay did not preserve pending record: %+v", report)
	}
}

func TestResolveWithoutRecordUnchanged(t *testing.T) {
	fixture := makeFixture(t, "base\n", "local\n")
	publishResolutionCenter(t, fixture, "pi")
	stub := &resolutionRouteStub{sourceStub: sourceStub{pullRaw: []byte(`{"ok":true,"status":"applied"}`)}}
	server := newWebServer(t, fixture, "test-token", stub, nil)
	response := request(t, server.Handler(), http.MethodPost,
		"/api/resolve?choice=center&agent=box&confirm=true", `{"adapters":["pi"]}`)
	if response.Code != http.StatusOK || len(stub.records) != 0 || len(stub.pullScopes) != 1 {
		t.Fatalf("legacy resolve = %d records=%d pulls=%d body=%s", response.Code, len(stub.records), len(stub.pullScopes), response.Body)
	}
	if stub.pullScopes[0].ApplyResolutions || stub.pullScopes[0].ClearResolutions || stub.pullScopes[0].CenterGeneration != 0 {
		t.Fatalf("legacy resolve unexpectedly applied staged options: %+v", stub.pullScopes[0])
	}
}

func TestResolveRecordValidationOrder(t *testing.T) {
	t.Run("center adapter validation precedes record", func(t *testing.T) {
		fixture := makeFixture(t, "base\n", "local\n")
		publishResolutionCenter(t, fixture, "pi")
		stub := &resolutionRouteStub{sourceStub: sourceStub{statusRaw: []byte(`{"adapters":[{"id":"pi","conflicts":1}],"errors":[]}`)}}
		server := newWebServer(t, fixture, "test-token", stub, nil)
		response := request(t, server.Handler(), http.MethodPost,
			"/api/resolve?choice=center&agent=box&confirm=true&record=true", `{"adapters":["missing"]}`)
		if response.Code != http.StatusBadRequest || len(stub.records) != 0 || len(stub.pullValues) != 0 {
			t.Fatalf("invalid center adapter = %d records=%d pulls=%d body=%s", response.Code, len(stub.records), len(stub.pullValues), response.Body)
		}
	})
	t.Run("unlock validation precedes record", func(t *testing.T) {
		server, keySource := dispatchFixtureWithBoundKey(t)
		stub := &resolutionWithKeySource{recordingKeySource: keySource}
		server.opts.Agents = stub
		response := request(t, server.Handler(), http.MethodPost,
			"/api/resolve?choice=center&agent=box&confirm=true&record=true", `{"adapters":["pi","keyring"]}`)
		if response.Code != http.StatusUnprocessableEntity || strings.Contains(response.Body.String(), "agent-outdated") || len(stub.records) != 0 || len(stub.pullValues) != 0 {
			t.Fatalf("unlock check did not precede record: %d records=%d pulls=%d body=%s", response.Code, len(stub.records), len(stub.pullValues), response.Body)
		}
	})
}

type resolutionWithKeySource struct {
	*recordingKeySource
	records []ResolveRecordRequest
}

func (s *resolutionWithKeySource) AgentResolveRecord(_ context.Context, _ string, req ResolveRecordRequest) (json.RawMessage, error) {
	s.records = append(s.records, req)
	return resolutionRecordRaw(req.CenterGeneration), nil
}

func TestResolveClearAndList(t *testing.T) {
	t.Run("clear", func(t *testing.T) {
		fixture := makeFixture(t, "base\n", "local\n")
		publishResolutionCenter(t, fixture, "pi")
		stub := &resolutionRouteStub{recordRaw: []byte(`{"ok":true,"action":"clear","entries":[],"pending":0,"errors":[]}`)}
		server := newWebServer(t, fixture, "test-token", stub, nil)
		response := request(t, server.Handler(), http.MethodPost,
			"/api/resolve/clear?agent=box&confirm=true", `{"adapters":["pi"]}`)
		if response.Code != http.StatusOK || len(stub.records) != 1 || stub.records[0].Action != resolutions.ActionClear || stub.records[0].CenterGeneration != 0 {
			t.Fatalf("clear = %d requests=%+v body=%s", response.Code, stub.records, response.Body)
		}
	})
	t.Run("list get marks stale", func(t *testing.T) {
		fixture := makeFixture(t, "base\n", "local\n")
		publishResolutionCenter(t, fixture, "pi")
		stub := &resolutionRouteStub{recordRaw: []byte(`{"ok":true,"action":"list","entries":[{"adapter":"pi","choice":"center","recordedAt":"2026-10-20T08:00:00Z","generationAtRecord":1}],"pending":1,"errors":[]}`)}
		server := newWebServer(t, fixture, "test-token", stub, nil)
		response := request(t, server.Handler(), http.MethodGet, "/api/resolve/record?agent=box")
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"centerGeneration":1`) || !strings.Contains(response.Body.String(), `"stale":false`) {
			t.Fatalf("list = %d %s", response.Code, response.Body)
		}
		if len(stub.records) != 1 || stub.records[0].Action != resolutions.ActionList {
			t.Fatalf("list request = %+v", stub.records)
		}
	})
}
