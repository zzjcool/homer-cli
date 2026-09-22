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
import { adapter, category, fileEntry, jsonEntry, rawJsonEntry, writeConfig } from './helpers.js';

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
    // root 必须可读（否则按 M-A 会打印 ⚠ 告警行；本用例测的是「无漂移 = 空输出」）
    mkdirSync(join(home, 'pi-agent'), { recursive: true });
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
    // 冲突行带 `⚡` 前缀（minor 3）
    expect(text).toContain('  ⚡ a.md');
    expect(text).toContain('-base');
    expect(text).toContain('+mine');
    expect(text).toContain('远端(↓)');
    expect(text).toContain('+theirs');
  });
});

/* ------------------------------------------------------------------ */
/* minor 3：冲突行 `⚡` 前缀                                            */
/* ------------------------------------------------------------------ */

describe('runDiff: 冲突标记 ⚡（minor 3）', () => {
  let home: string;

  beforeEach(() => {
    home = mkdtempSync(join(tmpdir(), 'homer-diff-conflict-'));
    writeConfig(home);
  });

  afterEach(() => {
    rmSync(home, { recursive: true, force: true });
  });

  it('merge 键级冲突（both-modified）→ 键行带 ⚡ 前缀', () => {
    const base = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ theme: 'light' }) })])];
    const local = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ theme: 'dark' }) })])];
    const remote = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ theme: 'blue' }) })])];

    const text = runDiff({ homerHome: home }, { base, local, remote });
    const lines = text.split('\n');

    // 键行 `⚡ theme: light → dark`；远端行为 `↓ ⚡ theme: light → blue`
    expect(lines).toContain('    ⚡ theme: light → dark');
    expect(lines).toContain('    ↓ ⚡ theme: light → blue');
    // 非冲突键行不带 ⚡（用于对比的 clean 键）
    expect(text).not.toContain('⚡ (无)');
  });

  it('merge 文件级冲突（modify-vs-delete）→ 文件名行带 ⚡', () => {
    // base 有文件、local 删除、remote 改值 → 文件级 modify-vs-delete
    const base = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ a: 1 }) })])];
    const local = [adapter('pi', [category('pi', 'settings', 'merge', {})])];
    const remote = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ a: 2 }) })])];

    const text = runDiff({ homerHome: home }, { base, local, remote });
    expect(text.split('\n')).toContain('  ⚡ settings.json');
  });

  it('mirror conflict op（双方都改）→ 文件名行带 ⚡', () => {
    const base = [adapter('pi', [category('pi', 'skills', 'mirror', { 'a.md': fileEntry('base\n') })])];
    const local = [adapter('pi', [category('pi', 'skills', 'mirror', { 'a.md': fileEntry('mine\n') })])];
    const remote = [adapter('pi', [category('pi', 'skills', 'mirror', { 'a.md': fileEntry('theirs\n') })])];

    const text = runDiff({ homerHome: home }, { base, local, remote });
    expect(text.split('\n')).toContain('  ⚡ a.md');
  });

  it('无冲突的普通漂移不带 ⚡（反向断言）', () => {
    const base = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ a: 1 }) })])];
    const local = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ a: 2 }) })])];

    const text = runDiff({ homerHome: home }, { base, local, remote: base });
    expect(text).toContain('    a: 1 → 2');
    expect(text).not.toContain('⚡');
  });
});

/* ------------------------------------------------------------------ */
/* minor 4：diff slotOf 解析失败 → 降级行级（与 drift.isDegraded 对齐）  */
/* ------------------------------------------------------------------ */

describe('runDiff: 损坏 JSON 条目降级（minor 4）', () => {
  let home: string;

  beforeEach(() => {
    home = mkdtempSync(join(tmpdir(), 'homer-diff-degrade-'));
    writeConfig(home);
  });

  afterEach(() => {
    rmSync(home, { recursive: true, force: true });
  });

  it('kind:json 但内容损坏 → 走行级 diff（不再按字符串「键值」渲染）', () => {
    // 修复前 slotOf 解析失败会退化成 `{ has: true, value: <原文> }`，
    // 渲染出 `$: <原文> → ...` 这种假键行；修复后应与 drift 一致走 mirror 行级。
    const base = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': rawJsonEntry('{ broken\n') })])];
    const local = [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': rawJsonEntry('{ broken changed\n') })])];

    const text = runDiff({ homerHome: home }, { base, local, remote: base });
    expect(text).toContain('  settings.json');
    expect(text).toContain('-{ broken');
    expect(text).toContain('+{ broken changed');
    expect(text).not.toContain('$:');
  });
});
