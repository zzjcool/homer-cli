package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zzjcool/homer-cli/internal/agentd"
	"github.com/zzjcool/homer-cli/internal/core"
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
	paths := ResolveHomerPaths(options.Home)
	listener, err := net.Listen("tcp", defaultAddr(options.Addr))
	if err != nil {
		writeLine(errOut, fmt.Sprintf("homer serve: 监听失败: %s", err.Error()))
		return 1
	}
	// The concrete bound address (ephemeral ports like :0 resolve here) is
	// what both the loopback check and the startup message must use.
	boundAddr := listener.Addr().String()
	loopback := isLoopbackBound(boundAddr)

	// --show-join prints the copy-paste agent bootstrap command and exits
	// without serving: the token is long-lived, so it must be fetchable
	// without scraping startup logs (Tailscale's "print once" follow-up).
	if options.ShowJoin {
		_ = listener.Close()
		return showJoinCommand(paths, boundAddr, options, out, errOut)
	}

	// Token lifecycle (advisor ruling): --token persists (deliberate
	// rotation), HOMER_HUB_TOKEN overrides this process only, then
	// keys/hub-token, and only a non-loopback bind without any of the three
	// generates a fresh token — loopback keeps working bare.
	token, created, tokenErr := resolveServeToken(paths, options.Token, loopback)
	if tokenErr != nil {
		_ = listener.Close()
		writeLine(errOut, fmt.Sprintf("homer serve: %s", tokenErr.Error()))
		return 1
	}
	registry := hub.NewRegistry()
	dispatcher := hub.NewDispatcher(registry, token)
	enrollment := hub.NewEnrollmentManager()
	server, err := web.NewServer(web.ServeOptions{
		Addr:          boundAddr,
		HomerHome:     options.Home,
		Token:         token,
		Agents:        dispatcher,
		AgentEndpoint: hub.NewAgentAPI(registry, token).SetEnrollment(enrollment),
		Enrollment:    enrollment,
	})
	if err != nil {
		_ = listener.Close()
		writeLine(errOut, fmt.Sprintf("homer serve: %s", err.Error()))
		return 1
	}
	writeLine(out, fmt.Sprintf("homer serve: http://%s（Ctrl+C 停止）", displayAddr(boundAddr)))
	// First-run without a password: the console is locked until the
	// administrator finishes setup in a browser. Loopback/LAN browsers set
	// the password bare; remote administrators use the one-time setup code
	// below (read it over SSH, enter it in the public page).
	if !hubReadPasswordFile(options.Home) {
		writeLine(out, "首次使用：请在浏览器打开上述地址，设置管理员密码完成初始化。")
		writeLine(out, "远程初始化码（一次性，公网页面需要）: "+server.SetupCode())
	}
	if token != "" {
		writeLine(out, "已启用 agent token 鉴权（keys/hub-token 持久化，--show-join 查看）。")
	}
	// The join command embeds the token exactly once — at generation time.
	// Later boots only point at --show-join, keeping the credential out of
	// recurring terminal scrollback and service logs.
	if created {
		writeLine(out, "")
		writeLine(out, "机器接入（复制到目标机器执行；token 仅本次显示）:")
		writeLine(out, joinCommand(boundAddr, token))
		writeLine(out, "")
		writeLine(out, "之后重新查看: homer serve --show-join")
	} else {
		writeLine(out, "agent 接入: homer agent --connect http://<本机地址>"+agentPortSuffix(boundAddr))
		writeLine(out, "查看含 token 的接入命令: homer serve --show-join")
	}
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
	paths := ResolveHomerPaths(options.Home)
	// Tailscale-style bootstrap: the install script lands the credential in
	// keys/hub-token on the agent machine. An hr_-prefixed value is a
	// one-time enrollment code (consumed at first registration for a
	// per-agent secret); anything else is a legacy shared hub token.
	// Priority stays flag > env > file.
	enrollCode := ""
	if strings.HasPrefix(token, "hr_") {
		enrollCode, token = token, ""
	}
	if token == "" && enrollCode == "" {
		if fileToken, ok := hub.ReadHubToken(paths.Home); ok {
			if strings.HasPrefix(fileToken, "hr_") {
				enrollCode = fileToken
			} else {
				token = fileToken
			}
		}
	}
	// A persisted per-agent secret (agent.json) supersedes shared tokens.
	var persisted agentd.AgentConfig
	if loaded, ok := agentd.LoadAgentConfig(paths.Home); ok {
		persisted = loaded
	}
	if persisted.AgentSecret != "" {
		// Already enrolled: keep using this machine's own secret.
		if token == "" && enrollCode == "" {
			token = persisted.AgentSecret
		}
	}
	config := agentd.Config{
		Home:         paths.Home,
		HomerHome:    options.Home,
		Token:        token,
		AgentSecret:  persisted.AgentSecret,
		EnrollCode:   enrollCode,
		ListenAddr:   options.Listen,
		AdvertiseURL: options.Advertise,
		ConnectURL:   options.Connect,
		HubURL:       options.Hub,
		AgentID:      options.ID,
	}
	// Fill in gaps from the persisted agent.json: a bare `homer agent`
	// restarts with the last successfully registered join state; explicit
	// flags always win (advisor ruling: failed URLs are never persisted,
	// so the file only ever holds a known-good configuration).
	resolved, err := agentd.ResolveConfig(config)
	if err != nil {
		writeLine(errOut, fmt.Sprintf("homer agent: %s", err.Error()))
		return 1
	}
	if _, err := resolved.Mode(); err != nil {
		writeLine(errOut, fmt.Sprintf("homer agent: %s", err.Error()))
		return 1
	}
	daemon := agentd.New(resolved, agentd.NewLocalExecutor(options.Home))
	writeLine(out, fmt.Sprintf("homer agent: %s 模式启动（Ctrl+C 停止）", agentModeLabel(resolved)))
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
// isLoopbackBound reports whether the serve bind is loopback-only (bare
// serve with no token stays allowed only in this case, plan §2.1).
func isLoopbackBound(boundAddr string) bool {
	host, _, err := net.SplitHostPort(boundAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// resolveServeToken applies the advisor-ruling priority for serve tokens:
// explicit --token (persisted = rotation) > HOMER_HUB_TOKEN (process-only)
// > keys/hub-token > freshly generated, and generation is only legal for
// non-loopback binds — bare `homer serve` on loopback must keep working.
func resolveServeToken(paths core.HomerPaths, flagToken string, loopback bool) (string, bool, error) {
	if strings.TrimSpace(flagToken) != "" || strings.TrimSpace(os.Getenv("HOMER_HUB_TOKEN")) != "" {
		token, _, err := hub.EnsureHubToken(paths.Home, flagToken)
		return token, false, err
	}
	if token, ok := hub.ReadHubToken(paths.Home); ok {
		return token, false, nil
	}
	if loopback {
		return "", false, nil
	}
	token, created, err := hub.EnsureHubToken(paths.Home, "")
	if err != nil {
		return "", false, err
	}
	return token, created, nil
}

// hubReadPasswordFile reports whether the administrator password has been
// set (keys/hub-password). First-run messaging depends on it.
func hubReadPasswordFile(home string) bool {
	paths := ResolveHomerPaths(home)
	_, err := os.Stat(filepath.Join(paths.Home, "keys", "hub-password"))
	return err == nil
}

// joinCommand renders the copy-paste agent bootstrap line. The token rides
// in an env prefix so it never shows in ps output on the agent machine.
func joinCommand(boundAddr, token string) string {
	host, port, err := net.SplitHostPort(boundAddr)
	if err != nil {
		return fmt.Sprintf("HOMER_HUB_TOKEN=%s homer agent --connect http://%s", token, boundAddr)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if lan, lanErr := hub.LanIPv4(); lanErr == nil {
			host = lan.String()
		} else {
			host = "<本机局域网 IP>"
		}
	}
	return fmt.Sprintf("HOMER_HUB_TOKEN=%s homer agent --connect http://%s", token, net.JoinHostPort(host, port))
}

// showJoinCommand backs `homer serve --show-join`: resolve the persisted
// token (never generate one here — a show-only path must not rotate), print
// the join command, exit 0. A missing token is a hint, not a failure.
func showJoinCommand(paths core.HomerPaths, boundAddr string, options CommandOptions, out, errOut io.Writer) int {
	token, ok := hub.ReadHubToken(paths.Home)
	if !ok {
		if strings.TrimSpace(options.Token) != "" {
			token = strings.TrimSpace(options.Token)
		} else if fromEnv := strings.TrimSpace(os.Getenv("HOMER_HUB_TOKEN")); fromEnv != "" {
			token = fromEnv
		} else {
			writeLine(errOut, "尚未配置 token：先用非回环地址启动一次 serve（自动生成）或用 --token 指定。")
			return 1
		}
	}
	writeLine(out, joinCommand(boundAddr, token))
	return 0
}

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
