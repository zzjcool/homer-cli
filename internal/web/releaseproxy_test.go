package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"runtime"

	"github.com/zzjcool/homer-cli/internal/gens"
)

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
	same := get(ts.URL + "/dl/homer?goos=" + fakeHubGOOS + "&goarch=" + fakeHubGOARCH)
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
	cross := get(ts.URL + "/dl/homer?goos=darwin&goarch=arm64")
	defer cross.Body.Close()
	if cross.StatusCode == http.StatusOK && cross.Header.Get("X-Homer-Platform") != "darwin/arm64" {
		t.Fatalf("cross-platform response returned a binary without the matching platform marker: %q", cross.Header.Get("X-Homer-Platform"))
	}
	if cross.StatusCode != http.StatusOK {
		// Offline test environment: expect the loud failure, not a wrong binary.
		if cross.StatusCode != http.StatusBadGateway {
			t.Fatalf("cross-platform download without network should fail loudly (502), got %d", cross.StatusCode)
		}
	}
}

// fakeHubGOOS/GOARCH mirror the hub's own platform for the routing test.
var fakeHubGOOS = runtime.GOOS
var fakeHubGOARCH = runtime.GOARCH
