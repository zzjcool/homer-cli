package toolctl

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/shellenv"
)

// fakeBin writes an executable script. Scripts use only shell builtins and
// absolute paths, and every test searches ONLY its own directory, so the
// results never depend on which of pi/herdr/opencode/code the machine running
// the tests happens to have.
func fakeBin(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// piScript is a stand-in for pi: the version lives in a file next to it, and
// "update --self" replaces it with the contents of ./next.
func piScript(dir string) string {
	return `dir="` + dir + `"
echo "$@" >> "$dir/pi.args"
echo "CI=$CI" >> "$dir/pi.env"
case "$1" in
  --version) read -r v < "$dir/pi.version"; echo "$v" ;;
  update)
    echo "Downloading pi..."
    if [ -f "$dir/next" ]; then read -r n < "$dir/next"; echo "$n" > "$dir/pi.version"; fi
    echo "done" ;;
esac
`
}

func setVersion(t *testing.T, dir, file, version string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustTool(t *testing.T, id string) adapter.Tool {
	t.Helper()
	tool, ok := adapter.ToolByID(id)
	if !ok {
		t.Fatalf("tool %s is not registered", id)
	}
	return tool
}

func TestParseVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0.87.1\n", "0.87.1"},
		{"herdr 0.9.1\n", "0.9.1"},
		{"1.138.0\n7debcd0e2acdea1c52de81bf9ee1620444407dda\nx64\n", "1.138.0"},
		{"v2.3.4", "2.3.4"},
		{"pi/0.87.1 linux-x64 node-v22.19.0", "0.87.1"},
		{"2.0.0-beta.1 (build 7)", "2.0.0-beta.1"},
		{"1.2.3.4", "1.2.3.4"},
		{"no version here", ""},
		{"version 7", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := ParseVersion(tc.in); got != tc.want {
			t.Errorf("ParseVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestProbeReadsTheVersion(t *testing.T) {
	dir := t.TempDir()
	fakeBin(t, dir, "pi", "echo 'pi 0.87.1'\n")
	status, ok := Probe(context.Background(), mustTool(t, "pi"), dir)
	if !ok {
		t.Fatal("installed pi reported as missing")
	}
	want := Status{ID: "pi", Adapter: "pi", Label: "pi", Version: "0.87.1", Upgradable: true}
	if status != want {
		t.Fatalf("status = %+v, want %+v", status, want)
	}
}

func TestProbeSkipsAMissingProgram(t *testing.T) {
	if status, ok := Probe(context.Background(), mustTool(t, "pi"), t.TempDir()); ok {
		t.Fatalf("missing pi was reported: %+v", status)
	}
}

func TestProbeFallsBackToStderr(t *testing.T) {
	dir := t.TempDir()
	fakeBin(t, dir, "herdr", "echo '0.5.0' >&2\n")
	status, ok := Probe(context.Background(), mustTool(t, "herdr"), dir)
	if !ok || status.Version != "0.5.0" {
		t.Fatalf("status = %+v ok=%v", status, ok)
	}
}

func TestProbeStdoutBeatsAWarningOnStderr(t *testing.T) {
	dir := t.TempDir()
	fakeBin(t, dir, "pi", "echo '(node:1) Warning: Node.js v22.1.0 is old' >&2\necho '0.87.1'\n")
	status, _ := Probe(context.Background(), mustTool(t, "pi"), dir)
	if status.Version != "0.87.1" {
		t.Fatalf("version = %q, want the one on stdout", status.Version)
	}
}

func TestProbeReportsAProgramThatCannotAnswer(t *testing.T) {
	cases := []struct {
		name, script, want string
	}{
		{"fails", "echo 'boom: unknown flag' >&2\nexit 2\n", "boom: unknown flag"},
		{"prints no version", "echo hello\n", "输出里没有版本号"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fakeBin(t, dir, "pi", tc.script)
			status, ok := Probe(context.Background(), mustTool(t, "pi"), dir)
			if !ok {
				t.Fatal("an installed program must still be reported")
			}
			if status.Version != "" || !strings.Contains(status.Error, tc.want) {
				t.Fatalf("status = %+v, want an error mentioning %q", status, tc.want)
			}
		})
	}
}

func TestProbeStopsAHungProgram(t *testing.T) {
	old := probeTimeout
	probeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { probeTimeout = old })
	dir := t.TempDir()
	fakeBin(t, dir, "pi", "exec /bin/sleep 30\n")
	started := time.Now()
	status, ok := Probe(context.Background(), mustTool(t, "pi"), dir)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("probe took %s; the timeout did not stop it", elapsed)
	}
	if !ok || !strings.Contains(status.Error, "还没有结束") {
		t.Fatalf("status = %+v ok=%v", status, ok)
	}
}

func TestProbeAllReturnsOnlyInstalledProgramsInRegistryOrder(t *testing.T) {
	dir := t.TempDir()
	fakeBin(t, dir, "code", "echo 1.138.0\necho 7debcd0e\necho x64\n")
	fakeBin(t, dir, "pi", "echo 0.87.1\n")
	got := ProbeAll(context.Background(), dir)
	if len(got) != 2 || got[0].ID != "pi" || got[1].ID != "vscode" {
		t.Fatalf("ProbeAll = %+v, want pi then vscode", got)
	}
	if got[0].Version != "0.87.1" || !got[0].Upgradable {
		t.Fatalf("pi = %+v", got[0])
	}
	if got[1].Version != "1.138.0" || got[1].Upgradable || got[1].Label != "VS Code" {
		t.Fatalf("vscode = %+v (version only, it is updated by the package manager)", got[1])
	}
	if empty := ProbeAll(context.Background(), t.TempDir()); len(empty) != 0 {
		t.Fatalf("machine without any tool reported %+v", empty)
	}
}

func TestBehind(t *testing.T) {
	cases := []struct {
		version, reference string
		want               bool
	}{
		{"0.80.0", "0.87.1", true},
		{"0.87.1", "0.87.1", false},
		{"0.88.0", "0.87.1", false},
		{"0.9.0", "0.10.0", true}, // numeric, not lexical
		{"1.2.3-beta.1", "1.2.3", true},
		{"", "0.87.1", false},
		{"0.80.0", "", false},
	}
	for _, tc := range cases {
		if got := Behind(tc.version, tc.reference); got != tc.want {
			t.Errorf("Behind(%q, %q) = %v, want %v", tc.version, tc.reference, got, tc.want)
		}
	}
}

func TestReferenceIsTheNewestReleaseInTheFleet(t *testing.T) {
	fleet := [][]Status{
		{{ID: "pi", Version: "0.80.0"}, {ID: "vscode", Version: "1.100.0"}},
		{{ID: "pi", Version: "0.87.1"}},
		{{ID: "pi", Version: "0.88.0-beta.1"}, {ID: "herdr", Error: "读不出版本"}},
		nil,
	}
	got := Reference(fleet)
	if got["pi"] != "0.87.1" {
		t.Fatalf("pi reference = %q; a beta must not make stable machines look old", got["pi"])
	}
	if got["vscode"] != "1.100.0" {
		t.Fatalf("vscode reference = %q", got["vscode"])
	}
	if _, ok := got["herdr"]; ok {
		t.Fatal("a program with no readable version produced a reference")
	}
}

func TestReferenceHonorsADeclaredFloor(t *testing.T) {
	tools := []adapter.Tool{{ID: "x", MinVersion: "2.0.0"}}
	if got := reference([][]Status{{{ID: "x", Version: "1.5.0"}}}, tools)["x"]; got != "2.0.0" {
		t.Fatalf("below the floor: reference = %q, want the floor", got)
	}
	if got := reference([][]Status{{{ID: "x", Version: "2.5.0"}}}, tools)["x"]; got != "2.5.0" {
		t.Fatalf("above the floor: reference = %q, want the fleet's newest", got)
	}
	if got := reference(nil, tools)["x"]; got != "2.0.0" {
		t.Fatalf("empty fleet: reference = %q, want the floor", got)
	}
	// A floor may itself be a prerelease: it is the maintainer's statement.
	pre := []adapter.Tool{{ID: "x", MinVersion: "3.0.0-rc.1"}}
	if got := reference(nil, pre)["x"]; got != "3.0.0-rc.1" {
		t.Fatalf("prerelease floor = %q", got)
	}
}

func TestUpgradeInvalidatesCachedLoginPATH(t *testing.T) {
	shellenv.Invalidate()
	previousRead := shellenv.ReadLoginPATH
	previousTTL := shellenv.LoginPATHTTL
	calls := 0
	firstLoginPath := t.TempDir()
	secondLoginPath := t.TempDir()
	shellenv.LoginPATHTTL = time.Minute
	shellenv.ReadLoginPATH = func() string {
		calls++
		if calls == 1 {
			return firstLoginPath
		}
		return secondLoginPath
	}
	t.Cleanup(func() {
		shellenv.ReadLoginPATH = previousRead
		shellenv.LoginPATHTTL = previousTTL
		shellenv.Invalidate()
	})

	if got := shellenv.Path(); !strings.Contains(got, firstLoginPath) {
		t.Fatalf("initial PATH = %q, want login PATH %q", got, firstLoginPath)
	}
	dir := t.TempDir()
	fakeBin(t, dir, "pi", piScript(dir))
	setVersion(t, dir, "pi.version", "0.80.0")
	setVersion(t, dir, "next", "0.90.2")
	if result := Upgrade(context.Background(), "pi", dir); !result.OK {
		t.Fatalf("Upgrade result = %+v, want successful upgrade", result)
	}
	if got := shellenv.Path(); !strings.Contains(got, secondLoginPath) {
		t.Fatalf("PATH after Upgrade = %q, want refreshed login PATH %q", got, secondLoginPath)
	}
	if calls != 2 {
		t.Fatalf("login PATH reads after Upgrade = %d, want 2", calls)
	}
}

func TestUpgradeRefusesAnUnknownTool(t *testing.T) {
	for _, id := range []string{"", "rm", "pi; rm -rf /", "../pi", "PI"} {
		result := Upgrade(context.Background(), id, t.TempDir())
		if result.OK || result.Status != "unknown-tool" {
			t.Fatalf("Upgrade(%q) = %+v", id, result)
		}
	}
}

func TestUpgradeNeedsTheProgramInstalled(t *testing.T) {
	result := Upgrade(context.Background(), "pi", t.TempDir())
	if result.OK || result.Status != "not-installed" {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Manual, "pi.dev/install") {
		t.Fatalf("manual = %q, want the official installer", result.Manual)
	}
}

func TestUpgradeLeavesAVersionOnlyProgramAlone(t *testing.T) {
	dir := t.TempDir()
	fakeBin(t, dir, "code", `echo "$@" >> "`+dir+`/code.args"`+"\necho 1.138.0\n")
	result := Upgrade(context.Background(), "vscode", dir)
	if result.OK || result.Status != "unsupported" || result.Before != "1.138.0" {
		t.Fatalf("result = %+v", result)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "code.args"))
	if strings.TrimSpace(string(args)) != "--version" {
		t.Fatalf("code was invoked with %q; only the version check may run", args)
	}
}

