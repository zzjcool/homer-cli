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

/* ------------------------------------------------------------------ */
/* 对抗式 review 修复项 1：deepEqual 的 NaN / -0 语义                  */
/* ------------------------------------------------------------------ */

/**
 * 修复前 `deepEqual` 用 `a === b`，NaN !== NaN 会让「三方同值 NaN」被判成
 * 双方都改 → 冲突（假冲突）；同时 +0/-0 被当成相等（漏报变更）。
 * JSON 本身不能表示 NaN，但调用方（drift / CLI 注入）可直接传 JS 值，
 * 且 `stripKeys` / 未来 YAML 前端也会产出非 JSON 字面量值。
 */
describe('deepEqual — Object.is 语义（NaN / -0）', () => {
  it('base≡local≡remote 全为 NaN → 无冲突、无变更（不误报 both-modified）', () => {
    const result = mergeJson({ threshold: Number.NaN }, { threshold: Number.NaN }, { threshold: Number.NaN });
    expect(result.status).toBe('clean');
    expect(result.conflicts).toEqual([]);
  });

  it('NaN 是「未变」：base≡local 的 NaN，remote 改值 → 取 remote（走 base≡local 分支）', () => {
    const result = mergeJson({ threshold: Number.NaN }, { threshold: Number.NaN }, { threshold: 5 });
    expect(result.status).toBe('clean');
    expect(result.conflicts).toEqual([]);
    expect(result.merged).toEqual({ threshold: 5 });
  });

  it('双方都改成不同值（含 NaN vs 数字）→ both-modified 冲突', () => {
    const result = mergeJson({ threshold: 1 }, { threshold: Number.NaN }, { threshold: 2 });
    expect(result.status).toBe('conflict');
    expect(result.conflicts.map((c) => c.keyPath)).toEqual(['threshold']);
  });

  it('diffJson：NaN 与自身相等 → 不计 changed', () => {
    expect(diffJson({ a: Number.NaN }, { a: Number.NaN })).toEqual({ changed: 0, added: 0, deleted: 0, keys: [] });
  });

  it('-0 与 +0 视为不同值（Object.is 语义）→ 计入 changed', () => {
    // `===` 下 -0 === 0 为 true（漏报）；Object.is 下不同（报出来更安全）
    expect(diffJson({ a: 0 }, { a: -0 })).toEqual({ changed: 1, added: 0, deleted: 0, keys: ['a'] });
    expect(diffJson({ a: -0 }, { a: -0 })).toEqual({ changed: 0, added: 0, deleted: 0, keys: [] });
  });

  it('嵌套数组里的 NaN 同样按相等处理', () => {
    expect(diffJson({ list: [Number.NaN, 1] }, { list: [Number.NaN, 1] })).toEqual({
      changed: 0, added: 0, deleted: 0, keys: [],
    });
    expect(diffJson({ list: [Number.NaN] }, { list: [1] })).toEqual({
      changed: 1, added: 0, deleted: 0, keys: ['list'],
    });
  });
});

/* ------------------------------------------------------------------ */
/* 对抗式 review 修复项 2：原型键（__proto__ / constructor）守卫        */
/* ------------------------------------------------------------------ */

/**
 * 修复前 `childSlot` 直接 `parent.value[key]`：`__proto__` / `constructor`
 * 会沿原型链取到 `Object.prototype` 的成员（例如 `constructor` = Object 函数），
 * 让「base 没有该 own key、local 也没有」的场景凭空生出漂移/冲突。
 * 修复后一律先 `hasOwnProperty` 守卫，按普通键处理。
 */
