# 插件体系改造契约（冻结 v1）

> 2026-10-11 全夜改造。本文档是所有并行 worker 的唯一接口。
> 发现契约矛盾 → 停止并在 verdict 中报告，禁止自行解释。
> 计划文档看不到 → 向父会话要内容，禁止新建同名文件。

## 目标

homer 的适配器/能力从「编译死的内置」重构为「插件」：

1. **单一注册源**：消灭 6 处散落注册点，官方插件目录成为唯一事实。
2. **用户可安装/卸载**：hub 上安装（写进中心配置+发布新世代），机器零改动。
3. **keyring 转正**：从隐形强制注入改为可见、可管理的 Carrier 插件。
4. **ssh-key 转正**：从硬编码按钮改为 Action 插件（UI 动作面板）。
5. **第三方路线**：manifest schemaVersion=1，官方插件与第三方插件同格式。

## 三类插件角色

| role | 含义 | 机器原语 |
|---|---|---|
| adapter | 配置同步作用域 | ScanAdapter 解释器（已存在，不改） |
| carrier | 随同步流走的横切能力 | secret 方法 + age（已存在，不改） |
| action | 一次性机器操作 | ssh-key 方法（已存在，不改） |

**机器端零改动是硬约束**：不改 internal/agentd、internal/hub、internal/stream、
internal/sync、internal/engine、internal/manifest、internal/core 的任何现有行为
（新增 plugin 目录只读父层 OK）。协议版本不升。

## 核心数据形状

```go
// internal/pluginregistry/pluginregistry.go（新包，唯一注册源）

type Plugin struct {
    ID   string `json:"id"`   // ^[a-z][a-z0-9-]*$（core.ValidAdapterID 复用）
    Role Role   `json:"role"` // adapter | carrier | action
    Name string `json:"name"` // 展示名（中文）
    Desc string `json:"description,omitempty"`

    // adapter/carrier 专用（来自 core.AdapterConfig，字段名不变）：
    Adapter *core.AdapterConfig `json:"adapter,omitempty"`

    // action 专用：
    Action *ActionSpec `json:"action,omitempty"`
}

type ActionSpec struct {
    Method string `json:"method"`           // 机器方法名（"ssh-key"），仅文档展示
    Form   []FormField `json:"form"`        // 表单字段
}

type FormField struct {
    Field    string `json:"field"`          // "githubUser"
    Label    string `json:"label"`
    Required bool   `json:"required"`
    Placeholder string `json:"placeholder,omitempty"`
}

// 官方目录（编译内置）：Builtins() []Plugin，返回副本
func Builtins() []Plugin
```

官方目录内容（顺序即此顺序）：

| id | role | name | 内容 |
|---|---|---|---|
| pi | adapter | pi | pi.DefaultPIAdapter + desc「pi 的配置与扩展同步」 |
| herdr | adapter | herdr | herdr.DefaultHerdrAdapter + desc「herdr 终端复用器配置」 |
| opencode | adapter | opencode | opencode.DefaultOpencodeAdapter + desc「opencode 配置」 |
| vscode | adapter | VS Code | vscode.DefaultVSCodeAdapter + desc「VS Code 设置与扩展」 |
| keyring | carrier | 密钥环 | keys.DefaultAdapter + desc「跟随同步的加密密钥载体」 |
| ssh-key | action | 登录公钥 | form: [githubUser/GitHub 用户名/必填/例如 octocat] + desc「把 GitHub 用户公钥写入机器 authorized_keys」 |

**ActionSpec 仅做 UI 呈现与表单渲染**，不驱动执行（执行仍走现有硬编码
handler：/api/agents/<id>/ssh-key）。B3 允许为渲染方便加非契约字段。

## 文件所有权（互不越界）

