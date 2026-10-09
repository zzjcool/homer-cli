package hub

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/streamtest"
	"github.com/zzjcool/homer-cli/internal/toolctl"
)

func registryTestSession() (*stream.Session, stream.Conn) {
	left, right := streamtest.Pipe(streamtest.PipeOptions{})
	return stream.NewSession(left, stream.Options{}), right
}

func TestRegistryAttach(t *testing.T) {
	r := NewRegistry()
	first, firstPeer := registryTestSession()
	defer firstPeer.CloseNow()
	defer first.Close(stream.CloseNormal, "test complete")

	info := AgentInfo{
		AgentID:  "agent-a",
		Hostname: "box-a",
		Version:  "v1",
		Drift:    &AgentDrift{Push: 1},
		Host:     &HostSnapshot{OS: "linux"},
		Tools:    []toolctl.Status{{ID: "pi", Version: "1.0"}},
	}
	if previous := r.Attach(info, first); previous != nil {
		t.Fatalf("first Attach() returned previous session %p", previous)
	}
	if got, ok := r.Session("agent-a"); !ok || got != first {
		t.Fatalf("Session() = (%p, %v), want first session", got, ok)
	}
	listed, ok := r.Get("agent-a")
	if !ok || listed.Stale || listed.LastSeen.IsZero() {
		t.Fatalf("newly attached agent = %+v, found=%v; want a live non-stale agent", listed, ok)
	}

	second, secondPeer := registryTestSession()
	defer secondPeer.CloseNow()
	defer second.Close(stream.CloseNormal, "test complete")
	previous := r.Attach(AgentInfo{AgentID: "agent-a", Hostname: "box-a-new"}, second)
	if previous != first {
		t.Fatalf("second Attach() returned %p, want first session %p", previous, first)
	}
	// A late disconnect from the replaced session must not detach its successor.
	r.Detach("agent-a", first)
	if got, ok := r.Session("agent-a"); !ok || got != second {
		t.Fatalf("stale Detach() cleared the current session: (%p, %v)", got, ok)
	}
	updated, ok := r.Get("agent-a")
	if !ok || updated.Hostname != "box-a-new" || updated.Version != "v1" || updated.Drift == nil || updated.Drift.Push != 1 || updated.Host == nil || updated.Host.OS != "linux" || len(updated.Tools) != 1 || updated.Tools[0].ID != "pi" {
		t.Fatalf("Attach() failed to refresh identity or preserve omitted reports: %+v", updated)
	}

	r.Detach("agent-a", second)
	if got, ok := r.Session("agent-a"); ok || got != nil {
		t.Fatalf("Detach(current) left a session: (%p, %v)", got, ok)
	}
	afterDetach, ok := r.Get("agent-a")
	if !ok || !afterDetach.Stale {
		t.Fatalf("agent without a session is not stale: %+v, found=%v", afterDetach, ok)
	}
}

func TestRegistryAttachStaleDerivation(t *testing.T) {
	r := NewRegistry()
	sess, peer := registryTestSession()
	defer peer.CloseNow()
	defer sess.Close(stream.CloseNormal, "test complete")

	old := time.Now().Add(-AgentStaleAfter)
	r.Attach(AgentInfo{AgentID: "agent-a", LastSeen: old}, sess)
	info, ok := r.Get("agent-a")
	if !ok || !info.Stale {
		t.Fatalf("active session with stale LastSeen = %+v, want stale", info)
	}
	r.Touch("agent-a")
	info, _ = r.Get("agent-a")
	if info.Stale || !info.LastSeen.After(old) {
		t.Fatalf("Touch() did not refresh liveness: %+v", info)
	}

	sess.Close(stream.CloseGoingAway, "test close")
	info, ok = r.Get("agent-a")
	if !ok || !info.Stale {
		t.Fatalf("closed session is not stale before Detach: %+v, found=%v", info, ok)
	}
}

func TestRegistryAttachConcurrent(t *testing.T) {
	r := NewRegistry()
	const workers = 40
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess, peer := registryTestSession()
			defer peer.CloseNow()
			previous := r.Attach(AgentInfo{AgentID: "shared", Hostname: "host"}, sess)
			if previous != nil {
				previous.Close(stream.CloseSuperseded, "test replacement")
				r.Detach("shared", previous)
			}
			if i%2 == 0 {
				r.Touch("shared")
				_, _ = r.Get("shared")
			}
			r.Detach("shared", sess)
			sess.Close(stream.CloseNormal, "test complete")
		}()
	}
	wg.Wait()
	if got := r.List(); len(got) != 1 || got[0].AgentID != "shared" {
		t.Fatalf("List() = %+v, want the retained agent record", got)
	}
	if _, ok := r.Session("shared"); ok {
		t.Fatal("all concurrent sessions detached, but one remains current")
	}
}

