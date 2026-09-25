package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/agecrypto"
	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/pair/pairtest"
)

func TestV13PairCLIBinaryWithEmptyPATHShowsInstallAndGitFallback(t *testing.T) {
	binary := buildHomer(t)
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer v1.3 Pair P3\n\temail = homer-v13-p3@example.invalid\n")
	result := runProcess(t, repoRoot(), global, []string{
		"PATH=" + filepath.Join(root, "empty-path"),
		"HOME=" + filepath.Join(root, "home"),
		"HOMER_HOME=" + filepath.Join(root, "homer"),
	}, binary, "pair")
	if result.code != 1 {
		t.Fatalf("homer pair exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", result.code, result.stdout, result.stderr)
	}
	for _, expected := range []string{
		"https://github.com/tailscale/tailcat",
		"homer secret push",
		"homer secret pull",
	} {
		if !strings.Contains(result.stderr, expected) {
			t.Fatalf("homer pair stderr missing %q:\n%s", expected, result.stderr)
		}
	}
}

func TestV13PairHelpAndJSONSmokeAtProcessBoundary(t *testing.T) {
	binary := buildHomer(t)
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer v1.3 Pair P3 Help\n\temail = homer-v13-p3-help@example.invalid\n")
	env := []string{
		"PATH=" + filepath.Join(root, "empty-path"),
		"HOME=" + filepath.Join(root, "home"),
		"HOMER_HOME=" + filepath.Join(root, "homer"),
	}

	help := runProcess(t, repoRoot(), global, env, binary, "pair", "--help")
	if help.code != 0 || help.stderr != "" {
		t.Fatalf("homer pair --help = %#v", help)
	}
	for _, expected := range []string{
		"用法: homer pair [options]",
		"homer pair <tc-addr>",
		"勿粘贴到 git",
	} {
		if !strings.Contains(help.stdout, expected) {
			t.Fatalf("homer pair --help missing %q:\n%s", expected, help.stdout)
		}
	}

	jsonResult := runProcess(t, repoRoot(), global, env, binary, "pair", "--json")
	if jsonResult.code != 1 {
		t.Fatalf("homer pair --json exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", jsonResult.code, jsonResult.stdout, jsonResult.stderr)
	}
	report := map[string]any{}
	if err := json.Unmarshal([]byte(jsonResult.stdout), &report); err != nil {
		t.Fatalf("homer pair --json output is not JSON: %v\n%s", err, jsonResult.stdout)
	}
	if report["ok"] != false || report["status"] != "no-tailcat" {
		t.Fatalf("homer pair --json report = %#v", report)
	}
	if !strings.Contains(jsonResult.stderr, "homer secret push") || !strings.Contains(jsonResult.stderr, "homer secret pull") {
		t.Fatalf("homer pair --json stderr missing git fallback: %s", jsonResult.stderr)
	}
}

func TestV13PairCLIOptionMatrixAtProcessBoundary(t *testing.T) {
	binary := buildHomer(t)
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer v1.3 Pair P3 Options\n\temail = homer-v13-p3-options@example.invalid\n")
	env := []string{
		"PATH=" + filepath.Join(root, "empty-path"),
		"HOME=" + filepath.Join(root, "home"),
		"HOMER_HOME=" + filepath.Join(root, "homer"),
	}
	cases := []struct {
		name       string
		args       []string
		code       int
		stdoutWant string
		stderrWant string
	}{
		{name: "short-help", args: []string{"pair", "-h"}, code: 0, stdoutWant: "用法: homer pair [options]"},
		{name: "help-before-preflight", args: []string{"pair", "--help", "--json"}, code: 0, stdoutWant: "homer pair <tc-addr>"},
		{name: "json", args: []string{"pair", "--json"}, code: 1, stdoutWant: "\"status\": \"no-tailcat\"", stderrWant: "homer secret push"},
		{name: "json-with-yes", args: []string{"pair", "--yes", "--json"}, code: 1, stdoutWant: "\"status\": \"no-tailcat\"", stderrWant: "homer secret pull"},
		{name: "unsupported-offline", args: []string{"pair", "--offline"}, code: 1, stderrWant: "不支持选项 --offline"},
		{name: "unsupported-no-push", args: []string{"pair", "--no-push"}, code: 1, stderrWant: "不支持选项 --no-push"},
		{name: "too-many-positionals", args: []string{"pair", "one", "two"}, code: 1, stderrWant: "多余的参数: two"},
		{name: "missing-home-value", args: []string{"pair", "--home"}, code: 1, stderrWant: "选项 --home 缺少值"},
		{name: "unknown-option", args: []string{"pair", "--unknown"}, code: 1, stderrWant: "未知选项: --unknown"},
		{name: "json-value-rejected", args: []string{"pair", "--json=1"}, code: 1, stderrWant: "选项 --json 不接受值"},
		{name: "yes-value-rejected", args: []string{"pair", "--yes=1"}, code: 1, stderrWant: "选项 --yes 不接受值"},
		{name: "explicit-join-address", args: []string{"pair", "--", "tc-test"}, code: 1, stdoutWant: "homer pair: no-tailcat", stderrWant: "https://github.com/tailscale/tailcat"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := runProcess(t, repoRoot(), global, env, binary, testCase.args...)
			if result.code != testCase.code {
				t.Fatalf("homer %s exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", strings.Join(testCase.args, " "), result.code, testCase.code, result.stdout, result.stderr)
			}
			if testCase.stdoutWant != "" && !strings.Contains(result.stdout, testCase.stdoutWant) {
				t.Fatalf("stdout missing %q:\n%s", testCase.stdoutWant, result.stdout)
			}
			if testCase.stderrWant != "" && !strings.Contains(result.stderr, testCase.stderrWant) {
				t.Fatalf("stderr missing %q:\n%s", testCase.stderrWant, result.stderr)
			}
		})
	}
}

