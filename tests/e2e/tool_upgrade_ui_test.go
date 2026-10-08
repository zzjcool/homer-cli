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

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/toolctl"
	"github.com/zzjcool/homer-cli/internal/web"
)

// toolUIStub is a hub with four machines and a pretend way to upgrade the
// programs on them. It sits behind the real web handlers, so the console talks
// to the real routes and the real failure envelope.
type toolUIStub struct {
	mu     sync.Mutex
	agents []web.AgentInfo
	calls  [][2]string
	fail   map[string]bool
	// installs is the version a successful upgrade ends on.
	installs string
}

func uiTool(id, label, version string, upgradable bool) web.AgentTool {
	return web.AgentTool{Status: toolctl.Status{ID: id, Adapter: id, Label: label, Version: version, Upgradable: upgradable}}
}

func newToolUIStub() *toolUIStub {
	broken := uiTool("opencode", "opencode", "", true)
	broken.Error = "读不出版本：输出里没有版本号"
	now := time.Now()
	return &toolUIStub{
		fail:     map[string]bool{},
		installs: "0.90.2",
		agents: []web.AgentInfo{
			{AgentID: "alpha", Hostname: "alpha", LastSeen: now,
				Tools: []web.AgentTool{uiTool("pi", "pi", "0.80.0", true), uiTool("herdr", "herdr", "0.9.1", true), broken}},
			{AgentID: "beta", Hostname: "beta", LastSeen: now,
				Tools: []web.AgentTool{uiTool("pi", "pi", "0.90.2", true), uiTool("vscode", "VS Code", "1.100.0", false)}},
			{AgentID: "gamma", Hostname: "gamma", LastSeen: now.Add(-10 * time.Minute), Stale: true,
				Tools: []web.AgentTool{uiTool("pi", "pi", "0.70.0", true)}},
			{AgentID: "delta", Hostname: "delta", LastSeen: now,
				Tools: []web.AgentTool{uiTool("vscode", "VS Code", "1.90.0", false)}},
		},
	}
}

func (s *toolUIStub) ListAgents() []web.AgentInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]web.AgentInfo, len(s.agents))
	for i, agent := range s.agents {
		agent.Tools = append([]web.AgentTool(nil), agent.Tools...)
		out[i] = agent
	}
	return out
}

func (s *toolUIStub) setVersion(agentID, tool, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.agents {
		if s.agents[i].AgentID != agentID {
			continue
		}
		for j := range s.agents[i].Tools {
			if s.agents[i].Tools[j].ID == tool {
				s.agents[i].Tools[j].Version = version
			}
		}
	}
}

func (s *toolUIStub) upgradeCalls() [][2]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][2]string(nil), s.calls...)
}

func (s *toolUIStub) AgentToolUpgrade(_ context.Context, agentID, tool string) (json.RawMessage, error) {
	s.mu.Lock()
	s.calls = append(s.calls, [2]string{agentID, tool})
	fail := s.fail[agentID+"/"+tool]
	installs := s.installs
	var before string
	for _, agent := range s.agents {
		if agent.AgentID != agentID {
			continue
		}
		for _, t := range agent.Tools {
			if t.ID == tool {
				before = t.Version
			}
		}
	}
	s.mu.Unlock()
	if fail {
		return json.Marshal(toolctl.UpgradeResult{
			Status: "failed", Tool: tool, Label: tool, Before: before,
			Note:   tool + " 升级没有成功：exit status 1",
			Output: "npm ERR! network timeout\nnpm ERR! A complete log of this run can be found in the cache",
			Manual: "curl -fsSL https://example.test/install.sh | sh",
		})
	}
	s.setVersion(agentID, tool, installs)
	return json.Marshal(toolctl.UpgradeResult{
		OK: true, Status: "upgraded", Tool: tool, Label: tool, Before: before, After: installs,
		Note: fmt.Sprintf("%s %s → %s", tool, before, installs),
	})
}

func (s *toolUIStub) AgentStatus(context.Context, string) (json.RawMessage, error) {
	return json.RawMessage(`{"adapters":[],"errors":[]}`), nil
}
func (s *toolUIStub) AgentDiff(context.Context, string, web.DiffParams) (string, error) {
	return "", nil
}
func (s *toolUIStub) AgentPush(context.Context, string, bool, web.SyncScope) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true,"status":"pushed"}`), nil
}
func (s *toolUIStub) AgentPull(context.Context, string, bool, web.SyncScope) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true,"status":"applied"}`), nil
}
func (s *toolUIStub) RemoveAgent(string) bool { return false }

