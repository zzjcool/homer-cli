# 冲突解决状态化（分阶段提交）实施计划

> 我只有只读工具，没有生成计划文件。本回复正文就是计划，建议调度方落盘到 `.pi-subagents/runs/<本次 run>/staged-resolution-plan.md`。

## 1. 目标与不做的事

**目标**：把「解决冲突」从立即执行改成持久化决定（暂存）。决定存在机器侧 `~/.homer/resolutions.json`，由单机「下发」消费：`center` 改写为中心内容，`local` 跳过写机器并把本机该 adapter 内容发布到中心。执行成功就清除，失败保留。保留「记录并立即执行」作为 fallback。

**Non-goals**
- 不做逐文件粒度，v1 只到 adapter 级。
- 不改 collect 的冲突门禁，有冲突的 adapter 仍不可收取。
- 不做 CLI（`homer merge/pull`）消费。
- `local` 消费后不向其他机器 fan-out。
- 不做「adapter 内容是否变过」的精细过期判定，只比较世代号。
- 不改 `Executor` 接口，不升 `stream.ProtocolVersion`，不改 `/api/snapshot`（它本来就没有 CAS）。
- 不修复 `preferRemotePlan` 对 excludeKeys 的处理，只补特征测试。

### 计划内默认决策（需求没写明，用户可否决）

| # | 决策 | 否决时的退路 |
|---|---|---|
| D1 | **过期**：`Stale = (generationAtRecord != 消费时 hub 当前世代)`。过期决定不应用、不删除，对应 adapter 仍视为未解决（不可勾选）。世代未知（0）时 fail-safe 视为过期。 | 让 `resolutions.Stale()` 恒返回 false，即退化为「有记录即视为已解决」，一行改动 |
| D2 | **重复记录**：同 adapter 覆盖（last-write-wins），刷新 `recordedAt` 和 `generationAtRecord`，并返回被覆盖的旧条目。不同 adapter 互不影响。 | — |
| D3 | **消费触发**：只有 `POST /api/agents/{id}/pull`（单机下发）、adapters 显式、且存在有效决定时才消费。fan-out、收取、旧 resolve、未显式 adapters 的 pull 永不消费。 | — |
| D4 | **local 消费**：该 adapter 不进入 RunPull（不写机器、不写基线）。先 pull，成功后再用 scoped push + overwrite 发布。 | — |
| D5 | **记录前置**：只允许对「此刻有冲突」的 adapter 记录。无冲突者跳过，全部无冲突则 422。 | — |
| D6 | 有效但已无冲突（moot）的决定，消费时按 no-op 清除。 | — |
| D7 | `local` 发布被密钥扫描拦下：决定保留并报错，引导走「记录并立即执行」里已有的「仍然以这台机器为准」确认。 | — |
| D8 | 卡片计数 = 「adapter 当前有冲突的已记录决定」数，不区分是否过期。过期细节在下发弹窗里显示。 | — |
| D9 | `resolve-record` 不占写门（走独立读通道加文件互斥）。写后手动 `bumpWriteGen + forgetDrift + signalHeartbeat`。 | — |

## 2. 冻结接口

### 2.1 文件格式（用户已冻结，字段不增不减）
```json
{"version":1,"entries":[{"adapter":"pi","choice":"center","recordedAt":"2026-10-20T08:00:00Z","generationAtRecord":7}]}
```
- 权限 0600，写入用临时文件加 rename 原子替换，条目按 adapter 排序。
- 读到未知 version 时拒绝写入。JSON 损坏时读路径视为空并告警，record 路径先把坏文件改名为 `resolutions.json.corrupt-<unix>` 再写新文件。

### 2.2 `internal/resolutions`（新包，只依赖 `core`）
```go
// types.go — S0
package resolutions

const (
	FileName = "resolutions.json"; FileVersion = 1
	ChoiceCenter = "center"; ChoiceLocal = "local"
	ActionRecord = "record"; ActionClear = "clear"; ActionList = "list"
	OutcomeApplied = "applied"; OutcomePublished = "published"; OutcomeNoop = "noop"
	OutcomeStale = "stale"; OutcomeFailed = "failed"
)
type Entry struct {
	Adapter            string `json:"adapter"`
	Choice             string `json:"choice"`
	RecordedAt         string `json:"recordedAt"`
	GenerationAtRecord int    `json:"generationAtRecord"`
}
type File struct { Version int `json:"version"`; Entries []Entry `json:"entries"` }
type Outcome struct {
	Adapter string `json:"adapter"`; Choice string `json:"choice"`
	Status  string `json:"status"`;  Message string `json:"message,omitempty"`
}
type Report struct { // resolve-record 任务的返回体
	OK bool `json:"ok"`; Action string `json:"action"`
	Entries  []Entry  `json:"entries"`            // 操作后的全部条目
	Recorded []Entry  `json:"recorded,omitempty"`
	Replaced []Entry  `json:"replaced,omitempty"` // 被覆盖的旧条目
	Removed  []Entry  `json:"removed,omitempty"`
	Pending  int      `json:"pending"`
	Warnings []string `json:"warnings,omitempty"`
	Errors   []string `json:"errors"`
}
var ErrCorrupt, ErrUnsupportedVersion error

// store.go — S1
func Path(paths core.HomerPaths) string                       // Join(paths.Home, FileName)，路径字面量只在本包出现
func ValidChoice(choice string) bool
func (e Entry) Validate() error                               // core.ValidAdapterID + 合法 choice + gen>=1 + RFC3339
func (e Entry) Same(o Entry) bool                             // 四字段全等
func Stale(e Entry, centerGeneration int) bool                // centerGeneration<1 || e.GenerationAtRecord != centerGeneration
func Index(entries []Entry) map[string]Entry
func Load(paths core.HomerPaths) (File, error)                // 文件不存在 → 空 File,nil；丢弃非法条目；重复 adapter 取最后一条
func Save(paths core.HomerPaths, file File) error             // O_EXCL 临时文件 → Chmod 0600 → fsync → rename
type RecordResult struct{ Recorded, Replaced []Entry }
func Record(paths core.HomerPaths, choice string, adapters []string, generation int, now time.Time) (RecordResult, error)
func Clear(paths core.HomerPaths, adapters []string) ([]Entry, error)         // 按 adapter 无条件删除
func RemoveIfSame(paths core.HomerPaths, consumed []Entry) ([]Entry, error)   // CAS：仅当文件里的条目仍与 consumed 全等才删
```
所有读改写共用包级 `sync.Mutex`。多进程同时写不加锁，属于已接受风险（见 R7）。

