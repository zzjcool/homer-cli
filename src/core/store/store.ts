import fs from 'node:fs';
import path from 'node:path';

import type { AdapterSnapshot, CategorySnapshot, HomerConfig, SnapshotEntry, SnapshotFiles } from '../types.js';
import type { HomerPaths } from '../paths.js';
import { CliError } from '../errors.js';
import { entryKindFor } from '../entry-kind.js';

/**
 * store 布局（冻结，见 docs/m1-plan.md §1.6）：
 *   <storeDir>/<adapterId>/<category>/<relPath>
 * relPath：目录型 path（'skills/'）→ 目录内相对路径（`skills/foo/SKILL.md` → `foo/SKILL.md`）；
 *          单文件型 path（'settings.json'）→ 文件名本身。
 *
 * write 语义：整目录覆盖 —— 内容先写进 `<adapterDir>.tmp-<pid>`，完成后原子替换
 * `<storeDir>/<adapterId>/`（见 writeSnapshotToStore）。快照是 store 的精确镜像（不做增量 diff）。
 *
 * 完整性标记：每个 adapter 目录写完后落 `.homer-complete`。read 侧发现 adapter 目录存在
 * 但缺该标记 → 抛 CliError（store 半残），不再静默当「空 base」而误导出全量 push。
 */

/** adapter 目录写入完成的完整性标记文件名。 */
export const STORE_COMPLETE_MARKER = '.homer-complete';

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
 * 把单个 adapter 的快照写入 store（原子替换该 adapter 目录）。
 * 空 files 的分类仍会创建空目录，使 roundtrip 结构稳定。
 *
 * 顺序：tmp 目录写全（含 `.homer-complete`）→ rm 旧目录 → rename tmp → 正式目录。
 * 中途失败最坏留下 `<adapterDir>.tmp-<pid>` 残留，而正式目录保持上一版完整内容——
 * 不会出现「半残目录被 read 侧当成空 base」的假漂移。
 */
export function writeSnapshotToStore(paths: HomerPaths, snapshot: AdapterSnapshot): void {
  assertSafeSegment(snapshot.adapterId, 'adapterId');

  const adapterDir = path.join(paths.storeDir, snapshot.adapterId);
  const tmpDir = `${adapterDir}.tmp-${process.pid}`;

  fs.mkdirSync(paths.storeDir, { recursive: true });
  fs.rmSync(tmpDir, { recursive: true, force: true });
  fs.mkdirSync(tmpDir, { recursive: true });

  try {
    for (const category of snapshot.categories) {
      assertSafeSegment(category.category, 'category');
      const categoryDir = path.join(tmpDir, category.category);
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

    // 完整性标记：最后写，作为「本目录已写全」的哨兵。
    fs.writeFileSync(path.join(tmpDir, STORE_COMPLETE_MARKER), '', 'utf8');

    fs.rmSync(adapterDir, { recursive: true, force: true });
    fs.renameSync(tmpDir, adapterDir);
  } catch (err) {
    fs.rmSync(tmpDir, { recursive: true, force: true });
    throw err;
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

/** adapter 目录是否「存在但未标记完整」（半残）。 */
function isIncompleteAdapterDir(adapterDir: string): boolean {
  let stat: fs.Stats;
  try {
    stat = fs.statSync(adapterDir);
  } catch {
    return false; // 目录不存在 = 尚未 init 该 adapter，不算半残
  }
  if (!stat.isDirectory()) return false;
  return !fs.existsSync(path.join(adapterDir, STORE_COMPLETE_MARKER));
}

/**
 * 读取 store 中所有（config 声明的、enabled 的）adapter 快照。
 *
 * 结构严格对齐 config：enabled 的 adapter / category 一律出现在结果里，
 * store 中缺目录 → files 为空 Map（= 该分类在 base 中没有内容，local 新增据此判为 push）。
 * 顺序：按 config 中 adapters / categories 的声明顺序；文件按 relPath 排序。
 * kind：`entryKindFor(category.mode, content)`（与 scan 侧共用同一判定）。
 *
 * adapter 目录存在但缺 `.homer-complete` → 抛 CliError（写入被中断，store 不可信）。
 */
export function readSnapshotFromStore(paths: HomerPaths, config: HomerConfig): AdapterSnapshot[] {
  const adapters: AdapterSnapshot[] = [];

  for (const [adapterId, adapter] of Object.entries(config.adapters)) {
    if (adapter.enabled === false) continue;

    const adapterDir = path.join(paths.storeDir, adapterId);
    if (isIncompleteAdapterDir(adapterDir)) {
      throw new CliError(
        `store 不完整: ${adapterDir} 缺少 ${STORE_COMPLETE_MARKER} 标记（上次写入被中断？）`,
        '请重新运行 `homer init --force` 重建快照。',
      );
    }

    const categories: CategorySnapshot[] = [];
    for (const [categoryName, category] of Object.entries(adapter.categories)) {
      if (category.enabled === false) continue;

      const categoryDir = path.join(adapterDir, categoryName);
      const relFiles = collectRelativeFiles(categoryDir) ?? [];

      const files: SnapshotFiles = new Map();
      for (const rel of relFiles) {
        if (rel === STORE_COMPLETE_MARKER) continue;
        const content = fs.readFileSync(path.join(categoryDir, rel), 'utf8');
        files.set(rel, { kind: entryKindFor(category.mode, content), content });
      }

      categories.push({ adapterId, category: categoryName, mode: category.mode, files });
    }

    adapters.push({ adapterId, categories });
  }

  return adapters;
}
