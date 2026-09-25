# homer-cli v1.2 实施计划（docs/v12-plan.md 内容）

> 基线：master `d4c4b1c`（v1.0.0，388 tests 绿）。三大特性：① 交互式 init 向导（体验层）② 用户自定义 adapter（声明层）③ manifest 分类 + VS Code adapter（结构增量）。引擎三路判定/快照/store 语义**零破坏性修改**，仅 additive。

## 0. 目标与 Non-goals

**目标**：
1. `homer init` TTY 树状勾选向导（adapter → category → 大目录逐条目），选择写回 homer.json（enabled/exclude）；非 TTY / `--json` / `--all` / `--adapters` 保持现有全量注册；re-init 增量调整。
2. 自定义 adapter：手写 homer.json 声明即用（root/categories/ignore/allowEscape），init 永不删除既有条目，validateConfig 收紧（ID 命名 / categories ≥1）。
3. `CategoryConfig.kind: "file"|"dir"|"manifest"` + `listCmd`/`applyCmd`；manifest 虚拟文件进快照/store；pull/home 应用 = 预览 → 确认 → 逐 ID 执行 applyCmd（只装不卸，单条失败记 warning 继续）；远端配置（home clone）manifest 门禁。
4. VS Code 内置 adapter（第 4 个）：settings/keybindings 文件分类 + extensions manifest 分类。
5. e2e 主线：自定义 adapter 声明 → init（非交互注册 + Go 级 fake-port 向导测试）→ push → 新机 home → 假 `code`/假脚本安装扩展全链路。

**Non-goals（v1.2 不做）**：全量加密存储（v2.0）、插件**卸载**（manifest 只装不卸）、自定义 adapter 自动发现（init 不扫，手工声明）、pi 扩展形态（M4）、manifest 三路冲突裁决（并集安装语义）、doctor 的 manifest 检查、`homer sync`、版本号发布流程（tag/goreleaser 另行走）。

## 1. 关键架构决策

