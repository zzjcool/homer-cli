/**
 * `homer diff` —— 漂移的详细差异（docs/m1-plan.md §1.7 / §2-P1-M4）。
 *
 * 冻结签名（§1.7）：
 *   DiffOptions / runDiff(opts): string   —— 渲染好的多行文本
 *
 * 输出约定（计划原文）：
 *   - merge 键行：`key: old → new`（缺失一侧渲染为 `(无)`）
 *   - mirror 文件：行级 `+` / `-`（LCS）
 *   - 空输出 = 无漂移（调用方据此判断；不打印「无漂移」占位，保证脚本可判空）
 *
 * `sources` 第二参数与 status 一致：单测 / M2 注入三方快照用；生产不传时走 store + 实时扫描。
 */

import { computeDrift, type AdapterDrift, type CategoryDrift, type MirrorOp } from '../../core/engine/index.js';
import type { AdapterSnapshot, SnapshotEntry, SnapshotFiles } from '../../core/types.js';
import { isPlainObject } from '../../core/entry-kind.js';
import { loadConfig } from '../../core/config.js';
import {
  CliError,
  collectSnapshotSources,
  diffLines,
  formatValue,
  resolveHomerPaths,
  sourceErrorMessages,
  type CliDriftSources,
} from '../render.js';

export interface DiffOptions {
  homerHome?: string;
  adapter?: string;
  category?: string;
}

export const DIFF_USAGE = `用法: homer diff [options]

显示漂移的详细差异：
  - merge 文件：按键行输出 \`key: old → new\`
  - mirror 文件：按行输出 \`+\` / \`-\`（LCS diff）

选项:
  --home <dir>       homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --adapter <id>     只看某个 adapter
  --category <name>  只看某个分类
  -h, --help         显示本帮助

无漂移时输出为空（exit 0）。`;

/* ------------------------------------------------------------------ */
/* JSON 键级 diff（渲染用，取 old/new 值）                              */
/* ------------------------------------------------------------------ */

interface JsonSlot { has: boolean; value: unknown }

interface KeyRow { path: string; before: unknown; after: unknown }

/**
 * merge 键行的取值：仅当条目标记为 kind:'json' 时才尝试解析。
 * 解析失败（如 kind:'json' 但内容损坏）→ undefined，调用方据此把该文件降级为
 * mirror 行级 diff（与 drift.ts 的 isDegraded 对齐），不再走「宽松解析」的第三套逻辑。
 */
function slotOf(entry: SnapshotEntry | undefined): JsonSlot | undefined {
  if (entry === undefined) return { has: false, value: undefined };
  if (entry.kind !== 'json') return undefined;
  try {
    return { has: true, value: JSON.parse(entry.content) as unknown };
  } catch {
    return undefined;
  }
}

function isJsonEntry(entry: SnapshotEntry | undefined): boolean {
  return entry === undefined || entry.kind === 'json';
}

function childSlot(parent: JsonSlot, key: string): JsonSlot {
  if (!parent.has || !isPlainObject(parent.value)) return { has: false, value: undefined };
  // hasOwnProperty 守卫：`__proto__` / `constructor` 不得沿原型链取值（同 merge.ts childSlot）。
  if (!Object.prototype.hasOwnProperty.call(parent.value, key)) return { has: false, value: undefined };
  return { has: true, value: parent.value[key] };
}

function slotsEqual(a: JsonSlot, b: JsonSlot): boolean {
  if (a.has !== b.has) return false;
  if (!a.has) return true;
  return JSON.stringify(a.value) === JSON.stringify(b.value);
}

/**
 * 递归收集键变更（与引擎 diffJson 的粒度对齐：双方都是对象 → 递归到叶子；
 * 整体新增/删除的子树只记子树根键）。根值非对象时记 `$`。
 * 双侧 slot 均为 undefined 表示「该条目不可 JSON 解析」→ 整体跳过（走行级 diff）。
 */