func TestV13PairSIGINTCleansTailcatProcessGroupAndAddressFile(t *testing.T) {
	binary := buildHomer(t)
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer v1.3 Pair P3 Cleanup\n\temail = homer-v13-p3-cleanup@example.invalid\n")
	homerHome := filepath.Join(root, "homer")
	writePairProcessConfig(t, homerHome, "cleanup-secret", "pair cleanup source\n")

	fakeDir := filepath.Join(root, "fake-bin")
	if err := os.MkdirAll(fakeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeTailcat := filepath.Join(fakeDir, "tailcat")
	writeFile(t, fakeTailcat, `#!/bin/sh
set -eu
printf '%s\n' 'fake-p3-address' > "$TAILCAT_ADDR_FILE"
printf '%s\n' "$TAILCAT_ADDR_FILE" > "$HOME/fake-tailcat.addrpath"
printf '%s\n' "$$" > "$HOME/fake-tailcat.pid"
trap 'exit 0' INT TERM HUP
/bin/cat
`)
	if err := os.Chmod(fakeTailcat, 0o700); err != nil {
		t.Fatal(err)
	}

	fakeHome := filepath.Join(root, "tailcat-home")
	process := startV13PairProcess(t, binary, global, homerHome, fakeHome, fakeDir, "pair", "--home", homerHome)
	var addressLine string
	addressDeadline := time.After(5 * time.Second)
	for addressLine == "" {
		select {
		case line, ok := <-process.stdoutLines:
			if !ok {
				t.Fatalf("pair exited before printing an address: %s", process.waitOutput(2*time.Second))
			}
			if strings.Contains(line, "fake-p3-address") {
				addressLine = line
			}
		case <-addressDeadline:
			process.kill()
			t.Fatalf("pair did not print an address")
		}
	}
	if !strings.Contains(addressLine, "fake-p3-address") {
		t.Fatalf("pair address line = %q", addressLine)
	}
	if !strings.Contains(addressLine, "勿入 git") {
		t.Fatalf("pair address line lacks out-of-band warning = %q", addressLine)
	}

	pidFile := filepath.Join(root, "tailcat-home", "fake-tailcat.pid")
	var pid int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && pid > 0 {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if pid == 0 {
		process.kill()
		t.Fatalf("fake tailcat did not publish its pid")
	}
	addressPathData := waitForV13File(t, filepath.Join(fakeHome, "fake-tailcat.addrpath"), time.Second)
	addressPath := strings.TrimSpace(string(addressPathData))
	if addressPath == "" {
		process.kill()
		t.Fatal("fake tailcat did not publish its address file path")
	}
	if err := process.cmd.Process.Signal(os.Interrupt); err != nil {
		process.kill()
		t.Fatalf("send SIGINT to homer pair: %v", err)
	}

	code, stdout, stderr := process.wait(5 * time.Second)
	if code != 1 {
		t.Fatalf("pair after tailcat termination exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "fake-p3-address") || !strings.Contains(stdout, "勿入 git") {
		t.Fatalf("pair output lost address warning after cleanup:\n%s", stdout)
	}
	if _, err := os.Stat(addressPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tailcat address file remains after SIGINT: %s (err=%v)", addressPath, err)
	}
	assertV13NoTailcatProcess(t, fakeTailcat)
}

func TestV13DoctorJSONShowsPairedDeviceCount(t *testing.T) {
	binary := buildHomer(t)
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	writeFile(t, global, "[user]\n\tname = Homer v1.3 Pair P3 Doctor\n\temail = homer-v13-p3-doctor@example.invalid\n")
	homerHome := filepath.Join(root, "homer")
	paths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return homerHome
		}
		return os.Getenv(name)
	})
	if err := core.SaveConfig(paths, core.HomerConfig{
		Version:  1,
		Adapters: map[string]core.AdapterConfig{},
		Secrets:  &core.SecretsConfig{Files: map[string]string{}},
	}); err != nil {
		t.Fatal(err)
	}
	runGit(t, global, homerHome, "init", "-b", "main")
	paired := []core.PairedDevice{
		{Hostname: "pair-one", Recipient: "age1" + strings.Repeat("a", 32), PairedAt: "2026-09-25T00:00:00Z"},
		{Hostname: "pair-two", Recipient: "age1" + strings.Repeat("b", 32), PairedAt: "2026-09-25T00:00:01Z"},
		{Hostname: "pair-three", Recipient: "age1" + strings.Repeat("c", 32), PairedAt: "2026-09-25T00:00:02Z"},
	}
	if err := core.SaveState(paths, core.HomerState{Version: 1, Paired: paired}); err != nil {
		t.Fatal(err)
	}

	result := runProcess(t, repoRoot(), global, []string{
		"HOME=" + filepath.Join(root, "home"),
		"HOMER_HOME=" + homerHome,
	}, binary, "doctor", "--offline", "--json")
	if result.code != 0 {
		t.Fatalf("homer doctor --json exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", result.code, result.stdout, result.stderr)
	}
	report := map[string]any{}
	if err := json.Unmarshal([]byte(result.stdout), &report); err != nil {
		t.Fatalf("doctor JSON decode: %v\n%s", err, result.stdout)
	}
	checks, ok := report["checks"].([]any)
	if !ok {
		t.Fatalf("doctor checks = %#v", report["checks"])
	}
	var ageCheck map[string]any
	for _, item := range checks {
		check, ok := item.(map[string]any)
		if ok && check["id"] == "age" {
			ageCheck = check
			break
		}
	}
	if ageCheck == nil {
		t.Fatalf("doctor JSON has no age check: %#v", checks)
	}
	message, _ := ageCheck["message"].(string)
	if !strings.Contains(message, "已配对 3 台设备") {
		t.Fatalf("doctor age message = %q", message)
	}
	details, _ := ageCheck["details"].([]any)
	if len(details) != 3 {
		t.Fatalf("doctor age details = %#v, want three devices", ageCheck["details"])
	}
}

