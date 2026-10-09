//go:build consolee2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// TestConsoleDockerScenarios drives the hub console in a real browser
// against a docker-compose fleet. New machines run the install command
// copied from the page; nothing enrolls them out of band.
//
// Scenarios, in order:
//  1. setup, logout, bad password, login
//  2. a wrong enrollment code does not add a machine
//  3. the page's install command brings up a new machine ("新机器 · 等待下发")
//  4. the same code does not enroll a second machine
//  5. 查看 on that machine is not a 502
//  6. 下发 while the center is empty fails and leaves the machine uninitialized
//  7. 收取 uploads the machine's local files; 查看存储内容 lists them
//  8. a second empty machine receives 下发; the first machine's file is unchanged
//  9. restarting the agent keeps 下发 working
//
// 10. 处理改动 collects, dispatches to online machines, and skips an offline one
// 11. offline row disables 查看/收取/下发, and they enable again after restart
// 12. 以中心为准 and 以这台机器为准
// 13. 吊销 makes the next 下发 fail; a new code can join; 移除 drops the row
// 14. 登录公钥 writes that GitHub user's public key onto the new machine
func TestConsoleDockerScenarios(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker is required")
	}
	root := consoleRepoRoot(t)
	composeFile := filepath.Join(root, "e2e", "console", "docker-compose.yml")
	compose := func(args ...string) *exec.Cmd {
		cmd := exec.Command("docker", append([]string{"compose", "-f", composeFile}, args...)...)
		cmd.Dir = root
		return cmd
	}
	down := compose("down", "-v", "--remove-orphans")
	_ = down.Run()
	t.Cleanup(func() {
		cmd := compose("down", "-v", "--remove-orphans")
		out, _ := cmd.CombinedOutput()
		if cmd.ProcessState != nil && !cmd.ProcessState.Success() {
			t.Logf("compose down: %s", out)
		}
	})

	t.Log("building and starting the console fleet")
	up := compose("up", "-d", "--build")
	up.Stdout, up.Stderr = os.Stdout, os.Stderr
	if err := up.Run(); err != nil {
		t.Fatalf("compose up: %v", err)
	}

	hubID := strings.TrimSpace(composeOutput(t, compose("ps", "-q", "hub")))
	hubIP := strings.TrimSpace(composeOutput(t, exec.Command("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", hubID)))
	if hubIP == "" {
		t.Fatal("hub container has no IP")
	}
	base := "http://" + hubIP + ":7760"
	t.Logf("hub %s", base)
	consoleWaitHTTP(t, base+"/api/health", 90*time.Second)

	ctx, cancel := newBrowser(t)
	defer cancel()
	env := &consoleEnv{t: t, ctx: ctx, base: base, compose: compose}
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		if _, ok := ev.(*page.EventJavascriptDialogOpening); ok {
			go chromedp.Run(ctx, page.HandleJavaScriptDialog(true))
		}
	})

	const password = "console-e2e-pass"
	env.navigate(base + "/")
	env.waitJS(`document.querySelector('#btn-setup') && !document.querySelector('#gate-setup').classList.contains('hidden')`, 20*time.Second)
	if hidden := env.eval(`document.querySelector('#workspace').classList.contains('hidden')`); hidden != "true" {
		t.Fatal("workspace is visible before login")
	}
	env.setValue("#in-setup-pw", "short")
	env.setValue("#in-setup-pw2", "short")
	env.click("#btn-setup")
	env.waitText("#err-setup", "至少", 10*time.Second)

	env.setValue("#in-setup-pw", password)
	env.setValue("#in-setup-pw2", password)
	env.click("#btn-setup")
	env.waitJS(`!document.querySelector('#workspace').classList.contains('hidden')`, 20*time.Second)
	env.waitText("#hero", "还没有机器接入", 20*time.Second)

	env.click("#btn-logout")
	env.waitJS(`!document.querySelector('#gate-login').classList.contains('hidden')`, 10*time.Second)
	if hidden := env.eval(`document.querySelector('#workspace').classList.contains('hidden')`); hidden != "true" {
		t.Fatal("workspace stayed visible after logout")
	}
	env.submit("#form-login", "#in-login-pw", "wrong-password-value")
	env.waitText("#err-login", "密码", 10*time.Second)
	if hidden := env.eval(`document.querySelector('#gate-login').classList.contains('hidden')`); hidden == "true" {
		t.Fatal("wrong password left the login gate")
	}
	// A failed login arms a short backoff that rejects the next attempt
	// outright. Wait it out, then sign in with the real password.
	time.Sleep(2 * time.Second)
	env.submit("#form-login", "#in-login-pw", password)
	env.waitJS(`!document.querySelector('#workspace').classList.contains('hidden')`, 20*time.Second)

	t.Log("wrong enrollment code")
	command := env.joinCommand()
	wrong := replaceToken(command, "hr_deadbeef")
	if out, err := env.exec("box-bad", wrong, 60*time.Second); err != nil {
		t.Logf("wrong-code install rejected: %v\n%s", err, out)
	}
	env.mustStayAbsent("box-bad", 15*time.Second)

	t.Log("install box-a from the page command")
	started := time.Now()
	if out, err := env.exec("box-a", command, 90*time.Second); err != nil {
		t.Fatalf("box-a install: %v\n%s", err, out)
	} else if time.Since(started) > 60*time.Second {
		t.Fatalf("install script held the terminal for %s\n%s", time.Since(started), out)
	} else {
		t.Logf("install.sh returned in %s\n%s", time.Since(started).Round(time.Millisecond), out)
	}
	env.waitRow("box-a", "新机器 · 等待下发", 60*time.Second)

	t.Log("reuse the spent code")
	if out, err := env.exec("box-reuse", command, 90*time.Second); err != nil {
		t.Logf("spent code rejected: %v\n%s", err, out)
	}
	env.mustStayAbsent("box-reuse", 20*time.Second)

	t.Log("view the fresh machine")
	env.clickRow("box-a", "查看")
	env.waitJS(`document.querySelector('#dlg-view').open`, 30*time.Second)
	env.waitJS(`!document.querySelector('#view-body').innerText.includes('正在向这台机器')`, 30*time.Second)
	view := env.eval(`document.querySelector('#view-body').innerText`)
	if strings.Contains(view, "502") || strings.Contains(view, "查询失败") {
		t.Fatalf("view failed: %s", view)
	}
	if !strings.Contains(view, "未找到 homer") && !strings.Contains(view, "pi") {
		t.Fatalf("view did not describe the machine: %s", view)
	}
	env.click(`#dlg-view button[value="ok"]`)

	t.Log("dispatch before the center has anything")
	env.confirmRow("box-a", "下发")
	alert := env.waitOutcome(40 * time.Second)
	if !strings.Contains(alert, "下发没有完成") {
		t.Fatalf("empty-center dispatch = %s", alert)
	}
	if env.fileExists("box-a", "/root/.homer/homer.json") {
		t.Fatal("failed dispatch still wrote homer.json")
	}

	t.Log("collect box-a into the center")
	env.confirmRow("box-a", "收取")
	if outcome := env.waitOutcome(90 * time.Second); !strings.Contains(outcome, "已从 box-a 收取") {
		t.Fatalf("collect = %s\n%s", outcome, env.agentLog("box-a"))
	}
	env.waitHero("查看存储内容", 20*time.Second)
	env.clickHero("查看存储内容")
	env.waitJS(`document.querySelector('#dlg-view').open && document.querySelector('#view-body').innerText.includes('settings.json')`, 20*time.Second)
	env.click(`#dlg-view button[value="ok"]`)

	t.Log("second machine, dispatch only to it")
	before := env.cat("box-a", "/root/.pi/agent/settings.json")
	commandB := env.joinCommand()
	if _, err := env.exec("box-b", commandB, 90*time.Second); err != nil {
		t.Fatalf("box-b install: %v", err)
	}
	env.waitRow("box-b", "新机器 · 等待下发", 60*time.Second)
	env.confirmRow("box-b", "下发")
	if outcome := env.waitOutcome(90 * time.Second); !strings.Contains(outcome, "已下发到 box-b") {
		t.Fatalf("dispatch box-b = %s\n%s", outcome, env.agentLog("box-b"))
	}
	// Drift is recomputed on the agent's own interval, so the badge can lag
	// the successful dispatch by half a minute.
	env.waitGone("box-b", "新机器 · 等待下发", 75*time.Second)
	if !env.fileExists("box-b", "/root/.homer/homer.json") {
		t.Fatal("dispatch did not create homer.json on box-b")
	}
	got := env.cat("box-b", "/root/.pi/agent/settings.json")
	if !strings.Contains(got, "from-box-a") {
		t.Fatalf("box-b settings = %q", got)
	}
	if after := env.cat("box-a", "/root/.pi/agent/settings.json"); after != before {
		t.Fatalf("dispatch to box-b changed box-a\nbefore %q\nafter %q", before, after)
	}

	t.Log("join a second machine through the same outbound stream command")
	streamCmd := env.joinCommand()
	if strings.Contains(streamCmd, "--listen") || strings.Contains(streamCmd, "--connect") {
		t.Fatalf("join command includes a removed agent flag: %q", streamCmd)
	}
	out, err := env.exec("box-listen", streamCmd, 90*time.Second)
	t.Logf("second agent install: %v\n%s", err, out)
	if err != nil {
		t.Fatalf("second agent install failed: %v\n%s", err, out)
	}
	time.Sleep(3 * time.Second)
	t.Logf("second agent log:\n%s", env.agentLog("box-listen"))
	env.waitRow("box-listen", "新机器 · 等待下发", 60*time.Second)
	env.clickRow("box-listen", "查看")
	env.waitJS(`document.querySelector('#dlg-view').open`, 20*time.Second)
	env.waitJS(`!document.querySelector('#view-body').innerText.includes('正在向这台机器')`, 30*time.Second)
	if view := env.eval(`document.querySelector('#view-body').innerText`); strings.Contains(view, "502") || strings.Contains(view, "查询失败") {
		t.Fatalf("second agent view = %s", view)
	}
	env.click(`#dlg-view button[value="ok"]`)
	env.confirmRow("box-listen", "下发")
	if outcome := env.waitOutcome(90 * time.Second); !strings.Contains(outcome, "已下发到 box-listen") {
		t.Fatalf("second agent dispatch = %s\n%s", outcome, env.agentLog("box-listen"))
	}
	if !env.fileExists("box-listen", "/root/.homer/homer.json") {
		t.Fatal("second agent dispatch did not create homer.json")
	}
	env.installLoginKey("box-listen", "e2euser")
	if outcome := env.waitOutcome(40 * time.Second); !strings.Contains(outcome, "已把 e2euser") {
		t.Fatalf("second agent login key = %s\n%s", outcome, env.agentLog("box-listen"))
	}
	if body := env.cat("box-listen", "/root/.ssh/authorized_keys"); !strings.Contains(body, "BEGIN homer github e2euser") {
		t.Fatalf("second agent authorized_keys =\n%s", body)
	}

	t.Log("restart box-b agent and dispatch again")
	env.killAgent("box-b")
	env.startAgent("box-b")
	env.waitRow("box-b", "机器上报", 60*time.Second)
	env.confirmRow("box-b", "下发")
	if outcome := env.waitOutcome(90 * time.Second); !strings.Contains(outcome, "已下发到 box-b") {
		t.Fatalf("dispatch after restart = %s\n%s", outcome, env.agentLog("box-b"))
	}

	t.Log("offline buttons, then fleet sync skipping the offline machine")
	env.writeFile("box-a", "/root/.pi/agent/settings.json", "{\"marker\":\"box-a-drift\"}\n")
	commandC := env.joinCommand()
	if _, err := env.exec("box-c", commandC, 90*time.Second); err != nil {
		t.Fatalf("box-c install: %v", err)
	}
	env.waitRow("box-c", "新机器 · 等待下发", 60*time.Second)
	env.killAgent("box-b")
	env.waitRow("box-b", "离线", 150*time.Second)
	if state := env.rowButtonState("box-b"); state != "disabled" {
		t.Fatalf("offline buttons = %s", state)
	}
	env.waitHero("处理改动", 50*time.Second)
	env.clickHero("处理改动")
	env.click("#btn-confirm-ok")
	if outcome := env.waitOutcome(120 * time.Second); strings.Contains(outcome, "没有全部完成") || strings.Contains(outcome, "502") {
		t.Fatalf("fleet sync = %s", outcome)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		body := env.cat("box-c", "/root/.pi/agent/settings.json")
		if strings.Contains(body, "box-a-drift") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("box-c did not receive the drift: %q\nbox-b %q", body, env.cat("box-b", "/root/.pi/agent/settings.json"))
		}
		time.Sleep(2 * time.Second)
	}
	if boxB := env.cat("box-b", "/root/.pi/agent/settings.json"); strings.Contains(boxB, "box-a-drift") {
		t.Fatalf("offline box-b was updated: %q", boxB)
	}
	env.startAgent("box-b")
	env.waitRow("box-b", "机器上报", 60*time.Second)
	env.waitJS(rowEnabledJS("box-b"), 40*time.Second)

	t.Log("resolve center, then local")
	env.writeFile("box-a", "/root/.pi/agent/settings.json", "{\"marker\":\"box-a-v2\"}\n")
	env.writeFile("box-c", "/root/.pi/agent/settings.json", "{\"marker\":\"box-c-v1\"}\n")
	env.confirmRow("box-a", "收取")
	if outcome := env.waitOutcome(90 * time.Second); !strings.Contains(outcome, "已从 box-a 收取") {
		t.Fatalf("collect for conflict = %s", outcome)
	}
	env.waitRow("box-c", "处理冲突", 70*time.Second)
	env.confirmRow("box-c", "处理冲突")
	// Staged-resolution dialog: pick the center radio, then 记录并立即执行.
	env.waitJS(`document.querySelector('#confirm-choices input[type=radio][data-resolution-adapter="pi"]') !== null`, 15*time.Second)
	env.setValue(`#confirm-choices input[type=radio][data-resolution-adapter="pi"][value="center"]`, "center")
	env.eval(`(() => { const r = document.querySelector('#confirm-choices input[type=radio][data-resolution-adapter="pi"][value="center"]'); r.checked = true; r.dispatchEvent(new Event('change', {bubbles:true})); return 'ok'; })()`)
	env.click("#btn-confirm-alt")
	if outcome := env.waitOutcome(90 * time.Second); !strings.Contains(outcome, "已记录并立即执行") {
		t.Fatalf("resolve center = %s\n%s", outcome, env.agentLog("box-c"))
	}
	env.waitFileContains("box-c", "/root/.pi/agent/settings.json", "box-a-v2", 40*time.Second)

	env.writeFile("box-a", "/root/.pi/agent/settings.json", "{\"marker\":\"box-a-v3\"}\n")
	env.writeFile("box-c", "/root/.pi/agent/settings.json", "{\"marker\":\"box-c-v2\"}\n")
	env.confirmRow("box-a", "收取")
	if outcome := env.waitOutcome(90 * time.Second); !strings.Contains(outcome, "已从 box-a 收取") {
		t.Fatalf("second collect = %s", outcome)
	}
	env.waitRow("box-c", "处理冲突", 70*time.Second)
	env.clearAlert()
	env.clickRow("box-c", "处理冲突")
	// Same dialog, this time the machine-wins radio feeds the local group.
	env.waitJS(`document.querySelector('#confirm-choices input[type=radio][data-resolution-adapter="pi"]') !== null`, 15*time.Second)
	env.eval(`(() => { const r = document.querySelector('#confirm-choices input[type=radio][data-resolution-adapter="pi"][value="local"]'); r.checked = true; r.dispatchEvent(new Event('change', {bubbles:true})); return 'ok'; })()`)
	env.waitJS(`document.querySelector('#dlg-confirm').open && !document.querySelector('#btn-confirm-alt').disabled`, 15*time.Second)
	env.click("#btn-confirm-alt")
	if outcome := env.waitOutcome(120 * time.Second); !strings.Contains(outcome, "已记录并立即执行") {
		t.Fatalf("resolve local = %s\n%s", outcome, env.agentLog("box-c"))
	}
	env.waitFileContains("box-a", "/root/.pi/agent/settings.json", "box-c-v2", 60*time.Second)

	t.Log("revoke, rejoin, remove")
	env.clickRow("box-c", "吊销")
	env.waitText("#toast", "已吊销", 15*time.Second)
	env.confirmRow("box-c", "下发")
	// The revoked agent stops taking tasks, so the hub waits out its
	// dispatch timeout before the console can show the failure.
	if outcome := env.waitOutcome(80 * time.Second); !strings.Contains(outcome, "下发没有完成") {
		t.Fatalf("dispatch after revoke = %s", outcome)
	}
	commandRe := env.joinCommand()
	if _, err := env.exec("box-rejoin", commandRe, 90*time.Second); err != nil {
		t.Fatalf("rejoin install: %v", err)
	}
	env.waitRow("box-rejoin", "机器上报", 60*time.Second)

	t.Log("login key onto the new machine")
	const existingKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAlreadyThereKeyMaterial1234567890 laptop"
	const managedKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHomerExampleKeyMaterial1234567890 homer"
	env.writeFile("box-rejoin", "/root/.ssh/authorized_keys", existingKey+"\n")
	env.installLoginKey("box-rejoin", "missing")
	if outcome := env.waitOutcome(30 * time.Second); !strings.Contains(outcome, "不存在") {
		t.Fatalf("unknown github user = %s", outcome)
	}
	if body := env.cat("box-rejoin", "/root/.ssh/authorized_keys"); strings.Contains(body, "BEGIN homer github") || !strings.Contains(body, existingKey) {
		t.Fatalf("failed key install changed authorized_keys:\n%s", body)
	}
	env.installLoginKey("box-rejoin", "e2euser")
	if outcome := env.waitOutcome(40 * time.Second); !strings.Contains(outcome, "已把 e2euser") {
		t.Fatalf("login key = %s\n%s", outcome, env.agentLog("box-rejoin"))
	}
	body := env.cat("box-rejoin", "/root/.ssh/authorized_keys")
	if !strings.Contains(body, existingKey) || !strings.Contains(body, managedKey) || strings.Count(body, "# BEGIN homer github e2euser") != 1 {
		t.Fatalf("authorized_keys =\n%s", body)
	}

	env.clickRow("box-c", "移除")
	env.waitText("#toast", "已移除", 15*time.Second)
	env.mustStayAbsent("box-c", 20*time.Second)
}

