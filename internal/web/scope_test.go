package web

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestReadSyncScopeConfigPolicy(t *testing.T) {
	t.Run("valid choices", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/api/agents/box/pull?confirm=true", strings.NewReader(
			`{"adapters":["pi","herdr"],"configPolicy":{"pi":"center","herdr":"keep"}}`,
		))
		scope, err := readSyncScope(request)
		if err != nil {
			t.Fatalf("readSyncScope: %v", err)
		}
		want := map[string]string{"pi": "center", "herdr": "keep"}
		if !reflect.DeepEqual(scope.ConfigPolicy, want) || !scope.Explicit || !reflect.DeepEqual(scope.Adapters, []string{"pi", "herdr"}) {
			t.Fatalf("scope = %+v", scope)
		}
	})

	for _, testCase := range []struct {
		name string
		body string
	}{
		{name: "unknown choice", body: `{"configPolicy":{"pi":"local"}}`},
		{name: "invalid adapter id", body: `{"configPolicy":{"Pi":"center"}}`},
		{name: "non-object", body: `{"configPolicy":["pi"]}`},
		{name: "null", body: `{"configPolicy":null}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/agents/box/pull?confirm=true", strings.NewReader(testCase.body))
			if _, err := readSyncScope(request); err == nil {
				t.Fatalf("readSyncScope accepted %s", testCase.body)
			}
		})
	}

	t.Run("policy with query selection", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/api/agents/box/pull?adapters=pi&confirm=true", strings.NewReader(`{"configPolicy":{"pi":"keep"}}`))
		scope, err := readSyncScope(request)
		if err != nil {
			t.Fatalf("readSyncScope: %v", err)
		}
		if !scope.Explicit || !reflect.DeepEqual(scope.Adapters, []string{"pi"}) || scope.ConfigPolicy["pi"] != "keep" {
			t.Fatalf("query/body scope = %+v", scope)
		}
	})

	t.Run("omitted policy", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/api/agents/box/pull?confirm=true", strings.NewReader(`{"adapters":["pi"]}`))
		scope, err := readSyncScope(request)
		if err != nil {
			t.Fatalf("readSyncScope: %v", err)
		}
		if scope.ConfigPolicy != nil {
			t.Fatalf("omitted policy = %#v, want nil", scope.ConfigPolicy)
		}
	})
}

func TestDispatchRejectsInvalidConfigPolicyAsBadRequest(t *testing.T) {
	fixture := makeFixture(t, "base\n", "base\n")
	server := newWebServer(t, fixture, "test-token", &sourceStub{}, nil)
	response := request(t, server.Handler(), http.MethodPost,
		"/api/sync?direction=dispatch&confirm=true",
		`{"adapters":["pi"],"configPolicy":{"pi":"local"}}`,
	)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid configPolicy status = %d, body=%s", response.Code, response.Body)
	}
	if code := decodeBody(t, response)["error"].(map[string]any)["code"]; code != "bad-request" {
		t.Fatalf("invalid configPolicy code = %v", code)
	}
}