func TestV13PairPipeTransportFullLinkFallback(t *testing.T) {
	root := t.TempDir()
	aHome := filepath.Join(root, "machine-a")
	bHome := filepath.Join(root, "machine-b")
	aPaths := writePairProcessConfig(t, aHome, "shared-secret", "P3 process-boundary secret\nsecond line\n")
	bPaths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return bHome
		}
		return os.Getenv(name)
	})
	bIdentity := agecrypto.GenerateIdentity()
	if err := core.SaveConfig(bPaths, core.HomerConfig{
		Version:  1,
		Adapters: map[string]core.AdapterConfig{},
		Secrets: &core.SecretsConfig{
			Files: map[string]string{"shared-secret": filepath.Join(bHome, "dest", "shared-secret")},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := agecrypto.WriteIdentityFile(bPaths, bIdentity); err != nil {
		t.Fatal(err)
	}

	left, right := pairtest.NewPipePair()
	addrCh := make(chan string, 1)
	serveCh := make(chan commands.PairServeReport, 1)
	go func() {
		serveCh <- commands.RunPairServe(commands.PairOptions{HomerHome: aHome, Yes: true}, &commands.PairDeps{
			Transport: left,
			OnAddr:    func(addr string) { addrCh <- addr },
		})
	}()
	select {
	case <-addrCh:
	case <-time.After(2 * time.Second):
		t.Fatal("pipe serve did not publish an address")
	}
	join := commands.RunPairJoin(commands.PairOptions{HomerHome: bHome, Addr: "pipe-test"}, &commands.PairDeps{Transport: right})
	var serve commands.PairServeReport
	select {
	case serve = <-serveCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("pipe serve did not finish; join report = %#v", join)
	}
	if !serve.OK || serve.Status != commands.PairServeStatusPaired {
		t.Fatalf("pipe serve report = %#v", serve)
	}
	if !join.OK || join.Status != commands.PairJoinStatusPaired {
		t.Fatalf("pipe join report = %#v", join)
	}

	source, err := os.ReadFile(filepath.Join(aHome, "dest", "shared-secret"))
	if err != nil {
		t.Fatal(err)
	}
	destination, err := os.ReadFile(filepath.Join(bHome, "dest", "shared-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(destination, source) {
		t.Fatalf("B destination = %q, want %q", destination, source)
	}
	if info, err := os.Stat(filepath.Join(bHome, "dest", "shared-secret")); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("B destination mode = %o, want 600", info.Mode().Perm())
	}
	aConfig, err := core.LoadConfig(aPaths)
	if err != nil {
		t.Fatal(err)
	}
	if !containsV13String(aConfig.Secrets.Recipients, bIdentity.Recipient) {
		t.Fatalf("A recipients = %v, missing B recipient", aConfig.Secrets.Recipients)
	}
	aState := core.LoadState(aPaths)
	if len(aState.Paired) != 1 || aState.Paired[0].Recipient != bIdentity.Recipient {
		t.Fatalf("A paired state = %#v", aState.Paired)
	}
	if _, err := agecrypto.DecryptSecretFromFile(agecrypto.NewAgeCryptoPort(), bPaths, "shared-secret"); err != nil {
		t.Fatalf("B vault cannot decrypt received ciphertext: %v", err)
	}
}

type v13PairProcess struct {
	cmd         *exec.Cmd
	stdoutLines <-chan string
	stdoutDone  <-chan string
	stderrDone  <-chan string
}

func startV13PairProcess(t *testing.T, binary, global, homerHome, fakeHome, path string, args ...string) *v13PairProcess {
	t.Helper()
	if err := os.MkdirAll(fakeHome, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, args...)
	command.Dir = repoRoot()
	command.Env = baseEnv(global)
	command.Env = setEnv(command.Env, "PATH", path)
	command.Env = setEnv(command.Env, "HOME", fakeHome)
	command.Env = setEnv(command.Env, "HOMER_HOME", homerHome)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatalf("start %s: %v", strings.Join(args, " "), err)
	}
	lines := make(chan string, 32)
	stdoutDone := make(chan string, 1)
	go func() {
		var output strings.Builder
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			output.WriteString(line)
			output.WriteByte('\n')
			lines <- line
		}
		close(lines)
		stdoutDone <- output.String()
	}()
	stderrDone := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(stderr)
		stderrDone <- string(data)
	}()
	return &v13PairProcess{cmd: command, stdoutLines: lines, stdoutDone: stdoutDone, stderrDone: stderrDone}
}

