/**
 * CLI 支撑模块（docs/m1-plan.md §1.7 / §2-P1-M4）。
 *
 * 两部分，刻意分开：
 *   1. 纯渲染函数（renderStatus / renderInit / formatValue / splitLines / diffLines）
 *      —— 只做「数据 → 文本」，不碰 fs / argv / stdout，便于单测。
 *   2. CLI 通用工具（CliError / resolveHomerPaths / collectSnapshotSources）
 *      —— 三个命令共用的路径解析、错误类型与三方快照采集，避免各命令重复实现。
 *
 * 命令模块（commands/*.ts）产出数据，分发层（index.ts）负责打印；
 * 二者之间的格式化全部收敛在这里。
 */

import { getHomerPaths, type HomerPaths } from '../core/paths.js';
import { CliError } from '../core/errors.js';
import { stripExcludeKeys } from '../core/engine/index.js';
import { gitExec, isGitRepo, isStoreClean, readStoreSnapshotAtCommit, upstreamRef } from '../core/git/index.js';
import { readSnapshotFromStore } from '../core/store/store.js';
import { scanAdapter, type ScanError } from '../adapters/pi/index.js';
import type { AdapterSnapshot, HomerConfig } from '../core/types.js';

// CliError 现在定义在 core/errors.ts（store 也要抛它，避免 core → cli 反向依赖）；
// 这里 re-export，既有 `import { CliError } from '../render.js'` 的调用方无需改动。
export { CliError };

const UP = '↑';
const DOWN = '↓';

/* ------------------------------------------------------------------ */
/* CLI 通用工具                                                        */
/* ------------------------------------------------------------------ */

/**
 * 解析 homer 工作区路径。
 * `homerHome`（来自 `--home` 或编程调用）优先；未给出时回落到 `HOMER_HOME` 环境变量 / `~/.homer`
 * （由 `getHomerPaths` 的默认参数处理）。
 */
export function resolveHomerPaths(homerHome?: string): HomerPaths {
  return homerHome !== undefined ? getHomerPaths({ HOMER_HOME: homerHome }) : getHomerPaths();
}

/**
 * 采集期错误条目（additive，M-A）：live scan 的 ScanOutcome.errors 按 adapterId 聚合。
 * 缺失 adapter root 时 local 快照为空，若丢弃 errors 会把「扫不到」静默当成「本地全空」
 * → status 报出全量 push 的假漂移。保留 errors 让命令层能显式提示并避免误报。
 *
 * `rootUnreadable`：整个 root 不可读（不存在 / 不是目录）。文本层据此决定告警措辞：
 * 根级失败 → `⚠ adapter root 不可读: <id>`（M-A 要求）；其余（如 symlink 逃逸）→ `⚠ 扫描告警`。
 */
export type SnapshotSourceErrors = { adapterId: string; rootUnreadable: boolean; errors: ScanError[] }[];

/**
 * 三方判定原料（§1.4）：
 *   base   = store 快照（最后一次同步态）
 *   local  = 各 enabled adapter root 的实时扫描
 *   remote = M2-W10 起：git 仓库存在且 upstream 可读时 = upstream 快照；否则 = base（M1 语义）
 */
export interface CliDriftSources {
  base: AdapterSnapshot[];
  local: AdapterSnapshot[];
  remote?: AdapterSnapshot[];
  /**
   * 采集期错误（可选：`collectSnapshotSources` 永远填它；单测 / M2 手工注入时可省略 = 无告警）。
   */
  errors?: SnapshotSourceErrors;
  /**
   * 采集期告警（additive，M2-W10）：非致命但用户必须看见的事实。
   * 目前唯一来源是「store 工作区脏」（直改 store 的 ↓ 计数依据的远端/基线不一致，需先 push）。
   * 文本层渲染为置顶 `⚠` 行，`--json` 消费者按 `warnings.length > 0` 判定。
   */
  warnings?: string[];
}

/** 单条采集错误 → 人类可读文本（路径 + 原因）。 */
export function formatScanError(error: ScanError): string {
  return `${error.path}: ${error.message}`;
}

/**
 * 采集错误 → 报告层字符串（`errors: string[]`，StatusReport / InitReport 的 additive 字段）。
 * 措辞区分根级失败（root 不可读）与其它扫描告警，避免对「root 明明可读」的场景误报。
 */
