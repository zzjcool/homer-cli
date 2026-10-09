package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/keyring"
	"github.com/zzjcool/homer-cli/internal/sshkey"
	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/web"
)

// registerHandlers installs every task method before Session.Run starts. Each
// inbound stream request has its own task ID and can safely execute in parallel
// when taskclass permits it.
func (d *Daemon) registerHandlers(session *stream.Session) {
	for _, method := range []string{
		string(hub.TaskKindStatus), string(hub.TaskKindDiff), string(hub.TaskKindPush),
		string(hub.TaskKindPull), string(hub.TaskKindSSHKey), string(hub.TaskKindSecret),
		string(hub.TaskKindUpgrade), string(hub.TaskKindToolUpgrade), hub.MethodInspect,
	} {
		method := method
		session.Handle(method, func(ctx context.Context, req *stream.Request) (any, error) {
			result, err := d.executeTask(ctx, req)
			if method == string(hub.TaskKindUpgrade) && err == nil {
				if report, ok := result.(commands.UpgradeReport); ok && report.OK {
					d.requestReexec(req.Session)
				}
			}
			return result, err
		})
	}
}

func (d *Daemon) executeTask(parent context.Context, req *stream.Request) (any, error) {
	if req == nil {
		return nil, &stream.Error{Code: stream.CodeBadRequest, Message: "task request is missing"}
	}
	var options hub.TaskOptions
	if err := req.Decode(&options); err != nil {
		return nil, &stream.Error{Code: stream.CodeBadRequest, Message: err.Error()}
	}
	if req.Method == string(hub.TaskKindSecret) && options.SecretAction == "" {
		var command keyring.Command
		if json.Unmarshal(options.SecretPayload, &command) == nil {
			options.SecretAction = command.Action
		}
	}
	class := classifyTask(req.Method, options)
	ctx, cancel := context.WithTimeout(parent, taskLimit(d.cfg.TaskTimeout, req.Method, req.Budget))
	defer cancel()

	var release func()
	var onComplete func()
	if class == taskClassRead {
		select {
		case d.readSem <- struct{}{}:
			onComplete = func() { <-d.readSem }
		case <-ctx.Done():
			return nil, taskContextError(ctx.Err())
		}
	} else {
		var err error
		release, _, err = d.writeGate.acquireWithGrant(ctx, func() {
			req.Progress(map[string]string{"stage": "queued"})
		}, d.beginWriteGen)
		if err != nil {
			return nil, taskContextError(err)
		}
	}

	if class == taskClassWrite {
		onComplete = func() {
			// The command layer may keep running after a canceled request. Keep
			// the FIFO write gate until that goroutine actually finishes, then
			// invalidate scans and the drift cache for the next heartbeat.
			d.bumpWriteGen()
			d.forgetDrift()
			d.signalHeartbeat()
			release()
		}
	}
	return d.runTask(ctx, req, options, onComplete)
}

func taskLimit(configured time.Duration, method string, requestBudget time.Duration) time.Duration {
	if configured <= 0 {
		configured = 50 * time.Second
	}
	minimum := time.Duration(0)
	switch method {
	case string(hub.TaskKindUpgrade):
		minimum = 3 * time.Minute
	case string(hub.TaskKindPull):
		minimum = 12 * time.Minute
	case string(hub.TaskKindToolUpgrade):
		minimum = ToolUpgradeBudget
	}
	limit := configured
	if limit < minimum {
		limit = minimum
	}
	if requestBudget > 0 && requestBudget < limit {
		limit = requestBudget
	}
	return limit
}

func taskContextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &stream.Error{Code: stream.CodeTimeout, Message: "task execution timeout: " + err.Error()}
	}
	return &stream.Error{Code: stream.CodeCanceled, Message: err.Error()}
}

type taskOutcome struct {
	result any
	err    error
}

func (d *Daemon) runTask(ctx context.Context, req *stream.Request, options hub.TaskOptions, onComplete func()) (any, error) {
	completed := make(chan taskOutcome, 1)
	go func() {
		outcome := taskOutcome{}
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					outcome.result = nil
					outcome.err = &stream.Error{Code: stream.CodeInternal, Message: fmt.Sprint(recovered)}
				}
			}()
			outcome.result, outcome.err = d.runTaskCommand(ctx, req, options)
		}()
		if onComplete != nil {
			onComplete()
		}
		completed <- outcome
	}()
	select {
	case outcome := <-completed:
		if ctx.Err() != nil {
			return nil, taskContextError(ctx.Err())
		}
		if outcome.err != nil {
			return nil, outcome.err
		}
		return outcome.result, nil
	case <-ctx.Done():
		// Most command execution is not context-aware. Return promptly and let
		// its goroutine finish naturally; cancellation does not roll back a
		// write that the command layer already started.
		return nil, taskContextError(ctx.Err())
	}
}

