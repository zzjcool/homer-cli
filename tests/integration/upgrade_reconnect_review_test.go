package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestUpgradeReconnectHelloKeepsPID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reexecAgent uses syscall.Exec only on Linux")
	}
	world := newHomerWorld(t)
	const agentID = "upgrade-hello-stable-pid"
	home := filepath.Join(world.root, "agent-home")
	initAgentWorkspace(t, world.global, home)
	agent := startHomerAgent(t, world.root, world.global, buildHomer(t), home, world.url, agentID, world.token)
	waitAgent(t, world, agentID, true, 15*time.Second)
	pid := agent.pid()
	if pid <= 0 {
		t.Fatalf("agent PID = %d, want a running process", pid)
	}

	connectedLine := fmt.Sprintf(`agent stream connected agent=%q`, agentID)
	readLog := func(process *childProcess) string {
		body, err := os.ReadFile(process.logPath)
		if err != nil {
			return "<read log: " + err.Error() + ">"
		}
		return string(body)
	}
	waitFor(t, 5*time.Second, "initial stream-connected log", func() bool {
		return strings.Count(readLog(world.process), connectedLine) >= 1
	})
	beforeConnected := strings.Count(readLog(world.process), connectedLine)
	beforeStarts := strings.Count(readLog(agent), "homer agent: 已启动")
	if beforeConnected != 1 {
		t.Fatalf("initial connected log count for %s = %d, want exactly 1; hub log:\n%s", agentID, beforeConnected, world.process.output())
	}

	upgrade := apiJSON(t, world, http.MethodPost, "/api/agents/"+agentID+"/upgrade", map[string]any{}, 4*time.Minute)
	if upgrade.err != nil || upgrade.status != http.StatusOK {
		t.Fatalf("agent upgrade response = %d err=%v body=%s", upgrade.status, upgrade.err, truncate(string(upgrade.body), 1024))
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(upgrade.body, &result); err != nil || !result.OK {
		t.Fatalf("agent upgrade result = %s err=%v", truncate(string(upgrade.body), 1024), err)
	}

	wantConnected := beforeConnected + 1
	waitFor(t, 15*time.Second, "post-upgrade hello to log a new stream connection", func() bool {
		return strings.Count(readLog(world.process), connectedLine) >= wantConnected
	})
	waitAgent(t, world, agentID, true, 10*time.Second)
	waitFor(t, 5*time.Second, "in-place reexec startup log", func() bool {
		return strings.Count(readLog(agent), "homer agent: 已启动") >= beforeStarts+1
	})

	// Let delayed logs settle so a spurious reconnect cannot be hidden by
	// checking immediately after the first new hello.
	settleUntil := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(settleUntil) {
		if got := strings.Count(readLog(world.process), connectedLine); got > wantConnected {
			t.Fatalf("connected log count after upgrade = %d, want exactly %d; hub log:\n%s", got, wantConnected, readLog(world.process))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := strings.Count(readLog(world.process), connectedLine); got != wantConnected {
		t.Fatalf("connected log count after upgrade = %d, want exactly %d; hub log:\n%s", got, wantConnected, readLog(world.process))
	}
	if got := agent.pid(); got != pid {
		t.Fatalf("agent PID changed across upgrade reexec: before=%d after=%d", pid, got)
	}
	select {
	case <-agent.finished:
		t.Fatalf("agent process exited during upgrade; PID %d was not preserved", pid)
	default:
	}
}
