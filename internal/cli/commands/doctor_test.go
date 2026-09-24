package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gitx"
)

func doctorTestPaths(t *testing.T) core.HomerPaths {
	t.Helper()
	home := t.TempDir()
	return core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return home
		}
		return os.Getenv(name)
	})
}

func doctorConfig(t *testing.T, paths core.HomerPaths, root string) {
	t.Helper()
	config := core.HomerConfig{
		Version: 1,
		Adapters: map[string]core.AdapterConfig{
			"pi": {Root: root, Categories: map[string]core.CategoryConfig{"settings": {Paths: []string{"settings.json"}, Mode: core.SyncModeMerge}}},
		},
	}
	if err := core.SaveConfig(paths, config); err != nil {
		t.Fatal(err)
	}
}

func TestRunDoctorOrderSkipJSONAndExit(t *testing.T) {
	paths := doctorTestPaths(t)
	report := RunDoctor(DoctorOptions{HomerHome: paths.Home, Offline: true}, nil)
	wantIDs := []string{"config", "repo", "store-clean", "remote", "adapters", "age", "machine", "required"}
	if len(report.Checks) != len(wantIDs) {
		t.Fatalf("checks len = %d, report = %#v", len(report.Checks), report)
	}
	for index, want := range wantIDs {
		if string(report.Checks[index].ID) != want {
			t.Fatalf("check %d id = %s, want %s", index, report.Checks[index].ID, want)
		}
	}
	if report.OK || report.Checks[0].Status != "fail" {
		t.Fatalf("missing config report = %#v", report)
	}
	for _, id := range []int{4, 5, 7} {
		if !strings.Contains(report.Checks[id].Message, "已跳过") {
			t.Fatalf("check %d was not skipped: %#v", id, report.Checks[id])
		}
	}

	var out bytes.Buffer
	if code := ExecuteDoctor(DoctorOptions{HomerHome: paths.Home, Offline: true, JSON: true}, nil, &out); code != 1 {
		t.Fatalf("doctor fail exit = %d", code)
	}
	var parsed struct {
		Checks []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("doctor JSON: %v (%s)", err, out.String())
	}
	if parsed.OK || len(parsed.Checks) != 8 || parsed.Checks[0].Status != "fail" {
		t.Fatalf("parsed doctor = %#v", parsed)
	}
}

func TestRunDoctorHealthyAndWarnExit(t *testing.T) {
	paths := doctorTestPaths(t)
	root := filepath.Join(paths.Home, "tool")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	doctorConfig(t, paths, root)
	if err := gitx.EnsureGitRepo(paths.Home); err != nil {
		t.Fatal(err)
	}
	// An unborn repository is enough to exercise the all-warnings path while
	// keeping the test local and avoiding any network access.
	var out bytes.Buffer
	code := ExecuteDoctor(DoctorOptions{HomerHome: paths.Home, Offline: true}, nil, &out)
	if code != 0 {
		t.Fatalf("warn-only doctor exit = %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "⚠") || !strings.Contains(out.String(), "homer doctor") {
		t.Fatalf("doctor text = %s", out.String())
	}
}

func TestRenderDoctorReport(t *testing.T) {
	report := coreDoctorReportForTest()
	text := RenderDoctorReport(report)
	for _, want := range []string{"homer doctor: 存在问题", "✗ config", "- 修复配置", "合计: ok 1  warn 1  fail 1", "有 fail 项"} {
		if !strings.Contains(text, want) {
			t.Fatalf("render missing %q: %s", want, text)
		}
	}
}

func coreDoctorReportForTest() DoctorReport {
	return DoctorReport{
		OK: false,
		Checks: []DoctorCheck{
			{ID: "config", Status: "fail", Message: "坏配置", Details: []string{"修复配置"}},
			{ID: "repo", Status: "ok", Message: "git 仓库"},
			{ID: "store-clean", Status: "warn", Message: "store 脏"},
		},
	}
}
