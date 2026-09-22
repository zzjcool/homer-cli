# homer-cli M1 实施计划

> 目标（DESIGN §6 M1）：仓库结构 + homer.json schema + pi adapter 只读 diff/status —— 跑通“看见漂移”。
> 本文冻结全部跨模块 TS 接口；worker 只实现，不发明接口。

## 0. 目标与不做的事

**目标**：`homer init` 扫描 `~/.pi/agent` 生成 `homer.json` + store 快照；`homer status` / `homer diff` 基于 §2.7 判定引擎输出只读漂移报告（含 `--json`）。

**Non-goals（M1 明确不做）**：
- 任何写路径：push / pull / merge / 备份 / 密钥扫描（M2）
- git 操作：base 从 git 历史读取（M1 中 base = store 工作区，因为 init 后 store == 最后一次提交；引擎接口已按“base 作为数据传入”设计，M2 只需新增 git reader）
- age 密钥、herdr/opencode adapter、pi 扩展形态（M3/M4）
- 交互式 init（@clack/prompts 已选定但 M1 的命令全部非交互，交互层 M2+ 接入）
- excludeKeys 的 `__REQUIRED__` 占位符写回（M1 只做 drift 比较前剥离，占位符机制 M2）

## 1. 冻结接口（全部签名，P0 由 scaffold worker 逐字落地，之后任何人不得改动）

### 1.1 `src/core/types.ts`（P0 落地后冻结）

```ts
export type SyncMode = 'merge' | 'mirror';

export interface CategoryConfig {
  paths: string[];              // 相对 adapter root；'xxx/' 结尾 = 目录，否则单文件
  mode: SyncMode;
  enabled?: boolean;            // 默认 true
  exclude?: string[];           // category 内文件名 glob（仅 '*' 通配）
  excludeKeys?: string[];       // merge 模式字段级排除（顶层键名）
}

export interface AdapterConfig {
  root: string;                 // '~' 需展开
  enabled?: boolean;
  categories: Record<string, CategoryConfig>;
  ignore?: string[];            // adapter 级忽略，相对 root 的路径 glob
}

export interface HomerConfig {
  version: 1;
  adapters: Record<string, AdapterConfig>;
}

export interface SnapshotEntry {
  kind: 'json' | 'file';        // 'json' = 可 JSON.parse；merge 分类中解析失败自动降级 'file'
  content: string;              // 原始文本（UTF-8）
}

/** key = category 内相对路径（见 §1.6 布局规则） */
export type SnapshotFiles = Map<string, SnapshotEntry>;

export interface CategorySnapshot {
  adapterId: string;
  category: string;
  mode: SyncMode;
  files: SnapshotFiles;
}

export interface AdapterSnapshot {
  adapterId: string;
  categories: CategorySnapshot[];
}
```

### 1.2 `src/core/engine/merge.ts` — 判定引擎（merge 半边）

```ts
export interface MergeConflict {
  keyPath: string;  // 点路径，如 'models.openai'；数组整体记数组键名
  reason: 'both-modified' | 'modify-vs-delete' | 'array-both-changed';
  base?: unknown; local?: unknown; remote?: unknown;
}

export interface MergeResult {
  status: 'clean' | 'conflict';
  merged: Record<string, unknown>;  // 冲突键暂取 local 值并列入 conflicts（M2 交互再裁决）
  conflicts: MergeConflict[];
}

/** 单文件 JSON 三路合并（base/local/remote 为已解析值） */
export function mergeJson(base: unknown, local: unknown, remote: unknown): MergeResult;

/** 键级变更统计：local 相对 base 改/增/删了哪些键 —— status ↑ 计数来源 */
export function diffJson(base: unknown, local: unknown):
  { changed: number; added: number; deleted: number; keys: string[] };

/** excludeKeys 剥离（顶层键），drift 比较前对 local 与 base 各调一次 */
export function stripKeys(value: unknown, keys: string[]): unknown;
```

**冻结的 merge 语义**（测试矩阵照此写，对应 DESIGN §2.7）：

