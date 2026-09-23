/**
 * category 的 `relPath` ↔ 工具目录**绝对路径**反向映射（docs/m2-plan.md §2.6，P2-W6）。
 *
 * 为什么需要它：`planPull` 产出的动作只有 `<adapterId>/<category>/<relPath>` 三元组
 * （relPath = store 侧相对 category 目录的路径，见 m1-plan §1.6），而 apply 必须把它落到
 * 用户工具目录的真实文件上。映射一旦写错就是「污染用户真实目录」——风险清单第 1 条，
 * 故本模块的规则与 `src/adapters/pi/scan.ts` 的收集规则**逐条互为逆映射**：
 *
 * | category.paths 形态 | scan 收集的 relPath            | 本模块的反向结果                     |
 * |---------------------|--------------------------------|--------------------------------------|
 * | 目录型 `'skills/'`  | 该目录内的相对路径（posix）    | `<root>/skills/<relPath>`            |
 * | 单文件型 `'settings.json'` | 文件名本身（basename）   | `<root>/settings.json`（即该 path）  |
 *
 * **多个候选时取「声明顺序里最后一个匹配者」**：scan 把同一 category 的所有 path 收进**同一个**
 * `Map<relPath, entry>`，同名 key 后写覆盖先写；因此只有「最后一个匹配的 path」才与 scan 的
 * 产出自洽（这也是本模块对「一个 category 声明多个目录型 path」这一歧义情形的裁定）。
 *
 * 编程错误（`'..'` 段 / 绝对路径 / 找不到对应 path）一律 **throw**：这些输入只可能来自
 * 代码 bug 或与 config 失配的旧 plan，静默落盘比报错危险得多。
 */

import path from 'node:path';

import type { CategoryConfig } from '../core/types.js';

/** 目录型 path：以 `'/'` 结尾（与 scan.ts 的 `isDirPath` 同义）。 */
function isDirPath(p: string): boolean {
  return p.endsWith('/');
}

/** 去掉结尾的全部 `'/'`（scan.ts 用 `replace(/\/+$/, '')`）。 */
function stripTrailingSlashes(p: string): string {
  return p.replace(/\/+$/, '');
}

/** posix basename（无 `'/'` → 自身）。relPath / 声明的 path 都是 posix 风格。 */
function basenameOf(p: string): string {
  const idx = p.lastIndexOf('/');
  return idx === -1 ? p : p.slice(idx + 1);
}

/**
 * relPath 的安全性与「是文件而非目录」校验。
 * - 空串：既不是目录内相对路径也不是 basename → 编程错误；
 * - 绝对路径（posix `/…`、Windows `C:\…`）：逃逸 root → throw；
 * - 含 `'..'` 段：可逃逸 root → throw（与 store.ts / backup.ts 的 assertSafeSegment 同因）；
 * - 以 `'/'` 结尾：那是目录路径，不是文件 relPath；放行只会在写盘时炸在 apply 半途。
 */
function assertFileRelPath(relPath: string): void {
  if (relPath.length === 0) {
    throw new Error('resolveCategoryFilePath: relPath 不能为空');
  }
  if (path.isAbsolute(relPath) || relPath.startsWith('/') || /^[A-Za-z]:[\\/]/.test(relPath)) {
    throw new Error(`resolveCategoryFilePath: relPath 非法（不得为绝对路径）: ${relPath}`);
  }
  if (relPath.split(/[\\/]/).includes('..')) {
    throw new Error(`resolveCategoryFilePath: relPath 非法（不得包含 '..' 段）: ${relPath}`);
  }
  if (relPath.endsWith('/')) {
    throw new Error(`resolveCategoryFilePath: relPath 非法（不得以 '/' 结尾，那不是文件路径）: ${relPath}`);
  }
}

interface CategoryPathMatch {
  /** 命中的声明的 path（原始文本）。 */
  declared: string;
  /** true = 目录型（relPath 拼在其下），false = 单文件型（path 本身即目标）。 */
  dir: boolean;
}

/**
 * 找出能产出该 relPath 的声明 path（声明顺序里**最后一个**匹配者，对齐 scan 的 Map 覆盖语义）。
 * 返回 undefined = 没有任何 path 能产出它（调用方 throw）。
 */
function matchCategoryPath(categoryCfg: CategoryConfig, relPath: string): CategoryPathMatch | undefined {
  let matched: CategoryPathMatch | undefined;
  for (const declared of categoryCfg.paths) {
    if (isDirPath(declared)) {
      if (stripTrailingSlashes(declared) === '') continue; // scan 同样跳过空目录 path
      matched = { declared, dir: true }; // 目录型：任意 relPath 都可能来自该目录
      continue;
    }
    if (basenameOf(declared) === relPath) matched = { declared, dir: false };
  }
  return matched;
}

/**
 * `relPath` → 工具目录内的绝对文件路径。
 *
 * @param root        adapter root 的**绝对**路径（`'~'` 由调用方先展开，见 `expandHome`）。
 * @param categoryCfg 该 category 的 config（`paths` / `mode`）。
 * @param relPath     store 侧相对路径（`planPull` 动作里的 `relPath`）。
 *
 * @throws relPath 为空 / 绝对路径 / 含 `'..'` 段 / 以 `'/'` 结尾，或该 category 的 `paths`
 *         里没有任何一项能产出它（自带名与任一单文件 path 的 basename 不等，且无目录型 path）。
 */
export function resolveCategoryFilePath(
  root: string,
  categoryCfg: CategoryConfig,
  relPath: string,
): string {
  assertFileRelPath(relPath);

  const matched = matchCategoryPath(categoryCfg, relPath);
  if (matched === undefined) {
    throw new Error(
      `resolveCategoryFilePath: relPath "${relPath}" 无法映射到 category.paths ` +
        `${JSON.stringify(categoryCfg.paths)} 中的任何一项（无目录型 path，且没有 basename 相等的单文件 path）`,
    );
  }

  if (!matched.dir) return path.join(root, matched.declared);
  return path.join(root, stripTrailingSlashes(matched.declared), relPath);
}