describe('原型键守卫 — __proto__ / constructor 不污染判定', () => {
  it('childSlot 不沿原型链取值：own key absent 即 absent（无假冲突）', () => {
    // 普通对象：constructor 来自原型而非 own key。修复前 childSlot 会取到 Object 构造函数，
    // 把「两侧都未声明 constructor」看成「都有且同值」；下述场景则把「只有 local 改」看错。
    const base: Record<string, unknown> = { a: 1 };
    const localSame: Record<string, unknown> = { a: 1 };
    expect(diffJson(base, localSame).keys).toEqual([]);

    const localChanged: Record<string, unknown> = { a: 2 };
    expect(diffJson(base, localChanged).keys).toEqual(['a']);
  });

  it('Object.prototype 未被污染：merge / stripKeys 后全局原型干净', () => {
    const evil = JSON.parse('{"__proto__":{"polluted":true}}') as Record<string, unknown>;
    mergeJson(evil, evil, evil);
    stripKeys(evil, []);

    // 任何对象都不应被注入 polluted
    expect(({} as Record<string, unknown>)['polluted']).toBeUndefined();
    expect(Object.prototype.hasOwnProperty.call(Object.prototype, 'polluted')).toBe(false);
    expect(Object.keys({}).length).toBe(0);
  });

  it('同名字符串键 __proto__ 作为普通键参与判定（不抛错）', () => {
    const base = JSON.parse('{"__proto__":"base"}') as Record<string, unknown>;
    const local = JSON.parse('{"__proto__":"local"}') as Record<string, unknown>;
    // parsed JSON 的 __proto__ 是 own key（值为字符串），改值记为 changed（不是 deleted）
    expect(diffJson(base, local)).toEqual({ changed: 1, added: 0, deleted: 0, keys: ['__proto__'] });
    const result = mergeJson(base, local, base);
    expect(result.status).toBe('clean');
  });

  it('constructor 作为普通 own key 时正常参与 merge', () => {
    const result = mergeJson({ constructor: 'a' }, { constructor: 'b' }, { constructor: 'a' });
    expect(result.status).toBe('clean');
    expect(result.merged).toEqual({ constructor: 'b' });
  });

  it('stripKeys 命中原型键名不做特殊处理（不污染原型，按普通键剥离）', () => {
    const src = JSON.parse('{"__proto__":{"x":1},"constructor":"c","keep":2}') as Record<string, unknown>;
    const stripped = stripKeys(src, ['keep']) as Record<string, unknown>;
    expect(Object.prototype.hasOwnProperty.call(stripped, 'keep')).toBe(false);
    expect(stripped['constructor']).toBe('c');
    // 赋值 `out['__proto__'] = v` 会走原型 setter 被静默丢弃（不污染原型）
    expect(({} as Record<string, unknown>)['x']).toBeUndefined();
  });
});

describe('原型键守卫 — 区分 added 与 changed（修复前会把原型成员当成 own 值）', () => {
  it('base 缺 constructor、local 新增 constructor → 记 added（修复前会沿原型取值记成 changed）', () => {
    // 用普通对象字面量（带 Object.prototype）：修复前 `parent.value['constructor']`
    // 会取到 Object 构造函数，把它当成「base 已有该键」→ 误记 changed 而非 added。
    const base: Record<string, unknown> = {};
    const local: Record<string, unknown> = { constructor: 'added' };
    expect(diffJson(base, local)).toEqual({ changed: 0, added: 1, deleted: 0, keys: ['constructor'] });
  });

  it('base 缺 __proto__、local 新增 __proto__ → 记 added', () => {
    const base: Record<string, unknown> = {};
    // JSON.parse 会产出 own key `__proto__`（对象字面量则改原型，不用）
    const local = JSON.parse('{"__proto__":"added"}') as Record<string, unknown>;
    expect(Object.prototype.hasOwnProperty.call(local, '__proto__')).toBe(true);
    expect(diffJson(base, local)).toEqual({ changed: 0, added: 1, deleted: 0, keys: ['__proto__'] });
  });

  it('merge：base 缺 constructor、仅 local 新增 → 取 local，无冲突', () => {
    const base: Record<string, unknown> = {};
    const local: Record<string, unknown> = { constructor: 'L' };
    const result = mergeJson(base, local, base);
    expect(result.status).toBe('clean');
    expect(result.conflicts).toEqual([]);
  });
});
