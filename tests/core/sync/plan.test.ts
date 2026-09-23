/**
 * W4 测试矩阵（docs/m2-plan.md §3-P1-W4 验收）。
 *
 * 纯函数测试：零 fs、零 HOMER_HOME —— 全部通过手工构造的三方快照驱动。
 * 分组：
 *   A. mirror 三类动作映射（pull / pull-delete / conflict；push 类无动作）
 *   B. merge 分类：无冲突写回 / 整文件 conflict / 两种删除情形 / degraded 降级 / 无动作
 *   C. excludeKeys 矩阵四条（占位符替换 / local 键植回 / 远端占位符不流入 / local 缺失不植回）
 *   D. checkPushSafety 三分支 + changedFiles 推导
 */

import { describe, expect, it } from 'vitest';

import { checkPushSafety, planPull } from '../../../src/core/sync/plan.js';
import {
  applyExcludeKeyPlaceholders,
  REQUIRED_PLACEHOLDER,
  stripSnapshotExcludeKeys,
} from '../../../src/core/sync/excluded-keys.js';
import { computeDrift } from '../../../src/core/engine/index.js';
import type { PullAction, PullConflictAction, PullPlan } from '../../../src/core/sync/types.js';
import type {
  AdapterConfig,
  AdapterSnapshot,
  CategoryConfig,
  CategorySnapshot,
  HomerConfig,
  SnapshotEntry,
  SnapshotFiles,
  SyncMode,
} from '../../../src/core/types.js';

/* ------------------------------------------------------------------ */
/* 构造工具                                                            */
/* ------------------------------------------------------------------ */

const json = (value: unknown): SnapshotEntry => ({ kind: 'json', content: JSON.stringify(value) });
const jsonText = (content: string): SnapshotEntry => ({ kind: 'json', content });
const file = (content: string): SnapshotEntry => ({ kind: 'file', content });

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

function configOf(
  categories: Record<string, CategoryConfig>,
  adapterId = 'pi',
  adapterExtra: Partial<AdapterConfig> = {},
): HomerConfig {
  return {
    version: 1,
    adapters: { [adapterId]: { root: '/tmp/pi', categories, ...adapterExtra } },
  };
}

const mirrorCfg = (excludeKeys?: string[]) =>
  configOf({ skills: { paths: ['skills/'], mode: 'mirror', ...(excludeKeys ? { excludeKeys } : {}) } });

const mergeCfg = (excludeKeys?: string[]) =>
  configOf({
    settings: { paths: ['settings.json'], mode: 'merge', ...(excludeKeys ? { excludeKeys } : {}) },
  });

const actions = (plan: PullPlan): PullAction[] => plan.actions;
const only = (plan: PullPlan): PullAction => {
  expect(plan.actions).toHaveLength(1);
  return plan.actions[0]!;
};
const onlyConflict = (plan: PullPlan): PullConflictAction => {
  const action = only(plan);
  expect(action.type).toBe('conflict');
  return action as PullConflictAction;
};

/* ================================================================== */
/* A. mirror 三类动作映射                                              */
/* ================================================================== */

