package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zzjcool/homer-cli/internal/agentd"
	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/web"
)

// hubDefaults mirrors plan §2.6: serve binds loopback by default; the token
// falls back to HOMER_HUB_TOKEN. Both are overridable by flags.
const defaultServeAddr = "127.0.0.1:7760"

func hubTokenFromEnv(flagToken string) string {
	if flagToken != "" {
		return flagToken
	}
	return os.Getenv("HOMER_HUB_TOKEN")
}

// runServe starts the hub: HTTP API + web console + agent registration
// endpoints + the dispatcher that reaches connected agents. It blocks until
// the listener returns. web.NewServer enforces the loopback-without-token
// rule from plan §2.1; the CLI layer surfaces its error and exits 1.
func runServe(options CommandOptions, out, errOut io.Writer) int {
	token := hubTokenFromEnv(options.Token)
	listener, err := net.Listen("tcp", defaultAddr(options.Addr))
	if err != nil {
		writeLine(errOut, fmt.Sprintf("homer serve: 监听失败: %s", err.Error()))
		return 1
	}
	// The concrete bound address (ephemeral ports like :0 resolve here) is
	// what both the loopback check and the startup message must use.
	boundAddr := listener.Addr().String()
	registry := hub.NewRegistry()
	dispatcher := hub.NewDispatcher(registry, token)
	server, err := web.NewServer(web.ServeOptions{
		Addr:          boundAddr,
		HomerHome:     options.Home,
		Token:         token,
		Agents:        dispatcher,
		AgentEndpoint: hub.NewAgentAPI(registry, token),
	})
	if err != nil {
		_ = listener.Close()
		writeLine(errOut, fmt.Sprintf("homer serve: %s", err.Error()))
		return 1
	}
	writeLine(out, fmt.Sprintf("homer serve: http://%s（Ctrl+C 停止）", displayAddr(boundAddr)))
	if token != "" {
		writeLine(out, "已启用 token 鉴权（HOMER_HUB_TOKEN / --token）。")
	} else {
		writeLine(out, "未设置 token：仅回环地址访问受信任。")
	}
	writeLine(out, "agent 接入: homer agent --connect http://<本机地址>"+agentPortSuffix(boundAddr))
	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      90 * time.Second,
	}
	// Graceful shutdown: SIGINT/SIGTERM drains in-flight requests before the
	// process exits, matching the pair command's NotifyContext pattern.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveDone := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			serveDone <- err
			return
		}
		serveDone <- nil
	}()
	select {
	case err := <-serveDone:
		if err != nil {
			writeLine(errOut, fmt.Sprintf("homer serve: %s", err.Error()))
			return 1
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = httpServer.Shutdown(shutdownCtx)
		cancel()
		_ = <-serveDone
	}
	return 0
}

// runAgent runs the agent daemon in listen or connect mode. Mode exclusivity
// is validated by validateCommandOptions; agentd.Config.Mode re-checks it.
func runAgent(options CommandOptions, out, errOut io.Writer) int {
	token := hubTokenFromEnv(options.Token)
	config := agentd.Config{
		HomerHome:    options.Home,
		Token:        token,
		ListenAddr:   options.Listen,
		AdvertiseURL: options.Advertise,
		ConnectURL:   options.Connect,
		HubURL:       options.Hub,
		AgentID:      options.ID,
	}
	if _, err := config.Mode(); err != nil {
		writeLine(errOut, fmt.Sprintf("homer agent: %s", err.Error()))
		return 1
	}
	daemon := agentd.New(config, agentd.NewLocalExecutor(options.Home))
	writeLine(out, fmt.Sprintf("homer agent: %s 模式启动（Ctrl+C 停止）", agentModeLabel(config)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := daemon.Run(ctx); err != nil && ctx.Err() == nil {
		writeLine(errOut, fmt.Sprintf("homer agent: %s", err.Error()))
		return 1
	}
	return 0
}

func agentModeLabel(config agentd.Config) string {
	if mode, err := config.Mode(); err == nil {
		return string(mode)
	}
	return "未知"
}

// agentPortSuffix formats the connect hint so users on another machine can
// copy-paste the agent bootstrap command.
func agentPortSuffix(boundAddr string) string {
	if _, port, err := net.SplitHostPort(boundAddr); err == nil {
		return ":" + port
	}
	return ""
}

// displayAddr rewrites wildcard binds (0.0.0.0:x, [::]:x) to localhost for
// the startup message so the printed URL is always clickable.
func displayAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return net.JoinHostPort("localhost", port)
	}
	return addr
}

func defaultAddr(addr string) string {
	if addr == "" {
		return defaultServeAddr
	}
	return addr
}
