/**
 * drift.ts 测试矩阵 —— merge / mirror / 降级 三类聚合用例 + excludeKeys 剥离。
 */

import { describe, expect, it } from 'vitest';
import { computeDrift, stripExcludeKeys } from '../../src/core/engine/index.js';
import type {
  AdapterSnapshot,
  CategorySnapshot,
  SnapshotEntry,
  SnapshotFiles,
  SyncMode,
} from '../../src/core/types.js';

const json = (value: unknown): SnapshotEntry => ({ kind: 'json', content: JSON.stringify(value) });
const file = (content: string): SnapshotEntry => ({ kind: 'file', content });
const corrupt = (content: string): SnapshotEntry => ({ kind: 'json', content });

function category(
  adapterId: string,
  name: string,
  mode: SyncMode,
  entries: Record<string, SnapshotEntry>,
): CategorySnapshot {
  return { adapterId, category: name, mode, files: new Map(Object.entries(entries)) as SnapshotFiles };
}

function adapter(adapterId: string, ...categories: CategorySnapshot[]): AdapterSnapshot {
  return { adapterId, categories };
}

const only = (drifts: ReturnType<typeof computeDrift>) => drifts[0]!.categories[0]!;

describe('computeDrift — merge 分类聚合', () => {
  const base = adapter('pi', category('pi', 'settings', 'merge', {
    'settings.json': json({ a: 1, b: 2 }),
  }));
  const local = adapter('pi', category('pi', 'settings', 'merge', {
    'settings.json': json({ a: 9, b: 2, c: 3 }),
  }));

  it('remote 缺省 = base 时，local 单边改动 → push = changed+added+deleted', () => {
    const drift = only(computeDrift([base], [local]));
    expect(drift.mode).toBe('merge');
    expect(drift.push).toBe(2); // a 改 + c 增
    expect(drift.pull).toBe(0);
    expect(drift.conflicts).toBe(0);
    expect(drift.changedKeys).toEqual(['settings.json:a', 'settings.json:c']);
    expect(drift.mergeConflicts).toEqual([]);
  });

  it('local 删键 → 计入 push 并记 deleted 键', () => {
    const deleted = adapter('pi', category('pi', 'settings', 'merge', {
      'settings.json': json({ a: 1 }),
    }));
    const drift = only(computeDrift([base], [deleted]));
    expect(drift.push).toBe(1);
    expect(drift.changedKeys).toEqual(['settings.json:b']);
  });

  it('双方改同键不同值 → mergeConflicts 聚合，conflicts 计数', () => {
    const remote = adapter('pi', category('pi', 'settings', 'merge', {
      'settings.json': json({ a: 5, b: 2 }),
    }));
    const drift = only(computeDrift([base], [local], [remote]));
    expect(drift.conflicts).toBe(1);
    expect(drift.mergeConflicts).toEqual([
      { keyPath: 'a', reason: 'both-modified', base: 1, local: 9, remote: 5 },
    ]);
  });

  it('remote 单边改动 → pull 计数（local 未动）', () => {
    const untouched = adapter('pi', category('pi', 'settings', 'merge', {
      'settings.json': json({ a: 1, b: 2 }),
    }));
    const remote = adapter('pi', category('pi', 'settings', 'merge', {
      'settings.json': json({ a: 1, b: 2, d: 4 }),
    }));
    const drift = only(computeDrift([base], [untouched], [remote]));
    expect(drift.push).toBe(0);
    expect(drift.pull).toBe(1);
  });

  it('local 改键 + remote 删同一键 → modify-vs-delete 冲突', () => {
    const remote = adapter('pi', category('pi', 'settings', 'merge', {
      'settings.json': json({ b: 2 }), // remote 删了 a
    }));
    const drift = only(computeDrift([base], [local], [remote]));
    expect(drift.conflicts).toBe(1);
    expect(drift.mergeConflicts).toContainEqual({
      keyPath: 'a', reason: 'modify-vs-delete', base: 1, local: 9,
    });
  });

  it('local 未动而 remote 删键 → 无冲突；远端删除体现为 pull 漂移', () => {
    const remote = adapter('pi', category('pi', 'settings', 'merge', {
      'settings.json': json({ b: 2 }),
    }));
    const drift = only(computeDrift([base], [base], [remote]));
    expect(drift.conflicts).toBe(0);
    expect(drift.push).toBe(0);
    expect(drift.pull).toBe(1); // 远端删了键 a，pull 时本地会随之删除
  });

  it('merge 分类多文件（settings.json + keybindings.json）：changedKeys 带文件前缀', () => {
    const multiBase = adapter('pi', category('pi', 'settings', 'merge', {
      'settings.json': json({ a: 1 }),
      'keybindings.json': json({ key: 'ctrl+k' }),
    }));
    const multiLocal = adapter('pi', category('pi', 'settings', 'merge', {
      'settings.json': json({ a: 2 }),
      'keybindings.json': json({ key: 'ctrl+k' }),
    }));
    const drift = only(computeDrift([multiBase], [multiLocal]));
    expect(drift.push).toBe(1);
    expect(drift.changedKeys).toEqual(['settings.json:a']);
  });

  it('merge 分类新增整文件 → push 1 且 changedKeys 记文件级键', () => {
    const addedBase = adapter('pi', category('pi', 'settings', 'merge', {}));
    const addedLocal = adapter('pi', category('pi', 'settings', 'merge', {
      'keybindings.json': json({ key: 'ctrl+k' }),
    }));
    const drift = only(computeDrift([addedBase], [addedLocal]));
    expect(drift.push).toBe(1);
    expect(drift.changedKeys).toEqual(['keybindings.json:key']);
  });
});