describe('planPull — mirror 分类动作映射', () => {
  const base = [adapter('pi', category('pi', 'skills', 'mirror', { 'foo/SKILL.md': file('v1') }))];

  it('remote 单边改 → write（内容 = remote 原文）', () => {
    const local = [adapter('pi', category('pi', 'skills', 'mirror', { 'foo/SKILL.md': file('v1') }))];
    const remote = [adapter('pi', category('pi', 'skills', 'mirror', { 'foo/SKILL.md': file('v2') }))];

    expect(actions(planPull(mirrorCfg(), base, local, remote))).toEqual([
      { type: 'write', adapterId: 'pi', category: 'skills', relPath: 'foo/SKILL.md', content: 'v2' },
    ]);
  });

  it('remote 删除 + local 未改 → delete', () => {
    const local = [adapter('pi', category('pi', 'skills', 'mirror', { 'foo/SKILL.md': file('v1') }))];
    const remote = [adapter('pi', category('pi', 'skills', 'mirror', {}))];

    expect(actions(planPull(mirrorCfg(), base, local, remote))).toEqual([
      { type: 'delete', adapterId: 'pi', category: 'skills', relPath: 'foo/SKILL.md' },
    ]);
  });

  it('双方都改（内容不同）→ conflict，带 local/remote 全量内容', () => {
    const local = [adapter('pi', category('pi', 'skills', 'mirror', { 'foo/SKILL.md': file('local') }))];
    const remote = [adapter('pi', category('pi', 'skills', 'mirror', { 'foo/SKILL.md': file('remote') }))];

    expect(onlyConflict(planPull(mirrorCfg(), base, local, remote))).toEqual({
      type: 'conflict',
      adapterId: 'pi',
      category: 'skills',
      relPath: 'foo/SKILL.md',
      reason: 'modify-vs-modify',
      localContent: 'local',
      remoteContent: 'remote',
    });
  });

  it('local 改 + remote 删 → conflict local-modify-vs-remote-delete', () => {
    const local = [adapter('pi', category('pi', 'skills', 'mirror', { 'foo/SKILL.md': file('local') }))];
    const remote = [adapter('pi', category('pi', 'skills', 'mirror', {}))];

    const action = onlyConflict(planPull(mirrorCfg(), base, local, remote));
    expect(action.reason).toBe('local-modify-vs-remote-delete');
    expect(action.localContent).toBe('local');
    expect(action.remoteContent).toBeUndefined();
  });

  it('local 删 + remote 改 → conflict local-delete-vs-remote-modify', () => {
    const local = [adapter('pi', category('pi', 'skills', 'mirror', {}))];
    const remote = [adapter('pi', category('pi', 'skills', 'mirror', { 'foo/SKILL.md': file('remote') }))];

    const action = onlyConflict(planPull(mirrorCfg(), base, local, remote));
    expect(action.reason).toBe('local-delete-vs-remote-modify');
    expect(action.localContent).toBeUndefined();
    expect(action.remoteContent).toBe('remote');
  });

  it('push / push-delete / noop 一律不产生动作', () => {
    const local = [
      adapter('pi', category('pi', 'skills', 'mirror', {
        'foo/SKILL.md': file('v1'),          // noop（都没动）
        'bar/SKILL.md': file('local-only'),  // push（local 新增）
        'baz/SKILL.md': file('changed'),     // push（local 改）
      })),
    ];
    const remote = [adapter('pi', category('pi', 'skills', 'mirror', {
      'foo/SKILL.md': file('v1'),
      'baz/SKILL.md': file('v1'),
    }))];
    const baseWithBaz = [
      adapter('pi', category('pi', 'skills', 'mirror', {
        'foo/SKILL.md': file('v1'),
        'baz/SKILL.md': file('v1'),
      })),
    ];

    expect(actions(planPull(mirrorCfg(), baseWithBaz, local, remote))).toEqual([]);
  });

  it('远端新增文件（base 缺失，仅 remote 有）→ write', () => {
    const local = [adapter('pi', category('pi', 'skills', 'mirror', {}))];
    const remote = [adapter('pi', category('pi', 'skills', 'mirror', { 'new/SKILL.md': file('remote') }))];

    expect(actions(planPull(mirrorCfg(), [], local, remote))).toEqual([
      { type: 'write', adapterId: 'pi', category: 'skills', relPath: 'new/SKILL.md', content: 'remote' },
    ]);
  });
});

/* ================================================================== */
/* B. merge 分类                                                       */
/* ================================================================== */