### 2.3 `internal/cli/commands`
```go
// pull.go — S0 加类型，S2 加行为
type PullOptions struct { /* 既有字段 */ PreferRemoteAdapters []string } // 仅这些 adapter 的冲突改写为中心内容；PreferRemote=true 时后者优先
type PullReport  struct { /* 既有字段 */ Resolutions []resolutions.Outcome `json:"resolutions,omitempty"` }
func NewPullReport(status PullStatus) PullReport   // 导出 newPullCommandReport，切片非 nil

// scope.go — S2
func preferRemotePlanFor(plan syncx.PullPlan, remote []core.AdapterSnapshot, adapterIDs []string) syncx.PullPlan
func preferRemotePlan(plan syncx.PullPlan, remote []core.AdapterSnapshot) syncx.PullPlan // 签名不变，委托给「全部 adapter」版本
```
RunPull 的改写点和现有 `PreferRemote` 完全相同：在 `PlanPull` 之后、`SplitManifestActions` 之前。

### 2.4 `internal/hub`
```go
// task.go / proto.go — S0
const TaskKindResolveRecord TaskKind = "resolve-record"
const MethodResolveRecord = string(TaskKindResolveRecord)
// TaskOptions 追加（全部 omitempty）。禁止复用既有 Resolve 字段：
// runTaskCommand 对 Resolve=local|center 不看 method，直接走旧 executorResolve。
ResolutionAction string `json:"resolutionAction,omitempty"` // record|clear|list
ResolutionChoice string `json:"resolutionChoice,omitempty"` // center|local
CenterGeneration int    `json:"centerGeneration,omitempty"` // hub 构造任务时的中心世代；0 = 未知
ApplyResolutions bool   `json:"applyResolutions,omitempty"` // 仅 pull：消费 Adapters 内的已记录决定
ClearResolutions bool   `json:"clearResolutions,omitempty"` // push/pull 成功后清除 Adapters 的决定（立即执行 fallback）

// registry.go — AgentDrift 加字段 S0；方法 S3a
type AgentDrift struct { /* 既有 */ Resolutions int `json:"resolutions,omitempty"` }
func (r *Registry) NoteResolutions(agentID string, pending int) // 仅当 Drift != nil 时更新

// dispatcher.go — S3a
func (d *Dispatcher) AgentResolveRecord(ctx context.Context, agentID string, req web.ResolveRecordRequest) (json.RawMessage, error)
func taskOptionsForScope(confirm bool, scope web.SyncScope) TaskOptions // 追加复制 ApplyResolutions/ClearResolutions/CenterGeneration
var _ web.ResolutionSource = (*Dispatcher)(nil)
```
- `AgentResolveRecord` 先检查 `info.caps`。没有 `resolve-record` 能力时返回 `newAgentError("agent-outdated", 409, ...)`，不走通用的 502 `unsupported`。
- 成功后解析 `Report.Pending`，调用 `Registry.NoteResolutions`。
- `Dispatcher.ListAgents` 复制 `Drift.Resolutions`。

### 2.5 `internal/web`
```go
// server.go — S0
type AgentDrift struct { /* 既有 */
### 2.5 `internal/web`（续，自 `AgentDrift` 起）

```go
// server.go — S0
type AgentDrift struct {
	Push        int    `json:"push"`
	Pull        int    `json:"pull"`
	Conflicts   int    `json:"conflicts"`
	Error       string `json:"error,omitempty"`
	Resolutions int    `json:"resolutions,omitempty"` // 有冲突且已记录决定的 adapter 数；0 不输出
}
type ResolutionSource interface { // 可选能力，类型断言发现（同 InspectSource）；AgentsSource 不改
	AgentResolveRecord(ctx context.Context, agentID string, req ResolveRecordRequest) (json.RawMessage, error)
}
type ResolveRecordRequest struct {
	Action           string   // resolutions.ActionRecord | ActionClear | ActionList
	Choice           string   // record 必填
	Adapters         []string // record/clear 必填，已 syncx.ParseAdapterIDs 规范化
	CenterGeneration int      // record 必填（>=1），只由 hub 从 gens 读取
}

// scope.go — S0。三项只由 hub 内部赋值；readSyncScope 不解析，请求体里的同名字段被忽略
type SyncScope struct { /* 既有 */
	ApplyResolutions bool
	ClearResolutions bool
	CenterGeneration int
}

