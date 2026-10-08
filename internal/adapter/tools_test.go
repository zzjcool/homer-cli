package adapter

import (
	"runtime"
	"strings"
	"testing"
)

func TestOfficialInstallPi(t *testing.T) {
	got, ok := OfficialInstall("pi")
	if !ok {
		t.Fatal("pi installer missing")
	}
	if !strings.Contains(got, "https://pi.dev/install") {
		t.Fatalf("install = %q", got)
	}
	if runtime.GOOS == "windows" {
		if !strings.Contains(got, "install.ps1") {
			t.Fatalf("windows install = %q", got)
		}
		return
	}
	if got != "curl -fsSL https://pi.dev/install.sh | sh" {
		t.Fatalf("install = %q", got)
	}
}

func TestOfficialInstallUnknown(t *testing.T) {
	if _, ok := OfficialInstall("code"); ok {
		t.Fatal("code has no official one-line installer registered")
	}
	if _, ok := OfficialInstall("definitely-not-a-tool"); ok {
		t.Fatal("unknown binary reported an installer")
	}
}

func TestToolsRegistryInvariants(t *testing.T) {
	seenID := map[string]bool{}
	seenBinary := map[string]bool{}
	for _, tool := range Tools() {
		if tool.ID == "" || tool.Adapter == "" || tool.Label == "" || tool.Binary == "" {
			t.Fatalf("incomplete tool: %+v", tool)
		}
		if len(tool.VersionArgs) == 0 {
			t.Fatalf("%s has no version arguments, so its version cannot be reported", tool.ID)
		}
		if seenID[tool.ID] || seenBinary[tool.Binary] {
			t.Fatalf("duplicate tool %s / %s", tool.ID, tool.Binary)
		}
		seenID[tool.ID] = true
		seenBinary[tool.Binary] = true
		for _, arg := range append(append([]string{}, tool.VersionArgs...), tool.UpgradeArgs...) {
			if strings.ContainsAny(arg, " \t\n|;&$`") {
				t.Fatalf("%s argument %q needs a shell; arguments are passed verbatim", tool.ID, arg)
			}
		}
		if tool.CanUpgrade() != (len(tool.UpgradeArgs) > 0) {
			t.Fatalf("%s CanUpgrade disagrees with UpgradeArgs", tool.ID)
		}
	}
	for _, id := range []string{"pi", "herdr", "opencode", "vscode"} {
		if !seenID[id] {
			t.Fatalf("built-in adapter %s has no registered tool", id)
		}
	}
}

func TestToolsReturnsACopy(t *testing.T) {
	first := Tools()
	first[0].ID = "tampered"
	first[0].UpgradeArgs[0] = "rm"
	again := Tools()
	if again[0].ID == "tampered" || again[0].UpgradeArgs[0] == "rm" {
		t.Fatalf("Tools() leaked internal state: %+v", again[0])
	}
}

func TestToolByID(t *testing.T) {
	pi, ok := ToolByID("pi")
	if !ok || pi.Binary != "pi" || !pi.CanUpgrade() {
		t.Fatalf("pi = %+v ok=%v", pi, ok)
	}
	if got := strings.Join(pi.UpgradeArgs, " "); got != "update --self" {
		t.Fatalf("pi upgrade = %q, want the self-only update", got)
	}
	vscode, ok := ToolByID("vscode")
	if !ok || vscode.Binary != "code" || vscode.CanUpgrade() {
		t.Fatalf("vscode = %+v ok=%v (version only: the OS package manager updates it)", vscode, ok)
	}
	if _, ok := ToolByID("rm"); ok {
		t.Fatal("an arbitrary program resolved as a tool")
	}
	if _, ok := ToolByID(""); ok {
		t.Fatal("empty id resolved as a tool")
	}
}
