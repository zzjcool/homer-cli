package agentd

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/wsconn"
	"github.com/zzjcool/homer-cli/internal/web"
)

type streamDialer func(context.Context, string, wsconn.DialOptions) (stream.Conn, *http.Response, error)

type retryPlan struct {
	delay  time.Duration
	key    string
	line   string
	always bool
}

func (d *Daemon) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if d == nil {
		return fmt.Errorf("nil agent daemon")
	}
	if d.exec == nil {
		return fmt.Errorf("agent executor is required")
	}
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		stableFor, err := d.runConnection(ctx)
		if stableFor >= 30*time.Second {
			// This failure is the first one after a stable connection, so use
			// the base delay and start a fresh backoff sequence.
			attempt = 0
		}
		if err == nil {
			if ctx.Err() != nil {
				return nil
			}
			// A completed session normally returns a close/error; a nil return
			// without cancellation is still a disconnect and must be retried.
			err = fmt.Errorf("stream session ended")
		}
		if ctx.Err() != nil {
			return nil
		}
		plan := d.retryFor(err, attempt)
		if plan.always {
			d.logf("%s", plan.line)
			if d.retries != nil {
				d.retries.Log(plan.key, "自动重连退避中："+plan.line)
			}
		} else if d.retries != nil {
			d.retries.Log(plan.key, plan.line)
		}
		// Every retry has already emitted its per-key throttled log before the
		// cancellable Backoff wait; there is no silent retry path.
		wait := d.retryWait
		if wait == nil {
			wait = sleepContext
		}
		if !wait(ctx, plan.delay) {
			if ctx.Err() != nil {
				return nil
			}
			// A custom waiter must not silently terminate Run. The failure was
			// already logged above; advance the backoff before retrying.
			attempt++
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		attempt++
	}
}

func (d *Daemon) runConnection(ctx context.Context) (time.Duration, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if d.dialStream == nil {
		d.dialStream = wsconn.Dial
	}
	header := make(http.Header)
	credential := d.streamCredential()
	if credential != "" {
		header.Set("Authorization", "Bearer "+credential)
	}
	conn, _, err := d.dialStream(ctx, streamURL(d.cfg.HubURL), wsconn.DialOptions{Header: header})
	if err != nil {
		return 0, err
	}
	session := stream.NewSession(conn, d.cfg.Stream)
	d.registerHandlers(session)
	ready := make(chan struct{})
	helloErr := make(chan error, 1)
	helloDone := make(chan struct{})
	runDone := make(chan error, 1)
	go func() { runDone <- session.Run(ctx) }()
	go func() {
		defer close(helloDone)
		err := d.hello(ctx, session)
		if err != nil {
			helloErr <- err
			session.Close(closeCodeForHello(err), closeReasonForHello(err))
			return
		}
		close(ready)
		go d.heartbeatLoop(ctx, session)
	}()
	select {
	case <-ready:
	case err := <-helloErr:
		<-helloDone
		<-runDone
		return 0, err
	case <-ctx.Done():
		session.Close(stream.CloseNormal, "agent exiting")
		<-helloDone
		<-runDone
		return 0, ctx.Err()
	case <-session.Done():
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		<-helloDone
		<-runDone
		select {
		case err := <-helloErr:
			return 0, err
		default:
			return 0, session.Err()
		}
	}
	connectedAt := time.Now()
	d.logf("已连接 hub %s", d.cfg.HubURL)
	var disconnectErr error
	select {
	case <-ctx.Done():
		session.Close(stream.CloseNormal, "agent exiting")
		<-runDone
		disconnectErr = ctx.Err()
	case err := <-runDone:
		if ctx.Err() != nil {
			disconnectErr = ctx.Err()
		} else if err != nil {
			disconnectErr = err
		} else {
			disconnectErr = io.EOF
		}
	}
	d.logf("与 hub %s 的连接已断开，持续 %s：%v", d.cfg.HubURL, time.Since(connectedAt).Round(time.Millisecond), disconnectErr)
	return time.Since(connectedAt), disconnectErr
}

func streamURL(hubURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(hubURL))
	if err != nil {
		return strings.TrimRight(strings.TrimSpace(hubURL), "/") + "/agent/v1/stream"
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	}
	if !strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/agent/v1/stream") {
		parsed.Path = strings.TrimRight(parsed.Path, "/") + "/agent/v1/stream"
	}
	return parsed.String()
}

func (d *Daemon) hello(ctx context.Context, session *stream.Session) error {
	hello := hub.HelloParams{
		Proto:    stream.ProtocolVersion,
		AgentID:  d.cfg.AgentID,
		Hostname: localHostname(),
		Version:  web.Version,
		Caps: []string{
			string(hub.TaskKindStatus), string(hub.TaskKindDiff), string(hub.TaskKindPush),
			string(hub.TaskKindPull), string(hub.TaskKindSSHKey), string(hub.TaskKindSecret),
			string(hub.TaskKindUpgrade), string(hub.TaskKindToolUpgrade), hub.MethodInspect,
		},
		Drift: d.cachedDrift(),
		Host:  d.cachedHost(),
		Tools: d.cachedTools(),
	}
	// Hello is time-sensitive: use only cached values here. Missing scans and
	// probes are scheduled after welcome so a newly issued agent secret cannot
	// race a data-plane request still using the one-time enrollment code.
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	payload, err := session.Call(callCtx, hub.MethodHello, hello, stream.WithBudget(10*time.Second))
	if err != nil {
		return err
	}
	var welcome hub.WelcomeResult
	if err := json.Unmarshal(payload, &welcome); err != nil {
		return fmt.Errorf("decode hello welcome: %w", err)
	}
	if welcome.Proto != stream.ProtocolVersion {
		return &stream.Error{Code: stream.CodeUnsupported, Message: "agent 协议版本不匹配，请升级 homer"}
	}
	if welcome.PingIntervalMs > 0 && welcome.PingTimeoutMs > 0 {
		session.SetHeartbeat(time.Duration(welcome.PingIntervalMs)*time.Millisecond, time.Duration(welcome.PingTimeoutMs)*time.Millisecond)
	}
	if welcome.AgentSecret != "" {
		d.setAgentSecret(welcome.AgentSecret)
		d.syncExecutorCredential()
	} else if d.hasEnrollCode() {
		return &stream.Error{Code: stream.CodeUnauthorized, Message: "接入码已兑换但 hub 未返回 agentSecret，请重新生成接入码"}
	}
	if err := d.saveAgentConfig(); err != nil {
		if welcome.AgentSecret != "" {
			// The one-time code is consumed. Keep running with the in-memory
			// secret, but make persistence failure conspicuous.
			d.logf("严重：welcome 返回的 agentSecret 未能落盘 agent.json：%v；当前进程将继续运行", err)
		} else {
			d.logf("严重：welcome 后无法持久化 agent.json 配置：%v；当前进程将继续运行", err)
		}
	}
	d.refreshHeartbeatCaches(ctx)
	return nil
}

