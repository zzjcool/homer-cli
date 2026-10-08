package spike

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestEchoUpgradeEchoCloseAndNDJSON(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel, done := startEcho(t, EchoConfig{Addr: addr, NDJSONCount: 3, NDJSONInterval: 20 * time.Millisecond})
	defer stopService(t, cancel, done)
	probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
	defer probeCancel()
	wsURL := "ws://" + addr + "/ws"
	cfg := ProbeConfig{URL: wsURL, NDJSONCount: 3, NDJSONInterval: 20 * time.Millisecond}

	upgrade := probeV1(probeCtx, cfg)
	if upgrade.Status != "pass" {
		t.Fatalf("V1 local handshake failed: %+v", upgrade)
	}
	echo := probeV2(probeCtx, cfg)
	if echo.Status != "pass" {
		t.Fatalf("V2 local echo failed: %+v", echo)
	}
	closeResult := probeV6(probeCtx, cfg)
	if closeResult.Status != "pass" {
		t.Fatalf("V6 custom close failed: %+v", closeResult)
	}
	streaming := probeR13(probeCtx, cfg)
	if streaming.Status != "pass" {
		t.Fatalf("R13 local NDJSON failed: %+v", streaming)
	}
}

func TestNoHijackerIsExplicitHTTP500(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel, done := startEcho(t, EchoConfig{Addr: addr})
	defer stopService(t, cancel, done)
	probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
	defer probeCancel()
	result := probeV12(probeCtx, ProbeConfig{URL: "ws://" + addr + "/ws"})
	if result.Status != "pass" {
		t.Fatalf("V12 should reject a ResponseWriter without Hijacker explicitly: %+v", result)
	}
	body, _ := result.Metrics["response_body"].(string)
	dialError, _ := result.Metrics["dial_error"].(string)
	if !strings.Contains(strings.ToLower(body), "not implemented") && !strings.Contains(dialError, "http.Hijacker") {
		t.Fatalf("V12 response/error should explain upgrade failure, body=%q error=%q", body, dialError)
	}
}

func TestV3AndV4ShortTimingScenarios(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel, done := startEcho(t, EchoConfig{Addr: addr})
	defer stopService(t, cancel, done)
	probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
	defer probeCancel()
	cfg := ProbeConfig{
		URL:             "ws://" + addr + "/ws",
		IdleTargets:     []time.Duration{40 * time.Millisecond, 80 * time.Millisecond},
		AppPingInterval: 10 * time.Millisecond,
		AppPingDuration: 40 * time.Millisecond,
		PingTimeout:     250 * time.Millisecond,
	}
	if result := probeV3(probeCtx, cfg); result.Status != "pass" {
		t.Fatalf("V3 short idle test failed: %+v", result)
	}
	if result := probeV4(probeCtx, cfg); result.Status != "pass" {
		t.Fatalf("V4 short application heartbeat failed: %+v", result)
	}
}

func TestV11HijackedConnectionOutlivesWriteTimeout(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel, done := startEcho(t, EchoConfig{Addr: addr, WriteTimeout: 3 * time.Second})
	defer stopService(t, cancel, done)
	probeCtx, probeCancel := context.WithTimeout(ctx, 12*time.Second)
	defer probeCancel()
	result := probeV11(probeCtx, ProbeConfig{
		URL:              "ws://" + addr + "/ws",
		WriteTimeoutURL:  "ws://" + addr + "/ws",
		WriteTimeoutWait: 4 * time.Second,
	})
	if result.Status != "pass" {
		t.Fatalf("V11 hijacked connection should survive WriteTimeout: %+v", result)
	}
}