// choices.go — S3a
const reasonStale = "已记录的决定已过期（中心有新内容），需要重新选择"
type ResolutionView struct {
	Choice             string `json:"choice"`
	RecordedAt         string `json:"recordedAt"`
	GenerationAtRecord int    `json:"generationAtRecord"`
	Stale              bool   `json:"stale"`
}
// AdapterChoice 追加字段：Resolution *ResolutionView `json:"resolution,omitempty"`
func ResolutionViews(entries []resolutions.Entry, centerGeneration int) map[string]ResolutionView
func BuildDispatchChoicesWithResolutions(centerIDs []string, machine []commands.StatusAdapterReport, views map[string]ResolutionView) []AdapterChoice
func BuildResolveChoicesWithResolutions(adapters []commands.StatusAdapterReport, views map[string]ResolutionView) []AdapterChoice
// BuildDispatchChoices / BuildResolveChoices 签名与行为不变，委托上面两个（views=nil），现有测试零改动

// resolve_record.go — S3a（新文件）
type resolveRecordSkip struct {
	Adapter string `json:"adapter"`
	Reason  string `json:"reason"`
}
type resolveRecordReport struct {
	OK       bool                `json:"ok"`
	Status   string              `json:"status"` // recorded|cleared|aborted|nothing-to-record|error
	AgentID  string              `json:"agentId"`
	Choice   string              `json:"choice,omitempty"`
	Recorded []resolutions.Entry `json:"recorded"`
	Replaced []resolutions.Entry `json:"replaced"`
	Skipped  []resolveRecordSkip `json:"skipped"`
	Pending  int                 `json:"pending"`
	Errors   []string            `json:"errors"`
}
func (s *Server) handleResolveRecord(w http.ResponseWriter, r *http.Request) // POST=记录，GET=列出（诊断/测试用，控制台不用）
func (s *Server) handleResolveClear(w http.ResponseWriter, r *http.Request)
func (s *Server) centerGeneration() int // gens 当前世代；无快照=0

// resolve.go — S3a：resolveReport 追加
//   Recorded []string `json:"recorded,omitempty"`  // 本次记录的 adapter
//   Pending  []string `json:"pending,omitempty"`   // 立即执行失败后仍保留决定的 adapter
// resolveOnMachine 读取 ?record=true
```

**HTTP 契约（冻结）**

| 路由 | 参数 / body | 成功 | 失败 |
|---|---|---|---|
| `POST /api/resolve/record` | `agent`、`choice=center\|local`、`confirm=true`；body `{adapters}`（必须显式） | 200 `status:"recorded"` | 400 参数；409 `aborted`/`no-snapshot`/`agent-outdated`；422 `nothing-to-record`；501 `agents-disabled`；其余走 `writeErrorValue` |
| `POST /api/resolve/clear` | `agent`、`confirm=true`；body `{adapters}` | 200 `status:"cleared"` | 同上 |
| `GET /api/resolve/record` | `agent` | 200 `{ok,entries[含 stale],centerGeneration}` | 400/501/5xx |
| `POST /api/resolve`（既有） | 追加 `record=true` | 既有字段 + `recorded`/`pending` | 既有 + 409 `agent-outdated` |
| `POST /api/agents/{id}/pull`（既有） | 不变 | 响应多 `resolutions[]` | 既有 |

规则：
1. **D5 过滤只在 `/api/resolve/record`**：先 `AgentStatus` 取有冲突的 adapter 集合，其余进 `skipped`。`record=true` 的立即执行路径不过滤，和旧行为一致。
2. `record=true` 要求 `scope.Explicit`，否则 400。顺序是：`requireCenterAdapters`、`refuseBoundKeyGap`、`dispatchUnlockErrors` 全部通过，再记录，再 relay。记录失败则不 relay，relay 失败不回滚记录。relay 带 `ClearResolutions=true`，成功后由 agent CAS 清除。
3. `handleAgentPull` 在 `confirmValue(r) && scope.Explicit` 时置 `ApplyResolutions=true`、`CenterGeneration=s.centerGeneration()`。fan-out、收取、旧 resolve、预览（confirm=false）不置位（D3）。
4. `errors.go`：`errorMessage("agent-outdated")` 返回「这台机器的 homer 版本太旧，不能记录决定，请先更新程序」。`agentStatusForCode` 加 409。
5. `Registry.NoteWriteOutcome` 在 `conflicts` 分支保留既有 `Resolutions`。`handleConsole` 的 `machineView.Drift` 复制 `Resolutions`。
6. `buildSyncChoicesResponse`：dispatch/resolve 用 `ResolutionViews(report.Resolutions, s.centerGeneration())`。resolve 的 hint **在原句之后追加**「每个适配器各选一边：只记录决定，下发时再执行；或记录并立即执行。」，现有断言保持绿。

dispatch 行规则：
- 有冲突且决定有效：`Enabled=true`、`Checked=true`、`Reason=""`，`Detail` 追加「已记录：以中心为准/以这台机器为准」。
- 有冲突且决定过期：`Enabled=false`、`Reason=reasonStale`，`Resolution` 保留。
- 无冲突：忽略决定（D6，消费时 noop 清除）。

### 2.6 前端 `internal/web/static/index.html`（S3b）

```js
// 函数名与文案不得含子串 "commit"/"bare"，也不得含 TestConsoleUserCopy 禁词（推送/拉取/仓库…）
function confirmResolve(agent, preset)            // preset: "center"|"local"|undefined。OK=「记录决定」，ALT=「记录并立即执行」
function confirmResolveOn(choice, agent)          // 薄封装 → confirmResolve(agent, choice)；presentWriteFailure 的两个按钮继续用它
function selectedPicks()                          // → {center:[], local:[], missing:[]}，只看勾选行
function resolutionOf(adapterId)                  // app.scopeChoices 行上的 resolution || null
function validDecisionCount()                     // dispatch：勾选行中 resolution && !stale 的个数
async function executeRecordResolve(agentId, name, picks, immediate, unlocks, allowSecrets)
async function executeResolveOn(choice, agentId, adapters, allowSecrets, unlocks, record) // 追加 record；重试闭包透传
async function clearResolution(agentId, adapterId)   // POST /api/resolve/clear
function resolutionOutcomeLines(data)             // data.resolutions → {done:[], failed:[]}
```

行为：
- **处理冲突弹窗**
  - 每个有冲突的行带单选「以中心为准 / 以这台机器为准」。预选顺序：`preset`，其次该行已有 `resolution.choice`（含过期的），否则不选。
  - 工具行加「全部以中心为准」「全部以这台机器为准」。
  - 有 `resolution` 的行显示徽标「已记录：…」（过期则显示「已记录的决定已过期…」）和「撤销决定」按钮。
  - 两个按钮在「已勾选≥1 且 `missing` 为空且非加载中」时可点。
- **记录决定**：按 choice 分组，最多两次 `POST /api/resolve/record`。成功后 toast「已记录 N 条决定，下发到「name」时执行」并 `refresh()`。记录本身不需要口令，也不受密钥可达性阻塞。
- **记录并立即执行**
  - 按 choice 分组串行，调用 `executeResolveOn(..., record=true)`。
  - 只有 center 组且绑定密钥时才进口令步。`dispatchKeysForSelection`、`boundPathsForSelection`、`blockedByKeys`、`selectionToken` 在 resolve 方向只看 center 组。
  - 口令步隐藏 ALT 按钮，返回选择步时按 `app.scopeAltLabel` 恢复。
  - 中途失败即停：已成功的组已执行，失败组的决定保留。
  - 响应 `!ok && pending.length` 时弹「决定已记录，立即执行没有完成」，提示「决定已保留，可稍后下发时再执行」。
  - 409 `agent-outdated` 时弹提示，并给「直接执行（不记录）」按钮，调用 `executeResolveOn(..., record=false)`。
- **下发弹窗**
  - 在 `#confirm-note` 后新增 `#confirm-resolutions`，由 `updateScopeConfirm()` 刷新。`validDecisionCount()>0` 时显示「将应用 N 条已解决的决定」，并逐条列出「pi：以中心为准」「herdr：以这台机器为准（不改这台机器，把它的内容写入中心）」。
  - 过期行显示 `reason`，并带「重新决定」按钮：关闭本弹窗，再 `confirmResolve(agent, resolution.choice)`。
  - 有效决定的行隐藏行内即时按钮。无决定的冲突行保留现有两个即时按钮（fallback，不改）。
  - 完成页追加 `resolutionOutcomeLines` 的 done 行。failed 行并入 `confirm-err`。
