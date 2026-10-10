package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gens"
	"github.com/zzjcool/homer-cli/internal/web"
)

// stagedUIStub is a machine with a pi conflict plus a recorded decision
// channel: it answers resolve-record calls and reports the pending count in
// drift so the console card can render the 待执行决定 chip.
type stagedUIStub struct {
	mu           sync.Mutex
	pullCount    int
	pushCount    int
	recordCalls  []web.ResolveRecordRequest
	clearCalls   []web.ResolveRecordRequest
	pendingCount int
	listCalls    int
}

func (s *stagedUIStub) ListAgents() []web.AgentInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	return []web.AgentInfo{{
		AgentID:  "box",
		Hostname: "box",
		LastSeen: time.Now(),
		Drift: &web.AgentDrift{
			Conflicts:   2,
			Resolutions: s.pendingCount,
		},
	}}
}

func (s *stagedUIStub) AgentStatus(context.Context, string) (json.RawMessage, error) {
	return json.RawMessage(`{"adapters":[{"id":"pi","conflicts":2}],"errors":[],` +
		`"resolutions":[{"adapter":"pi","choice":"center","recordedAt":"2026-10-09T00:00:00Z","generationAtRecord":1}]}`), nil
}

func (s *stagedUIStub) AgentDiff(context.Context, string, web.DiffParams) (string, error) {
	return "diff", nil
}

func (s *stagedUIStub) AgentPush(context.Context, string, bool, web.SyncScope) (json.RawMessage, error) {
	s.mu.Lock()
	s.pushCount++
	s.mu.Unlock()
	return json.RawMessage(`{"ok":true,"status":"pushed"}`), nil
}

func (s *stagedUIStub) AgentPull(context.Context, string, bool, web.SyncScope) (json.RawMessage, error) {
	s.mu.Lock()
	s.pullCount++
	s.mu.Unlock()
	return json.RawMessage(`{"ok":true,"status":"applied","applied":{"written":[],"deleted":[],"conflicts":[]}}`), nil
}

func (s *stagedUIStub) AgentResolveRecord(_ context.Context, _ string, req web.ResolveRecordRequest) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch req.Action {
	case "record":
		s.recordCalls = append(s.recordCalls, req)
		s.pendingCount = len(req.Adapters)
		recorded := make([]string, 0, len(req.Adapters))
		for _, id := range req.Adapters {
			recorded = append(recorded, fmt.Sprintf(`{"adapter":%q,"choice":%q,"recordedAt":"2026-10-09T00:00:00Z","generationAtRecord":%d}`, id, req.Choice, req.CenterGeneration))
		}
		return json.RawMessage(`{"ok":true,"action":"record","entries":[],"recorded":[` + strings.Join(recorded, ",") + `],"pending":` + fmt.Sprint(s.pendingCount) + `,"errors":[]}`), nil
	case "clear":
		s.clearCalls = append(s.clearCalls, req)
		s.pendingCount = 0
		return json.RawMessage(`{"ok":true,"action":"clear","entries":[],"removed":[{"adapter":"pi","choice":"center","recordedAt":"2026-10-09T00:00:00Z","generationAtRecord":1}],"pending":0,"errors":[]}`), nil
	default:
		return json.RawMessage(`{"ok":true,"action":"list","entries":[{"adapter":"pi","choice":"center","recordedAt":"2026-10-09T00:00:00Z","generationAtRecord":1}],"pending":1,"errors":[]}`), nil
	}
}

func (s *stagedUIStub) RemoveAgent(string) bool { return false }

