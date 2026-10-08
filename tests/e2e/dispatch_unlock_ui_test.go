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
