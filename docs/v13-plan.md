# homer-cli v1.3 实施计划（docs/v13-plan.md 内容）

> 基线：master `f24188e`（v1.2.0 已发布，455 tests 绿）。两大交付：① `homer pair` 在线配对命令（tailcat CLI 可选依赖做传输层，age 保持 at-rest 加密）② 状态/文档/测试收口。git 主通道、引擎、既有命令**零破坏性修改**，全部 additive。
>
> 依据：`/tmp/homer-tailcat-poc/POC-REPORT.md`（库 API 可用但 Go 1.27.1 + 24MB 两个硬伤 → CLI 子进程形态）；tailcat v0.7.0 源码验证过 `TAILCAT_ADDR_FILE`（官方支持的地址输出 env）、`--key=new`（强制 ephemeral key，防止用户存在 saved default key 时地址可复用）、无参 pipe 模式（stdin/stdout 双向管道）。

## 0. 目标与 Non-goals

**目标**：
1. `homer pair`（A 端 serve）：探测 tailcat CLI → 起一次性服务（`tailcat --key=new` 子进程）→ 打印一次性地址（带安全提示）→ 等待 B 连接 → **确认门**（TTY confirm / `--yes`）→ B 的 recipient 追加进 homer.json `secrets.recipients` → 发送 age 多 recipient 密文 bundle → 收 ack → 退出。
2. `homer pair <tc-addr>`（B 端 join）：连接 → 发 hello（hostname + B 的 age recipient）→ 收 offer（文件清单）→ 逐文件收 age 密文 → **全部解密验证成功后** 落盘（备份 → vault 0600 → destination 0600）→ ack → 退出。
3. 传输内容全程 age 密文（tailcat 只是加密管道）；协议为 homer 自有帧格式，与 tailcat 版本解耦。
4. A 侧 state.json 记录配对设备清单（hostname/recipient 指纹）；doctor 的 age 项显示已配对 N 台。
5. README：pair 用法、tailcat 安装（官方 INSTALL 链接）、DERP 延迟与自建 derper、安全模型。
6. 测试：协议全链路走注入的 fake PairTransport（进程内 io.Pipe/net.Pipe 对接）；tailcat 探测/降级走 PATH 注入假脚本；真实双机 e2e 留人工验收清单。

**Non-goals（v1.3 不做）**：不 import tailcat 库（Go 1.23 基线不动、CGO_ENABLED=0、依赖零新增）；不做文件同步快车道（只做密钥配对直传，不传 store）；不做多设备批量（一次一台）；不动 git 主通道语义（pair 不 commit 不 push，homer.json 改动留给用户 `homer push`）；B 侧 doctor 不显示配对清单（只 A 侧记录）；不做持久 pair 地址（每次 ephemeral）；不做 A 端既有 vault 的自动重加密（README 说明配对后跑一次 `homer secret push` 即可让 B 走 git 通道）。

## 1. 关键架构决策

| # | 决策 | 依据 |
|---|---|---|
| D1 | **CLI 子进程形态，协议自有**：homer 起 `tailcat --key=new`（serve，无参 pipe 模式）/ `tailcat <addr>`（join）子进程，stdin/stdout 即双向会话流；地址经 `TAILCAT_ADDR_FILE`（写 0600 临时文件，homer 轮询读取，30s 超时，同 tailcat 官方测试模式）。homer 侧帧格式完全自有 → tailcat 版本漂移不影响协议 | 用户确认的可选依赖形态；PoC §6.3 CLI 路径已验证；`--key=new` 强制 ephemeral（README Key Management 节：存在 saved default key 时无参 serve 会静默复用旧地址，必须显式 `--key=new`） |
| D2 | **bundle 生成 = 读源明文重加密（非解密旧 vault）**：A 读 `secrets.files` 登记的 destination 明文（与 `secret push` 同源同信任模型）→ `EncryptToRecipients(plaintext, A现有recipients + B.recipient)`。A 端不需要 identity 就绪；B 收到的密文同时给 A 自己与 B（多 recipient），B 原样写入自己的 vault 后 git 通道天然可用 | 与 RunSecretPush 完全一致的信任模型；避免 A 端解密环节（少一个故障面）；age 多 recipient 原生支持 |
| D3 | **双向 all-or-nothing**：A 端全部源读入内存后才发 offer（任一缺失 → abort，无部分传输）；B 端全部密文解密验证成功后才落盘（任一失败 → abort 帧，零写入）。落盘顺序：备份现有 destination → vault 原子写 → destination 写（0600） | 对齐 secret push/pull 既有 all-or-nothing 边界（`internal/cli/commands/secret.go` 同款模式） |
| D4 | **A 端确认门在发送任何密文之前**：收到 hello → 展示 hostname + recipient 指纹 → TTY confirm（非 TTY 必须 `--yes`）→ 拒绝则发 decline 帧、零写入（homer.json 不动）。顺序冻结：hello → confirm → 写 recipients → offer → blobs → ack | 任务安全要求；即使地址泄露，攻击者能拿到的只有 age 密文（无 B 的 identity 解不开），但密文本身仍是信息泄露面，确认门收敛之 |
| D5 | **传输层抽象 `PairTransport`**：生产实现 = tailcat CLI 子进程适配器；测试实现 = `internal/pair/pairtest` 的进程内 pipe 对接（net.Pipe + rendezvous channel）。命令编排（`RunPairServe/RunPairJoin`）只依赖接口 → CI 无真实 tailcat 也能测协议全链路 | 任务测试策略；manifest 包 `CommandPort` 同款 port seam 模式 |
| D6 | **tailcat 探测与降级**：`exec.LookPath("tailcat")`（可注入）→ 缺失：打印安装命令（官方 INSTALL 链接）+ 走 git 通道提示（`homer secret push`/`pull`），exit 1。不做静默降级（pair 的意义就是快车道，缺依赖应当显式失败） | 任务要求；显式失败优于半自动降级 |
| D7 | **B 端无确认门**（B 是主动 join 方，敲命令即意图）；offer 内容以预览行输出但不阻塞。`--yes` 仅对 A 端 serve 生效（B 端接受但 no-op，文档注明） | 任务原文 B 端"连接 → 配对 → 完成即退出" |
| D8 | **homer.json 修改不自动 commit**：pair 只原子写 recipients（复用 `SaveConfig`），报告 warning 提示用户随后 `homer push`（homer.json 在 git 仓库内，push 会带上）——B 若要走 git 通道，再跑一次 `homer secret push` 即可 | Non-goal"不动 git 主通道语义"；与 never-auto-push 哲学一致 |