export function sourceErrorMessages(errors: SnapshotSourceErrors): string[] {
  return errors.flatMap((entry) =>
    entry.errors.map((error) => {
      const prefix = entry.rootUnreadable ? 'adapter root 不可读' : '扫描告警';
      return `${prefix}: ${entry.adapterId} (${formatScanError(error)})`;
    }),
  );
}

/**
 * root 级失败判定：扫描结果空分类 + 有错 → 整个 root 不可读（不存在 / 不是目录）。
 * 用于 status 抑制「local 视作全空」造成的假 push。
 */
function isRootUnreadable(outcome: { snapshot: AdapterSnapshot; errors: ScanError[] }): boolean {
  return outcome.snapshot.categories.length === 0 && outcome.errors.length > 0;
}

/** 把单个 adapter 的 ScanOutcome 转为采集错误条目（无错时返回 undefined）。 */
export function scanOutcomeError(adapterId: string, outcome: { snapshot: AdapterSnapshot; errors: ScanError[] }): SnapshotSourceErrors[number] | undefined {
  if (outcome.errors.length === 0) return undefined;
  return { adapterId, rootUnreadable: isRootUnreadable(outcome), errors: outcome.errors };
}

/** 对单个快照应用该 adapter 各分类的 excludeKeys 剥离（§1.2：drift 比较前对 base / local 各调一次）。 */
function stripAdapterExcludedKeys(snapshot: AdapterSnapshot, config: HomerConfig): AdapterSnapshot {
  const adapterConfig = config.adapters[snapshot.adapterId];
  if (adapterConfig === undefined) return snapshot;

  const excludeKeysByCategory: Record<string, string[]> = {};
  for (const [name, category] of Object.entries(adapterConfig.categories)) {
    if (category.excludeKeys !== undefined && category.excludeKeys.length > 0) {
      excludeKeysByCategory[name] = category.excludeKeys;
    }
  }
  if (Object.keys(excludeKeysByCategory).length === 0) return snapshot;
  return stripExcludeKeys(snapshot, excludeKeysByCategory);
}

/**
 * 从 store + 实时扫描采集三方判定原料。
 * base / local 都按 config 的 enabled adapter & category 结构对齐（store 侧保证同构），
 * 这样 local 新增/删除才会被判成 push。
 *
 * live scan 的 `ScanOutcome.errors` 一律保留在返回值的 `errors` 里（M-A）：
 * root 不可读时 local 快照会退化为空，若直接参与比较则 base 里的每个文件都会被算成
 * push-delete（假漂移）。因此 root 不可读的 adapter 在判定时 local 视作 = base（零漂移），
 * 依靠 `errors` 把原因暴露给命令层。
 *
 * ## M2-W10：remote 注入（additive）
 *
 * - `paths.home` **是 git 仓库根**且 `@{upstream}` 可解析 → `git fetch` 后
 *   `remote` = 该 commit 的 store 快照。于是 `homer status` / `homer diff` 的 `↓`（pull）
 *   计数在 git 模式下真正激活（M1 已知限制 #1：「↓ 恒为 0」由此关闭）。
 *   base 仍是 store 工作区（M1 语义，status 不依赖 state.json）。
 * - **会 `git fetch`**（与 `collectSyncSources` 默认一致）：否则远端刚推进的 commit 不可见，
 *   `↓` 依旧恒 0。fetch 失败（离线 / 无网）→ `⚠` 告警 + `remote` 缺省（回落 = base），
 *   status 仍然可用、exit 码不变。
 * - 无 upstream / upstream 不可解析 → `remote` 缺省（调用方回落 = base），静默（不报错）。
 * - **store 工作区脏** → 置顶 `⚠` 告警：直改 store 后 base（工作区）与 HEAD / upstream 不再自洽，
 *   push 才能把三者对齐。
 */
