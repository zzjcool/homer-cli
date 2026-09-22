/**
 * mirror.ts 测试矩阵 —— 3×3 = 9 情形全枚举 + compareCategory 并集语义。
 * 内容相等 = 字符串全等（plan §1.3）。
 */

import { describe, expect, it } from 'vitest';
import {
  compareCategory,
  compareFile,
} from '../../src/core/engine/index.js';
import type { SnapshotEntry, SnapshotFiles } from '../../src/core/types.js';

const BASE_CONTENT = 'v1';
const NEW_CONTENT = 'v2';

const json = (content: string): SnapshotEntry => ({ kind: 'json', content });
const base = json(BASE_CONTENT);
const modified = json(NEW_CONTENT);

type Cell = 'same' | 'changed' | 'deleted';

function entryFor(cell: Cell): SnapshotEntry | undefined {
  return cell === 'changed' ? modified : cell === 'same' ? base : undefined;
}

/** 3×3 全枚举的期望结果（行 = local vs base，列 = remote vs base） */
/** 'conflict:<reason>' 编码冲突原因；非冲突值是 op.type 字面量 */
/**
 * 共享内容相等时 base 存在的 3×3 表仍照 DESIGN §2.7：改了/改了 = 冲突。
 * （同内容收敛只在 base 缺失的并集降级语义下成立）
 */
const MATRIX: Record<Cell, Record<Cell, string>> = {
  same: {
    same: 'noop',
    changed: 'pull',
    deleted: 'pull-delete',
  },
  changed: {
    same: 'push',
    changed: 'conflict:modify-vs-modify',
    deleted: 'conflict:local-modify-vs-remote-delete',
  },
  deleted: {
    same: 'push-delete',
    changed: 'conflict:local-delete-vs-remote-modify',
    deleted: 'noop',
  },
};

const CELLS: Cell[] = ['same', 'changed', 'deleted'];

describe('compareFile — mirror 3×3 全枚举', () => {
  for (const localCell of CELLS) {
    for (const remoteCell of CELLS) {
      const expected = MATRIX[localCell][remoteCell];
      it(`local=${localCell} × remote=${remoteCell} → ${expected}`, () => {
        const op = compareFile(base, entryFor(localCell), entryFor(remoteCell), 'skills/a.md');

        expect(op.path).toBe('skills/a.md');
        expect(op.type).toBe(expected.split(':')[0]);

        if (expected.startsWith('conflict:')) {
          expect(op.type).toBe('conflict');
          if (op.type === 'conflict') {
            expect(op.reason).toBe(expected.split(':')[1]);
          }
        }
      });
    }
  }

  it('9 情形恰好覆盖 4 种非冲突 op + 3 种冲突原因（无遗漏桶）', () => {
    const seen = new Set<string>();
    for (const localCell of CELLS) {
      for (const remoteCell of CELLS) {
        const op = compareFile(base, entryFor(localCell), entryFor(remoteCell), 'p');
        seen.add(op.type === 'conflict' ? `conflict:${op.reason}` : op.type);
      }
    }
    expect([...seen].sort()).toEqual([
      'conflict:local-delete-vs-remote-modify',
      'conflict:local-modify-vs-remote-delete',
      'conflict:modify-vs-modify',
      'noop',
      'pull',
      'pull-delete',
      'push',
      'push-delete',
    ]);
  });

  it('显式命名用例：双删 → noop（DESIGN 8 行补全项）', () => {
    expect(compareFile(base, undefined, undefined, 'x')).toEqual({ type: 'noop', path: 'x' });
  });

  it('显式命名用例：local 删 + remote 未变 → push-delete', () => {
    expect(compareFile(base, undefined, base, 'x')).toEqual({ type: 'push-delete', path: 'x' });
  });

  it('双方同改不同内容（base 存在）→ conflict modify-vs-modify', () => {
    expect(compareFile(base, modified, json('v3'), 'x')).toEqual({
      type: 'conflict', path: 'x', reason: 'modify-vs-modify',
    });
  });

  it('显式命名用例：base 存在时双方同改同值也按 DESIGN 表判冲突（不静默收敛）', () => {
    expect(compareFile(base, modified, json(NEW_CONTENT), 'x')).toEqual({
      type: 'conflict', path: 'x', reason: 'modify-vs-modify',
    });
  });

  it('kind 不影响判定（json vs file 同内容即相等）', () => {
    const asFile: SnapshotEntry = { kind: 'file', content: BASE_CONTENT };
    expect(compareFile(base, asFile, base, 'x')).toEqual({ type: 'noop', path: 'x' });
  });

  it('三方都不存在（防御）→ noop', () => {
    expect(compareFile(undefined, undefined, undefined, 'x')).toEqual({ type: 'noop', path: 'x' });
  });

  it('无 base（首次对接）降级：仅 local → push；仅 remote → pull；同路径异内容 → 冲突；同内容 → noop', () => {
    expect(compareFile(undefined, modified, undefined, 'x')).toEqual({ type: 'push', path: 'x' });
    expect(compareFile(undefined, undefined, modified, 'x')).toEqual({ type: 'pull', path: 'x' });
    expect(compareFile(undefined, modified, json('other'), 'x')).toEqual({
      type: 'conflict', path: 'x', reason: 'modify-vs-modify',
    });
    expect(compareFile(undefined, modified, json(NEW_CONTENT), 'x')).toEqual({ type: 'noop', path: 'x' });
  });
});