type consoleEnv struct {
	t       *testing.T
	ctx     context.Context
	base    string
	compose func(args ...string) *exec.Cmd
}

func (c *consoleEnv) navigate(url string) {
	c.t.Helper()
	// Accept window.confirm. The native dialog races the listener and was
	// dismissing 吊销/移除 before the click handler ran.
	accept := chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(`window.confirm = () => true;`).Do(ctx)
		return err
	})
	if err := chromedp.Run(c.ctx, accept, chromedp.Navigate(url)); err != nil {
		c.t.Fatal(err)
	}
}

func (c *consoleEnv) click(sel string) {
	c.t.Helper()
	if err := chromedp.Run(c.ctx, chromedp.Click(sel, chromedp.ByQuery)); err != nil {
		c.t.Fatalf("click %s: %v", sel, err)
	}
}

// submit sets one field and submits the form in the same turn so the
// browser cannot autofill a previous password over the value.
func (c *consoleEnv) submit(form, field, value string) {
	c.t.Helper()
	js := fmt.Sprintf(`(() => {
		const input = document.querySelector(%q);
		const form = document.querySelector(%q);
		if (!input || !form) return 'missing';
		input.value = %q;
		input.dispatchEvent(new Event('input', {bubbles:true}));
		form.requestSubmit();
		return 'ok';
	})()`, field, form, value)
	if got := c.eval(js); got != "ok" {
		c.t.Fatalf("submit %s: %s", form, got)
	}
}