func TestRegistryListSortsAndReturnsCopies(t *testing.T) {
	r := NewRegistry()
	for _, id := range []string{"b", "a"} {
		sess, peer := registryTestSession()
		r.Attach(AgentInfo{AgentID: id, Hostname: id, Host: &HostSnapshot{OS: "linux"}, Tools: []toolctl.Status{{ID: "pi", Version: "1.0"}}}, sess)
		defer peer.CloseNow()
		defer sess.Close(stream.CloseNormal, "test complete")
	}
	got := r.List()
	if len(got) != 2 || !sort.SliceIsSorted(got, func(i, j int) bool { return got[i].AgentID < got[j].AgentID }) {
		t.Fatalf("List() order = %+v", got)
	}
	got[0].Hostname = "mutated"
	got[0].Host.OS = "mutated"
	got[0].Tools[0].Version = "mutated"
	again, ok := r.Get("a")
	if !ok || again.Hostname != "a" || again.Host == nil || again.Host.OS != "linux" || again.Tools[0].Version != "1.0" {
		t.Fatalf("List() returned registry-owned values: %+v", again)
	}
}

func TestRegistryUpdatesAndRemove(t *testing.T) {
	r := NewRegistry()
	sess, peer := registryTestSession()
	defer peer.CloseNow()
	defer sess.Close(stream.CloseNormal, "test complete")
	r.Attach(AgentInfo{AgentID: "agent-a"}, sess)

	r.UpdateVersion("agent-a", " v2 ")
	r.UpdateDrift("agent-a", AgentDrift{Pull: 2})
	r.UpdateHost("agent-a", HostSnapshot{OS: "linux"})
	r.UpdateTools("agent-a", []toolctl.Status{{ID: "pi", Version: "1.0"}})
	info, ok := r.Get("agent-a")
	if !ok || info.Version != "v2" || info.Drift == nil || info.Drift.Pull != 2 || info.Host == nil || info.Host.OS != "linux" || len(info.Tools) != 1 {
		t.Fatalf("updates = %+v", info)
	}
	r.UpdateVersion("missing", "v3")
	r.UpdateDrift("missing", AgentDrift{})
	r.UpdateHost("missing", HostSnapshot{OS: "nope"})
	r.UpdateTools("missing", []toolctl.Status{{ID: "pi"}})
	r.NoteToolVersion("agent-a", "pi", "1.1")
	info, _ = r.Get("agent-a")
	if info.Tools[0].Version != "1.1" {
		t.Fatalf("NoteToolVersion() = %+v", info.Tools)
	}

	if !r.Remove("agent-a") || r.Remove("agent-a") {
		t.Fatal("Remove() did not remove the current agent exactly once")
	}
	if _, ok := r.Get("agent-a"); ok {
		t.Fatal("removed agent remains in the registry")
	}
}

func TestNoteWriteOutcomeReplacesFreshMachineMarker(t *testing.T) {
	r := NewRegistry()
	r.Attach(AgentInfo{
		AgentID: "box",
		Drift:   &AgentDrift{Error: "未找到 homer 配置: /home/agent/.homer/homer.json；请先运行 `homer init`"},
	}, nil)
	r.NoteWriteOutcome("box", "conflicts-remain", false, 1)
	info, ok := r.Get("box")
	if !ok || info.Drift == nil || info.Drift.Conflicts != 1 || info.Drift.Error != "" {
		t.Fatalf("conflict outcome = %+v", info.Drift)
	}
	r.NoteWriteOutcome("box", "resolved", true, 0)
	info, _ = r.Get("box")
	if info.Drift == nil || info.Drift.Conflicts != 0 || info.Drift.Error != "" {
		t.Fatalf("resolved outcome = %+v", info.Drift)
	}
	r.NoteWriteOutcome("box", "aborted", false, 0)
	info, _ = r.Get("box")
	if info.Drift == nil || info.Drift.Conflicts != 0 {
		t.Fatalf("aborted outcome changed drift: %+v", info.Drift)
	}
}