| # | 决策 | 依据 |
|---|---|---|
| D1 | **引擎零改动**：manifest 虚拟文件 = category 内普通 `Kind:"file"` 快照条目（键 `<category>.manifest.txt`），status/diff/push 天然工作（status 计数=ID 差异数，diff=逐行 ID diff）；pull/home 在 `PlanPull/PlanFirstContact` **之后**、`ApplyPullActions` **之前**拆出 manifest 动作 | 388 tests 零回归优先；`ResolveCategoryFilePath` 无法映射虚拟文件，必须前置拆出 |
| D2 | **manifest 应用语义 = 集合并集安装**：task IDs = 远端清单 ID − 本机 listCmd 实测 ID；write 与 **conflict** 动作都产出 task（conflict 的远端清单照装，本机多装的保留——收敛靠“push 存全量本机清单”）；delete 动作丢弃 + warning（不做卸载） | 两个方向各加一个扩展 → 双方 pull 后并集收敛，无冲突 UX |
| D3 | **命令执行安全**：无 shell——shellquote 拆 argv 后 `os/exec`；ID 过 `^[A-Za-z0-9][A-Za-z0-9._-]*$` 才传给 applyCmd（防 argv 注入）；listCmd 30s / applyCmd 120s 超时；applyCmd 只存在于 pull/home 应用路径（已有 confirm/--yes 门） | 任务确认“白名单不需要，远端注入防护必须有” |
| D4 | **远端注入门禁**：`homer home` clone 后配置含 manifest 分类且产生安装任务 → 预览追加“⚠ 远端配置声明了 manifest 分类，将执行外部命令”逐条列出 listCmd/applyCmd 原文；TTY 确认 / `--yes` 放行但 report.Warnings 记录“已按 --yes 确认执行远端声明的 manifest 命令”；非 TTY 无 `--yes` → aborted。**残余向量**（pull ff 引入新 manifest 命令、下次 status 即执行 listCmd）在 README 安全节明示，v1.3 用 state.json 指纹信任库收口 | 任务冻结范围 = home 门禁；e2e 主线要求 `home --yes` 可跑通 |
| D5 | **自定义与内置同名 = 定制内置**（同一 map key，用户可改 root/paths），不设冲突拒绝；新 ID 平行共存；`init` 任何形式**永不删除**既有 adapters/backup/secrets；`--force` 重置内置为默认值但保留非内置 adapter 与 backup/secrets | 冲突在数据模型上不存在；force 语义变化文档化 |
| D6 | **向导交互**：顺序三级 MultiSelect（adapter → 各选中 adapter 的 category → 超阈值目录类逐条目）；默认全选；未选 adapter/category → `enabled:false`，未选条目 → category.exclude 追加（目录类=首段，文件类=basename，去重不覆盖既有规则）；快照过滤用同一选择集（条目键首段匹配）。survey 键位：空格勾选 / enter 确认 / `/` 过滤；**'a' 全选 survey 无原生支持**——以“默认全选”达成同等效果，文案注明（风险表 R1） | 复用 survey（W8 已引入）；配置语义与现有 enabled/exclude 完全同构 |
| D7 | **re-init（已有 homer.json、无 --force、TTY）**：加载现有配置 → 向导预选自当前 enabled/exclude → 增量保存（只动 enabled/exclude，root/paths/mode/ignore/allowEscape/自定义 adapter/backup/secrets 不动）→ **不重写 store 快照**（报告提示用 push 提交）；非 TTY re-init 维持现状（拒绝覆盖）。`--force`：内置重置默认 + 向导（若 TTY）+ 全量重写 store，保留非内置字段 | 任务原文“增量更新 enabled/ignore，不动其他字段” |
| D8 | **vscode 默认 enabled:true**，root `~/.config/Code`；`code` CLI 缺失 / root 缺失 → ScanError 降级（root 缺失走既有 rootUnreadable→base 替换守卫，不误判删除意图）；e2e 用假 `code` 脚本（PATH 前置）+ 假 listCmd/applyCmd 脚本（共享 world 目录 + `$HOME` 差异化） | 任务确认的降级要求与 e2e 策略 |
| D9 | **向导测试策略**：`WizardPort` 注入 fake（现有 PromptPort 模式）；survey 适配器只做编译/类型契约 + 恒等回退测试；**e2e 永不模拟 TTY**（向导覆盖在 Go 级单测；e2e 主线的“init 交互选择”以非交互 init + patchConfig 添加自定义 adapter 表达） | 与既有 `TestPromptFallbackDoesNotBlock` 一脉；pty 模拟脆弱不引入 |

## 2. 冻结接口（全部签名，逐字落地）

### 2.1 core（P0）

```go
// internal/core/types.go —— additive
type CategoryKind string

const (
    CategoryKindFile     CategoryKind = "file"
    CategoryKindDir      CategoryKind = "dir"
    CategoryKindManifest CategoryKind = "manifest"
)

type CategoryConfig struct {
    Paths       []string      `json:"paths"`
    Mode        SyncMode      `json:"mode"`
    Kind        *CategoryKind `json:"kind,omitempty"`   // 缺省 = 现状（按 paths 推断，行为不变）
    ListCmd     string        `json:"listCmd,omitempty"`  // 仅 kind=manifest
    ApplyCmd    string        `json:"applyCmd,omitempty"` // 仅 kind=manifest
    Enabled     *bool         `json:"enabled,omitempty"`
    Exclude     []string      `json:"exclude,omitempty"`
    ExcludeKeys []string      `json:"excludeKeys,omitempty"`
}

// IsManifest is the single behavior switch; file/dir kinds stay informational.
func (c CategoryConfig) IsManifest() bool
```

`internal/core/config.go` 校验增量（validateCategory / validateConfigObject / configFromValue / configToValue 同步 round-trip，序列化键序 `paths, mode, kind, listCmd, applyCmd, enabled, exclude, excludeKeys`；`paths` 为 nil 时省略该键）：