func closeCodeForHello(err error) stream.CloseCode {
	var remote *stream.Error
	if errors.As(err, &remote) && remote.Code == stream.CodeUnsupported {
		return stream.CloseProtocol
	}
	return stream.CloseNormal
}

func closeReasonForHello(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (d *Daemon) retryFor(err error, attempt int) retryPlan {
	if err == nil {
		err = io.EOF
	}
	normalDelay := d.cfg.Backoff.Next(attempt, randomFloat64)
	plan := retryPlan{delay: normalDelay, key: "ws-dial-failed", line: "WebSocket 拨号/网络失败（退避重试中）: " + err.Error()}
	var dialErr *wsconn.DialError
	if errors.As(err, &dialErr) {
		switch dialErr.Status {
		case http.StatusUnauthorized:
			return retryPlan{delay: 5 * time.Minute, key: "ws-unauthorized", line: "WebSocket 401：凭证无效/接入码已用:请到控制台重新生成接入码（5 分钟后重试）"}
		case http.StatusGone:
			return retryPlan{delay: 5 * time.Minute, key: "ws-protocol-removed", line: "WebSocket 410：" + removedProtocolMessage(dialErr.Body) + "（5 分钟后重试）"}
		case http.StatusBadRequest:
			if strings.Contains(strings.ToLower(dialErr.Body), "unsupported-version") {
				return retryPlan{delay: 5 * time.Minute, key: "ws-unsupported-version", line: "hub 不支持当前 agent 协议版本，请升级 homer（5 分钟后重试）"}
			}
		}
		return plan
	}
	var remote *stream.Error
	if errors.As(err, &remote) {
		switch remote.Code {
		case stream.CodeUnsupported:
			return retryPlan{delay: 5 * time.Minute, key: "ws-unsupported-version", line: "hub hello 返回 unsupported-version，请升级 homer（5 分钟后重试）"}
		case stream.CodeUnauthorized:
			message := remote.Message
			if message == "" {
				message = "凭证无效/接入码已用:请到控制台重新生成"
			}
			return retryPlan{delay: 5 * time.Minute, key: "ws-unauthorized", line: "hub hello 鉴权失败：" + message + "（5 分钟后重试）"}
		}
	}
	var closeErr *stream.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
		case stream.CloseSuperseded:
			return retryPlan{delay: 30 * time.Second, key: "ws-superseded", line: "agent 被顶替：同 id 的另一进程已接管；至少 30 秒后重试", always: true}
		case stream.CloseRevoked:
			return retryPlan{delay: 5 * time.Minute, key: "ws-revoked", line: "agentSecret 已吊销，请重新接入（5 分钟后重试）"}
		case stream.CloseRemoved:
			return retryPlan{delay: 5 * time.Minute, key: "ws-removed", line: "agent 已从控制台移除，请重新接入（5 分钟后重试）"}
		case stream.CloseProtocol:
			if strings.Contains(strings.ToLower(closeErr.Reason), "unsupported-version") || strings.Contains(closeErr.Reason, "协议版本") {
				return retryPlan{delay: 5 * time.Minute, key: "ws-unsupported-version", line: "hub hello 返回 unsupported-version，请升级 homer（5 分钟后重试）"}
			}
		case stream.CloseGoingAway, stream.CloseHeartbeat:
			return retryPlan{delay: normalDelay, key: "ws-reconnect", line: fmt.Sprintf("WebSocket 断开（close %d %s），常规退避后重连", closeErr.Code, closeErr.Reason), always: true}
		}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return retryPlan{delay: normalDelay, key: "ws-reconnect", line: "WebSocket EOF/网络断开，常规退避后重连", always: true}
	}
	return plan
}

func removedProtocolMessage(body string) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &payload) == nil && strings.TrimSpace(payload.Error.Message) != "" {
		return strings.TrimSpace(payload.Error.Message)
	}
	if strings.TrimSpace(body) != "" {
		return strings.TrimSpace(body)
	}
	return "hub 协议已移除，当前 agent 版本过旧"
}

func randomFloat64() float64 {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return 0.5
	}
	return float64(binary.BigEndian.Uint64(raw[:])>>11) / float64(uint64(1)<<53)
}

// sleepContext is used only after Run has emitted a throttled retry log. The
// wait is context-cancellable so shutdown never has to wait out a backoff.
func sleepContext(ctx context.Context, duration time.Duration) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	if duration <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func localHostname() string {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return "localhost"
	}
	return hostname
}
