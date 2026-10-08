package hub

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/stream"
)

func TestCallBudget(t *testing.T) {
	cases := []struct {
		kind TaskKind
		want time.Duration
	}{
		{TaskKindStatus, CallBudgetDefault},
		{TaskKindDiff, CallBudgetDefault},
		{TaskKindPush, CallBudgetDefault},
		{TaskKindPull, 12 * time.Minute},
		{TaskKindSSHKey, CallBudgetDefault},
		{TaskKindSecret, CallBudgetDefault},
		{TaskKindUpgrade, 3 * time.Minute},
		{TaskKindToolUpgrade, 7 * time.Minute},
		{TaskKind("future-kind"), CallBudgetDefault},
	}
	for _, test := range cases {
		if got := CallBudget(test.kind); got != test.want {
			t.Errorf("CallBudget(%q) = %v, want %v", test.kind, got, test.want)
		}
	}
	if CallBudgetDefault != time.Minute || ReqDeadlineSlack != 5*time.Second {
		t.Fatalf("default budget/slack = %v/%v, want 1m/5s", CallBudgetDefault, ReqDeadlineSlack)
	}
	if AgentStaleAfter != 90*time.Second {
		t.Fatalf("AgentStaleAfter = %v, want 90s", AgentStaleAfter)
	}
}

func TestTaskOptionsWireShape(t *testing.T) {
	want := TaskOptions{
		Adapter: "pi", Category: "settings", Path: "settings.json", Confirm: true,
		Adapters: []string{"pi", "vscode"}, Overwrite: true, AllowSecrets: true,
		Resolve: "center", GitHubUser: "octocat", SSHKeys: []string{"ssh-ed25519 key"},
		SecretAction: "unlock", SecretPayload: json.RawMessage(`{"action":"unlock"}`), Tool: "pi",
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got TaskOptions
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TaskOptions round trip = %+v, want %+v", got, want)
	}
	// Frozen method constants are the wire names shared with agentd.
	if string(TaskKindStatus) != "status" || string(TaskKindDiff) != "diff" || string(TaskKindPush) != "push" || string(TaskKindPull) != "pull" || string(TaskKindSSHKey) != "ssh-key" || string(TaskKindSecret) != "secret" || string(TaskKindUpgrade) != "upgrade" || string(TaskKindToolUpgrade) != "tool-upgrade" {
		t.Fatal("TaskKind values no longer match the frozen wire method names")
	}
	if stream.ProtocolVersion != 1 {
		t.Fatalf("stream protocol version = %d", stream.ProtocolVersion)
	}
}
