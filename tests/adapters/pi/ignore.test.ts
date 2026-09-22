import { describe, expect, it } from 'vitest';

import { globMatch, matchesIgnore } from '../../../src/adapters/pi/ignore.js';
import { DEFAULT_PI_ADAPTER } from '../../../src/adapters/pi/defaults.js';

describe('globMatch — 冻结的 glob 语义', () => {
  // 字面量 / '*' 非跨 '/' / 尾 '/' 目录前缀 —— 逐条锁死
  const cases: { pattern: string; path: string; expected: boolean; note: string }[] = [
    { pattern: 'auth.json', path: 'auth.json', expected: true, note: '字面量精确匹配' },
    { pattern: 'auth.json', path: 'sub/auth.json', expected: false, note: '字面量不匹配子路径' },
    { pattern: 'auth.json', path: 'auth.jsonx', expected: false, note: '字面量必须全等' },
    { pattern: '*.bak', path: 'models.json.bak', expected: true, note: '* 任意非 / 字符' },
    { pattern: '*.bak', path: 'a/b.bak', expected: false, note: '* 不跨 /' },
    { pattern: '*.bak-*', path: 'models.json.bak-predirect', expected: true, note: '两侧 *' },
    { pattern: '*.bak-*', path: 'x/y.bak-predirect', expected: false, note: '两段均不跨 /' },
    { pattern: '*.bak-*', path: 'models.json.bak', expected: false, note: '缺 -<后缀>' },
    { pattern: '*.log', path: 'pi-tui-crash.log', expected: true, note: '日志后缀' },
    { pattern: '*cache*', path: 'pi-foo-cache', expected: true, note: '两端通配' },
    { pattern: '*cache*', path: 'mycache.json', expected: true, note: '中缀匹配' },
    { pattern: '*cache*', path: 'keep/index.js', expected: false, note: '不命中' },
    { pattern: 'sessions/', path: 'sessions/x', expected: true, note: '尾 / = 目录前缀' },
    { pattern: 'sessions/', path: 'sessions/a/b/c', expected: true, note: '前缀命中深层' },
    { pattern: 'sessions/', path: 'sessions', expected: true, note: '目录本身命中' },
    { pattern: 'sessions/', path: 'mysessions/x', expected: false, note: '前缀必须整段' },
    { pattern: 'npm/', path: 'npm', expected: true, note: '目录自身' },
    { pattern: 'npm/', path: 'npm/foo/bar', expected: true, note: '深层' },
    { pattern: 's*', path: 'skills/foo', expected: false, note: '单通配不跨 /' },
    { pattern: 's*', path: 'settings.json', expected: true, note: '单段通配' },
    { pattern: 's*/foo', path: 'skills/foo', expected: true, note: '多段通配' },
    { pattern: '', path: 'anything', expected: false, note: '空模式不匹配' },
  ];

  it.each(cases)('$pattern vs $path → $expected ($note)', ({ pattern, path, expected }) => {
    expect(globMatch(path, pattern)).toBe(expected);
  });

  it('不支持 ? 与 **（按字面/单星处理，不跨 /）', () => {
    // '?' 是字面字符
    expect(globMatch('a?b', 'a?b')).toBe(true);
    expect(globMatch('axb', 'a?b')).toBe(false);
    // '**' 等价单个 '*'，仍不跨 '/'（只吃一段）
    expect(globMatch('a/b', '**')).toBe(false);
    expect(globMatch('ab', '**')).toBe(true);
    expect(globMatch('deep/x', '**/x')).toBe(true);
    expect(globMatch('a/b/x', '**/x')).toBe(false);
  });

  it('前导 ./ 与 / 被归一化', () => {
    expect(globMatch('./sessions/x', 'sessions/')).toBe(true);
    expect(globMatch('/sessions/x', 'sessions/')).toBe(true);
    expect(globMatch('sessions/x', './sessions/')).toBe(true);
  });
});

describe('matchesIgnore — 表驱动（默认 pi ignore 列表）', () => {
  const ignore = DEFAULT_PI_ADAPTER.ignore ?? [];

  const cases: { path: string; expected: boolean; note: string }[] = [
    { path: 'auth.json', expected: true, note: '显式列名' },
    { path: 'trust.json', expected: true, note: '显式列名' },
    { path: 'sessions/x', expected: true, note: 'sessions/ 前缀' },
    { path: 'sessions/2026/foo.jsonl', expected: true, note: 'sessions/ 深层' },
    { path: 'npm/y', expected: true, note: 'npm/ 前缀' },
    { path: 'git/config', expected: true, note: 'git/ 前缀' },
    { path: 'tmp/scratch', expected: true, note: 'tmp/ 前缀' },
    { path: 'bin/homer', expected: true, note: 'bin/ 前缀' },
    { path: 'models.json.bak-predirect', expected: true, note: '*.bak-*' },
    { path: 'models.json.bak', expected: true, note: '*.bak' },
    { path: 'settings.json.bak', expected: true, note: '*.bak' },
    { path: 'pi-tui-crash.log', expected: true, note: '*.log' },
    { path: 'run-history.jsonl', expected: true, note: '显式列名' },
    { path: 'settings.json', expected: false, note: '正常配置' },
    { path: 'skills/foo/SKILL.md', expected: false, note: '正常配置' },
    { path: 'themes/dark.json', expected: false, note: '正常配置' },
    { path: 'sessions.json', expected: false, note: '前缀模式不误伤同前缀文件' },
    { path: 'auth.json.example', expected: false, note: '字面量不误伤' },
    { path: 'models/bak', expected: false, note: '*.bak 不跨 /' },
    { path: 'nested/pi-tui-crash.log', expected: false, note: '*.log 不跨 /（仅顶层段）' },
  ];

  it.each(cases)('$path → $expected ($note)', ({ path, expected }) => {
    expect(matchesIgnore(path, ignore)).toBe(expected);
  });

  it('空 pattern 列表 → 恒 false', () => {
    expect(matchesIgnore('anything', [])).toBe(false);
  });

  it('自定义模式列表同样生效', () => {
    expect(matchesIgnore('foo/bar.txt', ['foo/'])).toBe(true);
    expect(matchesIgnore('foo/bar.txt', ['*.txt'])).toBe(false);
    expect(matchesIgnore('bar.txt', ['*.txt'])).toBe(true);
  });
});
