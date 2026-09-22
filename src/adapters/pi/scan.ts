import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import type {
  AdapterConfig,
  AdapterSnapshot,
  CategoryConfig,
  CategorySnapshot,
  SnapshotEntry,
  SnapshotFiles,
} from '../../core/types.js';
import { matchesIgnore } from './ignore.js';

export interface ScanError {
  path: string;
  message: string;
}

export interface ScanOutcome {
  snapshot: AdapterSnapshot; // 仅含 root 存在且 enabled 的分类
  errors: ScanError[]; // root 不存在 → snapshot 空 + error；读失败不炸
}

/** 展开开头的 '~'（'~' / '~/x' / '~\\x'），其余原样。 */
function expandHome(input: string): string {
  if (input === '~') return os.homedir();
  if (input.startsWith('~/') || input.startsWith('~\\')) {
    return path.join(os.homedir(), input.slice(2));
  }
  return input;
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

function tryParseJson(content: string): boolean {
  try {
    JSON.parse(content);
    return true;
  } catch {
    return false;
  }
}

const MAX_DEPTH = 32;

/**
 * 递归收集目录下所有文件的 relPath（相对 root），已应用 ignore。
 * 目录本身命中 ignore / exclude 则整棵剪枝。
 */
function walk(
  absBase: string,
  relBase: string,
  opts: {
    ignore: string[] | undefined;
    exclude: string[] | undefined;
    depth: number;
    errors: ScanError[];
    out: string[];
  },
): void {
  if (opts.depth > MAX_DEPTH) return;
  let entries: fs.Dirent[];
  try {
    entries = fs.readdirSync(absBase, { withFileTypes: true });
  } catch (err) {
    opts.errors.push({ path: absBase, message: (err as Error).message });
    return;
  }
  entries.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  for (const entry of entries) {
    const rel = relBase === '' ? entry.name : `${relBase}/${entry.name}`;
    if (isIgnored(rel, opts.ignore) || isExcluded(rel, opts.exclude)) continue;
    const abs = path.join(absBase, entry.name);
    let isDir = entry.isDirectory();
    let isFile = entry.isFile();
    if (entry.isSymbolicLink()) {
      try {
        const st = fs.statSync(abs);
        isDir = st.isDirectory();
        isFile = st.isFile();
      } catch {
        continue;
      }
    }
    if (isDir) {
      walk(abs, rel, { ...opts, depth: opts.depth + 1 });
    } else if (isFile) {
      opts.out.push(rel);
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
  const kind: SnapshotEntry['kind'] = mode === 'merge' && tryParseJson(content) ? 'json' : 'file';
  return { kind, content };
}

function scanCategory(
  root: string,
  category: string,
  cfg: CategoryConfig,
  ignore: string[] | undefined,
  errors: ScanError[],
): CategorySnapshot {
  const files: SnapshotFiles = new Map<string, SnapshotEntry>();

  for (const p of cfg.paths) {
    if (isDirPath(p)) {
      const relDir = p.replace(/\/+$/, '');
      if (relDir === '') continue;
      if (isIgnored(relDir, ignore) || isExcluded(relDir, cfg.exclude)) continue;
      const absDir = path.join(root, relDir);
      let st: fs.Stats;
      try {
        st = fs.statSync(absDir);
      } catch {
        // 目录不存在 = 该分类此处无文件，不视为错误
        continue;
      }
      if (!st.isDirectory()) continue;
      const collected: string[] = [];
      walk(absDir, '', { ignore, exclude: cfg.exclude, depth: 0, errors, out: collected });
      for (const rel of collected) {
        const entry = readEntry(path.join(absDir, rel), cfg.mode, errors);
        if (entry) files.set(normalizeRel(rel), entry);
      }
    } else {
      if (isIgnored(p, ignore) || isExcluded(p, cfg.exclude)) continue;
      const abs = path.join(root, p);
      let st: fs.Stats;
      try {
        st = fs.statSync(abs);
      } catch {
        continue;
      }
      if (!st.isFile()) continue;
      const entry = readEntry(abs, cfg.mode, errors);
      // §1.6：单文件 → relPath = 文件名本身
      if (entry) files.set(basenameOf(normalizeRel(p)), entry);
    }
  }

  // 稳定顺序：按 relPath 排序，方便测试与 diff
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

  for (const [category, cfg] of Object.entries(config.categories)) {
    if (cfg.enabled === false) continue;
    const cat = scanCategory(root, category, cfg, config.ignore, errors);
    cat.adapterId = adapterId;
    snapshot.categories.push(cat);
  }

  return { snapshot, errors };
}
