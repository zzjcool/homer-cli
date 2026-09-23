import fs from 'node:fs';
import path from 'node:path';

import type {
  AdapterConfig,
  AdapterSnapshot,
  CategoryConfig,
  CategorySnapshot,
  SnapshotEntry,
  SnapshotFiles,
} from '../../core/types.js';
import { entryKindFor } from '../../core/entry-kind.js';
import { expandHome } from '../../core/paths.js';
import { matchesIgnore } from './ignore.js';

export interface ScanError {
  path: string;
  message: string;
}

export interface ScanOutcome {
  snapshot: AdapterSnapshot; // 仅含 root 存在且 enabled 的分类
  errors: ScanError[]; // root 不存在 → snapshot 空 + error；读失败不炸
}

/** 目录型 path（以 '/' 结尾）→ true。 */
function isDirPath(p: string): boolean {
  return p.endsWith('/');
}

function normalizeRel(rel: string): string {
  return rel.split(path.sep).join('/');
}

function basenameOf(rel: string): string {
  const idx = rel.lastIndexOf('/');
  return idx === -1 ? rel : rel.slice(idx + 1);
}

/**
 * category 内 exclude 判定：文件名 glob。
 * 语义 = matchesIgnore 应用于 relPath、其 basename、以及每一段路径名。
 * （`*cache*` 因此既排除 `foo-cache.js`，也排除 `cache/`、`mycache-tool/index.ts` 这类目录整棵。）
 */
function isExcluded(relPath: string, exclude: string[] | undefined): boolean {
  if (!exclude || exclude.length === 0) return false;
  if (matchesIgnore(relPath, exclude)) return true;
  if (matchesIgnore(basenameOf(relPath), exclude)) return true;
  const segments = relPath.split('/').filter((s) => s.length > 0);
  return segments.some((segment) => matchesIgnore(segment, exclude));
}

/**
 * adapter 级 ignore：§1.5 冻结为“相对 root 的路径 glob”。
 * 严格按 relPath 匹配（不做 basename / 分段兜底），避免 glob 语义膨胀；
 * 目录前缀模式（`sessions/`）天然覆盖整棵子树，因为 walk 逐层检查每个 relPath。
 */
function isIgnored(relPath: string, ignore: string[] | undefined): boolean {
  if (!ignore || ignore.length === 0) return false;
  return matchesIgnore(relPath, ignore);
}

const MAX_DEPTH = 32;

interface WalkState {
  ignore: string[] | undefined;
  exclude: string[] | undefined;
  /** symlink 逃逸 allowlist（相对 adapter root 的 glob，§2.0-1 / D7）。 */
  allowEscape: string[] | undefined;
  errors: ScanError[];
  out: string[];
  /** adapter root 的真实路径（symlink containment 基准）。 */
  rootReal: string;
  /**
   * allowlist 匹配基准：本次 walk 起点相对 **adapter root** 的路径（= 声明的 category path）。
   * 快照 key（`rel`）是相对 **category 目录** 的，而 allowEscape 是相对 root 的 glob，
   * 故匹配时必须用 `joinRel(rootRelBase, rel)` 还原成 root 相对路径。
   */
  rootRelBase: string;
  /** 本次 walk 已展开过的目录真实路径（symlink 回环检测）。 */
  visited: Set<string>;
}

/** 拼接 root 相对路径（rootRelBase 与 category 内 rel；两段都可能为空）。 */
function joinRel(rootRelBase: string, rel: string): string {
  if (rootRelBase === '') return rel;
  if (rel === '') return rootRelBase;
  return `${rootRelBase}/${rel}`;
}

/**
 * 逃逸是否被 allowlist 放行（D7）：对 **root 相对路径** 跑 `matchesIgnore`，语义与
 * adapter 级 ignore 完全一致（字面量 + `*` 不跨 `/`，尾 `/` 为目录前缀）；
 * 缺省 / 空数组 = 维持 M1 安全边界。
 */
function isEscapeAllowed(rootRel: string, allowEscape: string[] | undefined): boolean {
  if (!allowEscape || allowEscape.length === 0) return false;
  return matchesIgnore(rootRel, allowEscape);
}

/** realpath 封装：失败（悬空 / 竞争删除）→ undefined。 */
function tryRealpath(target: string): string | undefined {
  try {
    return fs.realpathSync(target);
  } catch {
    return undefined;
  }
}

/** child 是否位于 root 之下（两者都必须是 realpath）。 */
function isWithinRoot(child: string, root: string): boolean {
  if (child === root) return true;
  const prefix = root.endsWith(path.sep) ? root : `${root}${path.sep}`;
  return child.startsWith(prefix);
}