## 2. 冻结接口（全部签名，逐字落地）

### 2.1 core（P0）—— state.json 配对清单

```go
// internal/core/state.go —— additive
type PairedDevice struct {
    Hostname  string `json:"hostname"`
    Recipient string `json:"recipient"`
    PairedAt  string `json:"pairedAt"` // RFC3339Nano UTC，同 LastSyncAt 格式
}

type HomerState struct {
    Version         int            `json:"version"`
    LastSyncCommit  string         `json:"lastSyncCommit,omitempty"`
    LastSyncAt      string         `json:"lastSyncAt,omitempty"`
    LastSyncCommand string         `json:"lastSyncCommand,omitempty"`
    Paired          []PairedDevice `json:"paired,omitempty"`
}

// AddPairedDevice returns a copy with the device merged in. Dedup key is
// Recipient: an existing entry for the same recipient has its Hostname and
// PairedAt updated in place (position preserved); a new recipient appends.
func (s HomerState) AddPairedDevice(device PairedDevice) HomerState

// PairedDisplay renders "hostname（age1abcd1234…wxyz）" per device in
// recorded order; malformed entries are skipped. Used by doctor and reports.
func PairedDisplay(state HomerState) []string
```

`LoadState` 增量：解析 `paired` 数组（元素须为对象且 hostname/recipient/pairedAt 均为字符串；任何畸形 → 整段忽略，保持 state 的 non-authoritative 宽松哲学，**不**让畸形 paired 毁掉 lastSyncCommit）。`stateValue` 序列化：`paired` 排在 `lastSyncCommand` 之后（仅非空时输出）。

### 2.2 agecrypto（P0）—— 两个导出（原私有能力开放）

```go
// internal/agecrypto/vault.go —— additive
// CiphertextLooksSafe is the vault self-check (ciphertext != plaintext, no
// plaintext sample leakage at head/middle/tail) exported for the pair
// command's re-encryption path. Behavior identical to the private original.
func CiphertextLooksSafe(ciphertext, plaintext []byte) bool

// WriteVaultCiphertext atomically writes an already-encrypted payload to
// <secretsDir>/<name>.age with 0600 (temp file + rename, dir 0700). Invalid
// names return the same CliError as SecretFilePath. The join side uses this
// to store the received multi-recipient ciphertext verbatim.
func WriteVaultCiphertext(p core.HomerPaths, name string, ciphertext []byte) error
```

（实现即把现有 `atomicWriteVaultFile` 接到新导出函数上；`EncryptSecretToFile` 行为零变化。）

### 2.3 pair 包（P0 接口 + 探测；P1 协议；P2 适配器）