- `adapters` 每个键须匹配 `^[a-z][a-z0-9-]*$`（内置 pi/herdr/opencode/vscode 均合规，统一适用）。
- 每个 adapter 的 `categories` 须**非空对象**（≥1 分类）——现状空 `{}` 可通过，属收紧（P0 验证基线 fixtures，见风险 R5）。
- `kind` 缺省 → 现行规则不变（paths 非空必填）。
- `kind` ∈ {file, dir} → 同现行（informational）。
- `kind == "manifest"` → `paths` 必须缺省或空数组；`listCmd`、`applyCmd` 必须非空字符串；`mode` 必须为 `"mirror"`。
- `kind` 为其他字符串 → 报错。
- `listCmd`/`applyCmd` 在 `kind != manifest` 时出现 → 报错。

### 2.2 manifest 包（W1，仅依赖 core + 标准库）

```go
// internal/manifest/manifest.go
package manifest

// CommandPort abstracts external command execution. Production uses
// DefaultPort (shellquote argv split + os/exec, no shell); tests inject fakes.
type CommandPort interface {
    Output(command string) ([]byte, error) // run listCmd, capture stdout
    Apply(command string, id string) error // run applyCmd, id appended as last argv
}

func DefaultPort() CommandPort

const (
    ListTimeout  = 30 * time.Second
    ApplyTimeout = 120 * time.Second
)

// One manifest category == exactly one snapshot entry (virtual file).
func VirtualFileName(category string) string // category + ".manifest.txt"

// ID list <-> virtual file content. Content = one ID per line, LF, trailing newline.
func ParseIDs(stdout []byte) []string // trim space, drop empty lines, preserve order, dedupe
func IDsOf(content string) []string
func ContentOf(ids []string) string

var validIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
func ValidID(id string) bool

// ScanProblem is the manifest-side diagnostic; the adapter package converts
// it to adapter.ScanError{Path: Command, Message: ...}.
type ScanProblem struct {
    Command string
    Message string
}

// ScanCategory runs listCmd and produces the virtual-file snapshot entry.
// Failure (missing binary, non-zero exit, timeout) => empty Files + one problem.
func ScanCategory(adapterID, category string, cfg core.CategoryConfig, port CommandPort) (core.CategorySnapshot, []ScanProblem)

// Apply side.
type Task struct {
    AdapterID string
    Category  string
    IDs       []string // sorted, deduped, to install
    ListCmd   string
    ApplyCmd  string
}

type Failure struct {
    ID      string
    Message string
}

type ApplyResult struct {
    Installed []string // "adapter/category:id"
    Failed    []Failure
}

// ApplyTasks runs applyCmd once per ID; a single failure is recorded and
// never blocks the remaining IDs. Invalid IDs are skipped with a Failure.
func ApplyTasks(tasks []Task, port CommandPort) ApplyResult
```

### 2.3 adapter（W1）

```go
// internal/adapter/scan.go —— additive variadic deps, existing callers unchanged
type ScanDeps struct {
    Commands manifest.CommandPort // nil => manifest.DefaultPort()
}

func ScanAdapter(adapterID string, config core.AdapterConfig, deps ...ScanDeps) ScanOutcome
// scanCategory gains a port parameter; kind=manifest branches to
// manifest.ScanCategory (problems converted to ScanError), skipping paths logic.
// categoryOrder preferred map gains: "vscode": {"settings", "keybindings", "extensions"}.
```

### 2.4 sync（W1）

```go
// internal/sync/manifest.go
// SplitManifestActions removes manifest-kind actions from plan and turns them
// into install tasks. Write AND conflict actions yield tasks (IDs parsed from
// action.Content / action.RemoteContent, minus IDs present in local snapshots);
// delete actions are dropped with a warning (v1.2 does not uninstall).
func SplitManifestActions(config core.HomerConfig, plan PullPlan, local []core.AdapterSnapshot) (PullPlan, []manifest.Task, []string)

func BuildManifestPreview(tasks []manifest.Task) string
// "  安装扩展 2 个（vscode/extensions）\n    + pub.one\n    + pub.two" — capped at 20 lines + "… 其余 N 条省略"
```

### 2.5 init 向导（W2，commands 包）