/**
 * symlink 条目处理（安全边界）：
 *   1. realpath 失败（悬空链接）→ 静默跳过（等价于「该路径不存在」）；
 *   2. realpath 落在 adapter root 之外 → skip + 记 ScanError（防「链接把 root 外内容带进快照」），
 *      **除非** root 相对路径命中 `allowEscape`（D7，显式 opt-in）→ 视同 root 内链接继续走 3/4；
 *   3. realpath 指向目录 → 交给 walk（其入口用 visited 集合截断回环）；
 *   4. realpath 指向文件 → 正常收集。
 *
 * 注意 allowlist 只放行**这条链接本身**：逃逸目录内部的后续 symlink 仍按同规则重新判定
 * （要连带放行整棵子树，用尾 `/` 前缀模式，如 `skills/agent-browser/`）。
 */
function visitSymlink(abs: string, rel: string, depth: number, state: WalkState): void {
  const real = tryRealpath(abs);
  if (real === undefined) return;

  if (!isWithinRoot(real, state.rootReal) && !isEscapeAllowed(joinRel(state.rootRelBase, rel), state.allowEscape)) {
    state.errors.push({
      path: abs,
      message: `symlink 逃逸 adapter root: ${real}`,
    });
    return;
  }

  let st: fs.Stats;
  try {
    st = fs.statSync(real);
  } catch {
    return;
  }
  if (st.isDirectory()) {
    walk(abs, rel, depth + 1, state);
  } else if (st.isFile()) {
    state.out.push(rel);
  }
}

/**
 * 递归收集目录下所有文件的 relPath（相对扫描起点），已应用 ignore。
 * 目录本身命中 ignore / exclude 则整棵剪枝。
 *
 * 顺序：readdir 原始顺序（不再在内层排序——scanCategory 末尾的统一排序才是稳定顺序的唯一来源，
 * 两处排序纯属冗余）。symlink 见 visitSymlink：目录真实路径记入 visited，回环只展开一次。
 */
function walk(absBase: string, relBase: string, depth: number, state: WalkState): void {
  if (depth > MAX_DEPTH) return;

  const realBase = tryRealpath(absBase);
  if (realBase === undefined) return;
  if (state.visited.has(realBase)) return; // symlink 目录回环：不再重复展开
  state.visited.add(realBase);

  let entries: fs.Dirent[];
  try {
    entries = fs.readdirSync(absBase, { withFileTypes: true });
  } catch (err) {
    state.errors.push({ path: absBase, message: (err as Error).message });
    return;
  }
  for (const entry of entries) {
    const rel = relBase === '' ? entry.name : `${relBase}/${entry.name}`;
    if (isIgnored(rel, state.ignore) || isExcluded(rel, state.exclude)) continue;
    const abs = path.join(absBase, entry.name);

    if (entry.isSymbolicLink()) {
      visitSymlink(abs, rel, depth, state);
      continue;
    }
    if (entry.isDirectory()) {
      walk(abs, rel, depth + 1, state);
    } else if (entry.isFile()) {
      state.out.push(rel);
    }
  }
}

function readEntry(abs: string, mode: CategoryConfig['mode'], errors: ScanError[]): SnapshotEntry | undefined {
  let content: string;
  try {
    content = fs.readFileSync(abs, 'utf8');
  } catch (err) {
    errors.push({ path: abs, message: (err as Error).message });
    return undefined;
  }
  return { kind: entryKindFor(mode, content), content };
}

/**
 * 解析用户在 homer.json 里声明的 path（单文件 / 目录）——同样适用 root containment。
 *
 * 计划原文：「对每个 symlink 先 fs.realpathSync 解析真实路径——realpath 不在 root realpath 之下
 * → skip + 记 ScanError（防逃逸）」。因此**包括**声明的 category path 自身为 symlink 的情况。
 * 返回 undefined = 该 path 不存在（缺文件/缺目录，不视为错误）。
 *
 * D7（§2.0-1）：与 `visitSymlink` 共用同一逃逸判定 —— 声明的 path 逃逸时同样先查
 * `allowEscape`（匹配基准 = 声明的 path 去掉尾 `/` 后的 root 相对路径），命中才跟随。
 */