| 情形 | 结果 |
|---|---|
| 嵌套对象 | 递归合并 |
| 数组 | 原子值：base≡local 取 remote；base≡remote 取 local；双方同改同值取该值；双方改且不同 → 冲突 `array-both-changed` |
| local 删键 / remote 未动 | 删除生效（对称：remote 删 / local 未动也删除）|
| local 删键 / remote 改值（及对称） | 冲突 `modify-vs-delete` |
| 双方改不同键 | 自动合并 |
| 双方改同键不同值 | 冲突 `both-modified` |
| 一方新增键 | 取新增方 |
| merge 分类内 `kind:'file'` 条目 | 由 drift 层降级按 mirror compareFile 处理（不炸、不参与 mergeJson）|

### 1.3 `src/core/engine/mirror.ts` — 判定引擎（mirror 半边）

```ts
export type MirrorOp =
  | { type: 'noop'; path: string }
  | { type: 'push'; path: string }         // local 改/增，remote 未变
  | { type: 'push-delete'; path: string }  // local 删，remote 未变（DESIGN 矩阵补全）
  | { type: 'pull'; path: string }         // remote 改/增，local 未变
  | { type: 'pull-delete'; path: string }  // remote 删，local 未变
  | { type: 'conflict'; path: string;
      reason: 'modify-vs-modify' | 'local-delete-vs-remote-modify' | 'local-modify-vs-remote-delete' };

export function compareFile(
  base: SnapshotEntry | undefined,
  local: SnapshotEntry | undefined,
  remote: SnapshotEntry | undefined,
  relPath: string,
): MirrorOp;   // 内容相等用字符串全等；不存在 = deleted/added

export function compareCategory(
  base: SnapshotFiles, local: SnapshotFiles, remote: SnapshotFiles,
): MirrorOp[]; // 三方路径并集逐文件 compareFile；双删 = noop
```

**mirror 9 情形**（补全 DESIGN 8 行为 3×3 全枚举）：双删→noop；local删+remote未动→push-delete；其余 7 种照 DESIGN §2.7 表。

### 1.4 `src/core/engine/drift.ts` — status 聚合层

```ts
export interface CategoryDrift {
  adapterId: string; category: string; mode: SyncMode;
  push: number; pull: number; conflicts: number;
  ops: MirrorOp[];                 // mirror 模式（含降级文件）
  mergeConflicts: MergeConflict[]; // merge 模式
  changedKeys: string[];           // merge 模式 local vs base 的变更键
}
export interface AdapterDrift {
  adapterId: string;
  categories: CategoryDrift[];
}
/** M1: remote 缺省 = base（store 即最后一次同步态）。push 计数含 push-delete；merge 模式 push = changed+added+deleted */
export function computeDrift(
  base: AdapterSnapshot[],
  local: AdapterSnapshot[],
  remote?: AdapterSnapshot[],
): AdapterDrift[];
```

### 1.5 `src/adapters/pi/` — pi adapter 只读扫描

```ts
// defaults.ts
export const PI_ADAPTER_ID = 'pi';
export const DEFAULT_PI_ADAPTER: AdapterConfig = {
  root: '~/.pi/agent',
  enabled: true,
  categories: {
    settings:   { paths: ['settings.json', 'keybindings.json'], mode: 'merge' },
    skills:     { paths: ['skills/'], mode: 'mirror' },
    extensions: { paths: ['extensions/'], mode: 'mirror', exclude: ['*cache*'] },
    agents:     { paths: ['agents/'], mode: 'mirror' },
    models:     { paths: ['models.json'], mode: 'merge', excludeKeys: ['apiKeys'] },
    prompts:    { paths: ['prompts/'], mode: 'mirror' },
    themes:     { paths: ['themes/'], mode: 'mirror' },
  },
  ignore: ['auth.json', 'trust.json', 'sessions/', 'npm/', 'git/', 'tmp/',
           'bin/', '*.bak', '*.bak-*', '*.bak*', '*.log', 'run-history.jsonl'],
};

// scan.ts
export interface ScanError { path: string; message: string; }
export interface ScanOutcome {
  snapshot: AdapterSnapshot;   // 仅含 root 存在且 enabled 的分类
  errors: ScanError[];         // root 不存在 → snapshot 空 + error；读失败不炸
}
export function scanAdapter(adapterId: string, config: AdapterConfig): ScanOutcome;

// ignore.ts（pi adapter 私有，但签名冻结）
export function matchesIgnore(relPath: string, patterns: string[]): boolean;
// glob 规则冻结：字面量 + '*' 任意非 '/' 字符 + 尾 '/' 目录前缀匹配；不引入 picomatch
```