```go
// internal/cli/commands/initwizard.go
type WizardOption struct {
    Value string
    Label string
}

type WizardPort interface {
    // MultiSelect returns the checked subset of options. An error means the
    // user cancelled (Ctrl-C/EOF) and the whole init must abort.
    MultiSelect(message string, options []WizardOption, checked []string) ([]string, error)
}

type WizardEntry struct {
    Key      string // first path segment (dir categories) or the file key
    Label    string
    Included bool
}

type WizardCategory struct {
    Name      string
    Enabled   bool
    FileCount int
    Entries   []WizardEntry // first-level entries; empty when below threshold or file category
}

type WizardAdapter struct {
    ID         string
    Enabled    bool
    FileCount  int
    Categories []WizardCategory
}

type WizardState struct {
    Adapters []WizardAdapter
    ReInit   bool
}

type WizardSelection struct {
    Adapters   map[string]bool                 // adapter id -> keep enabled
    Categories map[string]map[string]bool      // adapter id -> category -> keep enabled
    Excluded   map[string]map[string][]string  // adapter id -> category -> unchecked entry keys
}

const WizardEntryDrillThreshold = 8 // categories with more first-level entries offer per-entry selection

// Pure orchestration over the port; no I/O. Flow: adapters -> categories
// (per selected adapter) -> entries (per selected category above threshold).
func RunSelectionWizard(port WizardPort, state WizardState) (WizardSelection, error)

// ApplySelectionToConfig mutates config in place: sets Enabled pointers,
// appends exclude patterns (entry Key; exact-duplicate-safe), never removes
// existing fields (root/paths/mode/ignore/allowEscape/backup/secrets intact).
func ApplySelectionToConfig(config *core.HomerConfig, selection WizardSelection)

// FilterSnapshotsBySelection drops disabled adapters/categories and entries
// whose first path segment (or exact key) is excluded. Shared choice set with
// ApplySelectionToConfig.
func FilterSnapshotsBySelection(snapshots []core.AdapterSnapshot, config core.HomerConfig) []core.AdapterSnapshot

// internal/cli/commands/initwizard_survey.go
type surveyWizardPort struct{}
func NewDefaultWizardPort(isTTY bool) WizardPort // non-TTY => identity port (returns checked unchanged, never errors)
```

`InitOptions` additive 字段 `All bool`；`InitDeps` additive 字段 `Wizard WizardPort`（nil → `NewDefaultWizardPort(isTTY())` 自检）。

**RunInit 交互判据（冻结）**：向导激活 iff `isTTY() && !opts.JSON && !opts.All && len(opts.Adapters)==0`。激活时：
- 无 homer.json：扫全部内置 → 向导（预选全选）→ `ApplySelectionToConfig` → `FilterSnapshotsBySelection` 后写 store（报告计数取过滤后）。
- 有 homer.json 且无 `--force`（re-init）：`LoadConfig` → 扫描（含自定义 adapter，预选自当前状态）→ 向导 → 增量保存；**不写 store**；报告 warnings 追加“已更新选择；store 快照未重写，`homer push` 提交当前本地状态”。
- `--force`：内置重置默认（TTY 则向导预选全选），保留非内置 adapters + backup/secrets，重写 store（现行为）。
- 向导 error（取消）→ `core.NewCliError("已取消初始化")`，零写入。
- 非 TTY / `--json` / `--all` / `--adapters` / 已存在配置且非 TTY 无 force：**行为与 v1.0 逐字节一致**（含“已存在 homer 配置…拒绝覆盖”报错，文案追加“TTY 下可交互调整选择”）。

### 2.6 pull/home manifest 应用与门禁（W4）

```go
// internal/cli/commands/pull.go / home.go —— additive
type ManifestApplyReport struct {
    Installed []string `json:"installed"` // "adapter/category:id"
    Failed    []string `json:"failed"`    // "adapter/category:id: message"
}

// PullReport / HomeReport additive field:
Manifest *ManifestApplyReport `json:"manifest,omitempty"`

// PullDeps / HomeDeps additive field:
Commands manifest.CommandPort // nil => manifest.DefaultPort()
```

**pull 流程（冻结顺序）**：`PlanPull` → `SplitManifestActions(config, plan, sources.Local)` → 无动作且无 task → no-drift → `BuildPullPreview + BuildManifestPreview` 合并预览 → confirm（task>0 也触发确认）→ `ApplyPullActions(remaining)` → `manifest.ApplyTasks(tasks, port)` → 结果并入 `report.Manifest`（失败进 warnings，不改变 applied/conflicts 计数与退出码语义）。`deps.NoApply` 同时跳过两者。

