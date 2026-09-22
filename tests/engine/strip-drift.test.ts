/**
 * 查证：rev-correctness 报了 `stripExcludeKeys` 的 `JSON.stringify` 会「重排数字键 / 消灭格式」
 * 从而在 `drift.ts` 的 `l.content !== r.content` 处造成**假漂移**。
 *
 * 结论（见 docs/m1-report.md 第 3.x 节）：**按报告描述不可复现**，用测试钉住实际行为 + 残留边界。
 *
 * 事实（本文件逐条钉住）：
 *   1. `stripExcludeKeys` 在 CLI 采集层对 **base / local 对称**调用（render.ts），
 *      且 remote 缺省 = base（已是剥离后的），因此三方比较用的都是同一套规范化文本；
 *   2. `JSON.stringify` 的行为恰好相反于报告担忧：它**统一**了数字键顺序（整数键升序）
 *      并**消灭**了缩进/空格差异 —— 所以它**减少**假漂移，而不是制造；
 *   3. 真实残留边界：`l.content !== r.content` 是**字节级**比较，语义相等但
 *      **键插入顺序**不同的 JSON 仍可能被判冲突（且该路径仅当显式注入 remote 时可达；
 *      M1 生产路径 remote 缺省 = base ⟹ base 缺失时 remote 也缺失，提前 return）。
 *      此边界 **pre-existing**：不调用 strip 同样复现，源于 plan §1.3 冻结的「内容相等 = 字符串全等」；
 *      故本文件**钉住现状**，不改冻结语义（是否引入 canonical 序列化留待 M2 决策）。
 */

import { describe, expect, it } from 'vitest';

import { computeDrift, stripExcludeKeys } from '../../src/core/engine/index.js';
import type { AdapterSnapshot, CategorySnapshot, SnapshotEntry } from '../../src/core/types.js';

const json = (content: string): SnapshotEntry => ({ kind: 'json', content });

/** 单文件 merge 分类快照。f.json 不存在 = base/local 缺失。 */
function snapshot(files: Record<string, string>): AdapterSnapshot {
  const entries = new Map<string, SnapshotEntry>(Object.entries(files).map(([k, v]) => [k, json(v)]));
  return {
    adapterId: 'pi',
    categories: [{ adapterId: 'pi', category: 'models', mode: 'merge', files: entries }],
  };
}

function categoryOf(snapshot: AdapterSnapshot): CategorySnapshot {
  const category = snapshot.categories[0];
  if (category === undefined) throw new Error('no category');
  return category;
}

const EXCLUDE = { models: ['apiKeys'] };

function driftOf(base: AdapterSnapshot, local: AdapterSnapshot, remote?: AdapterSnapshot[]) {
  const strip = (s: AdapterSnapshot): AdapterSnapshot => stripExcludeKeys(s, EXCLUDE);
  const drift = computeDrift([strip(base)], [strip(local)], remote?.map(strip));
  return drift[0]!.categories[0]!;
}

/** 剥离后某文件的文本（用于断言规范化结果）。 */
function strippedContent(snapshot: AdapterSnapshot, relPath: string): string | undefined {
  return stripExcludeKeys(snapshot, EXCLUDE).categories[0]?.files.get(relPath)?.content;
}

describe('查证：stripExcludeKeys 不会制造假漂移（报告描述不可复现）', () => {
  it('JSON.stringify 统一数字键顺序：无 base 也无 local 之外的改动 → 零漂移', () => {
    // 两侧仅数字键书写顺序不同（10 与 2），语义完全相等
    const base = snapshot({ 'f.json': '{"apiKeys":{},"10":1,"2":2}' });
    const local = snapshot({ 'f.json': '{"2":2,"10":1,"apiKeys":{}}' });

    expect(driftOf(base, local)).toMatchObject({ push: 0, pull: 0, conflicts: 0 });
    // 剥离后两侧文本逐字节相同（数字键被统一升序）
    expect(strippedContent(base, 'f.json')).toBe('{"2":2,"10":1}');
    expect(strippedContent(local, 'f.json')).toBe('{"2":2,"10":1}');
  });

  it('JSON.stringify 消灭缩进/空白差异：格式化不同 → 零漂移', () => {
    const base = snapshot({ 'f.json': '{\n  "apiKeys": {},\n  "b": 1\n}\n' });
    const local = snapshot({ 'f.json': '{"apiKeys":{},"b":1}' });

    expect(driftOf(base, local)).toMatchObject({ push: 0, pull: 0, conflicts: 0 });
  });

  it('仅 excludeKeys 命中的键变化 → 剥离后零漂移（strip 的核心职责）', () => {
    const base = snapshot({ 'f.json': '{"apiKeys":{"openai":"sk-a"},"b":1}' });
    const local = snapshot({ 'f.json': '{"apiKeys":{"openai":"sk-CHANGED"},"b":1}' });

    expect(driftOf(base, local)).toMatchObject({ push: 0, pull: 0, conflicts: 0 });
  });

  it('strip 是确定性 / 幂等的（同一输入永远同一输出 → 不会因调用顺序抖动）', () => {
    const source = snapshot({ 'f.json': '{\n "apiKeys": {},\n "b": 1,\n "a": 2\n}\n' });
    const first = strippedContent(source, 'f.json');
    const second = strippedContent(source, 'f.json');
    expect(first).toBe(second);
  });

  it('非整数键顺序不变（strip 不重排非整数键）→ 不引入新差异', () => {
    const source = snapshot({ 'f.json': '{"apiKeys":{},"b":1,"a":2}' });
    // JSON.stringify 保留非整数键的插入顺序
    expect(strippedContent(source, 'f.json')).toBe('{"b":1,"a":2}');
  });

  it('真实改动（非 excludeKeys 键）在 strip 后仍被检出（未因规范化而漏报）', () => {
    const base = snapshot({ 'f.json': '{"apiKeys":{},"10":1,"2":2}' });
    const local = snapshot({ 'f.json': '{"2":2,"10":99,"apiKeys":{}}' });

    const drift = driftOf(base, local);
    expect(drift.push).toBe(1);
    expect(drift.conflicts).toBe(0);
  });
});

