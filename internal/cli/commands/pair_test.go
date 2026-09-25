package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
)

func writeFakeTailcat(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "tailcat")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return binary
}

func TestExecutePairNoTailcatPrintsInstallAndGitFallback(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var out, errOut bytes.Buffer
	code := ExecutePair(PairOptions{HomerHome: t.TempDir()}, nil, &out, &errOut)
	if code != 1 {
		t.Fatalf("missing tailcat exit code = %d, want 1", code)
	}
	if !strings.Contains(out.String(), string(PairServeStatusNoTailcat)) {
		t.Fatalf("stdout = %q, want no-tailcat report", out.String())
	}
	for _, expected := range []string{"https://github.com/tailscale/tailcat", "homer secret push", "homer secret pull"} {
		if !strings.Contains(errOut.String(), expected) {
			t.Fatalf("stderr = %q, want %q", errOut.String(), expected)
		}
	}
}

func TestExecutePairNoConfigAfterTailcatPreflight(t *testing.T) {
	writeFakeTailcat(t)
	var serveOut, serveErr bytes.Buffer
	if code := ExecutePair(PairOptions{HomerHome: t.TempDir()}, nil, &serveOut, &serveErr); code != 1 {
		t.Fatalf("serve no-config exit code = %d, want 1", code)
	}
	if !strings.Contains(serveOut.String(), string(PairServeStatusNoConfig)) || !strings.Contains(serveErr.String(), "未找到 homer 配置") {
		t.Fatalf("serve no-config output = %q / %q", serveOut.String(), serveErr.String())
	}

	var joinOut, joinErr bytes.Buffer
	if code := ExecutePair(PairOptions{HomerHome: t.TempDir(), Addr: "tc-test"}, nil, &joinOut, &joinErr); code != 1 {
		t.Fatalf("join no-config exit code = %d, want 1", code)
	}
	if !strings.Contains(joinOut.String(), string(PairJoinStatusNoConfig)) || !strings.Contains(joinErr.String(), "未找到 homer 配置") {
		t.Fatalf("join no-config output = %q / %q", joinOut.String(), joinErr.String())
	}

	emptyHome := t.TempDir()
	emptyPaths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return emptyHome
		}
		return os.Getenv(name)
	})
	if err := core.SaveConfig(emptyPaths, core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{}}); err != nil {
		t.Fatal(err)
	}
	var emptyOut, emptyErr bytes.Buffer
	if code := ExecutePair(PairOptions{HomerHome: emptyHome}, nil, &emptyOut, &emptyErr); code != 1 {
		t.Fatalf("empty secrets.files exit code = %d, want 1", code)
	}
	if !strings.Contains(emptyOut.String(), string(PairServeStatusNoConfig)) || !strings.Contains(emptyErr.String(), "secrets.files") {
		t.Fatalf("empty secrets.files output = %q / %q", emptyOut.String(), emptyErr.String())
	}
}

func TestPairReportsUsageAndExitCodes(t *testing.T) {
	if (PairServeReport{OK: true}).ExitCode() != 0 || (PairServeReport{}).ExitCode() != 1 {
		t.Fatal("serve report exit code mapping changed")
	}
	if (PairJoinReport{OK: true}).ExitCode() != 0 || (PairJoinReport{}).ExitCode() != 1 {
		t.Fatal("join report exit code mapping changed")
	}
	if !strings.Contains(PAIR_USAGE, "homer pair <tc-addr>") || !strings.Contains(PAIR_USAGE, "https://github.com/tailscale/tailcat") {
		t.Fatalf("PAIR_USAGE missing frozen guidance: %q", PAIR_USAGE)
	}
	serve := RenderPairServeReport(PairServeReport{Status: PairServeStatusNoConfig, Errors: []string{"配置缺失"}})
	if !strings.Contains(serve, "homer pair: no-config") || !strings.Contains(serve, "配置缺失") {
		t.Fatalf("serve render = %q", serve)
	}
	join := RenderPairJoinReport(PairJoinReport{Status: PairJoinStatusNoConfig, Errors: []string{"配置缺失"}})
	if !strings.Contains(join, "homer pair: no-config") || !strings.Contains(join, "配置缺失") {
		t.Fatalf("join render = %q", join)
	}
}