export function collectSnapshotSources(paths: HomerPaths, config: HomerConfig): CliDriftSources {
  const base = readSnapshotFromStore(paths, config).map((snapshot) => stripAdapterExcludedKeys(snapshot, config));

  const local: AdapterSnapshot[] = [];
  const errors: SnapshotSourceErrors = [];
  for (const [adapterId, adapterConfig] of Object.entries(config.adapters)) {
    if (adapterConfig.enabled === false) continue;
    const outcome = scanAdapter(adapterId, adapterConfig);
    const entry = scanOutcomeError(adapterId, outcome);
    if (entry !== undefined) errors.push(entry);

    if (entry?.rootUnreadable === true) {
      // root 不可读：不把「扫不到」当成「本地全空」。
      const baseSnapshot = base.find((snapshot) => snapshot.adapterId === adapterId);
      local.push(baseSnapshot ?? stripAdapterExcludedKeys(outcome.snapshot, config));
      continue;
    }
    local.push(stripAdapterExcludedKeys(outcome.snapshot, config));
  }

  const warnings: string[] = [];

  // store 脏：master（HEAD）/ upstream 与工作区脱节，↓ 计数可能含刚被直改的字节。
  if (isGitRepo(paths.home) && !isStoreClean(paths.home)) {
    warnings.push(
      'store 工作区有未提交的改动（漂移计数以 git 基线为准，可能未反映刚直改的内容）；如需提交请运行 `homer push`',
    );
  }

  const remote = collectGitRemote(paths, config, warnings);

  const sources: CliDriftSources = { base, local, errors, warnings };
  if (remote !== undefined) sources.remote = remote;
  return sources;
}

/**
 * `remote` = `@{upstream}` 的 store 快照；不可得（非仓库 / 无 upstream / ref 不可解析）→ undefined
 * （调用方回落 M1 语义 = base）。
 *
 * fetch 失败 → `warnings` 里记一条 + 返回 undefined（远端变更不可见，status 仍可用）。
 */
function collectGitRemote(
  paths: HomerPaths,
  config: HomerConfig,
  warnings: string[],
): AdapterSnapshot[] | undefined {
  if (!isGitRepo(paths.home)) return undefined;

  const ref = upstreamRef(paths.home);
  if (ref === undefined) return undefined;

  // fetch 是「↓」能真正激活的前提：不 fetch 则 origin/main 停留在上次同步点，↓ 恒 0。
  const fetched = gitExec(paths.home, ['fetch']);
  if (!fetched.ok) {
    const reason = fetched.stderr.trim() === '' ? '未知错误' : fetched.stderr.trim();
    warnings.push(`git fetch 失败（远端变更不可见，↓ 计数可能偏小）: ${reason}`);
    return undefined;
  }

  const resolved = gitExec(paths.home, ['rev-parse', '--verify', `${ref}^{commit}`]);
  const commit = resolved.ok ? resolved.stdout.trim() : '';
  if (commit === '') return undefined;

  return readStoreSnapshotAtCommit(paths, config, commit).map((snapshot) => stripAdapterExcludedKeys(snapshot, config));
}

/* ------------------------------------------------------------------ */
/* 纯渲染函数                                                          */
/* ------------------------------------------------------------------ */

/** renderStatus 只依赖这个结构化形状，避免与 commands/status.ts 产生循环 import。 */
export interface StatusReportLike {
  adapters: {
    id: string;
    push: number;
    pull: number;
    conflicts: number;
    categories: { name: string; push: number; pull: number; conflicts: number }[];
  }[];
  /** 采集期错误（additive）：人类可读输出以 ⚠ 头行提示。 */
  errors?: string[];
  /** 采集期告警（additive，M2-W10）：同样以 ⚠ 头行置顶。 */
  warnings?: string[];
}

/** renderInit 只依赖这个结构化形状，避免与 commands/init.ts 产生循环 import。 */
export interface InitReportLike {
  homerHome: string;
  adapters: { id: string; categories: { name: string; fileCount: number }[] }[];
  /** 采集期错误（additive）：root 缺失时 init 也打印明显提示。 */
  errors?: string[];
}

export interface RenderStatusOptions {
  /** 打印每个分类的计数（默认只打 adapter 汇总）。 */
  verbose?: boolean;
}

/**
 * status 人类可读输出：
 *
 *   ⚠ adapter root 不可读: pi (/home/u/.pi/agent: ENOENT ...)
 *   pi  ↑3 ↓0
 *     settings  ↑1 ↓0
 *     skills  ↑2 ↓0
 *
 * 全零时追加一行 `无漂移`。采集错误（errors，可选）一律置顶为 ⚠ 头行。
 */