```go
// internal/pair/transport.go (P0)
package pair

// PairTransport produces the one bidirectional session stream used by the
// pairing protocol. Production uses the tailcat CLI adapter (tailcat.go);
// tests use internal/pair/pairtest.
type PairTransport interface {
    // Serve starts a one-shot server. The returned PairServer exposes the
    // ephemeral address before the peer connects.
    Serve(ctx context.Context) (PairServer, error)
    // Connect dials the peer's address and returns the session stream.
    Connect(ctx context.Context, addr string) (io.ReadWriteCloser, error)
}

// PairServer is the serve-side session handle.
type PairServer interface {
    // Addr is the one-shot tailcat address to exchange out of band.
    Addr() string
    // Accept blocks until the peer connects and returns the session stream.
    // One-shot: a second call returns an error.
    Accept(ctx context.Context) (io.ReadWriteCloser, error)
    // Close tears down the listener and any child process; idempotent.
    Close() error
}

// ErrNoTailcat is returned by DetectTailcat when the CLI is not in PATH.
var ErrNoTailcat = errors.New("tailcat CLI not found in PATH")

// DetectTailcat resolves the tailcat binary. lookPath nil => exec.LookPath.
// Both the serve and join paths call this first and render TailcatInstallHint
// on failure.
func DetectTailcat(lookPath func(string) (string, error)) (string, error)

// TailcatInstallHint is the frozen fallback guidance when tailcat is missing.
const TailcatInstallHint = "未找到 tailcat（密钥配对的传输层）。\n安装: 参考官方 INSTALL https://github.com/tailscale/tailcat（静态二进制 / brew / 包管理器）\n或继续使用 git 通道: 旧机 `homer secret push`，新机 `homer secret pull`。"

// Deadline constants (overridable only via test-injected transports/ctx).
const (
    AddrFilePollInterval = 100 * time.Millisecond
    AddrFileTimeout      = 30 * time.Second  // tailcat writes TAILCAT_ADDR_FILE
    AcceptDeadline       = 10 * time.Minute  // serve waits for the peer
    StepDeadline         = 120 * time.Second // hello/offer/ack per-step read
)
```

```go
// internal/pair/protocol.go (P1) —— 纯函数，仅依赖标准库 + agecrypto.RecipientValid
package pair

const ProtocolVersion = 1

// Frame bounds: one frame carries one age ciphertext at most.
const (
    MaxFrameBytes       = 16 << 20 // single-frame payload cap (matches manifest stdout cap)
    MaxTotalBundleBytes = 64 << 20 // whole-bundle cap across all blob frames
)

// Frame types. Wire format per frame:
//   [4-byte big-endian payload length][1-byte frame type][payload]
const (
    FrameHello   uint8 = 1 // join -> serve: PeerHello JSON
    FrameDecline uint8 = 2 // serve -> join: Decline JSON (user refused)
    FrameOffer   uint8 = 3 // serve -> join: Offer JSON (file manifest)
    FrameBlob    uint8 = 4 // serve -> join: one age ciphertext (order = Offer.Files)
    FrameAck     uint8 = 5 // join -> serve: Ack JSON
    FrameAbort   uint8 = 6 // either side: Abort JSON (all-or-nothing failure)
)

type PeerHello struct {
    Version      int    `json:"version"`   // must equal ProtocolVersion
    Hostname     string `json:"hostname"`
    Recipient    string `json:"recipient"` // join side's age1... public key
    HomerVersion string `json:"homerVersion,omitempty"` // informational; may be empty in v1.3
}

type Decline struct {
    Reason string `json:"reason,omitempty"`
}

type OfferFile struct {
    Name string `json:"name"` // vault name; must exist in BOTH sides' secrets.files
    Size int    `json:"size"` // ciphertext byte length of the matching blob frame
}

type Offer struct {
    Version int         `json:"version"`
    Files   []OfferFile `json:"files"`
}

type Ack struct {
    OK        bool     `json:"ok"`
    Applied   []string `json:"applied,omitempty"`
    BackupDir string   `json:"backupDir,omitempty"`
    Errors    []string `json:"errors,omitempty"`
}

type Abort struct {
    Reason string `json:"reason,omitempty"`
}

// WriteFrame writes one length-prefixed frame. A nil/short writer error is
// returned as-is; payloads larger than MaxFrameBytes are rejected before any
// write.
func WriteFrame(w io.Writer, frameType uint8, payload []byte) error

// ReadFrame reads one frame with io.ReadFull semantics. A frame whose
// declared length exceeds maxBytes (caller passes MaxFrameBytes) returns
// ErrFrameTooLarge without allocating; EOF/short reads return the wrapped
// error. frameType is validated against the known set.
func ReadFrame(r io.Reader, maxBytes int) (frameType uint8, payload []byte, err error)

// ErrFrameTooLarge guards against a malicious peer forcing an oversized
// allocation.
var ErrFrameTooLarge = errors.New("pair frame exceeds size limit")

// ValidateHello checks the peer greeting: Version == ProtocolVersion,
// Recipient passes agecrypto.RecipientValid, Hostname is 1..63 printable
// ASCII (no control characters — prevents terminal-escape injection into the
// serve side's confirmation prompt).
func ValidateHello(hello PeerHello) error

// PeerDisplay renders the confirmation prompt line:
//   "machine-b（age1abcd1234…wxyz，homer pair）" — hostname plus recipient
// fingerprint (first 12 + last 4 chars). Never echoes the full recipient.
func PeerDisplay(hello PeerHello) string

// Decode helpers: json.Unmarshal wrappers with strict type checks.
func DecodeHello(payload []byte) (PeerHello, error)
func DecodeOffer(payload []byte) (Offer, error)
func DecodeAck(payload []byte) (Ack, error)
```

