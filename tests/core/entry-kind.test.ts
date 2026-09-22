/**
 * `src/core/entry-kind.ts` —— 共享 JSON 形态判定（对抗式 review 修复项 4 / 6）。
 *
 * 钉住：scan 侧与 store 侧对「kind 怎么算」只有这一个实现点；
 * `isPlainObject` 的语义（数组 / null / 原始值不是 plain object）。
 * 注意 `__proto__` / `constructor` 作为 JSON 里普通字符串键时依然算「own key」。
 */

import os from 'node:os';
import path from 'node:path';

import { describe, expect, it } from 'vitest';

import { entryKindFor, isParsableJson, isPlainObject } from '../../src/core/entry-kind.js';
import { expandHome } from '../../src/core/paths.js';

describe('entryKindFor', () => {
  it('merge + 可 parse → json；merge + 不可 parse → file', () => {
    expect(entryKindFor('merge', '{"a":1}')).toBe('json');
    expect(entryKindFor('merge', '[1,2]')).toBe('json');
    expect(entryKindFor('merge', '42')).toBe('json');
    expect(entryKindFor('merge', '{ broken')).toBe('file');
    expect(entryKindFor('merge', '')).toBe('file');
  });

  it('mirror + 可 parse 仍是 file（mirror 不做键级判定）', () => {
    expect(entryKindFor('mirror', '{"a":1}')).toBe('file');
  });

  it('与 isParsableJson 一致（同一判据，无第二套解析）', () => {
    for (const sample of ['{"a":1}', 'null', 'not json', '']) {
      expect(entryKindFor('merge', sample)).toBe(isParsableJson(sample) ? 'json' : 'file');
    }
  });
});

describe('isPlainObject', () => {
  it('普通对象 → true；数组 / null / 原始值 → false', () => {
    expect(isPlainObject({})).toBe(true);
    expect(isPlainObject({ a: 1 })).toBe(true);
    expect(isPlainObject([])).toBe(false);
    expect(isPlainObject(null)).toBe(false);
    expect(isPlainObject(undefined)).toBe(false);
    expect(isPlainObject('x')).toBe(false);
    expect(isPlainObject(1)).toBe(false);
  });

  it('JSON.parse 产出的 __proto__ / constructor 仍是 own key（不算原型成员）', () => {
    const parsed = JSON.parse('{"__proto__":{"polluted":true},"constructor":"plain"}') as Record<string, unknown>;
    expect(isPlainObject(parsed)).toBe(true);
    expect(Object.prototype.hasOwnProperty.call(parsed, '__proto__')).toBe(true);
    expect(Object.prototype.hasOwnProperty.call(parsed, 'constructor')).toBe(true);
  });
});

/* ------------------------------------------------------------------ */
/* minor 5 / 6：重复实现已收敛到共享模块                                */
/* ------------------------------------------------------------------ */

describe('共享点收敛 — expandHome / isPlainObject（minor 5 / 6）', () => {
  it('paths.expandHome 是唯一实现（scan 直接 import 而非自带副本）', () => {
    expect(typeof expandHome).toBe('function');
    // 行为快照：'~' / '~/x' / '~\\x' / 其余原样
    expect(expandHome('~')).toBe(os.homedir());
    expect(expandHome('~/a/b')).toBe(path.join(os.homedir(), 'a/b'));
    expect(expandHome('/abs/path')).toBe('/abs/path');
    expect(expandHome('relative')).toBe('relative');
  });

  it('isPlainObject 三处调用方共用同一语义（数组 / null 都 false）', () => {
    // 若未来有人在某处重新定义，语义漂移会被这两个断言抓住
    expect(isPlainObject([])).toBe(false);
    expect(isPlainObject(null)).toBe(false);
    expect(isPlainObject({})).toBe(true);
  });
});
