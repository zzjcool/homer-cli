package web

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/gens"
	"github.com/zzjcool/homer-cli/internal/resolutions"
)

func TestDispatchChoicesWithResolutions(t *testing.T) {
	machine := []commands.StatusAdapterReport{{ID: "pi", Conflicts: 2, Pull: 3}}
	center := []string{"pi"}
	valid := map[string]ResolutionView{"pi": {
		Choice: resolutions.ChoiceCenter, RecordedAt: "2026-10-20T08:00:00Z",
		GenerationAtRecord: 7, Stale: false,
	}}
	got := BuildDispatchChoicesWithResolutions(center, machine, valid)
	if len(got) != 1 || !got[0].Enabled || !got[0].Checked || got[0].Reason != "" {
		t.Fatalf("valid decision choice = %#v", got)
	}
	if got[0].Resolution == nil || got[0].Resolution.Choice != resolutions.ChoiceCenter || !strings.Contains(got[0].Detail, "已记录：以中心为准") {
		t.Fatalf("valid decision details = %#v", got[0])
	}

	stale := map[string]ResolutionView{"pi": {
		Choice: resolutions.ChoiceLocal, GenerationAtRecord: 6, Stale: true,
	}}
	got = BuildDispatchChoicesWithResolutions(center, machine, stale)
	if len(got) != 1 || got[0].Enabled || got[0].Checked || got[0].Reason != reasonStale || got[0].Resolution == nil {
		t.Fatalf("stale decision choice = %#v", got)
	}

	withoutConflict := BuildDispatchChoicesWithResolutions(center,
		[]commands.StatusAdapterReport{{ID: "pi", Pull: 1}}, valid)
	if len(withoutConflict) != 1 || withoutConflict[0].Resolution != nil || !withoutConflict[0].Checked {
		t.Fatalf("decision without conflict must be ignored: %#v", withoutConflict)
	}

	legacy := BuildDispatchChoices(center, machine)
	withNil := BuildDispatchChoicesWithResolutions(center, machine, nil)
	if !reflect.DeepEqual(legacy, withNil) {
		t.Fatalf("nil views changed legacy dispatch choices: %#v != %#v", withNil, legacy)
	}
}

func TestResolveChoicesWithResolutionsAndLegacyBehavior(t *testing.T) {
	adapters := []commands.StatusAdapterReport{
		{ID: "pi", Conflicts: 1},
		{ID: "herdr", Push: 1},
	}
	views := map[string]ResolutionView{"pi": {
		Choice: resolutions.ChoiceLocal, GenerationAtRecord: 4, Stale: true,
	}}
	got := BuildResolveChoicesWithResolutions(adapters, views)
	if len(got) != 1 || got[0].ID != "pi" || got[0].Resolution == nil || !got[0].Resolution.Stale {
		t.Fatalf("resolve choices = %#v", got)
	}
	if legacy, withNil := BuildResolveChoices(adapters), BuildResolveChoicesWithResolutions(adapters, nil); !reflect.DeepEqual(legacy, withNil) {
		t.Fatalf("nil views changed legacy resolve choices: %#v != %#v", withNil, legacy)
	}
}

func TestResolutionViewsStale(t *testing.T) {
	entries := []resolutions.Entry{
		{Adapter: "pi", Choice: resolutions.ChoiceCenter, RecordedAt: "2026-10-20T08:00:00Z", GenerationAtRecord: 7},
		{Adapter: "herdr", Choice: resolutions.ChoiceLocal, RecordedAt: "2026-10-20T08:00:00Z", GenerationAtRecord: 6},
		{Adapter: "bad id", Choice: resolutions.ChoiceCenter, GenerationAtRecord: 7},
		{Adapter: "pad", Choice: "wrong", GenerationAtRecord: 7},
	}
	views := ResolutionViews(entries, 7)
	if len(views) != 2 || views["pi"].Stale || !views["herdr"].Stale {
		t.Fatalf("views = %#v", views)
	}
	unknown := ResolutionViews(entries[:1], 0)
	if !unknown["pi"].Stale {
		t.Fatalf("unknown center generation must fail stale: %#v", unknown)
	}
}

func TestChoicesResponseCarriesResolution(t *testing.T) {
	home := t.TempDir()
	fixture := makeFixtureAtHome(t, home, "base\n")
	if _, err := gens.New(home).Publish(map[string]map[string]string{
		"pi": {"settings/settings.json": "remote\n"},
	}, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	source := &sourceStub{statusRaw: json.RawMessage(`{"adapters":[{"id":"pi","conflicts":1,"pull":2}],"resolutions":[{"adapter":"pi","choice":"center","recordedAt":"2026-10-20T08:00:00Z","generationAtRecord":1}],"errors":[]}`)}
	server := newWebServer(t, fixture, "test-token", source, nil)
	response := request(t, server.Handler(), http.MethodGet, "/api/sync/choices?direction=dispatch&agent=box")
	if response.Code != http.StatusOK {
		t.Fatalf("choices = %d %s", response.Code, response.Body)
	}
	var payload struct {
		Adapters []AdapterChoice `json:"adapters"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Adapters) != 1 || payload.Adapters[0].Resolution == nil || payload.Adapters[0].Resolution.Choice != resolutions.ChoiceCenter {
		t.Fatalf("choice response omitted resolution: %#v", payload.Adapters)
	}
}
