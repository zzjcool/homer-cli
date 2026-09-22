/**
 * M1 三路判定引擎 —— drift 聚合层（纯函数，零 fs）。
 *
 * 签名冻结于 docs/m1-plan.md §1.4：
 *   - M1: remote 缺省 = base（store 即最后一次同步态）
 *   - mirror 模式：push 计数含 push-delete
 *   - merge 模式：push = changed + added + deleted（diffJson 口径）
 *
 * merge 分类中 `kind:'file'` 条目（JSON 损坏 / 本就不是 JSON）在此降级：
 * 交给 mirror 的 compareFile 判定，op 进 `ops`，计数按 mirror 口径（不参与 mergeJson，不炸）。
 *
 * changedKeys 格式（本模块对 §1.4「merge 模式 local vs base 的变更键」的落地约定）：
 *   - 键级变更：`<relPath>:<keyPath>`（如 `settings.json:theme`）
 *   - 空对象 base 兜底：整文件新增/删除按 leaf key 逐条记录，前缀同样是 relPath
 * 前缀是必需的：一个 merge 分类可含多个文件（如 pi 的 settings 含 settings.json + keybindings.json）。
 *
 * 唯一外部依赖：同目录的 merge / mirror 与冻结的 types.ts（纯类型）。
 */

import type { AdapterSnapshot, CategorySnapshot, SnapshotEntry, SnapshotFiles, SyncMode } from '../types.js';
import { diffJson, mergeJson, stripKeys, type MergeConflict } from './merge.js';
import { compareCategory, compareFile, type MirrorOp } from './mirror.js';

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
): AdapterDrift[] {
  const remoteSnapshots = remote ?? base;
  const out: AdapterDrift[] = [];

  for (const adapterId of unionAdapterIds([base, local, remoteSnapshots])) {
    const baseAdapter = findAdapter(base, adapterId);
    const localAdapter = findAdapter(local, adapterId);
    const remoteAdapter = findAdapter(remoteSnapshots, adapterId);

    const categories: CategoryDrift[] = [];
    for (const category of unionCategoryNames([baseAdapter, localAdapter, remoteAdapter])) {
      const b = findCategory(baseAdapter, category);
      const l = findCategory(localAdapter, category);
      const r = findCategory(remoteAdapter, category);
      const mode = l?.mode ?? b?.mode ?? r?.mode;
      if (mode === undefined) continue;
      categories.push(computeCategoryDrift(adapterId, category, mode, b, l, r));
    }
    out.push({ adapterId, categories });
  }

  return out;
}

/**
 * 附加工具（不改变 §1.4 冻结签名）：按 `category → excludeKeys` 剥离 merge 条目的顶层键，
 * 返回新快照（不可变，条目级拷贝）。落地 §1.2「drift 比较前对 local 与 base 各调一次」，
 * 让调用方（CLI/config 层）在进 computeDrift 之前完成剥离，引擎保持零配置依赖。
 */
export function stripExcludeKeys(
  snapshot: AdapterSnapshot,
  excludeKeysByCategory: Record<string, string[]>,
): AdapterSnapshot {
  return {
    adapterId: snapshot.adapterId,
    categories: snapshot.categories.map((category) => {
      const keys = excludeKeysByCategory[category.category];
      if (!keys || keys.length === 0) return cloneCategory(category);
      return { ...cloneCategory(category), files: stripFiles(category.files, keys) };
    }),
  };
}

/* ------------------------------------------------------------------ */
/* 聚合实现                                                            */
/* ------------------------------------------------------------------ */

const EMPTY_FILES: SnapshotFiles = new Map<string, SnapshotEntry>();

function computeCategoryDrift(
  adapterId: string,
  category: string,
  mode: SyncMode,
  b: CategorySnapshot | undefined,
  l: CategorySnapshot | undefined,
  r: CategorySnapshot | undefined,
): CategoryDrift {
  const drift: CategoryDrift = {
    adapterId, category, mode,
    push: 0, pull: 0, conflicts: 0,
    ops: [], mergeConflicts: [], changedKeys: [],
  };

  const baseFiles = b?.files ?? EMPTY_FILES;
  const localFiles = l?.files ?? EMPTY_FILES;
  const remoteFiles = r?.files ?? EMPTY_FILES;

  if (mode === 'mirror') {
    drift.ops = compareCategory(baseFiles, localFiles, remoteFiles);
    for (const op of drift.ops) countMirrorOp(drift, op);
    return drift;
  }

  for (const relPath of unionPaths(baseFiles, localFiles, remoteFiles)) {
    accumulateMergeFile(drift, relPath, baseFiles.get(relPath), localFiles.get(relPath), remoteFiles.get(relPath));
  }
  return drift;
}

function countMirrorOp(drift: CategoryDrift, op: MirrorOp): void {
  switch (op.type) {
    case 'push': case 'push-delete': drift.push += 1; break;
    case 'pull': case 'pull-delete': drift.pull += 1; break;
    case 'conflict': drift.conflicts += 1; break;
    case 'noop': break;
  }
}

