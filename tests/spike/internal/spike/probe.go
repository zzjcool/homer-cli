package spike

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

var allProbeIDs = []string{"V1", "V2", "V3", "V4", "V5", "V6", "V7", "V8", "V9", "V10", "V11", "V12", "R13"}

// ProbeConfig describes the endpoints and timing budgets for one probe run.
type ProbeConfig struct {
	URL               string
	Suite             string
	WriteTimeoutURL   string
	NoHijackerURL     string
	ProxyControlURL   string
	SourcePID         int
	CloudflaredPID    int
	CloudflaredBinary string
	CloudflaredOrigin string
	CloudflaredLog    string
	ResolveIP         string
	IdleTargets       []time.Duration
	AppPingInterval   time.Duration
	AppPingDuration   time.Duration
	PingTimeout       time.Duration
	FailureTimeout    time.Duration
	WriteTimeoutWait  time.Duration
	NDJSONCount       int
	NDJSONInterval    time.Duration
}

// ProbeResult is one pass/fail/not-run record. Metrics hold measured values;
// errors explain failures and skip reasons.
type ProbeResult struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Status     string         `json:"status"`
	Pass       *bool          `json:"pass,omitempty"`
	DurationMS int64          `json:"duration_ms"`
	Metrics    map[string]any `json:"metrics,omitempty"`
	Error      string         `json:"error,omitempty"`
}

// ProbeReport is written as JSON by wsprobe and includes every selected item.
type ProbeReport struct {
	Suite      string        `json:"suite"`
	URL        string        `json:"url"`
	StartedAt  string        `json:"started_at"`
	FinishedAt string        `json:"finished_at"`
	Status     string        `json:"status"`
	Results    []ProbeResult `json:"results"`
}

// RunProbeSuite runs each requested scenario and preserves canonical output
// ordering. In the all suite, the long idle and application-heartbeat probes
// run concurrently; tunnel restart and source kill are intentionally last so
// they cannot perturb the other measurements.
func RunProbeSuite(ctx context.Context, cfg ProbeConfig) ProbeReport {
	started := time.Now()
	report := ProbeReport{
		Suite:     cfg.Suite,
		URL:       cfg.URL,
		StartedAt: started.UTC().Format(time.RFC3339Nano),
		Status:    "pass",
	}
	cfg = normalizedProbeConfig(cfg)
	selected, err := parseSuite(cfg.Suite)
	if err != nil {
		report.Status = "fail"
		report.Results = []ProbeResult{failedResult("suite", "suite selection", 0, nil, err)}
		report.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return report
	}
	parsedURL, urlErr := url.Parse(cfg.URL)
	if urlErr != nil || parsedURL.Host == "" || (parsedURL.Scheme != "ws" && parsedURL.Scheme != "wss" && parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		report.Status = "fail"
		report.Results = []ProbeResult{failedResult("config", "WebSocket URL", 0, nil, fmt.Errorf("-url must be a valid ws(s) or http(s) URL"))}
		report.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return report
	}

	results := make(map[string]ProbeResult, len(selected))
	currentURL := cfg.URL
	var restartedTunnel *execTunnel
	cleanupTunnel := func() {
		if restartedTunnel != nil {
			restartedTunnel.stop()
		}
	}
	defer cleanupTunnel()

	if strings.EqualFold(strings.TrimSpace(cfg.Suite), "all") {
		fastIDs := []string{"V1", "V2", "V5", "V6", "V10", "V11", "V12", "R13"}
		var fastWG sync.WaitGroup
		var resultMu sync.Mutex
		for _, id := range fastIDs {
			if !selectedContains(selected, id) {
				continue
			}
			fastWG.Add(1)
			go func(id string) {
				defer fastWG.Done()
				probeCfg := cfg
				probeCfg.URL = currentURL
				probeResult := runOne(ctx, id, probeCfg)
				resultMu.Lock()
				results[id] = probeResult
				resultMu.Unlock()
			}(id)
		}
		fastWG.Wait()

		var longWG sync.WaitGroup
		for _, id := range []string{"V3", "V4"} {
			if !selectedContains(selected, id) {
				continue
			}
			longWG.Add(1)
			go func(id string) {
				defer longWG.Done()
				probeCfg := cfg
				probeCfg.URL = currentURL
				probeResult := runOne(ctx, id, probeCfg)
				resultMu.Lock()
				results[id] = probeResult
				resultMu.Unlock()
			}(id)
		}
		longWG.Wait()

		for _, id := range []string{"V8", "V9", "V7"} {
			if !selectedContains(selected, id) {
				continue
			}
			probeCfg := cfg
			probeCfg.URL = currentURL
			if id == "V9" {
				probeResult, newURL, tunnel := runV9(ctx, probeCfg)
				results[id] = probeResult
				if tunnel != nil {
					restartedTunnel = tunnel
				}
				if newURL != "" {
					currentURL = newURL
				}
				continue
			}
			probeCfg.URL = currentURL
			results[id] = runOne(ctx, id, probeCfg)
		}
	} else {
		for _, id := range selected {
			probeCfg := cfg
			probeCfg.URL = currentURL
			if id == "V9" {
				probeResult, newURL, tunnel := runV9(ctx, probeCfg)
				results[id] = probeResult
				if tunnel != nil {
					restartedTunnel = tunnel
				}
				if newURL != "" {
					currentURL = newURL
				}
				continue
			}
			results[id] = runOne(ctx, id, probeCfg)
		}
	}

	for _, id := range selected {
		if result, ok := results[id]; ok {
			report.Results = append(report.Results, result)
		} else {
			report.Results = append(report.Results, skippedResult(id, probeName(id), "not run"))
		}
	}
	for _, result := range report.Results {
		if result.Status == "fail" {
			report.Status = "fail"
			break
		}
	}
	report.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return report
}

