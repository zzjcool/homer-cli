package hub

import (
	"encoding/json"
	"testing"
)

// TestTaskOptionsZeroValueOmitsResolutionFields freezes the wire contract:
// an all-zero TaskOptions must keep serializing to "{}" so old agents never
// see unknown fields (staged-resolution plan S0).
func TestTaskOptionsZeroValueOmitsResolutionFields(t *testing.T) {
	data, err := json.Marshal(TaskOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{}" {
		t.Fatalf("zero TaskOptions serialized to %s, want {}", data)
	}
	var back TaskOptions
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	// Round-trip of every new field.
	filled, err := json.Marshal(TaskOptions{
		ResolutionAction: "record",
		ResolutionChoice: "center",
		CenterGeneration: 7,
		ApplyResolutions: true,
		ClearResolutions: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var reparsed TaskOptions
	if err := json.Unmarshal(filled, &reparsed); err != nil {
		t.Fatal(err)
	}
	if reparsed.ResolutionAction != "record" || reparsed.ResolutionChoice != "center" ||
		reparsed.CenterGeneration != 7 || !reparsed.ApplyResolutions || !reparsed.ClearResolutions {
		t.Fatalf("resolution fields did not round-trip: %s", filled)
	}
}
