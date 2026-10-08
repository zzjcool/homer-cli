package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/zzjcool/homer-cli/internal/adapter/pi"
	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/web"
)

type collectUIStub struct {
	mu          sync.Mutex
	agents      []web.AgentInfo
	events      []web.InspectEvent
	result      web.InspectResult
	started     chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	inspectErr  error
	eventDelay  time.Duration
	inspectCall int
}

func (s *collectUIStub) ListAgents() []web.AgentInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]web.AgentInfo(nil), s.agents...)
}

func (s *collectUIStub) AgentStatus(context.Context, string) (json.RawMessage, error) {
	return json.RawMessage(`{"adapters":[],"errors":[]}`), nil
}

func (s *collectUIStub) AgentDiff(context.Context, string, web.DiffParams) (string, error) {
	return "", nil
}

func (s *collectUIStub) AgentPush(context.Context, string, bool, web.SyncScope) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true,"status":"pushed"}`), nil
}

func (s *collectUIStub) AgentPull(context.Context, string, bool, web.SyncScope) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true,"status":"applied"}`), nil
}

func (s *collectUIStub) RemoveAgent(string) bool { return false }

func (s *collectUIStub) AgentInspect(ctx context.Context, _ string, _ web.InspectParams, onEvent func(web.InspectEvent)) (web.InspectResult, error) {
	s.mu.Lock()
	s.inspectCall++
	s.mu.Unlock()
	if s.started != nil {
		s.startOnce.Do(func() { close(s.started) })
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return web.InspectResult{}, ctx.Err()
		}
	}
	if s.inspectErr != nil {
		return web.InspectResult{}, s.inspectErr
	}
	for index, event := range s.events {
		if ctx.Err() != nil {
			return web.InspectResult{}, ctx.Err()
		}
		if onEvent != nil {
			onEvent(event)
		}
		if s.eventDelay > 0 && index+1 < len(s.events) {
			timer := time.NewTimer(s.eventDelay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return web.InspectResult{}, ctx.Err()
			}
		}
	}
	return s.result, nil
}

func (s *collectUIStub) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inspectCall
}

type collectUIEnrollment struct{}

func (collectUIEnrollment) Mint(time.Duration) (string, error) { return "hr_collect-ui-code", nil }
func (collectUIEnrollment) Revoke(string) bool                 { return false }
func (collectUIEnrollment) ValidCode(code string) bool         { return code == "hr_collect-ui-code" }

type bufferedCollectResponse struct {
	header http.Header
	status int
	body   strings.Builder
}

func newBufferedCollectResponse() *bufferedCollectResponse {
	return &bufferedCollectResponse{header: make(http.Header)}
}

func (w *bufferedCollectResponse) Header() http.Header { return w.header }
func (w *bufferedCollectResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *bufferedCollectResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}
func (w *bufferedCollectResponse) Flush() {}
func (w *bufferedCollectResponse) commit(target http.ResponseWriter) {
	for name, values := range w.header {
		for _, value := range values {
			target.Header().Add(name, value)
		}
	}
	if w.status != 0 {
		target.WriteHeader(w.status)
	}
	_, _ = target.Write([]byte(w.body.String()))
}

func newCollectTestHandler(next http.Handler, bufferStream bool) http.Handler {
	if !bufferStream {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sync/choices" || r.URL.Query().Get("stream") != "1" {
			next.ServeHTTP(w, r)
			return
		}
		buffer := newBufferedCollectResponse()
		next.ServeHTTP(buffer, r)
		buffer.commit(w)
	})
}

func runCollectBrowser(t *testing.T, stub *collectUIStub, bufferStream bool) (context.Context, context.CancelFunc) {
	t.Helper()
	chromium, err := chromiumExecutable(t)
	if err != nil {
		t.Skipf("chromium is not installed: %v", err)
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	homerHome := filepath.Join(root, ".homer")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.NewServer(web.ServeOptions{Addr: "127.0.0.1:0", HomerHome: homerHome, Agents: stub, Enrollment: collectUIEnrollment{}})
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: newCollectTestHandler(server.Handler(), bufferStream)}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromium),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	browser, browserCancel := chromedp.NewContext(allocCtx)
	ctx, stop := context.WithTimeout(browser, 45*time.Second)
	cancel := func() {
		stop()
		browserCancel()
		allocCancel()
	}
	admin := "collect-ui-admin-password"
	if err := chromedp.Run(ctx,
		chromedp.Navigate("http://"+listener.Addr().String()+"/"),
		chromedp.WaitVisible(`#in-setup-pw`, chromedp.ByQuery),
		chromedp.SendKeys(`#in-setup-pw`, admin, chromedp.ByQuery),
		chromedp.SendKeys(`#in-setup-pw2`, admin, chromedp.ByQuery),
		chromedp.Click(`#btn-setup`, chromedp.ByQuery),
		chromedp.WaitVisible(`#agent-list .agent-card`, chromedp.ByQuery),
	); err != nil {
		cancel()
		t.Fatalf("start collect UI: %v\n%s", err, browserText(ctx))
	}
	return ctx, cancel
}

