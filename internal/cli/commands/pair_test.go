package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/agecrypto"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/pair"
	"github.com/zzjcool/homer-cli/internal/pair/pairtest"
)

type pairConfirmFake struct {
	allow bool

	mu      sync.Mutex
	calls   int
	message string
}

func (fake *pairConfirmFake) Confirm(message string, _ bool) bool {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	fake.message = message
	return fake.allow
}

type pairFixture struct {
	aHome string
	bHome string
	aPath core.HomerPaths
	bPath core.HomerPaths

	aIdentity agecrypto.AgeIdentity
	bIdentity agecrypto.AgeIdentity

	aFiles map[string]string
	bFiles map[string]string
}

func newPairFixture(t *testing.T, aNames, bNames []string) pairFixture {
	t.Helper()
	aHome := t.TempDir()
	bHome := t.TempDir()
	aPath := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return aHome
		}
		return os.Getenv(name)
	})
	bPath := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return bHome
		}
		return os.Getenv(name)
	})
	aIdentity := agecrypto.GenerateIdentity()
	bIdentity := agecrypto.GenerateIdentity()
	if err := agecrypto.WriteIdentityFile(bPath, bIdentity); err != nil {
		t.Fatal(err)
	}

	aFiles := make(map[string]string, len(aNames))
	for _, name := range aNames {
		aFiles[name] = filepath.Join(aHome, "dest", name)
	}
	bFiles := make(map[string]string, len(bNames))
	for _, name := range bNames {
		bFiles[name] = filepath.Join(bHome, "dest", name)
	}
	if err := core.SaveConfig(aPath, core.HomerConfig{
		Version:  1,
		Adapters: map[string]core.AdapterConfig{},
		Secrets:  &core.SecretsConfig{Recipients: []string{aIdentity.Recipient}, Files: aFiles},
	}); err != nil {
		t.Fatal(err)
	}
	if err := core.SaveConfig(bPath, core.HomerConfig{
		Version:  1,
		Adapters: map[string]core.AdapterConfig{},
		Secrets:  &core.SecretsConfig{Files: bFiles},
	}); err != nil {
		t.Fatal(err)
	}
	for name, destination := range aFiles {
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, []byte("source-"+name+"\nwith-enough-content-to-exercise-the-ciphertext-check"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return pairFixture{
		aHome: aHome, bHome: bHome, aPath: aPath, bPath: bPath,
		aIdentity: aIdentity, bIdentity: bIdentity,
		aFiles: aFiles, bFiles: bFiles,
	}
}

func runLinkedPairWithUI(t *testing.T, fixture pairFixture, ui PromptPort, age agecrypto.AgeCryptoPort) (PairServeReport, PairJoinReport) {
	left, right := pairtest.NewPipePair()
	addrCh := make(chan string, 1)
	serveCh := make(chan PairServeReport, 1)
	go func() {
		serveCh <- RunPairServe(PairOptions{HomerHome: fixture.aHome}, &PairDeps{
			UI:        ui,
			Transport: left,
			OnAddr:    func(addr string) { addrCh <- addr },
		})
	}()
	select {
	case <-addrCh:
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not expose address")
	}
	join := RunPairJoin(PairOptions{HomerHome: fixture.bHome, Addr: "pipe-test"}, &PairDeps{Transport: right, Age: age})
	select {
	case serve := <-serveCh:
		return serve, join
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not finish")
		return PairServeReport{}, join
	}
}

func TestPairFullLinkSuccessCopiesPlaintextAndRecordsPairing(t *testing.T) {
	fixture := newPairFixture(t, []string{"shared"}, []string{"shared"})
	ui := &pairConfirmFake{allow: true}
	serve, join := runLinkedPairWithUI(t, fixture, ui, nil)
	if !serve.OK || serve.Status != PairServeStatusPaired {
		t.Fatalf("serve report = %#v", serve)
	}
	if !join.OK || join.Status != PairJoinStatusPaired {
		t.Fatalf("join report = %#v", join)
	}
	want, err := os.ReadFile(fixture.aFiles["shared"])
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(fixture.bFiles["shared"])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("destination = %q, want %q", got, want)
	}
	if mode := fileMode(t, fixture.bFiles["shared"]); mode != 0o600 {
		t.Fatalf("destination mode = %o, want 600", mode)
	}
	decrypted, err := agecrypto.DecryptSecretFromFile(agecrypto.NewAgeCryptoPort(), fixture.bPath, "shared")
	if err != nil || !bytes.Equal(decrypted, want) {
		t.Fatalf("vault decrypt = %q, %v", decrypted, err)
	}
	config, err := core.LoadConfig(fixture.aPath)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(config.Secrets.Recipients, fixture.bIdentity.Recipient) {
		t.Fatalf("A recipients = %v, want B recipient", config.Secrets.Recipients)
	}
	state := core.LoadState(fixture.aPath)
	if len(state.Paired) != 1 || state.Paired[0].Recipient != fixture.bIdentity.Recipient {
		t.Fatalf("A state paired = %#v", state.Paired)
	}
	if ui.calls != 1 || !strings.Contains(ui.message, fixture.bIdentity.Recipient[:12]) || strings.Contains(ui.message, fixture.bIdentity.Recipient) {
		t.Fatalf("confirmation message/calls = %q / %d", ui.message, ui.calls)
	}
}