**home 流程**：`PlanFirstContact` 后同样 split；task>0 时 `BuildHomePreview` 追加安全节（原文列出 listCmd/applyCmd）+ 确认触发条件加 `|| len(tasks)>0`；`--yes` 放行并 `report.Warnings` 记录“已按 --yes 确认执行远端声明的 manifest 命令（N 条）”；非 TTY 无 `--yes` → aborted（错误信息含 manifest 提示）。mode=skip → 不 split 不装。应用后 `report.Manifest` 同 pull。渲染：`安装扩展: N 成功 / M 失败` + 失败明细行。

### 2.7 status disabled 呈现（W3）

```go
// internal/cli/commands/status.go —— additive
// StatusReport additive field:
Disabled []string `json:"disabled,omitempty"` // "pi"（整 adapter）或 "pi/themes"

func DisabledSummaries(config core.HomerConfig) []string
```

render：非 verbose 追加一行 `另有 N 个未启用的 adapter/分类`；verbose 逐行列出。diff 命令不变（disabled 天然缺席）。

### 2.8 VS Code adapter（P0）

```go
// internal/adapter/vscode/defaults.go
package vscode

const VSCodeAdapterID = "vscode"

var DefaultVSCodeAdapter = core.AdapterConfig{
    Root:    "~/.config/Code",
    Enabled: boolPtr(true),
    Categories: map[string]core.CategoryConfig{
        "settings":    {Paths: []string{"settings.json"}, Mode: core.SyncModeMerge},
        "keybindings": {Paths: []string{"keybindings.json"}, Mode: core.SyncModeMirror},
        "extensions": {
            Kind:    kindPtr(core.CategoryKindManifest),
            Mode:    core.SyncModeMirror,
            ListCmd:  "code --list-extensions",
            ApplyCmd: "code --install-extension",
        },
    },
}
// index.go: ScanError/ScanOutcome/ScanAdapter aliases，同 pi 包模式
```

`KNOWN_ADAPTERS` 增 `vscode`；`knownAdapterOrder = [pi, herdr, opencode, vscode]`；`selectedAdapters` 未知 ID 报错文案更新为 `pi, herdr, opencode, vscode`。

### 2.9 CLI flag（P0）

`parseOptions` 增 `--all`（布尔，拒绝内联值）；`unsupportedOptions` 增 `"all"` case；`validateCommandOptions` 中仅 init 放行 `--all`，其余命令拒绝；`INIT_USAGE`/`commandUsage(init)` 更新；`run.go` init 分支传 `All: options.All`。

## 3. 波次拆分（文件归属互斥）

