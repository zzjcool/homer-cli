package cli

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/hub"
)

// T3 W-C: homer serve / homer agent 命令注册与解析（plan §2.6 冻结面）。

func TestCommandListIncludesServeAgent(t *testing.T) {
	found := map[string]bool{}
	for _, cmd := range COMMANDS {
		found[string(cmd)] = true
	}
	if !found["serve"] {
		t.Fatal("COMMANDS missing serve")
	}
	if !found["agent"] {
		t.Fatal("COMMANDS missing agent")
	}
	if !strings.Contains(USAGE, "serve") || !strings.Contains(USAGE, "agent") {
		t.Fatal("USAGE missing serve/agent entries")
	}
}

func TestServeCommandParses(t *testing.T) {
	options, err := parseOptions(CommandServe, []string{"--addr", "127.0.0.1:7760", "--home", "/tmp/x", "--token", "t"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if options.Addr != "127.0.0.1:7760" || options.Home != "/tmp/x" || options.Token != "t" {
		t.Fatalf("serve options = %#v", options)
	}
	if err := validateCommandOptions(CommandServe, options); err != nil {
		t.Fatal(err)
	}
	// --addr=value 形式
	options, err = parseOptions(CommandServe, []string{"--addr=:7760"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if options.Addr != ":7760" {
		t.Fatalf("inline addr = %q", options.Addr)
	}
}

func TestServeRejectsForeignOptions(t *testing.T) {
	options, err := parseOptions(CommandServe, []string{"--yes"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCommandOptions(CommandServe, options); err == nil {
		t.Fatal("serve --yes should be rejected")
	}
	_, err = parseOptions(CommandServe, []string{"--bogus"}, false)
	if err == nil {
		t.Fatal("serve --bogus should be a parse error")
	}
}

func TestAgentFlagsParsesHub(t *testing.T) {
	options, err := parseOptions(CommandAgent, []string{
		"--hub", "http://hub:7760",
		"--data-url", "http://127.0.0.1:7760",
		"--token", "t",
		"--home", "/tmp/x",
		"--id", "agent-a",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if options.Hub != "http://hub:7760" || options.DataURL != "http://127.0.0.1:7760" || options.ID != "agent-a" {
		t.Fatalf("agent options = %#v", options)
	}
	if err := validateCommandOptions(CommandAgent, options); err != nil {
		t.Fatal(err)
	}
}

// Removed agent transport flags receive migration guidance rather than a
// generic unknown-option error, and still exit nonzero through the CLI.
func TestAgentFlagsRejectsRemovedModeFlags(t *testing.T) {
	tests := []struct {
		flag string
		want string
	}{
		{"--connect", "--connect 已移除，请改用 --hub <url>"},
		{"--listen", "--listen 已移除；listen 模式已移除，agent 现在只主动连 hub"},
		{"--advertise", "--advertise 已移除；listen 模式已移除，agent 现在只主动连 hub"},
	}
	for _, test := range tests {
		t.Run(test.flag, func(t *testing.T) {
			_, err := parseOptions(CommandAgent, []string{test.flag, "x"}, false)
			if err == nil || err.Error() != test.want {
				t.Fatalf("parse error = %v, want %q", err, test.want)
			}
			_, err = parseOptions(CommandAgent, []string{test.flag + "=x"}, false)
			if err == nil || err.Error() != test.want {
				t.Fatalf("inline parse error = %v, want %q", err, test.want)
			}

			var out, errOut bytes.Buffer
			if code := runWithIO([]string{"homer", "agent", test.flag, "x"}, &out, &errOut); code == 0 {
				t.Fatalf("removed option exit code = 0, stderr=%q", errOut.String())
			}
			if !strings.Contains(errOut.String(), test.want) {
				t.Fatalf("CLI error = %q, want message %q", errOut.String(), test.want)
			}
		})
	}
}

// Bare `homer agent` restarts from the persisted join state;
// agentd.ResolveConfig errors when nothing was ever persisted.
func TestAgentFlagsBareRestartIsLegal(t *testing.T) {
	bare, err := parseOptions(CommandAgent, []string{"--token", "t"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCommandOptions(CommandAgent, bare); err != nil {
		t.Fatalf("persisted-restart invocation should parse: %v", err)
	}
}

func TestUsageServeAndAgent(t *testing.T) {
	usage := commandUsage(CommandServe)
	if !strings.Contains(usage, "--addr") || !strings.Contains(usage, "--token") {
		t.Fatalf("serve usage = %q", usage)
	}
	usage = commandUsage(CommandAgent)
	if !strings.Contains(usage, "--hub") || !strings.Contains(usage, "--data-url") {
		t.Fatalf("agent usage = %q", usage)
	}
	if strings.Contains(usage, "--listen") || strings.Contains(usage, "--connect") {
		t.Fatalf("agent usage still mentions removed modes: %q", usage)
	}
}

func TestServeShowJoinFlagParses(t *testing.T) {
	options, err := parseOptions(CommandServe, []string{"--show-join", "--addr", "0.0.0.0:7760"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !options.ShowJoin {
		t.Fatal("show-join flag not captured")
	}
	if err := validateCommandOptions(CommandServe, options); err != nil {
		t.Fatalf("show-join should be valid for serve: %v", err)
	}
	// agent must reject show-join (it belongs to serve).
	agentOptions, err := parseOptions(CommandAgent, []string{"--hub", "http://x", "--show-join"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCommandOptions(CommandAgent, agentOptions); err == nil {
		t.Fatal("show-join should be rejected for agent")
	}
}

func TestResolveServeTokenLifecycle(t *testing.T) {
	home := t.TempDir()
	paths := ResolveHomerPaths(home)
	t.Setenv("HOMER_HUB_TOKEN", "")

	// Loopback without any token: stays bare (bare `homer serve` keeps
	// working) and never generates a file.
	token, created, err := resolveServeToken(paths, "", true)
	if err != nil || token != "" || created {
		t.Fatalf("loopback bare = %q created=%v err=%v", token, created, err)
	}
	if _, ok := hub.ReadHubToken(home); ok {
		t.Fatal("loopback must not generate a token file")
	}

	// Non-loopback without any token: generates once, persists, and the
	// next call returns the same value without created=true.
	token, created, err = resolveServeToken(paths, "", false)
	if err != nil || !created || len(token) < 32 {
		t.Fatalf("generate = %q created=%v err=%v", token, created, err)
	}
	again, createdAgain, err := resolveServeToken(paths, "", false)
	if err != nil || again != token || createdAgain {
		t.Fatalf("reload = %q created=%v err=%v", again, createdAgain, err)
	}

	// Explicit --token rotates the persisted file.
	rotated, _, err := resolveServeToken(paths, "rotate-me", false)
	if err != nil || rotated != "rotate-me" {
		t.Fatalf("rotate = %q err=%v", rotated, err)
	}
	persisted, ok := hub.ReadHubToken(home)
	if !ok || persisted != "rotate-me" {
		t.Fatalf("rotation not persisted: %q ok=%v", persisted, ok)
	}

	// Env override wins over the file but leaves it untouched.
	t.Setenv("HOMER_HUB_TOKEN", "env-wins")
	fromEnv, _, err := resolveServeToken(paths, "", false)
	if err != nil || fromEnv != "env-wins" {
		t.Fatalf("env = %q err=%v", fromEnv, err)
	}
	persisted, _ = hub.ReadHubToken(home)
	if persisted != "rotate-me" {
		t.Fatalf("env leaked into file: %q", persisted)
	}
}

// --show-join must print the persisted join command even when the hub is
// already bound to that address. It must not listen.
func TestServeShowJoinDoesNotListenWhenHubIsUp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOMER_HUB_TOKEN", "")
	token, _, err := hub.CreateHubToken(home, false)
	if err != nil {
		t.Fatal(err)
	}

	// Occupy the default serve address, matching a hub that is already up.
	listener, err := net.Listen("tcp", defaultServeAddr)
	if err != nil {
		t.Fatalf("bind %s: %v", defaultServeAddr, err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	var out, errOut bytes.Buffer
	code := runWithIO([]string{"serve", "--show-join", "--home", home}, &out, &errOut)
	if code != 0 {
		t.Fatalf("show-join exit %d stderr %q stdout %q", code, errOut.String(), out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q", errOut.String())
	}
	want := joinCommand(defaultServeAddr, token)
	if strings.TrimSpace(out.String()) != want {
		t.Fatalf("stdout = %q, want %q", strings.TrimSpace(out.String()), want)
	}
	again, ok := hub.ReadHubToken(home)
	if !ok || again != token {
		t.Fatalf("token changed: %q ok=%v", again, ok)
	}

	// An explicit address that is already taken must behave the same way.
	explicit, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = explicit.Close() })
	addr := explicit.Addr().String()
	out.Reset()
	errOut.Reset()
	code = runWithIO([]string{"serve", "--show-join", "--addr", addr, "--home", home}, &out, &errOut)
	if code != 0 {
		t.Fatalf("explicit show-join exit %d stderr %q stdout %q", code, errOut.String(), out.String())
	}
	want = joinCommand(addr, token)
	if strings.TrimSpace(out.String()) != want {
		t.Fatalf("explicit stdout = %q, want %q", strings.TrimSpace(out.String()), want)
	}

	// No persisted token: still must not try to bind, and must not invent one.
	empty := t.TempDir()
	out.Reset()
	errOut.Reset()
	code = runWithIO([]string{"serve", "--show-join", "--home", empty}, &out, &errOut)
	if code == 0 {
		t.Fatal("missing token should fail")
	}
	if strings.Contains(errOut.String(), "监听失败") || strings.Contains(errOut.String(), "address already in use") {
		t.Fatalf("missing token tried to bind: %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "尚未配置 token") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if _, ok := hub.ReadHubToken(empty); ok {
		t.Fatal("show-join generated a token")
	}
}

func TestShowJoinAddrNeverNeedsAListener(t *testing.T) {
	if got := showJoinAddr(""); got != defaultServeAddr {
		t.Fatalf("default = %q", got)
	}
	if got := showJoinAddr(":7760"); got != "0.0.0.0:7760" {
		t.Fatalf(":port = %q", got)
	}
	if got := showJoinAddr("127.0.0.1:7760"); got != "127.0.0.1:7760" {
		t.Fatalf("explicit = %q", got)
	}
}

func TestJoinCommandHidesTokenFromProcessList(t *testing.T) {
	line := joinCommand("192.168.1.5:7760", "sekret")
	if !strings.Contains(line, "HOMER_HUB_TOKEN=sekret homer agent --hub http://192.168.1.5:7760") {
		t.Fatalf("join command = %q", line)
	}
	// Wildcard binds resolve through LanIPv4 or degrade to a placeholder.
	wild := joinCommand("0.0.0.0:7760", "sekret")
	if !strings.Contains(wild, "homer agent --hub http://") {
		t.Fatalf("wildcard join = %q", wild)
	}
}

// install.sh leaves the one-time enrollment code in keys/hub-token and nothing
// removes it after redemption. A restart must therefore NOT present that
// burned code when agent.json already holds the machine's own secret: the hub
// answers 401 and the agent would stay offline forever (it was reproduced with
// a real hub and a real restart on this exact setup).
func TestResolveAgentCredentialBurnedCodeFileDoesNotShadowSecret(t *testing.T) {
	cases := []struct {
		name                      string
		explicit, file, secret    string
		wantToken, wantEnrollCode string
	}{
		{"fresh install: code in file, no secret yet", "", "hr_abc", "", "", "hr_abc"},
		{"RESTART: burned code still in file, secret persisted", "", "hr_abc", "sec-1", "sec-1", ""},
		{"explicit --token code wins even when a secret exists (re-enroll)", "hr_new", "hr_old", "sec-1", "", "hr_new"},
		{"explicit hub token wins", "hubtok", "hr_abc", "sec-1", "hubtok", ""},
		{"file hub token (not a code) is used as-is", "", "hubtok", "sec-1", "hubtok", ""},
		{"only a persisted secret", "", "", "sec-1", "sec-1", ""},
		{"nothing at all", "", "", "", "", ""},
		{"whitespace around values is ignored", "  ", " hr_abc \n", " sec-1 ", "sec-1", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token, code := resolveAgentCredential(tc.explicit, tc.file, tc.secret)
			if token != tc.wantToken || code != tc.wantEnrollCode {
				t.Fatalf("resolveAgentCredential(%q,%q,%q) = (%q,%q), want (%q,%q)",
					tc.explicit, tc.file, tc.secret, token, code, tc.wantToken, tc.wantEnrollCode)
			}
		})
	}
}