func (d *Daemon) runTaskCommand(ctx context.Context, req *stream.Request, options hub.TaskOptions) (any, error) {
	if options.Resolve == "local" || options.Resolve == "center" {
		return d.executorResolve(ctx, options.Resolve)
	}
	switch req.Method {
	case string(hub.TaskKindStatus):
		return d.statusReport(ctx)
	case hub.MethodInspect:
		var params web.InspectParams
		if err := req.Decode(&params); err != nil {
			return nil, &stream.Error{Code: stream.CodeBadRequest, Message: err.Error()}
		}
		return d.inspect(ctx, params, req.Progress)
	case string(hub.TaskKindDiff):
		text, err := d.executorDiff(ctx, web.DiffParams{Adapter: options.Adapter, Category: options.Category, Path: options.Path})
		if err != nil {
			return nil, err
		}
		return struct {
			Text string `json:"text"`
		}{Text: text}, nil
	case string(hub.TaskKindPush):
		return d.executorPush(ctx, options.Confirm, options.Adapters, options.Overwrite, options.AllowSecrets)
	case string(hub.TaskKindPull):
		return d.executorPull(ctx, options.Confirm, options.Adapters, options.Overwrite)
	case string(hub.TaskKindSSHKey):
		home, err := os.UserHomeDir()
		if err != nil {
			return sshkey.Report{GitHubUser: options.GitHubUser, Errors: []string{"无法确定用户主目录: " + err.Error()}}, nil
		}
		return sshkey.Install(home, options.GitHubUser, options.SSHKeys), nil
	case string(hub.TaskKindSecret):
		var command keyring.Command
		if len(options.SecretPayload) != 0 {
			if err := json.Unmarshal(options.SecretPayload, &command); err != nil {
				return nil, &stream.Error{Code: stream.CodeBadRequest, Message: "密钥请求不是合法 JSON"}
			}
		}
		if command.Action == "" {
			command.Action = options.SecretAction
		}
		return keyring.Apply(d.cfg.HomerHome, command), nil
	case string(hub.TaskKindUpgrade):
		return d.runUpgrade(ctx), nil
	case string(hub.TaskKindToolUpgrade):
		return d.runToolUpgrade(ctx, options.Tool), nil
	default:
		return nil, &stream.Error{Code: stream.CodeUnknownMethod, Message: "unknown task method: " + req.Method}
	}
}

// runUpgrade downloads the hub binary from the configured data plane using the
// agent secret, then atomically replaces the running executable. Keeping this
// inside agentd lets DataURL be honored without changing the frozen command or
// Executor APIs.
func (d *Daemon) runUpgrade(ctx context.Context) commands.UpgradeReport {
	dataURL := strings.TrimRight(strings.TrimSpace(d.cfg.DataURL), "/")
	if dataURL == "" {
		dataURL = strings.TrimRight(strings.TrimSpace(d.cfg.HubURL), "/")
	}
	if dataURL == "" {
		return commands.UpgradeReport{OK: false, Status: "error", Note: "没有 hub 地址：请使用 --hub <url>"}
	}
	credential := d.bearerCredential()
	if credential == "" {
		return commands.UpgradeReport{OK: false, Status: "no-credential", Note: "没有 agent secret 或 hub token，无法下载 homer"}
	}
	self, err := os.Executable()
	if err != nil {
		return commands.UpgradeReport{OK: false, Status: "error", Note: "定位当前二进制失败: " + err.Error()}
	}
	self, _ = filepath.EvalSymlinks(self)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, dataURL+"/dl/homer", nil)
	if err != nil {
		return commands.UpgradeReport{OK: false, Status: "error", Note: err.Error()}
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	response, err := (&http.Client{Timeout: 5 * time.Minute}).Do(request)
	if err != nil {
		return commands.UpgradeReport{OK: false, Status: "error", Note: "下载失败: " + err.Error()}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return commands.UpgradeReport{OK: false, Status: "error", Note: fmt.Sprintf("下载失败: HTTP %d %s", response.StatusCode, strings.TrimSpace(string(body)))}
	}
	tmp := self + ".upgrade"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return commands.UpgradeReport{OK: false, Status: "error", Note: "写临时文件失败: " + err.Error()}
	}
	hasher := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(file, hasher), response.Body)
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return commands.UpgradeReport{OK: false, Status: "error", Note: "下载中断: " + copyErr.Error()}
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return commands.UpgradeReport{OK: false, Status: "error", Note: "写临时文件失败: " + closeErr.Error()}
	}
	if err := os.Rename(tmp, self); err != nil {
		_ = os.Remove(tmp)
		return commands.UpgradeReport{OK: false, Status: "error", Note: "替换二进制失败: " + err.Error()}
	}
	hash := hex.EncodeToString(hasher.Sum(nil))
	return commands.UpgradeReport{OK: true, Status: "upgraded", FromHub: dataURL, Binary: self, SizeBytes: size, Hash: hash}
}

const reexecInflightWait = 10 * time.Second

func (d *Daemon) requestReexec(session *stream.Session) {
	if d == nil || session == nil || d.reexec == nil {
		return
	}
	d.reexecMu.Lock()
	if d.reexecPending {
		d.reexecMu.Unlock()
		return
	}
	d.reexecPending = true
	d.reexecMu.Unlock()
	go func() {
		defer func() {
			d.reexecMu.Lock()
			d.reexecPending = false
			d.reexecMu.Unlock()
		}()
		// stream.Handler enqueues the result before removing the inbound call.
		// Wait for that transition so Flush cannot run before this upgrade
		// result has entered the send queue, but do not let unrelated incoming
		// reads postpone reexec forever.
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.NewTimer(reexecInflightWait)
		defer deadline.Stop()
		inflightTimedOut := false
		for session.Stats().InflightIn > 0 {
			select {
			case <-session.Done():
				return
			case <-deadline.C:
				inflightTimedOut = true
			case <-ticker.C:
			}
			if inflightTimedOut {
				logger := d.logger
				if logger != nil {
					logger.Printf("agent upgrade: reexec wait for in-flight requests timed out after %s; flushing and restarting", reexecInflightWait)
				}
				break
			}
		}
		flushCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = session.Flush(flushCtx)
		cancel()
		d.reexec()
	}()
}