export function renderStatus(report: StatusReportLike, opts: RenderStatusOptions = {}): string {
  const warningLines = renderSourceWarnings([...(report.errors ?? []), ...(report.warnings ?? [])]);

  if (report.adapters.length === 0) {
    return [...warningLines, '（homer.json 中没有启用的 adapter）'].join('\n');
  }

  const lines: string[] = [...warningLines];
  let totalPush = 0;
  let totalPull = 0;
  let totalConflicts = 0;

  for (const adapter of report.adapters) {
    totalPush += adapter.push;
    totalPull += adapter.pull;
    totalConflicts += adapter.conflicts;
    lines.push(`${adapter.id}  ${UP}${adapter.push} ${DOWN}${adapter.pull}${conflictTag(adapter.conflicts)}`);
    if (opts.verbose === true) {
      for (const category of adapter.categories) {
        lines.push(`  ${category.name}  ${UP}${category.push} ${DOWN}${category.pull}${conflictTag(category.conflicts)}`);
      }
    }
  }

  if (totalPush === 0 && totalPull === 0 && totalConflicts === 0) lines.push('无漂移');
  return lines.join('\n');
}

function conflictTag(conflicts: number): string {
  return conflicts > 0 ? `  冲突${conflicts}` : '';
}

/** 采集错误 → `⚠ <message>` 头行。 */
function renderSourceWarnings(errors: string[]): string[] {
  return errors.map((message) => `⚠ ${message}`);
}

/** init 人类可读输出：homer 工作区 + 每个 adapter / 分类写入了多少文件。 */
export function renderInit(report: InitReportLike): string {
  const lines: string[] = [`homer init: ${report.homerHome}`];
  if (report.errors && report.errors.length > 0) {
    lines.push(...renderSourceWarnings(report.errors).map((line) => `  ${line}`));
  }
  if (report.adapters.length === 0) {
    lines.push('（没有要初始化的 adapter）');
    return lines.join('\n');
  }
  for (const adapter of report.adapters) {
    const total = adapter.categories.reduce((sum, category) => sum + category.fileCount, 0);
    lines.push(`  ${adapter.id}: ${total} 个文件`);
    for (const category of adapter.categories) {
      lines.push(`    ${category.name}: ${category.fileCount}`);
    }
  }
  return lines.join('\n');
}

/**
 * 单值渲染（merge 键行的 old / new）：
 * 字符串直接原样输出（`theme: light → dark`，不加引号），
 * 其余标量 / 对象 / 数组用紧凑 JSON；`undefined`（键不存在）→ `(无)`。
 */
export function formatValue(value: unknown): string {
  if (value === undefined) return '(无)';
  if (typeof value === 'string') return value;
  if (typeof value === 'number' || typeof value === 'boolean' || value === null) {
    return JSON.stringify(value);
  }
  const json = JSON.stringify(value);
  return json === undefined ? String(value) : json;
}

/** 文本按行切分；末尾单个换行不产生空行；空串 → []。 */
export function splitLines(text: string): string[] {
  if (text === '') return [];
  const lines = text.split('\n');
  if (lines.length > 0 && lines[lines.length - 1] === '') lines.pop();
  return lines;
}

/**
 * 行级 diff（LCS，零依赖），前缀 `-` / `+` / 空格（上下文）。
 * 文件新增 = 全文 `+`；文件删除 = 全文 `-`；内容相同 → []。
 */
export function diffLines(before: string, after: string): string[] {
  if (before === after) return [];
  const a = splitLines(before);
  const b = splitLines(after);

  // dp[i][j] = a[i..] 与 b[j..] 的 LCS 长度
  const dp: number[][] = Array.from({ length: a.length + 1 }, () => new Array<number>(b.length + 1).fill(0));
  for (let i = a.length - 1; i >= 0; i -= 1) {
    const row = dp[i];
    const next = dp[i + 1];
    if (row === undefined || next === undefined) continue;
    for (let j = b.length - 1; j >= 0; j -= 1) {
      const av = a[i];
      const bv = b[j];
      row[j] = av === bv ? (next[j + 1] ?? 0) + 1 : Math.max(next[j] ?? 0, row[j + 1] ?? 0);
    }
  }

  const out: string[] = [];
  let i = 0;
  let j = 0;
  while (i < a.length && j < b.length) {
    const av = a[i] ?? '';
    const bv = b[j] ?? '';
    if (av === bv) {
      out.push(` ${av}`);
      i += 1;
      j += 1;
    } else if ((dp[i + 1]?.[j] ?? 0) >= (dp[i]?.[j + 1] ?? 0)) {
      out.push(`-${av}`);
      i += 1;
    } else {
      out.push(`+${bv}`);
      j += 1;
    }
  }
  while (i < a.length) {
    out.push(`-${a[i] ?? ''}`);
    i += 1;
  }
  while (j < b.length) {
    out.push(`+${b[j] ?? ''}`);
    j += 1;
  }
  return out;
}
