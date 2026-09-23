# homer-cli M2 实施计划（同步内核：push / pull / merge / 备份 / 密钥扫描）

> 目标（DESIGN §6 M2）：跑通「安全往返」——本地 ⇄ store ⇄ git 远端 的完整写路径，含密钥扫描拒推、应用前备份、交互式冲突解决。
> 本文冻结全部新增 TS 接口；worker 只实现，不发明接口。M1 冻结接口**不做破坏性修改**，仅允许 §1.0 列出的 3 处 additive 变更。

## 0. 目标与 Non-goals

**目标**：
1. `homer push`：本地快照 → 密钥扫描 → store 落盘 → git commit（+ 网络推送）→ state.json 记录
2. `homer pull`：git fetch → 三路判定 → diff 预览 → 确认 → 备份 → 应用到工具目录（含删除传播）→ store ff → state 更新
3. `homer merge`：交互式冲突解决（Accept Local / Accept Remote 逐项；`--accept-local/--accept-remote` 批量降级）
4. 备份：任何写工具目录的操作前，受影响文件备份到 `~/.homer/backups/<date>/`，按日期保留最近 N 份（默认 7）
5. 密钥扫描：≥10 类常见 API key/token 正则，push 前强制扫描，命中拒推
6. git 边界落地（见 §1 决策）：base 从「store 工作区」升级为「state.json 记录的上次同步 commit」

**Non-goals（M2 明确不做）**：
- `secrets/` 目录与 age 加密层（M3）；`homer home` / `doctor` / `secret` / `pair`（M3）
- `homer sync` 智能合并命令（M2 的 pull+merge 已覆盖判定与写路径核心；sync 编排留 M3+）
- herdr / opencode adapter（M3）
- tailcat 快车道、adapter 插件机制（M5）
- pi 扩展形态 / `--resolve <json>` 回调协议（M4；`PromptPort` 的注入式设计已为其留位）
- **symlink 逃逸 allowlist**（M1 已知限制：真实 `~/.pi/agent/skills/agent-browser` 是逃逸链接会被排除）：M2 不做白名单，维持 M-B 安全边界；是否引入显式 allowlist 配置留 M3 产品决策
- **canonical JSON 序列化**：维持 M1 冻结的「内容相等 = 字符串全等」。git remote 注入后 base-absent 分支生产可达，语义等价但键序不同的 JSON 可能产生**保守冲突**（进 merge 交互，不静默丢数据）——可接受，显式记录
- 密钥扫描的 `--skip-scan` 旁路（不做，安全优先）；仅提供 `homer.json` 的 `secrets.ignorePaths` 精确豁免
- 首次对接 Pull/Merge/Skip 三选一交互 UI（无 base 时走引擎已实现的并集语义 + 冲突列表；三选一归 M3 `home`）
- 并发锁 / `homer unlock`（v2）
- `@clack/prompts` 之外的任何新运行时依赖

## 1. git 边界决策（本计划的核心架构决策）

**结论**：`~/.homer` 本身是 git 仓库；`homer push` 自动 commit 并（有 remote 时）push；base 升级为 git 历史（state.json 记录的上次同步 commit）。

| # | 决策 | 依据 |
|---|---|---|
| D1 | `~/.homer` 是 git 仓库（DESIGN §2.1 原文「本地工作区（git clone of 配置中心仓库）」，store/homer.json 都在里面）。`homer push` 首次运行时若非 git 仓库 → 自动 `git init`（进入 **local-only 模式**：只 commit 不推送，输出 warning） | §2.1 布局、§2.4「git 主通道」 |
| D2 | `homer push` = 写 store → `git add -A store/ + commit` → 有 upstream 则 `git push`。**never auto-push 约束不适用于用户显式执行的 `homer push`**（该约束针对自动同步/自动 pull，§2.5/§2.8） | §2.3 命令语义、§2.5 第 5 条 |
| D3 | **base 升级**：base = `state.json.lastSyncCommit` 时 `store/` 的内容（新增 git reader，`git ls-tree + git show`）。fallback 链：state 有 commit 且可读 → 用之；否则 base = store 工作区（= M1 语义，兼容 init 后未 push / 无 git 的场景） | §2.7「base 从 git 历史来」；m1-plan §0「引擎按 base 传入设计，M2 只需新增 git reader」 |
| D4 | **remote** = `git fetch` 后跟踪分支（`@{upstream}`，如 `origin/main`）的 `store/` 内容。无 upstream → pull/merge 报 CliError（提示 `git push -u` / `git branch --set-upstream`）；push 降级 local-only | §2.4 传输分层 |
| D5 | `state.json` 与 `backups/` 写入 `~/.homer/.gitignore`（幂等 ensure），**不入库**——同步策略本身不同步 | §2.1 末段（Obsidian 参考） |
| D6 | `homer pull` = fetch → 前置检查（store 工作区干净 + HEAD 是 upstream 祖先）→ 三路判定 → 预览/确认 → 备份 → 应用工具目录 → `git merge --ff-only @{upstream}` → state=新 HEAD。ff 失败在应用**之前**即被前置检查拦截（HEAD 非 upstream 祖先 = 本地有未推送 commit 且远端前进 = 分叉 → CliError 提示先 push） | §2.3 pull 条目、§2.5 |