| Owner | 文件 |
|---|---|
| A 插件内核 | internal/pluginregistry/**（新）、internal/adapter/tools.go、internal/toolctl/toolctl.go、internal/cli/commands/toolcheck.go、internal/cli/commands/init.go（注册点）、internal/cli/commands/initwizard.go（只读适配）、internal/keyring/keyring.go（删静默注入）、cmd/homer/main.go（仅当确有必要时触碰；现有 version 注入点不得改动）、docs/plugin-registry.md、docs/plugin-manifest-v1.md、A 的测试 |
| B1 hub API | internal/web/plugins.go（新）、internal/web/plugins_test.go、internal/web/gate_matrix_test.go、internal/web/credentials.go、internal/web/handlers.go（只加路由分发行）、e2e/plugins_test.go |
| B2 心跳 | internal/hub/heartbeat_ext.go（新）、internal/hub/heartbeat_ext_test.go、internal/cli/hub.go（装配） |
| B3 UI | internal/web/static/index.html（唯一）、e2e/plugins_ui_test.go |
| B4 CLI+文档 | README.md、DESIGN.md、docs/api-reference.md（新）、docs/plan/2026-10-11-plugin-architecture.md（新） |

**handlers.go 只有 A 和 B1 可碰，且各自只碰自己的行**。
**keyring 包只有 A 可碰。core 包无人可碰。**

## 接口冻结点（A 实现，B1/B3 依赖）

1. `pluginregistry.Builtins() []Plugin`
2. `pluginregistry.Builtin(id string) (Plugin, bool)`
3. `pluginregistry.Tools() []adapter.Tool` / `pluginregistry.ToolByID(id)` / `pluginregistry.OfficialInstall(binary)` —— **内置清单整体从 internal/adapter/tools.go 迁入 pluginregistry**（避免 import 环：pluginregistry import adapter 拿 Tool 类型与各 defaults，故 adapter 包不得反向引用 pluginregistry）。adapter 包保留 `Tool` 类型定义本身（toolctl/hub/agentd 的类型引用不动）；`adapter.Tools()/ToolByID/OfficialInstall` 三个函数**删除**，调用方全部切换：toolctl.go、toolcheck.go。B1/B3 不直接消费这三项。
4. `pluginregistry.CredentialRules() []web.CredentialRule` —— **砍掉**，A 不碰 web；改为 B1 在 web/plugins.go 里定义 `func (s *Server) installedCredentialRules()`：遍历 `pluginregistry.Builtins()` 中 role=adapter 的、且 `pluginregistry.PluginCredentialFiles(p)` 非空的。故 A 提供第 3.5 项：
   `pluginregistry.PluginCredentialFiles(p Plugin) []CredentialFile`，`type CredentialFile struct{ Name, Destination, Note string }`（pi 三条 + opencode 一条，从现 credentials.go 平移）
5. keyring 包删除 `ensureAdapter` 的自动调用（keyring.go:183 一带）：Apply 不再自动把 keyring 塞进 homer.json。**谁补位**：B1 的 key API 层在 keyring 插件已安装时确保 adapter 存在（见 B1 节）。
6. `web.ServeOptions.Plugins *pluginruntime.State`：**由 B1 负责添加**（pluginruntime 是 B1 的新包；A 不碰 web 包任何文件）。web 对 nil 值必须全兼容（单测/老 e2e 不传）。

### pluginruntime.State（B1 实现并冻结，A 只依赖形状）

```go
package pluginruntime // internal/pluginruntime

// State 是 hub 进程内的插件安装状态，serve 启动时初始化，进程生命周期不变。
// 并发安全。go:embed 持久化为 ~/.homer/plugins.json，每次变更后写盘。
type State struct { /* 内部字段 */ }

