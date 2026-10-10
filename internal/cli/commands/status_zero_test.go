package commands

import (
	"encoding/json"
	"testing"
)

// TestStatusReportOmitsResolutionsWhenEmpty freezes additive agent→hub
// status fields: an empty report must not grow resolutions or configOutline,
// and a populated resolution must round-trip all four fields.
func TestStatusReportOmitsResolutionsWhenEmpty(t *testing.T) {
	data, err := json.Marshal(StatusReport{Adapters: []StatusAdapterReport{}, Errors: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" || json.Valid(data) == false {
		t.Fatalf("invalid JSON: %s", data)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatal(err)
	}
	if _, present := probe["resolutions"]; present {
		t.Fatalf("empty status must omit resolutions: %s", data)
	}
	if _, present := probe["configOutline"]; present {
		t.Fatalf("empty status must omit configOutline: %s", data)
	}
	withOne, err := json.Marshal(StatusReport{Resolutions: []StatusResolution{{
		Adapter: "pi", Choice: "center", RecordedAt: "2026-10-09T00:00:00Z", GenerationAtRecord: 7,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var back StatusReport
	if err := json.Unmarshal(withOne, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Resolutions) != 1 || back.Resolutions[0].Adapter != "pi" ||
		back.Resolutions[0].Choice != "center" || back.Resolutions[0].GenerationAtRecord != 7 {
		t.Fatalf("resolution did not round-trip: %s", withOne)
	}
}