func TestServerSupportsOnDemandPush(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel, done := startEcho(t, EchoConfig{Addr: addr})
	defer stopService(t, cancel, done)
	probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
	defer probeCancel()
	connection, err := dialAndReadUpgrade(probeCtx, "ws://"+addr+"/ws", ProbeConfig{}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.conn.CloseNow()
	request := []byte(`{"type":"request-push","id":"push-1","data":"hello"}`)
	if err := connection.conn.Write(probeCtx, websocket.MessageText, request); err != nil {
		t.Fatal(err)
	}
	_, response, err := connection.conn.Read(probeCtx)
	if err != nil {
		t.Fatal(err)
	}
	var pushed struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(response, &pushed); err != nil {
		t.Fatal(err)
	}
	if pushed.Type != "push" || pushed.ID != "push-1" {
		t.Fatalf("on-demand push = %s", response)
	}
}

func TestV5ProtocolPingAndV10ConcurrentRTT(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel, done := startEcho(t, EchoConfig{Addr: addr})
	defer stopService(t, cancel, done)
	probeCtx, probeCancel := context.WithTimeout(ctx, 10*time.Second)
	defer probeCancel()
	cfg := ProbeConfig{URL: "ws://" + addr + "/ws"}
	if result := probeV5(probeCtx, cfg); result.Status != "pass" {
		t.Fatalf("V5 protocol ping failed: %+v", result)
	}
	if result := probeV10(probeCtx, cfg); result.Status != "pass" {
		t.Fatalf("V10 concurrent request/response failed: %+v", result)
	}
}

func TestV8BlackholeDetectedAtPingTimeout(t *testing.T) {
	originAddr := freeAddr(t)
	proxyAddr := freeAddr(t)
	controlAddr := freeAddr(t)
	ctx, cancel, echoDone := startEcho(t, EchoConfig{Addr: originAddr})
	defer stopService(t, cancel, echoDone)

	proxyCtx, cancelProxy := context.WithCancel(ctx)
	proxyDone := make(chan error, 1)
	go func() {
		proxyDone <- ServeTCPProxy(proxyCtx, TCPProxyConfig{Addr: proxyAddr, Target: originAddr, ControlAddr: controlAddr})
	}()
	waitHTTP(t, "http://"+controlAddr+"/healthz")
	t.Cleanup(func() {
		cancelProxy()
		select {
		case <-proxyDone:
		case <-time.After(3 * time.Second):
			t.Error("TCP proxy did not stop before test deadline")
		}
	})

	probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
	defer probeCancel()
	cfg := ProbeConfig{
		URL:             "ws://" + proxyAddr + "/ws",
		ProxyControlURL: "http://" + controlAddr,
		AppPingInterval: 30 * time.Millisecond,
		PingTimeout:     120 * time.Millisecond,
	}
	result := probeV8(probeCtx, cfg)
	if result.Status != "pass" {
		t.Fatalf("V8 blackhole should be detected by application timeout: %+v", result)
	}
}

func TestSuiteParsingAndPercentiles(t *testing.T) {
	selected, err := parseSuite("v1, V6,R13")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 3 || selected[0] != "V1" || selected[1] != "V6" || selected[2] != "R13" {
		t.Fatalf("unexpected suite selection: %#v", selected)
	}
	if got := percentileMillis([]float64{5, 1, 3, 2, 4}, 0.5); got != 3 {
		t.Fatalf("p50 = %v, want 3", got)
	}
	if got := percentileMillis([]float64{5, 1, 3, 2, 4}, 0.95); got != 5 {
		t.Fatalf("p95 = %v, want 5", got)
	}
	if _, err := parseSuite("V99"); err == nil {
		t.Fatal("expected unknown suite item to fail")
	}
}

func startEcho(t *testing.T, cfg EchoConfig) (context.Context, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeEcho(ctx, cfg) }()
	waitHTTP(t, "http://"+cfg.Addr+"/healthz")
	return ctx, cancel, done
}

func stopService(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("service stop returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("service did not stop before test deadline")
	}
}

func waitHTTP(t *testing.T, rawURL string) {
	t.Helper()
	client := &http.Client{Timeout: 300 * time.Millisecond}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		request, _ := http.NewRequest(http.MethodGet, rawURL, nil)
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("HTTP endpoint %s did not become ready before deadline", rawURL)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestEchoRawEchoOnSingleConnection(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel, done := startEcho(t, EchoConfig{Addr: addr})
	defer stopService(t, cancel, done)
	probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
	defer probeCancel()
	connection, err := dialAndReadUpgrade(probeCtx, "ws://"+addr+"/ws", ProbeConfig{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.conn.CloseNow()
	payload := []byte("arbitrary text payload")
	if err := connection.conn.Write(probeCtx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	_, got, err := connection.conn.Read(probeCtx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo = %q, want %q", got, payload)
	}
}

func TestEchoUpgradeHeaderIsJSON(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel, done := startEcho(t, EchoConfig{Addr: addr})
	defer stopService(t, cancel, done)
	probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
	defer probeCancel()
	connection, err := dialAndReadUpgrade(probeCtx, "ws://"+addr+"/ws", ProbeConfig{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.conn.CloseNow()
	encoded, err := json.Marshal(connection.head)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(encoded) || connection.head.Type != "upgrade" {
		t.Fatalf("invalid upgrade metadata: %s", fmt.Sprint(encoded))
	}
}