```go
// internal/pair/pairtest/pipe.go (P1) —— 测试专用进程内对接
package pairtest

// NewPipePair returns two pair.PairTransport values wired to each other.
// Serve on one side makes the fixed address "pipe-test" available via
// Addr(); the peer's Connect with that address rendezvous through an
// internal channel and both sides receive opposite ends of a net.Pipe().
// Accept returns an error after the first connection (one-shot, like the
// production transport). Test-only: never used by production code paths.
func NewPipePair() (left, right pair.PairTransport)
```

```go
// internal/pair/tailcat.go (P2) —— CLI 子进程适配器
package pair

// TailcatOptions configures the CLI adapter.
type TailcatOptions struct {
    Binary          string        // default "tailcat"; resolved via DetectTailcat at construction
    AddrFileTimeout time.Duration // default AddrFileTimeout
    ExtraEnv        []string      // appended after os.Environ (e.g. TAILCAT_DERPMAP_URL passthrough)
    Stderr          io.Writer     // tailcat diagnostic passthrough; nil => io.Discard
}

// NewTailcatTransport validates the binary is present (DetectTailcat) and
// returns the production PairTransport. It returns ErrNoTailcat when the
// CLI is missing so the command layer can render TailcatInstallHint.
func NewTailcatTransport(options TailcatOptions) (*TailcatTransport, error)

// *TailcatTransport implements PairTransport:
//   Serve:  exec <binary> --key=new   (pipe mode, ephemeral key forced)
//           env: os.Environ + ExtraEnv + TAILCAT_ADDR_FILE=<0600 temp file>
//           polls the addr file (AddrFilePollInterval, AddrFileTimeout);
//           the child runs in its own process group (Setpgid), Accept
//           returns a ReadWriteCloser wrapping the child's stdin/stdout,
//           Close kills the whole process group and removes the addr file.
//   Connect: exec <binary> -- <addr>  (pipe client; stdin/stdout wrapped the
//           same way; Close waits for the child and kills the group).
```

### 2.4 commands（P0 stub → P1 全量）

```go
// internal/cli/commands/pair.go
type PairOptions struct {
    HomerHome string
    Addr      string // join side: target tc address; empty = serve side
    Yes       bool
    JSON      bool
}

type PairDeps struct {
    UI        PromptPort         // confirm gate; nil => non-interactive fallback
    Transport pair.PairTransport // nil => pair.NewTailcatTransport(zero options)
    Age       agecrypto.AgeCryptoPort // nil => agecrypto.NewAgeCryptoPort()
    OnAddr    func(addr string)  // serve side: called as soon as the one-shot
                                 // address is known (production prints it
                                 // immediately; JSON mode defers to the report)
}

type PairPeerSummary struct {
    Hostname  string `json:"hostname"`
    Recipient string `json:"recipient"`
}

type PairServeStatus string

const (
    PairServeStatusNoTailcat  PairServeStatus = "no-tailcat"
    PairServeStatusNoConfig   PairServeStatus = "no-config"   // no homer.json or secrets.files empty
    PairServeStatusNoSource   PairServeStatus = "missing-source"
    PairServeStatusAborted    PairServeStatus = "aborted"     // local user refused the peer
    PairServeStatusPeerFailed PairServeStatus = "peer-failed" // peer aborted / bad ack
    PairServeStatusPaired     PairServeStatus = "paired"
    PairServeStatusError      PairServeStatus = "error"       // transport/timeout/io
)

type PairServeReport struct {
    OK             bool             `json:"ok"`
    Status         PairServeStatus  `json:"status"`
    Addr           string           `json:"addr,omitempty"`
    Peer           *PairPeerSummary `json:"peer,omitempty"`
    Sent           []string         `json:"sent"`          // vault names transferred
    RecipientAdded bool             `json:"recipientAdded"` // homer.json updated
    PairedCount    int              `json:"pairedCount"`    // total devices incl. this one
    Warnings       []string         `json:"warnings"`
    Errors         []string         `json:"errors"`
}

func (report PairServeReport) ExitCode() int // OK => 0 else 1

type PairJoinStatus string

const (
    PairJoinStatusNoTailcat     PairJoinStatus = "no-tailcat"
    PairJoinStatusNoConfig      PairJoinStatus = "no-config"
    PairJoinStatusNoIdentity    PairJoinStatus = "no-identity"
    PairJoinStatusDeclined      PairJoinStatus = "declined"       // serve side refused
    PairJoinStatusUnknownSecret PairJoinStatus = "unknown-secret" // offer name not in local secrets.files
    PairJoinStatusUndecryptable PairJoinStatus = "undecryptable"
    PairJoinStatusPaired        PairJoinStatus = "paired"
    PairJoinStatusError         PairJoinStatus = "error"
)

type PairJoinReport struct {
    OK        bool             `json:"ok"`
    Status    PairJoinStatus   `json:"status"`
    Peer      *PairPeerSummary `json:"peer,omitempty"`
    Applied   []string         `json:"applied"`
    BackupDir string           `json:"backupDir,omitempty"`
    Warnings  []string         `json:"warnings"`
    Errors    []string         `json:"errors"`
}

func (report PairJoinReport) ExitCode() int // OK => 0 else 1

// RunPairServe implements the serve-side session (machine A). See §4 for the
// frozen step order and safety gates.
func RunPairServe(options PairOptions, deps *PairDeps) PairServeReport

// RunPairJoin implements the join-side session (machine B).
func RunPairJoin(options PairOptions, deps *PairDeps) PairJoinReport

// ExecutePair dispatches on options.Addr (empty => serve), renders the
// report (or ordered JSON), and returns the process exit code. It installs
// the signal context (SIGINT/SIGTERM => cancel => deferred transport Close).
func ExecutePair(options PairOptions, deps *PairDeps, out, errOut io.Writer) int

func RenderPairServeReport(report PairServeReport) string
func RenderPairJoinReport(report PairJoinReport) string

const PAIR_USAGE = `用法: homer pair [options]           （机器 A：serve，输出一次性地址并等待）
      homer pair <tc-addr> [options]  （机器 B：join，完成配对后退出）