describe('planPull — merge 分类', () => {
  const settings = (entries: Record<string, SnapshotEntry>) => [adapter('pi', category('pi', 'settings', 'merge', entries))];

  it('无冲突合并 → write，内容为确定性 JSON（2 空格缩进 + 尾换行）', () => {
    const base = settings({ 'settings.json': json({ theme: 'light', keep: 1 }) });
    const local = settings({ 'settings.json': json({ theme: 'light', keep: 1, localOnly: true }) });
    const remote = settings({ 'settings.json': json({ theme: 'dark', keep: 1 }) });

    const action = only(planPull(mergeCfg(), base, local, remote));
    expect(action.type).toBe('write');
    if (action.type !== 'write') throw new Error('unreachable');

    // local 的 localOnly 保留 + 远端 theme 合入
    expect(JSON.parse(action.content)).toEqual({ theme: 'dark', keep: 1, localOnly: true });
    expect(action.content).toBe(`${JSON.stringify({ theme: 'dark', keep: 1, localOnly: true }, null, 2)}\n`);
  });

  it('任一键冲突 → 整文件 conflict（reason=merge-keys，keyPaths 正确）', () => {
    const base = settings({ 'settings.json': json({ a: 1, b: 2 }) });
    const local = settings({ 'settings.json': json({ a: 9, b: 2 }) });
    const remote = settings({ 'settings.json': json({ a: 5, b: 2 }) });

    const action = onlyConflict(planPull(mergeCfg(), base, local, remote));
    expect(action.reason).toBe('merge-keys');
    expect(action.keyPaths).toEqual(['a']);
  });

  it('多个冲突键 → keyPaths 全量列出', () => {
    const base = settings({ 'settings.json': json({ a: 1, b: 1, c: 1 }) });
    const local = settings({ 'settings.json': json({ a: 2, b: 2, c: 1 }) });
    const remote = settings({ 'settings.json': json({ a: 3, b: 3, c: 1 }) });

    const action = onlyConflict(planPull(mergeCfg(), base, local, remote));
    expect(action.keyPaths).toEqual(['a', 'b']);
  });

  it('remote 删文件 + local 未改 → delete', () => {
    const base = settings({ 'settings.json': json({ a: 1 }) });
    const local = settings({ 'settings.json': json({ a: 1 }) });
    const remote = settings({});

    expect(actions(planPull(mergeCfg(), base, local, remote))).toEqual([
      { type: 'delete', adapterId: 'pi', category: 'settings', relPath: 'settings.json' },
    ]);
  });

  it('remote 删文件 + local 改过 → conflict local-modify-vs-remote-delete', () => {
    const base = settings({ 'settings.json': json({ a: 1 }) });
    const local = settings({ 'settings.json': json({ a: 2 }) });
    const remote = settings({});

    const action = onlyConflict(planPull(mergeCfg(), base, local, remote));
    expect(action.reason).toBe('local-modify-vs-remote-delete');
    expect(action.localContent).toBe(json({ a: 2 }).content);
  });

  it('local 删文件 + remote 改过 → conflict local-delete-vs-remote-modify', () => {
    const base = settings({ 'settings.json': json({ a: 1 }) });
    const local = settings({});
    const remote = settings({ 'settings.json': json({ a: 3 }) });

    const action = onlyConflict(planPull(mergeCfg(), base, local, remote));
    expect(action.reason).toBe('local-delete-vs-remote-modify');
    expect(action.remoteContent).toBe(json({ a: 3 }).content);
  });

  it('degraded（kind=file）→ 降级 mirror 语义：remote 改 → write 原文', () => {
    const base = settings({ 'settings.json': file('not json base') });
    const local = settings({ 'settings.json': file('not json base') });
    const remote = settings({ 'settings.json': file('not json remote') });

    expect(actions(planPull(mergeCfg(), base, local, remote))).toEqual([
      { type: 'write', adapterId: 'pi', category: 'settings', relPath: 'settings.json', content: 'not json remote' },
    ]);
  });

  it('degraded（JSON 损坏但 kind=json）→ 降级 mirror 语义：双方都改 → conflict', () => {
    const base = settings({ 'settings.json': jsonText('{ "a": 1') });
    const local = settings({ 'settings.json': jsonText('{ "a": 2') });
    const remote = settings({ 'settings.json': jsonText('{ "a": 3') });

    const action = onlyConflict(planPull(mergeCfg(), base, local, remote));
    expect(action.reason).toBe('modify-vs-modify');
  });

  it('remote 相对 base 未变动 → 不产生任何动作（local 的增删改都是 push 方向）', () => {
    const base = settings({ 'settings.json': json({ a: 1 }) });
    const local = settings({ 'settings.json': json({ a: 2, b: 3 }) });
    const remote = settings({ 'settings.json': json({ a: 1 }) });

    expect(actions(planPull(mergeCfg(), base, local, remote))).toEqual([]);
  });

  it('base 缺失（首次对接）：仅 remote 有 → write；双改不同 → conflict', () => {
    const local = settings({});
    const remote = settings({ 'settings.json': json({ a: 1 }) });
    expect(actions(planPull(mergeCfg(), [], local, remote))).toEqual([
      { type: 'write', adapterId: 'pi', category: 'settings', relPath: 'settings.json', content: json({ a: 1 }).content },
    ]);

    const local2 = settings({ 'settings.json': json({ a: 2 }) });
    const remote2 = settings({ 'settings.json': json({ a: 3 }) });
    const action = onlyConflict(planPull(mergeCfg(), [], local2, remote2));
    expect(action.reason).toBe('modify-vs-modify');
  });

  it('merged 语义等于 local（无实质变化）→ 不产生 write（避免无意义重写）', () => {
    const base = settings({ 'settings.json': json({ a: 1, b: 1 }) });
    const local = settings({ 'settings.json': json({ a: 1, b: 2 }) });   // local 改了 b
    const remote = settings({ 'settings.json': json({ a: 1, b: 2 }) });  // remote 改成同一个值

    expect(actions(planPull(mergeCfg(), base, local, remote))).toEqual([]);
  });

  it('root 值为数组的 merge 文件 → 降级 mirror（绝不把 mergeJson 的 \'{}\' 写回用户文件）', () => {
    const base = settings({ 'settings.json': jsonText('["a","b"]') });
    const local = settings({ 'settings.json': jsonText('["a","b"]') });
    const remote = settings({ 'settings.json': jsonText('["a","b","c"]') });

    // mirror 语义：仅 remote 改 → write 远端**原文**（不是 {}）
    expect(actions(planPull(mergeCfg(), base, local, remote))).toEqual([
      { type: 'write', adapterId: 'pi', category: 'settings', relPath: 'settings.json', content: '["a","b","c"]' },
    ]);
  });
});