- **机器卡片**：`a.drift.resolutions>0` 时在计数旁加 `el("span","counts c-resolved","待执行决定 "+n)`，CSS 加 `.agent-head .c-resolved`。「处理冲突」按钮的显示条件不变（`conflicts>0`），冲突计数照实显示（冲突确实还没执行）。
- **copy_test 必须新增的 required**：`记录决定`、`记录并立即执行`、`待执行决定`、`条已解决的决定`、`/api/resolve/record`、`/api/resolve/clear`、`撤销决定`、`已记录`。原有 required（`以这台机器为准`、`以中心为准`、`/api/resolve?` 等）继续满足。

### 2.7 `internal/agentd`

```go
// resolutions.go — S2b（新文件）
func (d *Daemon) resolutionPaths() core.HomerPaths // HOMER_HOME=d.cfg.HomerHome，与 executor 同款闭包
func (d *Daemon) runResolveRecord(ctx context.Context, options hub.TaskOptions) (resolutions.Report, error)
// 只依赖 d.cfg.HomerHome，不依赖 executor 类型，fake executor 的测试也能跑

type clearGuard struct {
	paths    core.HomerPaths
	consumed []resolutions.Entry
}
func (d *Daemon) beginClear(options hub.TaskOptions) clearGuard // ClearResolutions && Adapters 非空：任务开始时快照这些 adapter 的条目
func (g clearGuard) finish(ok bool) []string                    // ok → RemoveIfSame(consumed)；失败只返回 warning，不让任务失败

type ResolvedPullRequest struct {
	Confirm          bool
	Adapters         []string // 显式、必填
	CenterGeneration int
	AllowSecrets     bool
}
func (e *localExecutor) PullApplyingResolutions(ctx context.Context, req ResolvedPullRequest) (commands.PullReport, error)
func (d *Daemon) executorPullResolved(ctx context.Context, req ResolvedPullRequest) (commands.PullReport, error) // 同 executorResolve：类型断言 *localExecutor，否则报错

// executor.go — S2b。Executor 接口一字不改
func (e *localExecutor) pull(ctx context.Context, confirm bool, adapters []string, preferRemote bool, preferRemoteAdapters []string) (commands.PullReport, error)
// Pull(ctx, confirm, adapters, preferRemote) 委托 pull(..., nil)
func (e *localExecutor) attachResolutions(report *commands.StatusReport) // Status 成功后调用；读失败只加 warning（fail-open）；缺 homer.json 的降级分支不挂

// agentd.go — S2b
func countEffectiveResolutions(report commands.StatusReport) int // entries 中 adapter 在 report 里 Conflicts>0 的个数
// driftFromStatus 签名不变，内部追加 drift.Resolutions = countEffectiveResolutions(report)

// handlers.go / client.go / taskclass.go — S2b
// registerHandlers 方法表、hello().Caps 各追加 string(hub.TaskKindResolveRecord)
// classifyTask：resolve-record → taskClassRead（D9，不占写门）
// runTaskCommand 新增分支：
//   case string(hub.TaskKindResolveRecord):
//       report, err := d.runResolveRecord(ctx, options)
//       if err == nil && report.OK && options.ResolutionAction != resolutions.ActionList {
//           d.bumpWriteGen(); d.forgetDrift(); d.signalHeartbeat()
//       }
//       return report, err
//   push：guard := d.beginClear(options)；执行；err==nil 时 report.Warnings 追加 guard.finish(report.OK)
//   pull：options.ApplyResolutions → executorPullResolved；否则同 push 的 guard 流程（ClearResolutions 立即执行路径）
```

