package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/adapter"
)

// standIn is a pretend pi, herdr or opencode. It answers --version from a
// file next to it, and upgrades itself the way the real program does, by
// copying the "release" version over the installed one. It refuses any other
// arguments, so an upgrade that ran the wrong command shows up as a failure.
type standIn struct {
	name    string
	upgrade string // the arguments the real program upgrades itself with
}

var (
	standInPi       = standIn{name: "pi", upgrade: "update --self"}
	standInHerdr    = standIn{name: "herdr", upgrade: "update"}
	standInOpencode = standIn{name: "opencode", upgrade: "upgrade"}
)

const standInScript = `#!/bin/sh
dir=$(dirname "$0")
echo "$*" >> "$dir/.NAME-calls"
if [ "$1" = "--version" ]; then
	printf 'NAME %s\n' "$(cat "$dir/.NAME-version")"
	exit 0
fi
if [ "$*" = "UPGRADE" ]; then
	if [ -f "$dir/.NAME-fail" ]; then
		echo "NAME: could not reach the release server" >&2
		exit 1
	fi
	cp "$dir/.NAME-release" "$dir/.NAME-version"
	echo "NAME is now $(cat "$dir/.NAME-version")"
	exit 0
fi
echo "NAME: unexpected arguments: $*" >&2
exit 2
`

func (s standIn) dir(m machine) string { return filepath.Join(m.fakeHome, ".local", "bin") }

func (s standIn) file(m machine, suffix string) string {
	return filepath.Join(s.dir(m), "."+s.name+"-"+suffix)
}

// install puts the program on the machine at version. An upgrade moves it to
// release.
func (s standIn) install(t *testing.T, m machine, version, release string) {
	t.Helper()
	script := strings.NewReplacer("NAME", s.name, "UPGRADE", s.upgrade).Replace(standInScript)
	if err := os.MkdirAll(s.dir(m), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir(m), s.name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, s.file(m, "version"), version+"\n")
	writeFile(t, s.file(m, "release"), release+"\n")
}

