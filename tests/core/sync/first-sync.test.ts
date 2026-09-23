/**
 * P1-W5 验收测试（docs/m3-plan.md §2.4 语义矩阵 / §3-P1-W5）。
 *
 * 纯函数测试：零 fs、零 HOMER_HOME —— 全部通过手工构造的快照驱动（与 plan.test.ts 同风格）。
 * 分组：
 *   A. `emptyBaseSnapshots` 结构与 config 对齐（空 Map / 顺序 / enabled 过滤）
 *   B. `pull` 模式：remote 全量 write + 无 delete + local-only 无动作
 *   C. `merge` 模式：mirror 冲突（file 级）与 merge-JSON 键级并集冲突（keyPaths）
 *   D. `skip` 模式：空 plan
 *   E. excludeKeys 不变量：`__REQUIRED__` 永不流入首次对接计划
 *   F. 三模式 × 文件状态 表驱动矩阵
 */

import { describe, expect, it } from 'vitest';

import {
  emptyBaseSnapshots,
  planFirstContact,
  type FirstContactMode,
} from '../../../src/core/sync/first-sync.js';
import * as barrel from '../../../src/core/sync/index.js';
import { planPull } from '../../../src/core/sync/plan.js';
import { REQUIRED_PLACEHOLDER, serializeJsonContent } from '../../../src/core/sync/excluded-keys.js';
import type { PullAction, PullConflictAction, PullWriteAction } from '../../../src/core/sync/types.js';
import type {
  AdapterConfig,
  AdapterSnapshot,
  CategoryConfig,
  HomerConfig,
  SnapshotEntry,
} from '../../../src/core/types.js';

/* ------------------------------------------------------------------ */
/* 构造工具                                                            */
/* ------------------------------------------------------------------ */

const json = (value: unknown): SnapshotEntry => ({ kind: 'json', content: JSON.stringify(value) });
const jsonText = (content: string): SnapshotEntry => ({ kind: 'json', content });
const file = (content: string): SnapshotEntry => ({ kind: 'file', content });

interface CategorySpec { mode: 'mirror' | 'merge'; excludeKeys?: string[] }

/** 一个 adapter 的 config（`skills` mirror + `settings` merge 为默认；可覆盖）。 */
function configOf(
  categories: Record<string, CategorySpec> = {
    skills: { mode: 'mirror' },
    settings: { mode: 'merge' },
  },
  adapterExtra: Partial<AdapterConfig> = {},
): HomerConfig {
  return {
    version: 1,
    adapters: {
      pi: {
        root: '/tmp/pi',
        categories: Object.fromEntries(
          Object.entries(categories).map(([name, spec]) => [
            name,
            {
              paths: [spec.mode === 'mirror' ? 'skills/' : 'settings.json'],
              mode: spec.mode,
              ...(spec.excludeKeys ? { excludeKeys: spec.excludeKeys } : {}),
            } satisfies CategoryConfig,
          ]),
        ),
        ...adapterExtra,
      },
    },
  };
}

/** 构造一个 adapter 快照：`{ category → { relPath → entry } }`（顺序与 config 声明一致）。 */
function snapshot(files: Record<string, Record<string, SnapshotEntry>>): AdapterSnapshot[] {
  return [
    {
      adapterId: 'pi',
      categories: Object.entries(files).map(([category, entries]) => ({
        adapterId: 'pi',
        category,
        mode: category === 'settings' ? 'merge' : 'mirror',
        files: new Map(Object.entries(entries)),
      })),
    },
  ];
}

const writes = (actions: readonly PullAction[]): PullWriteAction[] =>
  actions.filter((action): action is PullWriteAction => action.type === 'write');
const conflictsOf = (actions: readonly PullAction[]): PullConflictAction[] =>
  actions.filter((action): action is PullConflictAction => action.type === 'conflict');

/* ================================================================== */
/* A0. barrel 导出                                                      */
/* ================================================================== */

