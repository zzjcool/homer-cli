package spike

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func probeV3(ctx context.Context, cfg ProbeConfig) ProbeResult {
	started := time.Now()
	type idleCheck struct {
		Target time.Duration
		Result map[string]any
		Err    error
	}
	checks := make(chan idleCheck, len(cfg.IdleTargets))
	for _, target := range cfg.IdleTargets {
		target := target
		go func() {
			checks <- runIdleCheck(ctx, cfg, target)
		}()
	}
	metricsChecks := make([]map[string]any, 0, len(cfg.IdleTargets))
	passed := true
	firstDisconnect := 0.0
	maxSurvived := 0.0
	minFailed := 0.0
	for range cfg.IdleTargets {
		check := <-checks
		if check.Err != nil {
			passed = false
			if minFailed == 0 || check.Target.Seconds() < minFailed {
				minFailed = check.Target.Seconds()
			}
		} else if survived, _ := check.Result["survived"].(bool); survived {
			if check.Target.Seconds() > maxSurvived {
				maxSurvived = check.Target.Seconds()
			}
		} else {
			passed = false
			observed, _ := check.Result["observed_seconds"].(float64)
			if firstDisconnect == 0 || observed < firstDisconnect {
				firstDisconnect = observed
			}
			if minFailed == 0 || check.Target.Seconds() < minFailed {
				minFailed = check.Target.Seconds()
			}
		}
		metricsChecks = append(metricsChecks, check.Result)
	}
	if firstDisconnect == 0 {
		for _, check := range metricsChecks {
			if check["survived"] == false {
				if observed, ok := check["observed_seconds"].(float64); ok && (firstDisconnect == 0 || observed < firstDisconnect) {
					firstDisconnect = observed
				}
			}
		}
	}
	metrics := map[string]any{
		"checks":                            metricsChecks,
		"target_idle_seconds":               durationsSeconds(cfg.IdleTargets),
		"first_disconnect_observed_seconds": nil,
		"max_survived_target_seconds":       maxSurvived,
	}
	if firstDisconnect > 0 {
		metrics["first_disconnect_observed_seconds"] = firstDisconnect
	}
	if minFailed > 0 {
		metrics["idle_limit_bracket_seconds"] = map[string]any{"at_least": maxSurvived, "less_than_or_equal_to": minFailed}
	} else {
		metrics["idle_limit_bracket_seconds"] = map[string]any{"at_least": maxSurvived, "upper_bound_seconds": nil}
	}
	if !passed {
		return failedResult("V3", probeName("V3"), time.Since(started), metrics, fmt.Errorf("one or more idle connections closed before their target duration"))
	}
	return passedResult("V3", time.Since(started), metrics)
}

func runIdleCheck(ctx context.Context, cfg ProbeConfig, target time.Duration) struct {
	Target time.Duration
	Result map[string]any
	Err    error
} {
	log.Printf("V3 idle probe starting target=%s", target)
	started := time.Now()
	connection, err := dialAndReadUpgrade(ctx, cfg.URL, cfg, false)
	if err != nil {
		return struct {
			Target time.Duration
			Result map[string]any
			Err    error
		}{target, map[string]any{"target_seconds": target.Seconds(), "survived": false, "observed_seconds": 0, "dial_elapsed_seconds": time.Since(started).Seconds()}, err}
	}
	defer connection.conn.CloseNow()
	idleStarted := time.Now()
	readDone := make(chan error, 1)
	go func() {
		_, _, readErr := connection.conn.Read(ctx)
		readDone <- readErr
	}()
	timer := time.NewTimer(target)
	defer timer.Stop()
	select {
	case readErr := <-readDone:
		observed := time.Since(idleStarted)
		code, reason := closeDetails(readErr)
		result := map[string]any{
			"target_seconds":   target.Seconds(),
			"observed_seconds": observed.Seconds(),
			"survived":         false,
			"close_code":       code,
			"close_reason":     reason,
		}
		return struct {
			Target time.Duration
			Result map[string]any
			Err    error
		}{target, result, fmt.Errorf("connection closed after %.3fs before %s: %v", observed.Seconds(), target, readErr)}
	case <-timer.C:
		observed := time.Since(idleStarted)
		_ = connection.conn.CloseNow()
		result := map[string]any{
			"target_seconds":   target.Seconds(),
			"observed_seconds": observed.Seconds(),
			"survived":         true,
			"close_code":       0,
			"close_reason":     "",
		}
		log.Printf("V3 idle probe survived target=%s", target)
		return struct {
			Target time.Duration
			Result map[string]any
			Err    error
		}{target, result, nil}
	case <-ctx.Done():
		_ = connection.conn.CloseNow()
		result := map[string]any{"target_seconds": target.Seconds(), "observed_seconds": time.Since(idleStarted).Seconds(), "survived": false, "context_done": true}
		return struct {
			Target time.Duration
			Result map[string]any
			Err    error
		}{target, result, ctx.Err()}
	}
}

