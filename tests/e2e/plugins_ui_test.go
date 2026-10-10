package e2e

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/pluginregistry"
	"github.com/zzjcool/homer-cli/internal/pluginruntime"
	"github.com/zzjcool/homer-cli/internal/web"
)

func TestPluginManagementConsole(t *testing.T) {
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
	plugins := pluginruntime.New(homer)
	for _, id := range []string{"pi", "keyring"} {
		plugin, ok := pluginregistry.Builtin(id)
		if !ok {
			t.Fatalf("pluginregistry.Builtin(%q) not found", id)
		}
		if err := plugins.Install(plugin); err != nil {
			t.Fatal(err)
		}
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.NewServer(web.ServeOptions{
		Addr: "127.0.0.1:0", HomerHome: homer, Agents: pluginUIAgentSource{}, Plugins: plugins,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: server.Handler()}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })
	pageURL := "http://" + listener.Addr().String() + "/"

	browser, cancel := newKeyBrowser(t)
	defer cancel()
	ctx, stop := context.WithTimeout(browser, 180*time.Second)
	defer stop()

	const admin = "plugin-browser-admin-password"
	err = chromedp.Run(ctx,
		chromedp.Navigate(pageURL),
		chromedp.WaitVisible(`#in-setup-pw`, chromedp.ByQuery),
		chromedp.SendKeys(`#in-setup-pw`, admin, chromedp.ByQuery),
		chromedp.SendKeys(`#in-setup-pw2`, admin, chromedp.ByQuery),
		chromedp.Click(`#btn-setup`, chromedp.ByQuery),
		chromedp.WaitVisible(`#btn-plugins`, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("browser setup: %v\npage:\n%s", err, browserText(ctx))
	}

	if err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {
		window.__pluginUIRequests = [];
		window.__pluginUIConfirmations = [];
		const fetchOriginal = window.fetch.bind(window);
		window.confirm = message => { window.__pluginUIConfirmations.push(String(message)); return true; };
		window.fetch = (resource, init = {}) => {
			const url = typeof resource === "string" ? resource : resource.url;
			if (url.startsWith("/api/agents/") && url.endsWith("/ssh-key")) {
				window.__pluginUIRequests.push({ url, method: init.method || "GET", body: init.body ? JSON.parse(init.body) : null });
				return Promise.resolve(new Response(JSON.stringify({ ok: true, installed: 2 }), { status: 200, headers: { "Content-Type": "application/json" } }));
			}
			if (url.startsWith("/api/sync?direction=dispatch")) {
				window.__pluginUIRequests.push({ url, method: init.method || "GET", body: init.body ? JSON.parse(init.body) : null });
				return Promise.resolve(new Response(JSON.stringify({ ok: true, status: "synced", agents: [] }), { status: 200, headers: { "Content-Type": "application/json" } }));
			}
			return fetchOriginal(resource, init);
		};
	})()`, nil)); err != nil {
		t.Fatalf("install browser API spies: %v", err)
	}

	err = chromedp.Run(ctx,
		chromedp.Click(`#btn-plugins`, chromedp.ByQuery),
		chromedp.WaitVisible(`#plugins-installed [data-plugin-id="pi"]`, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("open plugin manager: %v\npage:\n%s", err, browserText(ctx))
	}
	var installedText, availableText string
	if err := chromedp.Run(ctx,
		chromedp.Text(`#plugins-installed`, &installedText, chromedp.ByQuery),
		chromedp.Text(`#plugins-available`, &availableText, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read plugin tables: %v", err)
	}
	if !strings.Contains(installedText, "pi") || !strings.Contains(installedText, "适配器") || !strings.Contains(installedText, "密钥环") || !strings.Contains(installedText, "载体") || !strings.Contains(installedText, "0 台机器") {
		t.Fatalf("installed plugins did not render names, role badges and machine count: %q", installedText)
	}
	if !strings.Contains(availableText, "ssh-key") || !strings.Contains(availableText, "动作") || !strings.Contains(availableText, "执行") || !strings.Contains(availableText, "安装") {
		t.Fatalf("available ssh-key plugin did not render its action and install buttons: %q", availableText)
	}

	// The action form is driven by the API's form spec and only offers online machines.
	err = chromedp.Run(ctx,
		chromedp.Click(`#plugins-available [data-plugin-id="ssh-key"] button[data-plugin-action="execute"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`#dlg-plugin-action`, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("open ssh-key action form: %v\npage:\n%s", err, browserText(ctx))
	}
	var actionFormOK bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {
		const select = document.querySelector("#plugin-action-agent");
		const input = document.querySelector('#plugin-action-fields input[name="githubUser"]');
		return !!select && Array.from(select.options).some(option => option.value === "ui-box") &&
			!!input && input.required && input.placeholder === "例如 octocat" &&
			document.querySelector("#plugin-action-fields").innerText.includes("GitHub 用户名");
	})()`, &actionFormOK)); err != nil {
		t.Fatal(err)
	}
	if !actionFormOK {
		t.Fatal("ssh-key action form did not render an online machine and the required githubUser field from its form spec")
	}
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__pluginsBeforeSSHActionRefresh = app.plugins`, nil),
		chromedp.Evaluate(`document.getElementById("plugin-action-agent").value = "ui-box"; document.querySelector('#plugin-action-fields input[name="githubUser"]').value = "octocat";`, nil),
		chromedp.Click(`#btn-plugin-action-submit`, chromedp.ByQuery),
		chromedp.Poll(`window.__pluginUIRequests.some(request => request.url === "/api/agents/ui-box/ssh-key")`, nil, chromedp.WithPollingInterval(100*time.Millisecond)),
		chromedp.Poll(`app.plugins !== window.__pluginsBeforeSSHActionRefresh`, nil, chromedp.WithPollingInterval(100*time.Millisecond)),
		chromedp.WaitVisible(`#plugins-installed [data-plugin-id="pi"] button[data-plugin-action="dispatch"]`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("submit ssh-key action: %v\npage:\n%s", err, browserText(ctx))
	}
	var sshCall struct {
		URL    string `json:"url"`
		Method string `json:"method"`
		Body   struct {
			GitHubUser string `json:"githubUser"`
		} `json:"body"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__pluginUIRequests.find(request => request.url === "/api/agents/ui-box/ssh-key")`, &sshCall)); err != nil {
		t.Fatal(err)
	}
	if sshCall.Method != http.MethodPost || sshCall.Body.GitHubUser != "octocat" {
		t.Fatalf("ssh-key request = %+v, want POST body {githubUser: octocat}", sshCall)
	}

	// Adapter dispatch confirms the operation and posts the explicit adapter scope.
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__pluginsBeforeDispatchRefresh = app.plugins`, nil),
		chromedp.Click(`#plugins-installed [data-plugin-id="pi"] button[data-plugin-action="dispatch"]`, chromedp.ByQuery),
		chromedp.Poll(`window.__pluginUIRequests.some(request => request.url.startsWith("/api/sync?direction=dispatch"))`, nil, chromedp.WithPollingInterval(100*time.Millisecond)),
		chromedp.Poll(`app.plugins !== window.__pluginsBeforeDispatchRefresh`, nil, chromedp.WithPollingInterval(100*time.Millisecond)),
		chromedp.WaitVisible(`#plugins-installed [data-plugin-id="pi"] button[data-plugin-action="uninstall"]`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("dispatch adapter plugin: %v\npage:\n%s", err, browserText(ctx))
	}
	var dispatchCall struct {
		URL    string `json:"url"`
		Method string `json:"method"`
		Body   struct {
			Adapters []string `json:"adapters"`
			Confirm  bool     `json:"confirm"`
		} `json:"body"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__pluginUIRequests.find(request => request.url.startsWith("/api/sync?direction=dispatch"))`, &dispatchCall)); err != nil {
		t.Fatal(err)
	}
	if dispatchCall.URL != "/api/sync?direction=dispatch&confirm=true" || dispatchCall.Method != http.MethodPost || len(dispatchCall.Body.Adapters) != 1 || dispatchCall.Body.Adapters[0] != "pi" || !dispatchCall.Body.Confirm {
		t.Fatalf("adapter dispatch request = %+v, want confirmed POST for only pi", dispatchCall)
	}

	// Uninstalling an adapter asks for explicit confirmation and leaves it available again.
	if err := chromedp.Run(ctx,
		chromedp.Click(`#plugins-installed [data-plugin-id="pi"] button[data-plugin-action="uninstall"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`#plugins-available [data-plugin-id="pi"]`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("uninstall adapter plugin: %v\npage:\n%s", err, browserText(ctx))
	}
	var uninstallConfirmation string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__pluginUIConfirmations[window.__pluginUIConfirmations.length - 1]`, &uninstallConfirmation)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(uninstallConfirmation, "中心存储") || !strings.Contains(uninstallConfirmation, "保留本机定义") {
		t.Fatalf("adapter uninstall confirmation omitted its guard or data-retention semantics: %q", uninstallConfirmation)
	}

	// Install through the available row; the plugin moves to the installed list after refresh.
	if err := chromedp.Run(ctx,
		chromedp.Click(`#plugins-available [data-plugin-id="ssh-key"] button[data-plugin-action="install"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`#plugins-installed [data-plugin-id="ssh-key"]`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("install available ssh-key plugin: %v\npage:\n%s", err, browserText(ctx))
	}
	var installedAction string
	if err := chromedp.Run(ctx, chromedp.Text(`#plugins-installed [data-plugin-id="ssh-key"]`, &installedAction, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(installedAction, "动作") || !strings.Contains(installedAction, "执行") || !strings.Contains(installedAction, "卸载") {
		t.Fatalf("installed ssh-key row lost its role, action or uninstall button: %q", installedAction)
	}

	// The third-party form is collapsible and displays server validation errors.
	if err := chromedp.Run(ctx,
		chromedp.Click(`#btn-plugin-custom-toggle`, chromedp.ByQuery),
		chromedp.WaitVisible(`#form-plugin-manifest`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("show third-party manifest form: %v\npage:\n%s", err, browserText(ctx))
	}
	invalidManifest := `{"schemaVersion":1,"id":"not-supported","role":"carrier","root":"~/.config/not-supported","categories":{"files":{"paths":["settings.json"],"mode":"mirror"}}}`
	invalidJSON, _ := json.Marshal(invalidManifest)
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`document.getElementById("plugin-manifest").value = `+string(invalidJSON), nil),
		chromedp.Click(`#btn-plugin-manifest-submit`, chromedp.ByQuery),
		chromedp.WaitVisible(`#plugin-error`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("submit invalid third-party manifest: %v\npage:\n%s", err, browserText(ctx))
	}
	var manifestError string
	if err := chromedp.Run(ctx, chromedp.Text(`#plugin-error`, &manifestError, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(manifestError, "第三方插件目前只支持 role=adapter") {
		t.Fatalf("third-party API error was not shown in the plugin page: %q", manifestError)
	}

	validManifest := `{"schemaVersion":1,"id":"custom-editor","role":"adapter","name":"Custom editor","description":"UI test adapter","root":"~/.custom-editor","categories":{"files":{"paths":["settings.json"],"mode":"mirror"}}}`
	validJSON, _ := json.Marshal(validManifest)
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`document.getElementById("plugin-manifest").value = `+string(validJSON), nil),
		chromedp.Click(`#btn-plugin-manifest-submit`, chromedp.ByQuery),
		chromedp.WaitVisible(`#plugins-installed [data-plugin-id="custom-editor"]`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("install valid third-party manifest: %v\npage:\n%s", err, browserText(ctx))
	}
}

type pluginUIAgentSource struct{}

func (pluginUIAgentSource) ListAgents() []web.AgentInfo {
	return []web.AgentInfo{{AgentID: "ui-box", Hostname: "UI 测试机器", LastSeen: time.Now()}}
}

func (pluginUIAgentSource) AgentStatus(context.Context, string) (json.RawMessage, error) {
	return json.RawMessage(`{"report":{"adapters":[]}}`), nil
}

func (pluginUIAgentSource) AgentDiff(context.Context, string, web.DiffParams) (string, error) {
	return "", nil
}

func (pluginUIAgentSource) AgentPush(context.Context, string, bool, web.SyncScope) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true,"status":"pushed"}`), nil
}

func (pluginUIAgentSource) AgentPull(context.Context, string, bool, web.SyncScope) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true,"status":"applied"}`), nil
}

func (pluginUIAgentSource) RemoveAgent(string) bool { return true }
