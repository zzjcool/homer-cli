import fs from 'node:fs';
import path from 'node:path';

import type { AdapterSnapshot, CategorySnapshot, HomerConfig, SnapshotEntry, SnapshotFiles } from '../types.js';
import type { HomerPaths } from '../paths.js';

/**
 * store 布局（冻结，见 docs/m1-plan.md §1.6）：
 *   <storeDir>/<adapterId>/<category>/<relPath>
 * relPath：目录型 path（'skills/'）→ 目录内相对路径（`skills/foo/SKILL.md` → `foo/SKILL.md`）；
 *          单文件型 path（'settings.json'）→ 文件名本身。
 *
 * write 语义：整目录覆盖 —— 先清空 `<storeDir>/<adapterId>/` 再写，保证 store 是快照的精确镜像
 * （不做增量 diff，避免残留已删除文件）。
 */

/** 防止 adapterId / relPath 逃出 storeDir（例如 '..' 或绝对路径）。 */
function assertSafeSegment(segment: string, what: string): void {
  if (segment.length === 0) throw new Error(`${what} 不能为空`);
  if (path.isAbsolute(segment) || segment === '..' || segment.startsWith(`..${path.sep}`)) {
    throw new Error(`${what} 非法（不得为绝对路径或包含 '..'）: ${segment}`);
  }
  if (segment.split(/[\\/]/).includes('..')) {
    throw new Error(`${what} 非法（不得包含 '..' 段）: ${segment}`);
  }
}

/**
 * 把单个 adapter 的快照写入 store（覆盖该 adapter 目录）。
 * 空 files 的分类仍会创建空目录，使 roundtrip 结构稳定。
 */
export function writeSnapshotToStore(paths: HomerPaths, snapshot: AdapterSnapshot): void {
  assertSafeSegment(snapshot.adapterId, 'adapterId');

  const adapterDir = path.join(paths.storeDir, snapshot.adapterId);
  fs.rmSync(adapterDir, { recursive: true, force: true });

  for (const category of snapshot.categories) {
    assertSafeSegment(category.category, 'category');
    const categoryDir = path.join(adapterDir, category.category);
    fs.mkdirSync(categoryDir, { recursive: true });

    // 排序只为产出确定性（文件系统层面与内容无关）。
    const entries = [...category.files.entries()].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
    for (const [relPath, entry] of entries) {
      assertSafeSegment(relPath, 'relPath');
      const target = path.join(categoryDir, relPath);
      fs.mkdirSync(path.dirname(target), { recursive: true });
      fs.writeFileSync(target, entry.content, 'utf8');
    }
  }
}

/** 递归收集目录下所有文件的相对路径（posix 分隔符），目录不存在返回 null。 */
function collectRelativeFiles(dir: string): string[] | null {
  let stat: fs.Stats;
  try {
    stat = fs.statSync(dir);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return null;
    throw err;
  }
  if (!stat.isDirectory()) return null;

  const out: string[] = [];
  const walk = (current: string, prefix: string): void => {
    const dirents = fs.readdirSync(current, { withFileTypes: true }).sort((a, b) =>
      a.name < b.name ? -1 : a.name > b.name ? 1 : 0,
    );
    for (const dirent of dirents) {
      const rel = prefix === '' ? dirent.name : `${prefix}/${dirent.name}`;
      const abs = path.join(current, dirent.name);
      if (dirent.isDirectory()) {
        walk(abs, rel);
      } else if (dirent.isFile()) {
        out.push(rel);
      }
    }
  };
  walk(dir, '');
  return out;
}

/**
 * 读取 store 中所有（config 声明的、enabled 的）adapter 快照。
 *
 * 结构严格对齐 config：enabled 的 adapter / category 一律出现在结果里，
 * store 中缺目录 → files 为空 Map（= 该分类在 base 中没有内容，local 新增据此判为 push）。
 * 顺序：按 config 中 adapters / categories 的声明顺序；文件按 relPath 排序。
 * kind：category.mode === 'merge' 且内容可 JSON.parse → 'json'，否则 'file'。
 */
export function readSnapshotFromStore(paths: HomerPaths, config: HomerConfig): AdapterSnapshot[] {
  const adapters: AdapterSnapshot[] = [];

  for (const [adapterId, adapter] of Object.entries(config.adapters)) {
    if (adapter.enabled === false) continue;

    const categories: CategorySnapshot[] = [];
    for (const [categoryName, category] of Object.entries(adapter.categories)) {
      if (category.enabled === false) continue;

      const categoryDir = path.join(paths.storeDir, adapterId, categoryName);
      const relFiles = collectRelativeFiles(categoryDir) ?? [];

      const files: SnapshotFiles = new Map();
      for (const rel of relFiles) {
        const content = fs.readFileSync(path.join(categoryDir, rel), 'utf8');
        const kind: SnapshotEntry['kind'] = category.mode === 'merge' && isParsableJson(content) ? 'json' : 'file';
        files.set(rel, { kind, content });
      }

      categories.push({ adapterId, category: categoryName, mode: category.mode, files });
    }

    adapters.push({ adapterId, categories });
  }

  return adapters;
}

function isParsableJson(content: string): boolean {
  try {
    JSON.parse(content);
    return true;
  } catch {
    return false;
  }
}