| 波次 | 模块 | 文件清单（独占） | 依赖 | 验收（机器可查） |
|---|---|---|---|---|
| **P0** scaffold（单 writer，串行，~0.5d） | core 类型/校验/round-trip + vscode 声明注册 + `--all` + 基线修复 | `internal/core/{types.go,config.go,core_test.go}`、`internal/adapter/vscode/{defaults.go,index.go}`(新)、`internal/adapter/scan.go`(仅 categoryOrder 一行)、`internal/cli/commands/init.go`(仅 KNOWN_ADAPTERS/knownAdapterOrder/InitOptions.All/报错文案)、`internal/cli/{args.go,run.go}`(init 分支)、`internal/cli/readonly_test.go`(基线断言 3→4)、`tests/e2e/{mvp_test.go,v11_audit_test.go}`(adapters==4、ids 含 vscode、store 仍 10) | 无 | `go build ./... && go vet ./... && go test ./...` 全绿（≥388）；新校验表测试：kind 四分支/ID 正则/categories≥1/listCmd-applyCmd 互斥；manifest kind 的 config round-trip 往返一致 |
| **P1-W1** manifest 引擎（~1.5d） | manifest 包 + scan 集成 + plan 拆分 | `internal/manifest/{manifest.go,manifest_test.go}`(新)、`internal/adapter/scan.go`(scanCategory manifest 分支 + ScanDeps)、`internal/adapter/scan_test.go`(manifest 用例追加)、`internal/sync/{manifest.go,manifest_test.go}`(新) | P0 | `go test ./internal/manifest/... ./internal/adapter/... ./internal/sync/...`；ParseIDs 边界（空/CRLF/去重/保序）；ScanCategory 成功/失败/超时（fake port）；ScanDeps 注入经 `ScanAdapter` 三参调用且两参调用不回归；SplitManifestActions golden 向量（write→task、conflict→task 并集、delete→丢弃+warning、非 manifest 透传、ID 去重排序）；ApplyTasks（全成/单条失败继续/非法 ID 跳过） |
| **P1-W2** init 向导（~1.5d） | 向导 + RunInit 重构 | `internal/cli/commands/{initwizard.go,initwizard_survey.go,initwizard_test.go}`(新)、`internal/cli/commands/init.go`(RunInit 交互路径/re-init/保字段/INIT_USAGE) | P0 | `go test ./internal/cli/... -run 'TestInit'`：fake WizardPort 矩阵（adapter off→enabled:false；category off；entry off→exclude 追加去重、不覆盖既有 `*cache*`）；re-init 保留自定义 adapter/backup/secrets/allowEscape/改过的 root；恒等 port=零改动；非 TTY/--json/--all/--adapters 逐字节维持现行为；re-init 非 TTY 仍拒绝；取消→CliError 零写入；FilterSnapshotsBySelection（目录首段/文件键）；survey port 仅编译+恒等回退测试（不模拟 TTY） |
| **P1-W3** status 呈现 + 自定义 adapter 文档（~0.5d） | disabled 汇总 + README | `internal/cli/commands/status.go`、`internal/cli/render.go`、`internal/cli/commands/status_disabled_test.go`(新)、`README.md`(自定义 adapter 配置示例一节) | P0 | `go test ./internal/cli/... -run 'TestStatus'`：DisabledSummaries（混合 adapter/category）；render 折叠行 + verbose 明细；JSON `disabled` 字段；README 示例 JSON 块用 `ValidateConfig` 解析通过的 doc-test |
| **P2-W4** manifest 应用与门禁（~1d，单 writer） | pull/home 接线 | `internal/cli/commands/{pull.go,home.go}`、`internal/cli/commands/{pull_manifest_test.go,home_manifest_test.go}`(新) | **W1**（manifest 包 + SplitManifestActions） | 注入 Sources+fake Commands：预览含 `+ id` 安装节；confirm=false → aborted 零执行；`--yes` → report.Manifest.Installed 正确；单条 applyCmd 失败 → 继续 + warnings；home：task>0 触发确认、`--yes` 放行 + Warnings 含“已按 --yes 确认执行远端声明的 manifest 命令”、非 TTY 无 --yes → aborted 且错误含 manifest 提示、mode=skip → 无 task；PullReport/HomeReport JSON 含 manifest 字段；`go test ./internal/cli/commands/...` 全绿 |
| **P3-W5** e2e 主线 + 文档收口 + 对抗 review（~1d） | 全链路 e2e + README 收口 + 终检 | `tests/e2e/{v12_manifest_test.go,v12_custom_test.go}`(新)、`README.md`(向导/vscode/manifest/安全节，文案取自 W2/W4 report 草稿)、`docs/v12-report.md` | **W1–W4 全部** | e2e：机器 A（假 `code` PATH 脚本 + `~/.config/Code` fixtures + `$HOME/.fake-vscode-extensions`=2 ID）→ init --json（vscode extensions 虚拟文件=2 ID）→ patchConfig 加自定义 adapter `fakecli`（root `~/.fakecli`；conf 文件分类 + plugins manifest，listCmd/applyCmd 指向共享 world 目录脚本、经 `$HOME` 差异化）→ push --yes；机器 B（本地 1 ID）→ `home origin --yes --json` → 断言 homed、manifest.Installed 含缺的 2 ID、B 的假 apply 日志/假 `code` 安装日志、report.Warnings 含远端 manifest 门禁记录、store/vscode/extensions/extensions.manifest.txt 内容正确；机器 C 非 TTY 无 --yes → aborted + manifest 提示；`go vet ./... && go test ./...` 全绿；fresh-context pi 对抗 review（正确性/测试覆盖/简洁性三角度）后修复归零 |