describe('sync barrel — W5 导出', () => {
  it('`src/core/sync/index.ts` 导出 planFirstContact / emptyBaseSnapshots（命令层从 barrel 取）', () => {
    expect(barrel.planFirstContact).toBe(planFirstContact);
    expect(barrel.emptyBaseSnapshots).toBe(emptyBaseSnapshots);
  });
});

/* ================================================================== */
/* A. emptyBaseSnapshots                                               */
/* ================================================================== */

describe('emptyBaseSnapshots — 结构与 config 对齐', () => {
  it('每个 enabled adapter / category 一个**空 Map**，mode 从 config 带出，顺序同 config', () => {
    const config: HomerConfig = {
      version: 1,
      adapters: {
        pi: {
          root: '/tmp/pi',
          categories: {
            skills: { paths: ['skills/'], mode: 'mirror' },
            settings: { paths: ['settings.json'], mode: 'merge' },
            models: { paths: ['models.json'], mode: 'merge' },
          },
        },
        herdr: {
          root: '/tmp/herdr',
          categories: { config: { paths: ['config.toml'], mode: 'mirror' } },
        },
      },
    };

    const base = emptyBaseSnapshots(config);

    expect(base.map((item) => item.adapterId)).toEqual(['pi', 'herdr']);
    expect(base[0]!.categories.map((item) => item.category)).toEqual(['skills', 'settings', 'models']);
    expect(base[0]!.categories.map((item) => item.mode)).toEqual(['mirror', 'merge', 'merge']);
    expect(base[1]!.categories.map((item) => item.category)).toEqual(['config']);
    for (const adapter of base) {
      for (const category of adapter.categories) {
        expect(category.files).toBeInstanceOf(Map);
        expect(category.files.size).toBe(0);
        expect(category.adapterId).toBe(adapter.adapterId);
      }
    }
  });

  it('跳过 enabled === false 的 adapter 与 category（不参与同步者不出现）', () => {
    const config: HomerConfig = {
      version: 1,
      adapters: {
        pi: {
          root: '/tmp/pi',
          categories: {
            skills: { paths: ['skills/'], mode: 'mirror' },
            settings: { paths: ['settings.json'], mode: 'merge', enabled: false },
          },
        },
        herdr: { root: '/tmp/herdr', enabled: false, categories: { config: { paths: ['config.toml'], mode: 'mirror' } } },
      },
    };

    const base = emptyBaseSnapshots(config);

    expect(base.map((item) => item.adapterId)).toEqual(['pi']);
    expect(base[0]!.categories.map((item) => item.category)).toEqual(['skills']);
  });

  it('空 config（无 adapter）→ 空数组；返回的 Map 相互独立（可变性隔离）', () => {
    expect(emptyBaseSnapshots({ version: 1, adapters: {} })).toEqual([]);

    const base = emptyBaseSnapshots(configOf());
    base[0]!.categories[0]!.files.set('x', file('y'));
    expect(base[0]!.categories[1]!.files.size).toBe(0);
    expect(emptyBaseSnapshots(configOf())[0]!.categories[0]!.files.size).toBe(0);
  });
});

/* ================================================================== */
/* B. pull 模式                                                        */
/* ================================================================== */