**merge 完成语义（关键不变式）**：merge = `ff upstream`（store 对齐远端）→ state.lastSyncCommit = upstream HEAD（**远端变更已被看见**，base 前进）→ 逐项裁决写入工具目录（选 remote 的先备份）→ 重扫 local → 走 push 管线（有 drift 则 store+commit+push，无 drift 则仅 state 更新）。这样任何 resolution 之后 `local == 意图真相`，下一轮 sync 三方一致。

**降级矩阵**：

| 环境 | push | pull / merge |
|---|---|---|
| git 仓库 + upstream | 完整（commit + push） | 完整（fetch + ff） |
| git 仓库无 upstream | local-only（commit + warning） | CliError（提示配置 remote） |
| fetch 失败（离线） | warning + remote 视作 = base（退化为 M1 语义，仍 commit，跳过网络推送） | CliError |
| 非 git 仓库 | 自动 `git init` 后 local-only | CliError（先 push 建立 git 历史） |

## 2. 冻结接口（全部签名；各 worker 逐字落地，不得改动）

### 2.0 M1 冻结接口的 additive 变更（仅此 3 处，由 P0 独家执行）

1. `src/core/types.ts`：`HomerConfig` 增加两个**可选**顶层字段（不改任何既有字段）：
```ts
export interface BackupConfig { keep?: number }   // 保留最近 N 个日期目录，默认 7
export interface SecretsConfig { ignorePaths?: string[] } // store 相对路径 glob（matchesIgnore 语义）
// HomerConfig 增：backup?: BackupConfig; secrets?: SecretsConfig;
```
2. `src/core/paths.ts`：`HomerPaths` 增加字段 `backupsDir: string`（= `<home>/backups`）。
3. `src/core/config.ts`：`validateConfig` additive 校验 `backup.keep`（正整数）与 `secrets.ignorePaths`（string[]），缺省合法。

### 2.1 `src/core/state.ts`（P0 完整实现）

```ts
export interface HomerState {
  version: 1;
  lastSyncCommit?: string;                      // 上次同步成功后的 HEAD SHA
  lastSyncAt?: string;                          // ISO 8601
  lastSyncCommand?: 'push' | 'pull' | 'merge';
}
export function loadState(paths: HomerPaths): HomerState;   // 文件缺失/损坏 → { version: 1 }，不 throw
export function saveState(paths: HomerPaths, state: HomerState): void; // tmp + rename 原子写
```

### 2.2 `src/core/secrets/`（P1-W1）

```ts
// types.ts（P0 落地类型，冻结）
export interface SecretPattern { id: string; description: string; regex: RegExp; }
export interface SecretFinding {
  patternId: string; description: string;
  path: string;        // 'pi/settings/settings.json' 形式的 store 相对路径
  line: number;        // 1-based
  excerpt: string;     // 命中行（脱敏：命中串首尾各保留 4 字符，中段以 * 代替）
}

// patterns.ts + scan.ts + index.ts（W1 实现）
export const SECRET_PATTERNS: readonly SecretPattern[];   // ≥10 条，清单见下
export function scanContent(content: string, path: string): SecretFinding[];
export function scanSnapshots(snapshots: readonly AdapterSnapshot[]): SecretFinding[];
export function filterIgnored(findings: SecretFinding[], ignorePaths: string[] | undefined): SecretFinding[];
```

**正则清单（冻结 12 条，按序匹配，先命中先报）**：
`sk-ant-[A-Za-z0-9_-]{20,}`（Anthropic）、`sk-(?!ant-)[A-Za-z0-9_-]{20,}`（OpenAI）、`gh[pousr]_[A-Za-z0-9]{36,}`（GitHub classic）、`github_pat_[A-Za-z0-9_]{22,}`（GitHub fine-grained）、`AKIA[0-9A-Z]{16}`（AWS AK）、`xox[baprsce]-[A-Za-z0-9-]{10,}`（Slack）、`AIza[0-9A-Za-z_-]{35}`（Google）、`glpat-[A-Za-z0-9_-]{20,}`（GitLab）、`npm_[A-Za-z0-9]{36}`（npm）、`[sr]k_live_[A-Za-z0-9]{20,}`（Stripe live）、`-----BEGIN (RSA |EC |OPENSSH |PGP |DSA )?PRIVATE KEY( BLOCK)?-----`（私钥块）、`(?i)(api[_-]?key|secret|token|password)["']?\s*[:=]\s*["'][A-Za-z0-9+/_-]{20,}["']`（保守通用赋值；`__REQUIRED__` 占位符与 `${...}` 模板天然不命中）。命中 `secrets.ignorePaths` 的路径经 `filterIgnored` 豁免（glob 语义 = M1 冻结的 `matchesIgnore`，从 `src/adapters/pi/ignore.ts` import）。

