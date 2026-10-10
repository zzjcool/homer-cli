# 配置定义对齐（configPolicy）：下发时探测 + 用户选择

> 立项：2026-10-10。四路 scout 侦察（wire 链路 / 状态通路 / 前端 / agent 执行序）已完成，本文为冻结计划。

## 1. 背景

VM-209-126 下发永久卡死的根因：中心 homer.json 给 pi 加了 `files/context/extensions` 分类
（commit 7320e9a）并给 `settings` 追加 `mcp.json` 路径，而早接入的机器配置还是老形状。
`PlanPull` 按中心快照规划出 `pi/files/*` 写入动作，`ApplyPullActions` 的 `resolveAction`
（internal/sync/apply.go:188/192）按本机 config 校验失败 → 整批 error → 已记录决定被保留
→ 重试同路 → 死循环。

现有 `addMissingAdapters`（internal/agentd/executor.go:551）只补「整机缺失的 adapter」，
从不进 Categories 层（:566 一旦 adapter id 存在就整条跳过），无法自愈。

用户决定：**一致与增量都要支持**。下发时探测配置定义漂移，控制台让用户选：

- **center（以中心为准）**：该 adapter 的整个定义（Root/Enabled/Categories/Ignore/AllowEscape）
  替换为中心的。默认选项。
- **keep（保留本机定义）**：root、已有分类的定义、Ignore、AllowEscape 全部不动；
  ①补上中心有而本机没有的**分类**（整条定义来自中心）；②对共有分类做 **Paths 并集**
  （只把中心有、本机没有的 path 追加到末尾，不改已有 path 的顺序——last-match-wins 语义下
  追加是安全方向）。机器独有的分类保留不动。

keep 必须同时做 ①+②：只补分类修不了「settings 缺 mcp.json 路径」这种共有分类漂移，
会留下第二种卡死形态。

## 2. 契约（冻结，前端/后端/Golang 逐字遵守）

### 2.1 Wire 形状

```text
POST /api/agents/<id>/pull?confirm=true
{
  "adapters": ["pi", "keyring"],       // 现有
  "unlocks": [...],                    // 现有
  "configPolicy": { "pi": "center" }   // 新增：仅含「探测到漂移且被勾选」的 adapter
}
```

- 取值：`"center" | "keep"`；key 必须过 `core.ValidAdapterID`；违规 → 400 bad-request。
- **空缺 = 现状**：不传 configPolicy 时行为与今天完全一致（旧前端/API 调用方零影响）。
- 控制台只对「有漂移的行」收集决定；radio 默认选中 center（用户已拍板默认以中心为准）。
- `hub.TaskOptions` 新字段 `ConfigPolicy map[string]string \`json:"configPolicy,omitempty"\``
  ——必须 omitempty，`TaskOptions{}` 必须仍序列化为 `{}`（internal/hub/task_zero_test.go 冻结）。
- `web.SyncScope` 新字段 `ConfigPolicy map[string]string`（**用户可见字段**，readSyncScope
  从 body 解析——注意它与 ApplyResolutions/ClearResolutions/CenterGeneration 的
  「hub-internal only」约定不同，scope.go:29-32 注释要同步更新）。
- 扇出路径（web/sync.go:361-364 清三个 resolution 字段）**不清** configPolicy——
  但现有扇出调用方不传它，语义自然为空，无需特判。
- 命名禁区：**不要复用 `Resolve` 字段**（internal/hub/task.go:53-56 注释 + agentd/handlers.go:187
  的前置短路：Resolve=local|center 会绕过 method 分发直接走 executorResolve）。
  configPolicy 是独立字段，与 Resolve 无关。

### 2.2 探测数据（机器上报自己的配置声明大纲）

`commands.StatusReport`（internal/cli/commands/status.go:62-71）新增：

```go
type ConfigOutlineCategory struct {
    Name  string   `json:"name"`
    Paths []string `json:"paths,omitempty"`
}
type ConfigOutlineAdapter struct {
    ID         string                   `json:"id"`
    Root       string                   `json:"root,omitempty"`
    Categories []ConfigOutlineCategory  `json:"categories,omitempty"` // 声明（config 而非扫描结果），按名称排序
}
// StatusReport 新字段：
ConfigOutline *[]ConfigOutlineAdapter `json:"configOutline,omitempty"`
```