describe('planFirstContact — pull 模式', () => {
  const config = configOf();

  it('remote 每个文件 → write（mirror 与 merge 分类都算），content = remote 内容', () => {
    const local = snapshot({ skills: {}, settings: {} });
    const remote = snapshot({
      skills: { 'foo/SKILL.md': file('remote skill') },
      settings: { 'settings.json': jsonText('{"theme":"dark"}') },
    });

    const plan = planFirstContact(config, local, remote, 'pull');

    expect(plan.mode).toBe('pull');
    expect(plan.actions).toEqual([
      { type: 'write', adapterId: 'pi', category: 'skills', relPath: 'foo/SKILL.md', content: 'remote skill' },
      {
        type: 'write', adapterId: 'pi', category: 'settings', relPath: 'settings.json',
        content: '{"theme":"dark"}', // 远端原文逐字（merge 分类不重新序列化）
      },
    ]);
  });

  it('双方都有且内容不同 → 仍写 remote（远端覆盖语义）', () => {
    const local = snapshot({
      skills: { 'foo/SKILL.md': file('local skill') },
      settings: { 'settings.json': jsonText('{"theme":"light","localOnly":true}') },
    });
    const remote = snapshot({
      skills: { 'foo/SKILL.md': file('remote skill') },
      settings: { 'settings.json': jsonText('{"theme":"dark"}') },
    });

    const plan = planFirstContact(config, local, remote, 'pull');

    expect(plan.actions).toEqual([
      { type: 'write', adapterId: 'pi', category: 'skills', relPath: 'foo/SKILL.md', content: 'remote skill' },
      {
        type: 'write', adapterId: 'pi', category: 'settings', relPath: 'settings.json',
        content: '{"theme":"dark"}',
      },
    ]);
    expect(conflictsOf(plan.actions)).toEqual([]);
  });

  it('local-only 文件（mirror 与 merge 都有）→ 零动作；全程无 delete', () => {
    const local = snapshot({
      skills: { 'local/SKILL.md': file('local only'), 'both/SKILL.md': file('a') },
      settings: { 'settings.json': jsonText('{"localOnly":1}') },
    });
    const remote = snapshot({
      skills: { 'both/SKILL.md': file('b') },
      settings: {},
    });

    const plan = planFirstContact(config, local, remote, 'pull');

    // 只应触碰远端存在的项；`but/SKILL.md` 的 write（remote 覆盖）在列
    expect(plan.actions).toEqual([
      { type: 'write', adapterId: 'pi', category: 'skills', relPath: 'both/SKILL.md', content: 'b' },
    ]);
    expect(plan.actions.some((action) => action.type === 'delete')).toBe(false);
    expect(plan.actions.some((action) => action.relPath === 'local/SKILL.md')).toBe(false);
    expect(plan.actions.some((action) => action.relPath === 'settings.json')).toBe(false);
    expect(conflictsOf(plan.actions)).toEqual([]);
  });

  it('远端为空（store 空）→ 空动作清单', () => {
    const local = snapshot({ skills: { 'local/SKILL.md': file('x') }, settings: {} });
    expect(planFirstContact(config, local, snapshot({ skills: {}, settings: {} }), 'pull').actions).toEqual([]);
  });
});

/* ================================================================== */
/* C. merge 模式                                                       */
/* ================================================================== */