func durationsSeconds(values []time.Duration) []float64 {
	result := make([]float64, 0, len(values))
	for _, value := range values {
		result = append(result, value.Seconds())
	}
	return result
}

func probeV4(ctx context.Context, cfg ProbeConfig) ProbeResult {
	started := time.Now()
	log.Printf("V4 application ping probe starting interval=%s duration=%s", cfg.AppPingInterval, cfg.AppPingDuration)
	connection, err := dialAndReadUpgrade(ctx, cfg.URL, cfg, false)
	if err != nil {
		return failedResult("V4", probeName("V4"), time.Since(started), nil, err)
	}
	defer connection.conn.CloseNow()

	probeStarted := time.Now()
	finishAt := probeStarted.Add(cfg.AppPingDuration)
	nextPingAt := probeStarted
	pingCount := 0
	maxRTT := time.Duration(0)
	for !nextPingAt.After(finishAt) {
		if err := waitUntil(ctx, nextPingAt); err != nil {
			metrics := map[string]any{"ping_count": pingCount, "interval_seconds": cfg.AppPingInterval.Seconds(), "target_duration_seconds": cfg.AppPingDuration.Seconds()}
			return failedResult("V4", probeName("V4"), time.Since(started), metrics, err)
		}
		pingID := fmt.Sprintf("p-%03d", pingCount+1)
		payload, _ := json.Marshal(map[string]string{"type": "app-ping", "id": pingID})
		pingStarted := time.Now()
		writeCtx, cancelWrite := context.WithTimeout(ctx, 10*time.Second)
		writeErr := connection.conn.Write(writeCtx, websocket.MessageText, payload)
		cancelWrite()
		if writeErr != nil {
			metrics := map[string]any{"ping_count": pingCount, "interval_seconds": cfg.AppPingInterval.Seconds(), "target_duration_seconds": cfg.AppPingDuration.Seconds()}
			return failedResult("V4", probeName("V4"), time.Since(started), metrics, fmt.Errorf("send application ping %s: %w", pingID, writeErr))
		}
		readCtx, cancelRead := context.WithTimeout(ctx, cfg.PingTimeout)
		_, response, readErr := connection.conn.Read(readCtx)
		cancelRead()
		if readErr != nil {
			code, reason := closeDetails(readErr)
			metrics := map[string]any{"ping_count": pingCount, "interval_seconds": cfg.AppPingInterval.Seconds(), "target_duration_seconds": cfg.AppPingDuration.Seconds(), "close_code": code, "close_reason": reason}
			return failedResult("V4", probeName("V4"), time.Since(started), metrics, fmt.Errorf("application ping %s did not receive pong: %w", pingID, readErr))
		}
		var pong struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(response, &pong); err != nil || pong.Type != "app-pong" || pong.ID != pingID {
			return failedResult("V4", probeName("V4"), time.Since(started), map[string]any{"ping_count": pingCount, "last_response": limitedString(string(response), 128)}, fmt.Errorf("invalid application pong for %s", pingID))
		}
		pingCount++
		if rtt := time.Since(pingStarted); rtt > maxRTT {
			maxRTT = rtt
		}
		nextPingAt = nextPingAt.Add(cfg.AppPingInterval)
	}
	if err := waitUntil(ctx, finishAt); err != nil {
		return failedResult("V4", probeName("V4"), time.Since(started), map[string]any{"ping_count": pingCount}, err)
	}
	actual := time.Since(probeStarted)
	metrics := map[string]any{
		"ping_interval_seconds":   cfg.AppPingInterval.Seconds(),
		"target_duration_seconds": cfg.AppPingDuration.Seconds(),
		"actual_duration_seconds": actual.Seconds(),
		"application_pings":       pingCount,
		"missed_pongs":            0,
		"connection_survived":     true,
		"max_pong_rtt_ms":         maxRTT.Seconds() * 1000,
	}
	if actual < cfg.AppPingDuration {
		return failedResult("V4", probeName("V4"), time.Since(started), metrics, fmt.Errorf("connection survived only %.3fs; wanted %s", actual.Seconds(), cfg.AppPingDuration))
	}
	log.Printf("V4 application ping probe passed pings=%d elapsed=%.3fs", pingCount, actual.Seconds())
	return passedResult("V4", time.Since(started), metrics)
}

