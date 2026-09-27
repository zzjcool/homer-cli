package cli

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

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

// runServe starts the local hub: HTTP API + web console. It blocks until the
// listener returns. web.NewServer enforces the loopback-without-token rule
// from plan §2.1; the CLI layer surfaces its error and exits 1.
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
	server, err := web.NewServer(web.ServeOptions{
		Addr:      boundAddr,
		HomerHome: options.Home,
		Token:     token,
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
	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      90 * time.Second,
	}
	if err := httpServer.Serve(listener); err != nil {
		writeLine(errOut, fmt.Sprintf("homer serve: %s", err.Error()))
		return 1
	}
	return 0
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