func (c *consoleEnv) setValue(sel, value string) {
	c.t.Helper()
	js := fmt.Sprintf(`(() => { const n = document.querySelector(%q); n.value = %q; n.dispatchEvent(new Event('input', {bubbles:true})); return 'ok'; })()`, sel, value)
	if got := c.eval(js); got != "ok" {
		c.t.Fatalf("set %s: %s", sel, got)
	}
}

func (c *consoleEnv) eval(js string) string {
	c.t.Helper()
	var out string
	// Callers pass expressions and IIFEs. String() keeps bool results
	// ("true"/"false") in the same string channel as text results.
	wrapped := `(() => { const v = (` + js + `); return v == null ? "" : String(v); })()`
	if err := chromedp.Run(c.ctx, chromedp.Evaluate(wrapped, &out)); err != nil {
		c.t.Fatalf("eval: %v\npage: %s", err, c.pageText())
	}
	return out
}

func (c *consoleEnv) waitJS(expr string, timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		var ready bool
		err := chromedp.Run(c.ctx, chromedp.Evaluate(`!!(`+expr+`)`, &ready))
		if err == nil && ready {
			return
		}
		if err != nil {
			last = err.Error()
		}
		time.Sleep(300 * time.Millisecond)
	}
	c.t.Fatalf("timeout waiting for %s (%s)\npage: %s", expr, last, c.pageText())
}

