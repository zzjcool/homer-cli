/**
 * merge.ts 测试矩阵 —— 逐行覆盖 docs/m1-plan.md §1.2 语义表。
 * 表行 → describe 分组一一对应（共 13 组 ≥ 12）。
 */

import { describe, expect, it } from 'vitest';
import { diffJson, mergeJson, stripKeys } from '../../src/core/engine/index.js';

describe('mergeJson — §1.2 语义表逐行', () => {
  it('行1：嵌套对象递归合并（双方各改不同子键）', () => {
    const result = mergeJson(
      { models: { openai: { model: 'gpt-4', temp: 0.2 } } },
      { models: { openai: { model: 'gpt-5', temp: 0.2 } } },
      { models: { openai: { model: 'gpt-4', temp: 0.9 } } },
    );
    expect(result.status).toBe('clean');
    expect(result.merged).toEqual({ models: { openai: { model: 'gpt-5', temp: 0.9 } } });
    expect(result.conflicts).toEqual([]);
  });

  it('行2：数组原子值 — base≡local 时取 remote', () => {
    const result = mergeJson(
      { list: [1, 2] },
      { list: [1, 2] },
      { list: [1, 2, 3] },
    );
    expect(result.status).toBe('clean');
    expect(result.merged).toEqual({ list: [1, 2, 3] });
  });

  it('行2：数组原子值 — base≡remote 时取 local', () => {
    const result = mergeJson(
      { list: [1, 2] },
      { list: ['a'] },
      { list: [1, 2] },
    );
    expect(result.status).toBe('clean');
    expect(result.merged).toEqual({ list: ['a'] });
  });

  it('行2：数组双方同改同值 → 取该值（不冲突）', () => {
    const result = mergeJson(
      { list: [1] },
      { list: [9, 8] },
      { list: [9, 8] },
    );
    expect(result.status).toBe('clean');
    expect(result.merged).toEqual({ list: [9, 8] });
    expect(result.conflicts).toEqual([]);
  });

  it('行2：数组双方改且不同 → 冲突 array-both-changed', () => {
    const result = mergeJson(
      { list: [1] },
      { list: [2] },
      { list: [3] },
    );
    expect(result.status).toBe('conflict');
    expect(result.merged).toEqual({ list: [2] }); // 冲突键暂取 local
    expect(result.conflicts).toEqual([
      { keyPath: 'list', reason: 'array-both-changed', base: [1], local: [2], remote: [3] },
    ]);
  });

  it('行3：local 删键 / remote 未动 → 删除生效', () => {
    const result = mergeJson({ a: 1, b: 2 }, { b: 2 }, { a: 1, b: 2 });
    expect(result.status).toBe('clean');
    expect(result.merged).toEqual({ b: 2 });
  });

  it('行3（对称）：remote 删键 / local 未动 → 删除生效', () => {
    const result = mergeJson({ a: 1, b: 2 }, { a: 1, b: 2 }, { b: 2 });
    expect(result.status).toBe('clean');
    expect(result.merged).toEqual({ b: 2 });
  });

  it('行4：local 删键 / remote 改值 → 冲突 modify-vs-delete（冲突键取 local = 删除）', () => {
    const result = mergeJson({ a: 1 }, {}, { a: 2 });
    expect(result.status).toBe('conflict');
    expect(result.merged).toEqual({});
    expect(result.conflicts).toEqual([
      { keyPath: 'a', reason: 'modify-vs-delete', base: 1, remote: 2 },
    ]);
  });

  it('行4（对称）：remote 删键 / local 改值 → 冲突 modify-vs-delete（取 local 新值）', () => {
    const result = mergeJson({ a: 1 }, { a: 2 }, {});
    expect(result.status).toBe('conflict');
    expect(result.merged).toEqual({ a: 2 });
    expect(result.conflicts).toEqual([
      { keyPath: 'a', reason: 'modify-vs-delete', base: 1, local: 2 },
    ]);
  });

  it('行4 嵌套：删嵌套子键 vs 改同一子键 → 点路径冲突', () => {
    const result = mergeJson(
      { models: { openai: { model: 'a', temp: 1 } } },
      { models: { openai: { temp: 1 } } },
      { models: { openai: { model: 'b', temp: 1 } } },
    );
    expect(result.status).toBe('conflict');
    expect(result.merged).toEqual({ models: { openai: { temp: 1 } } });
    expect(result.conflicts).toEqual([
      { keyPath: 'models.openai.model', reason: 'modify-vs-delete', base: 'a', remote: 'b' },
    ]);
  });

  it('行5：双方改不同键 → 自动合并（含嵌套点路径）', () => {
    const result = mergeJson(
      { a: 1, models: { openai: { model: 'gpt-4', temp: 0.2 } } },
      { a: 2, models: { openai: { model: 'gpt-4', temp: 0.2 } } },
      { a: 1, models: { openai: { model: 'gpt-4o', temp: 0.2 } } },
    );
    expect(result.status).toBe('clean');
    expect(result.merged).toEqual({ a: 2, models: { openai: { model: 'gpt-4o', temp: 0.2 } } });
    expect(result.conflicts).toEqual([]);
  });

  it('行6：双方改同键不同值 → 冲突 both-modified（暂取 local）', () => {
    const result = mergeJson({ theme: 'dark' }, { theme: 'light' }, { theme: 'solar' });
    expect(result.status).toBe('conflict');
    expect(result.merged).toEqual({ theme: 'light' });
    expect(result.conflicts).toEqual([
      { keyPath: 'theme', reason: 'both-modified', base: 'dark', local: 'light', remote: 'solar' },
    ]);
  });

  it('行6 嵌套：双方改同嵌套键不同值 → 点路径冲突', () => {
    const result = mergeJson(
      { models: { openai: { temp: 0.2 } } },
      { models: { openai: { temp: 0.5 } } },
      { models: { openai: { temp: 0.9 } } },
    );
    expect(result.conflicts).toEqual([
      { keyPath: 'models.openai.temp', reason: 'both-modified', base: 0.2, local: 0.5, remote: 0.9 },
    ]);
  });

  it('行7：一方新增键 → 取新增方（local 新增）', () => {
    const result = mergeJson({ a: 1 }, { a: 1, added: true }, { a: 1 });
    expect(result.status).toBe('clean');
    expect(result.merged).toEqual({ a: 1, added: true });
  });

  it('行7（对称）：一方新增键 → 取新增方（remote 新增，含嵌套）', () => {
    const result = mergeJson({ a: 1 }, { a: 1 }, { a: 1, deep: { x: 1 } });
    expect(result.status).toBe('clean');
    expect(result.merged).toEqual({ a: 1, deep: { x: 1 } });
  });

  it('边界：双方同时新增同键（内容不同）→ 冲突 both-modified', () => {
    const result = mergeJson({}, { k: 1 }, { k: 2 });
    expect(result.status).toBe('conflict');
    expect(result.conflicts).toEqual([
      { keyPath: 'k', reason: 'both-modified', local: 1, remote: 2 },
    ]);
  });

  it('边界：双方同时新增同键（内容相同）→ 收敛，无冲突', () => {
    const result = mergeJson({}, { k: 1 }, { k: 1 });
    expect(result.status).toBe('clean');
    expect(result.merged).toEqual({ k: 1 });
  });

  it('边界：三方相同 → clean 且等于原值；未变的嵌套对象不丢键', () => {
    const base = { a: 1, nested: { b: [1, 2], c: 'x' } };
    const result = mergeJson(base, base, base);
    expect(result.status).toBe('clean');
    expect(result.merged).toEqual(base);
  });

  it('行8：merge 内混入 kind:file 条目不进 mergeJson（由 drift 降级，见 drift.test.ts）', () => {
    // mergeJson 只接受已解析值；非对象根值无法表达为 Record → merged 退化为 {}
    // （本函数不感知 kind；降级判定在 drift 层）
    const result = mergeJson(undefined, 'raw text', undefined);
    expect(result.status).toBe('clean');
    expect(result.merged).toEqual({});
  });
});