两台机器在线时的密钥快车道：tailcat 建立 WireGuard 加密管道（可选外部
依赖），传输内容全部为 age 密文——即使地址泄露，拿到的只是密文。

流程:
  1. 机器 A 运行 homer pair，把输出的一次性地址通过安全信道（当面/
     私密消息）交给机器 B；切勿粘贴到 git、issue 或群聊。
  2. 机器 B 运行 homer pair <tc-addr>；A 端确认对方 hostname 与
     recipient 指纹后放行（非 TTY 需 --yes）。
  3. A 将全部密钥以 age 多 recipient 密文（A 现有 recipients + B）发送；
     B 解密验证后写入 secrets.files 登记的目标路径（0600，覆盖前备份），
     并同步写入自己的 secrets/ vault。

前置: 两端已完成 homer init / homer home；B 端已 homer secret keygen；
     两端 PATH 中有 tailcat（官方 INSTALL:
     https://github.com/tailscale/tailcat，建议 v0.7.0+）。

选项:
  --home <dir>       homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --yes              serve 端跳过对端确认（非 TTY 必须）；join 端无操作
  --json             输出机器可读 JSON
  -h, --help         显示本帮助

退出码: 配对成功 → 0；tailcat 缺失 / 用法错误 / 对端拒绝或传输失败 → 1。

安全: 地址即凭证（bearer secret）且进程结束即失效；无 tailcat 时请继续
使用 homer secret push / homer secret pull（age 密文走 git）。`
```

### 2.5 doctor（P0）—— age 项配对显示

```go
// internal/doctor/checks.go —— CheckAge 签名不变，成功路径增量：
// state := core.LoadState(paths)
// if devices := core.PairedDisplay(state); len(devices) > 0 {
//     message 追加 "；已配对 %d 台设备"
//     details 逐台 "hostname（age1abcd1234…wxyz）"（上限 5 条 + "… 其余 N 台"）
// }
// 其余七项检查与八项顺序零变化。
```

### 2.6 CLI 面（P2）

```go
// internal/cli/args.go
const CommandPair Command = "pair"
// COMMANDS 顺序：... CommandSecret, CommandPair, CommandVersion, CommandHelp
// USAGE 命令清单在 secret 行后追加：
//   "  pair      在线配对另一台机器（tailcat 快车道，传输全程 age 密文）\n"
// USAGE 示例区追加：
//   "  homer pair\n  homer pair <tc-addr>\n"
// commandUsage(pair) 返回 commands.PAIR_USAGE 的首两行形态（"用法: homer pair [options]…"）

// internal/cli/run.go
// allowPositionals 增加 CommandPair；pair 特有校验：
//   len(Positionals) > 1 => "多余的参数: ..."
// dispatch: case CommandPair: return commands.ExecutePair(commands.PairOptions{
//     HomerHome: options.Home, Addr: firstPositionalOr(""), Yes: options.Yes, JSON: options.JSON,
// }, nil, out, errOut)

// validateCommandOptions: case CommandPair:
//   允许 --home/--yes/--json；拒绝 no-push/accept-local/accept-remote/offline/
//   all/verbose/force/adapters/adapter/category/mode/remote（unsupportedOptions 复用）
```

## 3. 协议时序（冻结）