function collectKeyDiffs(base: JsonSlot | undefined, local: JsonSlot | undefined, prefix: string, out: KeyRow[]): void {
  if (base === undefined || local === undefined) return;
  if (!base.has && !local.has) return;

  if (base.has && local.has && isPlainObject(base.value) && isPlainObject(local.value)) {
    const keys = new Set<string>([...Object.keys(base.value), ...Object.keys(local.value)]);
    for (const key of [...keys].sort()) {
      collectKeyDiffs(childSlot(base, key), childSlot(local, key), prefix === '' ? key : `${prefix}.${key}`, out);
    }
    return;
  }

  if (slotsEqual(base, local)) return;
  out.push({
    path: prefix === '' ? '$' : prefix,
    before: base.has ? base.value : undefined,
    after: local.has ? local.value : undefined,
  });
}

/* ------------------------------------------------------------------ */
/* 渲染                                                                */
/* ------------------------------------------------------------------ */

function findFiles(snapshots: readonly AdapterSnapshot[], adapterId: string, category: string): SnapshotFiles {
  return snapshots.find((s) => s.adapterId === adapterId)?.categories.find((c) => c.category === category)?.files
    ?? new Map<string, SnapshotEntry>();
}

/**
 * 渲染一个文件的差异（base→local 方向）。返回是否真的产出了行。
 * 第三方向（remote，仅当 remote !== base 时）以 `远端(↓)` 段落追加。
 */
function renderFileDiff(
  out: string[],
  relPath: string,
  base: SnapshotEntry | undefined,
  local: SnapshotEntry | undefined,
  remote: SnapshotEntry | undefined,
  conflict = false,
): boolean {
  const collect = (from: string, to: string): string[] => diffLines(from, to);

  let lines: string[] = [];
  if (base === undefined && local !== undefined) {
    lines = collect('', local.content);
  } else if (base !== undefined && local === undefined) {
    lines = collect(base.content, '');
  } else if (base !== undefined && local !== undefined && base.content !== local.content) {
    lines = collect(base.content, local.content);
  }

  let remoteLines: string[] = [];
  const remoteChanged = base === undefined ? remote !== undefined : remote === undefined || remote.content !== base.content;
  if (remoteChanged) {
    remoteLines = collect(base?.content ?? '', remote?.content ?? '');
  }

  if (lines.length === 0 && remoteLines.length === 0) return false;

  out.push(`  ${conflictMark(conflict)}${relPath}`);
  out.push(...lines);
  if (remoteLines.length > 0) {
    out.push('    远端(↓)');
    out.push(...remoteLines);
  }
  return true;
}

/** 冲突标记前缀（minor 3）：命中 mergeConflicts / conflict op 的行以 `⚡` 打头。 */
const CONFLICT_MARK = '⚡ ';

function conflictMark(conflict: boolean): string {
  return conflict ? CONFLICT_MARK : '';
}

function renderMirrorCategory(
  out: string[],
  adapterId: string,
  drift: CategoryDrift,
  base: SnapshotFiles,
  local: SnapshotFiles,
  remote: SnapshotFiles,
): void {
  let rendered = false;
  for (const op of drift.ops) {
    if (op.type === 'noop') continue;
    if (!rendered) {
      out.push(`${adapterId}/${drift.category}`);
      rendered = true;
    }
    renderFileDiff(out, op.path, base.get(op.path), local.get(op.path), remote.get(op.path), op.type === 'conflict');
  }
}