func waitUntil(ctx context.Context, target time.Time) error {
	remaining := time.Until(target)
	if remaining <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func probeV7(ctx context.Context, cfg ProbeConfig) ProbeResult {
	started := time.Now()
	if cfg.SourcePID <= 0 {
		return skippedResult("V7", probeName("V7"), "set -source-pid to the isolated wsecho process PID")
	}
	cmdline, err := readProcCmdline(cfg.SourcePID)
	if err != nil {
		return failedResult("V7", probeName("V7"), 0, map[string]any{"source_pid": cfg.SourcePID}, fmt.Errorf("read source process command line: %w", err))
	}
	if !strings.Contains(strings.ToLower(cmdline), "wsecho") {
		return failedResult("V7", probeName("V7"), 0, map[string]any{"source_pid": cfg.SourcePID, "source_cmdline": cmdline}, fmt.Errorf("refusing to kill PID %d: command line does not identify wsecho", cfg.SourcePID))
	}
	connection, err := dialAndReadUpgrade(ctx, cfg.URL, cfg, false)
	if err != nil {
		return failedResult("V7", probeName("V7"), time.Since(started), map[string]any{"source_pid": cfg.SourcePID}, err)
	}
	defer connection.conn.CloseNow()
	readDone := make(chan error, 1)
	go func() {
		_, _, readErr := connection.conn.Read(ctx)
		readDone <- readErr
	}()
	killStarted := time.Now()
	process, err := os.FindProcess(cfg.SourcePID)
	if err == nil {
		err = process.Kill()
	}
	if err != nil {
		return failedResult("V7", probeName("V7"), time.Since(started), map[string]any{"source_pid": cfg.SourcePID, "source_cmdline": cmdline}, fmt.Errorf("SIGKILL isolated source process: %w", err))
	}
	failureTimer := time.NewTimer(cfg.FailureTimeout)
	defer failureTimer.Stop()
	select {
	case readErr := <-readDone:
		observed := time.Since(killStarted)
		code, reason := closeDetails(readErr)
		metrics := map[string]any{"source_pid": cfg.SourcePID, "source_cmdline": cmdline, "source_killed": true, "client_detected_after_seconds": observed.Seconds(), "close_code": code, "close_reason": reason, "read_error": fmt.Sprint(readErr)}
		return passedResult("V7", time.Since(started), metrics)
	case <-failureTimer.C:
		return failedResult("V7", probeName("V7"), time.Since(started), map[string]any{"source_pid": cfg.SourcePID, "source_killed": true, "failure_timeout_seconds": cfg.FailureTimeout.Seconds()}, fmt.Errorf("client did not detect source kill within %s", cfg.FailureTimeout))
	case <-ctx.Done():
		return failedResult("V7", probeName("V7"), time.Since(started), map[string]any{"source_pid": cfg.SourcePID, "source_killed": true}, ctx.Err())
	}
}

func probeV8(ctx context.Context, cfg ProbeConfig) ProbeResult {
	started := time.Now()
	if cfg.ProxyControlURL == "" {
		return skippedResult("V8", probeName("V8"), "set -proxy-control-url for the isolated source-side TCP blackhole proxy")
	}
	connection, err := dialAndReadUpgrade(ctx, cfg.URL, cfg, false)
	if err != nil {
		return failedResult("V8", probeName("V8"), time.Since(started), nil, err)
	}
	defer connection.conn.CloseNow()

	controlClient, err := clientForURL(cfg.ProxyControlURL, cfg.ResolveIP)
	if err != nil {
		return failedResult("V8", probeName("V8"), time.Since(started), nil, err)
	}
	resumeURL, err := controlEndpoint(cfg.ProxyControlURL, "/resume")
	if err != nil {
		return failedResult("V8", probeName("V8"), time.Since(started), nil, err)
	}
	defer func() {
		resumeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		request, _ := http.NewRequestWithContext(resumeCtx, http.MethodPost, resumeURL, nil)
		response, postErr := controlClient.Do(request)
		if postErr == nil && response != nil {
			_ = response.Body.Close()
		}
	}()
	blackholeURL, err := controlEndpoint(cfg.ProxyControlURL, "/blackhole")
	if err != nil {
		return failedResult("V8", probeName("V8"), time.Since(started), nil, err)
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, blackholeURL, nil)
	response, err := controlClient.Do(request)
	if err != nil {
		return failedResult("V8", probeName("V8"), time.Since(started), nil, fmt.Errorf("enable source blackhole: %w", err))
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return failedResult("V8", probeName("V8"), time.Since(started), map[string]any{"control_status": response.StatusCode}, fmt.Errorf("blackhole control returned HTTP %d", response.StatusCode))
	}

	readEvents := make(chan error, 1)
	go func() {
		for {
			_, _, readErr := connection.conn.Read(ctx)
			if readErr != nil {
				readEvents <- readErr
				return
			}
			readEvents <- errors.New("unexpected WebSocket frame received after blackhole was enabled")
			return
		}
	}()
	startedBlackhole := time.Now()
	deadline := startedBlackhole.Add(cfg.PingTimeout)
	pingCount := 0
	var detection string
	var detectedAfter time.Duration
	var detectionErr error
	ticker := time.NewTicker(cfg.AppPingInterval)
	defer ticker.Stop()
	if err := sendAppPing(ctx, connection.conn, "blackhole-0"); err != nil {
		detection = "write-error"
		detectedAfter = time.Since(startedBlackhole)
		detectionErr = err
	} else {
		pingCount++
		for detection == "" {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				detection = "application-ping-timeout"
				detectedAfter = time.Since(startedBlackhole)
				break
			}
			timer := time.NewTimer(remaining)
			select {
			case readErr := <-readEvents:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				detection = "socket-error"
				detectedAfter = time.Since(startedBlackhole)
				detectionErr = readErr
			case <-ticker.C:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				if time.Now().Before(deadline) {
					if pingErr := sendAppPing(ctx, connection.conn, fmt.Sprintf("blackhole-%d", pingCount)); pingErr != nil {
						detection = "write-error"
						detectedAfter = time.Since(startedBlackhole)
						detectionErr = pingErr
					} else {
						pingCount++
					}
				}
			case <-timer.C:
				detection = "application-ping-timeout"
				detectedAfter = time.Since(startedBlackhole)
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				detection = "context-canceled"
				detectedAfter = time.Since(startedBlackhole)
				detectionErr = ctx.Err()
			}
		}
	}
	_ = connection.conn.CloseNow()
	statsURL, _ := controlEndpoint(cfg.ProxyControlURL, "/stats")
	statsCtx, cancelStats := context.WithTimeout(context.Background(), 5*time.Second)
	statsRequest, _ := http.NewRequestWithContext(statsCtx, http.MethodGet, statsURL, nil)
	statsResponse, statsErr := controlClient.Do(statsRequest)
	cancelStats()
	stats := map[string]any{}
	if statsErr == nil && statsResponse != nil {
		_ = json.NewDecoder(io.LimitReader(statsResponse.Body, 4096)).Decode(&stats)
		_ = statsResponse.Body.Close()
	}
	dropped, _ := stats["bytes_dropped"].(float64)
	metrics := map[string]any{
		"configured_ping_timeout_seconds": cfg.PingTimeout.Seconds(),
		"ping_interval_seconds":           cfg.AppPingInterval.Seconds(),
		"application_pings_sent":          pingCount,
		"detection":                       detection,
		"detection_after_seconds":         detectedAfter.Seconds(),
		"proxy_stats":                     stats,
		"proxy_stats_error":               errorString(statsErr),
	}
	if detectionErr != nil {
		metrics["detection_error"] = detectionErr.Error()
	}
	if dropped <= 0 {
		return failedResult("V8", probeName("V8"), time.Since(started), metrics, fmt.Errorf("blackhole proxy reported no dropped bytes"))
	}
	if detection == "context-canceled" {
		return failedResult("V8", probeName("V8"), time.Since(started), metrics, detectionErr)
	}
	if detectedAfter > cfg.PingTimeout+5*time.Second {
		return failedResult("V8", probeName("V8"), time.Since(started), metrics, fmt.Errorf("half-open detected after %s, exceeding ping timeout %s", detectedAfter, cfg.PingTimeout))
	}
	return passedResult("V8", time.Since(started), metrics)
}