**对 D4 的修订（以本节为准）**：`local` 组改为先于 pull 发布。原因：pull 阶段可能装插件，耗时数分钟，会把「检查世代 → 覆盖中心」的 TOCTOU 窗口拉长。`local` 组的 adapter 本来就不进 pull，发布先行不依赖 pull 成败，且窗口缩到秒级。

`PullApplyingResolutions` 步骤（冻结）：
1. `!Confirm`、`hubURL==""`、`Adapters` 为空、`Load` 后选中 adapter 无条目：直接走旧 `Pull`，结果与现状逐字节一致（`Load` 出错也走旧路径，加 warning）。
2. 对选中条目分类。`resolutions.Stale(e, req.CenterGeneration)` 为真则 outcome=`stale`，该 adapter 留在普通 pull 列表里，冲突照旧暴露。有效的 center 进 `centerIDs`，有效的 local 进 `localIDs`。同时快照条目用于最后的 CAS。
3. `localIDs` 非空：`e.Push(ctx,true,localIDs,true,req.AllowSecrets)`（scoped + overwrite）。Pushed→`published`；NoDrift→`noop`（D6）；`secrets-rejected`→`failed`（文案：「pi 没有写入中心：发现疑似密钥，决定已保留。到「处理冲突」选「记录并立即执行」并确认密钥提示」，D7）；其余→`failed`。
4. 余下 adapter 走 `e.pull(ctx,true,rest,false,centerIDs)`。余下为空则合成 `commands.NewPullReport(PullStatusNoDrift)`。
5. center outcome：`Applied` 里有该 adapter 的 written/deleted→`applied`；无变化→`noop`；pull 状态为 error/aborted，或该 adapter 仍在 `Conflicts` 里→`failed`。
6. 对 outcome ∈ {applied, published, noop} 的快照条目执行 `RemoveIfSame`（逐 adapter，不是整任务一刀切）。失败只记 warning，条目残留，下次消费按 noop 清除。
7. `report.Resolutions=outcomes`。有 `failed` 时：OK 原为 true 则置 OK=false、Status=`error`（`conflicts-remain` 保持不变），`Errors` 追加中文句，句内含「决定已保留」。
8. Daemon 层用 `d.logf` 记 adapter/choice/generation/status，不含文件内容。

`Pending = len(Entries)`（操作后）。hub 的 `NoteResolutions` 是乐观更新，下一次心跳的 `countEffectiveResolutions` 为准。

### 2.8 测试清单

**`internal/resolutions/resolutions_test.go`（S1，`-race`）**
- `TestSaveLoadRoundTrip`：精确 JSON 形状 `{"version":1,"entries":[…]}`、字段名、按 adapter 排序。
- `TestSaveMode0600AndAtomic`：`Perm()==0600`；预存 0644 文件被替换后是 0600；无 `.tmp-*` 残留。
- `TestLoadMissingIsEmpty`、`TestLoadCorruptReturnsErrCorrupt`、`TestRecordQuarantinesCorruptFile`（改名 `.corrupt-<unix>` 后写新文件）、`TestRecordRefusesUnsupportedVersion`（文件字节不变）。
- `TestLoadDropsInvalidEntries`：非法 adapter id / choice / gen<1 / 非 RFC3339 被丢弃；重复 adapter 取最后一条。
- `TestRecordOverwritesSameAdapter`（D2）：返回 `Replaced`，`recordedAt`/gen 刷新，其他 adapter 不动。`TestRecordRejectsInvalid`：空 adapters、坏 choice、gen<1、`../x`。
- `TestClear*`、`TestRemoveIfSameCAS`（被重新记录的条目不删）、`TestEmptyFileRemoved`（条目清空即删文件）、`TestStaleTable`（相等/不等/center=0/entry gen=0）、`TestConcurrentRecord`（50 goroutine）。

**`internal/cli/commands`（S2a）**
- `scope_test.go`：`TestPreferRemotePlanFor_SubsetOnly`、`_NilMeansAll_EmptyMeansNone`、`_RemoteMissingBecomesDelete`、`TestPreferRemotePlan_CharacterizationUnchanged`（含 excludeKeys 现状）。
- `pull_test.go`：`TestRunPull_PreferRemoteAdapters_OnlyListedRewritten`（另一 adapter 仍 `conflicts-remain`）、`_PreferRemoteBeatsAdapters`、`_NoOptionUnchanged`、`TestNewPullReport_NonNilSlices`。
- `status_test.go`：`TestStatusReportOmitsResolutionsWhenEmpty`。

**`internal/agentd`（S2b，`-race`）**
- `resolutions_test.go`：
  - `TestRunResolveRecord_RecordListClear`：经 `runTaskCommand`，临时 HomerHome，文件 0600，gen 取自 `CenterGeneration`。
  - `TestResolveRecordNotBlockedByWriteGate`：写门被占时 1s 内完成（D9）。
  - `TestResolveRecordInvalidatesDriftAndStatusFlight`：writeGen 增加、drift 被 forget、`heartbeatWake` 有信号。
  - `TestHelloAdvertisesResolveRecord`、`TestHandlersRegisterResolveRecord`。
  - `TestCountEffectiveResolutions`、`TestDriftFromStatusIncludesResolutions`。
  - `TestStatusEmbedsResolutions`、`TestStatusResolutionLoadErrorIsWarning`。