- 挂载点：RunStatus 内部、**config 确实从磁盘加载成功**时（走内置默认扫描的新机器不挂
  ——outline 缺失 = 没有用户配置 = 走整文件 bootstrap，不需要对话框）。
- 命名 lowerCamel，与现有 StatusResolution 风格一致。
- 结构必须是纯 JSON 可往返类型（agentd 的 cloneStatusReport 用 JSON marshal/unmarshal 克隆，
  internal/agentd/taskclass.go:311-321）。
- 空 report 契约：status_zero_test.go 冻结「空 report 不长出新 key」——omitempty 满足。

### 2.3 漂移计算（hub 侧，choices API）

`AdapterChoice`（internal/web/choices.go:51-71）新增：

```go
type ConfigDrift struct {
    NewCategories     []string `json:"newCategories,omitempty"`     // 中心有、本机没有的分类名
    ExtraCategories   []string `json:"extraCategories,omitempty"`    // 仅本机有的分类名
    CategoryAdditions []string `json:"categoryAdditions,omitempty"`  // 共有分类中中心会补 path 的分类名
    RootChanged       bool     `json:"rootChanged,omitempty"`
    MachineRoot       string   `json:"machineRoot,omitempty"`
    CenterRoot        string   `json:"centerRoot,omitempty"`
}
// AdapterChoice 新字段：
ConfigDrift *ConfigDrift `json:"configDrift,omitempty"`
```

- 计算位置：`buildSyncChoicesResponse` dispatch 分支（choices.go:603-643），choices 构建完后。
- **中心基线 = 机器实际会收到的同一份字节**：agent 的 downloadHubSnapshot 拿的是
  `/api/snapshot` 的 `homerJson` 字段。实现时追踪 internal/web/sync.go 快照 handler 的
  真实来源（generation Meta 或 hub 当前文件），diff 必须用同一来源，两边永远一致。
  若当前无 generation（centerGeneration < 1）→ 不计算（此时也无法下发）。
- 机器基线 = report.ConfigOutline（2.2）。outline 缺失（新机器）→ 不计算。
- 判定有漂移：NewCategories ∪ ExtraCategories ∪ CategoryAdditions 非空，或 RootChanged。
  （root 用原字符串比较，不做 ~ 展开归一——honest diff。）
- CategoryAdditions 判定：共有分类，中心 Paths 减去本机 Paths（按 normalizeRel
  归一后的集合差）非空。**本机 Paths 来自 2.2 outline 的 Categories[].Paths**
  （outline 因此携带 paths，不只是名字）。Mode/Exclude 等其他字段差异 **不判**、
  keep 也不修（v1 范围外）。
- keyring 行照算无害（前端隐藏该行，永不进 policy map）。

### 2.4 Agent 端应用（pull 前落配置）

- `ResolvedPullRequest`（internal/agentd/resolutions.go:145-150）新增 `ConfigPolicy map[string]string`。
- `localExecutor.pull`（executor.go:283-309，私有扩展点，已有 preferRemoteAdapters 先例）
  新增第 6 参 `configPolicy map[string]string`：
  - `Pull`（frozen 接口，executor.go:27-28）委托时传 nil；
  - `PullApplyingResolutions`：`legacyPull` 闭包（resolutions.go:159-161）改调
    `e.pull(ctx, req.Confirm, req.Adapters, false, nil, req.ConfigPolicy)`，
    主路径调用（resolutions.go:278）同样带上——四个 legacy 早退点全部不丢 policy；
  - agentd/handlers.go TaskKindPull 普通（非 ApplyResolutions）分支：policy 非空时
    用 `d.exec.(*localExecutor)` 类型断言（executorPullResolved 同款模式）走 e.pull，
    断言失败报错；policy 为空走现状 `executorPull`。