func collectUIButtonAndWait(t *testing.T, ctx context.Context, selector string) {
	t.Helper()
	if err := chromedp.Run(ctx, chromedp.Click(selector, chromedp.ByQuery)); err != nil {
		t.Fatalf("click %s: %v\n%s", selector, err, browserText(ctx))
	}
}

func collectUIValue(t *testing.T, ctx context.Context, expression string) string {
	t.Helper()
	var value string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`String((`+expression+`) ?? "")`, &value)); err != nil {
		t.Fatalf("evaluate %s: %v\n%s", expression, err, browserText(ctx))
	}
	return value
}

func TestCollectDialogStream(t *testing.T) {
	streamCollect := func(t *testing.T, buffered bool) string {
		t.Helper()
		one := &commands.StatusAdapterReport{ID: "pi", Push: 1}
		two := &commands.StatusAdapterReport{ID: "opencode", Push: 1}
		present := map[string]bool{pi.SecretDestination("auth.json"): true}
		stub := &collectUIStub{
			agents: []web.AgentInfo{{AgentID: "collect-box", Hostname: "collect-box", LastSeen: time.Now()}},
			events: []web.InspectEvent{
				{Stage: "adapter", Done: 1, Total: 2, Adapter: one},
				{Stage: "credentials", Present: present},
				{Stage: "adapter", Done: 2, Total: 2, Adapter: two},
			},
			result: web.InspectResult{
				Status:  commands.StatusReport{Adapters: []commands.StatusAdapterReport{{ID: "pi", Push: 1}, {ID: "opencode", Push: 1}}},
				Present: present,
			},
			eventDelay: 350 * time.Millisecond,
		}
		ctx, cancel := runCollectBrowser(t, stub, buffered)
		defer cancel()
		collectUIButtonAndWait(t, ctx, `#agent-list .agent-card button[title="选择要存入中心的适配器"]`)
		if buffered {
			if err := chromedp.Run(ctx, chromedp.Poll(`document.getElementById("dlg-confirm").open && document.querySelectorAll("#confirm-choices input[data-adapter]").length === 2 && !document.getElementById("btn-confirm-ok").disabled && document.getElementById("confirm-desc").textContent.includes("只会把勾选的适配器写入中心")`, nil, chromedp.WithPollingInterval(50*time.Millisecond))); err != nil {
				t.Fatalf("buffered collect dialog completion: %v\n%s", err, browserText(ctx))
			}
		} else {
			if err := chromedp.Run(ctx, chromedp.Poll(`document.getElementById("dlg-confirm").open && document.querySelectorAll("#confirm-choices input[data-adapter]").length === 1 && document.getElementById("confirm-note").textContent.includes("已读取 1/2")`, nil, chromedp.WithPollingInterval(50*time.Millisecond))); err != nil {
				t.Fatalf("first incremental collect row: %v\n%s", err, browserText(ctx))
			}
		}
		if got := collectUIValue(t, ctx, `([...document.querySelectorAll('#confirm-choices input[data-adapter]')].map(input => input.dataset.adapter).sort().join(','))`); got != "opencode,pi" && got != "pi" {
			t.Fatalf("collect adapter rows before uncheck = %q", got)
		}
		// In the incremental case this happens between adapter and credentials
		// events; in the buffered case it happens after all rows have arrived.
		if err := chromedp.Run(ctx, chromedp.Evaluate(`(() => { const input = document.querySelector('#confirm-choices input[data-adapter="pi"]'); input.checked = false; input.dispatchEvent(new Event('change', {bubbles:true})); })()`, nil)); err != nil {
			t.Fatal(err)
		}
		if buffered {
			if err := chromedp.Run(ctx, chromedp.Poll(`!document.getElementById("btn-confirm-ok").disabled && document.getElementById("confirm-desc").textContent.includes("只会把勾选的适配器写入中心")`, nil, chromedp.WithPollingInterval(50*time.Millisecond))); err != nil {
				t.Fatalf("buffered final reconciliation: %v\n%s", err, browserText(ctx))
			}
		} else {
			if err := chromedp.Run(ctx, chromedp.Poll(`document.getElementById("confirm-desc").textContent.includes("只会把勾选的适配器写入中心") && document.querySelectorAll("#confirm-choices input[data-adapter]").length === 2`, nil, chromedp.WithPollingInterval(50*time.Millisecond))); err != nil {
				t.Fatalf("incremental final reconciliation: %v\n%s", err, browserText(ctx))
			}
		}
		if got := collectUIValue(t, ctx, `([...document.querySelectorAll('#confirm-choices input[data-adapter]')].map(input => input.checked ? input.dataset.adapter : '').filter(Boolean).join(','))`); got != "opencode" {
			t.Fatalf("final selected rows = %q, want only opencode", got)
		}
		if got := collectUIValue(t, ctx, `document.getElementById('btn-confirm-ok').disabled`); got != "false" {
			t.Fatalf("confirm disabled after final check reconciliation = %s", got)
		}
		if got := collectUIValue(t, ctx, `document.getElementById('confirm-choices').innerText.includes('auth.json')`); got != "true" {
			t.Fatalf("credential enrichment missing after reconciliation")
		}
		if stub.calls() != 1 {
			t.Fatalf("inspect calls=%d, want one stream", stub.calls())
		}
		return collectUIValue(t, ctx, `([...document.querySelectorAll('#confirm-choices input[data-adapter]')].map(input => input.dataset.adapter + ':' + input.checked).sort().join(','))`)
	}
	buffered := streamCollect(t, true)
	incremental := streamCollect(t, false)
	if buffered != incremental {
		t.Fatalf("all-at-once UI state %q differs from incrementally delivered state %q", buffered, incremental)
	}
}