describe('compareCategory — 三方路径并集', () => {
  const files = (entries: Record<string, SnapshotEntry>): SnapshotFiles => new Map(Object.entries(entries));

  it('一方新增：local 新增 → push；remote 新增 → pull', () => {
    const empty = files({});
    const local = files({ 'local-new.md': modified });
    const remote = files({ 'remote-new.md': modified });

    expect(compareCategory(empty, local, empty)).toEqual([{ type: 'push', path: 'local-new.md' }]);
    expect(compareCategory(empty, empty, remote)).toEqual([{ type: 'pull', path: 'remote-new.md' }]);
  });

  it('一方删除：local 删 → push-delete；remote 删 → pull-delete', () => {
    const baseFiles = files({ 'gone.md': base });

    expect(compareCategory(baseFiles, files({}), baseFiles)).toEqual([
      { type: 'push-delete', path: 'gone.md' },
    ]);
    expect(compareCategory(baseFiles, baseFiles, files({}))).toEqual([
      { type: 'pull-delete', path: 'gone.md' },
    ]);
  });

  it('共同新增不同内容 → conflict modify-vs-modify（无 base 并集语义）', () => {
    const local = files({ 'same-path.md': modified });
    const remote = files({ 'same-path.md': json('other') });

    expect(compareCategory(files({}), local, remote)).toEqual([
      { type: 'conflict', path: 'same-path.md', reason: 'modify-vs-modify' },
    ]);
  });

  it('共同新增相同内容 → noop', () => {
    const local = files({ 'same-path.md': modified });
    const remote = files({ 'same-path.md': json(NEW_CONTENT) });
    expect(compareCategory(files({}), local, remote)).toEqual([
      { type: 'noop', path: 'same-path.md' },
    ]);
  });

  it('双删 = noop（并集含路径，但两个 undefined 比较为 noop）', () => {
    const baseFiles = files({ 'gone.md': base });
    expect(compareCategory(baseFiles, files({}), files({}))).toEqual([
      { type: 'noop', path: 'gone.md' },
    ]);
  });

  it('输出按 path 排序（确定性），且并集不丢不重', () => {
    const baseFiles = files({ 'b.md': base, 'c.md': base });
    const local = files({ 'a.md': modified, 'b.md': modified });
    const remote = files({ 'c.md': modified, 'd.md': modified });

    const ops = compareCategory(baseFiles, local, remote);
    expect(ops.map((op) => op.path)).toEqual(['a.md', 'b.md', 'c.md', 'd.md']);
    // b: local 改 / remote 删 → 冲突；c: local 删 / remote 改 → 冲突
    expect(ops.map((op) => op.type)).toEqual(['push', 'conflict', 'conflict', 'pull']);
  });

  it('混合矩阵：一次调用同时产出 push / pull / delete / conflict', () => {
    const baseFiles = files({
      'push.md': base,
      'pull.md': base,
      'del-local.md': base,
      'conflict.md': base,
      'quiet.md': base,
    });
    const local = files({
      'push.md': modified,
      'pull.md': base,
      'conflict.md': modified,
      'quiet.md': base,
    });
    const remote = files({
      'push.md': base,
      'pull.md': modified,
      'del-local.md': base,
      'conflict.md': json('remote-change'),
      'quiet.md': base,
    });

    expect(compareCategory(baseFiles, local, remote)).toEqual([
      { type: 'conflict', path: 'conflict.md', reason: 'modify-vs-modify' },
      { type: 'push-delete', path: 'del-local.md' },
      { type: 'pull', path: 'pull.md' },
      { type: 'push', path: 'push.md' },
      { type: 'noop', path: 'quiet.md' },
    ]);
  });

  it('三方全空 → 空数组', () => {
    expect(compareCategory(files({}), files({}), files({}))).toEqual([]);
  });
});
