package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
)

func TestAddMissingAdaptersLogsHubConfigValidationProblems(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return t.TempDir()
		}
		return os.Getenv(key)
	})

	if err := addMissingAdapters(paths, []byte(`{"version":1,"adapters":{"pi":{"root":"x","categories":{}}}}`)); err != nil {
		t.Fatalf("addMissingAdapters() error = %v", err)
	}
	if got := output.String(); !strings.Contains(got, "invalid hub homer.json config") || !strings.Contains(got, "categories") {
		t.Fatalf("hub config validation problems were not logged: %q", got)
	}
}

// A fresh machine (no homer.json yet) must answer status with a LEGAL
// report: the built-in adapters fall back to defaults and scan what is
// actually installed (the user story: "为什么不是直接扫描他的 pi 当前
// 安装的插件"), plus an Errors marker line for the console badge. The
// 502 the user saw came from treating this as an executor error.
func TestStatusOnFreshMachineIsLegalEmpty(t *testing.T) {
	home := t.TempDir() // no homer.json anywhere
	executor := NewLocalExecutorWithHub(home, "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	report, err := executor.Status(ctx)
	if err != nil {
		t.Fatalf("fresh machine status must not error: %v", err)
	}
	// Errors must carry the fresh-machine marker (drift badge keys on
	// it) — even though the scan itself succeeded via defaults.
	marked := false
	for _, message := range report.Errors {
		if strings.Contains(message, "未找到 homer 配置") {
			marked = true
		}
	}
	if !marked {
		t.Fatalf("fresh machine report must carry the marker in Errors: %+v", report.Errors)
	}
}

// 收取 on a machine that has tool files but no homer.json must initialize
// and upload. The console enables that button on "新机器 · 等待下发".
func TestPushBootstrapsFreshMachine(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	settingsDir := filepath.Join(userHome, ".pi", "agent")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const marker = `{"marker":"from-fresh"}`
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var sawMarker bool
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/snapshot" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var payload struct {
			Store map[string]map[string]string `json:"store"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode upload: %v", err)
		}
		if strings.Contains(payload.Store["pi"]["settings/settings.json"], "from-fresh") {
			sawMarker = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"generation":1}`))
	}))
	defer hub.Close()

	homerHome := filepath.Join(userHome, ".homer")
	executor := NewLocalExecutorWithHub(homerHome, hub.URL, "secret")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := executor.Push(ctx, true, nil, false, false)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if !report.OK || report.Status != commands.PushStatusPushed {
		t.Fatalf("push report = %+v", report)
	}
	if _, err := os.Stat(filepath.Join(homerHome, "homer.json")); err != nil {
		t.Fatalf("homer.json was not created: %v", err)
	}
	if !sawMarker {
		t.Fatal("hub did not receive the fresh machine's settings.json")
	}
}