func (c *consoleEnv) waitText(sel, needle string, timeout time.Duration) {
	c.t.Helper()
	c.waitJS(fmt.Sprintf(`(document.querySelector(%q)||{}).innerText && (document.querySelector(%q)||{}).innerText.includes(%q)`, sel, sel, needle), timeout)
}

func (c *consoleEnv) waitHero(label string, timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.click("#btn-refresh")
		time.Sleep(400 * time.Millisecond)
		got := c.eval(fmt.Sprintf(`!![...document.querySelectorAll('#hero button')].find(n => n.textContent === %q)`, label))
		if got == "true" {
			return
		}
		time.Sleep(time.Second)
	}
	c.t.Fatalf("hero button %s did not appear\npage: %s", label, c.pageText())
}

func (c *consoleEnv) clickHero(label string) {
	c.t.Helper()
	c.clearAlert()
	js := fmt.Sprintf(`(() => { const b = [...document.querySelectorAll('#hero button')].find(n => n.textContent === %q); if (!b) return 'missing'; b.click(); return 'ok'; })()`, label)
	if got := c.eval(js); got != "ok" {
		c.t.Fatalf("hero %s: %s\n%s", label, got, c.pageText())
	}
}

func (c *consoleEnv) clickRow(hostname, label string) {
	c.t.Helper()
	c.eval(`(() => { window.confirm = () => true; return 'ok'; })()`)
	c.click("#btn-refresh")
	time.Sleep(400 * time.Millisecond)
	js := fmt.Sprintf(`(() => {
		const row = [...document.querySelectorAll('.agent-line')].find(r => (r.querySelector('.hostname')||{}).textContent === %q);
		if (!row) return 'missing-row';
		const btn = [...row.querySelectorAll('button')].find(b => b.textContent === %q);
		if (!btn) return 'missing-button';
		if (btn.disabled) return 'disabled';
		btn.click();
		return 'ok';
	})()`, hostname, label)
	deadline := time.Now().Add(20 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		got = c.eval(js)
		if got == "ok" {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	c.t.Fatalf("click %s %s: %s\n%s", hostname, label, got, c.pageText())
}

func (c *consoleEnv) clearAlert() {
	c.t.Helper()
	c.eval(`(() => {
		const b = document.querySelector('#alert-slot button');
		if (b) b.click();
		const toast = document.querySelector('#toast');
		if (toast) { toast.classList.remove('show'); toast.textContent = ''; }
		return 'ok';
	})()`)
}

func (c *consoleEnv) installLoginKey(hostname, user string) {
	c.t.Helper()
	c.clearAlert()
	c.clickRow(hostname, "登录公钥")
	c.waitJS(`document.querySelector('#dlg-confirm').open && document.querySelector('#ssh-user')`, 10*time.Second)
	c.setValue("#ssh-user", user)
	c.click("#btn-confirm-ok")
}

func (c *consoleEnv) confirmRow(hostname, label string) {
	c.t.Helper()
	c.clearAlert()
	c.clickRow(hostname, label)
	c.waitJS(`document.querySelector('#dlg-confirm').open`, 10*time.Second)
	c.click("#btn-confirm-ok")
}

func (c *consoleEnv) waitOutcome(timeout time.Duration) string {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		text := c.eval(`(() => {
			const alert = document.querySelector('#alert-slot');
			if (alert && alert.innerText.trim()) return alert.innerText;
			const toast = document.querySelector('#toast.show');
			if (toast && toast.innerText.trim()) return toast.innerText;
			return '';
		})()`)
		if text != "" {
			return text
		}
		time.Sleep(300 * time.Millisecond)
	}
	c.t.Fatalf("no toast or alert\npage: %s", c.pageText())
	return ""
}

func (c *consoleEnv) waitRow(hostname, needle string, timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		c.click("#btn-refresh")
		time.Sleep(500 * time.Millisecond)
		last = c.eval(fmt.Sprintf(`(() => {
			const row = [...document.querySelectorAll('.agent-line')].find(r => (r.querySelector('.hostname')||{}).textContent === %q);
			return row ? row.innerText : '';
		})()`, hostname))
		if strings.Contains(last, needle) {
			return
		}
		time.Sleep(time.Second)
	}
	c.t.Fatalf("row %s never contained %q (last %q)\npage: %s", hostname, needle, last, c.pageText())
}