func TestPairUIRejectsWithDeclineAndZeroWrites(t *testing.T) {
	fixture := newPairFixture(t, []string{"shared"}, []string{"shared"})
	ui := &pairConfirmFake{allow: false}
	serve, join := runLinkedPairWithUI(t, fixture, ui, nil)
	if serve.Status != PairServeStatusAborted || serve.OK {
		t.Fatalf("serve rejection report = %#v", serve)
	}
	if join.Status != PairJoinStatusDeclined || join.OK {
		t.Fatalf("join decline report = %#v", join)
	}
	config, err := core.LoadConfig(fixture.aPath)
	if err != nil {
		t.Fatal(err)
	}
	if containsString(config.Secrets.Recipients, fixture.bIdentity.Recipient) {
		t.Fatal("B recipient was persisted after UI rejection")
	}
	if _, err := os.Stat(fixture.bFiles["shared"]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("B destination after rejection: %v", err)
	}
	if _, err := agecrypto.SecretFilePath(fixture.bPath, "shared"); err != nil {
		t.Fatal(err)
	} else if _, statErr := os.Stat(filepath.Join(fixture.bPath.SecretsDir, "shared.age")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("B vault after rejection: %v", statErr)
	}
	if state := core.LoadState(fixture.aPath); len(state.Paired) != 0 {
		t.Fatalf("state after rejection = %#v", state.Paired)
	}
}

func TestPairMissingSourceAbortsBeforeSendingBlob(t *testing.T) {
	fixture := newPairFixture(t, []string{"shared"}, []string{"shared"})
	if err := os.Remove(fixture.aFiles["shared"]); err != nil {
		t.Fatal(err)
	}
	serve, join := runLinkedPairWithUI(t, fixture, &pairConfirmFake{allow: true}, nil)
	if serve.Status != PairServeStatusNoSource || serve.OK || len(serve.Sent) != 0 {
		t.Fatalf("missing source serve report = %#v", serve)
	}
	if join.OK || join.Status != PairJoinStatusError {
		t.Fatalf("missing source join report = %#v", join)
	}
	if _, err := os.Stat(fixture.bFiles["shared"]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("B destination after missing source: %v", err)
	}
	config, err := core.LoadConfig(fixture.aPath)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(config.Secrets.Recipients, fixture.bIdentity.Recipient) {
		t.Fatal("accepted recipient was not persisted before source read")
	}
}

func TestPairNoIdentityReturnsBeforeConnectingAndServeCanTimeout(t *testing.T) {
	fixture := newPairFixture(t, []string{"shared"}, []string{"shared"})
	if err := os.Remove(agecrypto.IdentityFilePath(fixture.bPath)); err != nil {
		t.Fatal(err)
	}
	transport := immediateAcceptFailureTransport{}
	addrCh := make(chan string, 1)
	serveCh := make(chan PairServeReport, 1)
	go func() {
		serveCh <- RunPairServe(PairOptions{HomerHome: fixture.aHome}, &PairDeps{
			Transport: transport,
			OnAddr:    func(addr string) { addrCh <- addr },
		})
	}()
	select {
	case <-addrCh:
	case <-time.After(time.Second):
		t.Fatal("serve did not expose address")
	}
	join := RunPairJoin(PairOptions{HomerHome: fixture.bHome, Addr: "pipe-test"}, &PairDeps{Transport: transport})
	if join.Status != PairJoinStatusNoIdentity || join.OK {
		t.Fatalf("no-identity report = %#v", join)
	}
	select {
	case serve := <-serveCh:
		if serve.Status != PairServeStatusError {
			t.Fatalf("short accept report = %#v", serve)
		}
	case <-time.After(time.Second):
		t.Fatal("serve did not return from short accept failure")
	}
}