func TestUpgradeRunsExactlyTheRegisteredCommand(t *testing.T) {
	dir := t.TempDir()
	fakeBin(t, dir, "pi", piScript(dir))
	setVersion(t, dir, "pi.version", "0.80.0")
	setVersion(t, dir, "next", "0.90.2")

	result := Upgrade(context.Background(), "pi", dir)
	if !result.OK || result.Status != "upgraded" || result.Before != "0.80.0" || result.After != "0.90.2" {
		t.Fatalf("result = %+v", result)
	}
	if result.Note != "pi 0.80.0 → 0.90.2" {
		t.Fatalf("note = %q", result.Note)
	}
	if !strings.Contains(result.Output, "Downloading pi...") {
		t.Fatalf("output = %q, want what the command printed", result.Output)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "pi.args"))
	// First the version check, then the upgrade, then the version check again.
	if got := strings.Fields(strings.ReplaceAll(string(args), "\n", " | ")); strings.Join(got, " ") != "--version | update --self | --version |" {
		t.Fatalf("invocations = %q", args)
	}
	envLog, _ := os.ReadFile(filepath.Join(dir, "pi.env"))
	if !strings.Contains(string(envLog), "CI=1") {
		t.Fatalf("the upgrade must run non-interactively; env log = %q", envLog)
	}
}