/* ================================================================== */
/* C. excludeKeys 矩阵                                                 */
/* ================================================================== */

describe('applyExcludeKeyPlaceholders — push 写 store 前替换占位符', () => {
  it('apiKeys → "__REQUIRED__"，其余键原样保留，且不改动入参快照', () => {
    const snapshot = adapter('pi', category('pi', 'models', 'merge', {
      'models.json': json({ apiKeys: 'sk-secret-value', provider: 'anthropic', nested: { keep: 1 } }),
    }));
    const config = configOf({ models: { paths: ['models.json'], mode: 'merge', excludeKeys: ['apiKeys'] } });

    const out = applyExcludeKeyPlaceholders(snapshot, config);
    const entry = out.categories[0]!.files.get('models.json')!;

    expect(JSON.parse(entry.content)).toEqual({
      apiKeys: REQUIRED_PLACEHOLDER,
      provider: 'anthropic',
      nested: { keep: 1 },
    });
    expect(entry.content).toBe(`${JSON.stringify({ apiKeys: '__REQUIRED__', provider: 'anthropic', nested: { keep: 1 } }, null, 2)}\n`);
    // 不可变：原快照未被污染
    expect(JSON.parse(snapshot.categories[0]!.files.get('models.json')!.content)).toEqual({
      apiKeys: 'sk-secret-value',
      provider: 'anthropic',
      nested: { keep: 1 },
    });
  });

  it('excluded 键不存在于文件 → 条目字节不变（不凭空造键）', () => {
    const snapshot = adapter('pi', category('pi', 'models', 'merge', {
      'models.json': json({ provider: 'anthropic' }),
    }));
    const config = configOf({ models: { paths: ['models.json'], mode: 'merge', excludeKeys: ['apiKeys'] } });
    const original = snapshot.categories[0]!.files.get('models.json')!.content;

    expect(applyExcludeKeyPlaceholders(snapshot, config).categories[0]!.files.get('models.json')!.content)
      .toBe(original);
  });

  it('非 JSON 条目 / 未配置 excludeKeys 的分类 → 原样透传', () => {
    const snapshot = adapter('pi', category('pi', 'skills', 'mirror', { 'SKILL.md': file('# x') }));
    const config = mirrorCfg();

    const out = applyExcludeKeyPlaceholders(snapshot, config);
    expect(out.categories[0]!.files.get('SKILL.md')).toEqual({ kind: 'file', content: '# x' });
  });
});