func TestPairUnknownOfferNameAbortsAndServeReportsPeerFailure(t *testing.T) {
	fixture := newPairFixture(t, []string{"shared"}, []string{"different"})
	serve, join := runLinkedPairWithUI(t, fixture, &pairConfirmFake{allow: true}, nil)
	if join.Status != PairJoinStatusUnknownSecret || join.OK {
		t.Fatalf("unknown offer join report = %#v", join)
	}
	if serve.Status != PairServeStatusPeerFailed || serve.OK {
		t.Fatalf("unknown offer serve report = %#v", serve)
	}
}

type failingDecryptPort struct{}

func (failingDecryptPort) Encrypt(plaintext []byte, _ []string) ([]byte, error) {
	return agecrypto.EncryptToRecipients(plaintext, []string{agecrypto.GenerateIdentity().Recipient})
}

func (failingDecryptPort) Decrypt([]byte, agecrypto.AgeIdentity) ([]byte, error) {
	return nil, errors.New("fake decrypt failure")
}

func TestPairDecryptFailureLeavesDestinationUntouched(t *testing.T) {
	fixture := newPairFixture(t, []string{"shared"}, []string{"shared"})
	serve, join := runLinkedPairWithUI(t, fixture, &pairConfirmFake{allow: true}, failingDecryptPort{})
	if join.Status != PairJoinStatusUndecryptable || join.OK {
		t.Fatalf("decrypt failure join report = %#v", join)
	}
	if serve.Status != PairServeStatusPeerFailed || serve.OK {
		t.Fatalf("decrypt failure serve report = %#v", serve)
	}
	if _, err := os.Stat(fixture.bFiles["shared"]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination after decrypt failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.bPath.SecretsDir, "shared.age")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("vault after decrypt failure: %v", err)
	}
}

type failingAckTransport struct{ pair.PairTransport }

func (t failingAckTransport) Connect(ctx context.Context, addr string) (io.ReadWriteCloser, error) {
	conn, err := t.PairTransport.Connect(ctx, addr)
	if err != nil {
		return nil, err
	}
	return &failSecondWriteConn{ReadWriteCloser: conn}, nil
}

type failSecondWriteConn struct {
	io.ReadWriteCloser
	mu     sync.Mutex
	writes int
}

func (c *failSecondWriteConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	writes := c.writes
	c.mu.Unlock()
	if writes == 2 {
		return 0, errors.New("fake ack write failure")
	}
	return c.ReadWriteCloser.(io.Writer).Write(p)
}

func TestPairAckFailureReportsPeerFailureAndRecipientWarning(t *testing.T) {
	fixture := newPairFixture(t, []string{"shared"}, []string{"shared"})
	left, right := pairtest.NewPipePair()
	addrCh := make(chan string, 1)
	serveCh := make(chan PairServeReport, 1)
	go func() {
		serveCh <- RunPairServe(PairOptions{HomerHome: fixture.aHome, Yes: true}, &PairDeps{
			Transport: left,
			OnAddr:    func(addr string) { addrCh <- addr },
		})
	}()
	select {
	case <-addrCh:
	case <-time.After(time.Second):
		t.Fatal("serve did not expose address")
	}
	join := RunPairJoin(PairOptions{HomerHome: fixture.bHome, Addr: "pipe-test"}, &PairDeps{Transport: failingAckTransport{PairTransport: right}})
	var serve PairServeReport
	select {
	case serve = <-serveCh:
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not finish after ack failure")
	}
	if join.OK || join.Status == PairJoinStatusPaired {
		t.Fatalf("ack failure join report = %#v", join)
	}
	if serve.Status != PairServeStatusPeerFailed || serve.OK {
		t.Fatalf("ack failure serve report = %#v", serve)
	}
	if !containsSubstring(serve.Warnings, "recipient") {
		t.Fatalf("ack failure warnings = %#v", serve.Warnings)
	}
}

func TestExecutePairNoTailcatPrintsInstallAndGitFallback(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var out, errOut bytes.Buffer
	code := ExecutePair(PairOptions{HomerHome: t.TempDir()}, nil, &out, &errOut)
	if code != 1 {
		t.Fatalf("missing tailcat exit code = %d, want 1", code)
	}
	if !strings.Contains(out.String(), string(PairServeStatusNoTailcat)) {
		t.Fatalf("stdout = %q, want no-tailcat report", out.String())
	}
	for _, expected := range []string{"https://github.com/tailscale/tailcat", "homer secret push", "homer secret pull"} {
		if !strings.Contains(errOut.String(), expected) {
			t.Fatalf("stderr = %q, want %q", errOut.String(), expected)
		}
	}
}