- `executor_resolutions_test.go`（httptest hub + 临时 HOME，真实构造冲突）：
  - `TestPullApplyingResolutions_CenterRewritesAndClears`：本地文件=中心内容，有备份，条目清除，outcome=`applied`。
  - `…_LocalPublishesAndSkipsMachine`：本机文件字节不变；hub 收到 `adapters:["pi"]` 的 POST；条目清除；outcome=`published`。
  - `…_StaleNotApplied`：不写、条目保留、`conflicts-remain`、outcome=`stale`。
  - `…_PullErrorKeepsEntry`、`…_LocalUploadFailureKeepsEntry`（其余 adapter 仍已应用，OK=false）、`…_LocalSecretsRejectedKeepsEntry`（D7）。
  - `…_NoEntriesEqualsLegacyPull`、`…_MootClearedAsNoop`（D6）、`…_OnlyConsumesSelected`、`…_CASKeepsReRecorded`、`…_ConcurrentRecordSerialized`、`…_PublishBeforePull`（调用顺序）。
  - `TestClearOnSuccess_CenterImmediate`/`_Failure`/`_LocalImmediate`。
- 所有 fake executor 既有测试零改动即可编译；已有 hello caps 断言同步更新。

**`internal/hub`（S0/S3a）**
- `task_test.go`：新字段往返；零值 `TaskOptions` 序列化仍为 `{}`。
- `dispatcher_test.go`：
  - `TestAgentResolveRecordSendsTaskAndNotesPending`
  - `…OutdatedAgent`（caps 缺失→409 `agent-outdated`，不发流请求）
  - `…OfflineAgentIs503First`
  - `TestTaskOptionsForScopeCopiesResolutionFlags`
- `registry_test.go`：`TestNoteResolutionsOnlyWhenDriftPresent`、`TestNoteWriteOutcomeKeepsResolutionsOnConflicts`、`TestHeartbeatCarriesResolutions`、`TestListAgentsCopiesResolutions`。

**`internal/web`（S3a/S3b）**
- `resolve_record_test.go`：
  - 只记录不执行：`AgentPush/AgentPull` 零调用；`CenterGeneration==gens head`。
  - 错误路径：未确认→409；坏 choice / 无 adapters→400；无快照→409；无 `ResolutionSource`→501；老 agent→409。
  - D5：非冲突 adapter 进 `skipped`，全部非冲突→422。
  - `TestClientCannotForgeGeneration`。
  - `TestResolveWithRecordTrue_RecordsThenRelaysWithClear`：顺序、`ClearResolutions`、失败时 `pending` 且不回滚记录。
  - `TestResolveWithoutRecord_Unchanged`、`TestResolveRecordAfterValidationOrder`。
  - `TestResolveClear*`、`TestResolveRecordListGET`。
- `sync_test.go`：`TestAgentPullExplicitConfirmSetsApplyFlags`、`…NoAdaptersDoesNotApply`、`…PreviewDoesNotApply`、`TestFanoutDispatchNeverApplies`、`TestCollectAndLegacyResolveNeverApply`。
- `choices_resolution_test.go`：有效决定→启用并默认勾选；过期→禁用并带 `reasonStale`；无决定行为不变；`TestResolutionViewsStale`；`TestChoicesResponseCarriesResolution`。
- `web_test.go`：`/api/agents`、`/api/console` 带 `drift.resolutions`。
- `gate_matrix_test.go`：新增两条路由的行；各路由方法错误→405。
- `errors_test.go`：`agent-outdated` 文案与状态码。
- `copy_test.go`：新增 required（见 2.6）。新增 `TestIndexInlineScriptParses`：抽取内联 `<script>` 跑 `node --check`，无 node 则 skip。

**`tests/e2e/staged_resolution_test.go`（S3b，真 hub + 真 agent 走 WS，沿用 `hub_agents_test.go` 脚手架）**
1. 记录 center：本机文件和 HOME 不变，`resolutions.json` 为 0600，且 `generationAtRecord==HEAD`。状态仍显示冲突，choices 中该 adapter 可勾选。下发后文件=中心，条目清除，状态干净。
2. 记录 local→下发：本机文件不变，中心世代 +1 且内容=本机，条目清除。
3. 过期：记录后另一台机器收取使世代 +1。choices 禁用该 adapter；强制下发得到 `conflicts-remain`，条目保留。
4. 立即执行：`record=true` 执行且条目清除；不带 `record` 不产生文件。
5. 老 agent（无 cap）：409 `agent-outdated`。
6. 重启 agent 后条目仍在，仍可消费。

另沿用现有 `*_ui_test.go` 驱动方式新增 `staged_resolution_ui_test.go`，覆盖两个按钮、下发弹窗「将应用 N 条已解决的决定」、卡片 chip。无浏览器环境自带 skip。同时 `grep -rn 'btn-confirm-alt\|confirmResolve\|处理冲突' tests internal/web/*_test.go`，修正受旧按钮文案影响的断言。

## 3. 分阶段步骤

**依赖与并行**：S0 单 writer 先行。S0 合并后，S1 ∥ S2a ∥ S3a ∥ S3b-前端 各用独立 worktree（`staged-res/<lane>`）并行。S2b 依赖 S1+S2a。S3b-e2e/文档/评审门依赖全部合并。

