package commands

import (
	"bytes"
	"context"
	"encoding/json"
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

type delayedHelloTransport struct {
	pair.PairTransport
	delay time.Duration
}

func (transport delayedHelloTransport) Connect(ctx context.Context, addr string) (io.ReadWriteCloser, error) {
	conn, err := transport.PairTransport.Connect(ctx, addr)
	if err != nil {
		return nil, err
	}
	return &delayedFirstWriteConn{ReadWriteCloser: conn, delay: transport.delay}, nil
}

type delayedFirstWriteConn struct {
	io.ReadWriteCloser
	delay time.Duration
	once  sync.Once
}

func (conn *delayedFirstWriteConn) Write(payload []byte) (int, error) {
	conn.once.Do(func() { time.Sleep(conn.delay) })
	return conn.ReadWriteCloser.(io.Writer).Write(payload)
}

func TestPairServeAcceptsSlowHelloWithinAcceptWindow(t *testing.T) {
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
	join := RunPairJoin(PairOptions{HomerHome: fixture.bHome, Addr: "pipe-test"}, &PairDeps{
		Transport: delayedHelloTransport{PairTransport: right, delay: 250 * time.Millisecond},
	})
	select {
	case serve := <-serveCh:
		if !serve.OK || serve.Status != PairServeStatusPaired {
			t.Fatalf("slow hello serve report = %#v", serve)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("serve did not finish after slow hello; join report = %#v", join)
	}
	if !join.OK || join.Status != PairJoinStatusPaired {
		t.Fatalf("slow hello join report = %#v", join)
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
	if !containsSubstring(serve.Warnings, "B 端可能已完成写入") {
		t.Fatalf("ack failure peer completion warning = %#v", serve.Warnings)
	}
	if !containsSubstring(join.Warnings, "目标文件与 vault 已写入且验证通过，仅确认未送达 A") {
		t.Fatalf("ack failure join warning = %#v", join.Warnings)
	}
	if got, err := os.ReadFile(fixture.bFiles["shared"]); err != nil || !bytes.Equal(got, []byte("source-shared\nwith-enough-content-to-exercise-the-ciphertext-check")) {
		t.Fatalf("B destination after ack failure = %q, %v", got, err)
	}
	if _, err := agecrypto.DecryptSecretFromFile(agecrypto.NewAgeCryptoPort(), fixture.bPath, "shared"); err != nil {
		t.Fatalf("B vault after ack failure = %v", err)
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

type deadlineRecordingTransport struct {
	serveHasDeadline  bool
	acceptHasDeadline bool
}

func (transport *deadlineRecordingTransport) Serve(ctx context.Context) (pair.PairServer, error) {
	_, transport.serveHasDeadline = ctx.Deadline()
	return &deadlineRecordingServer{transport: transport}, nil
}

func (*deadlineRecordingTransport) Connect(context.Context, string) (io.ReadWriteCloser, error) {
	return nil, errors.New("unexpected connect")
}

type deadlineRecordingServer struct{ transport *deadlineRecordingTransport }

func (server *deadlineRecordingServer) Addr() string { return "deadline-test" }
func (server *deadlineRecordingServer) Accept(ctx context.Context) (io.ReadWriteCloser, error) {
	_, server.transport.acceptHasDeadline = ctx.Deadline()
	return nil, errors.New("stop after deadline inspection")
}
func (*deadlineRecordingServer) Close() error { return nil }

func TestPairServeUsesUnboundedServeContextAndBoundedAcceptContext(t *testing.T) {
	fixture := newPairFixture(t, []string{"shared"}, []string{"shared"})
	transport := &deadlineRecordingTransport{}
	report := RunPairServe(PairOptions{HomerHome: fixture.aHome}, &PairDeps{Transport: transport})
	if report.OK || report.Status != PairServeStatusError {
		t.Fatalf("deadline recording report = %#v", report)
	}
	if transport.serveHasDeadline {
		t.Fatal("Serve received an AcceptDeadline; session context must remain unbounded")
	}
	if !transport.acceptHasDeadline {
		t.Fatal("Accept did not receive a bounded context")
	}
}

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

func TestPairTransportErrorPreservesTailcatCause(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := pairTransportFor(&PairDeps{})
	if err == nil || !strings.Contains(err.Error(), "tailcat transport") {
		t.Fatalf("pair transport error = %v, want wrapped transport context", err)
	}
	if !errors.Is(err, pair.ErrNoTailcat) {
		t.Fatalf("pair transport error = %v, want ErrNoTailcat cause", err)
	}
}

type blockingPairConn struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingPairConn() *blockingPairConn {
	return &blockingPairConn{closed: make(chan struct{})}
}

func (conn *blockingPairConn) Read([]byte) (int, error) {
	<-conn.closed
	return 0, io.EOF
}

func (conn *blockingPairConn) Write(payload []byte) (int, error) {
	select {
	case <-conn.closed:
		return 0, io.ErrClosedPipe
	default:
		return len(payload), nil
	}
}

func (conn *blockingPairConn) Close() error {
	conn.once.Do(func() { close(conn.closed) })
	return nil
}

type singleConnTransport struct{ conn io.ReadWriteCloser }

func (transport singleConnTransport) Serve(context.Context) (pair.PairServer, error) {
	return nil, errors.New("unexpected Serve")
}

func (transport singleConnTransport) Connect(context.Context, string) (io.ReadWriteCloser, error) {
	return transport.conn, nil
}

func TestPairJoinPrintsConfirmationWaitAndTimeoutText(t *testing.T) {
	fixture := newPairFixture(t, []string{"shared"}, []string{"shared"})
	conn := newBlockingPairConn()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var output bytes.Buffer
	report := runPairJoin(ctx, PairOptions{HomerHome: fixture.bHome, Addr: "pipe-test"}, &PairDeps{
		Transport: singleConnTransport{conn: conn},
	}, &output)
	if report.OK || !containsSubstring(report.Errors, "等待确认超时") {
		t.Fatalf("join timeout report = %#v", report)
	}
	if !strings.Contains(output.String(), "等待对端确认…（最长 120s）") {
		t.Fatalf("join progress = %q", output.String())
	}
}

type frameErrorConn struct {
	reader *bytes.Reader
}

func (conn *frameErrorConn) Read(payload []byte) (int, error)  { return conn.reader.Read(payload) }
func (conn *frameErrorConn) Write(payload []byte) (int, error) { return len(payload), nil }
func (conn *frameErrorConn) Close() error                      { return nil }

func TestPairJoinFrameErrorReportsPeerReason(t *testing.T) {
	fixture := newPairFixture(t, []string{"shared"}, []string{"shared"})
	var frame bytes.Buffer
	if err := pair.WriteFrame(&frame, pair.FrameError, []byte(`{"reason":"DERP 中段断流"}`)); err != nil {
		t.Fatal(err)
	}
	report := RunPairJoin(PairOptions{HomerHome: fixture.bHome, Addr: "pipe-test"}, &PairDeps{
		Transport: singleConnTransport{conn: &frameErrorConn{reader: bytes.NewReader(frame.Bytes())}},
	})
	if report.OK || !containsSubstring(report.Errors, "DERP 中段断流") {
		t.Fatalf("FrameError report = %#v", report)
	}
}

type blockingConfirm struct {
	started chan struct{}
	release chan struct{}
}

func (prompt blockingConfirm) Confirm(string, bool) bool {
	close(prompt.started)
	<-prompt.release
	return false
}

func TestPairServeContextCancellationDuringConfirmClosesSession(t *testing.T) {
	fixture := newPairFixture(t, []string{"shared"}, []string{"shared"})
	left, right := pairtest.NewPipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	addr := make(chan string, 1)
	result := make(chan PairServeReport, 1)
	go func() {
		result <- runPairServe(ctx, PairOptions{HomerHome: fixture.aHome}, &PairDeps{
			Transport: left,
			UI:        blockingConfirm{started: started, release: release},
			OnAddr:    func(value string) { addr <- value },
		}, nil)
	}()
	select {
	case <-addr:
	case <-time.After(time.Second):
		t.Fatal("serve did not publish address")
	}
	client, err := right.Connect(context.Background(), "pipe-test")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	hello := pair.PeerHello{Version: pair.ProtocolVersion, Hostname: "cancel-peer", Recipient: fixture.bIdentity.Recipient}
	writeDone := make(chan error, 1)
	go func() { writeDone <- pair.WriteFrame(client, pair.FrameHello, mustJSON(t, hello)) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("serve did not enter confirmation")
	}
	cancel()
	select {
	case report := <-result:
		if report.OK || !containsSubstring(report.Errors, "context canceled") {
			t.Fatalf("cancelled serve report = %#v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("serve did not return after context cancellation")
	}
	_ = <-writeDone
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func containsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}