describe('planPull — excludeKeys 三重语义', () => {
  const modelsCfg = () => configOf({
    models: { paths: ['models.json'], mode: 'merge', excludeKeys: ['apiKeys'] },
  });
  const models = (content: string) =>
    [adapter('pi', category('pi', 'models', 'merge', { 'models.json': jsonText(content) }))];

  const base = models(JSON.stringify({ apiKeys: '__REQUIRED__', provider: 'anthropic', model: 'x' }));

  it('远端改了别的键 → write 结果里 local 的 apiKeys 真值被植回', () => {
    const local = models(JSON.stringify({ apiKeys: 'sk-local-real', provider: 'anthropic', model: 'x' }));
    const remote = models(JSON.stringify({ apiKeys: '__REQUIRED__', provider: 'anthropic', model: 'y' }));

    const action = only(planPull(modelsCfg(), base, local, remote));
    expect(action.type).toBe('write');
    if (action.type !== 'write') throw new Error('unreachable');

    expect(JSON.parse(action.content)).toEqual({
      apiKeys: 'sk-local-real',
      provider: 'anthropic',
      model: 'y',
    });
    expect(action.content).not.toContain(REQUIRED_PLACEHOLDER);
  });

  it('远端 __REQUIRED__ 永不流入结果（即使远端把该键改成了别的值）', () => {
    const baseWithSecret = models(JSON.stringify({ apiKeys: '__REQUIRED__', model: 'x' }));
    const local = models(JSON.stringify({ apiKeys: 'sk-local-real', model: 'x' }));
    const remote = models(JSON.stringify({ apiKeys: '__REQUIRED__', model: 'y' }));

    const action = only(planPull(modelsCfg(), baseWithSecret, local, remote));
    if (action.type !== 'write') throw new Error('expected write');
    expect(JSON.parse(action.content)).toEqual({ apiKeys: 'sk-local-real', model: 'y' });
  });

  it('远端把 apiKeys 换成了真实密钥 → 被剥离，不流入结果且不触发 remote-ahead', () => {
    const local = models(JSON.stringify({ apiKeys: 'sk-local-real', model: 'x' }));
    const remote = models(JSON.stringify({ apiKeys: 'sk-remote-other', model: 'x' }));

    // 剥离后三方 model 都是 'x' → 无动作
    expect(actions(planPull(modelsCfg(), base, local, remote))).toEqual([]);
    // 与 checkPushSafety 同口径：仅 excluded 键不同 → 可推
    expect(checkPushSafety(modelsCfg(), base, local, remote).status).toBe('ok');
  });

  it('local 缺失该 excluded 键 → 不植回（缺失必填项交由命令层 warning）', () => {
    const local = models(JSON.stringify({ model: 'x' }));
    const remote = models(JSON.stringify({ apiKeys: '__REQUIRED__', model: 'y' }));

    const action = only(planPull(modelsCfg(), base, local, remote));
    if (action.type !== 'write') throw new Error('expected write');
    expect(JSON.parse(action.content)).toEqual({ model: 'y' });
    expect(Object.keys(JSON.parse(action.content) as object)).not.toContain('apiKeys');
  });

  it('远端新增文件（base / local 均无该条目）时 excluded 键同样被剥离，不流入 write 内容', () => {
    const local = models(JSON.stringify({ provider: 'anthropic', model: 'x' }));
    const remote = models(JSON.stringify({ apiKeys: '__REQUIRED__', provider: 'anthropic', model: 'y' }));

    // local 完全没有该文件条目（用空 files 的分类表达）→ 仅 remote 有 → write
    const empty = [adapter('pi', category('pi', 'models', 'merge', {}))];
    const action = only(planPull(modelsCfg(), [], empty, remote));
    if (action.type !== 'write') throw new Error('expected write');
    expect(action.content).not.toContain(REQUIRED_PLACEHOLDER);

    // 对照：local 有该文件且内容不同 → 冲突（展示的 localContent 是**原始**文件，未被剥离）
    const conflict = onlyConflict(planPull(modelsCfg(), [], local, remote));
    expect(conflict.reason).toBe('modify-vs-modify');
    expect(conflict.localContent).toBe(json({ provider: 'anthropic', model: 'x' }).content);
  });

  it('整文件 conflict（merge-keys）只给 keyPaths：不带任何整文件内容（避免真实密钥 / 占位符外流）', () => {
    const local = models(JSON.stringify({ apiKeys: 'sk-local-real', model: 'l' }));
    const remote = models(JSON.stringify({ apiKeys: '__REQUIRED__', model: 'r' }));

    const action = onlyConflict(planPull(modelsCfg(), base, local, remote));
    expect(action.reason).toBe('merge-keys');
    expect(action.keyPaths).toEqual(['model']);
    expect(action.localContent).toBeUndefined();
    expect(action.remoteContent).toBeUndefined();
  });

  it('全量计划里 __REQUIRED__ 占位符永不出现；真实密钥仅出现在 write 内容（落盘目标）', () => {
    const baseWithSecret = models(JSON.stringify({ apiKeys: '__REQUIRED__', model: 'x' }));
    const local = models(JSON.stringify({ apiKeys: 'sk-local-real', model: 'x' }));
    const remote = models(JSON.stringify({ apiKeys: '__REQUIRED__', model: 'y' }));

    const plan = planPull(modelsCfg(), baseWithSecret, local, remote);
    const action = only(plan);
    if (action.type !== 'write') throw new Error('expected write');

    // 不变量一：占位符不出现在计划的任何角落
    expect(JSON.stringify(plan)).not.toContain(REQUIRED_PLACEHOLDER);
    // 不变量二：write 内容里只有**本地自己的**密钥值（本地永不被远端覆盖）
    expect(JSON.parse(action.content)).toEqual({ apiKeys: 'sk-local-real', model: 'y' });
  });

  it('stripSnapshotExcludeKeys 与 status 同口径（剥离后 drift 不再报远端变更）', () => {
    const local = models(JSON.stringify({ apiKeys: 'sk-local-real', provider: 'anthropic', model: 'x' }));
    const remote = models(JSON.stringify({ apiKeys: 'sk-remote-other', provider: 'anthropic', model: 'x' }));
    const cfg = modelsCfg();

    const drift = computeDrift(
      base.map((s) => stripSnapshotExcludeKeys(s, cfg)),
      local.map((s) => stripSnapshotExcludeKeys(s, cfg)),
      remote.map((s) => stripSnapshotExcludeKeys(s, cfg)),
    );
    expect(drift[0]!.categories[0]!).toMatchObject({ push: 0, pull: 0, conflicts: 0 });
  });
});

