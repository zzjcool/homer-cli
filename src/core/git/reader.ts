/**
 * git reader（M1 计划预留的 "git reader"，docs/m2-plan.md §2.4 D3/D4）。
 *
 * 用途：把 `store/` 在**任意 commit** 上的内容读成 `AdapterSnapshot[]`——
 * - `commitish = state.lastSyncCommit` → base（上次同步态）
 * - `commitish = @{upstream}` → remote（D4）
 *
 * 关键约束：布局与 kind 判定必须与 `readSnapshotFromStore`（工作区侧）**完全同构**，
 * 否则同一份 store 在「工作区」与「commit」两条路径上会产出不同快照，三方 diff 假漂移。
 * 因此本文件的规则逐条对照 store.ts：
 *   - 布局 `<storeDir>/<adapterId>/<category>/<relPath>`，其中 **category = config 里的分类名**；
 *   - `relPath`：目录型 path（以 '/' 结尾）→ 目录内相对路径；单文件型 path → 文件名本身；
 *   - kind = `entryKindFor(category.mode, content)`；
 *   - adapter 目录根下的 `.homer-complete` 是写入哨兵，跳过（不是内容）；
 *   - 缺目录 → 空 Map（M1 裁定语义）。
 * 实现走 `git ls-tree -r --name-only -z <commitish> -- store/` + `git show <commitish>:<path>`，
 * 不读工作区（commit 可能是历史版本，工作区内容可能与之一致也可能不同）。
 *
 * 与 readSnapshotFromStore 的唯一差别：**不做**「半残目录 → CliError」校验——
 * git commit 是原子快照，一个 commit 内的 store 不存在「写了目录没写标记」的中间态；
 * 对历史 commit 抛错只会让 base 读取在生产路径上不可用。
 */

import type { AdapterSnapshot, CategorySnapshot, HomerConfig, SnapshotFiles } from '../types.js';
import type { HomerPaths } from '../paths.js';
import { STORE_COMPLETE_MARKER } from '../store/store.js';
import { entryKindFor } from '../entry-kind.js';
import { gitExec } from './git.js';

/** commit 内 store 的路径前缀（相对仓库根，posix）。 */
const STORE_PREFIX = 'store/';

/**
 * `git ls-tree -r --name-only -z <commitish> -- store/`。
 * commit 不存在 / 非仓库 / git 缺失 → 空数组（与「该 commit 内 store 为空」同构，
 * 由调用方按「缺目录 → 空 Map」语义消化）。
 */
function listStoreFiles(paths: HomerPaths, commitish: string): string[] {
  const result = gitExec(paths.home, [
    'ls-tree',
    '-r',
    '--name-only',
    '-z',
    commitish,
    '--',
    STORE_PREFIX,
  ]);
  if (!result.ok) return [];
  return result.stdout.split('\0').filter((line) => line !== '');
}

/** 读 blob 内容；读不到 → undefined（对象损坏等，跳过该条目）。 */
function readBlob(paths: HomerPaths, commitish: string, storeRel: string): string | undefined {
  const result = gitExec(paths.home, ['show', `${commitish}:${storeRel}`]);
  return result.ok ? result.stdout : undefined;
}

/**
 * 读取 `commitish`（SHA / `HEAD` / `@{upstream}` 等）上 store 中所有
 * （config 声明的、enabled 的）adapter 快照。
 *
 * 结构严格对齐 config：enabled 的 adapter / category 一律出现在结果里，
 * commit 内缺该目录 → `files` 为空 Map（M1 裁定语义：该分类在该版本没有内容）。
 * `.homer-complete` 被跳过。顺序：config 中 adapters / categories 的声明顺序；
 * 文件按 relPath 排序（与 readSnapshotFromStore 一致）。
 */
export function readStoreSnapshotAtCommit(
  paths: HomerPaths,
  config: HomerConfig,
  commitish: string,
): AdapterSnapshot[] {
  const allStoreFiles = listStoreFiles(paths, commitish);
  const adapters: AdapterSnapshot[] = [];

  for (const [adapterId, adapter] of Object.entries(config.adapters)) {
    if (adapter.enabled === false) continue;

    const categories: CategorySnapshot[] = [];
    for (const [categoryName, category] of Object.entries(adapter.categories)) {
      if (category.enabled === false) continue;

      // store 布局：<store>/<adapterId>/<categoryName>/<relPath>
      const categoryPrefix = `${STORE_PREFIX}${adapterId}/${categoryName}/`;

      const matched = new Map<string, string>(); // categoryRel → storeRel
      for (const storeRel of allStoreFiles) {
        if (!storeRel.startsWith(categoryPrefix)) continue;
        const rel = storeRel.slice(categoryPrefix.length);
        if (rel === '' || rel === STORE_COMPLETE_MARKER) continue;
        matched.set(rel, storeRel);
      }

      const files: SnapshotFiles = new Map();
      const sorted = [...matched.entries()].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
      for (const [rel, storeRel] of sorted) {
        const content = readBlob(paths, commitish, storeRel);
        if (content === undefined) continue;
        files.set(rel, { kind: entryKindFor(category.mode, content), content });
      }

      categories.push({ adapterId, category: categoryName, mode: category.mode, files });
    }

    adapters.push({ adapterId, categories });
  }

  return adapters;
}
