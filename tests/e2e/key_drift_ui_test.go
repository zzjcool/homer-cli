package e2e

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gens"
	"github.com/zzjcool/homer-cli/internal/keyring"
	"github.com/zzjcool/homer-cli/internal/web"
)

// keyDriftStub reports pi with no config drift (pull=0) while the keyring
// itself has pending drift — the rotated-password shape.
type keyDriftStub struct {
	dispatchUIStub
}

func (s *keyDriftStub) AgentStatus(context.Context, string) (json.RawMessage, error) {
	return json.RawMessage(`{"adapters":[{"id":"pi","pull":0},{"id":"keyring","pull":4}],"errors":[]}`), nil
}

// 场景 B 实测：只有 keyring 有待下发（pi pull=0），下发表里 pi 必须默认勾选，
// 密钥才有随行通道；确认按钮应变为「下一步」进入口令步。
func TestKeyDriftPreChecksBoundAdapter(t *testing.T) {
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
	// 中心有 pi 的配置 + keyring；机器状态：pi pull=0（无配置漂移），keyring pull=4（密钥漂移）。
	if _, err := gens.New(homer).Publish(map[string]map[string]string{
		"pi":      {"settings/settings.json": "x\n"},
		"keyring": {"items/pi/manifest.json": "x\n"},
	}, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	stub := &keyDriftStub{}
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
	ctx, stop := context.WithTimeout(browser, 60*time.Second)
	defer stop()

	err = chromedp.Run(ctx,
		chromedp.Navigate("http://"+listener.Addr().String()+"/"),
		chromedp.WaitVisible(`#in-setup-pw`, chromedp.ByQuery),
		chromedp.SendKeys(`#in-setup-pw`, "browser-admin-pw", chromedp.ByQuery),
		chromedp.SendKeys(`#in-setup-pw2`, "browser-admin-pw", chromedp.ByQuery),
		chromedp.Click(`#btn-setup`, chromedp.ByQuery),
		chromedp.WaitVisible(`//div[@id="agent-list"]//button[normalize-space()="下发"]`, chromedp.BySearch),
		chromedp.Click(`//div[@id="agent-list"]//button[normalize-space()="下发"]`, chromedp.BySearch),
		chromedp.WaitVisible(`#confirm-choices input[data-adapter="pi"]`, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("open dispatch: %v\n%s", err, browserText(ctx))
	}
	var state struct {
		PiChecked bool   `json:"piChecked"`
		OkEnabled bool   `json:"okEnabled"`
		OkLabel   string `json:"okLabel"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`({
		piChecked: document.querySelector('#confirm-choices input[data-adapter="pi"]').checked,
		okEnabled: !document.getElementById("btn-confirm-ok").disabled,
		okLabel: document.getElementById("btn-confirm-ok").textContent
	})`, &state)); err != nil {
		t.Fatal(err)
	}
	t.Logf("state=%+v", state)
	if !state.PiChecked {
		t.Fatal("pi must be pre-checked when only the key has drift")
	}
	if !state.OkEnabled || state.OkLabel != "下一步" {
		t.Fatalf("with a bound key pre-checked the dialog must offer the password step: %+v", state)
	}
	// 点下一步必须进口令步
	if err := chromedp.Run(ctx,
		chromedp.Click(`#btn-confirm-ok`, chromedp.ByQuery),
		chromedp.WaitVisible(`#dispatch-unlocks input`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("unlock step: %v\n%s", err, browserText(ctx))
	}
	// 填口令确认，机器应收到含 keyring 的下发并在其上解开密钥
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`(() => { const i = document.querySelector("#dispatch-unlocks input"); i.value = "long-password"; i.dispatchEvent(new Event("input", {bubbles:true})); return 1; })()`, nil),
		chromedp.Click(`#btn-confirm-ok`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("confirm: %v\n%s", err, browserText(ctx))
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if pulls, unlocks := stub.counts(); pulls == 1 && unlocks == 1 {
			break
		}
		if time.Now().After(deadline) {
			pulls, unlocks := stub.counts()
			t.Fatalf("key drift did not dispatch and unlock: pulls=%d unlocks=%d\n%s", pulls, unlocks, browserText(ctx))
		}
		time.Sleep(100 * time.Millisecond)
	}
}