function resolveConfiguredPath(
  abs: string,
  rootReal: string,
  errors: ScanError[],
  rootRel: string,
  allowEscape: string[] | undefined,
): { real: string; stat: fs.Stats } | undefined {
  let lst: fs.Stats;
  try {
    lst = fs.lstatSync(abs);
  } catch {
    return undefined; // 不存在 = 此处无文件，不报错
  }

  if (!lst.isSymbolicLink()) {
    return { real: abs, stat: lst };
  }

  const real = tryRealpath(abs);
  if (real === undefined) return undefined; // 悬空链接 → 静默跳过（allowlist 命中也不例外）
  if (!isWithinRoot(real, rootReal) && !isEscapeAllowed(rootRel, allowEscape)) {
    errors.push({ path: abs, message: `symlink 逃逸 adapter root: ${real}` });
    return undefined;
  }
  try {
    return { real, stat: fs.statSync(real) };
  } catch {
    return undefined;
  }
}

function scanCategory(
  root: string,
  rootReal: string,
  category: string,
  cfg: CategoryConfig,
  ignore: string[] | undefined,
  allowEscape: string[] | undefined,
  errors: ScanError[],
): CategorySnapshot {
  const files: SnapshotFiles = new Map<string, SnapshotEntry>();

  for (const p of cfg.paths) {
    if (isDirPath(p)) {
      const relDir = p.replace(/\/+$/, '');
      if (relDir === '') continue;
      if (isIgnored(relDir, ignore) || isExcluded(relDir, cfg.exclude)) continue;
      const absDir = path.join(root, relDir);
      const resolved = resolveConfiguredPath(absDir, rootReal, errors, relDir, allowEscape);
      // 目录不存在 / 不是目录 / symlink 逃逸 → 该分类此处无文件
      if (resolved === undefined || !resolved.stat.isDirectory()) continue;

      // 合法（root 内）的 symlink 目录 → 用真实路径作扫描起点；walk 内部的 visited 集合
      // 仍会截断回环，readEntry 也用 realpath 读（scanBase 已 realpath）。
      const scanBase = resolved.real;
      const collected: string[] = [];
      walk(scanBase, '', 0, {
        ignore,
        exclude: cfg.exclude,
        allowEscape,
        errors,
        out: collected,
        rootReal,
        // allowlist 匹配基准用**声明的 path**（而非 realpath），与用户写的 glob 对齐
        rootRelBase: relDir,
        visited: new Set<string>(),
      });
      for (const rel of collected) {
        const entry = readEntry(path.join(scanBase, rel), cfg.mode, errors);
        if (entry) files.set(normalizeRel(rel), entry);
      }
    } else {
      if (isIgnored(p, ignore) || isExcluded(p, cfg.exclude)) continue;
      const abs = path.join(root, p);
      const resolved = resolveConfiguredPath(abs, rootReal, errors, p, allowEscape);
      if (resolved === undefined || !resolved.stat.isFile()) continue;
      const entry = readEntry(resolved.real, cfg.mode, errors);
      // §1.6：单文件 → relPath = 文件名本身
      if (entry) files.set(basenameOf(normalizeRel(p)), entry);
    }
  }

  // 稳定顺序：按 relPath 排序，方便测试与 diff（walk 内的中间排序已删除，这里是唯一来源）
  const sorted: SnapshotFiles = new Map<string, SnapshotEntry>();
  for (const key of [...files.keys()].sort()) {
    const value = files.get(key);
    if (value) sorted.set(key, value);
  }

  return { adapterId: '', category, mode: cfg.mode, files: sorted };
}

/**
 * 只读扫描一个 adapter root，产出 AdapterSnapshot。
 * root 不存在 → 空 categories + errors，不 throw。
 */
export function scanAdapter(adapterId: string, config: AdapterConfig): ScanOutcome {
  const errors: ScanError[] = [];
  const root = expandHome(config.root);

  const snapshot: AdapterSnapshot = { adapterId, categories: [] };

  if (config.enabled === false) {
    return { snapshot, errors };
  }

  let rootStat: fs.Stats;
  try {
    rootStat = fs.statSync(root);
  } catch (err) {
    errors.push({ path: root, message: (err as Error).message });
    return { snapshot, errors };
  }
  if (!rootStat.isDirectory()) {
    errors.push({ path: root, message: 'not a directory' });
    return { snapshot, errors };
  }

  const rootReal = tryRealpath(root) ?? root;

  for (const [category, cfg] of Object.entries(config.categories)) {
    if (cfg.enabled === false) continue;
    const cat = scanCategory(root, rootReal, category, cfg, config.ignore, config.allowEscape, errors);
    cat.adapterId = adapterId;
    snapshot.categories.push(cat);
  }

  return { snapshot, errors };
}