### 1.6 `src/core/store/` + `src/core/config.ts` — store / 配置 / 路径

**Store 布局（冻结，scan 与 store 两个 worker 都遵守）**：`~/.homer/store/<adapterId>/<category>/<relPath>`。
relPath 规则：category 的 `paths` 中以 `/` 结尾的目录 → 该目录下的相对路径（如 `skills/foo/SKILL.md` → relPath `foo/SKILL.md`）；非目录单文件 → 文件名本身（如 `settings.json`）。

```ts
// paths.ts
export interface HomerPaths { home: string; storeDir: string; configFile: string; stateFile: string; }
export function getHomerPaths(env?: { HOMER_HOME?: string }): HomerPaths;
// HOMER_HOME 环境变量覆盖 ~/.homer —— 测试不碰真实 HOME 的关键

// config.ts
export type ConfigResult =
  | { ok: true; config: HomerConfig }
  | { ok: false; errors: string[] };
export function validateConfig(raw: unknown): ConfigResult;   // 手写校验，零依赖
export function loadConfig(paths: HomerPaths): HomerConfig | undefined;  // 文件不存在 → undefined
export function saveConfig(paths: HomerPaths, config: HomerConfig): void;

// store.ts
export function writeSnapshotToStore(paths: HomerPaths, snapshot: AdapterSnapshot): void;
export function readSnapshotFromStore(paths: HomerPaths, config: HomerConfig): AdapterSnapshot[];
// store 中文件 kind：按 category.mode === 'merge' 且可 parse → 'json'，否则 'file'
```

### 1.7 `src/cli/` — 命令层

```ts
// commands/init.ts —— M1 最小版：非交互，扫描→写 homer.json + store 快照
export interface InitOptions { homerHome?: string; adapters?: string[]; json?: boolean }
export interface InitReport {
  homerHome: string;
  adapters: { id: string; categories: { name: string; fileCount: number }[] }[];
}
export function runInit(opts: InitOptions): Promise<InitReport>;

// commands/status.ts
export interface StatusOptions { homerHome?: string; json?: boolean; verbose?: boolean }
export interface StatusReport {
  adapters: {
    id: string; push: number; pull: number; conflicts: number;
    categories: { name: string; push: number; pull: number; conflicts: number }[];
  }[];
}
export function runStatus(opts: StatusOptions): StatusReport;  // 无 config → 报错提示先 init，exit 1

// commands/diff.ts —— 文本级：merge 文件按键行、mirror 文件按行 diff（+/- 前缀）
export interface DiffOptions { homerHome?: string; adapter?: string; category?: string }
export function runDiff(opts: DiffOptions): string;           // 渲染好的多行文本
```

**冻结的 CLI 约定**：参数解析用 `node:util` 的 `parseArgs`（零运行时依赖）；`status --json` / `init --json` 输出上表结构的 JSON；有漂移时 status/diff 仍 exit 0（漂移是信息不是错误）；`--help` 打印用法。exit 1 仅用于：无 homer.json、路径不存在等真错误。

## 2. 模块拆分 / 依赖 / 验收

```
P0  M0-scaffold ──┬──> P1 并行 ──┬── M1-engine      (src/core/engine/)
                  │              ├── M2-pi-adapter   (src/adapters/pi/)
                  │              ├── M3-store        (src/core/store/, src/core/config.ts, src/core/paths.ts)
                  │              └── M4-cli          (src/cli/commands/, src/cli/render.ts)
                  └──────────────────────────────────> P2  M5-integrator (e2e + 缝隙修复)
```

