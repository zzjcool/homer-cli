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
import { stripExcludeKeys } from '../core/engine/index.js';
import { readSnapshotFromStore } from '../core/store/store.js';
import { scanAdapter } from '../adapters/pi/index.js';
import type { AdapterSnapshot, HomerConfig } from '../core/types.js';

const UP = '↑';
const DOWN = '↓';

/* ------------------------------------------------------------------ */
/* CLI 通用工具                                                        */
/* ------------------------------------------------------------------ */

/**
 * 可预期的用户级错误（配置缺失 / 拒绝覆盖等）。
 * 分发层捕获后打印 `message`（+ 可选 `hint`）并以退出码 1 结束。
 */
export class CliError extends Error {
  readonly hint: string | undefined;

  constructor(message: string, hint?: string) {
    super(message);
    this.name = 'CliError';
    this.hint = hint;
  }
}

/**
 * 解析 homer 工作区路径。
 * `homerHome`（来自 `--home` 或编程调用）优先；未给出时回落到 `HOMER_HOME` 环境变量 / `~/.homer`
 * （由 `getHomerPaths` 的默认参数处理）。
 */
export function resolveHomerPaths(homerHome?: string): HomerPaths {
  return homerHome !== undefined ? getHomerPaths({ HOMER_HOME: homerHome }) : getHomerPaths();
}

/**
 * 三方判定原料（§1.4）：
 *   base   = store 快照（最后一次同步态）
 *   local  = 各 enabled adapter root 的实时扫描
 *   remote = M1 缺省 = base；M2 接 git remote 后由调用方注入
 */
export interface CliDriftSources {
  base: AdapterSnapshot[];
  local: AdapterSnapshot[];
  remote?: AdapterSnapshot[];
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
 */
export function collectSnapshotSources(paths: HomerPaths, config: HomerConfig): CliDriftSources {
  const base = readSnapshotFromStore(paths, config).map((snapshot) => stripAdapterExcludedKeys(snapshot, config));

  const local: AdapterSnapshot[] = [];
  for (const [adapterId, adapterConfig] of Object.entries(config.adapters)) {
    if (adapterConfig.enabled === false) continue;
    const outcome = scanAdapter(adapterId, adapterConfig);
    local.push(stripAdapterExcludedKeys(outcome.snapshot, config));
  }

  return { base, local };
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
}

/** renderInit 只依赖这个结构化形状，避免与 commands/init.ts 产生循环 import。 */
export interface InitReportLike {
  homerHome: string;
  adapters: { id: string; categories: { name: string; fileCount: number }[] }[];
}

export interface RenderStatusOptions {
  /** 打印每个分类的计数（默认只打 adapter 汇总）。 */
  verbose?: boolean;
}

/**
 * status 人类可读输出：
 *
 *   pi  ↑3 ↓0
 *     settings  ↑1 ↓0
 *     skills  ↑2 ↓0
 *
 * 全零时追加一行 `无漂移`。
 */
export function renderStatus(report: StatusReportLike, opts: RenderStatusOptions = {}): string {
  if (report.adapters.length === 0) return '（homer.json 中没有启用的 adapter）';

  const lines: string[] = [];
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

/** init 人类可读输出：homer 工作区 + 每个 adapter / 分类写入了多少文件。 */
export function renderInit(report: InitReportLike): string {
  const lines: string[] = [`homer init: ${report.homerHome}`];
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