### 2.3 `src/core/backup/`（P1-W2）

```ts
export interface BackupTarget { sourceAbs: string; label: string; }  // label = 'pi/settings/settings.json'，禁 '..'/绝对
export interface BackupResult { backupDir: string; backedUp: string[]; skipped: string[]; } // skipped = 源不存在（无需备份）
export function backupFiles(paths: HomerPaths, command: string, targets: readonly BackupTarget[]): BackupResult;
// 目录：<backupsDir>/<YYYYMMDD>/<HHmmss>-<command>/<label>；只备份已存在的源文件
export function pruneBackups(paths: HomerPaths, keep?: number): { removed: string[]; kept: string[] };
// keep 默认 7；按日期目录名降序保留前 keep 个，删除其余整个日期目录
```

### 2.4 `src/core/git/`（P1-W3）

```ts
// git.ts
export interface GitExecResult { ok: boolean; stdout: string; stderr: string; }
export function gitExec(home: string, args: readonly string[], opts?: { timeoutMs?: number }): GitExecResult;
// execFile('git', ...)，永不 throw，默认 timeout 15s
export function isGitRepo(home: string): boolean;
export function ensureGitRepo(home: string): void;
// git init（若缺）+ 幂等维护 <home>/.gitignore（至少含 state.json、backups/ 两行）
export function hasUpstream(home: string): boolean;
export function upstreamRef(home: string): string | undefined;   // 如 'origin/main'
export function gitFetch(home: string): GitExecResult;
export function gitPush(home: string): GitExecResult;
export function headCommit(home: string): string | undefined;    // git rev-parse HEAD
export function commitAllStore(home: string, message: string): string | undefined;
// git add -A store/ + commit → 新 SHA；无可提交变更 → undefined
export function mergeFfUpstream(home: string): GitExecResult;    // git merge --ff-only @{upstream}
export function isStoreClean(home: string): boolean;             // git status --porcelain -- store/ 为空
export function isAncestorOf(home: string, ancestor: string, descendant: string): boolean; // merge-base --is-ancestor

// reader.ts —— M1 计划预留的 "git reader"
export function readStoreSnapshotAtCommit(
  paths: HomerPaths, config: HomerConfig, commitish: string,
): AdapterSnapshot[];
// git ls-tree -r --name-only <commitish> -- store/ + git show <commitish>:<path>
// 布局/kind 判定与 readSnapshotFromStore 完全同构（entryKindFor by category.mode）；
// 跳过 .homer-complete 标记；结构严格对齐 config（缺目录 → 空 Map，M1 裁定语义）
```

### 2.5 `src/core/sync/`（类型 P0 落地；plan W4 实现，base/apply/pipeline W6 实现）