### P0 · M0-scaffold（串行，最先，其余全部依赖它）

- **文件**：`package.json`、`tsconfig.json`（NodeNext, strict, ESM）、`vitest.config.ts`、`bin/homer.js`（薄壳，`import('../dist/cli/index.js')`，dev 走 tsx）、`src/cli/index.ts`（parseArgs 分发到 init/status/diff 占位）、`src/cli/args.ts`、**`src/core/types.ts`（按 §1.1 逐字落地）**、`.gitignore` 增补（dist/ node_modules/）、README 一节“M1 范围”。
- **依赖**：无。
- **验收**：`npm install && npm run typecheck && npm run test && node bin/homer.js --help` 四条全绿（vitest 配 `passWithNoTests: true`）；scripts 含 `typecheck`(tsc --noEmit) / `test`(vitest run) / `build`(tsc)。
- **硬约束**：`types.ts` 落地即冻结，后续任何 worker 不得修改；发现接口问题只能报回 orchestrator 统一改本计划。

### P1 · 四个并行 worker（互不重叠目录，均依赖 P0）

#### M1-engine — 三路判定引擎（纯函数，零 fs）

- **文件**：`src/core/engine/{merge.ts, mirror.ts, drift.ts, index.ts}` + `tests/engine/*.test.ts`。
- **依赖**：P0（只用 types.ts，**不得 import 其他模块**，保证纯函数可独立测试）。
- **验收**（DESIGN §4 要求的字段级测试矩阵）：
  - merge：§1.2 语义表逐行至少 1 个用例，≥12 个断言组；`diffJson` 增/改/删各覆盖；`stripKeys` 覆盖。
  - mirror：3×3 = 9 情形全枚举用例，含 `push-delete` 与双删 noop；`compareCategory` 并集用例（一方新增、一方删除、共同新增不同内容）。
  - drift：merge + mirror + “merge 分类混入 file 条目降级” 三类聚合用例；excludeKeys 剥离后不计 drift 的用例。
  - `npm run typecheck && npx vitest run tests/engine` 绿。

#### M2-pi-adapter — pi 只读扫描

- **文件**：`src/adapters/pi/{defaults.ts, scan.ts, ignore.ts, index.ts}` + `tests/adapters/pi/`（fixture 建在测试内用 `fs.mkdtemp`，**禁止碰真实 `~/.pi`**）。
- **依赖**：P0（types + DEFAULT_PI_ADAPTER 定义）。
- **验收**：
  - fixture 构造 7 分类各有代表文件 + 垃圾（`sessions/x`、`npm/y`、`auth.json`、`models.json.bak-predirect`、`pi-tui-crash.log`）→ 扫描结果快照精确匹配：垃圾 0 条、`settings.json` 为 `kind:'json'`、`extensions/*cache*` 被 exclude、JSON 损坏的 merge 文件降级 `kind:'file'`。
  - `matchesIgnore` 表驱动用例 ≥10 条（`*.bak-*`、`sessions/` 前缀、`*` 不跨 `/`）。
  - root 不存在的目录 → 空 snapshot + errors，不 throw。
  - **加分验收（可选）**：对真实 `~/.pi/agent` 跑一次只读 dry-run 脚本（仅读），人工核对无垃圾进入快照。

#### M3-store — 路径 / 配置 / store 读写

- **文件**：`src/core/paths.ts`、`src/core/config.ts`、`src/core/store/store.ts` + `tests/store/*.test.ts`（全部走 `HOMER_HOME=$(mktemp -d)`）。
- **依赖**：P0。
- **验收**：
  - `HOMER_HOME` 覆盖生效；默认路径 `~/.homer`。
  - `validateConfig`：合法 / 缺 version / 错 mode / 空 paths / root 非 string 等 ≥6 用例。
  - 快照 roundtrip：构造 2 分类（merge+mirror，含子目录文件）→ `writeSnapshotToStore` → `readSnapshotFromStore` → 深比较相等；store 布局符合 §1.6（用固定断言路径，如 `store/pi/settings/settings.json`）。
  - 增量写入语义冻结：`writeSnapshotToStore` 覆盖该 adapter 目录（先清空 `<storeDir>/<adapterId>/` 再写）。

