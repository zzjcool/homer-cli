package hub

import (
	"encoding/json"
	"testing"
)

// TestAgentDriftZeroOmitsResolutions freezes the heartbeat wire contract:
// zero-value drift must not grow new keys (staged-resolution plan S0).
func TestAgentDriftZeroOmitsResolutions(t *testing.T) {
	data, err := json.Marshal(AgentDrift{Push: 1, Pull: 2, Conflicts: 3})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"push":1,"pull":2,"conflicts":3}` {
		t.Fatalf("zero drift serialized to %s", data)
	}
	withRes, err := json.Marshal(AgentDrift{Resolutions: 4})
	if err != nil {
		t.Fatal(err)
	}
	// push/pull/conflicts are non-omitempty (existing shape); resolutions
	// only adds when set, which is what old hubs tolerate.
	if string(withRes) != `{"push":0,"pull":0,"conflicts":0,"resolutions":4}` {
		t.Fatalf("drift with resolutions serialized to %s", withRes)
	}
}