func (c *consoleEnv) waitGone(hostname, needle string, timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.click("#btn-refresh")
		time.Sleep(400 * time.Millisecond)
		text := c.eval(fmt.Sprintf(`(() => {
			const row = [...document.querySelectorAll('.agent-line')].find(r => (r.querySelector('.hostname')||{}).textContent === %q);
			return row ? row.innerText : '';
		})()`, hostname))
		if text != "" && !strings.Contains(text, needle) {
			return
		}
		time.Sleep(time.Second)
	}
	c.t.Fatalf("row %s still shows %q\npage: %s", hostname, needle, c.pageText())
}

func (c *consoleEnv) mustStayAbsent(hostname string, duration time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		c.click("#btn-refresh")
		time.Sleep(400 * time.Millisecond)
		found := c.eval(fmt.Sprintf(`!![...document.querySelectorAll('.hostname')].find(n => n.textContent === %q)`, hostname))
		if found == "true" {
			c.t.Fatalf("%s appeared\npage: %s", hostname, c.pageText())
		}
		time.Sleep(time.Second)
	}
}

func (c *consoleEnv) rowButtonState(hostname string) string {
	c.t.Helper()
	c.click("#btn-refresh")
	time.Sleep(400 * time.Millisecond)
	return c.eval(fmt.Sprintf(`(() => {
		const row = [...document.querySelectorAll('.agent-line')].find(r => (r.querySelector('.hostname')||{}).textContent === %q);
		if (!row) return 'missing-row';
		const labels = ['查看', '收取', '下发'];
		const buttons = labels.map(label => [...row.querySelectorAll('button')].find(b => b.textContent === label));
		if (buttons.some(b => !b)) return 'missing-button';
		return buttons.every(b => b.disabled) ? 'disabled' : 'enabled';
	})()`, hostname))
}