func New(home string) *State            // 读 plugins.json，不存在→空态
func (s *State) List() []pluginregistry.Plugin    // 已安装（按安装顺序）
func (s *State) IsInstalled(id string) bool
func (s *State) Install(p pluginregistry.Plugin) error
func (s *State) Uninstall(id string) error
```

- 安装状态持久化在 `~/.homer/plugins.json`：`{"schemaVersion":1,"installed":["pi","herdr"]}`（id 数组；启动时按 Builtins 展开成完整 Plugin）。
- Install 时校验 `core.ValidAdapterID`、防重复；Uninstall 有守卫（见 B1）。

### hub 安装语义（B1 实现）

`POST /api/plugins/install {id}`（body 也可携带完整 manifest JSON，见第三方节）：
1. `plugins.json` 状态更新（pluginruntime）。
2. **写世代**：hub 配置改动 = 发布新 generation（沿用 hub 一贯语义——中心配置只在世代里变）：
   - adapter/carrier：读当前世代 meta（homer.json 内容）→ 注入/更新 `adapters[id]` → `gens.New(home).Publish(store, newMeta)`。store 沿用上一代（快照内容不动）。
   - action：不改 homer.json，只更新 plugins.json（不发布世代）。
3. 卸载（B1 v1 仅支持 action 类卸载）：action 直接从 plugins.json 移除；
   adapter/carrier 卸载需 body `{"force":true}` 且校验守卫，否则 409 + `code:"uninstall-guard"`。守卫规则：
   - 卸载 keyring（carrier）时：若任一密钥条目的 file.adapter 绑定在**已安装的 adapter 插件**上 → 409，error 说明先解绑。
   - 卸载 adapter 时：若中心 store 世代里存在 `store[id]` 非空 → 409，error 说明先清理中心数据（v1 不提供清理）。
   - 机器侧残留：卸载不删机器文件，下次 dispatch configPolicy=keep 语义由机器自治。文档写明。

**安装后生效链（自动，无需新协议）**：世代发布 → 在线机器下一次 pull（任何来源）→
bootstrapFromGeneration 补 adapter → 下一次 status 上报就出现。B2 的心跳拓展把
「插件装了但机器还没报」的中间态可视化（见 B2 节）。

### /api/plugins 端点（B1，全部挂 /api/* 宽闸下）

| 路由 | 方法 | 行为 |
|---|---|---|
| /api/plugins | GET | `{schemaVersion:1, installed:[Plugin...], available:[Plugin...]}`（available=官方目录中未安装的；instaled 拼写以实现为准但必须一致） |
| /api/plugins/install | POST | body `{id}` 或 `{manifest:{...}}`（第三方，见下节）；成功 `{ok:true, plugin:{...}}` |
| /api/plugins/uninstall | POST | body `{id, force?}`；守卫规则见上 |

路由挂载点：handlers.go 的 `/api/` 分支内，`/api/plugins` 前缀分发到 `handlePluginsAPI`（B1 新文件 plugins.go 内）。404 语义与其他 API 一致。

### 第三方 manifest（B1 实现，schemaVersion=1）

`POST /api/plugins/install` body `{"manifest": { ...第三方 JSON... }}`：
- 必须字段：`id`、`role:"adapter"`、`root`、`categories`（校验用 core 已有的路径与 category 校验逻辑能过；`mode` 只能 merge|mirror，kind file|dir|manifest）。
- 拒绝：`role` 非 adapter（v1 第三方仅 adapter 型——action/carrier 需要官方代码，返回 422 `code:"third-party-role"`）；`id` 撞官方目录或已安装（409）；schemaVersion 非 1（422 `code:"schema-version"`）。
- 转成 Plugin（Adapter 字段填好）后走同一安装链。
- 持久化：第三方插件的完整 manifest 存 `~/.homer/plugins.json` 的 `custom:[{...Plugin JSON...}]` 数组（不再只是 id），重启后按此恢复。pluginruntime.State 需要支持（List 返回时合并官方展开+custom）。

### key 补位（B1）

`internal/web/keys.go` 的 key 操作入口处（Create/Save 等会写 keyring 的 handler）：
keyring 插件未安装 → 503 `code:"plugin-required"`，error「密钥环插件未安装，请先在插件页安装」。
已安装 → 操作前确保 hub 的 homer.json 配置里 keyring adapter 存在（把 A 从 keyring.go 删除的 ensureAdapter 逻辑搬到这里，条件化于插件状态）。hub 本地 homer.json 的更新直接 `core.SaveConfig`（hub serve 场景这不是世代——hub 侧世代只在快照发布时变，密钥操作本来就直写本地配置，维持现状语义）。

### 心跳拓展（B2）

现状 `hb` 只带 version/drift/host/tools。新增**向后兼容**字段：

```go
// internal/hub/proto.go 不动——HeartbeatParams 是 agent→hub 方向。
// 拓展点在 hub 侧消费：registryAgent 增加 ReportedAdapters []string。
```

B2 的实现方式（**不改 stream/hub/agentd 的现有文件**）：
1. `internal/hub/heartbeat_ext.go`（新文件）：定义 `ReportedAdaptersSource` 接口
   `type ReportedAdaptersSource interface { AgentReportedAdapters(agentID string) []string }`，
   并提供 `func MachineReportedAdapters(status json.RawMessage) []string`（从 status 报告 JSON 里抽 `report.adapters[].id`，供 pull status 时补记）。
2. `internal/cli/hub.go` 装配处：在 dispatcher 外包一层，pull/status 成功后把 adapters id 列表喂给 registry（新增 `Registry.NoteReportedAdapters(agentID, ids)`，在 heartbeat_ext.go 里定义，**以方法扩展形式写进 registry.go**——此为 B2 唯一可碰 registry.go 的点，只加不改）。
3. `/api/plugins` 的 GET 响应每项 Plugin 增加 `machineCount int` 字段（B2 供数，B1 的 handler 调 `ReportedAdaptersSource` 断言取数；断言失败→省略字段）。统计口径：**机器上报过该 adapter id** 的在线机器数。

**Why 心跳**：agent 的 status 报告本来就含 `report.adapters[].id`（commands.RunStatus 产出），快照管道已有；B2 只是把这份数据引到插件页。不新增 agent↔hub 协议消息。

### UI 改造（B3）

1. **插件管理页**（新增，入口在顶栏）：列出 installed / available（含第三方安装表单：粘贴 manifest JSON → POST install）。每项显示 id、角色徽章（适配器/载体/动作）、描述、机器覆盖数（machineCount，缺省 0）。
2. **操作按钮**：每项已安装插件提供「卸载」（带确认弹窗，adapter/carrier 提示守卫与数据保留语义）；每项未安装提供「安装」。
3. **ssh-key 转正**：机器卡片「登录公钥」按钮迁移为插件页 action 面板（选机器 + 表单），**同时保留机器卡片上的原按钮**（不删——现网肌肉记忆；两者调同一 API）。表单字段渲染自 `/api/plugins` 的 form spec。
4. **keyring 转正**：插件页可见；原「随同步自动携带」语义不变（收集/下发弹窗的自动勾选逻辑不动，仍隐藏 keyring 勾选框——那是同步语义不是插件语义）。
5. **适配器维度操作**：插件页每个已安装 adapter 插件行加「下发到全部机器」按钮 → `POST /api/sync?direction=dispatch` body `{adapters:[id]}`（现有端点，无新 API）。
6. **30s 轮询接入**：插件页数据进 `refresh()` 循环（轻量：页面不可见时跳过）。
7. i18n：全中文文案，与现 UI 风格一致。
8. **e2e 断言同步**：`tests/e2e/` 下涉及 ssh-key 按钮/插件页的断言若因 DOM 变化失效，B3 同步修复（dispatch_unlock_ui_test.go 的 data-adapter 契约不动）。

### CLI 与文档（B4）

1. `homer init`：`--adapters` 未知 id 的报错文案改为「未知 adapter: %s；可用: pi, herdr, opencode, vscode；自定义 adapter 请直接编辑 homer.json 或在 hub 插件页安装」（列表从 pluginregistry 派生，不再硬编码）。`--all` 语义不变。
2. README.md：新增「插件体系」小节（三类角色、安装/卸载、第三方 manifest 示例、信任边界）。
3. DESIGN.md：M5 的「adapter 插件机制」标注「已于 2026-10-11 落地（见 docs/plan/2026-10-11-plugin-architecture.md）」。
4. docs/api-reference.md（新）：/api/plugins 三端点 + /api/sync?direction=dispatch 的适配器维度用法。
5. docs/plan/2026-10-11-plugin-architecture.md（新）：架构决策记录（三分法、manifest schemaVersion、keyring/ssh-key 转正、第三方路线 v1-adapter-only、heartbeat 可视化）。
6. B4 不碰任何 .go 代码。

## 测试与验收

- A：`go test ./internal/pluginregistry/... ./internal/adapter/... ./internal/cli/commands/... ./internal/keyring/...`；`go build ./...`
- B1：`go test ./internal/web/... ./internal/pluginruntime/...`；gate_matrix 补 /api/plugins 的 7 凭证矩阵行；e2e/plugins_test.go（hub 起真实 server：安装→/api/plugins 出现→重启 hub 进程→仍安装（plugins.json 持久化）→卸载 action 成功/卸载有密钥绑定的 keyring 得 409）。
- B2：单测（fake dispatcher 实现 ReportedAdaptersSource + status JSON 抽取）+ 集成（hub_test 里拉一次 status 后 plugins GET 的 machineCount>0）。
- B3：chromedp e2e：插件页渲染 installed/available、安装/卸载流、ssh-key action 面板可用、下发到全部机器按钮触发（mock 或断言请求体）。
- 全员：`go vet ./internal/...`、`go build ./...`。
- 最终验收（父会话执行）：全量 `go test ./...` + `gofmt -l` 空 + 关键 e2e（父会话按 SOP 抽查 dispatch/unlock/plugins）。

## SOP 交叉引用

- enroll/e2e 纪律：`homer-enroll-e2e-discipline`（新路由必须过 gate 矩阵+e2e——B1 已含）
- 版本注入：`homer-version-identity`（main.version 注入点不能丢——A 只加不改）
- 协议 tag：无 stream 协议变更，不触发 `homer-release-tag-protocol-generation` 的强制 tag；但完成合并后父会话按惯例打 git tag。

## 集成顺序与合并

1. A 合入 master（B1-B4 均依赖其接口）。
2. B1 → B4 依次合入（共享 handlers.go/init 注册区，序列化）。
3. 父会话最终集成验证 + 补缝隙（见未分配项）。

## 未分配/父会话保留

- 全量回归与 e2e 巡检。
- 触及 `internal/core` 的意外需求（应当为零）。
- git tag 与晨报（/tmp/homer-morning-report.md）。
