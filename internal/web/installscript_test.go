package web

import (
	"strings"
	"testing"
)

// The one-line install script (Tailscale-style) served by the hub. It must
// embed the hub's own reachability (scheme://host from the request), the
// runtime GOOS/GOARCH pair so agents can self-check, and never the token —
// the token arrives via the installer argument at the agent machine.
func TestRenderInstallScript(t *testing.T) {
	script := RenderInstallScript("https://homerhw.openaaas.org", "linux", "amd64")
	for _, want := range []string{
		"#!/bin/sh",
		"HUB=https://homerhw.openaaas.org",
		"GOOS_EXPECT=linux",
		"GOARCH_EXPECT=amd64",
		"dl/homer",
		"keys/hub-token",
		"homer agent --connect",
		"chmod 600",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("install script missing %q\n%s", want, script)
		}
	}
	if strings.Contains(script, "HOMER_HUB_TOKEN=") {
		t.Fatal("install script must not embed the token")
	}
}

// Platforms other than the hub's own build get the source-build fallback
// instead of a broken binary download.
func TestRenderInstallScriptFallback(t *testing.T) {
	script := RenderInstallScript("http://192.168.8.204:7760", "darwin", "arm64")
	if !strings.Contains(script, "darwin") || !strings.Contains(script, "arm64") {
		t.Fatal("fallback should surface the mismatch pair")
	}
	if !strings.Contains(script, "git clone") || !strings.Contains(script, "go install") {
		t.Fatal("fallback should print build-from-source instructions")
	}
}