func TestUpgradeReportsAnUnchangedVersion(t *testing.T) {
	dir := t.TempDir()
	fakeBin(t, dir, "pi", piScript(dir))
	setVersion(t, dir, "pi.version", "0.90.2")
	result := Upgrade(context.Background(), "pi", dir)
	if !result.OK || result.Status != "unchanged" || result.After != "0.90.2" {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Note, "版本没有变化（0.90.2）") || !strings.Contains(result.Note, "done") {
		t.Fatalf("note = %q, want the version and what the tool said", result.Note)
	}
}

func TestUpgradeReportsAFailureWithItsReasonAndAWayOut(t *testing.T) {
	dir := t.TempDir()
	fakeBin(t, dir, "pi", `case "$1" in
  --version) echo 0.80.0 ;;
  update) echo "fetching" ; echo "npm ERR! network request failed" >&2 ; exit 1 ;;
esac
`)
	result := Upgrade(context.Background(), "pi", dir)
	if result.OK || result.Status != "failed" || result.Before != "0.80.0" {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Note, "npm ERR! network request failed") {
		t.Fatalf("note = %q, want the tool's own last words", result.Note)
	}
	if !strings.Contains(result.Output, "fetching") || result.Manual == "" {
		t.Fatalf("result = %+v, want the output and the manual installer", result)
	}
}