func normalizedProbeConfig(cfg ProbeConfig) ProbeConfig {
	if cfg.IdleTargets == nil {
		cfg.IdleTargets = []time.Duration{60 * time.Second, 100 * time.Second, 150 * time.Second, 300 * time.Second, 600 * time.Second}
	}
	if cfg.AppPingInterval <= 0 {
		cfg.AppPingInterval = 25 * time.Second
	}
	if cfg.AppPingDuration <= 0 {
		cfg.AppPingDuration = 15 * time.Minute
	}
	if cfg.PingTimeout <= 0 {
		cfg.PingTimeout = 75 * time.Second
	}
	if cfg.FailureTimeout <= 0 {
		cfg.FailureTimeout = 30 * time.Second
	}
	if cfg.WriteTimeoutWait <= 0 {
		cfg.WriteTimeoutWait = 4 * time.Second
	}
	if cfg.NDJSONCount <= 0 {
		cfg.NDJSONCount = 5
	}
	if cfg.NDJSONInterval <= 0 {
		cfg.NDJSONInterval = time.Second
	}
	if cfg.CloudflaredBinary == "" {
		cfg.CloudflaredBinary = "cloudflared"
	}
	if cfg.CloudflaredOrigin == "" {
		cfg.CloudflaredOrigin = "http://127.0.0.1:17801"
	}
	return cfg
}

func parseSuite(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "all") {
		return append([]string(nil), allProbeIDs...), nil
	}
	known := make(map[string]bool, len(allProbeIDs))
	for _, id := range allProbeIDs {
		known[id] = true
	}
	seen := make(map[string]bool)
	var selected []string
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		id := strings.ToUpper(strings.TrimSpace(field))
		if !known[id] {
			return nil, fmt.Errorf("unknown suite item %q; valid items: %s", field, strings.Join(allProbeIDs, ","))
		}
		if !seen[id] {
			selected = append(selected, id)
			seen[id] = true
		}
	}
	if len(selected) == 0 {
		return nil, errors.New("suite selected no probes")
	}
	return selected, nil
}

func selectedContains(selected []string, id string) bool {
	for _, candidate := range selected {
		if candidate == id {
			return true
		}
	}
	return false
}

func runOne(ctx context.Context, id string, cfg ProbeConfig) ProbeResult {
	switch id {
	case "V1":
		return probeV1(ctx, cfg)
	case "V2":
		return probeV2(ctx, cfg)
	case "V3":
		return probeV3(ctx, cfg)
	case "V4":
		return probeV4(ctx, cfg)
	case "V5":
		return probeV5(ctx, cfg)
	case "V6":
		return probeV6(ctx, cfg)
	case "V7":
		return probeV7(ctx, cfg)
	case "V8":
		return probeV8(ctx, cfg)
	case "V10":
		return probeV10(ctx, cfg)
	case "V11":
		return probeV11(ctx, cfg)
	case "V12":
		return probeV12(ctx, cfg)
	case "R13":
		return probeR13(ctx, cfg)
	default:
		return skippedResult(id, probeName(id), "unknown probe")
	}
}