describe('planFirstContact — merge 模式', () => {
  const config = configOf();
  const empty = snapshot({ skills: {}, settings: {} });

  it('mirror：remote-only → write；local-only → 无动作；双方都有且不同 → file 级 conflict（无 delete）', () => {
    const local = snapshot({
      skills: {
        'local/SKILL.md': file('local only'),
        'both/SKILL.md': file('local version'),
        'same/SKILL.md': file('identical'),
      },
      settings: {},
    });
    const remote = snapshot({
      skills: {
        'remote/SKILL.md': file('remote only'),
        'both/SKILL.md': file('remote version'),
        'same/SKILL.md': file('identical'),
      },
      settings: {},
    });

    const plan = planFirstContact(config, local, remote, 'merge');

    expect(plan.mode).toBe('merge');
    expect(writes(plan.actions)).toEqual([
      { type: 'write', adapterId: 'pi', category: 'skills', relPath: 'remote/SKILL.md', content: 'remote only' },
    ]);
    const conflicts = conflictsOf(plan.actions);
    expect(conflicts).toHaveLength(1);
    expect(conflicts[0]).toMatchObject({
      type: 'conflict',
      adapterId: 'pi',
      category: 'skills',
      relPath: 'both/SKILL.md',
      reason: 'modify-vs-modify',
      localContent: 'local version',
      remoteContent: 'remote version',
    });
    // local-only / same 无动作；无 delete、无 keyPaths（mirror 是文件级）
    expect(plan.actions).toHaveLength(2);
    expect(plan.actions.some((action) => action.type === 'delete')).toBe(false);
    expect(conflicts[0]!.keyPaths).toBeUndefined();
  });

  it('merge-JSON：remote-only 文件 → write（远端原文）；local-only 文件 → 无动作', () => {
    const local = snapshot({
      skills: {},
      settings: { 'local-only.json': jsonText('{"mine":1}') },
    });
    const remote = snapshot({
      skills: {},
      settings: { 'settings.json': jsonText('{"theme":"dark"}') },
    });

    const plan = planFirstContact(config, local, remote, 'merge');

    expect(plan.actions).toEqual([
      {
        type: 'write', adapterId: 'pi', category: 'settings', relPath: 'settings.json',
        content: '{"theme":"dark"}',
      },
    ]);
  });

  it('merge-JSON：双方都有、键不相交 → 键级并集 clean write（无冲突，local 独有键保留）', () => {
    const local = snapshot({ skills: {}, settings: { 'settings.json': json({ a: 1 }) } });
    const remote = snapshot({ skills: {}, settings: { 'settings.json': json({ b: 2 }) } });

    const plan = planFirstContact(config, local, remote, 'merge');

    const write = writes(plan.actions);
    expect(write).toHaveLength(1);
    expect(conflictsOf(plan.actions)).toEqual([]);
    expect(JSON.parse(write[0]!.content)).toEqual({ a: 1, b: 2 });
    expect(write[0]!.content).toBe(serializeJsonContent({ a: 1, b: 2 }));
  });

  it('merge-JSON：双方都有、同键不同值 → conflict merge-keys + keyPaths（单键）', () => {
    const local = snapshot({ skills: {}, settings: { 'settings.json': json({ a: 1, b: 2 }) } });
    const remote = snapshot({ skills: {}, settings: { 'settings.json': json({ a: 9, b: 2 }) } });

    const plan = planFirstContact(config, local, remote, 'merge');

    expect(plan.actions).toHaveLength(1);
    const conflict = conflictsOf(plan.actions)[0]!;
    expect(conflict.reason).toBe('merge-keys');
    expect(conflict.keyPaths).toEqual(['a']);
    // 键级裁决只给 keyPaths（防真实密钥 / 占位符外流）
    expect(conflict.localContent).toBeUndefined();
    expect(conflict.remoteContent).toBeUndefined();
  });

  it('merge-JSON：多个冲突键（含嵌套点路径）→ keyPaths 全量列出', () => {
    const local = snapshot({
      skills: {},
      settings: { 'settings.json': json({ a: 1, b: { x: 1, y: 2 }, keep: 'l' }) },
    });
    const remote = snapshot({
      skills: {},
      settings: { 'settings.json': json({ a: 3, b: { x: 9, z: 5 }, keep: 'l' }) },
    });

    const plan = planFirstContact(config, local, remote, 'merge');

    const conflict = conflictsOf(plan.actions)[0]!;
    expect(conflict.reason).toBe('merge-keys');
    expect(conflict.keyPaths).toEqual(['a', 'b.x']);
  });

  it('merge-JSON：内容相同 → 无动作（双方都未相对空 base 变化）', () => {
    const local = snapshot({ skills: {}, settings: { 'settings.json': json({ a: 1 }) } });
    const remote = snapshot({ skills: {}, settings: { 'settings.json': jsonText('{"a":1}') } });

    expect(planFirstContact(config, local, remote, 'merge').actions).toEqual([]);
  });

  it('merge-JSON 降级（非 JSON 对象）→ file 级 conflict（不送进 mergeJson，无 keyPaths）', () => {
    const local = snapshot({ skills: {}, settings: { 'settings.json': file('local text') } });
    const remote = snapshot({ skills: {}, settings: { 'settings.json': file('remote text') } });

    const plan = planFirstContact(config, local, remote, 'merge');

    expect(plan.actions).toEqual([
      {
        type: 'conflict', adapterId: 'pi', category: 'settings', relPath: 'settings.json',
        reason: 'modify-vs-modify', localContent: 'local text', remoteContent: 'remote text',
      },
    ]);
  });

  it('merge-JSON 根值为数组（无法键级合并）→ file 级 conflict，不把用户文件覆写成 {}', () => {
    const local = snapshot({ skills: {}, settings: { 'settings.json': jsonText('["a","b"]') } });
    const remote = snapshot({ skills: {}, settings: { 'settings.json': jsonText('["a","c"]') } });

    const plan = planFirstContact(config, local, remote, 'merge');

    const conflict = conflictsOf(plan.actions)[0]!;
    expect(conflict.reason).toBe('modify-vs-modify');
    expect(conflict.keyPaths).toBeUndefined();
    expect(writeContent(plan.actions)).not.toContain('{}');
  });

  it('merge-JSON 降级（kind=file 但内容是可解析对象）→ 与 planPull 同口径走 mirror（不重写）', () => {
    // 两个条目都是 kind:'file'（merge 分类里解析失败 / 降级），内容恰好都是合法对象 JSON。
    // `planPull` 的 isDegraded 只看 kind === 'file' → 本模块的基线合成必须同样排除，
    // 否则会把它们强送 mergeJson，把「同内容」误报成冲突。
    const local = snapshot({ skills: {}, settings: { 'settings.json': file('{"a":1}') } });
    const remote = snapshot({ skills: {}, settings: { 'settings.json': file('{"a":1}') } });

    expect(planFirstContact(config, local, remote, 'merge').actions).toEqual([]);

    // 同 kind 但内容不同 → file 级冲突（mirror 降级语义）
    const remote2 = snapshot({ skills: {}, settings: { 'settings.json': file('{"a":2}') } });
    const conflict = conflictsOf(planFirstContact(config, local, remote2, 'merge').actions)[0]!;
    expect(conflict.reason).toBe('modify-vs-modify');
    expect(conflict.keyPaths).toBeUndefined();
  });

  it('D5 灾难守卫：三种模式都不产生任何 delete 动作（保留的本地文件随后成为 push 漂移）', () => {
    const local = snapshot({
      skills: { 'local/SKILL.md': file('keep me') },
      settings: { 'settings.json': jsonText('{"localKey":"keep"}') },
    });
    const remote = snapshot({
      skills: { 'remote/SKILL.md': file('r') },
      settings: { 'settings.json': jsonText('{"remoteKey":1}') },
    });

    for (const mode of ['pull', 'merge', 'skip'] as FirstContactMode[]) {
      const plan = planFirstContact(config, local, remote, mode);
      expect(plan.actions.some((action) => action.type === 'delete'), `${mode} 不应有 delete`).toBe(false);
    }

    // merge：local-only 的 mirror 文件与 merge 文件都不进计划（除 clean write 的并集）
    const mergePlan = planFirstContact(config, local, remote, 'merge');
    expect(mergePlan.actions.some((action) => action.relPath === 'local/SKILL.md')).toBe(false);
  });

  it('空 local（全新机器）→ 全量 write（remote-only 逐字写远端原文），不含冲突', () => {
    const remote = snapshot({
      skills: { 'a/SKILL.md': file('a') },
      settings: { 'settings.json': jsonText('{"x":1}') },
    });

    const plan = planFirstContact(config, empty, remote, 'merge');

    expect(writes(plan.actions)).toEqual([
      { type: 'write', adapterId: 'pi', category: 'skills', relPath: 'a/SKILL.md', content: 'a' },
      { type: 'write', adapterId: 'pi', category: 'settings', relPath: 'settings.json', content: '{"x":1}' },
    ]);
    expect(conflictsOf(plan.actions)).toEqual([]);
  });

  it('空 remote（store 空）→ 空动作清单', () => {
    const local = snapshot({ skills: { 'local/SKILL.md': file('x') }, settings: { 'settings.json': json({ a: 1 }) } });
    expect(planFirstContact(config, local, empty, 'merge').actions).toEqual([]);
  });
});