func TestUpgradeStopsACommandThatNeverEnds(t *testing.T) {
	old := upgradeTimeout
	upgradeTimeout = 400 * time.Millisecond
	t.Cleanup(func() { upgradeTimeout = old })
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	// The command leaves a descendant behind, as installers do.
	fakeBin(t, dir, "pi", `case "$1" in
  --version) echo 0.80.0 ;;
  update) /bin/sleep 60 &
    echo $! > "`+pidFile+`"
    wait ;;
esac
`)
	started := time.Now()
	result := Upgrade(context.Background(), "pi", dir)
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("upgrade took %s; the timeout did not stop it", elapsed)
	}
	if result.OK || result.Status != "failed" || !strings.Contains(result.Note, "还没有结束") {
		t.Fatalf("result = %+v", result)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the command never started its child: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			break // gone
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant %d survived the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestUpgradeGivesTheCommandNoInput(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "stdin")
	fakeBin(t, dir, "pi", `case "$1" in
  --version) echo 0.80.0 ;;
  update) if read -r line; then echo got-input > "`+marker+`"; else echo no-input > "`+marker+`"; fi ;;
esac
`)
	done := make(chan UpgradeResult, 1)
	go func() { done <- Upgrade(context.Background(), "pi", dir) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the upgrade waited for input that can never come")
	}
	if got, _ := os.ReadFile(marker); strings.TrimSpace(string(got)) != "no-input" {
		t.Fatalf("stdin marker = %q", got)
	}
}

func TestUpgradeRunsOneAtATime(t *testing.T) {
	dir := t.TempDir()
	fakeBin(t, dir, "pi", piScript(dir))
	setVersion(t, dir, "pi.version", "0.80.0")
	upgradeSlot <- struct{}{}
	defer func() { <-upgradeSlot }()
	result := Upgrade(context.Background(), "pi", dir)
	if result.OK || result.Status != "busy" {
		t.Fatalf("result = %+v", result)
	}
	if args, _ := os.ReadFile(filepath.Join(dir, "pi.args")); strings.Contains(string(args), "update") {
		t.Fatalf("a second upgrade ran in parallel: %q", args)
	}
}

func TestUpgradeCancelledByTheCaller(t *testing.T) {
	dir := t.TempDir()
	fakeBin(t, dir, "pi", `case "$1" in
  --version) echo 0.80.0 ;;
  update) exec /bin/sleep 60 ;;
esac
`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan UpgradeResult, 1)
	go func() { done <- Upgrade(ctx, "pi", dir) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case result := <-done:
		if result.OK || result.Status != "failed" {
			t.Fatalf("result = %+v", result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling the caller did not stop the command")
	}
}