func probeName(id string) string {
	names := map[string]string{
		"V1":  "Authorization and WebSocket subprotocol pass-through",
		"V2":  "text frame round-trip sizes",
		"V3":  "idle connection survival",
		"V4":  "application ping keepalive",
		"V5":  "WebSocket protocol ping/pong",
		"V6":  "custom close code and reason pass-through",
		"V7":  "source process kill detection",
		"V8":  "source-side blackhole detection",
		"V9":  "temporary cloudflared restart and reconnect",
		"V10": "50 concurrent request/response RTT",
		"V11": "WriteTimeout after hijack",
		"V12": "middleware without http.Hijacker",
		"R13": "NDJSON line streaming through the tunnel",
	}
	return names[id]
}

func passedResult(id string, duration time.Duration, metrics map[string]any) ProbeResult {
	pass := true
	return ProbeResult{ID: id, Name: probeName(id), Status: "pass", Pass: &pass, DurationMS: duration.Milliseconds(), Metrics: metrics}
}

func failedResult(id, name string, duration time.Duration, metrics map[string]any, err error) ProbeResult {
	pass := false
	result := ProbeResult{ID: id, Name: name, Status: "fail", Pass: &pass, DurationMS: duration.Milliseconds(), Metrics: metrics}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

func skippedResult(id, name, reason string) ProbeResult {
	return ProbeResult{ID: id, Name: name, Status: "not-run", Error: reason}
}

type wsConnection struct {
	conn *websocket.Conn
	resp *http.Response
	head upgradeEcho
}

type upgradeEcho struct {
	Type           string      `json:"type"`
	RequestHeaders http.Header `json:"requestHeaders"`
	Subprotocol    string      `json:"subprotocol"`
}

func dialAndReadUpgrade(ctx context.Context, rawURL string, cfg ProbeConfig, authorization bool) (*wsConnection, error) {
	client, err := clientForURL(rawURL, cfg.ResolveIP)
	if err != nil {
		return nil, err
	}
	headers := make(http.Header)
	if authorization {
		headers.Set("Authorization", "Bearer homer-ws-spike-probe")
	}
	conn, response, err := websocket.Dial(ctx, rawURL, &websocket.DialOptions{
		HTTPClient:      client,
		HTTPHeader:      headers,
		Subprotocols:    []string{EchoSubprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("WebSocket dial failed with HTTP %d: %w", response.StatusCode, err)
		}
		return nil, fmt.Errorf("WebSocket dial failed: %w", err)
	}
	conn.SetReadLimit(16 << 20)
	if response == nil || response.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("expected HTTP 101, got response %#v", response)
	}
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	messageType, payload, err := conn.Read(readCtx)
	if err != nil {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("read upgrade echo: %w", err)
	}
	if messageType != websocket.MessageText {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("upgrade echo has message type %v, want text", messageType)
	}
	var echoed upgradeEcho
	if err := json.Unmarshal(payload, &echoed); err != nil {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("decode upgrade echo: %w", err)
	}
	return &wsConnection{conn: conn, resp: response, head: echoed}, nil
}

func clientForURL(rawURL, resolveIP string) (*http.Client, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := parsed.Hostname()
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("http.DefaultTransport is not *http.Transport")
	}
	clone := transport.Clone()
	if resolveIP != "" {
		baseDial := clone.DialContext
		if baseDial == nil {
			baseDial = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		}
		clone.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			addressHost, port, splitErr := net.SplitHostPort(address)
			if splitErr == nil && strings.EqualFold(addressHost, host) {
				address = net.JoinHostPort(resolveIP, port)
			}
			return baseDial(ctx, network, address)
		}
	}
	return &http.Client{Transport: clone}, nil
}

func targetURL(base, path string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	case "http", "https":
	default:
		return "", fmt.Errorf("unsupported URL scheme %q", parsed.Scheme)
	}
	parsed.Path = path
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func websocketURL(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported URL scheme %q", parsed.Scheme)
	}
	return parsed.String(), nil
}

func addCloseQuery(rawURL string, code int, reason string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("closeCode", fmt.Sprint(code))
	query.Set("closeReason", reason)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func closeDetails(err error) (int, string) {
	var closeErr websocket.CloseError
	if errors.As(err, &closeErr) {
		return int(closeErr.Code), closeErr.Reason
	}
	return int(websocket.CloseStatus(err)), ""
}

func headerValuesFold(header http.Header, name string) []string {
	for key, values := range header {
		if strings.EqualFold(key, name) {
			return values
		}
	}
	return nil
}

func percentileMillis(values []float64, percentile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	index := int(float64(len(sorted))*percentile+0.999999) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func hashBytes(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func readResponseBody(response *http.Response) string {
	if response == nil || response.Body == nil {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
	return strings.TrimSpace(string(body))
}

var tryCloudflareURL = regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)

func limitedString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func readProcCmdline(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(string(data), "\x00", " "), nil
}
