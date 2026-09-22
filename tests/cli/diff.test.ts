/**
 * `homer diff` 测试（§2-P1-M4 验收）：
 *   - merge 键行 `key: old → new`
 *   - mirror 行级 +/-（LCS）
 *   - 空输出 = 无漂移
 *
 * 手工构造 snapshot + 注入快照，不 import store / scan 侧实现。
 */

import { mkdtempSync, rmSync, writeFileSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { runDiff } from '../../src/cli/commands/diff.js';
import { run } from '../../src/cli/index.js';
import { CliError } from '../../src/cli/render.js';
import { adapter, category, fileEntry, jsonEntry, writeConfig } from './helpers.js';

describe('runDiff: merge 键行', () => {
  let home: string;

  beforeEach(() => {
    home = mkdtempSync(join(tmpdir(), 'homer-diff-'));
    writeConfig(home);
  });

  afterEach(() => {
    rmSync(home, { recursive: true, force: true });
  });

  it('改键 → `key: old → new`', () => {
    const base = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ theme: 'light' }) })])];
    const local = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ theme: 'dark' }) })])];

    const text = runDiff({ homerHome: home }, { base, local, remote: base });
    expect(text).toContain('pi/settings');
    expect(text).toContain('theme: light → dark');
  });

  it('新增键 → old 为 (无)', () => {
    const base = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ a: 1 }) })])];
    const local = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ a: 1, b: 2 }) })])];

    const text = runDiff({ homerHome: home }, { base, local, remote: base });
    expect(text).toContain('b: (无) → 2');
  });

  it('删除键 → new 为 (无)', () => {
    const base = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ a: 1, b: 2 }) })])];
    const local = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ a: 1 }) })])];

    const text = runDiff({ homerHome: home }, { base, local, remote: base });
    expect(text).toContain('b: 2 → (无)');
  });

  it('嵌套对象 → 点路径键行', () => {
    const base = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ models: { openai: 'a' } }) })])];
    const local = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ models: { openai: 'b' } }) })])];

    const text = runDiff({ homerHome: home }, { base, local, remote: base });
    expect(text).toContain('models.openai: a → b');
  });

  it('JSON 对象值渲染为紧凑 JSON', () => {
    const base = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ arr: [1] }) })])];
    const local = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ arr: [1, 2] }) })])];

    const text = runDiff({ homerHome: home }, { base, local, remote: base });
    expect(text).toContain('arr: [1] → [1,2]');
  });
});

describe('runDiff: mirror 行级', () => {
  let home: string;

  beforeEach(() => {
    home = mkdtempSync(join(tmpdir(), 'homer-diff-mirror-'));
    writeConfig(home);
  });

  afterEach(() => {
    rmSync(home, { recursive: true, force: true });
  });

  it('文件新增 → 全文 +', () => {
    const base = [adapter('pi', [category('pi', 'skills', 'mirror', {})])];
    const local = [adapter('pi', [category('pi', 'skills', 'mirror', { 'new.md': fileEntry('line1\nline2\n') })])];

    const text = runDiff({ homerHome: home }, { base, local, remote: base });
    expect(text).toContain('pi/skills');
    expect(text).toContain('  new.md');
    expect(text).toContain('+line1');
    expect(text).toContain('+line2');
  });

  it('文件删除 → 全文 -', () => {
    const base = [adapter('pi', [category('pi', 'skills', 'mirror', { 'gone.md': fileEntry('old\n') })])];
    const local = [adapter('pi', [category('pi', 'skills', 'mirror', {})])];

    const text = runDiff({ homerHome: home }, { base, local, remote: base });
    expect(text).toContain('  gone.md');
    expect(text).toContain('-old');
  });

  it('文件修改 → 行级 +/-', () => {
    const base = [adapter('pi', [category('pi', 'skills', 'mirror', { 'a.md': fileEntry('keep\nold\n') })])];
    const local = [adapter('pi', [category('pi', 'skills', 'mirror', { 'a.md': fileEntry('keep\nnew\n') })])];

    const text = runDiff({ homerHome: home }, { base, local, remote: base });
    expect(text.split('\n')).toContain('-old');
    expect(text.split('\n')).toContain('+new');
    expect(text.split('\n')).toContain(' keep');
  });
});

describe('runDiff: 空输出 = 无漂移', () => {
  let home: string;

  beforeEach(() => {
    home = mkdtempSync(join(tmpdir(), 'homer-diff-empty-'));
    writeConfig(home);
  });

  afterEach(() => {
    rmSync(home, { recursive: true, force: true });
  });

  it('三方一致 → 空字符串', () => {
    const base = [
      adapter('pi', [
        category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ a: 1 }) }),
        category('pi', 'skills', 'mirror', { 'a.md': fileEntry('x\n') }),
      ]),
    ];
    expect(runDiff({ homerHome: home }, { base, local: base, remote: base })).toBe('');
  });

  it('只输出有漂移的分类，无漂移分类为空', () => {
    const base = [
      adapter('pi', [
        category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ a: 1 }) }),
        category('pi', 'skills', 'mirror', { 'a.md': fileEntry('x\n') }),
      ]),
    ];
    const local = [
      adapter('pi', [
        category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ a: 2 }) }),
        category('pi', 'skills', 'mirror', { 'a.md': fileEntry('x\n') }),
      ]),
    ];

    const text = runDiff({ homerHome: home }, { base, local, remote: base });
    expect(text).toContain('pi/settings');
    expect(text).not.toContain('pi/skills');
  });
});

