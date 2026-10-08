package spike

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

func probeV1(ctx context.Context, cfg ProbeConfig) ProbeResult {
	started := time.Now()
	connection, err := dialAndReadUpgrade(ctx, cfg.URL, cfg, true)
	if err != nil {
		return failedResult("V1", probeName("V1"), time.Since(started), nil, err)
	}
	defer connection.conn.CloseNow()
	authorization := headerValuesFold(connection.head.RequestHeaders, "Authorization")
	offeredProtocols := headerValuesFold(connection.head.RequestHeaders, "Sec-WebSocket-Protocol")
	authOK := len(authorization) == 1 && authorization[0] == "Bearer homer-ws-spike-probe"
	protocolSeen := false
	for _, offered := range offeredProtocols {
		for _, value := range strings.Split(offered, ",") {
			if strings.TrimSpace(value) == EchoSubprotocol {
				protocolSeen = true
			}
		}
	}
	metrics := map[string]any{
		"http_status":                connection.resp.StatusCode,
		"authorization_header_count": len(authorization),
		"authorization_echoed":       authOK,
		"offered_protocols":          offeredProtocols,
		"protocol_header_seen":       protocolSeen,
		"selected_subprotocol":       connection.head.Subprotocol,
		"client_subprotocol":         connection.conn.Subprotocol(),
	}
	if connection.resp.StatusCode != http.StatusSwitchingProtocols {
		return failedResult("V1", probeName("V1"), time.Since(started), metrics, fmt.Errorf("HTTP status was %d, want 101", connection.resp.StatusCode))
	}
	if !authOK {
		return failedResult("V1", probeName("V1"), time.Since(started), metrics, fmt.Errorf("Authorization header was not echoed exactly"))
	}
	if !protocolSeen || connection.head.Subprotocol != EchoSubprotocol || connection.conn.Subprotocol() != EchoSubprotocol {
		return failedResult("V1", probeName("V1"), time.Since(started), metrics, fmt.Errorf("subprotocol was not passed through and selected as %q", EchoSubprotocol))
	}
	return passedResult("V1", time.Since(started), metrics)
}

func probeV2(ctx context.Context, cfg ProbeConfig) ProbeResult {
	started := time.Now()
	connection, err := dialAndReadUpgrade(ctx, cfg.URL, cfg, false)
	if err != nil {
		return failedResult("V2", probeName("V2"), time.Since(started), nil, err)
	}
	defer connection.conn.CloseNow()

	sizes := []int{1 << 10, 1 << 20, 4 << 20, 8 << 20}
	checks := make([]map[string]any, 0, len(sizes))
	for _, size := range sizes {
		payload := make([]byte, size)
		for index := range payload {
			payload[index] = byte('a' + index%26)
		}
		wantHash := hashBytes(payload)
		startedRoundTrip := time.Now()
		writeCtx, cancelWrite := context.WithTimeout(ctx, 90*time.Second)
		err := connection.conn.Write(writeCtx, websocket.MessageText, payload)
		cancelWrite()
		if err != nil {
			metrics := map[string]any{"checks": checks, "failed_size_bytes": size}
			return failedResult("V2", probeName("V2"), time.Since(started), metrics, fmt.Errorf("write %d-byte frame: %w", size, err))
		}
		readCtx, cancelRead := context.WithTimeout(ctx, 90*time.Second)
		messageType, echoed, err := connection.conn.Read(readCtx)
		cancelRead()
		elapsed := time.Since(startedRoundTrip)
		if err != nil {
			metrics := map[string]any{"checks": checks, "failed_size_bytes": size, "round_trip_ms": elapsed.Seconds() * 1000}
			return failedResult("V2", probeName("V2"), time.Since(started), metrics, fmt.Errorf("read %d-byte echo: %w", size, err))
		}
		gotHash := hashBytes(echoed)
		check := map[string]any{
			"size_bytes":    size,
			"echo_bytes":    len(echoed),
			"round_trip_ms": elapsed.Seconds() * 1000,
			"sha256":        gotHash,
			"message_type":  messageType.String(),
			"lossless":      messageType == websocket.MessageText && len(echoed) == size && gotHash == wantHash,
		}
		checks = append(checks, check)
		if messageType != websocket.MessageText || len(echoed) != size || gotHash != wantHash {
			return failedResult("V2", probeName("V2"), time.Since(started), map[string]any{"checks": checks, "failed_size_bytes": size}, fmt.Errorf("%d-byte frame did not round-trip losslessly", size))
		}
	}
	return passedResult("V2", time.Since(started), map[string]any{"checks": checks, "message_sizes_bytes": sizes})
}