func TestRegistryNilAndMissingOperations(t *testing.T) {
	var r *Registry
	if info, ok := r.Get("missing"); ok || info.AgentID != "" {
		t.Fatal("nil Registry.Get() returned data")
	}
	if got := r.List(); len(got) != 0 {
		t.Fatalf("nil Registry.List() = %+v", got)
	}
	if sess, ok := r.Session("missing"); sess != nil || ok {
		t.Fatalf("nil Registry.Session() = (%v, %v)", sess, ok)
	}
	r.Detach("missing", nil)
	r.Touch("missing")
	r.UpdateDrift("missing", AgentDrift{})
	r.UpdateHost("missing", HostSnapshot{})
	r.UpdateTools("missing", nil)
	r.UpdateVersion("missing", "v1")
	r.NoteWriteOutcome("missing", "resolved", true, 0)
	r.NoteToolVersion("missing", "pi", "1.0")
	if r.Remove("missing") {
		t.Fatal("nil Registry.Remove() = true")
	}

	if info, ok := NewRegistry().Get("missing"); ok || info.AgentID != "" {
		t.Fatal("unknown Registry.Get() returned data")
	}
}

func TestRegistryStoresToolEmptyAndNilDistinction(t *testing.T) {
	r := NewRegistry()
	sess, peer := registryTestSession()
	defer peer.CloseNow()
	defer sess.Close(stream.CloseNormal, "test complete")
	r.Attach(AgentInfo{AgentID: "agent-a", Tools: []toolctl.Status{{ID: "pi"}}}, sess)
	r.Attach(AgentInfo{AgentID: "agent-a"}, sess)
	info, _ := r.Get("agent-a")
	if len(info.Tools) != 1 {
		t.Fatalf("an omitted tools list erased the previous report: %+v", info.Tools)
	}
	r.UpdateTools("agent-a", []toolctl.Status{})
	info, _ = r.Get("agent-a")
	if info.Tools == nil || len(info.Tools) != 0 {
		t.Fatalf("an explicit empty tools list was not retained: %#v", info.Tools)
	}
	if !reflect.DeepEqual(cloneTools(info.Tools), []toolctl.Status{}) {
		t.Fatalf("empty tools clone = %#v", cloneTools(info.Tools))
	}
}

func TestRegistryHeartbeatModelsKeepJSONShape(t *testing.T) {
	// The registry fields exposed to web remain JSON-compatible with the
	// status data already consumed by the console.
	r := NewRegistry()
	session, peer := registryTestSession()
	defer session.Close(stream.CloseNormal, "test complete")
	defer peer.CloseNow()
	r.Attach(AgentInfo{AgentID: "agent-a", Drift: &AgentDrift{Push: 2}}, session)
	encoded, err := json.Marshal(r.List())
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(encoded) {
		t.Fatalf("agent list is not JSON: %s", encoded)
	}
}

func TestHeartbeatCarriesResolutions(t *testing.T) {
	r := NewRegistry()
	session, peer := registryTestSession()
	defer session.Close(stream.CloseNormal, "test complete")
	defer peer.CloseNow()
	r.Attach(AgentInfo{AgentID: "box"}, session)
	hub := NewAgentHub(r, &Authenticator{Token: hubTestToken}, nil, HubOptions{})
	payload, err := json.Marshal(HeartbeatParams{Drift: &AgentDrift{Conflicts: 1, Resolutions: 2}})
	if err != nil {
		t.Fatal(err)
	}
	hub.heartbeatHandler("box", session)(context.Background(), MethodHeartbeat, payload)
	info, ok := r.Get("box")
	if !ok || info.Drift == nil || info.Drift.Resolutions != 2 {
		t.Fatalf("heartbeat drift = %+v, found=%v", info.Drift, ok)
	}
}

func TestNoteResolutionsOnlyWhenDriftPresent(t *testing.T) {
	r := NewRegistry()
	r.Attach(AgentInfo{AgentID: "without"}, nil)
	r.Attach(AgentInfo{AgentID: "with", Drift: &AgentDrift{Conflicts: 2}}, nil)

	r.NoteResolutions("without", 4)
	r.NoteResolutions("with", 3)
	without, _ := r.Get("without")
	with, _ := r.Get("with")
	if without.Drift != nil {
		t.Fatalf("NoteResolutions created a drift record: %+v", without.Drift)
	}
	if with.Drift == nil || with.Drift.Resolutions != 3 {
		t.Fatalf("NoteResolutions did not update existing drift: %+v", with.Drift)
	}
}

func TestNoteWriteOutcomeKeepsResolutionsOnConflicts(t *testing.T) {
	r := NewRegistry()
	r.Attach(AgentInfo{AgentID: "box", Drift: &AgentDrift{Conflicts: 1, Resolutions: 3}}, nil)
	r.NoteWriteOutcome("box", "conflicts-remain", false, 2)
	info, _ := r.Get("box")
	if info.Drift == nil || info.Drift.Conflicts != 2 || info.Drift.Resolutions != 3 {
		t.Fatalf("conflict write outcome lost resolution count: %+v", info.Drift)
	}
}