```ts
// ---- types.ts（P0 落地，冻结）----
export interface PullWriteAction  { type: 'write';  adapterId: string; category: string; relPath: string; content: string; }
export interface PullDeleteAction { type: 'delete'; adapterId: string; category: string; relPath: string; }
export interface PullConflictAction {
  type: 'conflict'; adapterId: string; category: string; relPath: string;
  reason: 'modify-vs-modify' | 'local-delete-vs-remote-modify' | 'local-modify-vs-remote-delete' | 'merge-keys';
  keyPaths?: string[];          // reason='merge-keys' 时的冲突键点路径
  localContent?: string; remoteContent?: string;   // mirror 文件冲突时给全量内容供 merge 交互展示
}
export type PullAction = PullWriteAction | PullDeleteAction | PullConflictAction;
export interface PullPlan { actions: PullAction[]; }

export interface ApplyResult {
  written:  { adapterId: string; category: string; relPath: string }[];
  deleted:  { adapterId: string; category: string; relPath: string }[];
  conflicts:{ adapterId: string; category: string; relPath: string }[]; // 跳过保留本地
  backupDir?: string;
}
export type SyncBaseMode = 'git' | 'store';
export interface SyncSources {
  mode: SyncBaseMode;
  base: AdapterSnapshot[]; local: AdapterSnapshot[]; remote: AdapterSnapshot[];
  baseCommit?: string; remoteRef?: string;    // mode='git' 时填
  warnings: string[]; errors: string[];
}

// ---- plan.ts（P1-W4，纯函数：只依赖 types.ts / engine / sync/types.ts）----
export function planPull(
  config: HomerConfig,
  base: AdapterSnapshot[],     // 未做 excludeKeys 剥离的原始快照（plan 内部自行处理）
  local: AdapterSnapshot[],
  remote: AdapterSnapshot[],
): PullPlan;

export interface PushCheck {
  status: 'ok' | 'remote-ahead' | 'conflicts';
  changedFiles: { adapterId: string; category: string; relPath: string }[]; // status='ok' 时：本次将写入 store 的文件清单
  conflictItems: PullConflictAction[];        // status='conflicts' 时
  remoteAheadFiles: { adapterId: string; category: string; relPath: string }[]; // status='remote-ahead' 时
}
export function checkPushSafety(
  config: HomerConfig, base: AdapterSnapshot[], local: AdapterSnapshot[], remote: AdapterSnapshot[],
): PushCheck;   // 基于 computeDrift（先做与 status 同口径的 stripExcludeKeys）：
                // 任一分类 pull>0 → 'remote-ahead'；任一 conflicts>0 → 'conflicts'；否则 'ok'

// ---- excluded-keys.ts（P1-W4）----
export function applyExcludeKeyPlaceholders(snapshot: AdapterSnapshot, config: HomerConfig): AdapterSnapshot;
// push 写 store 前：excludeKeys 顶层键 → 值 "__REQUIRED__"（DESIGN §2.2/§2.7 占位符机制）

// ---- base.ts（P2-W6）----
export function collectSyncSources(paths: HomerPaths, config: HomerConfig, opts?: { fetch?: boolean }): SyncSources;
// git 模式：base=readStoreSnapshotAtCommit(state.lastSyncCommit)（无 state → store 工作区），remote=readStoreSnapshotAtCommit(upstream)
// local=scanAdapter 原始快照（不剥离）；root 不可读的 adapter local:=base（沿用 M-A 守卫，防全量假删除）
// opts.fetch=false 供测试跳过网络；fetch 失败 → warnings + remote:=base（仅 push 用），pull/merge 由命令层先行检查

// ---- apply.ts（P2-W6）----
export function applyPullActions(
  paths: HomerPaths, config: HomerConfig, plan: PullPlan, opts?: { backup?: boolean },
): ApplyResult;
// write → 备份已存在的本地文件 → 写入（mkdir -p 父目录）；delete → 备份 → rm + 清理空父目录（至 category 目录根）；
// conflict → 跳过（保留本地）记入 conflicts。opts.backup 默认 true

// ---- pipeline.ts（P2-W6）----
export function prepareStoreSnapshot(local: AdapterSnapshot[], config: HomerConfig): AdapterSnapshot[];
// = applyExcludeKeyPlaceholders（push/merge 写 store 前的统一入口）
export function commitStoreIfNeeded(paths: HomerPaths, message: string): string | undefined;
export function requireCleanStore(paths: HomerPaths): void;   // store 脏 → CliError（提示先 homer push）
export function requireFastForwardable(paths: HomerPaths): void; // HEAD 非 upstream 祖先 → CliError（分叉，提示先 push）
```

**`planPull` 语义矩阵（冻结）**：
- mirror 分类（含 merge 降级的 file 条目）：`compareCategory` 结果 → `pull`→write(remote content)、`pull-delete`→delete、`conflict`→conflict action（带 local/remote 全量内容）、`push/push-delete/noop`→不产生动作。
- merge 分类逐文件：三方 parse → `mergeJson`；**任一键冲突 → 整文件进 conflict action（keyPaths=冲突键清单，文件级保留本地）**；无冲突且 merged ≠ local 内容（或 local 缺失）→ write（`JSON.stringify(merged, null, 2) + '\n'`，确定性格式）；remote 整文件删除且 local 未改 → delete；remote 删除且 local 改过 → conflict（modify-vs-delete）。
- **excludeKeys 三重语义在此收口**：mergeJson 前 base/local/remote 三方都 strip 掉 excludeKeys 顶层键；merge 完成后把 **local 原文件中的 excluded 键值重新植回 merged**（本地永不被远端覆盖；local 缺该键 → 不植回，命令层记 warning「缺失必填项」）。远端 store 里的 `__REQUIRED__` 占位符因 strip 而永远不会流入工具目录。

### 2.6 `src/adapters/paths.ts`（P2-W6，relPath ↔ 绝对路径反向映射）

```ts
export function resolveCategoryFilePath(root: string, categoryCfg: CategoryConfig, relPath: string): string;
// 目录型 path（'skills/'）→ <root>/skills/<relPath>；单文件型 → <root>/<paths 中 basename===relPath 的那个 path>
// relPath 含 '..' / 绝对路径 / 找不到对应单文件 path → throw（编程错误）
```

### 2.7 `src/cli/ui.ts`（P1-W5，@clack/prompts 唯一接入点）

```ts
export interface PromptPort {
  confirm(message: string, fallback: boolean): Promise<boolean>;
  select<T extends string>(message: string, options: readonly { value: T; label: string }[], fallback: T): Promise<T>;
}
export function createClackPromptPort(): PromptPort;          // @clack/prompts（intro/outro/select/confirm）
export function createNonInteractivePromptPort(): PromptPort; // 永远返回 fallback（无 TTY / CI / 测试）
export function createDefaultPromptPort(): PromptPort;        // process.stdout.isTTY ? clack : non-interactive
```

命令层约定：`--yes` 时**完全不创建 port**（不提示，走默认策略）；无 `--yes` 且非 TTY → port 返回 fallback（confirm=false → abort 并提示「非交互环境，请加 --yes」）。