// toolUI drives the console in a real browser.
type toolUI struct {
	t   *testing.T
	ctx context.Context
}

func (u *toolUI) eval(js string, out any) {
	u.t.Helper()
	if err := chromedp.Run(u.ctx, chromedp.Evaluate(js, out)); err != nil {
		u.t.Fatalf("eval %s: %v\npage: %s", js, err, browserText(u.ctx))
	}
}

func (u *toolUI) text(js string) string {
	u.t.Helper()
	var out string
	u.eval(`(() => { const v = (`+js+`); return v == null ? "" : String(v); })()`, &out)
	return out
}

func (u *toolUI) wait(expr string, timeout time.Duration) {
	u.t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		var ready bool
		err := chromedp.Run(u.ctx, chromedp.Evaluate(`!!(`+expr+`)`, &ready))
		if err == nil && ready {
			return
		}
		if err != nil {
			last = err.Error()
		}
		time.Sleep(100 * time.Millisecond)
	}
	u.t.Fatalf("timed out waiting for %s (%s)\npage: %s", expr, last, browserText(u.ctx))
}

// card is the JS expression for one machine's card.
func card(host string) string {
	return fmt.Sprintf(`[...document.querySelectorAll('#agent-list .agent-card')].find(c => (c.querySelector('.hostname')||{}).textContent === %q)`, host)
}

func (u *toolUI) chips(host string) string {
	u.t.Helper()
	return u.text(`(() => { const c = ` + card(host) + `; const row = c && c.querySelector('.agent-tools'); return row ? row.innerText : '' })()`)
}

func (u *toolUI) behindCount(host string) string {
	u.t.Helper()
	return u.text(`(() => { const c = ` + card(host) + `; return c ? c.querySelectorAll('.tool-chip.behind').length : -1 })()`)
}

func (u *toolUI) click(js string) {
	u.t.Helper()
	got := u.text(`(() => { const el = ` + js + `; if (!el) return 'missing'; if (el.disabled) return 'disabled'; el.click(); return 'ok' })()`)
	if got != "ok" {
		u.t.Fatalf("click %s = %s\npage: %s", js, got, browserText(u.ctx))
	}
}

