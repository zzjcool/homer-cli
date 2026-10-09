package e2e

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gens"
	"github.com/zzjcool/homer-cli/internal/keyring"
	"github.com/zzjcool/homer-cli/internal/web"
)

// The dispatch dialog is one conversation: pick adapters, then type the
// password in the next step, and stay there until the files are written.
func TestDispatchDialogUnlocksBeforeSend(t *testing.T) {
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
	destination := filepath.Join(root, "models.json")
	if err := os.WriteFile(destination, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const password = "long-password"
	created := keyring.Apply(homer, keyring.Command{Action: "create", ID: "pi", Name: "pi", Password: password, WorkFactor: 14})
	if !created.OK {
		t.Fatalf("create = %#v", created)
	}
	encrypted := keyring.Apply(homer, keyring.Command{Action: "encrypt", ID: "pi", Path: destination, Adapter: "pi", Password: password, WorkFactor: 14})
	if !encrypted.OK {
		t.Fatalf("encrypt = %#v", encrypted)
	}
	if _, err := gens.New(homer).Publish(map[string]map[string]string{
		"pi":      {"settings/settings.json": "x\n"},
		"herdr":   {"config/config.toml": "x\n"},
		"keyring": {"marker.txt": "x\n"},
	}, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}

	stub := &dispatchUIStub{}
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
	pageURL := "http://" + listener.Addr().String() + "/"

	browser, cancel := newKeyBrowser(t)
	defer cancel()
	ctx, stop := context.WithTimeout(browser, 90*time.Second)
	defer stop()

	admin := "browser-admin-pw"
	err = chromedp.Run(ctx,
		chromedp.Navigate(pageURL),
		chromedp.WaitVisible(`#in-setup-pw`, chromedp.ByQuery),
		chromedp.SendKeys(`#in-setup-pw`, admin, chromedp.ByQuery),
		chromedp.SendKeys(`#in-setup-pw2`, admin, chromedp.ByQuery),
		chromedp.Click(`#btn-setup`, chromedp.ByQuery),
		chromedp.WaitVisible(`//div[@id="agent-list"]//button[normalize-space()="下发"]`, chromedp.BySearch),
		chromedp.Click(`//div[@id="agent-list"]//button[normalize-space()="下发"]`, chromedp.BySearch),
		chromedp.WaitVisible(`//button[@id="btn-confirm-ok" and normalize-space()="下一步"]`, chromedp.BySearch),
	)
	if err != nil {
		t.Fatalf("open dispatch: %v\n%s", err, browserText(ctx))
	}
	var first struct {
		Open   bool `json:"open"`
		Hidden bool `json:"hidden"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`({open: document.getElementById("dlg-confirm").open, hidden: document.getElementById("dispatch-unlocks").classList.contains("hidden")})`, &first)); err != nil {
		t.Fatal(err)
	}
	if !first.Open || !first.Hidden {
		t.Fatalf("first step = %+v", first)
	}

	err = chromedp.Run(ctx,
		chromedp.Click(`#btn-confirm-ok`, chromedp.ByQuery),
		chromedp.WaitVisible(`#dispatch-unlocks input`, chromedp.ByQuery),
		chromedp.WaitVisible(`//button[@id="btn-confirm-back" and normalize-space()="上一步"]`, chromedp.BySearch),
	)
	if err != nil {
		t.Fatalf("unlock step: %v\n%s", err, browserText(ctx))
	}
	var step struct {
		Open bool   `json:"open"`
		Desc string `json:"desc"`
		Path bool   `json:"path"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`({open: document.getElementById("dlg-confirm").open, desc: document.getElementById("confirm-desc").textContent, path: document.getElementById("dispatch-unlocks").innerText.includes("models.json")})`, &step)); err != nil {
		t.Fatal(err)
	}
	if !step.Open || step.Desc != "先解开这些密钥，然后才会下发。" || !step.Path {
		t.Fatalf("unlock step = %+v", step)
	}

	err = chromedp.Run(ctx,
		chromedp.SendKeys(`#dispatch-unlocks input`, "wrong-password", chromedp.ByQuery),
		chromedp.Click(`#btn-confirm-ok`, chromedp.ByQuery),
		chromedp.WaitVisible(`#confirm-err`, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("wrong password: %v\n%s", err, browserText(ctx))
	}
	var wrong struct {
		Open bool   `json:"open"`
		Err  string `json:"err"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`({open: document.getElementById("dlg-confirm").open, err: document.getElementById("confirm-err").textContent})`, &wrong)); err != nil {
		t.Fatal(err)
	}
	if !wrong.Open || wrong.Err == "" {
		t.Fatalf("wrong password left the dialog: %+v", wrong)
	}
	pulls, unlocks := stub.counts()
	if pulls != 0 || unlocks != 0 {
		t.Fatalf("wrong password sent the dispatch: pulls=%d unlocks=%d", pulls, unlocks)
	}

	err = chromedp.Run(ctx,
		chromedp.Evaluate(`(() => { const input = document.querySelector("#dispatch-unlocks input"); input.value = "long-password"; input.dispatchEvent(new Event("input", {bubbles:true})); return input.value.length; })()`, nil),
		chromedp.Click(`#btn-confirm-ok`, chromedp.ByQuery),
		chromedp.WaitVisible(`//button[@id="btn-confirm-ok" and normalize-space()="完成"]`, chromedp.BySearch),
	)
	if err != nil {
		t.Fatalf("confirm: %v\n%s", err, browserText(ctx))
	}
	var done struct {
		Open  bool   `json:"open"`
		Desc  string `json:"desc"`
		Alert string `json:"alert"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`({open: document.getElementById("dlg-confirm").open, desc: document.getElementById("confirm-desc").textContent, alert: document.getElementById("alert-slot").innerText})`, &done)); err != nil {
		t.Fatal(err)
	}
	if !done.Open || done.Alert != "" || !strings.Contains(done.Desc, "已下发到") || !strings.Contains(done.Desc, "models.json") {
		t.Fatalf("done = %+v", done)
	}
	pulls, unlocks = stub.counts()
	if pulls != 1 || unlocks != 1 {
		t.Fatalf("dispatch counts pulls=%d unlocks=%d", pulls, unlocks)
	}
}

type dispatchUIStub struct {
	mu       sync.Mutex
	pulls    int
	unlocks  int
	password string
}

func (s *dispatchUIStub) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pulls, s.unlocks
}

func (s *dispatchUIStub) ListAgents() []web.AgentInfo {
	return []web.AgentInfo{{
		AgentID:  "box",
		Hostname: "box",
		LastSeen: time.Now(),
	}}
}

func (s *dispatchUIStub) AgentStatus(context.Context, string) (json.RawMessage, error) {
	return json.RawMessage(`{"adapters":[{"id":"pi","pull":2},{"id":"herdr","pull":1}],"errors":[]}`), nil
}

func (s *dispatchUIStub) AgentDiff(context.Context, string, web.DiffParams) (string, error) {
	return "", nil
}

func (s *dispatchUIStub) AgentPush(context.Context, string, bool, web.SyncScope) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true,"status":"pushed"}`), nil
}

func (s *dispatchUIStub) AgentPull(context.Context, string, bool, web.SyncScope) (json.RawMessage, error) {
	s.mu.Lock()
	s.pulls++
	s.mu.Unlock()
	return json.RawMessage(`{"ok":true,"status":"applied"}`), nil
}

func (s *dispatchUIStub) RemoveAgent(string) bool { return false }

func (s *dispatchUIStub) AgentKey(_ context.Context, _ string, cmd keyring.Command) (json.RawMessage, error) {
	s.mu.Lock()
	s.unlocks++
	s.password = cmd.Password
	s.mu.Unlock()
	return json.Marshal(keyring.Result{OK: true, Status: "unlocked"})
}

// An adapter whose credential file is bound to a key must not be dispatchable
// when the key cannot travel (the center holds no keyring yet). Offering the
// button anyway writes the adapter's plain files and strands the credentials:
// the machine looks synced but cannot log in, and nothing says why.
func TestDispatchBlockedWhenBoundKeyCannotTravel(t *testing.T) {
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
	destination := filepath.Join(root, "auth.json")
	if err := os.WriteFile(destination, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const password = "long-password"
	if r := keyring.Apply(homer, keyring.Command{Action: "create", ID: "pi", Name: "pi", Password: password, WorkFactor: 14}); !r.OK {
		t.Fatalf("create = %#v", r)
	}
	if r := keyring.Apply(homer, keyring.Command{Action: "encrypt", ID: "pi", Path: destination, Adapter: "pi", Password: password, WorkFactor: 14}); !r.OK {
		t.Fatalf("encrypt = %#v", r)
	}
	// The center has pi and herdr content but NO keyring, so the key bound to
	// pi cannot be delivered with it.
	if _, err := gens.New(homer).Publish(map[string]map[string]string{
		"pi":    {"settings/settings.json": "x\n"},
		"herdr": {"config/config.toml": "x\n"},
	}, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}

	stub := &dispatchUIStub{}
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
		chromedp.WaitVisible(`//div[@id="agent-list"]//button[normalize-space()="下发"]`, chromedp.BySearch),
		chromedp.Click(`//div[@id="agent-list"]//button[normalize-space()="下发"]`, chromedp.BySearch),
		chromedp.WaitVisible(`#btn-confirm-ok`, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("open dispatch: %v\n%s", err, browserText(ctx))
	}
	type state struct {
		Disabled bool   `json:"disabled"`
		Err      string `json:"err"`
	}
	read := func(label string) state {
		t.Helper()
		var st state
		if err := chromedp.Run(ctx, chromedp.Evaluate(`({disabled: document.getElementById("btn-confirm-ok").disabled, err: document.getElementById("confirm-err").textContent})`, &st)); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		return st
	}
	setPicked := func(adapter string, checked bool) {
		t.Helper()
		js := `(() => { const el = document.querySelector('#confirm-choices input[data-adapter="` + adapter + `"]'); if (!el) return "missing"; if (el.checked !== ` + map[bool]string{true: "true", false: "false"}[checked] + `) { el.checked = ` + map[bool]string{true: "true", false: "false"}[checked] + `; el.dispatchEvent(new Event("change", {bubbles:true})); } return "ok"; })()`
		var out string
		if err := chromedp.Run(ctx, chromedp.Evaluate(js, &out)); err != nil || out != "ok" {
			t.Fatalf("set %s=%v: out=%q err=%v\n%s", adapter, checked, out, err, browserText(ctx))
		}
	}

	// pi picked: its key cannot travel, so the button must be off and say why.
	setPicked("herdr", false)
	setPicked("pi", true)
	blocked := read("pi picked")
	if !blocked.Disabled || !strings.Contains(blocked.Err, "密钥") || !strings.Contains(blocked.Err, "不能下发") {
		t.Fatalf("pi picked while its key cannot travel: %+v (want disabled with a reason)", blocked)
	}
	// herdr alone has no bound key: allowed.
	setPicked("pi", false)
	setPicked("herdr", true)
	allowed := read("herdr only")
	if allowed.Disabled || allowed.Err != "" {
		t.Fatalf("herdr has no bound key and must stay dispatchable: %+v", allowed)
	}
	// Mixed selection is still blocked by the pi part.
	setPicked("pi", true)
	if mixed := read("mixed"); !mixed.Disabled {
		t.Fatalf("herdr+pi must be blocked by pi's key: %+v", mixed)
	}
	if pulls, _ := stub.counts(); pulls != 0 {
		t.Fatalf("nothing should have been sent, pulls=%d", pulls)
	}
}

// resolveUIStub is a machine with a conflict in pi, whose credential file is
// bound to a key. It records what the resolve actually sent.
type resolveUIStub struct {
	dispatchUIStub
	mu2      sync.Mutex
	pullBody []web.SyncScope
	pushBody []web.SyncScope
}

// The console only shows 处理冲突 for a machine whose last heartbeat reported
// conflicts, so the stub has to report them the way a real agent does.
func (s *resolveUIStub) ListAgents() []web.AgentInfo {
	return []web.AgentInfo{{
		AgentID:  "box",
		Hostname: "box",
		LastSeen: time.Now(),
		Drift:    &web.AgentDrift{Conflicts: 2},
	}}
}

func (s *resolveUIStub) AgentStatus(context.Context, string) (json.RawMessage, error) {
	return json.RawMessage(`{"adapters":[{"id":"pi","conflicts":2}],"errors":[]}`), nil
}

func (s *resolveUIStub) AgentPull(ctx context.Context, id string, confirm bool, scope web.SyncScope) (json.RawMessage, error) {
	s.mu2.Lock()
	s.pullBody = append(s.pullBody, scope)
	s.mu2.Unlock()
	return s.dispatchUIStub.AgentPull(ctx, id, confirm, scope)
}

func (s *resolveUIStub) AgentPush(_ context.Context, _ string, _ bool, scope web.SyncScope) (json.RawMessage, error) {
	s.mu2.Lock()
	s.pushBody = append(s.pushBody, scope)
	s.mu2.Unlock()
	return json.RawMessage(`{"ok":true,"status":"pushed"}`), nil
}

// Resolving a conflict "in favour of the center" writes the center's content
// onto the machine, so a key bound to the adapter has to travel and open with
// it. The dialog must offer the password step; refusing without one strands
// the user on a conflict they cannot resolve. "In favour of the machine"
// uploads and must not need a password or carry the key.
func TestResolveCenterAsksForPasswordAndLocalDoesNot(t *testing.T) {
	if _, err := chromiumExecutable(t); err != nil {
		t.Skipf("chromium is not installed: %v", err)
	}
	build := func(t *testing.T) (string, *resolveUIStub) {
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
		destination := filepath.Join(root, "auth.json")
		if err := os.WriteFile(destination, []byte("token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if r := keyring.Apply(homer, keyring.Command{Action: "create", ID: "pi", Name: "pi", Password: "long-password", WorkFactor: 14}); !r.OK {
			t.Fatalf("create = %#v", r)
		}
		if r := keyring.Apply(homer, keyring.Command{Action: "encrypt", ID: "pi", Path: destination, Adapter: "pi", Password: "long-password", WorkFactor: 14}); !r.OK {
			t.Fatalf("encrypt = %#v", r)
		}
		if _, err := gens.New(homer).Publish(map[string]map[string]string{
			"pi":      {"settings/settings.json": "x\n"},
			"keyring": {"marker.txt": "x\n"},
		}, []byte("{}\n")); err != nil {
			t.Fatal(err)
		}
		stub := &resolveUIStub{}
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
		return "http://" + listener.Addr().String() + "/", stub
	}
	open := func(t *testing.T, pageURL string) (context.Context, func()) {
		browser, cancel := newKeyBrowser(t)
		ctx, stop := context.WithTimeout(browser, 90*time.Second)
		err := chromedp.Run(ctx,
			chromedp.Navigate(pageURL),
			chromedp.WaitVisible(`#in-setup-pw`, chromedp.ByQuery),
			chromedp.SendKeys(`#in-setup-pw`, "browser-admin-pw", chromedp.ByQuery),
			chromedp.SendKeys(`#in-setup-pw2`, "browser-admin-pw", chromedp.ByQuery),
			chromedp.Click(`#btn-setup`, chromedp.ByQuery),
			chromedp.WaitVisible(`//div[@id="agent-list"]//button[normalize-space()="处理冲突"]`, chromedp.BySearch),
			chromedp.Click(`//div[@id="agent-list"]//button[normalize-space()="处理冲突"]`, chromedp.BySearch),
			chromedp.WaitVisible(`#confirm-choices input[data-adapter="pi"]`, chromedp.ByQuery),
		)
		if err != nil {
			t.Fatalf("open resolve: %v\n%s", err, browserText(ctx))
		}
		return ctx, func() { stop(); cancel() }
	}

	t.Run("center needs a password, then writes and opens the key", func(t *testing.T) {
		pageURL, stub := build(t)
		ctx, done := open(t, pageURL)
		defer done()
		err := chromedp.Run(ctx,
			chromedp.WaitVisible(`//button[@id="btn-confirm-ok" and normalize-space()="下一步"]`, chromedp.BySearch),
			chromedp.Click(`#btn-confirm-ok`, chromedp.ByQuery),
			chromedp.WaitVisible(`#dispatch-unlocks input`, chromedp.ByQuery),
		)
		if err != nil {
			t.Fatalf("password step: %v\n%s", err, browserText(ctx))
		}
		if pulls, unlocks := stub.counts(); pulls != 0 || unlocks != 0 {
			t.Fatalf("nothing may be written before the password: pulls=%d unlocks=%d", pulls, unlocks)
		}
		err = chromedp.Run(ctx,
			chromedp.Evaluate(`(() => { const i = document.querySelector("#dispatch-unlocks input"); i.value = "long-password"; i.dispatchEvent(new Event("input", {bubbles:true})); return 1; })()`, nil),
			chromedp.Click(`#btn-confirm-ok`, chromedp.ByQuery),
		)
		if err != nil {
			t.Fatalf("confirm: %v\n%s", err, browserText(ctx))
		}
		deadline := time.Now().Add(15 * time.Second)
		for {
			if pulls, unlocks := stub.counts(); pulls == 1 && unlocks == 1 {
				break
			}
			if time.Now().After(deadline) {
				pulls, unlocks := stub.counts()
				t.Fatalf("center resolve did not write and open the key: pulls=%d unlocks=%d\n%s", pulls, unlocks, browserText(ctx))
			}
			time.Sleep(100 * time.Millisecond)
		}
		stub.mu2.Lock()
		defer stub.mu2.Unlock()
		carried := false
		for _, a := range stub.pullBody[0].Adapters {
			if a == "keyring" {
				carried = true
			}
		}
		if !carried {
			t.Fatalf("the resolve to the center did not carry the keyring: %+v", stub.pullBody[0].Adapters)
		}
	})

	t.Run("machine wins uploads without a password or the key", func(t *testing.T) {
		pageURL, stub := build(t)
		ctx, done := open(t, pageURL)
		defer done()
		err := chromedp.Run(ctx,
			chromedp.WaitVisible(`#btn-confirm-alt`, chromedp.ByQuery),
			chromedp.Click(`#btn-confirm-alt`, chromedp.ByQuery),
		)
		if err != nil {
			t.Fatalf("click machine wins: %v\n%s", err, browserText(ctx))
		}
		deadline := time.Now().Add(15 * time.Second)
		for {
			stub.mu2.Lock()
			pushed := len(stub.pushBody)
			stub.mu2.Unlock()
			if pushed == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("machine-wins did not push\n%s", browserText(ctx))
			}
			time.Sleep(100 * time.Millisecond)
		}
		stub.mu2.Lock()
		defer stub.mu2.Unlock()
		for _, a := range stub.pushBody[0].Adapters {
			if a == "keyring" {
				t.Fatalf("machine-wins must not upload the keyring: %+v", stub.pushBody[0].Adapters)
			}
		}
		if pulls, unlocks := stub.counts(); pulls != 0 || unlocks != 0 {
			t.Fatalf("machine-wins must not write the machine or open keys: pulls=%d unlocks=%d", pulls, unlocks)
		}
	})
}