### 2.8 `src/cli/commands/{push,pull,merge}.ts`（P0 落地类型+stub，P3 三 worker 分别实现）

```ts
// ---- push.ts ----
export interface PushOptions { homerHome?: string; json?: boolean; yes?: boolean; noPush?: boolean; }
export interface PushDeps { ui?: PromptPort; sources?: SyncSources; }   // 测试注入
export interface PushReport {
  ok: boolean;
  status: 'pushed' | 'no-drift' | 'secrets-rejected' | 'remote-ahead' | 'conflicts' | 'aborted' | 'error';
  secrets: SecretFinding[];
  changedFiles: { adapterId: string; category: string; relPath: string }[];
  commit?: string;
  pushedToRemote: boolean;
  warnings: string[];
  errors: string[];
}
export const PUSH_USAGE: string;
export async function runPush(opts: PushOptions, deps?: PushDeps): Promise<PushReport>;

// ---- pull.ts ----
export interface PullOptions { homerHome?: string; json?: boolean; yes?: boolean; }
export interface PullDeps { ui?: PromptPort; sources?: SyncSources; noFetch?: boolean; noApply?: boolean; }
export interface PullReport {
  ok: boolean;   // true = 应用完成且无残留冲突
  status: 'applied' | 'no-drift' | 'aborted' | 'conflicts-remain' | 'error';
  applied: ApplyResult;
  conflicts: PullConflictAction[];   // 残留冲突（已保留本地）
  commit?: string;
  warnings: string[];
  errors: string[];
}
export const PULL_USAGE: string;
export async function runPull(opts: PullOptions, deps?: PullDeps): Promise<PullReport>;

// ---- merge.ts ----
export interface MergeOptions { homerHome?: string; json?: boolean; acceptLocal?: boolean; acceptRemote?: boolean; }
export interface MergeDeps { ui?: PromptPort; sources?: SyncSources; noFetch?: boolean; }
export interface MergeReport {
  ok: boolean;
  status: 'resolved' | 'no-conflicts' | 'aborted' | 'error';
  resolutions: { adapterId: string; category: string; relPath: string; keyPaths?: string[]; choice: 'local' | 'remote' }[];
  applied: ApplyResult;   // 选择 remote 的项写入工具目录（含备份）
  commit?: string;        // ff 后有新 commit 时
  warnings: string[];
  errors: string[];
}
export const MERGE_USAGE: string;
export async function runMerge(opts: MergeOptions, deps?: MergeDeps): Promise<MergeReport>;
```

**push 流程（冻结）**：load config → sources（fetch 失败降级，见 §1 矩阵）→ `scanSnapshots(prepareStoreSnapshot(local))` 密钥扫描（含 ignorePaths 豁免）→ 命中 → `secrets-rejected` exit 1（逐条打印 path:line + pattern + 脱敏摘录）→ `checkPushSafety`：remote-ahead → exit 1 提示 `homer pull`；conflicts → exit 1 提示 `homer merge` → changedFiles 空 → `no-drift` exit 0 → 非 `--yes` 时 ui.confirm 预览（变更文件数汇总），拒绝 → `aborted` exit 1 → `writeSnapshotToStore(prepareStoreSnapshot(local))`（逐 adapter）→ `ensureGitRepo` + `commitStoreIfNeeded` → state 更新 → 有 upstream 且非 `--no-push` → `gitPush`（失败 → warning + exit 1「本地 commit 已成功，远端推送失败」）。**push 不写工具目录，无需备份**（store 本身在 git 版本化）。

**pull 流程（冻结）**：load config → 前置：isGitRepo / hasUpstream / requireCleanStore / gitFetch / requireFastForwardable → sources（base=state commit，无 state → store 工作区；root 不可读 → local:=base）→ `planPull` → 无动作 → `no-drift` exit 0 → 预览（write/delete/conflict 计数 + 复用 `diffLines` 的行级预览，每文件截断前 20 行）→ 非 `--yes` 时 confirm，拒绝 → `aborted` exit 1 → `applyPullActions`（backup=true）→ `mergeFfUpstream` → state=新 HEAD → 残留 conflicts → `conflicts-remain` exit 1（提示 `homer merge`），否则 exit 0。**`--yes` 时冲突默认策略 = 保留本地 + 标红列出**（DESIGN §2.8）。

**merge 流程（冻结）**：前置同 pull → sources → `planPull` 取 conflict actions → 空 → `no-conflicts` exit 0 → 裁决：`--accept-local`/`--accept-remote` 批量；否则逐项 `ui.select`（Accept Local / Accept Remote，无 skip——保留本地等价于 accept-local 并会在后续 push 覆盖远端，语义上不允许「不表态」）→ `requireCleanStore` + `mergeFfUpstream` → state.lastSyncCommit = upstream HEAD（**base 前进，远端变更已看见**）→ 选 remote 的项备份后写入工具目录（构造 write-only 子 plan 走 `applyPullActions`）→ 重扫 local → `checkPushSafety(base=remote, local, remote=remote)` 必为 ok → 有 drift 则 push 管线（store+commit+push）→ state=最终 HEAD → exit 0。