describe('查证：残留边界 —— l.content !== r.content 的字节级比较', () => {
  /**
   * 该分支（drift.ts `if (b === undefined) { ... if (l.content !== r.content) }`）
   * 仅当 **显式注入 remote** 且 base 缺失时可达。
   * M1 生产路径 `remote ??= base`：base 缺失 ⟹ remote 也缺失 → 提前 return，不可达。
   */
  it('M1 生产形态（remote 缺省 = base）时不可达：base 缺失 → 无冲突', () => {
    const base = snapshot({});
    const local = snapshot({ 'f.json': '{"apiKeys":{},"b":1,"a":2}' });

    // 不传 remote → remote 缺省 = base（同为剥离后的空）
    // 剥离后 local 相对空 base 是「新增 b / a 两个键」→ push 2（无冲突）
    expect(driftOf(base, local)).toMatchObject({ push: 2, pull: 0, conflicts: 0 });
  });

  it('【钉住现状】base 缺失 + 两侧语义相等但「键插入顺序」不同 → 仍判冲突（pre-existing）', () => {
    const base = snapshot({});
    const local = snapshot({ 'f.json': '{"apiKeys":{},"b":1,"a":2}' });
    const remote = snapshot({ 'f.json': '{"apiKeys":{},"a":2,"b":1}' });

    const drift = driftOf(base, local, [remote]);
    // 语义上两侧相等（该文件在 base 中不存在 → 双方各自新增同值文件）。
    // 但 l.content/r.content 经 strip 后按键插入顺序序列化，字节不同 → conflicts = 1。
    expect(drift.conflicts).toBe(1);
    expect(drift.mergeConflicts).toEqual([{ keyPath: 'f.json', reason: 'both-modified' }]);
  });

  it('【钉住现状】同上但不调用 strip 也复现 → 证明非 strip 引入', () => {
    const base = snapshot({});
    const local = snapshot({ 'f.json': '{"apiKeys":{},"b":1,"a":2}' });
    const remote = snapshot({ 'f.json': '{"apiKeys":{},"a":2,"b":1}' });

    // 完全不做 excludeKeys 剥离，直接进引擎
    const drift = computeDrift([base], [local], [remote])[0]!.categories[0]!;
    expect(drift.conflicts).toBe(1);
  });

  it('【对照】base 缺失 + 两侧字节完全相同 → 不冲突（strip 规范化生效）', () => {
    const base = snapshot({});
    const local = snapshot({ 'f.json': '{\n "apiKeys": {},\n "b": 1\n}\n' });
    // remote 是 local 的同值不同格式写法：strip 把两者归一到同一串 → 不再假冲突
    const remote = snapshot({ 'f.json': '{"apiKeys":{},"b":1}' });

    expect(driftOf(base, local, [remote])).toMatchObject({ push: 1, pull: 0, conflicts: 0 });
  });

  it('【对照】不 strip 时同值不同格式会假冲突 → 证明 strip 减少假漂移', () => {
    const base = snapshot({});
    const local = snapshot({ 'f.json': '{\n "apiKeys": {},\n "b": 1\n}\n' });
    const remote = snapshot({ 'f.json': '{"apiKeys":{},"b":1}' });

    const drift = computeDrift([base], [local], [remote])[0]!.categories[0]!;
    expect(drift.conflicts).toBe(1);
  });

  it('mirror 分类不受 strip 影响（kind 恒为 file，stripFiles 跳过）', () => {
    const mirror: AdapterSnapshot = {
      adapterId: 'pi',
      categories: [
        {
          adapterId: 'pi',
          category: 'models',
          mode: 'mirror',
          files: new Map([['f.json', { kind: 'file', content: '{"apiKeys":{},"b":1}' } as SnapshotEntry]]),
        },
      ],
    };
    // excludeKeys 对 mirror 形同虚设：内容逐字节保留
    expect(categoryOf(stripExcludeKeys(mirror, EXCLUDE)).files.get('f.json')?.content).toBe('{"apiKeys":{},"b":1}');
  });
});
