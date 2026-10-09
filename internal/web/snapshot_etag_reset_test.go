package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestSnapshotETagChangesWhenGenerationNumberIsReused(t *testing.T) {
	fixture := webFixture{home: t.TempDir()}
	server := newWebServer(t, fixture, "test-token", nil, nil)
	handler := server.Handler()

	publish := func(content string) {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"homerJson": "{}",
			"store": map[string]map[string]string{
				"pi": {"settings/settings.json": content},
			},
		})
		if err != nil {
			t.Fatalf("marshal snapshot: %v", err)
		}
		response := request(t, handler, http.MethodPost, "/api/snapshot", string(body))
		if response.Code != http.StatusOK {
			t.Fatalf("publish snapshot = %d %s", response.Code, response.Body)
		}
	}
	get := func(etag string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
		req.Header.Set("Authorization", "Bearer test-token")
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}

	publish("generation one content A")
	first := get("")
	if first.Code != http.StatusOK || first.Body.Len() == 0 {
		t.Fatalf("first snapshot = %d %s", first.Code, first.Body)
	}
	firstETag := first.Header().Get("ETag")
	if !regexp.MustCompile(`^"g1-[0-9a-f]{16}"$`).MatchString(firstETag) {
		t.Fatalf("first ETag = %q, want generation plus 16-hex content digest", firstETag)
	}
	if same := get(firstETag); same.Code != http.StatusNotModified || same.Body.Len() != 0 {
		t.Fatalf("same generation/content conditional GET = %d body=%q", same.Code, same.Body.String())
	}

	// Simulate deleting generations/ and HEAD, then publishing a distinct
	// payload which reuses generation number 1.
	if err := os.RemoveAll(filepath.Join(fixture.home, "generations")); err != nil {
		t.Fatalf("remove generation history: %v", err)
	}
	if err := os.Remove(filepath.Join(fixture.home, "HEAD")); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove HEAD: %v", err)
	}
	publish("generation one content B")
	changed := get(firstETag)
	if changed.Code != http.StatusOK || changed.Body.Len() == 0 {
		t.Fatalf("old validator after generation reset = %d body=%q, want full 200", changed.Code, changed.Body.String())
	}
	secondETag := changed.Header().Get("ETag")
	if secondETag == firstETag {
		t.Fatalf("ETag did not change when generation 1 content changed: %q", secondETag)
	}
	var payload struct {
		Generation int                          `json:"generation"`
		Store      map[string]map[string]string `json:"store"`
	}
	if err := json.Unmarshal(changed.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode changed snapshot: %v", err)
	}
	if payload.Generation != 1 || payload.Store["pi"]["settings/settings.json"] != "generation one content B" {
		t.Fatalf("changed full response = generation %d store %#v", payload.Generation, payload.Store)
	}
	if same := get(secondETag); same.Code != http.StatusNotModified || same.Body.Len() != 0 {
		t.Fatalf("current generation/content conditional GET = %d body=%q", same.Code, same.Body.String())
	}

	// Reset the same content to the same generation again: its digest/validator
	// is content-addressed and remains usable for a 304.
	if err := os.RemoveAll(filepath.Join(fixture.home, "generations")); err != nil {
		t.Fatalf("remove generation history for identical republish: %v", err)
	}
	if err := os.Remove(filepath.Join(fixture.home, "HEAD")); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove HEAD for identical republish: %v", err)
	}
	publish("generation one content B")
	identical := get(secondETag)
	if identical.Code != http.StatusNotModified || identical.Body.Len() != 0 || identical.Header().Get("ETag") != secondETag {
		t.Fatalf("identical content republish conditional GET = %d etag=%q body=%q, want 304 with %q", identical.Code, identical.Header().Get("ETag"), identical.Body.String(), secondETag)
	}
}