func sendAppPing(ctx context.Context, conn *websocket.Conn, id string) error {
	payload, _ := json.Marshal(map[string]string{"type": "app-ping", "id": id})
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, payload)
}

func controlEndpoint(base, path string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	parsed.Path = path
	parsed.RawPath = ""
	parsed.RawQuery = ""
	return parsed.String(), nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func probeV11(ctx context.Context, cfg ProbeConfig) ProbeResult {
	started := time.Now()
	if cfg.WriteTimeoutURL == "" {
		return skippedResult("V11", probeName("V11"), "set -write-timeout-url to the wsecho instance started with -write-timeout=3s")
	}
	target, err := websocketURL(cfg.WriteTimeoutURL)
	if err != nil {
		return failedResult("V11", probeName("V11"), 0, nil, err)
	}
	connection, err := dialAndReadUpgrade(ctx, target, cfg, false)
	if err != nil {
		return failedResult("V11", probeName("V11"), time.Since(started), nil, err)
	}
	defer connection.conn.CloseNow()
	if err := waitUntil(ctx, time.Now().Add(cfg.WriteTimeoutWait)); err != nil {
		return failedResult("V11", probeName("V11"), time.Since(started), map[string]any{"wait_seconds": cfg.WriteTimeoutWait.Seconds()}, err)
	}
	payload := []byte("write-timeout-hijack-survived")
	writeStarted := time.Now()
	writeCtx, cancelWrite := context.WithTimeout(ctx, 10*time.Second)
	writeErr := connection.conn.Write(writeCtx, websocket.MessageText, payload)
	cancelWrite()
	if writeErr != nil {
		return failedResult("V11", probeName("V11"), time.Since(started), map[string]any{"wait_seconds": cfg.WriteTimeoutWait.Seconds(), "write_error": writeErr.Error()}, fmt.Errorf("write after hijack wait: %w", writeErr))
	}
	readCtx, cancelRead := context.WithTimeout(ctx, 10*time.Second)
	messageType, echoed, readErr := connection.conn.Read(readCtx)
	cancelRead()
	metrics := map[string]any{
		"configured_server_write_timeout_seconds": 3,
		"wait_after_hijack_seconds":               cfg.WriteTimeoutWait.Seconds(),
		"round_trip_ms":                           time.Since(writeStarted).Seconds() * 1000,
		"echoed_bytes":                            len(echoed),
		"connection_alive_after_wait":             readErr == nil && messageType == websocket.MessageText && bytes.Equal(echoed, payload),
	}
	if readErr != nil || messageType != websocket.MessageText || !bytes.Equal(echoed, payload) {
		return failedResult("V11", probeName("V11"), time.Since(started), metrics, fmt.Errorf("connection failed after hijack: %v", readErr))
	}
	return passedResult("V11", time.Since(started), metrics)
}

func probeV12(ctx context.Context, cfg ProbeConfig) ProbeResult {
	started := time.Now()
	target := cfg.NoHijackerURL
	if target == "" {
		var err error
		target, err = targetURL(cfg.URL, "/no-hijacker/ws")
		if err != nil {
			return failedResult("V12", probeName("V12"), 0, nil, err)
		}
	}
	wsURL, err := websocketURL(target)
	if err != nil {
		return failedResult("V12", probeName("V12"), 0, nil, err)
	}
	client, err := clientForURL(wsURL, cfg.ResolveIP)
	if err != nil {
		return failedResult("V12", probeName("V12"), 0, nil, err)
	}
	conn, response, dialErr := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPClient:      client,
		Subprotocols:    []string{EchoSubprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if conn != nil {
		_ = conn.CloseNow()
	}
	status := 0
	body := ""
	if response != nil {
		status = response.StatusCode
		body = readResponseBody(response)
	}
	metrics := map[string]any{
		"http_status":           status,
		"upgrade_rejected":      dialErr != nil && status != http.StatusSwitchingProtocols,
		"response_body":         body,
		"dial_error":            errorString(dialErr),
		"writer_has_hijacker":   false,
		"middleware_test_route": target,
	}
	if status < http.StatusInternalServerError || dialErr == nil {
		return failedResult("V12", probeName("V12"), time.Since(started), metrics, fmt.Errorf("Accept did not explicitly reject a ResponseWriter without Hijacker; got HTTP %d, error=%v", status, dialErr))
	}
	return passedResult("V12", time.Since(started), metrics)
}

func probeR13(ctx context.Context, cfg ProbeConfig) ProbeResult {
	started := time.Now()
	ndjsonURL, err := targetURL(cfg.URL, "/ndjson")
	if err != nil {
		return failedResult("R13", probeName("R13"), 0, nil, err)
	}
	client, err := clientForURL(ndjsonURL, cfg.ResolveIP)
	if err != nil {
		return failedResult("R13", probeName("R13"), 0, nil, err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, ndjsonURL, nil)
	if err != nil {
		return failedResult("R13", probeName("R13"), 0, nil, err)
	}
	request.Header.Set("Accept", "application/x-ndjson")
	request.Header.Set("Cache-Control", "no-cache")
	response, err := client.Do(request)
	if err != nil {
		return failedResult("R13", probeName("R13"), time.Since(started), nil, fmt.Errorf("GET NDJSON endpoint: %w", err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return failedResult("R13", probeName("R13"), time.Since(started), map[string]any{"http_status": response.StatusCode, "body": string(body)}, fmt.Errorf("NDJSON endpoint returned HTTP %d", response.StatusCode))
	}
	contentType := response.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(contentType), "application/x-ndjson") {
		return failedResult("R13", probeName("R13"), time.Since(started), map[string]any{"content_type": contentType}, fmt.Errorf("Content-Type is %q, want application/x-ndjson", contentType))
	}
	reader := bufio.NewReader(response.Body)
	arrivals := make([]float64, 0, cfg.NDJSONCount)
	sequences := make([]int, 0, cfg.NDJSONCount)
	for len(arrivals) < cfg.NDJSONCount {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil {
			return failedResult("R13", probeName("R13"), time.Since(started), map[string]any{"lines_received": len(arrivals), "content_type": contentType}, fmt.Errorf("read NDJSON line %d: %w", len(arrivals)+1, readErr))
		}
		var value struct {
			Seq int `json:"seq"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(line), &value); err != nil {
			return failedResult("R13", probeName("R13"), time.Since(started), map[string]any{"lines_received": len(arrivals)}, fmt.Errorf("decode NDJSON line: %w", err))
		}
		arrivals = append(arrivals, time.Since(started).Seconds())
		sequences = append(sequences, value.Seq)
	}
	firstToLast := arrivals[len(arrivals)-1] - arrivals[0]
	expectedSpread := float64(cfg.NDJSONCount-1) * cfg.NDJSONInterval.Seconds()
	transferEncoding := append([]string(nil), response.TransferEncoding...)
	metrics := map[string]any{
		"http_status":                     response.StatusCode,
		"content_type":                    contentType,
		"response_protocol":               response.Proto,
		"transfer_encoding":               transferEncoding,
		"content_length":                  response.ContentLength,
		"line_count":                      len(arrivals),
		"line_sequences":                  sequences,
		"line_arrival_seconds":            arrivals,
		"first_line_to_last_line_seconds": firstToLast,
		"expected_first_to_last_seconds":  expectedSpread,
		"first_line_arrival_seconds":      arrivals[0],
		"last_line_arrival_seconds":       arrivals[len(arrivals)-1],
		"streamed_progressively":          firstToLast >= expectedSpread*0.5,
	}
	if firstToLast < expectedSpread*0.5 {
		return failedResult("R13", probeName("R13"), time.Since(started), metrics, fmt.Errorf("NDJSON lines appear buffered: first-to-last %.3fs, expected at least %.3fs", firstToLast, expectedSpread*0.5))
	}
	return passedResult("R13", time.Since(started), metrics)
}