describe('runDiff: --adapter / --category 过滤 + CliError', () => {
  let home: string;

  beforeEach(() => {
    home = mkdtempSync(join(tmpdir(), 'homer-diff-filter-'));
    writeConfig(home);
  });

  afterEach(() => {
    rmSync(home, { recursive: true, force: true });
  });

  it('--adapter 过滤', () => {
    const base = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ a: 1 }) })])];
    const local = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ a: 2 }) })])];

    expect(runDiff({ homerHome: home, adapter: 'other' }, { base, local, remote: base })).toBe('');
    expect(runDiff({ homerHome: home, adapter: 'pi' }, { base, local, remote: base })).toContain('a: 1 → 2');
  });

  it('--category 过滤', () => {
    const base = [
      adapter('pi', [
        category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ a: 1 }) }),
        category('pi', 'skills', 'mirror', { 'a.md': fileEntry('x\n') }),
      ]),
    ];
    const local = [
      adapter('pi', [
        category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ a: 2 }) }),
        category('pi', 'skills', 'mirror', { 'a.md': fileEntry('y\n') }),
      ]),
    ];

    const text = runDiff({ homerHome: home, category: 'skills' }, { base, local, remote: base });
    expect(text).toContain('pi/skills');
    expect(text).not.toContain('pi/settings');
  });

  it('无 homer.json → CliError', () => {
    const empty = mkdtempSync(join(tmpdir(), 'homer-diff-noconfig-'));
    try {
      expect(() => runDiff({ homerHome: empty })).toThrowError(CliError);
    } finally {
      rmSync(empty, { recursive: true, force: true });
    }
  });
});

describe('CLI dispatch: homer diff', () => {
  let home: string;

  beforeEach(() => {
    home = mkdtempSync(join(tmpdir(), 'homer-diff-cli-'));
  });

  afterEach(() => {
    rmSync(home, { recursive: true, force: true });
  });

  function capture(): { io: { out: (l: string) => void; err: (l: string) => void }; out: string[]; err: string[] } {
    const out: string[] = [];
    const err: string[] = [];
    return { io: { out: (l) => out.push(l), err: (l) => err.push(l) }, out, err };
  }

  it('无 config → exit 1', async () => {
    const cap = capture();
    const code = await run(['diff', '--home', home], cap.io);
    expect(code).toBe(1);
    expect(cap.err.join('\n')).toMatch(/homer init/);
  });

  it('diff --help 可用', async () => {
    const cap = capture();
    const code = await run(['diff', '--help'], cap.io);
    expect(code).toBe(0);
    expect(cap.out.join('\n')).toContain('用法: homer diff');
  });

  it('有 config 无漂移 → exit 0 且无输出', async () => {
    mkdirSync(home, { recursive: true });
    writeFileSync(
      join(home, 'homer.json'),
      `${JSON.stringify({ version: 1, adapters: { pi: { root: join(home, 'pi-agent'), enabled: true, categories: {} } } }, null, 2)}\n`,
      'utf8',
    );
    const cap = capture();
    const code = await run(['diff', '--home', home], cap.io);
    expect(code).toBe(0);
    expect(cap.out).toEqual([]);
  });
});

describe('runDiff: pull 方向 / 降级 / 冲突', () => {
  let home: string;

  beforeEach(() => {
    home = mkdtempSync(join(tmpdir(), 'homer-diff-pull-'));
    writeConfig(home);
  });

  afterEach(() => {
    rmSync(home, { recursive: true, force: true });
  });

  it('remote 改键（pull 方向）→ ↓ 键行', () => {
    const base = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ theme: 'light' }) })])];
    const remote = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ theme: 'dark' }) })])];

    const text = runDiff({ homerHome: home }, { base, local: base, remote });
    expect(text).toContain('pi/settings');
    expect(text).toContain('↓ theme: light → dark');
  });

  it('merge 分类内 JSON 损坏文件降级走行级 diff（不炸）', () => {
    const base = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': fileEntry('not json\n') })])];
    const local = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': fileEntry('not json changed\n') })])];

    const text = runDiff({ homerHome: home }, { base, local, remote: base });
    expect(text).toContain('  settings.json');
    expect(text).toContain('-not json');
    expect(text).toContain('+not json changed');
  });

  it('mirror 远端删除（pull-delete）→ 行级 -', () => {
    const base = [adapter('pi', [category('pi', 'skills', 'mirror', { 'gone.md': fileEntry('bye\n') })])];
    const remote = [adapter('pi', [category('pi', 'skills', 'mirror', {})])];

    const text = runDiff({ homerHome: home }, { base, local: base, remote });
    expect(text).toContain('  gone.md');
    expect(text).toContain('-bye');
  });

  it('mirror 双方都改 → 冲突分类仍渲染差异（exit 画面不为空）', () => {
    const base = [adapter('pi', [category('pi', 'skills', 'mirror', { 'a.md': fileEntry('base\n') })])];
    const local = [adapter('pi', [category('pi', 'skills', 'mirror', { 'a.md': fileEntry('mine\n') })])];
    const remote = [adapter('pi', [category('pi', 'skills', 'mirror', { 'a.md': fileEntry('theirs\n') })])];

    const text = runDiff({ homerHome: home }, { base, local, remote });
    expect(text).toContain('  a.md');
    expect(text).toContain('-base');
    expect(text).toContain('+mine');
    expect(text).toContain('远端(↓)');
    expect(text).toContain('+theirs');
  });
});