#### M4-cli — init / status / diff 命令

- **文件**：`src/cli/commands/{init.ts, status.ts, diff.ts}`、`src/cli/render.ts`、更新 `src/cli/index.ts` 分发 + `tests/cli/*.test.ts`。
- **依赖**：P0 的 types + **冻结接口**（engine/scan/store 的签名按本计划编码；单测中用手工构造的 snapshot 打桩，不 import P1 三模块的实现，避免并行期编译失败）。
- **验收**：
  - status：手工 snapshot（base==remote，local 改 1 键 + 增 1 文件 + 删 1 文件）→ `push=3, pull=0`；反向构造 → `↓` 计数；`--json` 结构符合 `StatusReport`。
  - diff：merge 键行 `key: old → new`；mirror 行级 `+/-`；空输出 = 无漂移。
  - init：打桩 scan + 真 store → `InitReport` 正确；已存在 homer.json 时拒绝覆盖（提示 + exit 1），除非 `--force`（P1 期间可先只做拒绝分支）。

### P2 · M5-integrator（串行收尾，依赖 P1 全部四个）

- **文件**：`tests/e2e/m1.test.ts`、缝隙修复（只许改自己引入的缝合点；接口问题上报不私改 types.ts）、`docs/m1-report.md`（验收记录）。
- **验收（M1 里程碑级，全绿才算 M1 完成）**：
  1. `npm run typecheck && npm test` 全量绿（含 P1 全部测试）。
  2. e2e（全程临时目录 + `HOMER_HOME`）：构造 pi fixture → `homer init` → 断言 store 出现 7 分类文件、homer.json 合法 → 改 settings.json 一个键、新增 skills 文件、删一个 skill、直改 store 中一个 theme 文件 → `homer status` 断言 `↑3 ↓1`（或按构造精确断言）→ `homer status --json` 可 `JSON.parse` 且计数一致 → `homer diff` 输出含该键新旧值 → `homer diff` 对无漂移分类输出空。
  3. 真实环境冒烟（只读，可选手动）：`HOMER_HOME=/tmp/homer-smoke homer init --json` 后 `homer status` 应为 `↑0 ↓0`，且 store 无 auth.json/sessions/npm/*.bak。

## 3. 风险与回滚点

1. **接口漂移**（最易错）：四个并行 worker 各自“顺手改 types.ts”= 冲突灾难。对策：types.ts 由 P0 独家落地并 git commit 作为检查点；P1 worker 目录互斥（engine/ pi/ store+config/ cli/commands+render）；发现接口缺陷一律报 orchestrator，由其统一修订计划并广播。
2. **relPath 布局二义性**：scan 侧与 store 侧对“目录型 category 的相对路径”理解不一致会导致 e2e 全挂。对策：§1.6 布局规则在两方测试里都用硬编码绝对断言路径锁死（M3 roundtrip 测试 + M2 fixture 断言），最先不一致会在各自单测暴露而非 e2e。
3. **ignore glob 语义膨胀**：worker 现场发明 `**` / `?` 语义会埋兼容坑。对策：glob 规则在 §1.5 冻结为“字面量 + `*` 非跨 `/` + 尾 `/` 目录前缀”，表驱动测试锁死；未来需要更强 glob 时是显式 breaking change。

回滚点：P0 commit（骨架+types）；P1 各模块独立 commit（engine / pi / store / cli 各一）；任一 P1 模块失败 2 次重试后回滚该模块 commit 并降级为串行修复，不阻塞其余三个。

## 4. 机器可验收标准（M1 Done 定义）

```bash
npm run typecheck && npm test        # 全量绿，含 engine 矩阵 / pi fixture / store roundtrip / cli / e2e
HOMER_HOME=$(mktemp -d) node bin/homer.js init --json   # 退出码 0，stdout 为合法 JSON
HOMER_HOME=<同上> node bin/homer.js status --json       # parseable，drift 计数与构造一致
```