func (s standIn) version(t *testing.T, m machine) string {
	t.Helper()
	data, err := os.ReadFile(s.file(m, "version"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// upgrades is how many times the program was asked to upgrade itself.
func (s standIn) upgrades(t *testing.T, m machine) int {
	t.Helper()
	data, err := os.ReadFile(s.file(m, "calls"))
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == s.upgrade {
			count++
		}
	}
	return count
}

// failUpgrades makes the program's upgrade command exit with an error.
func (s standIn) failUpgrades(t *testing.T, m machine, fail bool) {
	t.Helper()
	if fail {
		writeFile(t, s.file(m, "fail"), "x\n")
		return
	}
	if err := os.Remove(s.file(m, "fail")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// hermeticBin is a directory holding only the system programs the stand-ins
// and git need. The agents are started with it as their whole PATH, so they
// can never see, let alone upgrade, the pi or herdr installed on the machine
// that runs the tests.
func hermeticBin(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sh", "cat", "cp", "dirname", "touch", "git"} {
		target, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("this test needs %s: %v", name, err)
		}
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// skipIfARealToolCouldBeFound keeps the test from ever touching a real
// program. The agents also look in the Homebrew directories whatever their PATH
// says, and a real pi there would be found before the stand-in.
func skipIfARealToolCouldBeFound(t *testing.T) {
	t.Helper()
	for _, dir := range []string{"/home/linuxbrew/.linuxbrew/bin", "/opt/homebrew/bin"} {
		for _, tool := range adapter.Tools() {
			if _, err := os.Stat(filepath.Join(dir, tool.Binary)); err == nil {
				t.Skipf("%s is installed in %s; an agent would find it before the stand-in", tool.Binary, dir)
			}
		}
	}
}

type processLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *processLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *processLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// startHermetic starts homer with a PATH that holds nothing but bin, and no
// login shell to widen it.
func startHermetic(t *testing.T, binary string, m machine, global, bin string, args ...string) *exec.Cmd {
	t.Helper()
	command := exec.Command(binary, args...)
	orphanGuard(command)
	command.Env = append(serveEnv(m, global), "PATH="+bin, "SHELL=/nonexistent")
	logs := &processLog{}
	command.Stdout, command.Stderr = logs, logs
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		if t.Failed() {
			t.Logf("homer %s output:\n%s", strings.Join(args[:2], " "), logs.String())
		}
	})
	return command
}

// hubTool is what the hub's machine list says about one program on one machine.
type hubTool struct {
	ID         string `json:"id"`
	Version    string `json:"version"`
	Latest     string `json:"latest"`
	Outdated   bool   `json:"outdated"`
	Upgradable bool   `json:"upgradable"`
}

type toolHub struct {
	t    *testing.T
	url  string
	auth map[string]string
}

func (h toolHub) do(method, path string, headers map[string]string, body any) (int, []byte) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, h.url+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	response, err := client.Do(request)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	return response.StatusCode, data
}

// tools lists, per machine, the programs the hub knows about.
func (h toolHub) tools() map[string]map[string]hubTool {
	h.t.Helper()
	status, data := h.do(http.MethodGet, "/api/agents", h.auth, nil)
	if status != http.StatusOK {
		h.t.Fatalf("GET /api/agents = %d %s", status, data)
	}
	var payload struct {
		Agents []struct {
			AgentID string    `json:"agentId"`
			Tools   []hubTool `json:"tools"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		h.t.Fatalf("agents JSON: %v\n%s", err, data)
	}
	out := map[string]map[string]hubTool{}
	for _, agent := range payload.Agents {
		byID := map[string]hubTool{}
		for _, tool := range agent.Tools {
			byID[tool.ID] = tool
		}
		out[agent.AgentID] = byID
	}
	return out
}

// upgrade asks the hub to upgrade one program on one machine.
func (h toolHub) upgrade(agentID, tool string) (int, map[string]any) {
	h.t.Helper()
	status, data := h.do(http.MethodPost, "/api/agents/"+agentID+"/tool-upgrade", h.auth, map[string]string{"tool": tool})
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		h.t.Fatalf("upgrade %s/%s = %d, not JSON: %s", agentID, tool, status, data)
	}
	return status, body
}

func (h toolHub) waitFor(what string, timeout time.Duration, ready func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s; the hub says %v", what, h.tools())
}

func field(t *testing.T, body map[string]any, path ...string) any {
	t.Helper()
	var current any = body
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("%v: %T is not an object in %v", path, current, body)
		}
		current = object[key]
	}
	return current
}

func wantTool(t *testing.T, got map[string]map[string]hubTool, agentID, id, version, latest string, outdated bool) {
	t.Helper()
	tool, ok := got[agentID][id]
	if !ok {
		t.Errorf("%s does not report %s; it reports %v", agentID, id, got[agentID])
		return
	}
	if tool.Version != version || tool.Latest != latest || tool.Outdated != outdated || !tool.Upgradable {
		t.Errorf("%s/%s = %+v, want version %s latest %s outdated %v upgradable", agentID, id, tool, version, latest, outdated)
	}
}

// TestToolVersionsAndUpgradeThroughHub is the whole path with real processes:
// agents report the versions of the programs their adapters drive, the hub
// marks the ones that fall behind the fleet, and one request upgrades a
// program on a machine over the outbound WebSocket stream. The programs are
// stand-ins; nothing real is touched.
func TestToolVersionsAndUpgradeThroughHub(t *testing.T) {
	skipIfARealToolCouldBeFound(t)
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = T\n\temail = t@t\n")
	binary := binaryOf(t)
	bin := hermeticBin(t)

	hubMachine := makeMachine(t, root, "H", global, false)
	streamA := makeMachine(t, root, "A", global, false) // behind on pi and herdr
	streamB := makeMachine(t, root, "B", global, false) // behind on pi and opencode
	newest := makeMachine(t, root, "X", global, false)  // runs the newest of everything
	bare := makeMachine(t, root, "Z", global, false)    // has none of the programs

	standInPi.install(t, streamA, "0.80.0", "0.90.2")
	standInHerdr.install(t, streamA, "0.9.0", "0.9.1")
	standInPi.install(t, streamB, "0.70.0", "0.90.2")
	standInOpencode.install(t, streamB, "1.2.0", "1.4.0")
	standInPi.install(t, newest, "0.90.2", "0.90.2")
	standInHerdr.install(t, newest, "0.9.1", "0.9.1")
	standInOpencode.install(t, newest, "1.4.0", "1.4.0")
	// A program the registry has never heard of. If a request could name any
	// command, this is the one it would run.
	for _, m := range []machine{streamA, streamB} {
		script := "#!/bin/sh\ntouch \"$(dirname \"$0\")/.evil-ran\"\n"
		if err := os.WriteFile(filepath.Join(standInPi.dir(m), "evil"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if result := runHomer(t, binary, hubMachine, "init", "--json"); result.code != 0 {
		t.Fatalf("init hub: %s%s", result.stdout, result.stderr)
	}
	token := "tool-e2e-token"
	hubPort := freePort(t)
	hubURL := fmt.Sprintf("http://127.0.0.1:%d", hubPort)
	hubProc := startHomer(t, binary, hubMachine, global,
		"serve", "--addr", fmt.Sprintf("127.0.0.1:%d", hubPort), "--token", token, "--home", hubMachine.homerHome)
	defer stopProc(t, hubProc)
	hub := toolHub{t: t, url: hubURL, auth: map[string]string{"Authorization": "Bearer " + token}}

	startHermetic(t, binary, streamA, global, bin,
		"agent", "--hub", hubURL, "--token", token, "--id", "agent-a", "--home", streamA.homerHome)
	for id, m := range map[string]machine{"agent-b": streamB, "agent-x": newest, "agent-z": bare} {
		startHermetic(t, binary, m, global, bin,
			"agent", "--hub", hubURL, "--token", token, "--id", id, "--home", m.homerHome)
	}
	for _, id := range []string{"agent-a", "agent-b", "agent-x", "agent-z"} {
		waitRegistered(t, hubURL, hub.auth, hubProc, id)
	}
	hub.waitFor("every machine to report its programs", 30*time.Second, func() bool {
		got := hub.tools()
		return len(got["agent-a"]) == 2 && len(got["agent-b"]) == 2 && len(got["agent-x"]) == 3
	})

	t.Run("versions are reported and compared with the fleet", func(t *testing.T) {
		got := hub.tools()
		wantTool(t, got, "agent-a", "pi", "0.80.0", "0.90.2", true)
		wantTool(t, got, "agent-a", "herdr", "0.9.0", "0.9.1", true)
		wantTool(t, got, "agent-b", "pi", "0.70.0", "0.90.2", true)
		wantTool(t, got, "agent-b", "opencode", "1.2.0", "1.4.0", true)
		wantTool(t, got, "agent-x", "pi", "0.90.2", "0.90.2", false)
		wantTool(t, got, "agent-x", "herdr", "0.9.1", "0.9.1", false)
		wantTool(t, got, "agent-x", "opencode", "1.4.0", "1.4.0", false)
		if len(got["agent-z"]) != 0 {
			t.Errorf("a machine with none of the programs reports %v", got["agent-z"])
		}
		for _, id := range []string{"agent-a", "agent-b", "agent-x"} {
			if _, ok := got[id]["evil"]; ok {
				t.Errorf("%s reports a program that is not in the registry", id)
			}
		}
	})

	// psApplications is the last column of `homer ps`, one entry per machine.
	psApplications := func(t *testing.T) []string {
		t.Helper()
		result := runHomer(t, binary, hubMachine, "ps", "--host", hubURL, "--token", token)
		if result.code != 0 {
			t.Fatalf("homer ps: %s%s", result.stdout, result.stderr)
		}
		var cells []string
		for _, line := range strings.Split(strings.TrimSpace(result.stdout), "\n")[1:] {
			columns := strings.Split(line, "\t")
			if len(columns) != 5 {
				t.Fatalf("homer ps row %q has %d columns, want 5", line, len(columns))
			}
			cells = append(cells, columns[4])
		}
		return cells
	}

	t.Run("homer ps lists the versions too", func(t *testing.T) {
		got := psApplications(t)
		for _, want := range []string{
			"pi 0.80.0（落后，最新 0.90.2）, herdr 0.9.0（落后，最新 0.9.1）",
			"pi 0.70.0（落后，最新 0.90.2）, opencode 1.2.0（落后，最新 1.4.0）",
			"pi 0.90.2, herdr 0.9.1, opencode 1.4.0",
			"—",
		} {
			found := false
			for _, cell := range got {
				found = found || cell == want
			}
			if !found {
				t.Errorf("homer ps has no machine with applications %q; it printed %q", want, got)
			}
		}
	})

	t.Run("requests that are not allowed", func(t *testing.T) {
		if status, _ := hub.do(http.MethodPost, "/api/agents/agent-a/tool-upgrade", nil, map[string]string{"tool": "pi"}); status != http.StatusUnauthorized {
			t.Errorf("without the token = %d, want 401", status)
		}
		for _, bad := range []string{"", "PI", "pi;reboot", "../pi", "pi update --self", "-rf", strings.Repeat("a", 80)} {
			if status, _ := hub.upgrade("agent-a", bad); status != http.StatusBadRequest {
				t.Errorf("tool %q = %d, want 400", bad, status)
			}
		}
		if status, _ := hub.upgrade("agent-nowhere", "pi"); status != http.StatusNotFound {
			t.Errorf("unknown machine = %d, want 404", status)
		}
		for _, m := range []machine{streamA, streamB, newest} {
			for _, tool := range []standIn{standInPi, standInHerdr, standInOpencode} {
				if n := tool.upgrades(t, m); n != 0 {
					t.Errorf("%s on %s was upgraded %d times by requests that should have been refused", tool.name, m.name, n)
				}
			}
		}
	})

	t.Run("only registered programs run", func(t *testing.T) {
		for _, id := range []string{"agent-a", "agent-b"} {
			status, body := hub.upgrade(id, "evil")
			if status != http.StatusUnprocessableEntity {
				t.Errorf("%s: unregistered program = %d %v, want 422", id, status, body)
			}
			if got := field(t, body, "report", "status"); got != "unknown-tool" {
				t.Errorf("%s: report status = %v, want unknown-tool", id, got)
			}
		}
		for _, m := range []machine{streamA, streamB} {
			if _, err := os.Stat(filepath.Join(standInPi.dir(m), ".evil-ran")); err == nil {
				t.Errorf("an unregistered program ran on %s", m.name)
			}
		}
	})

	t.Run("a program that is not installed says how to install it", func(t *testing.T) {
		pi, _ := adapter.ToolByID("pi")
		opencode, _ := adapter.ToolByID("opencode")
		for _, tc := range []struct {
			agent, tool, manual string
		}{
			{"agent-z", "pi", pi.Install},             // outbound stream
			{"agent-a", "opencode", opencode.Install}, // outbound stream
		} {
			status, body := hub.upgrade(tc.agent, tc.tool)
			if status != http.StatusUnprocessableEntity {
				t.Errorf("%s/%s = %d %v, want 422", tc.agent, tc.tool, status, body)
				continue
			}
			if got := field(t, body, "report", "status"); got != "not-installed" {
				t.Errorf("%s/%s status = %v, want not-installed", tc.agent, tc.tool, got)
			}
			if got := field(t, body, "report", "manual"); got != tc.manual || tc.manual == "" {
				t.Errorf("%s/%s manual = %q, want the official installer %q", tc.agent, tc.tool, got, tc.manual)
			}
			if got := field(t, body, "error", "code"); got != "tool-upgrade-failed" {
				t.Errorf("%s/%s error code = %v", tc.agent, tc.tool, got)
			}
		}
	})

	t.Run("upgrading agent A over its WebSocket stream", func(t *testing.T) {
		for _, tc := range []struct {
			tool          standIn
			before, after string
		}{
			{standInPi, "0.80.0", "0.90.2"},
			{standInHerdr, "0.9.0", "0.9.1"},
		} {
			status, body := hub.upgrade("agent-a", tc.tool.name)
			if status != http.StatusOK || field(t, body, "ok") != true || field(t, body, "status") != "upgraded" {
				t.Fatalf("upgrade agent-a/%s = %d %v", tc.tool.name, status, body)
			}
			if field(t, body, "before") != tc.before || field(t, body, "after") != tc.after {
				t.Errorf("agent-a/%s went %v → %v, want %s → %s", tc.tool.name, field(t, body, "before"), field(t, body, "after"), tc.before, tc.after)
			}
			if got := tc.tool.version(t, streamA); got != tc.after {
				t.Errorf("the program on the machine is at %s, want %s", got, tc.after)
			}
			if n := tc.tool.upgrades(t, streamA); n != 1 {
				t.Errorf("%s ran its upgrade %d times, want once", tc.tool.name, n)
			}
		}
		got := hub.tools()
		wantTool(t, got, "agent-a", "pi", "0.90.2", "0.90.2", false)
		wantTool(t, got, "agent-a", "herdr", "0.9.1", "0.9.1", false)
	})

	t.Run("upgrading agent B over its WebSocket stream", func(t *testing.T) {
		for _, tc := range []struct {
			tool          standIn
			before, after string
		}{
			{standInPi, "0.70.0", "0.90.2"},
			{standInOpencode, "1.2.0", "1.4.0"},
		} {
			status, body := hub.upgrade("agent-b", tc.tool.name)
			if status != http.StatusOK || field(t, body, "ok") != true || field(t, body, "status") != "upgraded" {
				t.Fatalf("upgrade agent-b/%s = %d %v", tc.tool.name, status, body)
			}
			if field(t, body, "before") != tc.before || field(t, body, "after") != tc.after {
				t.Errorf("agent-b/%s went %v → %v, want %s → %s", tc.tool.name, field(t, body, "before"), field(t, body, "after"), tc.before, tc.after)
			}
			if got := tc.tool.version(t, streamB); got != tc.after {
				t.Errorf("the program on the machine is at %s, want %s", got, tc.after)
			}
			if n := tc.tool.upgrades(t, streamB); n != 1 {
				t.Errorf("%s ran its upgrade %d times, want once", tc.tool.name, n)
			}
		}
		got := hub.tools()
		wantTool(t, got, "agent-b", "pi", "0.90.2", "0.90.2", false)
		wantTool(t, got, "agent-b", "opencode", "1.4.0", "1.4.0", false)
		for id, tools := range got {
			for name, tool := range tools {
				if tool.Outdated {
					t.Errorf("%s/%s is still marked behind after every upgrade: %+v", id, name, tool)
				}
			}
		}
	})

	t.Run("a failed upgrade keeps the version and explains itself", func(t *testing.T) {
		pi, _ := adapter.ToolByID("pi")
		for _, tc := range []struct {
			agent string
			m     machine
			tool  standIn
		}{
			{"agent-x", newest, standInPi},     // outbound stream
			{"agent-a", streamA, standInHerdr}, // outbound stream
		} {
			tc.tool.failUpgrades(t, tc.m, true)
			before := tc.tool.version(t, tc.m)
			status, body := hub.upgrade(tc.agent, tc.tool.name)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("%s/%s = %d %v, want 422", tc.agent, tc.tool.name, status, body)
			}
			if field(t, body, "error", "code") != "tool-upgrade-failed" || field(t, body, "report", "status") != "failed" {
				t.Errorf("%s/%s = %v", tc.agent, tc.tool.name, body)
			}
			message, _ := field(t, body, "error", "message").(string)
			if !strings.Contains(message, "升级没有成功") || !strings.Contains(message, "could not reach the release server") {
				t.Errorf("%s/%s message %q does not say what went wrong", tc.agent, tc.tool.name, message)
			}
			details, _ := field(t, body, "error", "details").([]any)
			if len(details) == 0 || !strings.Contains(fmt.Sprint(details), "could not reach the release server") {
				t.Errorf("%s/%s details = %v, want the end of the command's output", tc.agent, tc.tool.name, details)
			}
			if tc.tool.name == "pi" && field(t, body, "report", "manual") != pi.Install {
				t.Errorf("%s/%s report offers %v, want the official installer", tc.agent, tc.tool.name, field(t, body, "report", "manual"))
			}
			if got := tc.tool.version(t, tc.m); got != before {
				t.Errorf("a failed upgrade moved %s from %s to %s", tc.tool.name, before, got)
			}
			if got := hub.tools()[tc.agent][tc.tool.name].Version; got != before {
				t.Errorf("the hub shows %s/%s at %s after a failed upgrade, want %s", tc.agent, tc.tool.name, got, before)
			}
			tc.tool.failUpgrades(t, tc.m, false)
		}
	})

	t.Run("an upgrade that changes nothing says so", func(t *testing.T) {
		status, body := hub.upgrade("agent-x", "pi")
		if status != http.StatusOK || field(t, body, "ok") != true || field(t, body, "status") != "unchanged" {
			t.Fatalf("upgrade of a current pi = %d %v, want 200 unchanged", status, body)
		}
		if got := hub.tools()["agent-x"]["pi"].Version; got != "0.90.2" {
			t.Errorf("pi on agent-x = %s", got)
		}
	})

	t.Run("homer ps no longer shows anything behind", func(t *testing.T) {
		for _, cell := range psApplications(t) {
			if strings.Contains(cell, "落后") {
				t.Errorf("homer ps still marks something behind after every upgrade: %q", cell)
			}
		}
	})
}