**S0 骨架（串行，先行）**
- 文件：
  - `internal/resolutions/types.go`（类型、常量、错误）。
  - `internal/resolutions/store.go`：纯函数（`ValidChoice/Entry.Validate/Entry.Same/Stale/Index`）为终稿；IO 函数（`Path/Load/Save/Record/Clear/RemoveIfSame`）只放桩，返回 `errors.New("resolutions: not implemented")`。
  - `hub/task.go`、`hub/proto.go`（`TaskKindResolveRecord`、`MethodResolveRecord`、5 个 TaskOptions 字段）。
  - `hub/registry.go`（`AgentDrift.Resolutions` 字段）。
  - `web/server.go`、`web/scope.go`（2.5 的类型与字段）。
  - `cli/commands/pull.go`（`PreferRemoteAdapters`、`PullReport.Resolutions`、`NewPullReport`，仅类型）。
  - `cli/commands/status.go`（`StatusReport.Resolutions`）。
- 前置核查：`grep -rn DisallowUnknownFields internal/` 在 hello、心跳、任务解码路径上必须无命中。有命中则先放宽，否则新版 agent 的 `resolutions` 字段会被老 hub 拒收。
- 验收：
  - `go build ./... && go vet ./...` 退出码 0。
  - `go test ./internal/... -count=1` 全绿。
  - `git diff --name-status <base> -- '*_test.go' | grep -v '^A'` 无输出（既有测试零修改，只新增）。
  - 新增零值兼容测试通过：`TaskOptions{}`→`{}`、`AgentDrift` 与 `StatusReport` 零值不输出新键。
- 回滚点：整体 revert，纯加法。

**S1 resolutions 存储（lane A）**
- 文件：`internal/resolutions/store.go`（填实）、`resolutions_test.go`、`internal/gitx/git.go`（`requiredGitignoreLines` 追加 `resolutions.json`）、`gitx/git_test.go`（两处期望值：L104 的 required 列表、L131 的 want 串）。
- 依赖：S0。
- 验收：
  - `go test ./internal/resolutions ./internal/gitx -race -count=1` 全绿。
  - `go test -coverprofile=/tmp/res.cov ./internal/resolutions && go tool cover -func=/tmp/res.cov | awk '/total:/{gsub("%","",$3); exit !($3>=90)}'` 退出码 0。
  - `grep -rn '"resolutions.json"' internal --include='*.go' | grep -v '_test.go' | grep -v '^internal/resolutions/' | grep -v '^internal/gitx/git.go'` 无输出。

**S2a commands 层（lane B）**
- 文件：`cli/commands/scope.go`（`preferRemotePlanFor`，`preferRemotePlan` 委托 nil=全部，非 nil 空切片=不改写）、`pull.go`（`RunPull` 在 `PlanPull` 之后、`SplitManifestActions` 之前：`PreferRemote` 优先，否则 `len(PreferRemoteAdapters)>0` 时用 `preferRemotePlanFor`）、对应测试。
- 依赖：S0。
- 验收：
  - `go test ./internal/cli/... -count=1` 全绿。
  - `grep -n 'func preferRemotePlan(' internal/cli/commands/scope.go` 的签名与改前一致。
  - `go test ./internal/cli/commands -run 'PreferRemote|NewPullReport|StatusReport' -count=1` 通过。

**S2b agentd（依赖 S1+S2a）**
- 文件：`agentd/resolutions.go`（新）、`executor.go`、`agentd.go`、`handlers.go`、`taskclass.go`、`client.go`、两个新测试文件，以及既有 caps 断言的同步修改。
- 依赖：S1、S2a。
- 验收：
  - `go test ./internal/agentd -race -count=1` 全绿。
  - `go test ./internal/agentd -run 'Resolution|ResolveRecord|ClearOnSuccess' -race -count=5` 稳定。
  - `grep -c 'resolve-record\|TaskKindResolveRecord' internal/agentd/client.go internal/agentd/handlers.go` 每个文件 ≥1。
  - `type Executor interface` 块零 diff。
  - `NoEntriesEqualsLegacyPull` 通过。
- 回滚点：revert 本步。hub 通过 caps 协商，老 agent 路径不受影响。

**S3a hub + web（lane C，依赖 S0，测试只用 fake）**
- 文件：`hub/dispatcher.go`（`AgentResolveRecord`：先 `requireOnline`，再查 cap→`agent-outdated`，再 `callTask`，成功后 `NoteResolutions`；`taskOptionsForScope` 复制三项；`ListAgents` 复制 `Resolutions`）、`hub/registry.go`（`NoteResolutions`、`NoteWriteOutcome` 保留 Resolutions）、`web/choices.go`、`web/resolve.go`、`web/resolve_record.go`、`web/handlers.go`（两条新路由、`handleAgentPull` 置位）、`web/sync.go`（`handleConsole`）、`web/errors.go`，及对应测试。
- 依赖：S0。
- 验收：
  - `go test ./internal/hub ./internal/web -race -count=1` 全绿。
  - `go test ./internal/web -run 'ResolveRecord|ResolveClear|Choices|ApplyFlags|NeverApply|Gate' -count=1` 通过。
  - 既有 `choices_test.go`/`resolve_test.go` 不改一行就通过。
  - `var _ web.ResolutionSource = (*Dispatcher)(nil)` 编译通过。
- 回滚点：撤掉 `handleAgentPull` 那 3 行，决定即变惰性，系统回到旧行为。

**S3b 前端、端到端、文档、收尾门**
- S3b-1（lane D，依赖 S0，可与 S3a 并行）：`index.html` 与 `copy_test.go`。验收：
  - `go test ./internal/web -run 'TestConsoleUserCopy|TestIndexInlineScriptParses' -count=1` 通过。
  - `grep -nE 'commit|bare' internal/web/static/index.html` 无输出。
