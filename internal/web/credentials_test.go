package web

import (
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
)

func TestApplyCredentialProbeAddsPerMachinePresence(t *testing.T) {
	rules := credentialRules()
	found := map[string]bool{
		rules[0].Destination: true,
		rules[1].Destination: false,
	}
	choices := []AdapterChoice{{ID: "pi"}, {ID: "opencode"}, {ID: "herdr"}}
	applyCredentialProbe(choices, found)

	if len(choices[0].Credentials) != len(credentialRulesFor("pi")) || choices[0].Credentials[0].Exists != credPresent || choices[0].Credentials[1].Exists != credAbsent {
		t.Fatalf("pi credentials = %#v", choices[0].Credentials)
	}
	if len(choices[1].Credentials) != 1 || choices[1].Credentials[0].Exists != credUnknown {
		t.Fatalf("opencode credentials = %#v", choices[1].Credentials)
	}
	if len(choices[2].Credentials) != 0 {
		t.Fatalf("herdr should have no credential rules: %#v", choices[2].Credentials)
	}
}

func TestSecretDestinationMapsStorePathToMachinePath(t *testing.T) {
	config := &core.HomerConfig{Adapters: map[string]core.AdapterConfig{
		"pi": {Root: "~/.pi/agent", Categories: map[string]core.CategoryConfig{
			"files": {Paths: []string{"./"}, Mode: core.SyncModeMirror},
		}},
	}}
	adapterID, destination := secretDestination(config, "pi/files/web-search.json")
	if adapterID != "pi" || destination != "~/.pi/agent/web-search.json" {
		t.Fatalf("got %q %q", adapterID, destination)
	}
	if id, dest := secretDestination(config, "pi/nope/x"); id != "pi" || dest != "" {
		t.Fatalf("unknown category = %q %q", id, dest)
	}
	if id, dest := secretDestination(nil, "pi/files/x"); id != "pi" || dest != "" {
		t.Fatalf("no config = %q %q", id, dest)
	}
}