```
机器 A (serve)                              机器 B (join)
homer pair                                  homer pair <tc-addr>
  │ DetectTailcat → Serve → addr              │ DetectTailcat → Connect(addr)
  │ OnAddr(addr)：打印地址+安全提示            │
  │ Accept（等连接，AcceptDeadline 10min）     │
  │                                           │── FrameHello {v1, hostname, recipient} ──▶
  │◀──────────────────────────────────────────│
  │ ValidateHello（版本/recipient/hostname）    │
  │ TTY confirm / --yes ── 拒绝 ──▶ FrameDecline ─▶ B: declined, exit 1; A: aborted, exit 1
  │ 同意：                                     │
  │  recipients += B（去重）→ SaveConfig 原子写 │
  │  全部源读入内存（缺一 → FrameAbort）        │
  │── FrameOffer {files:[{name,size}...]} ───▶│ 校验 names ⊆ 本机 secrets.files
  │── FrameBlob ×N（age 密文）──────────────▶│ 逐个解密验证（用 B 的 identity）
  │                                           │  任一失败 → FrameAbort → A: peer-failed
  │                                           │ 全部成功：
  │                                           │  备份 destination → WriteVaultCiphertext
  │                                           │  → 写 destination 0600
  │◀──────────── FrameAck {ok, applied, ...} ─│
  │ ack.ok → state.Paired 更新 → SaveState     │
  │ 双方 exit 0                                │
```

**帧格式**：`[4B 大端 payload 长度][1B frameType][payload]`；单帧 ≤16MiB、bundle 总量 ≤64MiB；JSON 消息用 `encoding/json`（顺序无关）。

## 4. 安全设计（单列）

| # | 面 | 设计 |
|---|---|---|
| S1 | **地址生命周期（one-shot）** | ① `--key=new` 强制 ephemeral key——用户机器上存在 saved default key（`~/.config/tailcat/keys/default.private.json`）时，裸 `tailcat` 会静默复用旧地址（v0.7.0 README Key Management 节），显式 flag 封死该路径；② 地址经 `TAILCAT_ADDR_FILE` 写入 0600 临时文件，pair 结束（成功/失败/中断）defer 删除；③ serve 是 one-shot：协议完成或失败即 Close（杀子进程组）→ 地址随 ephemeral key 消亡，PoC §4 已证 ephemeral 重启后旧地址不可用；④ 打印地址时强制提示“仅带外交换，勿入 git/chat/issue”；JSON 模式 `addr` 字段文档警告勿落盘共享位置 |
| S2 | **A 端确认门** | hello 之后、**发送任何密文之前**，A 展示 `PeerDisplay`（hostname + recipient 指纹，**不回显完整 recipient**）+ TTY confirm；非 TTY 无 `--yes` → aborted（fallback=false 安全默认，与 push/pull promptConfirm 同哲学）。拒绝 → decline 帧 + **零写入**（homer.json/state 均不动）。纵深：即使攻击者绕过确认拿到数据，也只有 age 密文（recipients = A 现有 + B，攻击者无 B 的 identity）；`ValidateHello` 拒绝含控制字符的 hostname（防终端转义注入确认提示） |
| S3 | **bundle 重加密与原子性** | A 端：全部 destination 源文件读入内存后才发 offer（all-or-nothing，任一缺失 → abort，无部分传输）；每密文 `CiphertextLooksSafe` 自检（密文≠明文、无明文样本）通过才发送。B 端：全部密文解密验证成功后才落盘（all-or-nothing，零部分写入）；落盘顺序 = 备份现有 destination（`backup.BackupFiles`，0700/0600）→ vault 原子写（temp+rename 0600）→ destination 写（MkdirAll+WriteFile 0600+Chmod）。homer.json recipients 经 `SaveConfig` 原子写（含 ValidateConfig 回读）。落盘阶段失败：报告列出已写入文件 + 备份目录路径（可手动恢复），不做自动回滚（文档说明） |
| S4 | **帧与路径防御** | `MaxFrameBytes` 16MiB / `MaxTotalBundleBytes` 64MiB（超限即断，防恶意对端 OOM）；blob 的 name 必须在本机 `secrets.files` 配置内（B 端 `DestinationOf` 查不到 → unknown-secret 中止）——destination 路径永远来自本机配置而非对端数据，路径穿越无从谈起；`SecretNameValid` 已有的名称校验继续生效 |
| S5 | **中断清理** | `ExecutePair` 用 `signal.NotifyContext(ctx, SIGINT, SIGTERM)`；tailcat 子进程 `Setpgid` 独立进程组，`Close()`/ctx cancel → `kill(-pid, SIGKILL)`（与 gitx/manifest 现有进程组模式一致）→ Ctrl-C 不留孤儿 tailcat；defer 链：conn Close → server Close（杀进程组+删地址文件）。A 端 ack 失败/中断：报告 warning“recipients 已写入 homer.json 但配对未完成（B 公钥无害，可保留或重试）” |
| S6 | **密钥分层** | tailcat = 传输层（WireGuard E2E + NAT/DERP）；age = at-rest 与端到端内容加密（bundle 对 B 的 recipient 加密，管道泄露不等于密钥泄露）；git 通道 = 兜底（无 tailcat 时 push/pull 不受影响）。三层职责 README 安全节明示 |

## 5. 波次拆分（文件归属互斥）