**退出码总表**：push：pushed/no-drift→0，其余→1；pull：applied/no-drift→0，aborted/conflicts-remain/error→1；merge：resolved/no-conflicts→0，aborted/error→1。CLI flag：`--home/--json/-h` 通用；push `--yes/--no-push`；pull `--yes`；merge `--accept-local/--accept-remote`。

## 3. 波次拆分 / 文件归属 / 依赖 / 验收

```
P0 M0-scaffold（串行）──┬──> P1 五并行 ──┬── W1-secrets      (src/core/secrets/{patterns,scan,index}.ts)
                        │                ├── W2-backup       (src/core/backup/, src/core/paths.ts†, src/core/config.ts†)
                        │                ├── W3-git          (src/core/git/{git,reader,index}.ts)
                        │                ├── W4-plan         (src/core/sync/{plan,excluded-keys,index}.ts)
                        │                └── W5-ui           (src/cli/ui.ts)
                        │        P2 串行 ── W6-sync-core      (src/core/sync/{base,apply,pipeline}.ts, src/adapters/paths.ts)
                        │        P3 三并行 ─┬─ W7-push        (src/cli/commands/push.ts)
                        │                   ├─ W8-pull        (src/cli/commands/pull.ts)
                        │                   └─ W9-merge       (src/cli/commands/merge.ts)
                        └──────────────────> P4 串行 W10-integrator（e2e + status/diff remote 注入 + 报告）
```
† = 对 M1 文件做 §2.0 列明的 additive 变更，全里程碑仅此两处既有文件被 P1 触碰（P0 除外）。

### P0 · M0-scaffold（串行，最先）

- **文件**：`package.json`（+`@clack/prompts` 运行时依赖，`npm install` 更新 lock）、`src/cli/args.ts`（COMMANDS += push/pull/merge + USAGE/各命令 usage 文本）、`src/cli/index.ts`（三个命令的完整 dispatch：选项解析按 §2.8 flag 表；**此后 P3 不得再改 index.ts/args.ts**）、`src/cli/commands/{push,pull,merge}.ts`（§2.8 接口逐字落地 + stub `throw new CliError('尚未实现')`）、`src/core/secrets/types.ts`、`src/core/sync/types.ts`（类型冻结）、`src/core/state.ts`（**完整实现**）+ `tests/core/state.test.ts`、`src/core/types.ts`（§2.0-1 additive）、`src/core/paths.ts`（§2.0-2 additive）。
- **依赖**：无。
- **验收**：`npm install && npm run typecheck && npm test` 全绿（331 既有 + state 新测试）；`node bin/homer.js push` 打印「尚未实现」exit 1；`node bin/homer.js --help` 含三条新命令；`git diff src/core/types.ts` 仅含两个可选字段。**commit 作为检查点。**

### P1 · 五个并行 worker（目录互斥，均只依赖 P0 + M1 既有代码）

**W1-secrets**：文件 `src/core/secrets/{patterns.ts,scan.ts,index.ts}` + `tests/core/secrets/*.test.ts`。纯函数零 fs。
验收：12 条 pattern 各 ≥1 正例 + ≥2 反例（如 `sk-ant-` 不误报为 openai；`__REQUIRED__` 不命中通用赋值；短 token 不命中）；`scanContent` 行号/excerpt 脱敏正确；`scanSnapshots` 跨 adapter 聚合；`filterIgnored` 豁免 `secrets.ignorePaths`；`npm run typecheck && npx vitest run tests/core/secrets` 绿。

**W2-backup**：文件 `src/core/backup/backup.ts` + `src/core/config.ts`（additive 校验）+ `tests/core/backup/*.test.ts` + `tests/core/config.test.ts` 增例。全部走 `HOMER_HOME=$(mkdtemp -d)`。
验收：备份到 `<backupsDir>/<date>/<time>-pull/pi/settings/settings.json` 精确路径断言；源不存在 → skipped；label 含 `..` → throw；prune：构造 10 个日期目录 keep=7 → 恰删最旧 3 个；`validateConfig` 对 `backup.keep: 0 / 'x'`、`secrets.ignorePaths: [42]` 报错、合法值通过。

**W3-git**：文件 `src/core/git/{git.ts,reader.ts,index.ts}` + `tests/core/git/*.test.ts`。测试全部用 `mkdtemp` 临时仓库（含 `git init --bare` 假 origin + clone）。
验收：ensureGitRepo 幂等（二次运行 .gitignore 不重复追加）；commitAllStore 无变更 → undefined、有变更 → SHA 且 `git log` 可见；readStoreSnapshotAtCommit 与 writeSnapshotToStore → commit → read 的 **roundtrip 深比较相等**（含多分类/子目录/空分类，跳过 `.homer-complete`）；isStoreClean / isAncestorOf / mergeFfUpstream（ff 成功、分叉失败）各覆盖；gitExec 对不存在命令/超时永不 throw。

