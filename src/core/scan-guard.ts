/**
 * adapter 扫描结果 → 采集期告警的**唯一**判定点（对抗式 review 修复 minor 1）。
 *
 * 背景：`snapshot.categories.length === 0 ∧ errors.length > 0` 这一「整个 root 不可读」的判定公式
 * 曾在三处各自复制（`cli/render.ts` 的 `isRootUnreadable`、`core/sync/base.ts` 的 `collectLocal`、
 * `cli/commands/merge.ts` 的 `rescanLocal`）。公式一旦在某一处漂移，就会出现
 * 「一处把空分类当假删除清空、另一处当成正常扫描」的不一致 —— 判定与措辞都必须收敛到一处。
 *
 * 放在 core 而非 cli：`core/sync/base.ts` 也要用它，而 **core 不得反向依赖 cli**
 * （与 `core/errors.ts` 拆出 CliError 的理由相同）。本模块只依赖 `types.js`（纯类型），
 * 运行时零 import、零 fs、零副作用。
 */

import type { AdapterSnapshot } from './types.js';

/** 结构化的单条扫描问题（`adapters/pi/scan.ts` 的 `ScanError` 同形）。 */
export interface ScanProblem {
  path: string;
  message: string;
}

/** `scanAdapter` 返回形状的结构化子集（避免 core → adapters 的类型依赖）。 */
export interface ScanOutcomeLike {
  snapshot: AdapterSnapshot;
  errors: readonly ScanProblem[];
}

/**
 * root 级失败判定：扫描结果**空分类 + 有错** → 整个 root 不可读（不存在 / 不是目录）。
 *
 * 用途：避免把「扫不到」静默当成「本地全空」（base 里每个文件都会被算成假删除 / 假 pull-delete）。
 * 判定公式只此一份；三处调用方共用。
 */
export function isRootUnreadable(outcome: ScanOutcomeLike): boolean {
  return outcome.snapshot.categories.length === 0 && outcome.errors.length > 0;
}

/** 采集告警的前缀：根级失败与普通扫描告警用不同措辞（M-A 要求）。 */
export function scanErrorPrefix(rootUnreadable: boolean): string {
  return rootUnreadable ? 'adapter root 不可读' : '扫描告警';
}

/**
 * 单条采集问题的规范文本：`<prefix>: <adapterId> (<path>: <message>)`。
 * `homer status` 的 `sourceErrorMessages` / `collectSyncSources` / `merge` 的重扫共用同一格式。
 */
export function scanProblemMessage(
  adapterId: string,
  error: ScanProblem,
  rootUnreadable: boolean,
): string {
  return `${scanErrorPrefix(rootUnreadable)}: ${adapterId} (${error.path}: ${error.message})`;
}

/** 一次扫描的全部采集问题文本（逐条，顺序与 `outcome.errors` 一致）。 */
export function scanWarningMessages(adapterId: string, outcome: ScanOutcomeLike): string[] {
  const rootUnreadable = isRootUnreadable(outcome);
  return outcome.errors.map((error) => scanProblemMessage(adapterId, error, rootUnreadable));
}
