package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zzjcool/homer-cli/tests/spike/internal/spike"
)

func main() {
	var cfg spike.ProbeConfig
	var out string
	var idleTargets string
	var totalTimeout time.Duration
	flag.StringVar(&cfg.URL, "url", "", "WebSocket URL, e.g. wss://<host>/ws")
	flag.StringVar(&cfg.Suite, "suite", "all", "comma-separated probe IDs (V1..V12,R13) or all")
	flag.StringVar(&out, "out", "", "write JSON result to this file")
	flag.StringVar(&cfg.WriteTimeoutURL, "write-timeout-url", "", "WebSocket URL of a wsecho started with -write-timeout=3s")
	flag.StringVar(&cfg.NoHijackerURL, "no-hijacker-url", "", "HTTP/WebSocket URL of a ResponseWriter wrapper without Hijacker")
	flag.StringVar(&cfg.ProxyControlURL, "proxy-control-url", "", "loopback HTTP control URL for the source-side blackhole proxy")
	flag.IntVar(&cfg.SourcePID, "source-pid", 0, "PID of the isolated wsecho process to SIGKILL for V7")
	flag.IntVar(&cfg.CloudflaredPID, "cloudflared-pid", 0, "PID of this spike's TryCloudflare child process for V9")
	flag.StringVar(&cfg.CloudflaredBinary, "cloudflared-bin", "cloudflared", "cloudflared executable used for the isolated V9 restart")
	flag.StringVar(&cfg.CloudflaredOrigin, "cloudflared-origin", "http://127.0.0.1:17801", "loopback origin used by the isolated temporary tunnel")
	flag.StringVar(&cfg.CloudflaredLog, "cloudflared-log", "", "append V9 restart logs to this file")
	flag.StringVar(&cfg.ResolveIP, "resolve-ip", "", "optional edge IP override for sb-tun DNS interception")
	flag.StringVar(&idleTargets, "idle-targets", "60s,100s,150s,300s,600s", "comma-separated idle durations for V3")
	flag.DurationVar(&cfg.AppPingInterval, "ping-interval", 25*time.Second, "application ping interval for V4/V8")
	flag.DurationVar(&cfg.AppPingDuration, "ping-duration", 15*time.Minute, "application ping keepalive duration for V4")
	flag.DurationVar(&cfg.PingTimeout, "ping-timeout", 75*time.Second, "application ping timeout for V8")
	flag.DurationVar(&cfg.FailureTimeout, "failure-timeout", 30*time.Second, "maximum source/tunnel disconnect detection time")
	flag.DurationVar(&cfg.WriteTimeoutWait, "write-timeout-wait", 4*time.Second, "V11 wait after hijack; must exceed 3s")
	flag.IntVar(&cfg.NDJSONCount, "ndjson-count", 5, "expected lines from /ndjson")
	flag.DurationVar(&cfg.NDJSONInterval, "ndjson-interval", time.Second, "expected delay between /ndjson lines")
	flag.DurationVar(&totalTimeout, "timeout", 38*time.Minute, "overall probe context timeout")
	flag.Parse()

	if cfg.URL == "" {
		fmt.Fprintln(os.Stderr, "wsprobe: -url is required")
		os.Exit(2)
	}
	var err error
	cfg.IdleTargets, err = parseDurations(idleTargets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wsprobe: invalid -idle-targets: %v\n", err)
		os.Exit(2)
	}
	if cfg.WriteTimeoutWait <= 3*time.Second {
		fmt.Fprintln(os.Stderr, "wsprobe: -write-timeout-wait must exceed the 3s server WriteTimeout")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()
	log.Printf("wsprobe starting suite=%s url=%s", cfg.Suite, cfg.URL)
	report := spike.RunProbeSuite(ctx, cfg)
	encoded, marshalErr := json.MarshalIndent(report, "", "  ")
	if marshalErr != nil {
		fmt.Fprintf(os.Stderr, "wsprobe: encode JSON result: %v\n", marshalErr)
		os.Exit(1)
	}
	if out != "" {
		if err := os.WriteFile(out, append(encoded, '\n'), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "wsprobe: write JSON result to %s: %v\n", out, err)
			os.Exit(1)
		}
	}
	fmt.Println(string(encoded))
	for _, result := range report.Results {
		if result.Status == "fail" {
			os.Exit(1)
		}
	}
}

func parseDurations(raw string) ([]time.Duration, error) {
	var result []time.Duration
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		duration, err := time.ParseDuration(item)
		if err != nil || duration <= 0 {
			if err == nil {
				err = fmt.Errorf("duration must be positive")
			}
			return nil, fmt.Errorf("%q: %w", item, err)
		}
		result = append(result, duration)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("at least one duration is required")
	}
	return result, nil
}