function renderMergeCategory(
  out: string[],
  adapterId: string,
  drift: CategoryDrift,
  base: SnapshotFiles,
  local: SnapshotFiles,
  remote: SnapshotFiles,
): void {
  // 降级文件（merge 分类里 kind:'file'）由 ops 接管，走行级 diff。
  const degraded = new Set(drift.ops.filter((op) => op.type !== 'noop').map((op) => op.path));
  // 冲突打标（minor 3）：mergeConflicts 的 keyPath 既可能是键点路径（'theme'），
  // 也可能是文件级路径（relPath，来自 base 缺失 / modify-vs-delete 等文件级分支）。
  const conflictKeys = new Set(drift.mergeConflicts.map((conflict) => conflict.keyPath));
  const conflictOpPaths = new Set(drift.ops.filter((op) => op.type === 'conflict').map((op) => op.path));
  const fileConflicts = (relPath: string): boolean => conflictKeys.has(relPath) || conflictOpPaths.has(relPath);
  const keyConflicts = (keyPath: string): boolean => conflictKeys.has(keyPath);

  let rendered = false;
  const head = (): void => {
    if (!rendered) {
      out.push(`${adapterId}/${drift.category}`);
      rendered = true;
    }
  };

  for (const op of drift.ops) {
    if (op.type === 'noop') continue;
    head();
    renderFileDiff(out, op.path, base.get(op.path), local.get(op.path), remote.get(op.path), op.type === 'conflict');
  }

  const paths = new Set<string>([...base.keys(), ...local.keys(), ...remote.keys()]);
  for (const relPath of [...paths].sort()) {
    if (degraded.has(relPath)) continue;

    const baseEntry = base.get(relPath);
    const localEntry = local.get(relPath);
    const remoteEntry = remote.get(relPath);
    if (!isJsonEntry(baseEntry) || !isJsonEntry(localEntry) || !isJsonEntry(remoteEntry)) continue;

    const rows: KeyRow[] = [];
    collectKeyDiffs(slotOf(baseEntry), slotOf(localEntry), '', rows);

    const remoteRows: KeyRow[] = [];
    if (remoteEntry?.content !== baseEntry?.content) {
      collectKeyDiffs(slotOf(baseEntry), slotOf(remoteEntry), '', remoteRows);
    }
    if (rows.length === 0 && remoteRows.length === 0) continue;

    head();
    out.push(`  ${conflictMark(fileConflicts(relPath))}${relPath}`);
    for (const row of rows) {
      out.push(`    ${conflictMark(keyConflicts(row.path))}${row.path}: ${formatValue(row.before)} → ${formatValue(row.after)}`);
    }
    for (const row of remoteRows) {
      out.push(`    ↓ ${conflictMark(keyConflicts(row.path))}${row.path}: ${formatValue(row.before)} → ${formatValue(row.after)}`);
    }
  }
}

function isEmptyDrift(drift: AdapterDrift): boolean {
  return drift.categories.every((c) => c.push === 0 && c.pull === 0 && c.conflicts === 0);
}

/**
 * 渲染漂移差异文本。无漂移 → 空字符串。
 * 无 homer.json → 抛 CliError（分发层提示先 init，exit 1）。
 *
 * 采集告警（M-A）以 `⚠ ...` 行置顶：diff 的空输出是脚本判据，
 * root 不可读时不能静默输出空（那会被误读为「无漂移」）。
 */
export function runDiff(opts: DiffOptions, sources?: CliDriftSources): string {
  const paths = resolveHomerPaths(opts.homerHome);
  const config = loadConfig(paths);
  if (config === undefined) {
    throw new CliError(
      `未找到 homer 配置: ${paths.configFile}`,
      '请先运行 `homer init` 生成 homer.json 与 store 快照。',
    );
  }

  const src: CliDriftSources = sources ?? collectSnapshotSources(paths, config);
  const drifts = computeDrift(src.base, src.local, src.remote);
  const remoteSnapshots = src.remote ?? src.base;

  const out: string[] = sourceErrorMessages(src.errors ?? []).map((message) => `⚠ ${message}`);
  for (const adapter of drifts) {
    if (opts.adapter !== undefined && adapter.adapterId !== opts.adapter) continue;
    if (isEmptyDrift(adapter)) continue;

    for (const category of adapter.categories) {
      if (opts.category !== undefined && category.category !== opts.category) continue;
      if (category.push === 0 && category.pull === 0 && category.conflicts === 0) continue;

      const base = findFiles(src.base, adapter.adapterId, category.category);
      const local = findFiles(src.local, adapter.adapterId, category.category);
      const remote = findFiles(remoteSnapshots, adapter.adapterId, category.category);

      if (category.mode === 'mirror') {
        renderMirrorCategory(out, adapter.adapterId, category, base, local, remote);
      } else {
        renderMergeCategory(out, adapter.adapterId, category, base, local, remote);
      }
    }
  }

  return out.join('\n');
}