| 波次 | 模块 | 文件清单（独占） | 依赖 | 验收（机器可查） | 并行 |
|---|---|---|---|---|---|
| **P0** 基础（单 writer，串行，~0.5d） | state.Paired + doctor 显示 + agecrypto 导出 + pair 包 scaffold（接口/Detect/常量/消息类型定义）+ commands/pair.go stub | `internal/core/{state.go,core_test.go}`、`internal/doctor/{checks.go,checks_test.go}`、`internal/agecrypto/{vault.go,agecrypto_test.go}`、`internal/pair/{transport.go,detect.go,detect_test.go,placeholder_test.go}`(新包)、`internal/cli/commands/{pair.go,pair_test.go}`(新，stub：ExecutePair 只实现 no-tailcat/no-config 前置检查 + PAIR_USAGE + 渲染骨架) | 无 | `go build ./... && go vet ./... && go test ./...` 全绿（455 基线零回归）；state round-trip 含 paired（新增/去重/畸形忽略）；doctor CheckAge paired 显示断言；CiphertextLooksSafe/WriteVaultCiphertext 表测；DetectTailcat（PATH 注入存在/缺失） | — |
| **P1** 协议层 + 命令编排（~1.5d） | 帧读写/校验纯函数 + PipeTransport + RunPairServe/RunPairJoin 全量 + 进程内全链路测试 | `internal/pair/{protocol.go,protocol_test.go}`、`internal/pair/pairtest/{pipe.go,pipe_test.go}`(新子包)、`internal/cli/commands/{pair.go(重写编排),pair_test.go}` | P0 | `go test ./internal/pair/... ./internal/cli/commands/...`：帧 round-trip/超长帧拒绝/EOF 短读；ValidateHello 四分支（版本/recipient/hostname 长度/控制字符）；PipeTransport 双端 rendezvous + 二次 Accept 拒绝；全链路（PipeTransport + 真 age port + fake UI）：成功路径断言 B destination 逐字节+0600、B vault 可解密、A recipients 含 B、A state.paired 记录、双方 report status=paired；UI 拒绝 → aborted+declined+零写入；A 源缺失 → missing-source+B error；B 无 identity → no-identity（serve 端短 ctx accept 超时路径）；offer 未知名 → unknown-secret+A peer-failed；fake Age Decrypt 失败 → undecryptable；ack 失败 → peer-failed+recipients warning | **P1 ∥ P2** |
| **P2** tailcat 适配器 + CLI 接线（~1d） | CLI 子进程传输 + args/run 注册 + 探测/降级测试 | `internal/pair/{tailcat.go,tailcat_test.go}`、`internal/cli/{args.go,run.go,exitcode_test.go}`、`tests/e2e/v13_pair_cli_test.go`(新) | P0（接口）；与 P1 零文件交集 | `go test ./internal/pair/... ./internal/cli/... ./tests/e2e/...`：TailcatTransport 用 PATH 注入假 tailcat 脚本（写 TAILCAT_ADDR_FILE + `cat` 双向）测 Serve（地址轮询/超时/进程组清理/Close 幂等）与 Connect（argv 断言 `--key=new` / `-- <addr>`）；args 表测（无参/带参/2 个位置参数拒绝/未知 flag/--help 打印 PAIR_USAGE）；runWithIO + 空 PATH → serve/join 均 exit 1 且 stderr 含安装提示与 git 通道提示；USAGE 含 pair 行 | **P2 ∥ P1** |
| **P3** e2e + 文档 + review（~1d，串行收口） | CLI 级 e2e（进程边界） + README + 人工验收清单 + 对抗 review | `tests/e2e/v13_pair_test.go`(新)、`README.md`、`docs/v13-report.md`(新) | **P1+P2 全部** | e2e：① 真二进制 + 空 PATH：`homer pair` exit 1 + 提示；② PATH 假 tailcat（立即写地址文件 + sleep 挂等）：`homer pair` stdout 含地址与“勿入 git”提示，kill 后 `pgrep tailcat` 无残留；③ `homer pair --help` / `--json` 冒烟；④ `homer doctor --json` 在含 paired state 的 home 上显示已配对 N 台；README：pair 章节（用法/tailcat 安装链接/`--key=new` 说明）、DERP 延迟与自建 derper（`TAILCAT_DERPMAP_URL` env 透传说明）、安全模型（S1-S6 摘要）、无 tailcat 的 git 通道回退；`go vet ./... && go test ./...` 全绿（≥515）；fresh-context 对抗 review（正确性/测试覆盖/简洁性）后修复归零 | — |

并行度：P0 串行 → **P1 ∥ P2 两路**（文件零交集：P1 管 `internal/pair/protocol*`、`pairtest/`、`commands/pair*.go`；P2 管 `internal/pair/tailcat*`、`internal/cli/{args,run}*.go`、pair e2e CLI 冒烟）→ P3 收口。**总计 4 个波次 6 个模块，1 处两路并行（P1∥P2）**。

## 6. 风险与回滚点