**W4-plan**：文件 `src/core/sync/{plan.ts,excluded-keys.ts,index.ts}` + `tests/core/sync/plan.test.ts`。纯函数（只 import types/engine/sync-types）。
验收：mirror 侧 pull/pull-delete/conflict 三类动作映射；merge 侧：无冲突合并写回、**任一键冲突整文件进 conflict（keyPaths 正确）**、remote 删文件+local 未改→delete、remote 删+local 改→conflict、degraded 文件降级 mirror 语义；**excludeKeys 矩阵**：占位符替换（`apiKeys→"__REQUIRED__"`）、merge 时 local 的 excluded 键植回、远端 `__REQUIRED__` 不流入结果、local 缺失时不植回；checkPushSafety 三分支（ok/remote-ahead/conflicts）+ changedFiles 推导（mirror ops + merge changedKeys 的文件集合）。

**W5-ui**：文件 `src/cli/ui.ts` + `tests/cli/ui.test.ts`。
验收：non-interactive port confirm/select 永远返回 fallback（含默认值缺失场景）；clack port 仅做 import smoke + 类型契约（不模拟 TTY）；`createDefaultPromptPort` 在 `isTTY=false` 测试进程内返回 non-interactive 实例。

### P2 · W6-sync-core（串行，依赖 P1 全部）

- **文件**：`src/core/sync/{base.ts,apply.ts,pipeline.ts}`（更新 `src/core/sync/index.ts` barrel）、`src/adapters/paths.ts` + `tests/core/sync/{base,apply,pipeline}.test.ts`、`tests/adapters/paths.test.ts`。
- **验收**：
  - `resolveCategoryFilePath`：目录型/单文件型/多单文件型 category、`..` 与未知 basename → throw，与 scan 的 relPath 规则**互为逆映射**（用 M1 e2e 同款 fixture 断言绝对路径）。
  - `applyPullActions`（临时 HOME+HOMER_HOME）：write（含父目录自动创建）、delete（备份存在→rm→空父目录清理；源不存在→只记 deleted）、conflict 跳过保留本地、backup=false 跳过备份；**备份文件内容 == 覆盖前内容** 的精确断言。
  - `collectSyncSources`：git 模式（临时仓库 + 手工 commit + state）base/remote 快照正确；无 state → base=store；root 不可读 → local:=base；fetch=false。
  - `pipeline`：commitStoreIfNeeded / requireCleanStore（脏→CliError）/ requireFastForwardable（分叉→CliError）。

### P3 · 三个并行命令 worker（依赖 P2；每人只 owns 一个命令文件 + 自己的测试文件，禁改 index.ts/args.ts/render.ts，渲染逻辑写在各自命令文件内）

**W7-push / W8-pull / W9-merge** 共同验收基线：全部测试注入 `deps.sources`（手工构造三方快照）+ non-interactive port / `--yes`，**绝不碰真实 `~/.homer`、`~/.pi`、真实 git remote**；`--json` 输出可 `JSON.parse` 且与文本计数一致；退出码符合 §2.8 总表。
- W7-push 专项：密钥命中拒推（store 未被写入、origin 无新 commit）；remote-ahead/conflicts 拒绝路径；aborted（confirm=false）；完整成功路径（stub git：注入 deps 或临时仓库）含 state.lastSyncCommit 断言；`--no-push`。
- W8-pull 专项：预览文本含 diffLines 行级输出；`--yes` 冲突保留本地 + `conflicts-remain` exit 1；成功路径 applied.written/deleted/backups 断言；前置检查失败各 exit 1（无 upstream / store 脏 / 分叉）。
- W9-merge 专项：`--accept-local` / `--accept-remote` 批量路径（accept-remote 后工具目录 == remote 内容且 state==upstream HEAD）；逐项交互（注入 fake port 记录 select 调用序列）；no-conflicts exit 0；混合裁决后 push 管线被触发（新 commit 落在 upstream 之上）。

### P4 · W10-integrator（串行收尾，依赖 P3 全部）

