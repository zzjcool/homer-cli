# Homer agent↔hub 传输层重构:WebSocket 多路复用 Stream

**日期**:2026-10-08 · **基线**:master@dd29054(打 tag `pre-ws-master`)· **状态**:待执行
**取代**:`docs/plan/2026-09-27-hub-plan.md` 的「不做 WebSocket / 长轮询」决策。用户已明确推翻。旧文档顶部加的横幅文本见 B-P5。

## 0. 目标与不做的事

**目标**:把 agent↔hub 传输层统一成「WebSocket 上的多路复用 Stream」。列出的 4 个实测瓶颈全部解决:
- 单槽串行加固定 sleep 2s。
- 一次弹窗 4 个机器任务。
- 每个 status 全量下载 447KB 快照。
- 每个适配器一次登录 shell PATH 探测。

**Non-goals**:
- 不保留 listen、长轮询(poll/report/register/enroll HTTP)、降级和兼容 flag。
- 不动 `commands/core/keyring/agecrypto`、`Executor` 接口语义、`EnrollmentManager` 的语义、`/api/snapshot` 数据面形状(仅加 ETag)、web console 路由和 `AgentsSource` 签名。
- 不做 TLS 终结,不做多 hub,不做 Registry 持久化。
- 不做 gRPC 实现,只保证 `Conn` 接口可插拔。
- 不处理浏览器→CF→hub 的 HTTP 100s(524)长请求问题(见 F-3)。

**已自行拍板的决策**(理由在括号里):

| 项 | 决策 |
|---|---|
| WS 库 | **coder/websocket**(固定在 ≥v1.8.12,go.mod 的 `go` 行必须保持 1.23.4)。理由:context 原生,Read/Write/Ping 都吃 ctx。`Write` 并发安全。零传递依赖。`SetReadLimit`、`Close(code,reason)`、`CloseNow` 齐全。Hijack 后自管 deadline。gorilla 需手工 `SetReadDeadline`/`WriteControl`,无 ctx,单写者约束要自己包,2022 年前后曾归档,后来恢复。两者体积都小,没有决定性差别。 |
| 保活 | **应用层 `ping`/`pong` 帧**(25s/75s),不依赖 WS 协议 ping。理由:传输无关,gRPC 也能用。数据帧一定能重置 CF/代理的空闲计时器。 |
| agent 本地 web UI | **删除**。agent 不再监听任何端口。理由:消除入站端口和第二套鉴权面,同时删除 `LocalResolve/LocalUpgrade/LocalToolUpgrade/AfterLocalWrite/SyncDeps/Identity` 钩子。本机要 UI 就跑 `homer serve`(loopback)。 |
| agent CLI | `homer agent --hub <url> [--data-url <url>] [--token] [--home] [--id]`。`--connect/--listen/--advertise` 直接删除,不留别名。agent.json 字段:`agentId, hubUrl, dataUrl, agentSecret`。`mode` 字段删除。 |
| 同机绕 tunnel | 显式配置 `--data-url`(也可用 env `HOMER_DATA_URL` 或 agent.json 的 `dataUrl`),只用于 `/api/snapshot` 和 `/dl/*`,默认等于 hub URL。不做自动探测(见 F-2)。控制面 WS 仍走 `--hub`。 |
| 旧协议端点 | `/agent/v1/{enroll,register,poll,report}` 回 **410** 和 JSON `{"error":{"code":"protocol-removed","message":"agent 版本过旧…"}}`(墓碑,无鉴权,无数据)。这样旧 agent 的重试日志能说清原因,不会静默。 |
| precheck | 合并进新方法 `collect.inspect`,一次机器任务返回 status、凭证存在性、密钥扫描命中、keys list。删除 `/api/sync/precheck` 路由。 |
| 弹窗增量 | `/api/sync/choices?stream=1` 返回 NDJSON,前端 `fetch` 流式读取。不带 `stream` 时返回和现在一致的整包 JSON。 |
| 快照缓存 | `/api/snapshot` 加 `ETag: "g<generation>"`,agent 带 `If-None-Match`,命中返回 304。agent 内存缓存解码后的快照。 |
| PATH 缓存 | `shellenv` 加 30s TTL、singleflight,并提供 `Invalidate()`。 |
| 同 agentId 重复连接 | 新连接顶掉旧连接。旧连接收 close 4001,其在途调用立刻失败。 |

---

## A. 冻结的接口

> 以下签名 worker 不得自行改动。需要变更时回报 orchestrator,走「接口变更单」。

### A1. `internal/stream`(传输无关,纯库)

```go
package stream

const (
	ProtocolVersion = 1
	Subprotocol     = "homer.stream.v1"
)

// ---- Conn: the transport-independent "Stream": bidirectional, ordered, message-framed. ----
// Contract:
//  - Read returns exactly one whole text message. A peer close is *CloseError.
//    Binary/oversize/invalid-UTF8 from the peer => *ProtocolError (session closes 1003/1009/1002).
//  - Write is safe for concurrent use and honours ctx deadline/cancel.
//  - Close sends a close frame and waits briefly; CloseNow drops the transport at once.
type Conn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, msg []byte) error
	Close(code CloseCode, reason string) error
	CloseNow() error
	Info() ConnInfo
}
type ConnInfo struct {
	Transport   string // "ws" | "pipe" | future "grpc"
	Remote      string
	Subprotocol string
	MaxMessage  int64 // read limit enforced by the transport
}

type CloseCode int

const (
	CloseNormal      CloseCode = 1000
	CloseGoingAway   CloseCode = 1001 // hub restarting / agent exiting
	CloseProtocol    CloseCode = 1002 // malformed JSON, missing "t", bad version
	CloseUnsupported CloseCode = 1003 // binary message
	ClosePolicy      CloseCode = 1008 // hello timeout, frame before hello, slow consumer
	CloseTooBig      CloseCode = 1009
	CloseInternal    CloseCode = 1011
	CloseHeartbeat   CloseCode = 4000 // ping timeout (local decision, sent best-effort)
	CloseSuperseded  CloseCode = 4001 // same agentId connected elsewhere
	CloseRevoked     CloseCode = 4401 // secret revoked / auth failed after upgrade
	CloseRemoved     CloseCode = 4403 // console "移除"
)

type CloseError struct {
	Code   CloseCode
	Reason string
	Remote bool // true: the peer closed
}
type ProtocolError struct{ Code CloseCode; Msg string }

// ---- Frames ----
type FrameType string

const (
	TReq    FrameType = "req"
	TRes    FrameType = "res"
	TProg   FrameType = "prog"
	TCancel FrameType = "cancel"
	TEvt    FrameType = "evt"
	TPing   FrameType = "ping"
	TPong   FrameType = "pong"
)

type Frame struct {
	T   FrameType       `json:"t"`
	ID  string          `json:"id,omitempty"`  // req/res/prog/cancel; ping/pong echo a counter
	M   string          `json:"m,omitempty"`   // req/evt method
	P   json.RawMessage `json:"p,omitempty"`   // params / result / progress payload
	OK  bool            `json:"ok,omitempty"`  // res only; absent => failure, E must be set
	E   *Error          `json:"e,omitempty"`
	DL  int64           `json:"dl,omitempty"`  // req only: remaining budget in ms (relative, no clock sync)
	Seq uint64          `json:"seq,omitempty"` // prog only: per-call increasing
}

func EncodeFrame(f *Frame, maxFrame int) ([]byte, error) // ErrFrameTooLarge if > maxFrame
func DecodeFrame(b []byte, maxFrame int) (*Frame, error) // *ProtocolError on bad JSON / empty t / oversize
// Unknown-but-well-formed "t" values decode fine; the session drops them (throttled log).
// Unknown JSON fields are ignored (forward compatible).

// ---- Errors ----
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"msg,omitempty"`
	Retryable bool   `json:"retry,omitempty"`
}

func (e *Error) Error() string

const (
	CodeUnknownMethod = "unknown-method"
	CodeBadRequest    = "bad-request"
	CodeDuplicateID   = "duplicate-id"
	CodeOverloaded    = "overloaded"
	CodeCanceled      = "canceled"
	CodeTimeout       = "timeout"
	CodeInternal      = "internal"
	CodeFrameTooLarge = "frame-too-large"
	CodeExecFailed    = "exec-failed" // handler returned a plain error
	CodeUnauthorized  = "unauthorized"
	CodeUnsupported   = "unsupported-version"
)

var ErrFrameTooLarge = errors.New("stream: frame exceeds max size")

// SessionClosedError is returned by Call/Notify when the session ends (also for in-flight calls: immediately).
type SessionClosedError struct{ Cause error } // Cause: *CloseError | io error | nil
func (e *SessionClosedError) Error() string
func (e *SessionClosedError) Unwrap() error

// ---- Session: multiplexing, request/response correlation, heartbeat, cancel ----
type Logger interface{ Printf(format string, args ...any) }

type Options struct {
	Logger        Logger
	MaxFrame      int           // default 8<<20
	PingInterval  time.Duration // default 25s
	PingTimeout   time.Duration // default 75s: no inbound frame for this long => close 4000
	WriteTimeout  time.Duration // default 15s per frame; a stalled writer closes 1008 "slow-consumer"
	SendQueue     int           // default 256 (res/cancel/evt/ping); progress has its own droppable ring of 64
	MaxInflightIn int           // default 128 concurrent inbound requests; excess => overloaded
	Clock         Clock         // nil => real clock (tests inject fake)
}
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
	NewTicker(d time.Duration) Ticker
}