func (process *v13PairProcess) wait(timeout time.Duration) (int, string, string) {
	waitResult := make(chan error, 1)
	go func() { waitResult <- process.cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waitResult:
	case <-time.After(timeout):
		process.kill()
		waitErr = <-waitResult
	}
	stdout := <-process.stdoutDone
	stderr := <-process.stderrDone
	return v13ExitCode(waitErr), stdout, stderr
}

func (process *v13PairProcess) waitOutput(timeout time.Duration) string {
	if process.cmd.ProcessState == nil {
		process.kill()
	}
	_, stdout, stderr := process.wait(timeout)
	return stdout + stderr
}

func (process *v13PairProcess) kill() {
	if process.cmd.Process != nil && process.cmd.ProcessState == nil {
		_ = process.cmd.Process.Kill()
	}
}

func v13ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func writePairProcessConfig(t *testing.T, home, name, plaintext string) core.HomerPaths {
	t.Helper()
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return home
		}
		return os.Getenv(key)
	})
	destination := filepath.Join(home, "dest", name)
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte(plaintext), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := agecrypto.GenerateIdentity()
	if err := core.SaveConfig(paths, core.HomerConfig{
		Version:  1,
		Adapters: map[string]core.AdapterConfig{},
		Secrets: &core.SecretsConfig{
			Recipients: []string{identity.Recipient},
			Files:      map[string]string{name: destination},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return paths
}

func waitForV13File(t *testing.T, filename string, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filename)
		if err == nil && len(data) > 0 {
			return data
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", filename)
	return nil
}

func containsV13String(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertV13NoTailcatProcess(t *testing.T, needle string) {
	t.Helper()
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Fatalf("pgrep is required for tailcat cleanup assertion: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		output, err := exec.Command("pgrep", "-af", "tailcat").Output()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				return
			}
			t.Fatalf("pgrep tailcat: %v", err)
		}
		matches := make([]string, 0)
		for _, line := range strings.Split(string(output), "\n") {
			if strings.Contains(line, needle) {
				matches = append(matches, line)
			}
		}
		if len(matches) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	output, _ := exec.Command("pgrep", "-af", "tailcat").CombinedOutput()
	t.Fatalf("tailcat process remains after cleanup (%s):\n%s", needle, output)
}
