/**
 * M1 三路判定引擎 —— mirror 半边（纯函数，零 fs）。
 *
 * 语义冻结于 docs/m1-plan.md §1.3（对应 DESIGN §2.7 文件级三路矩阵）：
 * 以「local 相对 base」×「remote 相对 base」的 3×3 全枚举判定：
 *
 *              remote 未变      remote 改了                remote 删了
 *  local 未变  noop            pull                       pull-delete
 *  local 改了  push            conflict modify-vs-modify  conflict local-modify-vs-remote-delete
 *  local 删了  push-delete     conflict local-delete-...  noop（双删）
 *
 * 内容相等 = 字符串全等（plan §1.3）；不存在 = 删除/新增。
 * base 存在时，其余 7 种情形严格照 DESIGN §2.7 表：双方都相对 base 变化（含「改了/改了」
 * 与「删了/改了」）一律 conflict，不因最终内容相同而静默收敛。
 * base 不存在（无基线，如首次对接）时按 DESIGN §2.7 降级：仅有 local → push，仅有 remote → pull，
 * 两边都有且内容不同 → conflict modify-vs-modify（	“同路径不同内容进冲突列表	”），同内容 → noop。
 *
 * 唯一外部依赖是冻结的 src/core/types.ts（纯类型，零 fs）。
 */

import type { SnapshotEntry, SnapshotFiles } from '../types.js';

export type MirrorOp =
  | { type: 'noop'; path: string }
  | { type: 'push'; path: string }         // local 改/增，remote 未变
  | { type: 'push-delete'; path: string }  // local 删，remote 未变（DESIGN 矩阵补全）
  | { type: 'pull'; path: string }         // remote 改/增，local 未变
  | { type: 'pull-delete'; path: string }  // remote 删，local 未变
  | { type: 'conflict'; path: string;
      reason: 'modify-vs-modify' | 'local-delete-vs-remote-modify' | 'local-modify-vs-remote-delete' };

export function compareFile(
  base: SnapshotEntry | undefined,
  local: SnapshotEntry | undefined,
  remote: SnapshotEntry | undefined,
  relPath: string,
): MirrorOp {
  const noop: MirrorOp = { type: 'noop', path: relPath };

  // 三方都不存在（并集遍历不应出现，防御性）
  if (base === undefined && local === undefined && remote === undefined) return noop;

  const baseAbsent = base === undefined;
  const localAbsent = local === undefined;
  const remoteAbsent = remote === undefined;

  const localEqualsBase = !localAbsent && !baseAbsent && local.content === base.content;
  const remoteEqualsBase = !remoteAbsent && !baseAbsent && remote.content === base.content;

  // 双删 → 无事（3×3 补全项）
  if (localAbsent && remoteAbsent) return noop;

  // 无 base：并集语义（新增 = push/pull；同路径不同内容 = 冲突）
  if (baseAbsent) {
    if (localAbsent) return { type: 'pull', path: relPath };
    if (remoteAbsent) return { type: 'push', path: relPath };
    return local.content === remote.content
      ? noop
      : { type: 'conflict', path: relPath, reason: 'modify-vs-modify' };
  }

  // 单边删除
  if (localAbsent) {
    if (remoteEqualsBase) return { type: 'push-delete', path: relPath };
    return { type: 'conflict', path: relPath, reason: 'local-delete-vs-remote-modify' };
  }
  if (remoteAbsent) {
    if (localEqualsBase) return { type: 'pull-delete', path: relPath };
    return { type: 'conflict', path: relPath, reason: 'local-modify-vs-remote-delete' };
  }

  // 三方都在
  if (localEqualsBase && remoteEqualsBase) return noop;
  if (localEqualsBase) return { type: 'pull', path: relPath };   // 仅 remote 改了
  if (remoteEqualsBase) return { type: 'push', path: relPath };  // 仅 local 改了

  // 双方都改了：照 DESIGN §2.7 表 = 冲突（内容恰好相同也不静默收敛）
  return { type: 'conflict', path: relPath, reason: 'modify-vs-modify' };
}

/** 三方路径并集逐文件 compareFile（双删 = noop），按 path 排序保证确定性 */
export function compareCategory(
  base: SnapshotFiles,
  local: SnapshotFiles,
  remote: SnapshotFiles,
): MirrorOp[] {
  const paths = new Set<string>([...base.keys(), ...local.keys(), ...remote.keys()]);
  return [...paths].sort().map((path) =>
    compareFile(base.get(path), local.get(path), remote.get(path), path),
  );
}
