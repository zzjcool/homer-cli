package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"runtime"

	"github.com/zzjcool/homer-cli/internal/gens"
)

// otherPlatform returns a platform pair guaranteed to differ from the
// running one, so the routing tests hold on any host OS/arch (the original
// test hardcoded darwin/arm64 and failed on a darwin/arm64 CI host).
func otherPlatform() (goos, goarch string) {
	if runtime.GOOS == "darwin" {
		return "linux", "amd64"
	}
	return "darwin", "arm64"
}

// TestServeSelfBinaryPlatformParam verifies the platform routing of
// /dl/homer: a same-platform request still streams the hub's own binary
// (X-Homer-Platform absent), while a cross-platform request must NOT return
// the hub binary — it either proxies the release or fails loudly, but never
// serves an unexecutable binary silently.
func TestServeSelfBinaryPlatformParam(t *testing.T) {
	fixture := authFixture(t)
	if _, err := gens.New(fixture.home).Publish(map[string]map[string]string{
		"pi": {"settings/settings.json": "x\n"},
	}, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServeOptions{Addr: "127.0.0.1:0", HomerHome: fixture.home, Token: "hub-token"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)

	get := func(url string) *http.Response {
		request, _ := http.NewRequest(http.MethodGet, url, nil)
		request.Header.Set("Authorization", "Bearer hub-token")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	// Same-platform request: the hub's own binary, no platform marker needed.
	same := get(ts.URL + "/dl/homer?goos=" + runtime.GOOS + "&goarch=" + runtime.GOARCH)
	defer same.Body.Close()
	if same.StatusCode != http.StatusOK {
		t.Fatalf("same-platform download = %d", same.StatusCode)
	}
	if marker := same.Header.Get("X-Homer-Platform"); marker != "" {
		t.Fatalf("same-platform response must not carry a platform marker: %q", marker)
	}

	// Cross-platform request: never the hub's own binary. Against the real
	// GitHub it proxies the release; in this offline test the fetch fails
	// and must surface a 502 rather than silently streaming an unexecutable
	// linux binary to a darwin agent.
	otherOS, otherArch := otherPlatform()
	cross := get(ts.URL + "/dl/homer?goos=" + otherOS + "&goarch=" + otherArch)
	defer cross.Body.Close()
	if cross.StatusCode == http.StatusOK && cross.Header.Get("X-Homer-Platform") != otherOS+"/"+otherArch {
		t.Fatalf("cross-platform response returned a binary without the matching platform marker: %q", cross.Header.Get("X-Homer-Platform"))
	}
	if cross.StatusCode != http.StatusOK {
		// Offline test environment: expect the loud failure, not a wrong binary.
		if cross.StatusCode != http.StatusBadGateway {
			t.Fatalf("cross-platform download without network should fail loudly (502), got %d", cross.StatusCode)
		}
	}
}

// platformSourceStub is an AgentsSource whose platform answers are fixed.
type platformSourceStub struct {
	platforms map[string][2]string // agentID -> {goos, goarch}
}

func (s *platformSourceStub) ListAgents() []AgentInfo { return nil }
func (s *platformSourceStub) AgentStatus(ctx context.Context, agentID string) (json.RawMessage, error) {
	return nil, nil
}

func (s *platformSourceStub) AgentPlatform(agentID string) (goos, goarch string, ok bool) {
	pair, known := s.platforms[agentID]
	if !known || pair[0] == "" || pair[1] == "" {
		// Unknown machine, or enrolled but never heartbeated a platform
		// (the empty entry): the hub must not guess.
		return "", "", false
	}
	return pair[0], pair[1], true
}

// stubAgentsSource is the AgentsSource subset NewServer requires besides the
// platform capability; the platform stub above embeds the rest via a helper
// below to keep the matrix readable.
func (s *platformSourceStub) AgentDiff(ctx context.Context, agentID string, params DiffParams) (string, error) {
	return "", nil
}
func (s *platformSourceStub) AgentPush(ctx context.Context, agentID string, confirm bool, scope SyncScope) (json.RawMessage, error) {
	return nil, nil
}
func (s *platformSourceStub) AgentPull(ctx context.Context, agentID string, confirm bool, scope SyncScope) (json.RawMessage, error) {
	return nil, nil
}
func (s *platformSourceStub) RemoveAgent(agentID string) bool { return false }

// TestDownloadRoutesPlatformlessByMachineIdentity pins the 2026-10-10 Mac
// incident follow-up: an OLD client's platform-less /dl/homer request must
// be routed by the machine's heartbeat platform, never by the hub's own.
func TestDownloadRoutesPlatformlessByMachineIdentity(t *testing.T) {
	fixture := authFixture(t)
	if _, err := gens.New(fixture.home).Publish(map[string]map[string]string{
		"pi": {"settings/settings.json": "x\n"},
	}, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}

	otherOS, otherArch := otherPlatform()
	source := &platformSourceStub{platforms: map[string][2]string{
		// The bricked Mac shape: a machine on a platform the hub is not.
		"mac-agent": {otherOS, otherArch},
		// Same-platform machine: keeps the streaming channel.
		"linux-agent": {runtime.GOOS, runtime.GOARCH},
		// Enrolled but never heartbeated a platform: must be refused, not guessed.
		"fresh-agent": {},
	}}
	server, err := NewServer(ServeOptions{
		Addr:       "127.0.0.1:0",
		HomerHome:  fixture.home,
		Token:      "hub-token",
		Agents:     source,
		Enrollment: testEnrollmentService{},
		AgentEndpointAuthorized: func(r *http.Request) bool {
			return r.Header.Get("Authorization") == "Bearer agent-mac-secret" ||
				r.Header.Get("Authorization") == "Bearer agent-linux-secret" ||
				r.Header.Get("Authorization") == "Bearer agent-fresh-secret"
		},
		AgentIDOfRequest: func(r *http.Request) (string, bool) {
			switch r.Header.Get("Authorization") {
			case "Bearer agent-mac-secret":
				return "mac-agent", true
			case "Bearer agent-linux-secret":
				return "linux-agent", true
			case "Bearer agent-fresh-secret":
				return "fresh-agent", true
			}
			return "", false
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	get := func(bearer string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/dl/homer", nil)
		request.Header.Set("Authorization", bearer)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	// Cross-platform machine, platform-less request: proxied from the
	// release (or a loud 502 offline) — never the hub's own binary.
	mac := get("Bearer agent-mac-secret")
	if mac.Code == http.StatusOK && mac.Header().Get("X-Homer-Platform") != otherOS+"/"+otherArch {
		t.Fatalf("platform-less cross-platform request must carry the machine platform marker, got %q", mac.Header().Get("X-Homer-Platform"))
	}
	if mac.Code != http.StatusOK && mac.Code != http.StatusBadGateway {
		t.Fatalf("platform-less cross-platform request = %d body=%s", mac.Code, mac.Body)
	}

	// Same-platform machine, platform-less request: the hub's own stream,
	// no marker.
	linux := get("Bearer agent-linux-secret")
	if linux.Code != http.StatusOK {
		t.Fatalf("same-platform platform-less request = %d body=%s", linux.Code, linux.Body)
	}
	if marker := linux.Header().Get("X-Homer-Platform"); marker != "" {
		t.Fatalf("same-platform response must not carry a marker: %q", marker)
	}

	// Machine without a heartbeat platform: refused loudly, never guessed.
	fresh := get("Bearer agent-fresh-secret")
	if fresh.Code != http.StatusConflict {
		t.Fatalf("unknown-platform machine must be refused with 409, got %d body=%s", fresh.Code, fresh.Body)
	}

	// Shared credentials (hub token): bootstrap channel keeps the hub's own
	// binary — install.sh gates platform checks client-side.
	token := get("Bearer hub-token")
	if token.Code != http.StatusOK {
		t.Fatalf("hub-token request = %d body=%s", token.Code, token.Body)
	}
	if marker := token.Header().Get("X-Homer-Platform"); marker != "" {
		t.Fatalf("hub-token response must not carry a marker: %q", marker)
	}
}