func rowEnabledJS(hostname string) string {
	return fmt.Sprintf(`(() => {
		const row = [...document.querySelectorAll('.agent-line')].find(r => (r.querySelector('.hostname')||{}).textContent === %q);
		if (!row) return false;
		const btn = [...row.querySelectorAll('button')].find(b => b.textContent === '下发');
		return !!(btn && !btn.disabled);
	})()`, hostname)
}

func (c *consoleEnv) joinCommand() string {
	c.t.Helper()
	c.click("#btn-join-top")
	c.waitJS(`(document.querySelector('#join-cmd')||{}).value && document.querySelector('#join-cmd').value.includes('curl ')`, 15*time.Second)
	command := c.eval(`document.querySelector('#join-cmd').value`)
	if got := c.eval(`document.querySelectorAll('#dlg-join .join-cmd input').length`); got != "1" {
		c.t.Fatalf("join dialog has %s command fields, want one", got)
	}
	if got := c.eval(`!!document.querySelector('#join-listen')`); got != "false" {
		c.t.Fatalf("join dialog still exposes a listen command")
	}
	c.click(`#dlg-join button[value="ok"]`)
	time.Sleep(300 * time.Millisecond)
	if !strings.Contains(command, "hr_") || !strings.Contains(command, c.base) {
		c.t.Fatalf("join command = %q", command)
	}
	if strings.Contains(command, "--listen") || strings.Contains(command, "--connect") {
		c.t.Fatalf("join command uses a removed agent flag: %q", command)
	}
	return command
}