describe('diffJson — 增 / 改 / 删', () => {
  it('改：同键不同值计 changed + 键路径', () => {
    expect(diffJson({ a: 1, b: 2 }, { a: 3, b: 2 })).toEqual({
      changed: 1, added: 0, deleted: 0, keys: ['a'],
    });
  });

  it('增：local 多出的键计 added', () => {
    expect(diffJson({ a: 1 }, { a: 1, new1: true, new2: { x: 1 } })).toEqual({
      changed: 0, added: 2, deleted: 0, keys: ['new1', 'new2'],
    });
  });

  it('删：local 缺的键计 deleted', () => {
    expect(diffJson({ a: 1, b: 2, c: 3 }, { a: 1 })).toEqual({
      changed: 0, added: 0, deleted: 2, keys: ['b', 'c'],
    });
  });

  it('嵌套：递归到叶子键，路径带点号', () => {
    const diff = diffJson(
      { models: { openai: { temp: 0.2, model: 'a' } } },
      { models: { openai: { temp: 0.9, model: 'a' } } },
    );
    expect(diff).toEqual({ changed: 1, added: 0, deleted: 0, keys: ['models.openai.temp'] });
  });

  it('嵌套子树整体新增/删除只记子树根键', () => {
    const added = diffJson({}, { sub: { a: 1, b: 2 } });
    expect(added).toEqual({ changed: 0, added: 1, deleted: 0, keys: ['sub'] });
    const deleted = diffJson({ sub: { a: 1, b: 2 } }, {});
    expect(deleted).toEqual({ changed: 0, added: 0, deleted: 1, keys: ['sub'] });
  });

  it('数组整体变化记 1 次 changed；无变化全 0', () => {
    expect(diffJson({ l: [1, 2] }, { l: [1, 2, 3] })).toEqual({
      changed: 1, added: 0, deleted: 0, keys: ['l'],
    });
    expect(diffJson({ l: [1] }, { l: [1] })).toEqual({
      changed: 0, added: 0, deleted: 0, keys: [],
    });
  });
});

describe('stripKeys — excludeKeys 顶层剥离', () => {
  it('剥离指定顶层键，保留其余键与嵌套结构', () => {
    const stripped = stripKeys({ apiKeys: 'secret', theme: 'dark', nested: { a: 1 } }, ['apiKeys']);
    expect(stripped).toEqual({ theme: 'dark', nested: { a: 1 } });
  });

  it('剥离多个键；不存在的键无副作用；原对象不被修改', () => {
    const source = { a: 1, b: 2, c: 3 };
    const stripped = stripKeys(source, ['a', 'c', 'missing']);
    expect(stripped).toEqual({ b: 2 });
    expect(source).toEqual({ a: 1, b: 2, c: 3 });
  });

  it('只剥顶层：同名的嵌套键保留', () => {
    const stripped = stripKeys({ keep: { apiKeys: 'inner' }, apiKeys: 'top' }, ['apiKeys']);
    expect(stripped).toEqual({ keep: { apiKeys: 'inner' } });
  });

  it('非对象输入原样返回；空 keys 返回等值副本', () => {
    expect(stripKeys('text', ['a'])).toBe('text');
    expect(stripKeys([1, 2], ['a'])).toEqual([1, 2]);
    expect(stripKeys({ a: 1 }, [])).toEqual({ a: 1 });
  });
});