func TestCollectDialogAgentDrops(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	stub := &collectUIStub{
		agents:     []web.AgentInfo{{AgentID: "drop-box", Hostname: "drop-box", LastSeen: time.Now()}},
		started:    started,
		release:    release,
		inspectErr: &web.AgentError{Code: "agent-offline", Status: http.StatusServiceUnavailable, Err: errors.New("agent dropped the stream")},
	}
	ctx, cancel := runCollectBrowser(t, stub, false)
	defer cancel()
	collectUIButtonAndWait(t, ctx, `#agent-list .agent-card button[title="选择要存入中心的适配器"]`)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("collect inspect did not start")
	}
	// The RPC remains pending while the dialog opens. Once released, it fails
	// with the simulated live-session disconnect; the browser must show an
	// explicit error instead of staying on "正在读取可选项…".
	close(release)
	if err := chromedp.Run(ctx, chromedp.Poll(`document.getElementById("dlg-confirm").open && document.getElementById("confirm-desc").textContent.includes("读取收取选项失败") && document.getElementById("confirm-err").textContent.includes("没有收到完整") && document.getElementById("confirm-desc").textContent !== "正在读取可选项…"`, nil, chromedp.WithPollingInterval(50*time.Millisecond))); err != nil {
		t.Fatalf("agent drop did not render an explicit failure: %v\n%s", err, browserText(ctx))
	}
	if got := collectUIValue(t, ctx, `document.getElementById('btn-confirm-ok').disabled`); got != "true" {
		t.Fatalf("confirm after interrupted stream is disabled=%s, want true", got)
	}
}

func TestJoinDialog(t *testing.T) {
	stub := &collectUIStub{agents: []web.AgentInfo{{AgentID: "join-box", Hostname: "join-box", LastSeen: time.Now()}}}
	ctx, cancel := runCollectBrowser(t, stub, false)
	defer cancel()
	if err := chromedp.Run(ctx,
		chromedp.Click(`#btn-join-top`, chromedp.ByQuery),
		chromedp.Poll(`document.getElementById("dlg-join").open && document.getElementById("join-cmd").value.includes("curl ") && document.querySelectorAll("#dlg-join input[readonly]").length === 1`, nil, chromedp.WithPollingInterval(50*time.Millisecond)),
	); err != nil {
		t.Fatalf("open join dialog: %v\n%s", err, browserText(ctx))
	}
	var state struct {
		Inputs      int    `json:"inputs"`
		Command     string `json:"command"`
		ListenNodes int    `json:"listenNodes"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`({inputs: document.querySelectorAll('#dlg-join input[readonly]').length, command: document.getElementById('join-cmd').value, listenNodes: document.querySelectorAll('#join-listen,#listenCommand,[id*=join-listen],[class*=join-listen]').length})`, &state)); err != nil {
		t.Fatal(err)
	}
	if state.Inputs != 1 || state.ListenNodes != 0 {
		t.Fatalf("join dialog = %+v; want exactly one command and no listen control", state)
	}
	if strings.Contains(state.Command, "--listen") || strings.Contains(state.Command, "--connect") {
		t.Fatalf("join command contains a removed agent flag: %q", state.Command)
	}
	if !strings.Contains(state.Command, "hr_") {
		t.Fatalf("join command has no one-time code: %q", state.Command)
	}
	collectUIButtonAndWait(t, ctx, `#dlg-join button[value="ok"]`)
}