/* ================================================================== */
/* D. skip 模式                                                        */
/* ================================================================== */

describe('planFirstContact — skip 模式', () => {
  it('恒为空 plan（mode 回填，零动作，即使双方都有内容）', () => {
    const config = configOf();
    const local = snapshot({ skills: { 'a/SKILL.md': file('l') }, settings: { 'settings.json': json({ a: 1 }) } });
    const remote = snapshot({ skills: { 'b/SKILL.md': file('r') }, settings: { 'settings.json': json({ a: 2 }) } });

    expect(planFirstContact(config, local, remote, 'skip')).toEqual({ mode: 'skip', actions: [] });
  });
});

/* ================================================================== */
/* E. excludeKeys 不变量                                               */
/* ================================================================== */

describe('planFirstContact — excludeKeys 语义', () => {
  const config = configOf({ settings: { mode: 'merge', excludeKeys: ['apiKeys'] } });

  it('pull：remote 的 __REQUIRED__ 占位符被剥离，不流入 write 内容', () => {
    const local = snapshot({ settings: { 'settings.json': jsonText('{"apiKeys":"sk-local","theme":"light"}') } });
    const remote = snapshot({ settings: { 'settings.json': jsonText('{"apiKeys":"__REQUIRED__","theme":"dark"}') } });

    const plan = planFirstContact(config, local, remote, 'pull');

    const write = writes(plan.actions)[0]!;
    expect(JSON.parse(write.content)).toEqual({ theme: 'dark' });
    expect(write.content).not.toContain(REQUIRED_PLACEHOLDER);
    expect(JSON.stringify(plan)).not.toContain(REQUIRED_PLACEHOLDER);
  });

  it('merge：本地密钥键被植回 merge 结果（本地永不被远端占位符覆盖）', () => {
    const local = snapshot({ settings: { 'settings.json': jsonText('{"apiKeys":"sk-local","theme":"light"}') } });
    const remote = snapshot({ settings: { 'settings.json': jsonText('{"apiKeys":"__REQUIRED__","extra":1}') } });

    const plan = planFirstContact(config, local, remote, 'merge');

    const write = writes(plan.actions)[0]!;
    expect(JSON.parse(write.content)).toEqual({ theme: 'light', extra: 1, apiKeys: 'sk-local' });
    expect(JSON.stringify(plan)).not.toContain(REQUIRED_PLACEHOLDER);
  });

  it('merge：冲突键为 excluded 之外时 keyPaths 只列真冲突键，计划整体无占位符', () => {
    const local = snapshot({ settings: { 'settings.json': jsonText('{"apiKeys":"sk-local","theme":"light"}') } });
    const remote = snapshot({ settings: { 'settings.json': jsonText('{"apiKeys":"__REQUIRED__","theme":"dark"}') } });

    const plan = planFirstContact(config, local, remote, 'merge');

    const conflict = conflictsOf(plan.actions)[0]!;
    expect(conflict.reason).toBe('merge-keys');
    expect(conflict.keyPaths).toEqual(['theme']);
    expect(JSON.stringify(plan)).not.toContain(REQUIRED_PLACEHOLDER);
  });
});

