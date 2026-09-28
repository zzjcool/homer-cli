package cli

import (
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

func TestAgentCommandParsesListen(t *testing.T) {
	options, err := parseOptions(CommandAgent, []string{
		"--listen", "0.0.0.0:7761",
		"--advertise", "http://agent-a:7761",
		"--hub", "http://hub:7760",
		"--token", "t",
		"--home", "/tmp/x",
		"--id", "agent-a",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if options.Listen != "0.0.0.0:7761" || options.Advertise != "http://agent-a:7761" || options.Hub != "http://hub:7760" || options.ID != "agent-a" {
		t.Fatalf("agent options = %#v", options)
	}
	if err := validateCommandOptions(CommandAgent, options); err != nil {
		t.Fatal(err)
	}
}

func TestAgentCommandParsesConnect(t *testing.T) {
	options, err := parseOptions(CommandAgent, []string{"--connect", "http://hub:7760", "--token", "t"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if options.Connect != "http://hub:7760" {
		t.Fatalf("connect = %q", options.Connect)
	}
	if err := validateCommandOptions(CommandAgent, options); err != nil {
		t.Fatal(err)
	}
}

func TestAgentCommandRequiresExactlyOneMode(t *testing.T) {
	both, err := parseOptions(CommandAgent, []string{"--listen", ":1", "--connect", "http://x"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCommandOptions(CommandAgent, both); err == nil {
		t.Fatal("listen+connect should be a usage error")
	}
	// Bare `homer agent` (no mode flags) is now legal: it restarts from the
	// persisted join state; agentd.ResolveConfig errors when nothing was
	// ever persisted. Explicit mode flags keep the either-or rule.
	neither, err := parseOptions(CommandAgent, []string{"--token", "t"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCommandOptions(CommandAgent, neither); err != nil {
		t.Fatalf("persisted-restart invocation should parse: %v", err)
	}
	listenOnly, err := parseOptions(CommandAgent, []string{"--listen", "127.0.0.1:7761"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCommandOptions(CommandAgent, listenOnly); err != nil {
		t.Fatalf("listen only should be valid: %v", err)
	}
}

func TestServeUsageText(t *testing.T) {
	usage := commandUsage(CommandServe)
	if !strings.Contains(usage, "--addr") || !strings.Contains(usage, "--token") {
		t.Fatalf("serve usage = %q", usage)
	}
	usage = commandUsage(CommandAgent)
	if !strings.Contains(usage, "--listen") || !strings.Contains(usage, "--connect") {
		t.Fatalf("agent usage = %q", usage)
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
	agentOptions, err := parseOptions(CommandAgent, []string{"--listen", ":1", "--show-join"}, false)
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

func TestJoinCommandHidesTokenFromProcessList(t *testing.T) {
	line := joinCommand("192.168.1.5:7760", "sekret")
	if !strings.Contains(line, "HOMER_HUB_TOKEN=sekret homer agent --connect http://192.168.1.5:7760") {
		t.Fatalf("join command = %q", line)
	}
	// Wildcard binds resolve through LanIPv4 or degrade to a placeholder.
	wild := joinCommand("0.0.0.0:7760", "sekret")
	if !strings.Contains(wild, "homer agent --connect http://") {
		t.Fatalf("wildcard join = %q", wild)
	}
}