- `bootstrapFromGeneration`（executor.go:526）新增 policy 参，传给新函数
  `applyConfigPolicy(paths, meta, policy)`（取代 addMissingAdapters）：
  - 保留全部 fail-open 语义：meta 空 / 中心 config 非法（log+nil）/ 本机 config 坏 → nil；
  - 无本机 config：整文件写入不变（executor.go:542）；
  - 中心有而本机没有的 adapter：无条件补（现状语义，keyring 靠它落地）；
  - policy=center：本机已有该 adapter → 整个 AdapterConfig 换成中心的；
  - policy=keep：①补缺失分类（中心定义整条）；②共有分类 Paths 并集——本机已有的
    path 保持原顺序在前，中心独有的 path 按中心顺序追加在后；其余字段（root/mode/
    exclude/enabled/ignore/allowEscape/机器独有分类）一律不动；
  - policy 无该 adapter 条目：现状（不动已有 adapter）；
  - 零写入守卫保留：无任何变更 → 不写盘（bootstrap_adapters_test.go:53 逐字节比对锁定）；
  - 有变更 → **先备份**：`backup.BackupFiles(paths, "pull", []backup.BackupTarget{{
    SourceAbs: paths.ConfigFile, Label: "homer.json"}})`（单文件 API，home.go/secret.go 有先例；
    Label 过 assertSafeSegment 校验），再 `core.SaveConfig`（原子写）。
- 执行序已验证：bootstrap 写盘（executor.go:293）在 RunPull 读 config（pull.go:402）之前，
  同一次 pull 内生效。config 变更后的失效（bumpWriteGen/forgetDrift/signalHeartbeat）
  已被通用写任务收尾覆盖（agentd/handlers.go:87-96），无需新增。

### 2.5 失配文案（internal/sync/apply.go:188/192，无测试锁定，两处同改）

改为可操作指引，例：

```
applyPullActions: pi/files 不在 homer.json 中（中心与本机的配置定义不一致）。
到控制台对该机器重新下发，确认框会提供定义对齐方式：以中心为准，或保留本机定义并补上缺失分类。
```

### 2.6 前端（internal/web/static/index.html）

- 漂移块渲染：`renderScopeChoices`（1368-1551）的 `list.forEach` 内、1474 行
  （collect 专用追加）之后，dispatch 方向且 `item.configDrift` 存在时追加到该 adapter 的
  `block`（1430）。
- radio 组：复制 1477-1497 的 resolve radio 模式——`el("div","resolution-choice-row")`、
  `input.name = "config-policy-" + item.id`（独立 name 防跨行互斥）、
  `dataset.configPolicyAdapter = item.id`、change → `updateScopeConfirm()`。
- 选项文案：**「以中心为准」**（默认选中）/ **「保留本机定义，补上缺失分类」**。
  选择状态存 `app.configPolicySelections`（先例 app.recordChoiceSelections，426 行附近），
  并在 openScopeDialog 的状态重置区（1816-1827）清空——renderScopeChoices 全量重绘，
  DOM 不存状态。
- 详情行（muted small）：「中心新增分类：…」（NewCategories）/「仅本机有的分类：…
  （以中心为准后不再同步它们）」（ExtraCategories）/「将补全的分类：…」
  （CategoryAdditions）/「root：A → B」（RootChanged）。
- payload：`executeDispatchOne`（2321）——**不得改动 2323 行
  `const body = { adapters: adapters }` 的字面写法**（copy_test 钉死），
  在 2324 之后追加：
  ```js
  const configPolicy = configPolicyForSelection();
  if (configPolicy) body.configPolicy = configPolicy;
  ```
  `configPolicyForSelection()`：收集「被勾选 且 有 configDrift」的行 →
  `{id: app.configPolicySelections[id] || "center"}`；空对象则不发送。
  keyring 行被 1429 跳过渲染，天然不进 map。
- updateScopeConfirm 无需改：policy 有默认值，不 gate 确认按钮。
- **copy_test 禁词**（internal/web/copy_test.go:15-33）：不得出现 推送/拉取/查看差异/
  处理漂移/仓库/commit/allow-secrets/以本机为准/这台机器的改动/勾选「密钥」/单独「下发」
  等禁词；新 UI 文案（「保留本机定义，补上缺失分类」等）加入 required 列表冻结。