| # | 风险 | 对策 | 回滚点 |
|---|---|---|---|
| R1 | **tailcat CLI pipe 模式的连接前写入语义未证**：A 端在收到 hello 前绝不写数据（协议顺序天然保证）；B 端 Connect 后立即写 hello，依赖 client 子进程在 tunnel 建立前缓冲 stdin——PoC 只证了 client→server 单向，真实双机行为留 P3 人工验收第 7 条显式验证 | Go 级全链路用 PipeTransport 不受影响；人工清单覆盖；若真机发现 client 丢弃连接前 stdin，修复方案（B 端等待 server 先发 server-hello 帧）是协议 additive 扩展，不动既有帧 | 协议帧 additive；PairTransport 接口不变 |
| R2 | **DERP 延迟秒级**（PoC 实测 ping ~5.4s，map fetch 1.4s）：StepDeadline 120s / AcceptDeadline 10min 已覆盖慢路径；README 明示公网 DERP 无 SLA + rate limit，自建 derper 指引（ExtraEnv 透传 `TAILCAT_DERPMAP_URL`） | 常量集中在 transport.go，单点调整 | — |
| R3 | **tailcat 无稳定性承诺**（README Stability 节）：homer 侧协议自有帧（tailcat 只是字节管道）→ 版本漂移只影响子进程 argv/env 三个约定（`--key=new`、无参 pipe、`TAILCAT_ADDR_FILE`），全部集中在 tailcat.go 一个文件；README 注明“建议 v0.7.0+”；P3 人工验收 pin v0.7.0 | 适配器单文件可替换 | `internal/pair/tailcat.go` 独立，revert 即回退到“tailcat 缺失”路径 |
| R4 | **USAGE/命令注册扰动基线**：`homer --help` 输出变化（e2e 冒烟只断言 contains "homer"，安全）；`homer pair` 从“未知命令”变为 dispatch（P0 stub 阶段行为 = 前置检查失败提示，语义合理）；readonly/exitcode 测试 P0 波次全量跑确认 | P0 独立 commit，可单独 revert | — |
| R5 | **B 端部分落盘中断**（备份/vault 已写、destination 写失败）：不自动回滚（备份在，报告列明路径+备份目录）；文档化恢复步骤。A 端 ack 未到 → peer-failed + recipients 保留 warning（B 公钥无害） | 测试钉死失败路径的输出 | — |
| R6 | **测试环境无真实 tailcat**：传输层全走注入；e2e 用假脚本（与 manifest fake Commands / gitx shim 同模式）；真实双机留人工清单（§7.2） | 任务既定策略 | — |

## 7. 验收标准

### 7.1 机器可查

1. `go build ./... && go vet ./... && go test ./...` 退出码 0；测试数 ≥ 515（基线 455 + 新增 ~60），**零既有测试修改性失败**。
2. `CGO_ENABLED=0 go build -trimpath ./cmd/homer` 成功；`go.mod` 零新增 require（`tailcat` 是外部可选二进制，非依赖）。
3. 冻结接口 §2 逐字编译一致（review 时 diff 核对签名）。
4. 各波次 §5 表内验收命令全绿。
5. `homer pair`（无 tailcat、无 config 等前置失败）全部 exit 1 且 stderr 含可操作提示；成功配对双方 exit 0。
6. README 含：pair 章节、tailcat 安装链接、DERP/derper 说明、安全模型六条、git 通道回退。

### 7.2 人工验收清单（真实双机，P3 写入 README/docs，PoC 已证库可行、CI 无双机）

1. 两台真机（不同网络/NAT，最好一个家宽一个办公网）各装 tailcat v0.7.0 + homer v1.3 release 二进制。
2. 机器 A（已有完整配置+密钥）：`homer pair` → 记录地址出现耗时（预期 <10s）。
3. 带外传递地址（当面/私密消息）；确认地址未出现在任何 git/聊天记录。
4. 机器 B（已 `homer home` + `homer secret keygen`）：`homer pair <tc-addr>` → A 端出现确认提示（核对 hostname 与 recipient 指纹）→ 确认。
5. 记录全程耗时与 DERP/direct 路径（另开终端 `tailcat ping <addr>` 观察 `via DERP(sfo)` / `via IP:port`）。
6. B 断言：每个 destination 文件 0600 且内容与 A 逐字节一致；`homer secret list` 显示 vault present；`homer doctor` age 项通过。
7. A 断言：homer.json `secrets.recipients` 含 B 的 recipient；state.json 有 `paired`；`homer doctor` age 项显示“已配对 1 台设备”。
8. 中断用例：A 在等待连接时 Ctrl-C → `pgrep -af tailcat` 无残留、地址文件已删；A 在确认提示选否 → 双方 exit 1、A 的 homer.json 未变。
9. 回归用例：B 再跑 `homer secret push`（recipients 已含 B，A pull 可解）→ git 通道与 pair 通道互不干扰。

## 8. 完成报告格式

`docs/v13-report.md`：波次结果、测试数变化（455 → N）、冻结接口 diff 核对结论、人工验收清单执行情况（留待真机）、对抗 review 修复记录。

---

**verdict**