func (c *consoleEnv) exec(service, script string, timeout time.Duration) (string, error) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	base := c.compose("exec", "-T", service, "sh", "-c", script)
	cmd := exec.CommandContext(ctx, base.Args[0], base.Args[1:]...)
	cmd.Dir = base.Dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (c *consoleEnv) killAgent(service string) {
	c.t.Helper()
	// Build the needle at runtime so this shell's own command line does not
	// contain it and get killed too.
	script := `a=homer; b=agent; needle="$a $b"; for p in /proc/[0-9]*; do cmd=$(tr '\0' ' ' < "$p/cmdline" 2>/dev/null || true); case "$cmd" in *"$needle"*) kill "${p##*/}" || true;; esac; done; echo killed`
	if out, err := c.exec(service, script, 15*time.Second); err != nil {
		c.t.Fatalf("kill %s: %v\n%s", service, err, out)
	}
}

func (c *consoleEnv) startAgent(service string) {
	c.t.Helper()
	script := `if [ -x /root/.local/bin/homer ]; then BIN=/root/.local/bin/homer; else BIN=/usr/local/bin/homer; fi; nohup "$BIN" agent --home /root/.homer >>/root/.homer/agent.log 2>&1 & echo started`
	if out, err := c.exec(service, script, 15*time.Second); err != nil {
		c.t.Fatalf("start %s: %v\n%s", service, err, out)
	}
}