## 3. 兼容性矩阵

场景 | 表现
--- | ---
旧 hub + 新 agent | params 无 configPolicy → 零值 → 现状
新 hub + 旧 agent | omitempty 零值不出现 → 旧 agent 看不到未知键
旧前端（收藏的旧 UI/curl） | 不传 configPolicy → 现状
旧机器 status（无 outline） | choices 不算漂移 → 无对话框 → 现状
新机器（无 homer.json） | outline 缺失 → 无对话框；首次下发整文件 bootstrap 不变
resolutions DeepEqual 契约（executor_resolutions_test.go:438） | policy 为 nil 时 resolved 路径与 legacy 报告逐字段相同 → 保住
TaskOptions 零值契约（task_zero_test.go） | map + omitempty → 保住

## 4. 任务切分（三路并行，worktree 隔离，文件零交集）

Worker | 文件（所有权，禁止越界）
--- | ---
A：agent 端 + wire | internal/web/scope.go、internal/hub/task.go、internal/hub/dispatcher.go、internal/agentd/handlers.go、internal/agentd/resolutions.go、internal/agentd/executor.go、internal/agentd/agentd.go（如需）、internal/agentd/bootstrap_adapters_test.go、internal/agentd/executor_resolutions_test.go、internal/agentd/executor_test.go、internal/hub 相关测试、internal/web 的 scope 解析测试
B：探测 + 漂移 + 文案 | internal/cli/commands/status.go、internal/web/choices.go、internal/sync/apply.go、各自测试
C：前端 | internal/web/static/index.html、internal/web/copy_test.go（required 增补）

**注意**：三路在独立 worktree，互相看不到对方改动；本计划的契约就是唯一接口。
合并后（A+B+C 全进 master）需补一个横切集成测试：choices 漂移 → readSyncScope policy →
stub agent 消费（web 包内），由合并者补。

### 验收

- 全仓 `go test ./...` 绿（排除 .pi-subagents/** 副本污染——历史 worktree 有 40+ 份完整拷贝，
  rg/grep 必须加排除）。
- `gofmt -l internal/` 无输出。
- Worker A 必须新增的测试：
  - applyConfigPolicy：center 整替（只动选中 adapter）；keep 补分类 + paths 并集
    （顺序：本机在前中心追加在后）；keep 不动 root/mode/ignore/机器独有分类；无 policy 零写入
    （现有逐字节测试保住）；备份产生（backups/ 下有 homer.json）；fail-open 三分支。
  - PullApplyingResolutions 带 policy：老形状 config + 中心含 files 分类 + center 决定 →
    下发成功、pi/files 写入、决定被消费（**VM 卡死场景的回归测试**）；
    legacy 早退路径 policy 不丢。
  - taskOptionsForScope 复制 policy；TaskOptions{} 零值契约不变。
  - readSyncScope：合法/非法值/缺省。
- Worker B 必须新增的测试：outline 挂载（有 config）/新机器不挂/omitempty；
  漂移判定四种（new/extra/additions/root）+ 无漂移不挂 + 无 generation 不挂 + outline 缺失不挂。
- Worker C 验收：`go test ./internal/web/ -run 'TestConsoleUserCopy'` 绿，
  新增 required 词进 copy_test；人工核对（无浏览器测试链）radio 结构与 resolve 组一致。

## 5. 不做什么（v1 范围外）

- mode/exclude/enabled/ignore/allowEscape 的细粒度漂移显示与 keep 修复——真踩到时由
  2.5 的可操作文案兜底，用户改选 center 解决。
- 收取（collect）方向的 policy：push 按机器 config 自扫，自洽，不需要。
- 「记录并立即执行」（resolveOnMachine）路径的漂移对话框：该路径失败时文案指引用户
  走下发对话框（那里有完整探测）。policy 若经 readSyncScope 流入该路径，
  语义自然为空，无需特判。
- 多机批量下发的每机对话框：批量流不收集 policy，行为同现状。
