package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSnapshotUploadCAS (R2): a scoped upload whose baseGeneration names a
// generation the center has moved past must be refused with 409
// generation-conflict, and the moving writer's generation must stay intact.
// A matching base (and 0 = legacy) still publishes.
func TestSnapshotUploadCAS(t *testing.T) {
	fixture := authFixture(t)
	server, err := NewServer(ServeOptions{
		Addr:      "127.0.0.1:0",
		HomerHome: fixture.home,
		Token:     "hub-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)

	post := func(body string) (int, string) {
		request, err := http.NewRequest(http.MethodPost, ts.URL+"/api/snapshot", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer hub-token")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		buf := make([]byte, 1<<16)
		n, _ := response.Body.Read(buf)
		return response.StatusCode, string(buf[:n])
	}

	// Seed generation 1.
	status, body := post(`{"homerJson":"{}","store":{"pi":{"settings/settings.json":"v1\n"}},"generation":0}`)
	if status != http.StatusOK || !strings.Contains(body, `"generation":1`) {
		t.Fatalf("seed upload = %d %s", status, body)
	}
	// A CAS upload based on generation 1 succeeds.
	status, body = post(`{"homerJson":"{}","store":{"pi":{"settings/settings.json":"v2\n"}},"generation":1,"adapters":["pi"]}`)
	if status != http.StatusOK || !strings.Contains(body, `"generation":2`) {
		t.Fatalf("matching CAS upload = %d %s", status, body)
	}
	// The same stale base must now conflict (the center is at 2).
	status, body = post(`{"homerJson":"{}","store":{"pi":{"settings/settings.json":"v3\n"}},"generation":1,"adapters":["pi"]}`)
	if status != http.StatusConflict || !strings.Contains(body, "generation-conflict") {
		t.Fatalf("stale CAS upload = %d %s", status, body)
	}
	// The center still holds v2 — the conflicting upload wrote nothing.
	get, err := http.NewRequest(http.MethodGet, ts.URL+"/api/snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	get.Header.Set("Authorization", "Bearer hub-token")
	response, err := http.DefaultClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload struct {
		Generation int                          `json:"generation"`
		Store      map[string]map[string]string `json:"store"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Generation != 2 || payload.Store["pi"]["settings/settings.json"] != "v2\n" {
		t.Fatalf("center must hold generation 2 with v2: %d %+v", payload.Generation, payload.Store["pi"])
	}
	// baseGeneration 0 (legacy / full replace) bypasses the check.
	status, body = post(`{"homerJson":"{}","store":{"pi":{"settings/settings.json":"v4\n"}},"generation":0}`)
	if status != http.StatusOK || !strings.Contains(body, `"generation":3`) {
		t.Fatalf("legacy upload = %d %s", status, body)
	}
}