/* ================================================================== */
/* D. checkPushSafety                                                  */
/* ================================================================== */

describe('checkPushSafety — 三分支 + changedFiles 推导', () => {
  it('remote 单边改动 → remote-ahead（优先于 conflicts），列出远端会覆盖的文件', () => {
    const base = [adapter('pi', category('pi', 'skills', 'mirror', { 'a.md': file('v1') }))];
    const local = [
      adapter('pi', category('pi', 'skills', 'mirror', { 'a.md': file('v1'), 'b.md': file('local') })),
    ];
    const remote = [
      adapter('pi', category('pi', 'skills', 'mirror', { 'a.md': file('v2'), 'c.md': file('remote') })),
    ];

    const check = checkPushSafety(mirrorCfg(), base, local, remote);
    expect(check.status).toBe('remote-ahead');
    expect(check.remoteAheadFiles).toEqual([
      { adapterId: 'pi', category: 'skills', relPath: 'a.md' },
      { adapterId: 'pi', category: 'skills', relPath: 'c.md' },
    ]);
    expect(check.changedFiles).toEqual([]);
    expect(check.conflictItems).toEqual([]);
  });

  it('双方都改同一文件 → conflicts，conflictItems 带 keyPaths / 内容', () => {
    const base = [adapter('pi', category('pi', 'skills', 'mirror', { 'a.md': file('v1') }))];
    const local = [adapter('pi', category('pi', 'skills', 'mirror', { 'a.md': file('local') }))];
    const remote = [adapter('pi', category('pi', 'skills', 'mirror', { 'a.md': file('remote') }))];

    const check = checkPushSafety(mirrorCfg(), base, local, remote);
    expect(check.status).toBe('conflicts');
    expect(check.conflictItems).toHaveLength(1);
    expect(check.conflictItems[0]).toMatchObject({
      adapterId: 'pi', category: 'skills', relPath: 'a.md', reason: 'modify-vs-modify',
    });
    expect(check.changedFiles).toEqual([]);
  });

  it('merge 分类键冲突 → conflicts（keyPaths 透出）', () => {
    const base = [adapter('pi', category('pi', 'settings', 'merge', { 'settings.json': json({ a: 1 }) }))];
    const local = [adapter('pi', category('pi', 'settings', 'merge', { 'settings.json': json({ a: 2 }) }))];
    const remote = [adapter('pi', category('pi', 'settings', 'merge', { 'settings.json': json({ a: 3 }) }))];

    const check = checkPushSafety(mergeCfg(), base, local, remote);
    expect(check.status).toBe('conflicts');
    expect(check.conflictItems[0]!.keyPaths).toEqual(['a']);
  });

  it('无远端未拉变更 → ok，changedFiles = mirror push ops + merge changedKeys 的文件集合', () => {
    const base = [
      adapter('pi',
        category('pi', 'skills', 'mirror', { 'keep.md': file('same'), 'gone.md': file('v1') }),
        category('pi', 'settings', 'merge', { 'settings.json': json({ a: 1, b: 1 }) }),
      ),
    ];
    const local = [
      adapter('pi',
        category('pi', 'skills', 'mirror', {
          'keep.md': file('same'),   // noop
          'new.md': file('added'),   // push
        }),
        category('pi', 'settings', 'merge', { 'settings.json': json({ a: 2, b: 1 }) }),
      ),
    ];
    const remote = base;
    const config = configOf({
      skills: { paths: ['skills/'], mode: 'mirror' },
      settings: { paths: ['settings.json'], mode: 'merge' },
    });

    const check = checkPushSafety(config, base, local, remote);
    expect(check.status).toBe('ok');
    expect(check.changedFiles).toEqual([
      { adapterId: 'pi', category: 'skills', relPath: 'gone.md' },
      { adapterId: 'pi', category: 'skills', relPath: 'new.md' },
      { adapterId: 'pi', category: 'settings', relPath: 'settings.json' },
    ]);
    expect(check.conflictItems).toEqual([]);
    expect(check.remoteAheadFiles).toEqual([]);
  });

  it('无任何变更 → ok 且 changedFiles 为空', () => {
    const snap = [
      adapter('pi',
        category('pi', 'skills', 'mirror', { 'a.md': file('v1') }),
        category('pi', 'settings', 'merge', { 'settings.json': json({ a: 1 }) }),
      ),
    ];
    const config = configOf({
      skills: { paths: ['skills/'], mode: 'mirror' },
      settings: { paths: ['settings.json'], mode: 'merge' },
    });

    const check = checkPushSafety(config, snap, snap, snap);
    expect(check).toEqual({
      status: 'ok', changedFiles: [], conflictItems: [], remoteAheadFiles: [],
    });
  });

  it('仅 excludeKeys 键不同（store 占位符 vs 本地真值）→ ok，不误报 remote-ahead', () => {
    const base = [
      adapter('pi', category('pi', 'models', 'merge', {
        'models.json': json({ apiKeys: '__REQUIRED__', model: 'x' }),
      })),
    ];
    const local = [
      adapter('pi', category('pi', 'models', 'merge', {
        'models.json': json({ apiKeys: 'sk-local-real', model: 'x' }),
      })),
    ];
    const remote = base;
    const config = configOf({ models: { paths: ['models.json'], mode: 'merge', excludeKeys: ['apiKeys'] } });

    const check = checkPushSafety(config, base, local, remote);
    expect(check.status).toBe('ok');
    expect(check.changedFiles).toEqual([]);
  });

  it('remote-ahead 的 remoteAheadFiles 与 planPull 的 write/delete 动作一致', () => {
    const base = [adapter('pi', category('pi', 'skills', 'mirror', {
      'a.md': file('v1'), 'del.md': file('v1'),
    }))];
    const local = [adapter('pi', category('pi', 'skills', 'mirror', {
      'a.md': file('v1'), 'del.md': file('v1'),
    }))];
    const remote = [adapter('pi', category('pi', 'skills', 'mirror', { 'a.md': file('v2') }))];

    const check = checkPushSafety(mirrorCfg(), base, local, remote);
    const plan = planPull(mirrorCfg(), base, local, remote);

    expect(check.status).toBe('remote-ahead');
    expect(check.remoteAheadFiles).toEqual(
      actions(plan).map((a) => ({ adapterId: a.adapterId, category: a.category, relPath: a.relPath })),
    );
  });

  it('pull 计划与 status 口径自洽：drift 报的 pull/conflicts 都有对应动作', () => {
    const base = [adapter('pi', category('pi', 'skills', 'mirror', {
      'a.md': file('v1'), 'b.md': file('v1'),
    }))];
    const local = [adapter('pi', category('pi', 'skills', 'mirror', {
      'a.md': file('v1'), 'b.md': file('local'),
    }))];
    const remote = [adapter('pi', category('pi', 'skills', 'mirror', {
      'a.md': file('v2'), 'b.md': file('remote'),
    }))];
    const config = mirrorCfg();

    const drift = computeDrift(base, local, remote)[0]!.categories[0]!;
    const plan = planPull(config, base, local, remote);

    expect(drift.pull).toBe(1);       // a.md
    expect(drift.conflicts).toBe(1);  // b.md
    expect(actions(plan)).toHaveLength(drift.pull + drift.conflicts);
  });
});