describe('computeDrift — mirror 分类聚合', () => {
  it('push-delete 计入 push（计数含 push-delete）', () => {
    const base = adapter('pi', category('pi', 'skills', 'mirror', { 'a.md': file('v1') }));
    const local = adapter('pi', category('pi', 'skills', 'mirror', {}));
    const drift = only(computeDrift([base], [local]));
    expect(drift.push).toBe(1);
    expect(drift.ops).toEqual([{ type: 'push-delete', path: 'a.md' }]);
  });

  it('pull-delete 计入 pull', () => {
    const base = adapter('pi', category('pi', 'skills', 'mirror', { 'a.md': file('v1') }));
    const local = adapter('pi', category('pi', 'skills', 'mirror', { 'a.md': file('v1') }));
    const remote = adapter('pi', category('pi', 'skills', 'mirror', {}));
    const drift = only(computeDrift([base], [local], [remote]));
    expect(drift.pull).toBe(1);
    expect(drift.ops).toEqual([{ type: 'pull-delete', path: 'a.md' }]);
  });

  it('冲突计入 conflicts 且 ops 保留 conflict op', () => {
    const base = adapter('pi', category('pi', 'skills', 'mirror', { 'a.md': file('v1') }));
    const local = adapter('pi', category('pi', 'skills', 'mirror', { 'a.md': file('v2') }));
    const remote = adapter('pi', category('pi', 'skills', 'mirror', { 'a.md': file('v3') }));
    const drift = only(computeDrift([base], [local], [remote]));
    expect(drift.conflicts).toBe(1);
    expect(drift.push).toBe(0);
    expect(drift.ops).toEqual([{ type: 'conflict', path: 'a.md', reason: 'modify-vs-modify' }]);
  });
});

describe('computeDrift — merge 分类混入 file 条目降级', () => {
  it('kind:file 条目降级走 mirror compareFile，不炸且不进 mergeConflicts', () => {
    const base = adapter('pi', category('pi', 'settings', 'merge', {
      'settings.json': json({ a: 1 }),
      'notes.txt': file('原始'),
    }));
    const local = adapter('pi', category('pi', 'settings', 'merge', {
      'settings.json': json({ a: 1 }),
      'notes.txt': file('改过'),
    }));

    const drift = only(computeDrift([base], [local]));
    expect(drift.push).toBe(1);
    expect(drift.mergeConflicts).toEqual([]);
    expect(drift.ops).toEqual([{ type: 'push', path: 'notes.txt' }]);
  });

  it('JSON 损坏的 merge 条目（kind:json 但 parse 失败）同样降级', () => {
    const base = adapter('pi', category('pi', 'settings', 'merge', { 'broken.json': corrupt('{oops') }));
    const local = adapter('pi', category('pi', 'settings', 'merge', { 'broken.json': file('{fixed') }));

    const drift = only(computeDrift([base], [local]));
    expect(drift.push).toBe(1);
    expect(drift.ops).toEqual([{ type: 'push', path: 'broken.json' }]);
    expect(drift.mergeConflicts).toEqual([]);
  });

  it('降级条目双删 → noop，不产生任何计数', () => {
    const base = adapter('pi', category('pi', 'settings', 'merge', { 'notes.txt': file('x') }));
    const local = adapter('pi', category('pi', 'settings', 'merge', {}));
    const remote = adapter('pi', category('pi', 'settings', 'merge', {}));
    const drift = only(computeDrift([base], [local], [remote]));
    expect(drift.push).toBe(0);
    expect(drift.pull).toBe(0);
    expect(drift.conflicts).toBe(0);
    expect(drift.ops).toEqual([{ type: 'noop', path: 'notes.txt' }]);
  });
});