func (c *consoleEnv) cat(service, path string) string {
	c.t.Helper()
	out, err := c.exec(service, "cat "+path, 15*time.Second)
	if err != nil {
		c.t.Fatalf("cat %s %s: %v\n%s", service, path, err, out)
	}
	return out
}

func (c *consoleEnv) fileExists(service, path string) bool {
	c.t.Helper()
	_, err := c.exec(service, "test -f "+path, 15*time.Second)
	return err == nil
}

func (c *consoleEnv) writeFile(service, path, body string) {
	c.t.Helper()
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	script := fmt.Sprintf("mkdir -p %q && cat > %q <<'EOF'\n%sEOF\n", filepath.Dir(path), path, body)
	if out, err := c.exec(service, script, 15*time.Second); err != nil {
		c.t.Fatalf("write %s: %v\n%s", path, err, out)
	}
}

func (c *consoleEnv) waitFileContains(service, path, needle string, timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		out, err := c.exec(service, "cat "+path, 15*time.Second)
		last = out
		if err == nil && strings.Contains(out, needle) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	c.t.Fatalf("%s:%s never contained %q (last %q)", service, path, needle, last)
}

func (c *consoleEnv) agentLog(service string) string {
	out, err := c.exec(service, "tail -n 80 /root/.homer/agent.log 2>/dev/null || true", 15*time.Second)
	if err != nil {
		return err.Error()
	}
	return out
}

func (c *consoleEnv) pageText() string {
	var text string
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	_ = chromedp.Run(ctx, chromedp.Evaluate(`document.body ? document.body.innerText : ''`, &text))
	if len(text) > 4000 {
		text = text[:4000]
	}
	return text
}

func replaceToken(command, token string) string {
	index := strings.LastIndex(command, "hr_")
	if index < 0 {
		return command + " " + token
	}
	return command[:index] + token
}

func newBrowser(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath("/usr/bin/chromium"),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancel := chromedp.NewContext(allocCtx)
	return ctx, func() {
		cancel()
		allocCancel()
	}
}

func consoleWaitHTTP(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		cmd := exec.Command("curl", "-fsS", "-m", "3", url)
		if err := cmd.Run(); err == nil {
			return
		} else {
			last = err
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("health %s: %v", url, last)
}

func composeOutput(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(cmd.Args, " "), err, out)
	}
	return string(out)
}

func consoleRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