func probeV5(ctx context.Context, cfg ProbeConfig) ProbeResult {
	started := time.Now()
	connection, err := dialAndReadUpgrade(ctx, cfg.URL, cfg, false)
	if err != nil {
		return failedResult("V5", probeName("V5"), time.Since(started), nil, err)
	}
	defer connection.conn.CloseNow()

	readCtx, cancelRead := context.WithCancel(ctx)
	readDone := make(chan error, 1)
	go func() {
		for {
			_, _, readErr := connection.conn.Read(readCtx)
			if readErr != nil {
				readDone <- readErr
				return
			}
		}
	}()
	pingCtx, cancelPing := context.WithTimeout(ctx, 15*time.Second)
	pingStarted := time.Now()
	err = connection.conn.Ping(pingCtx)
	rtt := time.Since(pingStarted)
	cancelPing()
	cancelRead()
	_ = connection.conn.CloseNow()
	select {
	case <-readDone:
	case <-time.After(time.Second):
	}
	metrics := map[string]any{
		"protocol_ping_forwarded": err == nil,
		"pong_rtt_ms":             rtt.Seconds() * 1000,
		"client_subprotocol":      connection.conn.Subprotocol(),
	}
	if err != nil {
		return failedResult("V5", probeName("V5"), time.Since(started), metrics, fmt.Errorf("protocol Ping did not receive pong: %w", err))
	}
	return passedResult("V5", time.Since(started), metrics)
}

func probeV6(ctx context.Context, cfg ProbeConfig) ProbeResult {
	started := time.Now()
	checks := make([]map[string]any, 0, 2)
	for _, expected := range []struct {
		code   int
		reason string
	}{{4001, "spike-close-4001"}, {4401, "spike-close-4401"}} {
		closeURL, err := addCloseQuery(cfg.URL, expected.code, expected.reason)
		if err != nil {
			return failedResult("V6", probeName("V6"), time.Since(started), nil, err)
		}
		connection, err := dialAndReadUpgrade(ctx, closeURL, cfg, false)
		if err != nil {
			return failedResult("V6", probeName("V6"), time.Since(started), map[string]any{"checks": checks, "expected_code": expected.code}, err)
		}
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _, readErr := connection.conn.Read(readCtx)
		cancel()
		connection.conn.CloseNow()
		code, reason := closeDetails(readErr)
		check := map[string]any{
			"expected_code":   expected.code,
			"received_code":   code,
			"expected_reason": expected.reason,
			"received_reason": reason,
			"close_error":     readErr != nil,
		}
		checks = append(checks, check)
		if code != expected.code || reason != expected.reason {
			return failedResult("V6", probeName("V6"), time.Since(started), map[string]any{"checks": checks}, fmt.Errorf("close code/reason mismatch: got %d/%q, want %d/%q (read error: %v)", code, reason, expected.code, expected.reason, readErr))
		}
	}
	return passedResult("V6", time.Since(started), map[string]any{"checks": checks})
}

