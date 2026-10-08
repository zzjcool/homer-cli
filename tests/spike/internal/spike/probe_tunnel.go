package spike

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type execTunnel struct {
	cmd  *exec.Cmd
	done chan error
	once sync.Once
}

func (t *execTunnel) stop() {
	if t == nil || t.cmd == nil || t.cmd.Process == nil {
		return
	}
	t.once.Do(func() {
		_ = t.cmd.Process.Kill()
		select {
		case <-t.done:
		case <-time.After(5 * time.Second):
		}
	})
}

func runV9(ctx context.Context, cfg ProbeConfig) (ProbeResult, string, *execTunnel) {
	started := time.Now()
	if cfg.CloudflaredPID <= 0 {
		return skippedResult("V9", probeName("V9"), "set -cloudflared-pid to the isolated TryCloudflare process PID"), "", nil
	}
	origin, err := url.Parse(cfg.CloudflaredOrigin)
	if err != nil || origin.Scheme != "http" || !isLoopbackHost(origin.Hostname()) {
		return failedResult("V9", probeName("V9"), 0, map[string]any{"origin": cfg.CloudflaredOrigin}, fmt.Errorf("refusing tunnel restart: origin must be an http URL on loopback")), "", nil
	}
	cmdline, err := readProcCmdline(cfg.CloudflaredPID)
	if err != nil {
		return failedResult("V9", probeName("V9"), 0, map[string]any{"cloudflared_pid": cfg.CloudflaredPID}, fmt.Errorf("read cloudflared command line: %w", err)), "", nil
	}
	cmdLower := strings.ToLower(cmdline)
	if !strings.Contains(cmdLower, "cloudflared") || !strings.Contains(cmdline, cfg.CloudflaredOrigin) || !strings.Contains(cmdline, "--url") || !strings.Contains(cmdLower, "--protocol http2") {
		return failedResult("V9", probeName("V9"), 0, map[string]any{"cloudflared_pid": cfg.CloudflaredPID, "cloudflared_cmdline": cmdline, "expected_origin": cfg.CloudflaredOrigin}, fmt.Errorf("refusing to restart PID %d: it is not the isolated --protocol http2 quick tunnel for the configured loopback origin", cfg.CloudflaredPID)), "", nil
	}
	oldConnection, err := dialAndReadUpgrade(ctx, cfg.URL, cfg, false)
	if err != nil {
		return failedResult("V9", probeName("V9"), time.Since(started), map[string]any{"cloudflared_pid": cfg.CloudflaredPID, "cloudflared_cmdline": cmdline}, fmt.Errorf("connect before temporary cloudflared restart: %w", err)), "", nil
	}
	oldReadDone := make(chan error, 1)
	go func() {
		_, _, readErr := oldConnection.conn.Read(ctx)
		oldReadDone <- readErr
	}()

	restartStarted := time.Now()
	process, err := os.FindProcess(cfg.CloudflaredPID)
	if err == nil {
		err = process.Kill()
	}
	if err != nil {
		oldConnection.conn.CloseNow()
		return failedResult("V9", probeName("V9"), time.Since(started), map[string]any{"cloudflared_pid": cfg.CloudflaredPID}, fmt.Errorf("kill isolated cloudflared process: %w", err)), "", nil
	}

	var disconnectAfter float64
	disconnectTimer := time.NewTimer(cfg.FailureTimeout)
	select {
	case readErr := <-oldReadDone:
		disconnectTimer.Stop()
		disconnectAfter = time.Since(restartStarted).Seconds()
		oldConnection.conn.CloseNow()
		_ = readErr
	case <-disconnectTimer.C:
		oldConnection.conn.CloseNow()
		return failedResult("V9", probeName("V9"), time.Since(started), map[string]any{"cloudflared_pid": cfg.CloudflaredPID, "old_connection_disconnected": false, "disconnect_timeout_seconds": cfg.FailureTimeout.Seconds()}, fmt.Errorf("old WebSocket did not detect tunnel process restart within %s", cfg.FailureTimeout)), "", nil
	case <-ctx.Done():
		disconnectTimer.Stop()
		oldConnection.conn.CloseNow()
		return failedResult("V9", probeName("V9"), time.Since(started), nil, ctx.Err()), "", nil
	}

	var logFile *os.File
	if cfg.CloudflaredLog != "" {
		logFile, err = os.OpenFile(cfg.CloudflaredLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return failedResult("V9", probeName("V9"), time.Since(started), map[string]any{"old_connection_disconnected": true, "disconnect_after_seconds": disconnectAfter}, fmt.Errorf("open temporary tunnel log: %w", err)), "", nil
		}
		defer logFile.Close()
	}
	capture := &synchronizedBuffer{}
	var output io.Writer = capture
	if logFile != nil {
		output = io.MultiWriter(capture, logFile)
	}
	cmd := exec.CommandContext(ctx, cfg.CloudflaredBinary, "tunnel", "--protocol", "http2", "--url", cfg.CloudflaredOrigin, "--no-autoupdate")
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return failedResult("V9", probeName("V9"), time.Since(started), map[string]any{"old_connection_disconnected": true, "disconnect_after_seconds": disconnectAfter}, fmt.Errorf("restart cloudflared: %w", err)), "", nil
	}
	tunnel := &execTunnel{cmd: cmd, done: make(chan error, 1)}
	go func() {
		tunnel.done <- cmd.Wait()
	}()

	startupDeadline := time.Now().Add(180 * time.Second)
	var newHost string
	for time.Now().Before(startupDeadline) {
		if match := tryCloudflareURL.FindString(capture.String()); match != "" {
			newHost = strings.TrimSuffix(match, "/")
			break
		}
		select {
		case commandErr := <-tunnel.done:
			tunnel.done <- commandErr
			return failedResult("V9", probeName("V9"), time.Since(started), map[string]any{"old_connection_disconnected": true, "disconnect_after_seconds": disconnectAfter, "cloudflared_log_tail": tail(capture.String(), 3000)}, fmt.Errorf("restarted cloudflared exited before creating a TryCloudflare URL: %v", commandErr)), "", tunnel
		case <-ctx.Done():
			tunnel.stop()
			return failedResult("V9", probeName("V9"), time.Since(started), nil, ctx.Err()), "", tunnel
		case <-time.After(500 * time.Millisecond):
		}
	}
	if newHost == "" {
		tunnel.stop()
		return failedResult("V9", probeName("V9"), time.Since(started), map[string]any{"old_connection_disconnected": true, "disconnect_after_seconds": disconnectAfter, "cloudflared_log_tail": tail(capture.String(), 3000)}, fmt.Errorf("no https://*.trycloudflare.com URL observed in restarted cloudflared logs within 180s")), "", tunnel
	}
	newWSURL, err := targetURL(newHost, "/ws")
	if err != nil {
		tunnel.stop()
		return failedResult("V9", probeName("V9"), time.Since(started), map[string]any{"new_trycloudflare_url": newHost}, err), "", tunnel
	}
	newWSURL, err = websocketURL(newWSURL)
	if err != nil {
		tunnel.stop()
		return failedResult("V9", probeName("V9"), time.Since(started), map[string]any{"new_trycloudflare_url": newHost}, err), "", tunnel
	}
	var reconnectAfter float64
	var lastDialErr error
	for time.Now().Before(startupDeadline) {
		tryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		connection, dialErr := dialAndReadUpgrade(tryCtx, newWSURL, cfg, false)
		cancel()
		if dialErr == nil {
			connection.conn.CloseNow()
			reconnectAfter = time.Since(restartStarted).Seconds()
			metrics := map[string]any{
				"cloudflared_pid_before_restart": cfg.CloudflaredPID,
				"cloudflared_cmdline_before":     cmdline,
				"old_connection_disconnected":    true,
				"old_disconnect_after_seconds":   disconnectAfter,
				"new_trycloudflare_url":          newHost,
				"new_connection_reconnected":     true,
				"reconnect_after_seconds":        reconnectAfter,
				"restarted_origin":               cfg.CloudflaredOrigin,
				"protocol":                       "http2",
			}
			return passedResult("V9", time.Since(started), metrics), newWSURL, tunnel
		}
		lastDialErr = dialErr
		select {
		case <-ctx.Done():
			tunnel.stop()
			return failedResult("V9", probeName("V9"), time.Since(started), map[string]any{"new_trycloudflare_url": newHost, "last_dial_error": lastDialErr.Error()}, ctx.Err()), "", tunnel
		case commandErr := <-tunnel.done:
			tunnel.done <- commandErr
			return failedResult("V9", probeName("V9"), time.Since(started), map[string]any{"new_trycloudflare_url": newHost, "cloudflared_log_tail": tail(capture.String(), 3000)}, fmt.Errorf("restarted cloudflared exited before client reconnect: %v", commandErr)), "", tunnel
		case <-time.After(2 * time.Second):
		}
	}
	tunnel.stop()
	return failedResult("V9", probeName("V9"), time.Since(started), map[string]any{"new_trycloudflare_url": newHost, "old_disconnect_after_seconds": disconnectAfter, "last_dial_error": errorString(lastDialErr), "cloudflared_log_tail": tail(capture.String(), 3000)}, fmt.Errorf("client did not reconnect within 180s after cloudflared restart")), "", tunnel
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func tail(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[len(text)-limit:]
}