type Handler func(ctx context.Context, req *Request) (result any, err error)
type EventHandler func(ctx context.Context, method string, params json.RawMessage)

type Request struct {
	ID      string
	Method  string
	Params  json.RawMessage
	Budget  time.Duration // from frame.dl; 0 = none (ctx already carries the deadline)
	Session *Session
}
func (r *Request) Decode(v any) error
// Progress is non-blocking and droppable under backpressure; it never fails the handler.
func (r *Request) Progress(v any)

type CallOption func(*callOpts)

func WithBudget(d time.Duration) CallOption               // sets frame.dl
func WithProgress(fn func(json.RawMessage)) CallOption   // runs on the Call goroutine, in order, never blocks the read loop

func NewSession(conn Conn, opts Options) *Session
func (s *Session) Handle(method string, h Handler)       // before Run; panics after
func (s *Session) OnEvent(method string, h EventHandler) // before Run
func (s *Session) Run(ctx context.Context) error         // blocks: read loop + ping loop + writer. Returns *CloseError / nil on local Close
// A handler error that is *stream.Error is sent as-is; any other error => {code:"exec-failed",msg:err.Error()}.
// A handler panic => {code:"internal"} and the session survives.
func (s *Session) Call(ctx context.Context, method string, params any, opts ...CallOption) (json.RawMessage, error)
// ctx done => sends best-effort cancel and returns ctx.Err(); remote errors => *Error; session ended => *SessionClosedError.
func (s *Session) Notify(ctx context.Context, method string, params any) error // evt, no reply
func (s *Session) SetHeartbeat(interval, timeout time.Duration)                  // adopt the hub's welcome values
func (s *Session) Flush(ctx context.Context) error                               // wait until the send queue drains (used before reexec)
func (s *Session) Close(code CloseCode, reason string)                           // idempotent; fails all pending calls at once
func (s *Session) Done() <-chan struct{}
func (s *Session) Err() error
func (s *Session) Info() ConnInfo
func (s *Session) Stats() Stats // FramesIn/Out, ProgressDropped, Pending, InflightIn, PingRTT
```

**Session 语义**(冻结,测试据此):
1. 两个方向各有独立的 id 空间。调用方持有 outgoing 表,处理方持有 incoming 表,互不冲突。
2. 每个 req 在独立 goroutine 里跑,ctx 由三者取消:对端 `cancel` 帧、dl 到期、session 结束。
3. **cancel 与完成竞争**:每个调用的 `done` 用 CAS 保证只发一个终态。完成后收到 cancel 则忽略。cancel 先到则丢弃 handler 结果,**不回 res**。
4. **重复 id**:in-flight 的 id 再来 req,新的那条回 `duplicate-id`,原有的继续跑。完成后的 id 可复用。
5. 未知 method 回 `unknown-method`。未知帧类型丢弃并节流记日志。畸形 JSON、缺 `t`、超大帧会关连接,分别用 1002、1002、1009。二进制帧关 1003。
6. **背压**:progress 走可丢环形队列,满了丢最旧并计数。res/cancel/evt/ping 走有界队列。入队阻塞超过 `WriteTimeout` 则关 1008 `slow-consumer`。ping/pong 优先于其他帧出队。
7. **心跳**:双方每 `PingInterval` 发 ping。任何入站帧都刷新 `lastRecv`。`now-lastRecv > PingTimeout` 则关 4000。长任务期间 ping 不受影响(handler 在独立 goroutine,writer 单独调度)。
8. **session 结束**:所有 pending Call 立即以 `*SessionClosedError` 失败,所有 handler ctx 被取消。
9. Call 的 `Params` 编码后超过 `MaxFrame` 时,Call 同步返回 `ErrFrameTooLarge`,不发送。handler 结果过大时回 `frame-too-large` 错误帧。

```go
// stream/backoff.go
type Backoff struct{ Base, Max time.Duration; Factor, Jitter float64 } // 1s,60s,2,0.5
func (b Backoff) Next(attempt int, rnd func() float64) time.Duration  // d=min(Max,Base*Factor^attempt); return d*(1-Jitter*rnd())
// stream/throttle.go
func NewThrottledLogger(l Logger, window time.Duration) *ThrottledLogger
func (t *ThrottledLogger) Log(key, line string) bool // true=emitted. 同一 key 每 window 至多一行
```

### A2. `internal/stream/wsconn`(第一个传输实现)

```go
package wsconn

type DialOptions struct {
	Header      http.Header   // Authorization etc.
	HTTPClient  *http.Client  // nil => client with ProxyFromEnvironment, dial timeout 15s
	MaxMessage  int64         // read limit, default 8<<20 + 4096
}
func Dial(ctx context.Context, url string, o DialOptions) (stream.Conn, *http.Response, error)
// url accepts ws/wss/http/https (http->ws, https->wss). Subprotocol offered: stream.Subprotocol. Compression disabled.
// A non-101 response is returned as *DialError{Status int; Body string} (body ≤4KB) so callers can tell 401 from 410 from 5xx.

type AcceptOptions struct{ MaxMessage int64 }
func Accept(w http.ResponseWriter, r *http.Request, o AcceptOptions) (stream.Conn, error)
// Origin check stays at the library default (agents send no Origin; auth is a Bearer header, so CSRF does not apply).
// The ResponseWriter chain must implement http.Hijacker (tested through web.Server middleware).
```

`internal/stream/streamtest`:
- `Pipe(opts) (a, b stream.Conn)`:内存双向连接,可注入延迟、丢包、半开(只吞不回)、乱序和限速读。
- `FaultConn` 包装器。
- `TCPProxy`(真 socket 故障代理):`Cut()` 立即 RST,`Blackhole()` 半开,`Delay(d)`,`CloseGraceful()`。
- `FakeClock`。

### A3. 线格式(JSON 文本帧,一帧一条 WS text message)

```jsonc
// hub→agent 请求(dl = 剩余预算毫秒)
{"t":"req","id":"t-000123","m":"status","p":{},"dl":55000}
{"t":"req","id":"t-000124","m":"collect.inspect","p":{"adapters":null,"credentials":["~/.pi/agent/auth.json"],"wantKeys":true},"dl":55000}
// 进度(可丢)
{"t":"prog","id":"t-000124","seq":2,"p":{"stage":"adapter","done":2,"total":4,"adapter":{"id":"pi","push":3,"pull":0,"conflicts":0,"categories":[]}}}
// 成功(p 即原先的 report;报告内部 ok:false 例如 push 冲突,仍然是 res.ok=true)
{"t":"res","id":"t-000123","ok":true,"p":{"adapters":[],"errors":[]}}
// 失败
{"t":"res","id":"t-000123","e":{"code":"exec-failed","msg":"task execution timeout: context deadline exceeded"}}
{"t":"cancel","id":"t-000123"}
// agent→hub 事件(心跳),与任务通道独立
{"t":"evt","m":"hb","p":{"version":"v0.9.1","drift":{"push":1,"pull":0,"conflicts":0},"host":{},"tools":[]}}
{"t":"ping","id":"17"}   {"t":"pong","id":"17"}

// 握手:agent 第一帧必须是 hello(10s 内),其他任何帧 => close 1008
{"t":"req","id":"a-1","m":"hello","p":{"proto":1,"agentId":"hw-7735","hostname":"hw","version":"v0.9.1",
   "caps":["status","diff","push","pull","ssh-key","secret","upgrade","tool-upgrade","collect.inspect"],
   "drift":null,"host":null,"tools":null}}
{"t":"res","id":"a-1","ok":true,"p":{"proto":1,"hubVersion":"v0.9.1","instanceId":"3f9c…",
   "agentSecret":"<仅 enroll 码兑换那次返回>","pingIntervalMs":25000,"pingTimeoutMs":75000,"maxFrame":8388608}}
```

**版本协商**:
- 升级请求带 `Sec-WebSocket-Protocol: homer.stream.v1`。hub 不认就在升级前回 HTTP 400 `unsupported-version`。
- `hello.proto` 与 hub 不一致时,回 `unsupported-version`,消息「agent 版本过旧/过新,请 homer upgrade 或重新安装」,随后 close 1002。
- `caps` 缺失的方法,hub 调用时直接回 `unsupported`(不下发)。
- 以后新增字段只加不改。破坏性变更升 `v2` 子协议。

### A4. hub 侧(`internal/hub`)

```go
// proto.go —— 方法名即 TaskKind,hub(调用方)与 agentd(处理方)共用
const (
	MethodHello     = "hello"
	MethodHeartbeat = "hb" // evt
	MethodInspect   = "collect.inspect"
	// 其余 = TaskKind 常量: status diff push pull ssh-key secret upgrade tool-upgrade
)
type HelloParams struct {
	Proto    int                `json:"proto"`
	AgentID  string             `json:"agentId"`
	Hostname string             `json:"hostname"`
	Version  string             `json:"version"`
	Caps     []string           `json:"caps"`
	Drift    *AgentDrift        `json:"drift,omitempty"`
	Host     *HostSnapshot      `json:"host,omitempty"`
	Tools    *[]toolctl.Status  `json:"tools,omitempty"` // 指针语义保持:缺省=未上报,空表=没装
}
type WelcomeResult struct {
	Proto          int    `json:"proto"`
	HubVersion     string `json:"hubVersion"`
	InstanceID     string `json:"instanceId"`
	AgentSecret    string `json:"agentSecret,omitempty"`
	PingIntervalMs int    `json:"pingIntervalMs"`
	PingTimeoutMs  int    `json:"pingTimeoutMs"`
	MaxFrame       int    `json:"maxFrame"`
}
type HeartbeatParams struct {
	Version string            `json:"version,omitempty"`
	Drift   *AgentDrift       `json:"drift,omitempty"`
	Host    *HostSnapshot     `json:"host,omitempty"`
	Tools   *[]toolctl.Status `json:"tools,omitempty"`
}

