package cli

import (
	"strings"
	"testing"
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
	neither, err := parseOptions(CommandAgent, []string{"--token", "t"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCommandOptions(CommandAgent, neither); err == nil {
		t.Fatal("no mode should be a usage error")
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