describe('computeDrift — excludeKeys 剥离后不计 drift', () => {
  it('stripExcludeKeys 剥掉 apiKeys 后，同快照比较 push = 0', () => {
    const base = adapter('pi', category('pi', 'models', 'merge', {
      'models.json': json({ models: { openai: 'gpt-4' }, apiKeys: 'OLD' }),
    }));
    const local = adapter('pi', category('pi', 'models', 'merge', {
      'models.json': json({ models: { openai: 'gpt-4' }, apiKeys: 'NEW-DIFFERENT' }),
    }));

    // 剥离前：apiKeys 变化会被记为 drift
    expect(only(computeDrift([base], [local])).push).toBe(1);

    const keys = { models: ['apiKeys'] };
    const drift = only(computeDrift([stripExcludeKeys(base, keys)], [stripExcludeKeys(local, keys)]));
    expect(drift.push).toBe(0);
    expect(drift.changedKeys).toEqual([]);
  });

  it('剥离只在顶层：apiKeys 之外的键仍正常计 drift', () => {
    const base = adapter('pi', category('pi', 'models', 'merge', {
      'models.json': json({ models: { openai: 'gpt-4' }, apiKeys: 'OLD' }),
    }));
    const local = adapter('pi', category('pi', 'models', 'merge', {
      'models.json': json({ models: { openai: 'gpt-5' }, apiKeys: 'NEW' }),
    }));

    const keys = { models: ['apiKeys'] };
    const drift = only(computeDrift([stripExcludeKeys(base, keys)], [stripExcludeKeys(local, keys)]));
    expect(drift.push).toBe(1);
    expect(drift.changedKeys).toEqual(['models.json:models.openai']);
  });

  it('stripExcludeKeys 不可变：不改写入参快照', () => {
    const source = adapter('pi', category('pi', 'models', 'merge', {
      'models.json': json({ apiKeys: 'SECRET', keep: 1 }),
    }));
    const stripped = stripExcludeKeys(source, { models: ['apiKeys'] });
    expect(JSON.parse(stripped.categories[0]!.files.get('models.json')!.content)).toEqual({ keep: 1 });
    expect(JSON.parse(source.categories[0]!.files.get('models.json')!.content)).toEqual({ apiKeys: 'SECRET', keep: 1 });
  });
});

describe('computeDrift — 多 adapter / 多分类', () => {
  it('按 adapter 分组，adapter 并集（local 独有 adapter 也出现）', () => {
    const base = [
      adapter('pi', category('pi', 'settings', 'merge', { 'settings.json': json({ a: 1 }) })),
    ];
    const local = [
      adapter('pi', category('pi', 'settings', 'merge', { 'settings.json': json({ a: 2 }) })),
      adapter('herdr', category('herdr', 'config', 'mirror', { 'config.toml': file('x') })),
    ];
    const drifts = computeDrift(base, local);
    expect(drifts.map((d) => d.adapterId).sort()).toEqual(['herdr', 'pi']);
    expect(drifts.find((d) => d.adapterId === 'pi')!.categories[0]!.push).toBe(1);
    expect(drifts.find((d) => d.adapterId === 'herdr')!.categories[0]!.push).toBe(1);
  });

  it('分类并集：分类名排序稳定，缺失一侧按空处理', () => {
    const base = [
      adapter('pi',
        category('pi', 'agents', 'mirror', {}),
        category('pi', 'settings', 'merge', { 'settings.json': json({ a: 1 }) }),
      ),
    ];
    const local = [
      adapter('pi', category('pi', 'settings', 'merge', { 'settings.json': json({ a: 9 }) })),
    ];
    const drift = computeDrift(base, local)[0]!;
    expect(drift.categories.map((c) => c.category)).toEqual(['agents', 'settings']);
    expect(drift.categories[0]!.push).toBe(0); // agents 三方皆空
    expect(drift.categories[1]!.push).toBe(1);
  });

  it('无漂移时全部计数为 0（含 ops 全 noop）', () => {
    const snap = adapter('pi', category('pi', 'skills', 'mirror', { 'a.md': file('v1') }));
    const drift = only(computeDrift([snap], [snap]));
    expect(drift).toMatchObject({ push: 0, pull: 0, conflicts: 0 });
  });
});