- **文件**：`tests/e2e/m2.test.ts`、`src/cli/commands/status.ts` + `src/cli/render.ts`（**仅此处允许**：`collectSnapshotSources` additive 注入 remote——git 仓库存在且 upstream 可读时 remote=upstream 快照，否则维持 M1 行为；store 脏时置顶 `⚠` 告警）、`README.md`、`docs/m2-report.md`、缝隙修复（接口问题上报 orchestrator，禁私改冻结签名）。
- **e2e 验收（全程 mkdtemp + 假 HOME/HOMER_HOME + `git init --bare` 假 origin，子进程跑 `node --import tsx src/cli/index.ts`）**：
  1. **安全往返**：clone A → init → push --yes → bare origin 有 commit、store 文件齐全、state.lastSyncCommit==HEAD → 改 A 本地 skill + 删另一 skill + 改 settings 键 → push --yes → commit 包含对应增删改。
  2. **密钥拒推**：A 本地 settings.json 植入 `sk-ant-...` → push exit 1、报告含 path:line、origin 无新 commit；加 `secrets.ignorePaths` 豁免后可推。
  3. **pull 应用**：clone B（从 origin，homer.json 随仓库）→ B 本地预先放旧版文件 → pull --yes → 工具目录 == store 内容、`backups/<date>/` 存在且为旧版内容。
  4. **remote-ahead**：A 改 → push；B 改另一文件 → push 被拒（remote-ahead）；B pull --yes 成功合并双方。
  5. **冲突 + merge**：A/B 改同一 mirror 文件 → B pull → conflicts-remain exit 1 → B merge --accept-remote → B 工具目录 == A 版本 → A pull 拿到一致状态。
  6. **pull-delete 传播**：A 删文件 push → B pull --yes → B 本地文件被删且备份存在。
  7. **status ↓ 计数激活**：git 模式下 A push 后 B 直改 store 无 → B `status --json` 出现非零 pull（M1 已知限制 #1 关闭）。
  8. **retention**：手工构造 10 个日期目录 → 任一写操作后 prune 至 7。
- 里程碑 Done 后按 AGENTS.md 起三路 fresh-context 对抗 review（正确性/测试覆盖/简洁性），修复后写 `docs/m2-report.md`。

## 4. 风险与回滚点

1. **relPath ↔ 绝对路径反向映射**（最易错，M1 同类风险的写侧版本）：apply 把 `skills/foo/SKILL.md` 写错位置 = 污染用户真实目录。对策：`resolveCategoryFilePath` 签名冻结 + 与 scan 互为逆映射的双侧硬编码断言；apply 测试全部在假 HOME 下断言绝对路径。
2. **excludeKeys 三重语义缠结**（status 剥离 / push 占位符 / pull 植回本地键）：三处分别归 render（已存在）/ W4 excluded-keys / W4 planPull，单一 owner；W4 测试矩阵逐条锁定「占位符永不流入工具目录」「本地密钥键永不被远端覆盖」。
3. **git 状态机边角**（store 脏、分叉、fetch 失败、无 upstream）：全部收敛为 §1 降级矩阵 + 三个前置检查（requireCleanStore / requireFastForwardable / hasUpstream），命令层顺序冻结（检查全部在任何写操作**之前**），e2e 第 4/5 组覆盖。@clack 在无 TTY 下挂起的风险由 `isTTY` 守卫 + 测试一律注入 port 规避。

**回滚点**：P0 commit（依赖+类型+state）；P1 各模块独立 commit（secrets/backup/git/plan/ui 各一）；P2、P3 各命令独立 commit。任一 worker 失败 2 次重试后回滚该模块 commit、降级为 orchestrator 串行修复，不阻塞波次内其他 worker。`src/core/types.ts` 若发现需要超出 §2.0 的变更 → 一律上报 orchestrator 统一修订本计划并广播，worker 不得私改。

## 5. 机器可验收标准（M2 Done 定义）

```bash
npm run typecheck && npm test          # 全量绿（M1 331 + M2 全部新增；e2e 含假 origin 完整往返）
H=$(mktemp -d); FAKE_HOME=$(mktemp -d)
# 子进程级：init → push（拒密钥）→ push（成功）→ 第二 clone pull → 冲突 → merge --accept-remote
# 全程 exit code 与 §2.8 总表一致；state.json.lastSyncCommit 与 git rev-parse HEAD 相等
```
外加人工核验：`git -C ~/.homer log --oneline` 提交信息可读、`~/.homer/.gitignore` 含 state.json/backups/、真实 `~/.pi/agent` 除 pull --yes 显式应用外零写入。

---

## 6. 实施裁定追认（orchestrator，2026-09-23）

W10-integrator 上报的 4 处接缝裁定（docs/m2-report.md §4.2）全部追认：

- **S1 首次同步基线**：零漂移且仓库有 HEAD 且 store 未入库 → 落「建立同步基线」commit（否则 git 模式永不可启动，与 §3-P4①/§5 矛盾）。§2.8 push 流程据此补一行。
- **S2 首次接入判定**：upstream commit 无 store/ 树 → remote := base（非「远端删光」）。
- **S3 hasPushTarget**：push 推送判定用 `@{upstream} 或 branch.<name>.remote`（clone 空 origin 无 upstream ref 的 git 事实）。
- **S4 base 前移语义收口**：`lastSyncCommit` = 上次**成功**同步后的 HEAD；带残留冲突的 pull 不前移 base（由 merge 收尾前移）。§1-D6 据此修正。

W9 三未决问题裁决（docs/m2-report.md §6 建议）：clean 动作维持「有冲突才应用」；`applied` 部分结果留 M3 additive；merge push 失败维持 exit 0 + warning。