并行度：P1 三路（W1/W2/W3 文件零交集），P2 单路（W4 依赖 W1），P3 收口。总计 6 个工作模块，3 路可并行。

## 4. 风险与回滚点

| # | 风险 | 对策 / 测试策略 | 回滚点 |
|---|---|---|---|
| R1 | **TTY 向导测试**：survey 无 'a' 全选原生键位；e2e 模拟 TTY 脆弱 | fake `WizardPort` 全覆盖逻辑（D9）；survey 适配器仅编译+恒等测试；向导激活判据集中一处（`isTTY() && !JSON && !All && 无 --adapters`），CI 全链路走非 TTY 旧路径 = 388 基线天然回归网 | 向导整块可由 `--all`/非 TTY 旁路；`initwizard*.go` 独立文件，revert 不伤引擎 |
| R2 | **manifest 执行安全边界**：远端注入不止 home clone——pull 的 ff 也会带入新 homer.json（syncPathspecs 含 homer.json），下次 status 即执行新 listCmd | 已交付防护：home 门禁（D4）+ 无 shell exec + ID 字符集 + applyCmd 仅在 confirm/--yes 门内；**残余向量** README 安全节明示，v1.3 方案已冻结草案（state.json 增 `trustedManifests map[string]string` = sha256(listCmd+"\x00"+applyCmd) 指纹库；scan 时未命中→不执行+base 替换+warning，init/home 为唯一写入点）。W1 的 `CommandPort` seam 即为该收口预留，v1.3 无接口变更 | manifest 分类是纯 additive（kind 缺省=现状）；vscode 可经配置禁用；`internal/manifest` 独立包可整体摘除 |
| R3 | **自定义 adapter 与内置优先级**：同名"冲突" | 定制内置（同 key 合法，用户可改 root）；新 ID 平行；init 永不删除既有 adapters/backup/secrets；`--force` 语义变化（保留非内置）文档化 + 测试钉死 | force 语义 revert 即恢复 v1.0 |
| R4 | vscode 默认注册扰动基线（init 3→4、home 建 `~/.config/Code`、warnings 增多） | P0 单 writer 一次性修齐 e2e/单测断言；root 缺失走既有 rootUnreadable 守卫，store 计数不变 | P0 独立 commit，可单独 revert |
| R5 | categories ≥1 收紧可能破既有 fixture | P0 执行时先全量跑测定位；若存在合法空 categories fixture → worker 上报（决策点：规则限 non-builtin 或撤回该条），不得自行放宽 | 校验规则独立小 commit |
| R6 | manifest 本机 listCmd 失败（无 GUI 无 code CLI）→ 本机清单视为空 → pull 会把远端全量当“待装”再逐条失败 | 接受为降级路径（文档化：applyCmd 同样失败 → 全部进 Failed/warnings，不写任何文件）；不引入额外启发式 | — |
| R7 | `ScanAdapter` variadic deps 与 pi/herdr/opencode 的 `var ScanAdapter = adapter.ScanAdapter` 别名传播 | 别名是同一 func value，签名自动带 variadic；W1 验证三包编译 | — |

## 5. 验收标准

1. `go build ./... && go vet ./... && go test ./...` 退出码 0；测试数 ≥ 480（基线 388 + 新增 ~90+），**零既有测试修改性失败**（仅 P0 声明式断言更新）。
2. 冻结接口 §2 逐字编译一致（review 时 diff 核对签名）。
3. 各模块 §3 表内验收命令全绿；e2e 主线（W5）一条不漏。
4. README 含：自定义 adapter 示例（过 ValidateConfig doc-test）、init 向导（--all/非 TTY/re-init）、vscode + manifest（含“只装不卸”与远端门禁）、安全节含残余向量说明。
5. 对抗 review（fresh-context，三角度）意见修复或记录于 `docs/v12-report.md`。