// task.go 保留: TaskKind + 常量, TaskOptions(原样,作为各 TaskKind 方法的 params), PullWait/UpgradeWait/ToolUpgradeWait。
// 删除: AgentMode*, Task, TaskResult, TaskQueueCapacity, TaskTTL, TaskLifetime。新增:
const (
	CallBudgetDefault = 60 * time.Second // = 原 dispatcherTimeout
	ReqDeadlineSlack  = 5 * time.Second  // frame.dl = budget - slack,agent 先于 hub 超时并带着消息返回
)
func CallBudget(kind TaskKind) time.Duration // Pull 12m, Upgrade 3m, ToolUpgrade 7m, 其余 60s

// registry.go: AgentInfo 删 Mode/Addr/DialSecret;保留其余字段。Registry 删队列/任务表。
func NewRegistry() *Registry
func (r *Registry) Attach(info AgentInfo, sess *stream.Session) (prev *stream.Session)
func (r *Registry) Detach(agentID string, sess *stream.Session) // 仅当 sess 仍是当前会话才清
func (r *Registry) Session(agentID string) (*stream.Session, bool)
func (r *Registry) Touch/UpdateDrift/UpdateHost/UpdateTools/UpdateVersion/NoteWriteOutcome/NoteToolVersion/Get/List/Remove // 签名不变
// Stale = 无活会话 || now-LastSeen >= AgentStaleAfter(90s)。每个入站帧刷新 LastSeen。

// auth.go
type PrincipalKind int
const (PrincipalSecret PrincipalKind = iota + 1; PrincipalHubToken; PrincipalEnrollCode)
type Principal struct{ Kind PrincipalKind; AgentID string /* 仅 Secret */ ; Code string /* 仅 EnrollCode */ }
type Authenticator struct{ Token string; Enrollment *EnrollmentManager }
func (a *Authenticator) Authenticate(bearer string) (Principal, bool) // 常量时间比较 hub token;enroll 码只 ValidCode,不烧
func (a *Authenticator) Authorized(r *http.Request) bool            // secret || hub token(web /api,/dl 闸口用,不含 enroll 码)

// agenthub.go
type HubOptions struct {
	Logger stream.Logger; Session stream.Options
	HelloTimeout time.Duration // 10s
	Version string
}
func NewAgentHub(reg *Registry, auth *Authenticator, enr *EnrollmentManager, o HubOptions) *AgentHub
func (h *AgentHub) ServeHTTP(w http.ResponseWriter, r *http.Request) // GET /agent/v1/stream;/agent/v1/{enroll,register,poll,report} => 410 protocol-removed;其它 /agent/ => 404
func (h *AgentHub) Kick(agentID string, code stream.CloseCode, reason string) bool
func (h *AgentHub) Shutdown(ctx context.Context) error // 全部 close 1001 "hub restarting"

// enrollment.go 新增 + 修复
func (m *EnrollmentManager) SetRevokeHook(fn func(agentID string)) // Revoke 成功后(锁外)调用
// 修复: VerifySecret 读 m.secrets 未加锁(-race 会报),必须加锁。

// dispatcher.go: 对外签名全部不变(web.AgentsSource + AgentKey/AgentUpgrade/AgentToolUpgrade/AgentInstallSSHKeys/AgentResolve/RemoveAgent)
type Dispatcher struct{ Registry *Registry; Token string; Hub *AgentHub /* 可 nil; RemoveAgent 时 Kick(4403) */ }
func NewDispatcher(reg *Registry, token string) *Dispatcher // 签名不变
func (d *Dispatcher) AgentInspect(ctx context.Context, agentID string, p web.InspectParams, onEvent func(web.InspectEvent)) (web.InspectResult, error) // 新增,实现 web.InspectSource
```

**错误映射**(dispatcher → `web.AgentError`):

| 来源 | code | HTTP |
|---|---|---|
| 无会话、`SessionClosedError`(含 superseded/revoked/removed/shutdown/heartbeat) | `agent-offline` | 503,message 带原因 |
| `ctx.DeadlineExceeded` / 调用方 cancel | `agent-timeout` | 504 |
| 远端 `*Error{timeout}` | `agent-timeout` | 504 |
| 远端 `exec-failed` / `internal` / `unknown-method` / `unsupported` / `overloaded` | `agent-unreachable` | 502,message 透传 |
| `bad-request` | `bad-request` | 400 |
| agent 不在 Registry | `agent-not-found` | 404 |

`requireOnline` 现在统一用于所有调用,**包括 `AgentDiff`**(原先缺失,离线最坏等 60s)。每个 agent 的调用并发上限 64(超出在 ctx 内排队),超时和取消都按上表映射。

### A5. agent 侧(`internal/agentd`)

```go
type Config struct {
	HomerHome, Home, Token, AgentSecret, EnrollCode string
	HubURL  string // http(s)/ws(s),控制面
	DataURL string // 空 => HubURL;/api/snapshot 与 /dl/*
	AgentID string
	TaskTimeout time.Duration // 原 ReportTimeout,默认 50s;Upgrade 3m / Pull 12m / ToolUpgrade ToolUpgradeBudget 下限不变
	Backoff stream.Backoff    // 默认 {1s,60s,2,0.5}
	Stream  stream.Options   // 测试用:缩短 ping
}
// 删除: ListenAddr AdvertiseURL ConnectURL PollInterval PollWait, Config.Mode(), IsAgentNotFound, DeriveAdvertiseURL, discoverLanIPv4, listenAuthorized, dialSecret
func New(cfg Config, exec Executor) *Daemon // 不变
func (d *Daemon) Run(ctx context.Context) error // 不变签名;重连循环,仅 ctx 取消时返回
func (d *Daemon) SetToolkit(Toolkit)           // 不变

