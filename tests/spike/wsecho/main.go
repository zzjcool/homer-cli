package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zzjcool/homer-cli/tests/spike/internal/spike"
)

func main() {
	var cfg spike.EchoConfig
	var proxyTo string
	var controlAddr string
	flag.StringVar(&cfg.Addr, "addr", "127.0.0.1:17801", "listen address (loopback by default)")
	flag.DurationVar(&cfg.WriteTimeout, "write-timeout", 0, "http.Server WriteTimeout (for V11 use 3s)")
	flag.BoolVar(&cfg.NoHijacker, "no-hijacker", false, "wrap every ResponseWriter without exposing http.Hijacker")
	flag.DurationVar(&cfg.PushInterval, "push-interval", 0, "periodically push a JSON message to each WebSocket")
	flag.IntVar(&cfg.PushCount, "push-count", 0, "number of periodic pushes per WebSocket (requires -push-interval)")
	flag.IntVar(&cfg.CloseCode, "close-code", 0, "send this close code after the handshake (0 disables; query may override)")
	flag.StringVar(&cfg.CloseReason, "close-reason", "", "close reason (query may override)")
	flag.IntVar(&cfg.NDJSONCount, "ndjson-count", 5, "number of lines served by /ndjson")
	flag.DurationVar(&cfg.NDJSONInterval, "ndjson-interval", time.Second, "delay between /ndjson lines")
	flag.Int64Var(&cfg.MaxReadSize, "max-read-size", 16<<20, "maximum WebSocket message bytes")
	flag.StringVar(&proxyTo, "proxy-to", "", "run a raw TCP forwarder to this address instead of the HTTP server")
	flag.StringVar(&controlAddr, "control-addr", "127.0.0.1:17803", "loopback HTTP control address in TCP proxy mode")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	if proxyTo != "" {
		err = spike.ServeTCPProxy(ctx, spike.TCPProxyConfig{
			Addr:        cfg.Addr,
			Target:      proxyTo,
			ControlAddr: controlAddr,
		})
	} else {
		err = spike.ServeEcho(ctx, cfg)
	}
	if err != nil {
		log.Printf("wsecho stopped with error: %v", err)
		os.Exit(1)
	}
}