/* ================================================================== */
/* F. 三模式 × 文件状态 表驱动矩阵                                      */
/* ================================================================== */

interface MatrixCase {
  name: string;
  mode: FirstContactMode;
  /** mirror 分类里某文件：local / remote 两侧的内容（undefined = 该侧无此文件）。 */
  mirror?: { local?: string; remote?: string };
  /** merge 分类里某文件：两侧的 JSON 文本（undefined = 该侧无此文件）。 */
  mergeJson?: { local?: string; remote?: string };
  expected: { writes: { category: string; relPath: string }[]; conflicts: { category: string; relPath: string; reason: string }[] };
}

const MIRROR_PATH = 'f/SKILL.md';
const MERGE_PATH = 'settings.json';

const matrix: MatrixCase[] = [
  // ---- pull：remote 全量覆盖 ----
  { name: 'pull · remote-only → write', mode: 'pull', mirror: { remote: 'r' },
    expected: { writes: [{ category: 'skills', relPath: MIRROR_PATH }], conflicts: [] } },
  { name: 'pull · both-exist-differ → write remote（不冲突）', mode: 'pull', mirror: { local: 'l', remote: 'r' },
    expected: { writes: [{ category: 'skills', relPath: MIRROR_PATH }], conflicts: [] } },
  { name: 'pull · both-exist-same → write remote（幂等覆盖）', mode: 'pull', mirror: { local: 'l', remote: 'l' },
    expected: { writes: [{ category: 'skills', relPath: MIRROR_PATH }], conflicts: [] } },
  { name: 'pull · local-only → 零动作', mode: 'pull', mirror: { local: 'l' },
    expected: { writes: [], conflicts: [] } },
  { name: 'pull · 两侧都无 → 零动作', mode: 'pull',
    expected: { writes: [], conflicts: [] } },

  // ---- merge：mirror 分类（文件级） ----
  { name: 'merge · mirror remote-only → write', mode: 'merge', mirror: { remote: 'r' },
    expected: { writes: [{ category: 'skills', relPath: MIRROR_PATH }], conflicts: [] } },
  { name: 'merge · mirror both-exist-differ → file 级 conflict', mode: 'merge', mirror: { local: 'l', remote: 'r' },
    expected: { writes: [], conflicts: [{ category: 'skills', relPath: MIRROR_PATH, reason: 'modify-vs-modify' }] } },
  { name: 'merge · mirror both-exist-same → 零动作', mode: 'merge', mirror: { local: 'l', remote: 'l' },
    expected: { writes: [], conflicts: [] } },
  { name: 'merge · mirror local-only → 零动作（保留本地）', mode: 'merge', mirror: { local: 'l' },
    expected: { writes: [], conflicts: [] } },

  // ---- merge：merge 分类（键级） ----
  { name: 'merge · mergeJSON remote-only → write', mode: 'merge', mergeJson: { remote: '{"b":2}' },
    expected: { writes: [{ category: 'settings', relPath: MERGE_PATH }], conflicts: [] } },
  { name: 'merge · mergeJSON disjoint keys → 并集 write', mode: 'merge', mergeJson: { local: '{"a":1}', remote: '{"b":2}' },
    expected: { writes: [{ category: 'settings', relPath: MERGE_PATH }], conflicts: [] } },
  { name: 'merge · mergeJSON same key differs → 键级 conflict', mode: 'merge', mergeJson: { local: '{"a":1}', remote: '{"a":2}' },
    expected: { writes: [], conflicts: [{ category: 'settings', relPath: MERGE_PATH, reason: 'merge-keys' }] } },
  { name: 'merge · mergeJSON identical → 零动作', mode: 'merge', mergeJson: { local: '{"a":1}', remote: '{"a":1}' },
    expected: { writes: [], conflicts: [] } },
  { name: 'merge · mergeJSON local-only → 零动作（保留本地）', mode: 'merge', mergeJson: { local: '{"a":1}' },
    expected: { writes: [], conflicts: [] } },

  // ---- skip：恒空 ----
  { name: 'skip · 任意状态 → 零动作', mode: 'skip', mirror: { local: 'l', remote: 'r' }, mergeJson: { local: '{"a":1}', remote: '{"a":2}' },
    expected: { writes: [], conflicts: [] } },
];

describe('planFirstContact — 三模式语义矩阵（表驱动）', () => {
  const config = configOf();

  it.each(matrix)('$name', ({ mode, mirror, mergeJson, expected }) => {
    const build = (side: 'local' | 'remote') => snapshot({
      skills: mirror?.[side] === undefined ? {} : { [MIRROR_PATH]: file(mirror[side]!) },
      settings: mergeJson?.[side] === undefined ? {} : { [MERGE_PATH]: jsonText(mergeJson[side]!) },
    });

    const plan = planFirstContact(config, build('local'), build('remote'), mode);

    expect(plan.mode).toBe(mode);
    expect(writes(plan.actions).map(({ category, relPath }) => ({ category, relPath })))
      .toEqual(expected.writes);
    expect(conflictsOf(plan.actions).map(({ category, relPath, reason }) => ({ category, relPath, reason })))
      .toEqual(expected.conflicts);
    expect(plan.actions.some((action) => action.type === 'delete')).toBe(false);
  });
});

/* ------------------------------------------------------------------ */
/* 局部断言工具                                                        */
/* ------------------------------------------------------------------ */

function writeContent(actions: readonly PullAction[]): string {
  return writes(actions).map((action) => action.content).join('\n');
}