type AgentConfig struct { // agent.json, 0600
	AgentID string `json:"agentId"`; HubURL string `json:"hubUrl,omitempty"`; DataURL string `json:"dataUrl,omitempty"`; AgentSecret string `json:"agentSecret,omitempty"`
}
func ResolveConfig(cfg Config) (Config, error) // 显式参数 > agent.json;无 HubURL => 明确报错
```

**任务类别**(`taskclass.go`,单测覆盖,默认未知 → 写类):
- **读类**(并行,经 `readSem=4`):`status`、`diff`、`collect.inspect`,以及 `secret` 的 `list/exists/status`。
- **写类**(FIFO 互斥,等待 ctx 可取消,并发 progress `{"stage":"queued"}`):`push`、`pull`、`ssh-key`、`upgrade`、`tool-upgrade`,以及 `secret` 的其余动作。
- **singleflight**:并发 status/inspect 共享同一次扫描。写操作完成后递增 `writeGen`,不允许 join 写之前启动的扫描。扫描结果同时喂 drift 缓存。

**心跳**(`heartbeat.go`):独立 goroutine,每 30s 抖动发 `evt hb`,内容为缓存的 drift/host/tools。后台扫描完成或工具探测完成时立刻补发一次。**绝不等待扫描,绝不占任务通道。**

**重连与关闭码处理**:

| 事件 | agent 行为 | 日志 |
|---|---|---|
| 拨号失败/网络错误 | 指数退避加抖动,连续稳定 ≥30s 才重置计数 | 节流 1 次/分,key `ws-dial-failed` |
| HTTP 401 | 退避拉满 5min | 节流 key `ws-unauthorized`,写明「凭证无效/接入码已用:请到控制台重新生成」 |
| HTTP 410 | 退避拉满 5min | 节流,打印 hub 返回的 message(协议已删) |
| close 1001/EOF/4000 | 常规退避 | 每次断开都记一行(状态迁移),重试走节流 |
| close 4001 superseded | 至少 30s 后重试 | 每次都记,提示「同 id 的另一进程已接管」 |
| close 4401 revoked | 5min 退避 | 节流,提示重新接入 |
| close 4403 removed | 5min 退避 | 同上 |
| hello 返回 unsupported-version | 5min 退避 | 节流,提示升级 |

**upgrade 任务**:handler 返回结果后,`Flush(2s)` 再 `reexecAgent()`。新进程自行重连并重新 hello。

### A6. web 层变更面(`internal/web`)

```go
// inspect_types.go(冻结提交里由 P1 创建,P4c 之后接管)
type InspectParams struct {
	Adapters    []string `json:"adapters,omitempty"`
	Credentials []string `json:"credentials,omitempty"` // 要探测的目的路径(由 web 的 credentialRules 提供)
	WantKeys    bool     `json:"wantKeys,omitempty"`
}
type InspectEvent struct {
	Stage       string                         `json:"stage"` // adapter | credentials | secrets | keys | queued
	Done        int                            `json:"done,omitempty"`
	Total       int                            `json:"total,omitempty"`
	Adapter     *commands.StatusAdapterReport  `json:"adapter,omitempty"`
	Present     map[string]bool                `json:"present,omitempty"`
	Secrets     []InspectSecret                `json:"secrets,omitempty"`
	Keys        *keyring.Result                `json:"keys,omitempty"`
}
type InspectSecret struct{ Path, Description string; Line int }
type InspectResult struct {
	Status  commands.StatusReport `json:"status"`
	Present map[string]bool       `json:"present,omitempty"`
	Secrets []InspectSecret       `json:"secrets,omitempty"`
	Keys    *keyring.Result       `json:"keys,omitempty"`
	Errors  []string              `json:"errors,omitempty"` // 子步骤的非致命失败
}
type InspectSource interface {
	AgentInspect(ctx context.Context, agentID string, p InspectParams, onEvent func(InspectEvent)) (InspectResult, error)
}
```

**NDJSON**:`GET /api/sync/choices?direction=collect&agent=X&stream=1`,`Content-Type: application/x-ndjson`,每行一个 JSON:
```
{"type":"adapter","adapter":{...AdapterChoice...},"done":1,"total":4}
{"type":"precheck","credentials":{"pi":[...]},"secretHits":{"pi":[...]}}
{"type":"keys","keys":[...]}
{"type":"done","hint":"…","adapters":[...全量 AdapterChoice,含 credentials/secretHits...]}
{"type":"error","code":"agent-timeout","message":"…"}
```
不带 `stream=1` 时返回整包 JSON(同 `done` 的内容加 `ok/direction/agentId`)。`dispatch` 和 `resolve` 仍走 `AgentStatus`。

**ServeOptions 删除字段**:`Identity`、`SyncDeps`(及 `SyncDepsSource`)、`LocalResolve`、`LocalUpgrade`、`LocalToolUpgrade`、`AfterLocalWrite`。保留 `AgentEndpoint`(= `*hub.AgentHub`)和 `AgentEndpointAuthorized`(= `Authenticator.Authorized`,**仅**供 `/api/`、`/dl/` 闸口承认 per-agent secret)。

**删除的路由/函数**:
- `/api/ssh-key`、`/api/upgrade`、`/api/tools/upgrade`(agent 本地路由)。
- `/api/sync/precheck`。
- `/agent/v1/info`。
- `handleLocal*`。

**`web.AgentInfo`**:删除 `Mode`、`Addr`。`index.html:719` 的「直连」文案删除,`join-listen` DOM 和 `listenCommand` 删除。

**`/api/snapshot`**:GET 加 `ETag: "g<N>"`,处理 `If-None-Match` 返回 304(无 body)。无快照仍是 409。POST 响应不变。

### A7. 默认参数汇总

| 项 | 值 |
|---|---|
| PingInterval / PingTimeout | 25s / 75s(hub 在 welcome 里下发,agent 采用) |
| HelloTimeout | 10s |
| WriteTimeout | 15s |
| MaxFrame | 8 MiB(WS 读限制 = MaxFrame+4KiB) |
| SendQueue / Progress 环 | 256 / 64 |
| 单会话入站并发上限 | 128 |
| hub 每 agent 并发调用 | 64 |
| 调用预算 | 默认 60s、Upgrade 3m、ToolUpgrade 7m、Pull 12m |
| Backoff | 1s→60s,×2,抖动 50%,稳定 30s 重置 |
| 认证/移除类退避 | 5min |
| superseded 退避 | ≥30s |
| shellenv TTL | 30s |
| DriftInterval | 30s(抖动) |

---

## B. 阶段划分与依赖

```
P0 spike ─────► G0(CF 验证通过)
G0 ─► P1 stream 库 ─► F1(接口骨架+streamtest+可用的 happy-path Session)
P4a shellenv ──(与 P1 并行,立即开工)
F1 ─► P2 hub ║ P3 agent ║ P4c web+UI      (三个独立 worktree,并行)
P3 ─► P4b agent 数据面(executor/inspect/快照缓存)
P2+P3+P4a+P4b+P4c ─► I1 集成(orchestrator 串行合并,build 变绿)
I1 ─► P5 CLI/脚本/docker/文档 ║ P6a 集成测试(P2+P3 合并后即可提前开始)
P5+P6a ─► P6b e2e/docker/UI ─► P7 hw 性能验收与部署演练(orchestrator + 用户)
```

**纪律**:
- 每个 worker 一个独立 git worktree,分支 `ws/<phase>`,同一 worktree 只留一个 writer。
- worker 不开子 agent。
- 所有命令用 `timeout` 包裹,禁止 sudo。
- 不得触碰 `~/.homer/keys/*`、`agent.json`、`homer.json`。调试一律用 `--home /tmp/xxx`。
- 合并顺序固定:P1 → P4a → P2 → P3 → P4c → P4b → P5 → P6。每合一个都跑 E 节的 `build+vet`。
- 完成后按 AGENTS.md 向 orchestrator 回调,并附报告路径 `/tmp/ws-<phase>-report.md`。

**每张任务卡通用前言**(粘贴时放在卡片最前面):
> 你是 worker,只做本卡文件清单内的改动,不改清单外文件(发现需要改时在报告里写「接口变更请求」)。先读 `docs/plan/2026-10-08-ws-stream-plan.md` 的 A 节,签名一字不改。TDD:先写失败测试再实现。每个命令包 `timeout`。完成前必须通过本卡验收命令。同一问题重试超过 2 次就回滚到上一个检查点并在报告里记录,然后跳过。完成后执行 `herdr agent prompt orchestrator "<一句话> 报告:/tmp/ws-<phase>-report.md"`。

### P0 — CF/cloudflared WebSocket 透传验证(spike)

- **文件归属**:`tests/spike/**`(独立 `go.mod`,不进主模块的 `./...`)、报告 `docs/plan/2026-10-08-ws-spike-report.md`。
- **依赖**:无。**规模**:0.5d。
- **先读**:SOP `cloudflared-tunnel-on-this-host.md`(QUIC 被屏蔽须强制 `--protocol http2`)、`cloudflare-tunnel-hw-host.md`(sb-tun DNS 劫持、通配证书)。
- **环境隔离(不碰生产)**:
  - 首选 TryCloudflare 快速隧道:`timeout 600 cloudflared tunnel --protocol http2 --url http://127.0.0.1:17801`。
  - 备选:新建独立 tunnel 和独立 config(`--config /tmp/spike/config.yml`,新 hostname 如 `homer-spike.openaaas.org`)。**绝不编辑生产 ingress 和 systemd unit,绝不重启 cloudflared 生产服务。**
- **任务**:写 `wsecho`(coder/websocket 服务端:回显、按需推送、可配置关闭码、原样回显升级请求头)和 `wsprobe` 客户端。逐项测并记录结果:

| # | 检查项 |
|---|---|
| V1 | `Authorization` 与 `Sec-WebSocket-Protocol` 头透传,拿到 101,子协议回显正确 |
| V2 | 文本帧 1KB/1MB/4MB/8MB 往返无损 |
| V3 | 完全空闲 60/100/150/300/600s 后是否被断开,记录首次断开的时间和 close code |
| V4 | 应用层 ping 25s 保活 15min,不断 |
| V5 | WS 协议 ping/pong 是否被透传(仅记录) |
| V6 | 自定义 close code 4001/4401 及 reason 经 CF 是否原样到达 |
| V7 | 源站 `kill -9` 后客户端感知耗时 |
| V8 | 源站侧用 `streamtest.TCPProxy`(或临时 userland 代理)blackhole,验证半开靠 ping 超时发现(CF 侧无法模拟,记录即可) |
| V9 | 重启**临时** cloudflared 后客户端重连耗时 |
| V10 | 单连接 50 并发小 req/res 的 RTT p50/p95 |
| V11 | http.Server 设 `WriteTimeout=3s` 时,hijack 后连接是否活过 3s(验证 Go 清 deadline) |
| V12 | 经 Go 中间件链(包一层不实现 Hijacker 的 ResponseWriter)时 Accept 的失败表现,记录以便 P2 防护 |

- **验收**:`cd tests/spike && timeout 900 go run ./wsprobe -url wss://<host>/ws -suite all -out /tmp/spike.json`,报告给出每项 pass/fail 和数字。
- **闸口 G0**:V1、V4、V6(不通过则 close code 改走 `reason` 前缀)必须通过,否则暂停并上报。**不得回退长轮询。**
- 同时给出 CF 实测空闲上限,据此确认 25s/75s 取值,必要时调整 A7。

### P1 — stream 库(transport + session)

- **文件归属**:`internal/stream/**`(含 `wsconn`、`streamtest`)、`go.mod`、`go.sum`、新文件 `internal/hub/proto.go`(只放 A4 的常量和类型)、`internal/web/inspect_types.go`(A6 类型)。
- **依赖**:G0。**规模**:~1600 行实现 + ~1800 行测试,1.5d。
- **步骤**:
  1. `go get github.com/coder/websocket@v1.8.12`。确认 `go` 行仍是 1.23.4,若被抬高就回退到兼容版本。`go mod tidy` 后 diff 里只多这一个依赖。
  2. 先写 `Frame/Conn/errors/Options` 骨架、`streamtest.Pipe`、能跑通 happy-path 的 `Session`(Call/Handle/Progress/cancel/ping)。**到这一步打 tag `ws/f1-stream-lib` 并回调 orchestrator(F1 检查点)**,P2/P3/P4c 此后开工。
  3. 补齐背压、大帧、心跳超时、竞态、panic 防护。
  4. `wsconn` 加真 socket 测试,`TCPProxy` 加故障注入。
  5. 单测覆盖 D 节「P1 单测」全部行。
- **验收**:`timeout 600 go test -race -count=1 ./internal/stream/...`,再加 `go vet ./internal/stream/...`。`-race -count=20` 跑 session 并发相关用例,要稳定。

### P2 — hub 侧重写

- **文件归属**:`internal/hub/**`(`agenthub.go`、`auth.go` 新增;`registry.go`、`dispatcher.go`、`task.go`、`enrollment.go` 改;`agentapi.go` 及其测试删除;全部 `*_test.go` 重写)。`host.go/tools.go/hubtoken.go` 不动。
- **依赖**:F1。**规模**:~1000 行实现 + ~1500 行测试,1.5d。
- **任务**:
  1. `Authenticator`:hub token 用 `subtle.ConstantTimeCompare`,enroll 码只 `ValidCode`。
  2. `AgentHub`:严格按 A4 的 6 步握手,另加:
     - enroll 码在 hello 中 `Redeem+BindAgent`,secret 的 `agentId` 必须和 hello 一致,不一致回 `agent-id-mismatch` 并关 4401。
     - 410 墓碑端点。
     - `Kick`/`Shutdown`。
     - 外层 ResponseWriter 若不是 Hijacker,返回明确 500 并打日志。
  3. `Registry` 改成会话绑定。
  4. `Dispatcher` 全部方法改成 `call(...)`,保持对外签名。实现 `AgentInspect`。保留 `noteWriteOutcome`、`noteToolUpgrade`。
  5. `EnrollmentManager.SetRevokeHook`,并修 `VerifySecret` 的数据竞争。
  6. `hb` 事件进入 `Registry.Update*`。
  7. 日志:连接、断开、顶替、鉴权失败(限速)全部可见。
- **验收**:`timeout 600 go test -race -count=1 ./internal/hub/...`。「重复 id / 顶替 / 在途断连 / 吊销踢线 / enroll 码烧毁」等用例见 D 节。

### P3 — agent 侧重写

- **文件归属**:`internal/agentd/{agentd.go, agentcfg.go, client.go, handlers.go, heartbeat.go, taskclass.go, tools.go, reexec_linux.go, reexec_other.go}` 和 `agentd_test.go/tools_test.go/agentcfg_test.go/bootstrap_adapters_test.go`。`executor*.go` 和 `hoststat*.go` 不碰。
- **依赖**:F1。**规模**:~900 行实现 + ~1200 行测试,1.5d。
- **任务**:
  1. `Run` 重连循环,严格按 A5 关闭码表。节流日志用 `retryLogger`,把它放进 `stream.ThrottledLogger` 或保留本地实现均可,但**必须每个重试点有日志**。
  2. 按 taskId 并发的 handler 注册表,任务类别和 `readSem`、写锁、singleflight 按 A5。
  3. 每任务 ctx 超时取 `min(dl-?, kind 限额)`,取消可中断(执行层不是 ctx-aware 的部分,取消后立即返回错误,后台 goroutine 自然结束,与现状一致,在注释里写明)。
  4. 心跳独立。drift 复用 `driftSummary` 逻辑(保留「未找到 homer 配置」新机器标记和 `forgetDrift`)。
  5. enroll:首次用 `hr_` 码连接,welcome 返回 secret 后**先落盘 agent.json 再宣布就绪**。落盘失败要大声记日志并继续运行。
  6. upgrade:`Flush` 后 reexec。
  7. `agentcfg` 新结构;删除所有 listen/advertise/mode 代码。
  8. 删除 `syncDeps()` 方法(`agentSyncDeps` 类型留给 P4b 删)。
- **验收**:`timeout 600 go test -race -count=1 ./internal/agentd/... -run 'TestDaemon|TestAgentCfg|TestHandlers|TestHeartbeat|TestReconnect|TestTaskClass'`。executor 相关旧测试可暂时不过(P4b 接手),但整个包必须能编译。

### P4a — shellenv PATH 缓存(可立即开工)

- **文件归属**:`internal/shellenv/**`。**依赖**:无。**规模**:0.3d。
- **任务**:`Path()` 对 `ReadLoginPATH()` 的结果做 30s TTL 缓存 + singleflight(并发调用只触发一次登录 shell)。新增 `Invalidate()`。保留 `ReadLoginPATH` 变量的可替换性(`manifest_test`、`path_test` 依赖)。变量 `LoginPATHTTL` 可被测试改写。
- **验收**:`timeout 300 go test -race -count=1 ./internal/shellenv/... ./internal/toolctl/... ./internal/manifest/...`。新增用例:TTL 内两次 `Path()` 只读一次、过期重读、`Invalidate` 立即生效、64 goroutine 并发只读一次。

### P4b — agent 数据面优化

- **文件归属**:`internal/agentd/{executor.go, inspect.go, snapshotcache.go, statusflight.go}`、`executor_test.go`、`executor_audit_test.go`、以及新测试文件。
- **依赖**:P3 合并后。**规模**:~700 行 + ~800 行测试,1d。
- **任务**:
  1. 快照缓存:`downloadHubSnapshot` 带 `If-None-Match`,304 复用内存里解码好的快照。缓存按 ETag 失效,并发安全。数据面请求用 `DataURL`(空则 `HubURL`)。
  2. `collect.inspect` handler:
     - 先下载(或命中缓存)一次快照。
     - 过滤出单个适配器的 config,分别 `CollectSnapshotSources` + `RunStatus(opts, sources)`,有界并发 4,每完成一个适配器发一个 `progress{stage:adapter}`。
     - 并发做凭证存在性探测(`keyring exists`)、`Push(confirm=false)` 预检提取 `secrets-rejected`、可选 `keys list`。
     - 合并报告(Adapters 拼接,Errors/Warnings 去重并集)。
     - **回退**:config 读不到(新机器)或任何一步异常时,退回整体 `RunStatus` 一次性返回。
  3. 单适配器合并后的结果必须与整体 `RunStatus` 在 `testdata` 上**逐字段等价**(golden 测试)。
  4. 删除 `agentSyncDeps` 类型。
- **验收**:`timeout 600 go test -race -count=1 ./internal/agentd/...`;额外 `go test -run TestInspectEquivalence -count=5`。

### P4c — web 层 + 前端(含 listen 清理)

- **文件归属**:`internal/web/**` 全部(含 `static/index.html`、`installscript.go`、`password.go`、`handlers.go`、`server.go`、`auth.go`、`choices.go`、`credentials.go`、`sync.go`)及其测试。
- **依赖**:F1(只需 `inspect_types.go`)。**规模**:~800 行 Go + ~300 行 JS,1.5d。
- **任务**:
  1. **闸口**:`/agent/v1/stream` 不经 web 的 `requireAuth`(由 `AgentHub` 自行鉴权,含 enroll 码)。其余 `/agent/*` 交给 `AgentEndpoint`(返回 410/404)。`/api/*`、`/dl/*` 的闸口**保持现状**(cookie、hub token、enroll 码、per-agent secret),用测试固化(见 D 闸口矩阵)。
  2. 删除 A6 列出的字段、路由、函数。`NewServer` 里 `Identity == nil &&` 判断简化。
  3. `handleSyncChoices`:`collect` 方向走 `InspectSource`(类型断言,缺失则退回 `AgentStatus`);支持 `stream=1` NDJSON(用 `http.Flusher`,逐行 flush,监听 `r.Context()` 取消并向下取消机器调用)。删除 `handleCollectPrecheck`、`probeCredentials`、`preflightSecrets` 的 HTTP 往返实现,改为消费 `InspectResult`(`secretDestination` 保留)。
  4. `handleSnapshotDownload` 加 ETag/304。
  5. `fanoutPullOnlineAgents` 改成**有界并发**(上限 8),结果顺序保持和 `ListAgents` 一致。
  6. `joinCommandsForRequest` 只返回单一命令;API 删 `listenCommand`;`installscript.go` 删 `--listen/--advertise/LISTEN/ADVERTISE`,启动参数改 `agent --hub $HUB`。收到 `--listen` 时报错并说明「listen 模式已移除」。**必须保持:后台起 daemon、不占前台**(SOP 规则 5)。
  7. `index.html`:
     - 删 `loadCollectPrecheck`、`precheckToken`、`/api/sync/precheck`。
     - `openScopeDialog` 对 collect 用 `fetch(...&stream=1)` 读取 `ReadableStream`,逐行增量渲染。每收到一个 `adapter` 事件渲染一行并显示「已读取 n/m」。`precheck/keys` 事件到达后就地更新各行(保留用户已勾选状态)。`done` 事件做最终对账。密钥列表在 collect 方向直接用 `keys` 事件,不再另发 `keyApi`。
     - 删 `a.mode === "listen"` 文案(719 行)、`join-listen` 相关 DOM 和 JS(379-380、2482-2508 行)。
     - 流被中断时显示明确错误,不留「正在读取…」。
- **验收**:`timeout 600 go test -race -count=1 ./internal/web/...`;`go vet ./internal/web/...`。新增:闸口矩阵表驱动测试、NDJSON 解析测试、ETag 304 测试、取消传播测试。

### P5 — CLI / 脚本 / docker / 文档

- **文件归属**:`internal/cli/**`(含 `commands/upgrade.go`、`agentrestart*.go` 测试数据)、`e2e/**`(docker-compose、`Dockerfile`、`hub-smoke.sh`、`console/docker-compose.yml`)、`README.md`、`DESIGN.md`、`docs/plan/2026-09-27-hub-plan.md`(只加顶部横幅)、`docs/plan/2026-10-08-ws-stream-plan.md`。另外 `grep -rn "connect\|listen\|poll" npm/` 确认 npm 包有无引用。
- **依赖**:I1。**规模**:~500 行,0.7d。
- **mode 概念散落点处理表**:

| 位置 | 处理 |
|---|---|
| `cli/args.go:167-174,334-357,404` | 删 `Listen/Connect/Advertise` 选项和解析;`--hub` 变为 agent 主 URL;加 `--data-url`;usage 文案重写 |
| `cli/run.go:404-409,517-521` | 删 `listen/connect` 的 unsupportedOptions 分支及二选一校验;`agent` 无参数=用 agent.json 重启 |
| `cli/hub.go` | `runAgent` 去掉 mode 分支;`hubBase` 用 `HubURL`;`credential` 同前;`agentModeLabel` 删除;`joinCommand` 和提示文案改 `--hub`;`runServe` 在 `Shutdown` 前先 `agentHub.Shutdown`,并装配 `Authenticator/AgentHub/RevokeHook/Dispatcher.Hub` |
| `cli/commands/upgrade.go:155-161` | `readAgentIdentity` 读 `hubUrl`(不再读 `connectUrl`);`--connect` 选项改 `--hub` |
| `agentrestart_test.go` | 测试数据里的 `--connect` 改 `--hub` |
| `cli/hubcommands_test.go` | 重写 listen/connect 互斥用例为新参数用例 |
| `e2e/docker-compose.yml:80,104`、`e2e/console/docker-compose.yml` | agent-a 去掉 `--listen/--advertise`,统一 `--hub http://hub:7760`(保留两个 agent 容器以覆盖多 agent) |
| `README.md:43-48` 及 `DESIGN.md:295` | 改写 agent 章节 |
| 旧计划文档 | 顶部加横幅(见下) |

**旧文档横幅**:
> **⚠️ 已被取代(2026-10-08)**:agent↔hub 传输层已重构为 WebSocket 多路复用 Stream,listen 模式与长轮询(connect)协议整体删除。本文 §0「不做 WebSocket」、§2.3 长轮询决策及相关 listen/connect 描述作废。新决策见 `docs/plan/2026-10-08-ws-stream-plan.md`。其余章节(web console、no-git 数据面等)除该文档另有说明外仍有效。

- **验收**:`timeout 600 go test -race -count=1 ./internal/cli/...`;`go vet -tags consolee2e ./tests/e2e/` 能编译(不要求能跑);`docker compose -f e2e/docker-compose.yml config` 通过(若本机有 docker)。

### P6 — 测试矩阵

- **文件归属**:`tests/e2e/**`、`tests/integration/**`(新)、`tests/bench/**`(新)。
- **P6a 集成**(P2+P3 合并后即可开始):`tests/integration/`,用 `buildHomer` 起 hub 和多个 agent 真进程,故障用 `streamtest.TCPProxy`。覆盖 D 节「集成」全部行。**规模**:~1500 行,1.5d。
- **P6b e2e/UI**(I1+P5 之后):
  - 更新 `hub_agents_test.go`、`tool_upgrade_test.go`、`installflow_test.go`、`console_docker_test.go` 等里所有 `--listen/--connect` 用法,改为新参数。
  - 删除或改写只测 listen 的用例。
  - chromedp UI 用例更新:去掉 `join-listen` 和 precheck,加增量渲染断言。
  - 新增:`TestWSReconnectAfterHubRestart`、`TestEnrollCodeBurnedReconnect`、`TestRevokeKicksLiveAgent` 等 e2e。
  - 在 `tests/bench/` 加性能脚本(见 D 的性能节)。
- **验收**:E 节的 e2e 命令全绿,**三个 SOP 守门员测试必须通过**。

### P7 — hw 实测与部署演练(orchestrator + 用户)

不在 worker 范围。步骤见 D 的性能验收与 C 的回滚点。部署严格遵守 `homer-serve-deploy-discipline`:只做 `go build -o ~/.local/bin/homer ./cmd/homer` + `systemctl --user restart homer-serve`。

---

# 第二段:风险与回滚(C)、测试矩阵(D)、自检命令(E)、开放问题(F)

## C. 风险登记与回滚检查点

**回滚检查点**(git tag):`pre-ws-master`(= dd29054)、`ws/f1-stream-lib`、`ws/i1-integrated`(各分支合并后 build+vet+单测绿)、`ws/p5-cli-green`、`ws/p6-green`。
**规则**:同一问题连续 2 次失败就回滚到上一个 tag,把原因写到 `docs/plan/ws-progress.md`,跳过该方向继续其他阶段。
**生产回滚**:`git checkout pre-ws-master && go build -o ~/.local/bin/homer ./cmd/homer && systemctl --user restart homer-serve`,不碰任何凭证文件。因为不保留兼容,新旧二进制的 hub 与 agent 不互通,回滚时 agent 也要换回旧二进制。

| # | 风险 | 触发场景 | 缓解 / 检查点 |
|---|---|---|---|
| R1 | CF/cloudflared 不透传 WS Upgrade 或空闲踢线 | `homerhw.openaaas.org` 经 http2 tunnel | **P0 第一验证项**。闸口 G0 不过不进 P1。应用层 ping 25s;若 CF 实测空闲上限低于 75s,缩 ping 间隔并更新 A7 |
| R2 | Hijack 与 `http.Server.WriteTimeout`(13min)/中间件冲突 | `ResponseWriter` 包装链不实现 Hijacker | P0 V11/V12 先验证;P2 增加「经 `web.Server` 全链路」集成测试 `TestStreamThroughWebServer`;包装器不是 Hijacker 时返回显式 500 和日志 |
| R3 | 旧 agent 与新 hub 互不兼容 | hw 现网 agent(connect 模式,systemd unit 里是 `--connect`)升级 hub 后失联 | 410 墓碑让旧 agent 日志可读;**部署 runbook**:先停旧 agent、改 unit 的 ExecStart 为 `homer agent --hub <url>`(agent.json 的 `agentSecret` 仍有效,`connectUrl` 字段被忽略,所以必须显式给 `--hub`),再启动。用户已接受不兼容 |
| R4 | 鉴权闸口回归(SOP 规则 3) | 改 `/agent/` 过闸方式时误伤 `/api/snapshot` 的 per-agent secret 或 `homer upgrade` 的 `/dl/homer` | P4c 必须带闸口矩阵表驱动测试;P6b 的 `TestInstallScriptEnrollFlow` 等守门员必跑。现状里 `authorized()` 对 `/api/*` 接受 enroll 码和 per-agent secret 属已有的宽放行,**本次不收紧**,在矩阵中标 `KNOWN-BROAD`,见 F-4 |
| R5 | 写操作并发破坏机器状态 | 并行 push/pull/upgrade | agent 写类任务 FIFO 互斥(A5);写完 `writeGen++` 防 singleflight 误用旧扫描。未知类别默认走写类 |
| R6 | 取消语义被高估 | 执行层(`commands`)非 ctx-aware | 文档写明取消=立刻停止等待并回错,已开始的写入不回滚(与现状一致);测试只断言「调用方立即返回」和「不重复发 res」 |
| R7 | 单适配器 status 合并结果与整体 `RunStatus` 不一致 | 新机器/manifest 类/remote 归一 | P4b golden 等价测试;任何异常回退到整体 `RunStatus` |
| R8 | enroll 码在 hello 内烧毁后网络中断 | agent 没收到 secret | 保持严格一次性(不削弱 SOP 规则);agent 明确日志,用户重新生成码;集成测试 `TestEnrollBurnedAfterHelloLost` 记录该行为 |
| R9 | 同 agentId 双进程互踢抖动 | 误启两个 agent | 被顶替方 ≥30s 退避且每次都记日志,hub 日志记录顶替事件;不会静默 |
| R10 | 半开连接 | 网络中途黑洞 | 75s ping 超时关闭会话并让在途调用立即失败;`TestHalfOpenDetectedByPing`、`TestBlackholeProxy` |
| R11 | hub 重启时在途任务 | `systemctl restart homer-serve` | `AgentHub.Shutdown` 先发 close 1001,在途调用以 `agent-offline` 立即失败;agent 退避重连自动重新 hello |
| R12 | 依赖获取受限(无外网) | `go get coder/websocket` 失败 | P1 第一步先验证;失败时上报 orchestrator 由人工 vendor,不改选 gorilla(除非 coder 在 P0 里有硬伤) |
| R13 | 流式响应被中间层缓冲 | CF 对 `application/x-ndjson` 缓冲 | P0 追加一项:经隧道的 chunked NDJSON 逐行到达时间;若被缓冲,前端自动退化为一次性渲染,但仍受益于并发和缓存(不属于协议降级) |

## D. 测试矩阵

### D1. 单测(`-race` 必过)

| ID | 场景 | 层级 | 阶段 | 验收命令 |
|---|---|---|---|---|
| U1 | 帧编解码往返、字段缺省、未知字段忽略 | 单测 | P1 | `go test -race ./internal/stream -run TestFrameCodec` |
| U2 | 畸形 JSON/缺 `t`/空帧/超 MaxFrame → `ProtocolError` | 单测 | P1 | `-run TestFrameDecodeInvalid` |
| U3 | 大帧上限:发送侧 `ErrFrameTooLarge`、接收侧关 1009、handler 结果过大回 `frame-too-large` | 单测 | P1 | `-run TestMaxFrame` |
| U4 | 未知帧类型被丢弃且连接存活,日志节流 | 单测 | P1 | `-run TestUnknownFrameType` |
| U5 | 多路复用:100 个并发 Call 正确配对 | 单测 | P1 | `-run TestMultiplexCorrelation -count=20` |
| U6 | 并发任务乱序完成(后发先到) | 单测 | P1 | `-run TestOutOfOrderCompletion` |
| U7 | 重复 in-flight id → `duplicate-id`,原调用不受影响;完成后 id 可复用 | 单测 | P1 | `-run TestDuplicateID` |
| U8 | 未知 method → `unknown-method` | 单测 | P1 | `-run TestUnknownMethod` |
| U9 | 取消:调用方 ctx 取消发 `cancel`,handler ctx 被取消,无多余 res | 单测 | P1 | `-run TestCancel` |
| U10 | **取消与完成同时**(CAS 终态唯一,无 panic/双发) | 单测 | P1 | `-run TestCancelCompleteRace -count=200` |
| U11 | dl 到期:handler ctx 超时,回 `timeout` | 单测 | P1 | `-run TestDeadlineBudget` |
| U12 | progress 有序到达;背压下可丢并计数,终态 res 不丢 | 单测 | P1 | `-run TestProgress` |
| U13 | 背压:对端不读,发送队列满 → 1008 `slow-consumer`,在途调用失败 | 单测 | P1 | `-run TestBackpressureSlowConsumer` |
| U14 | 心跳:收不到任何帧 75s(FakeClock)→ 关 4000;有数据帧则不超时 | 单测 | P1 | `-run TestHeartbeatTimeout` |
| U15 | 长 handler(超过 3×PingTimeout)期间 ping 不被饿死,连接不断 | 单测 | P1 | `-run TestLongHandlerKeepsAlive` |
| U16 | handler panic → `internal`,session 存活 | 单测 | P1 | `-run TestHandlerPanic` |
| U17 | session 结束:所有 pending Call 立即以 `SessionClosedError` 失败,handler ctx 取消 | 单测 | P1 | `-run TestSessionCloseFailsPending` |
| U18 | 入站并发上限 → `overloaded` | 单测 | P1 | `-run TestInflightLimit` |
| U19 | 二进制帧 → 关 1003 | 单测 | P1 | `-run TestBinaryFrame` |
| U20 | Backoff 范围/抖动/上限;节流日志窗口 | 单测 | P1 | `-run 'TestBackoff|TestThrottledLogger'` |
| U21 | wsconn 往返、close code/reason 保真、读限制、Hijack 后 deadline 被清(WriteTimeout=200ms 的服务器上活过 1s) | 单测(真 socket) | P1 | `go test -race ./internal/stream/wsconn` |
| U22 | `Authenticator`:secret/hub token/enroll 码/无效/吊销;常量时间比较 | 单测 | P2 | `go test -race ./internal/hub -run TestAuthenticator` |
| U23 | `VerifySecret` 加锁(并发 Revoke/Bind/Verify 无 race) | 单测 | P2 | `-run TestEnrollmentRace -race -count=20` |
| U24 | Registry:Attach 顶替返回 prev、Detach 只清当前、Stale 派生 | 单测 | P2 | `-run TestRegistryAttach` |
| U25 | Dispatcher 错误映射表全覆盖 | 单测 | P2 | `-run TestDispatcherErrorMapping` |
| U26 | taskclass 分类表(未知默认写类);写锁 FIFO、等待可取消 | 单测 | P3 | `go test -race ./internal/agentd -run TestTaskClass` |
| U27 | singleflight:并发 status 只扫描一次;写后 `writeGen` 防旧扫描 | 单测 | P3/P4b | `-run TestStatusFlight` |
| U28 | agentcfg:新结构读写、忽略旧 `mode/connectUrl`、缺 `hubUrl` 报明确错误 | 单测 | P3 | `-run TestAgentCfg` |
| U29 | shellenv TTL/Invalidate/并发 singleflight | 单测 | P4a | `go test -race ./internal/shellenv` |
| U30 | 快照缓存:ETag 命中不解码、变更重取、并发安全 | 单测 | P4b | `-run TestSnapshotCache` |
| U31 | Inspect 等价性(per-adapter 合并 == 整体 RunStatus,含新机器、manifest、扫描错误) | 单测 | P4b | `-run TestInspectEquivalence -count=5` |
| U32 | `/api/snapshot` ETag/If-None-Match/304/409 | 单测 | P4c | `go test -race ./internal/web -run TestSnapshotETag` |
| U33 | NDJSON 流:事件顺序、客户端断开取消机器调用、错误行 | 单测 | P4c | `-run TestChoicesStream` |
| U34 | 有界并发 fanout,结果顺序稳定 | 单测 | P4c | `-run TestFanoutBounded` |
| U35 | CLI 参数:`--listen/--connect/--advertise` 报未知选项;`--hub/--data-url` 解析;`agent` 无参数=重启 | 单测 | P5 | `go test ./internal/cli -run 'TestAgentFlags|TestUsage'` |

### D2. 闸口矩阵(SOP 规则 3,每格一条表驱动用例)

行=路径,列=凭证。✓=放行 ✗=401/403 410=墓碑。

| 路径 \ 凭证 | 无 | cookie | hub token | enroll 码(未烧) | enroll 码(已烧/过期) | per-agent secret | 吊销的 secret |
|---|---|---|---|---|---|---|---|
| `GET /agent/v1/stream` | ✗401 | ✗401 | ✓(hello 自报 id) | ✓(hello 内兑换) | ✗401 | ✓(hello id 必须匹配) | ✗401 |
| `/agent/v1/{enroll,register,poll,report}` | 410 | 410 | 410 | 410 | 410 | 410 | 410 |
| 其他 `/agent/*` | 404 | 404 | 404 | 404 | 404 | 404 | 404 |
| `/api/*`(如 `/api/console`) | ✗ | ✓ | ✓ | ✓ `KNOWN-BROAD` | ✗ | ✓ `KNOWN-BROAD` | ✗ |
| `GET /api/snapshot` | ✗ | ✓ | ✓ | ✓ | ✗ | ✓(agent 数据面必需) | ✗ |
| `GET /dl/homer`、`/dl/homer.gz` | ✗ | ✓ | ✓ | ✓(新机器下载) | ✗ | ✓(`homer upgrade`) | ✗ |
| `/install.sh` | 公开 | 公开 | 公开 | 公开 | 公开 | 公开 | 公开 |

命令:`timeout 300 go test -race ./internal/web ./internal/hub -run TestGateMatrix -count=1 -v`;e2e 版 `TestGateMatrixE2E` 在 P6b。

### D3. 集成(tests/integration,真进程,P6a)

| ID | 场景 | 阶段 | 验收命令 |
|---|---|---|---|
| I1 | hub + 3 agent 同时在线,各自 status 正确 | P6a | `timeout 900 go test ./tests/integration -run TestMultiAgentOnline -count=1 -race` |
| I2 | 断线重连:`TCPProxy.Cut()` 后 agent 在 ≤backoff 内重连,日志有重试行 | P6a | `-run TestReconnectAfterCut` |
| I3 | hub 重启恢复:重启 hub,agent 自动重连并重新注册,Registry 重建 | P6a | `-run TestHubRestartRecovery` |
| I4 | agent 重启:console 立即显示 offline,重启后上线 | P6a | `-run TestAgentRestart` |
| I5 | 任务在途时断连:调用立即 `agent-offline`,不等 60s | P6a | `-run TestInflightDisconnectFailsFast` |
| I6 | 同 agentId 重复连接:新连接顶掉旧连接,旧连接收 4001,在途调用失败,agent 日志可见 | P6a | `-run TestSameAgentIDSupersede` |
| I7 | stale/吊销 secret 被拒(401);`Revoke` 踢掉**已建立**连接并收到 4401 | P6a | `-run TestRevokedSecretKicked` |
| I8 | enroll 码烧毁后重连:用已烧码连接被拒且日志明确;用落盘 secret 能连 | P6a | `-run TestBurnedEnrollCodeReconnect` |
| I9 | 慢消费者:一个 agent 不读,不拖慢其他 agent 的调用 | P6a | `-run TestSlowConsumerIsolation` |
| I10 | 大 payload:2MB、7MB 往返成功;>8MB 得到明确错误 | P6a | `-run TestLargePayload` |
| I11 | 并发 50 任务全部成功,总耗时≈最慢单任务 | P6a | `-run TestFiftyConcurrentTasks` |
| I12 | 取消任务:调用方取消后 agent 侧 ctx 被取消,hub 立即返回 | P6a | `-run TestCancelTask` |
| I13 | 任务超时:hub 预算先到,返回 `agent-timeout` | P6a | `-run TestTaskTimeout` |
| I14 | upgrade:agent 返回结果后 reexec,重新连接并 hello | P6a | `-run TestUpgradeReexecReconnect` |
| I15 | tool upgrade 长任务(缩放:ping 20ms、任务 3s)不被心跳/读超时误杀 | P6a | `-run TestToolUpgradeLongTask` |
| I16 | Pull 长预算(缩放)不被误杀;12min 常量断言 | P6a | `-run TestPullLongBudget` |
| I17 | 并发 4 机器任务总耗时≈最慢单任务(用延迟 executor) | P6a | `-run TestFourTasksParallel` |
| I18 | 写类任务互斥、读类并行 | P6a | `-run TestWriteSerializedReadParallel` |
| I19 | hub 优雅关闭发 1001,agent 记录并重连 | P6a | `-run TestGracefulShutdownClose` |
| I20 | 经 `web.Server` 全链路的 Hijack(`WriteTimeout` 很小仍存活) | P2/P6a | `-run TestStreamThroughWebServer` |

### D4. 故障注入清单(用 `TCPProxy` / `FaultConn`)

| 故障 | 期望 | 对应用例 |
|---|---|---|
| 代理中途断连(RST) | 双方立即感知,在途调用失败,重连 | I2/I5 |
| 半开连接(blackhole,无 FIN) | 靠 ping 超时(缩放参数)发现,日志 `heartbeat timeout` | `TestHalfOpenDetectedByPing`(U14 的真 socket 版,P1/P6a) |
| 服务端优雅关闭发 close 帧 | agent 收到 `CloseError{Remote}`,按码表退避 | I19 |
| 客户端发畸形 JSON | hub 关 1002,不影响其他会话 | `TestMalformedJSONFromAgent`(P2) |
| 客户端发超大帧(>MaxFrame) | hub 关 1009 | U3 + `TestOversizeFromAgent`(P2) |
| 客户端发二进制帧 | hub 关 1003 | U19 |
| 慢 agent(handler 慢/读慢) | 其他调用不阻塞;预算到点超时 | I9/I13 |
| 乱序响应 | 正确配对 | U6 |
| 取消竞态 | 终态唯一 | U10 |
| 握手阶段:不发 hello / 先发其他帧 | 10s 超时或立即 1008 | `TestHelloTimeout`、`TestFrameBeforeHello`(P2) |
| hello 内 agentId 与 secret 不匹配 | `agent-id-mismatch`,关 4401 | `TestHelloIDMismatch`(P2) |

### D5. e2e / Docker / UI(P6b)

| ID | 场景 | 层级 | 验收命令 |
|---|---|---|---|
| E1 **守门员** | `TestInstallScriptEnrollFlow`(install.sh + hr_ 码 → 后台 agent → WS enroll → 带 drift 上线;脚本 30s 内返回) | e2e | 见 E 节 |
| E2 **守门员** | `TestPureServerFourStepStory` | e2e | 同上 |
| E3 **守门员** | `TestManualSyncFanout` | e2e | 同上 |
| E4 | 全部既有 hub/agent e2e 改用 `--hub` 后通过(`hub_agents_test.go`、`tool_upgrade_test.go` 等) | e2e | `timeout 1500 go test ./tests/e2e -count=1 -timeout 1400s` |
| E5 | `TestWSReconnectAfterHubRestart`:真 hub 进程重启,agent 重连后任务可用 | e2e | `-run TestWSReconnectAfterHubRestart` |
| E6 | `TestRevokeKicksLiveAgent`:控制台吊销后 agent 被踢且持续报 401 日志 | e2e | `-run TestRevokeKicksLiveAgent` |
| E7 | `TestGateMatrixE2E` | e2e | `-run TestGateMatrixE2E` |
| E8 | `TestOldProtocolTombstone`:旧端点返回 410 和可读 message | e2e | `-run TestOldProtocolTombstone` |
| E9 | docker e2e:hub + 2 agent 容器(都用 `--hub`) | docker | `timeout 1200 go test -tags consolee2e ./tests/e2e -run TestConsole -count=1`(需要 docker) |
| U-UI1 | chromedp:收取弹窗增量渲染(逐适配器出现,「已读取 n/m」),precheck 结果就地补上 | ui | `go test ./tests/e2e -run 'TestKeyringUI|TestDispatchUnlockUI|TestToolUpgradeUI|TestCollectDialogStream' -count=1` |
| U-UI2 | chromedp:接入对话框只有一条命令,无 `join-listen` | ui | `-run TestJoinDialog` |
| U-UI3 | 弹窗中途断开 agent:显示明确错误,不卡在「正在读取…」 | ui | `-run TestCollectDialogAgentDrops` |

### D6. 性能验收(hw,P7)

**环境**:不动生产。另起隔离 hub 和 agent:
```bash
mkdir -p /tmp/bench-hub /tmp/bench-agent
timeout 3600 homer serve --addr 127.0.0.1:17800 --home /tmp/bench-hub   # 设置密码/token 用隔离 home
# 在 hub 控制台或 API 铸造 enroll 码后:
HOMER_HUB_TOKEN=<hr_码> homer agent --hub http://127.0.0.1:17800 --data-url http://127.0.0.1:17800 --id hw-bench --home /tmp/bench-agent
```
agent 只读扫描真实 `~/.pi` 等目录。基准全部是读操作(choices/inspect/keys list),不执行 pull。

**基准脚本** `tests/bench/dialog_bench.sh`(worker 实现,骨架如下;keys list 的实际路径以 `internal/web/keys.go` 为准):
```bash
TOKEN=<bench hub token>; HUB=http://127.0.0.1:17800; AG=hw-bench; N=40
for i in $(seq 1 $N); do
  s=$(date +%s.%N)
  curl -s -H "Authorization: Bearer $TOKEN" "$HUB/api/sync/choices?direction=collect&agent=$AG" -o /dev/null &
  wait
  e=$(date +%s.%N); echo "$e - $s" | bc
done | sort -n | awk '{a[NR]=$1} END{print "p50",a[int(NR*.5)],"p95",a[int(NR*.95)],"max",a[NR]}'
```
另附 `go test ./tests/bench -bench BenchmarkFourParallel -benchtime 30x`(用延迟 executor 验证「并发 4 任务总耗时≈最慢单任务」)。

**验收**:
- P1:弹窗点击到可选项出现(含 precheck 与 keys 的整包 `stream=0`)**P95 < 2s**,基线 3.6~11.8s。配置了 `--data-url` 回环的情形。
- 无 `--data-url`(走 tunnel)的 P95 作为对照记录,不作硬指标。
- 首个适配器事件到达(流式)P95 < 1s。
- 并发 4 任务总耗时 ≤ 最慢单任务 × 1.3。
- 第二次及以后的 status 不再下载快照(agent 日志/计数器断言 304 命中率)。
- 一次登录 shell PATH 探测次数:单次 inspect ≤ 1。

## E. 每阶段自检命令

仓库里**没有** Makefile、`.golangci*`,CI 只有 `.github/workflows/release.yml`(goreleaser 与 install.sh 冒烟)。所以只用 Go 自带工具,**不使用 golangci-lint**。

**每个阶段结束必跑**(包 `timeout`):
```bash
cd <worktree>
timeout 120 gofmt -l .                      # 必须无输出
timeout 300 go build ./...
timeout 300 go vet ./...
timeout 900 go test -race -count=1 ./internal/<本阶段包>/...
```

**P1 额外**:基线 go.mod 本身未 tidy(`// indirect` 标记有误),所以**不要运行 `go mod tidy`**。依赖处理:`go get github.com/coder/websocket@v1.8.12` 之后写出第一个 import 它的源文件,再用 `go build ./...` 确认;验收用 `git diff -- go.mod go.sum`,只允许新增 coder/websocket 这一项 require 与对应 go.sum 行,`go 1.23.4` 不变。

**I1 集成后**:
```bash
timeout 300 go build ./... && timeout 300 go vet ./...
timeout 1500 go test -race -count=1 ./internal/... -timeout 1400s
```

**P6b 守门员(SOP 规则 1,合入前必跑)**:
```bash
GIT_CONFIG_GLOBAL=$(mktemp) timeout 900 go test ./tests/e2e/ \
  -run "TestInstallScriptEnrollFlow|TestPureServerFourStepStory|TestManualSyncFanout" \
  -count=1 -v -timeout 300s
```
**全仓**:
```bash
GIT_CONFIG_GLOBAL=$(mktemp) timeout 2400 go test ./... -count=1 -timeout 2300s
go vet -tags consolee2e ./tests/e2e/      # docker e2e 至少能编译
```

**部署后冒烟**(部署纪律,只做两步构建加重启,不碰凭证文件):
```bash
go build -o ~/.local/bin/homer ./cmd/homer && systemctl --user restart homer-serve
systemctl --user is-active homer-serve
curl -s http://127.0.0.1:7760/api/auth/status       # configured 应与部署前一致
TOKEN=$(cat ~/.homer/keys/hub-token)
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:7760/api/console | python3 -m json.tool | head -5
```

**SOP 规则 2 自检**:
```bash
grep -rn "sleepContext\|time.Sleep" internal/agentd/client.go   # 每个重试/退避点旁必须有 retries.log / ThrottledLogger 调用
```
并保留契约测试 `TestAgentdWSUnauthorizedIsLogged`(取代 `TestAgentdPoll401IsLogged`)。

## F. 需要用户拍板的开放问题

只留 4 条,其余都已在 0 节自行决定。

1. **hw 现网迁移(R3)**:hw 上现有 `hw-7735`(connect 模式,systemd unit 写死 `--connect`)在部署新 hub 后会失联。默认方案:**给 runbook,不写迁移代码**,由你手动改 unit 的 ExecStart 成 `--hub`(agent.json 的 secret 继续有效)。如果希望 agent 读到旧 agent.json 的 `connectUrl` 就自动当 `hubUrl` 用,告诉我,那是一段约 10 行的一次性兼容,会违背「不保留兼容」,所以默认不做。
2. **同机绕 tunnel 的自动探测**:默认只做显式 `--data-url`。如果想让 hub 在 welcome 里下发自身监听地址,让同机 agent 自动改走回环,需要多一个校验 hub 实例身份的步骤(`instanceId` 已在 welcome 里预留)。默认不做;你在 hw 配一次 `--data-url http://127.0.0.1:7760` 即可。
3. **浏览器→CF→hub 的 HTTP 100s(524)**:Pull 预算 12min 的控制台请求本来就会被 CF 在 100s 掐掉,这是已有问题,与本次传输层无关。现在有了 progress 帧,后续可以把 pull/upgrade 做成 NDJSON 流或异步任务+轮询。**本次不做**,是否单独立项?
4. **闸口收紧**:现状 `/api/*` 对 enroll 码和 per-agent secret 都放行(`KNOWN-BROAD`),我只用测试固化现状,没有收紧,避免本次同时改鉴权语义。建议后续单独做:`/api/*` 只认 cookie 和 hub token,secret 与 enroll 码限定在 `/api/snapshot` 和 `/dl/*`。是否排期?