func TestExecutePairNoConfigAfterTailcatPreflight(t *testing.T) {
	writeFakeTailcat(t)
	var serveOut, serveErr bytes.Buffer
	if code := ExecutePair(PairOptions{HomerHome: t.TempDir()}, nil, &serveOut, &serveErr); code != 1 {
		t.Fatalf("serve no-config exit code = %d, want 1", code)
	}
	if !strings.Contains(serveOut.String(), string(PairServeStatusNoConfig)) || !strings.Contains(serveErr.String(), "未找到 homer 配置") {
		t.Fatalf("serve no-config output = %q / %q", serveOut.String(), serveErr.String())
	}

	var joinOut, joinErr bytes.Buffer
	if code := ExecutePair(PairOptions{HomerHome: t.TempDir(), Addr: "tc-test"}, nil, &joinOut, &joinErr); code != 1 {
		t.Fatalf("join no-config exit code = %d, want 1", code)
	}
	if !strings.Contains(joinOut.String(), string(PairJoinStatusNoConfig)) || !strings.Contains(joinErr.String(), "未找到 homer 配置") {
		t.Fatalf("join no-config output = %q / %q", joinOut.String(), joinErr.String())
	}

	emptyHome := t.TempDir()
	emptyPaths := core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return emptyHome
		}
		return os.Getenv(name)
	})
	if err := core.SaveConfig(emptyPaths, core.HomerConfig{Version: 1, Adapters: map[string]core.AdapterConfig{}}); err != nil {
		t.Fatal(err)
	}
	var emptyOut, emptyErr bytes.Buffer
	if code := ExecutePair(PairOptions{HomerHome: emptyHome}, nil, &emptyOut, &emptyErr); code != 1 {
		t.Fatalf("empty secrets.files exit code = %d, want 1", code)
	}
	if !strings.Contains(emptyOut.String(), string(PairServeStatusNoConfig)) || !strings.Contains(emptyErr.String(), "secrets.files") {
		t.Fatalf("empty secrets.files output = %q / %q", emptyOut.String(), emptyErr.String())
	}
}

func TestPairReportsUsageAndExitCodes(t *testing.T) {
	if (PairServeReport{OK: true}).ExitCode() != 0 || (PairServeReport{}).ExitCode() != 1 {
		t.Fatal("serve report exit code mapping changed")
	}
	if (PairJoinReport{OK: true}).ExitCode() != 0 || (PairJoinReport{}).ExitCode() != 1 {
		t.Fatal("join report exit code mapping changed")
	}
	if !strings.Contains(PAIR_USAGE, "homer pair <tc-addr>") || !strings.Contains(PAIR_USAGE, "https://github.com/tailscale/tailcat") {
		t.Fatalf("PAIR_USAGE missing frozen guidance: %q", PAIR_USAGE)
	}
	serve := RenderPairServeReport(PairServeReport{Status: PairServeStatusNoConfig, Errors: []string{"配置缺失"}})
	if !strings.Contains(serve, "homer pair: no-config") || !strings.Contains(serve, "配置缺失") {
		t.Fatalf("serve render = %q", serve)
	}
	join := RenderPairJoinReport(PairJoinReport{Status: PairJoinStatusNoConfig, Errors: []string{"配置缺失"}})
	if !strings.Contains(join, "homer pair: no-config") || !strings.Contains(join, "配置缺失") {
		t.Fatalf("join render = %q", join)
	}
}

func writeFakeTailcat(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "tailcat")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return binary
}

type immediateAcceptFailureTransport struct{}

func (immediateAcceptFailureTransport) Serve(context.Context) (pair.PairServer, error) {
	return immediateAcceptFailureServer{}, nil
}

func (immediateAcceptFailureTransport) Connect(context.Context, string) (io.ReadWriteCloser, error) {
	return nil, errors.New("unexpected connect")
}

type immediateAcceptFailureServer struct{}

func (immediateAcceptFailureServer) Addr() string { return "short-test" }
func (immediateAcceptFailureServer) Accept(context.Context) (io.ReadWriteCloser, error) {
	return nil, context.DeadlineExceeded
}
func (immediateAcceptFailureServer) Close() error { return nil }

func fileMode(t *testing.T, name string) os.FileMode {
	t.Helper()
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}