// TestStagedResolutionUI drives the browser through the staged flow:
// record-only button (no pull/push), the dispatch-dialog decision summary,
// the card chip, and the undo button.
func TestStagedResolutionUI(t *testing.T) {
	if _, err := chromiumExecutable(t); err != nil {
		t.Skipf("chromium is not installed: %v", err)
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	homer := filepath.Join(root, ".homer")
	paths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return homer
		}
		return ""
	})
	if err := core.SaveConfig(paths, core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := gens.New(homer).Publish(map[string]map[string]string{
		"pi": {"settings/settings.json": "x\n"},
	}, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	stub := &stagedUIStub{pendingCount: 1}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.NewServer(web.ServeOptions{Addr: "127.0.0.1:0", HomerHome: homer, Agents: stub})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: server.Handler()}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })

	browser, cancel := newKeyBrowser(t)
	defer cancel()
	ctx, stop := context.WithTimeout(browser, 90*time.Second)
	defer stop()

	admin := "browser-admin-pw"
	err = chromedp.Run(ctx,
		chromedp.Navigate("http://"+listener.Addr().String()+"/"),
		chromedp.WaitVisible(`#in-setup-pw`, chromedp.ByQuery),
		chromedp.SendKeys(`#in-setup-pw`, admin, chromedp.ByQuery),
		chromedp.SendKeys(`#in-setup-pw2`, admin, chromedp.ByQuery),
		chromedp.Click(`#btn-setup`, chromedp.ByQuery),
		// The card chip renders the pending decision count.
		chromedp.WaitVisible(`//div[@id="agent-list"]//span[contains(text(),"待执行决定")]`, chromedp.BySearch),
		chromedp.Click(`//div[@id="agent-list"]//button[normalize-space()="处理冲突"]`, chromedp.BySearch),
		chromedp.WaitVisible(`#confirm-choices input[type=radio][data-resolution-adapter="pi"]`, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("open resolve dialog: %v\n%s", err, browserText(ctx))
	}

	// Pick center for pi and click 记录决定 (OK button). Recording must not
	// pull or push anything.
	err = chromedp.Run(ctx,
		chromedp.Click(`#confirm-choices input[type=radio][data-resolution-adapter="pi"][value="center"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`//button[@id="btn-confirm-ok" and normalize-space()="记录决定"]`, chromedp.BySearch),
		chromedp.Click(`#btn-confirm-ok`, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("record decision: %v\n%s", err, browserText(ctx))
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		stub.mu.Lock()
		calls, pulls, pushes := len(stub.recordCalls), stub.pullCount, stub.pushCount
		stub.mu.Unlock()
		if calls == 1 && pulls == 0 && pushes == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("record-only must not write: records=%d pulls=%d pushes=%d\n%s", calls, pulls, pushes, browserText(ctx))
		}
		time.Sleep(100 * time.Millisecond)
	}
	stub.mu.Lock()
	first := stub.recordCalls[0]
	stub.mu.Unlock()
	if first.Choice != "center" || len(first.Adapters) != 1 || first.Adapters[0] != "pi" {
		t.Fatalf("record call = %+v", first)
	}

	// The dispatch dialog now summarizes the pending decision — tick pi so
	// the decision rides with this dispatch. The dispatch button waits in a
	// bounded loop so a failure reports the page state instead of a bare
	// timeout (chromedp's WaitVisible cannot dump the DOM once the context
	// it runs in has expired).
	dispatchReady := false
	deadlineDispatch := time.Now().Add(60 * time.Second)
	var lastPage string
	for !dispatchReady && time.Now().Before(deadlineDispatch) {
		if err := chromedp.Run(ctx,
			chromedp.Evaluate(`(() => { const b = [...document.querySelectorAll('#agent-list button')].find(x => x.textContent.trim() === '下发'); return b ? 'ready' : 'waiting'; })()`, &lastPage),
		); err != nil {
			t.Fatalf("dispatch-button probe: %v\npage: %s", err, lastPage)
		}
		dispatchReady = lastPage == "ready"
		if !dispatchReady {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if !dispatchReady {
		var pageText string
		_ = chromedp.Run(ctx, chromedp.Evaluate(`document.body ? document.body.innerText.slice(0, 1200) : "no body"`, &pageText))
		t.Fatalf("dispatch button never appeared\nlast probe: %s\npage: %s", lastPage, pageText)
	}
	// The click can only land when the full-screen busy veil is gone; if it
	// is still up, clicking the dispatch button hits the veil and the dialog
	// never opens (this was the flaky failure).
	waitBusyGone := false
	for !waitBusyGone && time.Now().Before(deadlineDispatch) {
		var busy string
		if err := chromedp.Run(ctx, chromedp.Evaluate(`document.getElementById('busy').classList.contains('hidden') ? 'gone' : 'visible'`, &busy)); err != nil {
			t.Fatalf("busy probe: %v", err)
		}
		waitBusyGone = busy == "gone"
		if !waitBusyGone {
			time.Sleep(300 * time.Millisecond)
		}
	}
	if !waitBusyGone {
		t.Fatal("busy veil never lifted after recording the decision")
	}
	err = chromedp.Run(ctx,
		chromedp.Click(`//div[@id="agent-list"]//button[normalize-space()="下发"]`, chromedp.BySearch),
		chromedp.WaitVisible(`#confirm-choices input[data-adapter="pi"]`, chromedp.ByQuery),
		chromedp.Evaluate(`(() => { const i = document.querySelector('#confirm-choices input[data-adapter="pi"]'); i.checked = true; i.dispatchEvent(new Event('change', {bubbles:true})); return 'ok'; })()`, nil),
	)
	if err != nil {
		t.Fatalf("open dispatch dialog: %v\n%s", err, browserText(ctx))
	}
	var summary string
	err = chromedp.Run(ctx,
		chromedp.WaitVisible(`#confirm-resolutions`, chromedp.ByQuery),
		chromedp.Evaluate(`document.getElementById("confirm-resolutions").textContent`, &summary),
	)
	if err != nil {
		t.Fatalf("read decision summary: %v\n%s", err, browserText(ctx))
	}
	if !strings.Contains(summary, "已解决的决定") || !strings.Contains(summary, "以中心为准") {
		t.Fatalf("dispatch summary must list the decision: %q", summary)
	}

	// 撤销决定 clears the record again. Close the dispatch dialog with its
	// cancel button, reopen 处理冲突, then use the row's undo button.
	err = chromedp.Run(ctx,
		chromedp.Click(`#btn-confirm-cancel`, chromedp.ByQuery),
		chromedp.WaitVisible(`//div[@id="agent-list"]//button[normalize-space()="处理冲突"]`, chromedp.BySearch),
		chromedp.Click(`//div[@id="agent-list"]//button[normalize-space()="处理冲突"]`, chromedp.BySearch),
		chromedp.WaitVisible(`//button[normalize-space()="撤销决定"]`, chromedp.BySearch),
		chromedp.Click(`//button[normalize-space()="撤销决定"]`, chromedp.BySearch),
	)
	if err != nil {
		t.Fatalf("undo decision: %v\n%s", err, browserText(ctx))
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		stub.mu.Lock()
		clears := len(stub.clearCalls)
		stub.mu.Unlock()
		if clears == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("undo never called: clears=%d\n%s", clears, browserText(ctx))
		}
		time.Sleep(100 * time.Millisecond)
	}
}