func probeV10(ctx context.Context, cfg ProbeConfig) ProbeResult {
	started := time.Now()
	connection, err := dialAndReadUpgrade(ctx, cfg.URL, cfg, false)
	if err != nil {
		return failedResult("V10", probeName("V10"), time.Since(started), nil, err)
	}
	defer connection.conn.CloseNow()

	const requestCount = 50
	probeCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	type response struct {
		id  string
		rtt time.Duration
		err error
	}
	responses := make(chan response, requestCount)
	sentAt := make(map[string]time.Time, requestCount)
	var sentMu sync.RWMutex
	var writeMu sync.Mutex
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for index := 0; index < requestCount; index++ {
			_, payload, readErr := connection.conn.Read(probeCtx)
			if readErr != nil {
				responses <- response{err: readErr}
				return
			}
			var reply struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(payload, &reply) != nil || reply.ID == "" {
				responses <- response{err: fmt.Errorf("invalid echo response %q", limitedString(string(payload), 128))}
				return
			}
			sentMu.RLock()
			when, ok := sentAt[reply.ID]
			sentMu.RUnlock()
			if !ok {
				responses <- response{err: fmt.Errorf("response id %q was not sent", reply.ID)}
				return
			}
			responses <- response{id: reply.ID, rtt: time.Since(when)}
		}
	}()

	var writers sync.WaitGroup
	writeErrors := make(chan error, requestCount)
	for index := 0; index < requestCount; index++ {
		id := fmt.Sprintf("r-%03d", index+1)
		payload, _ := json.Marshal(map[string]any{"type": "req", "id": id, "payload": "small-spike-request"})
		writers.Add(1)
		go func(id string, payload []byte) {
			defer writers.Done()
			sentMu.Lock()
			sentAt[id] = time.Now()
			sentMu.Unlock()
			writeMu.Lock()
			writeErr := connection.conn.Write(probeCtx, websocket.MessageText, payload)
			writeMu.Unlock()
			if writeErr != nil {
				writeErrors <- writeErr
			}
		}(id, payload)
	}
	writers.Wait()
	close(writeErrors)
	var firstWriteErr error
	for writeErr := range writeErrors {
		if firstWriteErr == nil {
			firstWriteErr = writeErr
		}
	}

	rtts := make([]float64, 0, requestCount)
	ids := make(map[string]bool, requestCount)
	var responseErr error
	for index := 0; index < requestCount; index++ {
		select {
		case got := <-responses:
			if got.err != nil {
				responseErr = got.err
				index = requestCount
				continue
			}
			ids[got.id] = true
			rtts = append(rtts, got.rtt.Seconds()*1000)
		case <-probeCtx.Done():
			responseErr = probeCtx.Err()
			index = requestCount
		}
	}
	<-readDone
	metrics := map[string]any{
		"requests":          requestCount,
		"responses":         len(rtts),
		"unique_ids":        len(ids),
		"rtt_p50_ms":        percentileMillis(rtts, 0.50),
		"rtt_p95_ms":        percentileMillis(rtts, 0.95),
		"rtt_min_ms":        minFloat(rtts),
		"rtt_max_ms":        maxFloat(rtts),
		"single_connection": true,
	}
	if firstWriteErr != nil {
		return failedResult("V10", probeName("V10"), time.Since(started), metrics, fmt.Errorf("write concurrent request: %w", firstWriteErr))
	}
	if responseErr != nil {
		return failedResult("V10", probeName("V10"), time.Since(started), metrics, fmt.Errorf("read concurrent response: %w", responseErr))
	}
	if len(rtts) != requestCount || len(ids) != requestCount {
		return failedResult("V10", probeName("V10"), time.Since(started), metrics, fmt.Errorf("received %d unique responses; want %d", len(ids), requestCount))
	}
	return passedResult("V10", time.Since(started), metrics)
}

func minFloat(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	min := values[0]
	for _, value := range values[1:] {
		if value < min {
			min = value
		}
	}
	return min
}

func maxFloat(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	max := values[0]
	for _, value := range values[1:] {
		if value > max {
			max = value
		}
	}
	return max
}
