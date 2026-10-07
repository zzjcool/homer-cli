package e2e

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/web"
)

func TestKeyringConsoleClicksCreateEncryptUnlock(t *testing.T) {
	if _, err := os.Stat("/usr/bin/chromium"); err != nil {
		t.Skip("chromium is not installed")
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
	plainPath := filepath.Join(root, "providers.json")
	const plaintext = "ui-plain-marker\n"
	writeFile(t, plainPath, plaintext)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.NewServer(web.ServeOptions{Addr: "127.0.0.1:0", HomerHome: homer})
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
	keyPassword := "long-password"
	err = chromedp.Run(ctx,
		chromedp.Navigate(pageURL),
		chromedp.WaitVisible(`#in-setup-pw`, chromedp.ByQuery),
		chromedp.SendKeys(`#in-setup-pw`, admin, chromedp.ByQuery),
		chromedp.SendKeys(`#in-setup-pw2`, admin, chromedp.ByQuery),
		chromedp.Click(`#btn-setup`, chromedp.ByQuery),
		chromedp.WaitVisible(`#sec-keys button`, chromedp.ByQuery),
		chromedp.Click(`//button[normalize-space()="新建密钥"]`, chromedp.BySearch),
		chromedp.WaitVisible(`#key-id`, chromedp.ByQuery),
		chromedp.SendKeys(`#key-id`, "codebuddy", chromedp.ByQuery),
		chromedp.SendKeys(`#key-name`, "CodeBuddy", chromedp.ByQuery),
		chromedp.SendKeys(`#key-pw`, keyPassword, chromedp.ByQuery),
		chromedp.SendKeys(`#key-pw2`, keyPassword, chromedp.ByQuery),
		chromedp.Click(`#key-dlg-ok`, chromedp.ByQuery),
		chromedp.WaitVisible(`//div[@id="key-body"]//span[normalize-space()="CodeBuddy"]`, chromedp.BySearch),
		chromedp.Click(`//div[@id="key-body"]//button[normalize-space()="加密文件"]`, chromedp.BySearch),
		chromedp.WaitVisible(`#key-file`, chromedp.ByQuery),
		chromedp.SendKeys(`#key-file`, "providers", chromedp.ByQuery),
		chromedp.SetValue(`#key-path`, plainPath, chromedp.ByQuery),
		chromedp.SendKeys(`#key-pw`, keyPassword, chromedp.ByQuery),
		chromedp.Click(`#key-dlg-ok`, chromedp.ByQuery),
		chromedp.WaitVisible(`//div[@id="key-body"][contains(., "providers")]`, chromedp.BySearch),
	)
	if err != nil {
		t.Fatalf("browser create/encrypt: %v\npage:\n%s", err, browserText(ctx))
	}
	if err := os.Remove(plainPath); err != nil {
		t.Fatal(err)
	}
	err = chromedp.Run(ctx,
		chromedp.Click(`//div[@id="key-body"]//button[normalize-space()="解开"]`, chromedp.BySearch),
		chromedp.WaitVisible(`#key-pw`, chromedp.ByQuery),
		chromedp.SendKeys(`#key-pw`, keyPassword, chromedp.ByQuery),
		chromedp.Click(`#key-dlg-ok`, chromedp.ByQuery),
		chromedp.Poll(`!document.getElementById("dlg-key").open && document.body.innerText.includes("已写回文件")`, nil, chromedp.WithPollingInterval(100*time.Millisecond)),
	)
	if err != nil {
		t.Fatalf("browser unlock: %v\npage:\n%s", err, browserText(ctx))
	}
	restored, err := os.ReadFile(plainPath)
	if err != nil {
		t.Fatalf("plaintext was not written back: %v\npage:\n%s", err, browserText(ctx))
	}
	if string(restored) != plaintext {
		t.Fatalf("restored = %q", restored)
	}
	var body string
	if err := chromedp.Run(ctx, chromedp.Text(`#key-body`, &body, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, plaintext) || strings.Contains(body, keyPassword) {
		t.Fatalf("page showed secret material: %q", body)
	}
}

func newKeyBrowser(t *testing.T) (context.Context, context.CancelFunc) {
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

func browserText(ctx context.Context) string {
	var text string
	_ = chromedp.Run(ctx, chromedp.Evaluate(`document.body ? document.body.innerText : ""`, &text))
	if len(text) > 2000 {
		text = text[:2000]
	}
	return text
}