func TestToolUpgradeConsole(t *testing.T) {
	if _, err := chromiumExecutable(t); err != nil {
		t.Skipf("chromium is not installed: %v", err)
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	homerHome := filepath.Join(root, ".homer")
	paths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return homerHome
		}
		return ""
	})
	if err := core.SaveConfig(paths, core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{}}); err != nil {
		t.Fatal(err)
	}

	stub := newToolUIStub()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.NewServer(web.ServeOptions{Addr: "127.0.0.1:0", HomerHome: homerHome, Agents: stub})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: server.Handler()}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })
	pageURL := "http://" + listener.Addr().String() + "/"

	browser, cancel := newKeyBrowser(t)
	defer cancel()
	ctx, stop := context.WithTimeout(browser, 120*time.Second)
	defer stop()
	ui := &toolUI{t: t, ctx: ctx}

	// Any uncaught error on the page fails the test: a console that throws
	// while rendering the new rows would otherwise look merely "empty".
	var errMu sync.Mutex
	var pageErrors []string
	chromedp.ListenTarget(ctx, func(ev any) {
		if exception, ok := ev.(*runtime.EventExceptionThrown); ok {
			errMu.Lock()
			pageErrors = append(pageErrors, exception.ExceptionDetails.Error())
			errMu.Unlock()
		}
	})
	t.Cleanup(func() {
		errMu.Lock()
		defer errMu.Unlock()
		if len(pageErrors) > 0 {
			t.Errorf("the console threw: %v", pageErrors)
		}
	})

	// The page answers confirm() and records what it was asked, and clipboard
	// writes land in a list instead of needing a permission prompt.
	const initScript = `
		window.__confirms = [];
		window.confirm = (m) => { window.__confirms.push(String(m)); return true; };
		window.__copied = [];
		window.prompt = () => null;
		Object.defineProperty(navigator, 'clipboard', { configurable: true,
			value: { writeText: (t) => { window.__copied.push(String(t)); return Promise.resolve(); } } });
	`
	admin := "browser-admin-pw"
	err = chromedp.Run(ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(initScript).Do(ctx)
			return err
		}),
		chromedp.Navigate(pageURL),
		chromedp.WaitVisible(`#in-setup-pw`, chromedp.ByQuery),
		chromedp.SendKeys(`#in-setup-pw`, admin, chromedp.ByQuery),
		chromedp.SendKeys(`#in-setup-pw2`, admin, chromedp.ByQuery),
		chromedp.Click(`#btn-setup`, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("open console: %v\n%s", err, browserText(ctx))
	}
	ui.wait(`document.querySelectorAll('#agent-list .agent-card').length === 4`, 20*time.Second)

	// 1. Each machine's card lists its applications with versions, and marks
	// the ones that have fallen behind the fleet.
	alpha := ui.chips("alpha")
	for _, want := range []string{"应用", "pi", "0.80.0", "最新 0.90.2", "herdr", "0.9.1", "opencode", "版本未知"} {
		if !strings.Contains(alpha, want) {
			t.Fatalf("alpha's applications = %q, missing %q", alpha, want)
		}
	}
	if got := ui.behindCount("alpha"); got != "1" {
		t.Fatalf("alpha has %s behind applications, want only pi", got)
	}
	if got := ui.text(`(() => { const c = ` + card("alpha") + `; return [...c.querySelectorAll('.tool-chip')].find(x => x.innerText.includes('opencode')).title })()`); !strings.Contains(got, "读不出版本") {
		t.Fatalf("an unreadable version should say why on hover, title = %q", got)
	}
	if got := ui.behindCount("beta"); got != "0" {
		t.Fatalf("beta runs the newest pi but has %s behind applications", got)
	}
	if beta := ui.chips("beta"); !strings.Contains(beta, "VS Code") || !strings.Contains(beta, "1.100.0") {
		t.Fatalf("beta's applications = %q", beta)
	}
	// An application this machine cannot upgrade itself gets no button.
	if got := ui.text(`(() => { const c = ` + card("delta") + `; return c.querySelectorAll('.agent-tools button').length })()`); got != "0" {
		t.Fatalf("delta's VS Code cannot be upgraded here but has %s buttons", got)
	}
	if got := ui.behindCount("delta"); got != "1" {
		t.Fatalf("delta's older VS Code should be marked behind, got %s", got)
	}
	// An offline machine cannot be reached: marked behind, button disabled.
	if got := ui.behindCount("gamma"); got != "1" {
		t.Fatalf("gamma's old pi should be marked behind, got %s", got)
	}
	if got := ui.text(`(() => { const b = ` + card("gamma") + `.querySelector('.agent-tools button'); return b ? b.disabled : 'missing' })()`); got != "true" {
		t.Fatalf("gamma is offline; its upgrade button disabled = %s", got)
	}
	ui.wait(`[...document.querySelectorAll('#agent-list button')].some(b => b.textContent === '全部升级应用')`, 5*time.Second)

	// 2. One click upgrades one application on one machine.
	ui.click(card("alpha") + `.querySelector('.tool-chip.behind button')`)
	ui.wait(`document.getElementById('toast').textContent.includes('pi 0.80.0 → 0.90.2')`, 10*time.Second)
	if calls := stub.upgradeCalls(); len(calls) != 1 || calls[0] != [2]string{"alpha", "pi"} {
		t.Fatalf("upgrade calls = %v, want only alpha/pi", calls)
	}
	asked := ui.text(`window.__confirms[0]`)
	for _, want := range []string{"alpha", "pi", "0.80.0", "0.90.2"} {
		if !strings.Contains(asked, want) {
			t.Fatalf("the confirmation %q does not mention %q", asked, want)
		}
	}
	ui.wait(`(() => { const c = `+card("alpha")+`; return c && c.querySelector('.agent-tools').innerText.includes('0.90.2') && c.querySelectorAll('.tool-chip.behind').length === 0 })()`, 10*time.Second)
	// Nothing reachable and upgradable is behind any more.
	ui.wait(`![...document.querySelectorAll('#agent-list button')].some(b => b.textContent === '全部升级应用')`, 5*time.Second)

	// 3. The detail view lists every application with its state. A program
	// that must be updated through the machine's package manager says so.
	ui.click(card("delta") + `.querySelector('button[title="系统、资源、网卡"]')`)
	ui.wait(`document.getElementById('dlg-view').open && document.getElementById('view-body').innerText.includes('应用版本')`, 5*time.Second)
	body := ui.text(`document.getElementById('view-body').innerText`)
	for _, want := range []string{"VS Code", "1.90.0", "落后，最新 1.100.0", "要用这台机器的包管理器更新"} {
		if !strings.Contains(body, want) {
			t.Fatalf("delta's detail = %q, missing %q", body, want)
		}
	}
	if got := ui.text(`document.querySelectorAll('#view-body .tool-row button').length`); got != "0" {
		t.Fatalf("a program that cannot be upgraded here has %s buttons in the detail", got)
	}
	ui.eval(`document.getElementById('dlg-view').close()`, nil)

	// 4. A failed upgrade says what happened, shows the end of the output and
	// offers the official installer.
	stub.mu.Lock()
	stub.fail["alpha/herdr"] = true
	stub.mu.Unlock()
	ui.click(card("alpha") + `.querySelector('button[title="系统、资源、网卡"]')`)
	ui.wait(`document.getElementById('dlg-view').open`, 5*time.Second)
	if state := ui.text(`document.getElementById('view-body').innerText`); !strings.Contains(state, "已是已知最新版本") {
		t.Fatalf("alpha's detail = %q; an application level with the fleet should say so", state)
	}
	ui.click(`[...document.querySelectorAll('#view-body .tool-row')].find(r => r.innerText.includes('herdr')).querySelector('button')`)
	ui.wait(`document.getElementById('alert-slot').innerText.includes('有应用没有升级成功')`, 10*time.Second)
	if open := ui.text(`document.getElementById('dlg-view').open`); open != "false" {
		t.Fatalf("the detail dialog stayed open over the progress and the result")
	}
	alert := ui.text(`document.getElementById('alert-slot').innerText`)
	for _, want := range []string{"herdr 升级没有成功：exit status 1", "它的输出", "npm ERR! network timeout", "curl -fsSL https://example.test/install.sh | sh", "复制安装命令"} {
		if !strings.Contains(alert, want) {
			t.Fatalf("failure alert = %q, missing %q", alert, want)
		}
	}
	ui.click(`[...document.querySelectorAll('#alert-slot button')].find(b => b.textContent === '复制安装命令')`)
	ui.wait(`window.__copied.length === 1`, 5*time.Second)
	if got := ui.text(`window.__copied[0]`); got != "curl -fsSL https://example.test/install.sh | sh" {
		t.Fatalf("copied %q", got)
	}
	if got := ui.text(`(() => { const c = ` + card("alpha") + `; return c.querySelector('.agent-tools').innerText })()`); !strings.Contains(got, "0.9.1") {
		t.Fatalf("a failed upgrade changed herdr's version on the card: %q", got)
	}

	// 5. "全部升级应用" upgrades every reachable application that is behind.
	stub.setVersion("beta", "pi", "0.88.0")
	ui.click(`document.getElementById('btn-refresh')`)
	ui.wait(`[...document.querySelectorAll('#agent-list button')].some(b => b.textContent === '全部升级应用')`, 10*time.Second)
	before := len(stub.upgradeCalls())
	ui.click(`[...document.querySelectorAll('#agent-list button')].find(b => b.textContent === '全部升级应用')`)
	ui.wait(`document.getElementById('toast').textContent.includes('pi 0.88.0 → 0.90.2')`, 10*time.Second)
	calls := stub.upgradeCalls()
	if len(calls) != before+1 || calls[len(calls)-1] != [2]string{"beta", "pi"} {
		t.Fatalf("upgrade calls = %v, want one more for beta/pi (and none for the offline or non-upgradable ones)", calls)
	}
	if confirmed := ui.text(`window.__confirms[window.__confirms.length - 1]`); !strings.Contains(confirmed, "1 个") {
		t.Fatalf("the bulk confirmation %q does not say how many", confirmed)
	}
	ui.wait(`![...document.querySelectorAll('#agent-list button')].some(b => b.textContent === '全部升级应用')`, 10*time.Second)
}