- S3b-2（依赖 S2b+S3a+S3b-1）：`tests/e2e/staged_resolution_test.go`、`staged_resolution_ui_test.go`、受影响旧断言修正、`DESIGN.md` §2.5 增补第 6 条（hub 形态下先记录后执行）、`docs/plan/staged-resolution.md`。验收：
  - `go test ./tests/e2e -run StagedResolution -count=1 -timeout 5m` 通过。
  - `gofmt -l .` 无输出。
  - `go build ./... && go vet ./... && go test ./... -count=1 -timeout 20m` 全绿（docker/浏览器类用例无环境时自带 skip）。
- 收尾门：3 个 fresh-context 评审（正确性/回归、测试覆盖、简洁性）并行，P0/P1 清零后才算完成。

## 4. 风险点

**最易错的 3 处（含回滚点）**

| # | 风险 | 缓解 | 回滚点 |
|---|---|---|---|
| R1 | **记录与实际冲突不匹配**。D1 用全局世代：任何一次发布都会让所有未执行决定过期。例如消费一条 `local` 会使其他机器的决定全部过期，需重新确认；这是有意的保守（中心内容已变，盲覆盖不安全）。另外「决定是 adapter 级」，消费时作用于该 adapter 此刻的全部冲突；机器上冲突若消失后又出现，旧条目可能被复活。 | 过期条目只显示不执行，预选旧 choice，重新决定一键完成；消费前必须经下发弹窗，弹窗列出消费时刻的实时冲突文件，这是最后一道人工确认；D6 对 moot 条目在消费时清除；有效期判定 fail-safe（世代未知视为过期）。 | `handleAgentPull` 去掉置位即失效；`Stale` 恒 false 是更宽松的退路，不推荐 |
| R2 | **local 消费的覆盖面**。`/api/snapshot` 无 CAS：agent 下载快照到上传之间，别的机器的发布会被静默覆盖。 | 发布先行（D4 修订），窗口缩到秒级；hub 在发任务时读世代，agent 在消费时再比对；每次覆盖前有过期检查。后续可给 POST 加 `baseGeneration` CAS（超出本期）。 | 同上 |
| R3 | **持久化与并发**。写中断导致文件损坏；record 与消费并发。 | 临时文件 + `Chmod 0600` + fsync + rename；读到损坏视为空，record 前把坏文件改名 `.corrupt-<unix>`；未知 version 拒写；所有读改写共用进程内互斥；消费用「开始时快照 + 结束时 `RemoveIfSame` CAS」，期间被重新记录的条目会保留。 | 删除 `resolutions.json` 即清空全部决定，无其他副作用 |

**其他风险**

| # | 风险 | 对策 |
|---|---|---|
| R4 | **版本混跑**。新 hub+旧 agent：记录得 409 `agent-outdated`，下发不受影响。旧 hub+新 agent：hub 不置位，决定惰性，心跳里的 `resolutions` 被忽略。 | 靠 caps 协商；S0 前置核查 `DisallowUnknownFields`；部署顺序任意，先升 agent 最顺 |
| R5 | **重复记录**。D2：同 adapter 覆盖，刷新 `recordedAt` 和世代并返回被覆盖项。多标签页并发时后写者胜；立即执行路径靠 CAS 避免误删新决定。 | 已冻结，有测试 |
| R6 | **前端复杂度**。`updateScopeConfirm` 等函数交织，混合 choice、口令步、ALT 按钮复用；旧 UI e2e 可能断言旧按钮文案；`TestConsoleUserCopy` 禁词含子串 `commit`、`bare`、`推送`、`拉取`。 | 新标识符一律用 `record*/resolution*` 前缀；先跑 copy_test 再提交；S3b-2 统一 grep 修正旧断言；`TestIndexInlineScriptParses` 兜语法 |
| R7 | **密钥绑定**。center 决定对绑定密钥的 adapter 在下发时仍需口令；local 决定的 adapter 在下发弹窗里同样会被要求口令（hub 不知道哪些 adapter 是 local）。 | 记录本身不受阻；下发弹窗给出原因；已知限制，文档注明 |
| R8 | **写门与 D9**。`resolve-record` 走读通道，`readSem` 容量 4，被大量 status/inspect 占满时记录会排队，最长到 50s 超时。 | 可接受；超时返回既有 `agent-timeout` |
| R9 | **`preferRemotePlan` 对 excludeKeys 的既有缺陷**（可能把占位符带入写入内容）。center 消费复用同一机制，继承该问题。 | 只补特征测试，不在本期修复；评审门记录为已知项 |
| R10 | **多进程写**。CLI 目前没有任何命令写 `resolutions.json`，只有 agent 单实例写；进程内互斥足够。将来若 CLI 要写，需要加 flock。 | 文档注明 |
| R11 | **UI 语义**。有决定的 adapter 仍计入卡片冲突数（冲突确实未执行），用户可能误以为「还没处理」。 | 卡片同时显示「待执行决定 N」；`confirm-resolutions` 文案明确「下发时执行」 |

## 5. 总验收（机器可检查）

```
gofmt -l .                                   # 无输出
go build ./... && go vet ./...               # 退出码 0
go test ./... -count=1 -timeout 20m          # 全绿
go test ./internal/resolutions ./internal/agentd ./internal/hub ./internal/web -race -count=1
go test ./tests/e2e -run StagedResolution -count=1 -timeout 5m
```

```json
{"ok": true, "reason": "plan ready, 6 steps (S0,S1,S2a,S2b,S3a,S3b); 4 parallel lanes after S0; D4 revised to publish-first"}
```