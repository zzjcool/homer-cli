package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
)

func TestPairCommandIsRegisteredInFrozenOrderAndUsage(t *testing.T) {
	secretIndex, pairIndex, versionIndex := -1, -1, -1
	for index, command := range COMMANDS {
		switch command {
		case CommandSecret:
			secretIndex = index
		case CommandPair:
			pairIndex = index
		case CommandVersion:
			versionIndex = index
		}
	}
	if !(secretIndex >= 0 && secretIndex < pairIndex && pairIndex < versionIndex) {
		t.Fatalf("COMMANDS pair order = %#v", COMMANDS)
	}
	if !strings.Contains(USAGE, "  pair      在线配对另一台机器") || !strings.Contains(USAGE, "homer pair <tc-addr>") {
		t.Fatalf("top-level USAGE missing pair: %q", USAGE)
	}
	wantUsage := "用法: homer pair [options]           （机器 A：serve，输出一次性地址并等待）\n      homer pair <tc-addr> [options]  （机器 B：join，完成配对后退出）"
	if got := commandUsage(CommandPair); got != wantUsage {
		t.Fatalf("pair commandUsage = %q, want %q", got, wantUsage)
	}
	if got := SplitCommand([]string{"pair"}); got.Command != CommandPair {
		t.Fatalf("SplitCommand pair = %#v", got)
	}
	if got := SplitCommand([]string{"pair", "address"}); len(got.Rest) != 1 || got.Rest[0] != "address" {
		t.Fatalf("SplitCommand pair args = %#v", got)
	}
	if !strings.Contains(commands.PAIR_USAGE, "勿粘贴到 git") && !strings.Contains(commands.PAIR_USAGE, "勿入 git") {
		t.Fatalf("PAIR_USAGE missing address safety warning")
	}
}

func TestPairArgumentValidationTable(t *testing.T) {
	oldPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })
	if err := os.Setenv("PATH", t.TempDir()); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		stdoutWant string
		stderrWant string
	}{
		{
			name:       "serve no args",
			args:       []string{"homer", "pair"},
			wantCode:   1,
			stdoutWant: string(commands.PairServeStatusNoTailcat),
			stderrWant: "homer secret push",
		},
		{
			name:       "join one addr",
			args:       []string{"homer", "pair", "tc-test"},
			wantCode:   1,
			stdoutWant: string(commands.PairJoinStatusNoTailcat),
			stderrWant: "homer secret pull",
		},
		{
			name:       "two positionals",
			args:       []string{"homer", "pair", "first", "second"},
			wantCode:   1,
			stderrWant: "多余的参数: second",
		},
		{
			name:       "unknown flag",
			args:       []string{"homer", "pair", "--not-a-pair-flag"},
			wantCode:   1,
			stderrWant: "未知选项",
		},
		{
			name:       "unsupported flag",
			args:       []string{"homer", "pair", "--offline"},
			wantCode:   1,
			stderrWant: "不支持选项 --offline",
		},
		{
			name:       "shared options",
			args:       []string{"homer", "pair", "--home", t.TempDir(), "--yes", "--json"},
			wantCode:   1,
			stdoutWant: `"status": "no-tailcat"`,
			stderrWant: "https://github.com/tailscale/tailcat",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := runWithIO(test.args, &out, &errOut)
			if code != test.wantCode {
				t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", code, test.wantCode, out.String(), errOut.String())
			}
			if test.stdoutWant != "" && !strings.Contains(out.String(), test.stdoutWant) {
				t.Fatalf("stdout = %q, want %q", out.String(), test.stdoutWant)
			}
			if test.stderrWant != "" && !strings.Contains(errOut.String(), test.stderrWant) {
				t.Fatalf("stderr = %q, want %q", errOut.String(), test.stderrWant)
			}
		})
	}
}

func TestPairHelpAndJSONSmoke(t *testing.T) {
	var helpOut, helpErr bytes.Buffer
	if code := runWithIO([]string{"homer", "pair", "--help"}, &helpOut, &helpErr); code != 0 {
		t.Fatalf("pair --help exit = %d, stderr=%q", code, helpErr.String())
	}
	if helpErr.Len() != 0 || !strings.Contains(helpOut.String(), "homer pair <tc-addr>") || !strings.Contains(helpOut.String(), "勿粘贴到 git") {
		t.Fatalf("pair --help output = %q / %q", helpOut.String(), helpErr.String())
	}

	oldPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })
	if err := os.Setenv("PATH", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	var jsonOut, jsonErr bytes.Buffer
	if code := runWithIO([]string{"homer", "pair", "--json"}, &jsonOut, &jsonErr); code != 1 {
		t.Fatalf("pair --json exit = %d, stdout=%q stderr=%q", code, jsonOut.String(), jsonErr.String())
	}
	value := map[string]any{}
	if err := json.Unmarshal(jsonOut.Bytes(), &value); err != nil {
		t.Fatalf("pair --json is not JSON: %v\n%s", err, jsonOut.String())
	}
	if value["ok"] != false || value["status"] != string(commands.PairServeStatusNoTailcat) {
		t.Fatalf("pair --json report = %#v", value)
	}
	if !strings.Contains(jsonErr.String(), "homer secret push") {
		t.Fatalf("pair --json stderr = %q", jsonErr.String())
	}
}