function accumulateMergeFile(
  drift: CategoryDrift,
  relPath: string,
  b: SnapshotEntry | undefined,
  l: SnapshotEntry | undefined,
  r: SnapshotEntry | undefined,
): void {
  // 任一侧不是可解析 JSON → 降级按 mirror compareFile 处理（不炸、不参与 mergeJson）
  if (isDegraded(b) || isDegraded(l) || isDegraded(r)) {
    const op = compareFile(b, l, r, relPath);
    drift.ops.push(op);
    countMirrorOp(drift, op);
    return;
  }

  // 双删 = 收敛，无漂移
  if (l === undefined && r === undefined) return;

  const baseValue = b === undefined ? {} : parseEntry(b);
  const localValue = l === undefined ? {} : parseEntry(l);

  // push：local 相对 base 的键级变更（含整文件新增 added / 删除 deleted）
  const localDiff = diffJson(baseValue, localValue);
  drift.push += localDiff.changed + localDiff.added + localDiff.deleted;
  for (const key of localDiff.keys) drift.changedKeys.push(`${relPath}:${key}`);

  // local 删除，remote 未删 → 删除生效；remote 同时改 → 文件级 modify-vs-delete
  if (l === undefined) {
    if (r !== undefined && r.content !== b?.content) {
      drift.conflicts += 1;
      drift.mergeConflicts.push({ keyPath: relPath, reason: 'modify-vs-delete' });
    }
    return;
  }

  // local 未改，remote 删除 → 该文件的远端删除进 pull
  if (r === undefined) {
    if (localDiff.keys.length === 0) drift.pull += 1;
    else {
      drift.conflicts += 1;
      drift.mergeConflicts.push({ keyPath: relPath, reason: 'modify-vs-delete' });
    }
    return;
  }

  const remoteValue = parseEntry(r);

  // base 缺失（首次对接）：并集语义
  if (b === undefined) {
    if (l.content !== r.content) {
      drift.conflicts += 1;
      drift.mergeConflicts.push({ keyPath: relPath, reason: 'both-modified' });
    }
    return;
  }

  const result = mergeJson(baseValue, localValue, remoteValue);
  drift.conflicts += result.conflicts.length;
  drift.mergeConflicts.push(...result.conflicts);

  // pull：remote 相对 base 的变更中 local 未同时改动的键（会静默合并进来的部分）
  const localChanged = new Set(localDiff.keys);
  const remoteDiff = diffJson(baseValue, remoteValue);
  drift.pull += remoteDiff.keys.filter((key) => !localChanged.has(key)).length;
}

function isDegraded(entry: SnapshotEntry | undefined): boolean {
  if (entry === undefined) return false;
  return entry.kind === 'file' || parseEntry(entry) === PARSE_FAILED;
}

const PARSE_FAILED = Symbol('parse-failed');

function parseEntry(entry: SnapshotEntry): unknown | typeof PARSE_FAILED {
  try {
    return JSON.parse(entry.content) as unknown;
  } catch {
    return PARSE_FAILED;
  }
}

/* ------------------------------------------------------------------ */
/* 集合工具                                                            */
/* ------------------------------------------------------------------ */

function unionAdapterIds(groups: AdapterSnapshot[][]): string[] {
  const seen = new Set<string>();
  const ids: string[] = [];
  for (const group of groups) {
    for (const snapshot of group) {
      if (!seen.has(snapshot.adapterId)) { seen.add(snapshot.adapterId); ids.push(snapshot.adapterId); }
    }
  }
  return ids;
}

function unionCategoryNames(adapters: (AdapterSnapshot | undefined)[]): string[] {
  const seen = new Set<string>();
  const names: string[] = [];
  for (const adapter of adapters) {
    if (!adapter) continue;
    for (const category of adapter.categories) {
      if (!seen.has(category.category)) { seen.add(category.category); names.push(category.category); }
    }
  }
  return names;
}

function unionPaths(...fileSets: SnapshotFiles[]): string[] {
  const seen = new Set<string>();
  for (const files of fileSets) for (const path of files.keys()) seen.add(path);
  return [...seen].sort();
}

function findAdapter(snapshots: AdapterSnapshot[], adapterId: string): AdapterSnapshot | undefined {
  return snapshots.find((snapshot) => snapshot.adapterId === adapterId);
}

function findCategory(adapter: AdapterSnapshot | undefined, category: string): CategorySnapshot | undefined {
  return adapter?.categories.find((item) => item.category === category);
}

function cloneCategory(category: CategorySnapshot): CategorySnapshot {
  return { ...category, files: new Map(category.files) };
}

function stripFiles(files: SnapshotFiles, keys: string[]): SnapshotFiles {
  const out: SnapshotFiles = new Map();
  for (const [relPath, entry] of files) {
    if (entry.kind !== 'json') { out.set(relPath, entry); continue; }
    const value = parseEntry(entry);
    if (value === PARSE_FAILED) { out.set(relPath, entry); continue; }
    out.set(relPath, { kind: 'json', content: JSON.stringify(stripKeys(value, keys)) });
  }
  return out;
}
