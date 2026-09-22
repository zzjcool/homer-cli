import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { DEFAULT_PI_ADAPTER, PI_ADAPTER_ID, scanAdapter } from '../../../src/adapters/pi/index.js';
import type { AdapterSnapshot, CategorySnapshot, SnapshotEntry } from '../../../src/core/types.js';

let tmp: string;

beforeEach(() => {
  tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-pi-scan-'));
});

afterEach(() => {
  fs.rmSync(tmp, { recursive: true, force: true });
});

function write(rel: string, content: string): void {
  const abs = path.join(tmp, rel);
  fs.mkdirSync(path.dirname(abs), { recursive: true });
  fs.writeFileSync(abs, content, 'utf8');
}

/** 把 snapshot 转成纯对象，便于精确 toEqual。 */
function dump(snapshot: AdapterSnapshot): Record<string, Record<string, SnapshotEntry>> {
  const out: Record<string, Record<string, SnapshotEntry>> = {};
  for (const cat of snapshot.categories) {
    const files: Record<string, SnapshotEntry> = {};
    for (const [k, v] of cat.files) files[k] = v;
    out[cat.category] = files;
  }
  return out;
}

function cat(snapshot: AdapterSnapshot, name: string): CategorySnapshot {
  const found = snapshot.categories.find((c) => c.category === name);
  if (!found) throw new Error(`category not found: ${name}`);
  return found;
}

const GOOD_SETTINGS = JSON.stringify({ theme: 'dark', nested: { a: 1 } }, null, 2);
const BROKEN_JSON = '{ "theme": "dark", ';

describe('pi adapter defaults', () => {
  it('DEFAULT_PI_ADAPTER 逐字符合计划 §1.5', () => {
    expect(PI_ADAPTER_ID).toBe('pi');
    expect(DEFAULT_PI_ADAPTER).toEqual({
      root: '~/.pi/agent',
      enabled: true,
      categories: {
        settings: { paths: ['settings.json', 'keybindings.json'], mode: 'merge' },
        skills: { paths: ['skills/'], mode: 'mirror' },
        extensions: { paths: ['extensions/'], mode: 'mirror', exclude: ['*cache*'] },
        agents: { paths: ['agents/'], mode: 'mirror' },
        models: { paths: ['models.json'], mode: 'merge', excludeKeys: ['apiKeys'] },
        prompts: { paths: ['prompts/'], mode: 'mirror' },
        themes: { paths: ['themes/'], mode: 'mirror' },
      },
      ignore: [
        'auth.json',
        'trust.json',
        'sessions/',
        'npm/',
        'git/',
        'tmp/',
        'bin/',
        '*.bak',
        '*.bak-*',
        '*.bak*',
        '*.log',
        'run-history.jsonl',
      ],
    });
  });

  it('7 个分类 + ignore 列表逐项冻结', () => {
    expect(Object.keys(DEFAULT_PI_ADAPTER.categories)).toEqual([
      'settings',
      'skills',
      'extensions',
      'agents',
      'models',
      'prompts',
      'themes',
    ]);
    expect(DEFAULT_PI_ADAPTER.categories['settings']).toEqual({
      paths: ['settings.json', 'keybindings.json'],
      mode: 'merge',
    });
    expect(DEFAULT_PI_ADAPTER.categories['extensions']).toEqual({
      paths: ['extensions/'],
      mode: 'mirror',
      exclude: ['*cache*'],
    });
    expect(DEFAULT_PI_ADAPTER.categories['models']).toEqual({
      paths: ['models.json'],
      mode: 'merge',
      excludeKeys: ['apiKeys'],
    });
    expect(DEFAULT_PI_ADAPTER.ignore).toEqual([
      'auth.json',
      'trust.json',
      'sessions/',
      'npm/',
      'git/',
      'tmp/',
      'bin/',
      '*.bak',
      '*.bak-*',
      '*.bak*',
      '*.log',
      'run-history.jsonl',
    ]);
  });
});

describe('scanAdapter — 7 分类 fixture', () => {
  beforeEach(() => {
    // 7 分类代表文件
    write('settings.json', GOOD_SETTINGS);
    write('keybindings.json', BROKEN_JSON); // merge 分类内 JSON 损坏 → 降级 file
    write('skills/foo/SKILL.md', '# foo skill\n');
    write('skills/foo/reference/notes.md', 'notes\n');
    write('skills/bar/SKILL.md', '# bar skill\n');
    write('extensions/tool/index.js', 'export const x = 1;\n');
    write('extensions/pi-foo-cache/index.js', 'MUST BE EXCLUDED\n');
    write('extensions/mycache.json', '{"MUST":"BE EXCLUDED"}');
    write('agents/reviewer.md', '# reviewer\n');
    write('models.json', JSON.stringify({ provider: 'openai', apiKeys: { openai: 'sk-x' } }));
    write('prompts/system.md', '# system\n');
    write('themes/dark.json', JSON.stringify({ bg: '#000' })); // mirror → kind 永远是 file

    // 垃圾
    write('sessions/x', 'session data');
    write('npm/y', 'npm data');
    write('auth.json', '{"token":"secret"}');
    write('models.json.bak-predirect', GOOD_SETTINGS);
    write('pi-tui-crash.log', 'stack trace');
  });

  it('快照精确匹配（含垃圾 0 条 / kind 降级 / exclude）', () => {
    const { snapshot, errors } = scanAdapter(PI_ADAPTER_ID, { ...DEFAULT_PI_ADAPTER, root: tmp });

    expect(errors).toEqual([]);
    expect(snapshot.adapterId).toBe('pi');
    expect(snapshot.categories.map((c) => c.category)).toEqual([
      'settings',
      'skills',
      'extensions',
      'agents',
      'models',
      'prompts',
      'themes',
    ]);

    expect(dump(snapshot)).toEqual({
      settings: {
        'settings.json': { kind: 'json', content: GOOD_SETTINGS },
        'keybindings.json': { kind: 'file', content: BROKEN_JSON },
      },
      skills: {
        'bar/SKILL.md': { kind: 'file', content: '# bar skill\n' },
        'foo/SKILL.md': { kind: 'file', content: '# foo skill\n' },
        'foo/reference/notes.md': { kind: 'file', content: 'notes\n' },
      },
      extensions: {
        'tool/index.js': { kind: 'file', content: 'export const x = 1;\n' },
      },
      agents: {
        'reviewer.md': { kind: 'file', content: '# reviewer\n' },
      },
      models: {
        'models.json': {
          kind: 'json',
          content: JSON.stringify({ provider: 'openai', apiKeys: { openai: 'sk-x' } }),
        },
      },
      prompts: {
        'system.md': { kind: 'file', content: '# system\n' },
      },
      themes: {
        'dark.json': { kind: 'file', content: JSON.stringify({ bg: '#000' }) },
      },
    });
  });

  it('垃圾 0 条：sessions/npm/auth/bak/log 全部不出现', () => {
    const { snapshot } = scanAdapter(PI_ADAPTER_ID, { ...DEFAULT_PI_ADAPTER, root: tmp });
    const allKeys = snapshot.categories.flatMap((c) => [...c.files.keys()]);
    const allText = snapshot.categories
      .flatMap((c) => [...c.files.values()])
      .map((e) => e.content)
      .join('\n');

    for (const junk of [
      'x',
      'y',
      'auth.json',
      'models.json.bak-predirect',
      'pi-tui-crash.log',
      'pi-foo-cache/index.js',
      'mycache.json',
    ]) {
      expect(allKeys).not.toContain(junk);
    }
    expect(allText).not.toContain('session data');
    expect(allText).not.toContain('npm data');
    expect(allText).not.toContain('secret');
    expect(allText).not.toContain('stack trace');
    expect(allText).not.toContain('MUST BE EXCLUDED');
  });

  it('settings.json = kind:json；keybindings 损坏 → kind:file（merge 降级）', () => {
    const { snapshot } = scanAdapter(PI_ADAPTER_ID, { ...DEFAULT_PI_ADAPTER, root: tmp });
    const s = cat(snapshot, 'settings');
    expect(s.mode).toBe('merge');
    expect(s.files.get('settings.json')?.kind).toBe('json');
    expect(s.files.get('keybindings.json')?.kind).toBe('file');
  });

  it('models.json 损坏时同样降级 kind:file', () => {
    fs.writeFileSync(path.join(tmp, 'models.json'), 'not json at all', 'utf8');
    const { snapshot, errors } = scanAdapter(PI_ADAPTER_ID, { ...DEFAULT_PI_ADAPTER, root: tmp });
    expect(errors).toEqual([]);
    expect(cat(snapshot, 'models').files.get('models.json')).toEqual({
      kind: 'file',
      content: 'not json at all',
    });
  });

  it('mirror 分类里的合法 JSON 仍是 kind:file（themes/prompts/skills）', () => {
    const { snapshot } = scanAdapter(PI_ADAPTER_ID, { ...DEFAULT_PI_ADAPTER, root: tmp });
    expect(cat(snapshot, 'themes').files.get('dark.json')?.kind).toBe('file');
    expect(cat(snapshot, 'themes').mode).toBe('mirror');
  });

  it('relPath 规则：目录型 category 用目录下相对路径；单文件用文件名', () => {
    const { snapshot } = scanAdapter(PI_ADAPTER_ID, { ...DEFAULT_PI_ADAPTER, root: tmp });
    expect([...cat(snapshot, 'skills').files.keys()]).toEqual([
      'bar/SKILL.md',
      'foo/SKILL.md',
      'foo/reference/notes.md',
    ]);
    expect([...cat(snapshot, 'settings').files.keys()]).toEqual([
      'keybindings.json',
      'settings.json',
    ]);
    expect([...cat(snapshot, 'models').files.keys()]).toEqual(['models.json']);
  });

  it('exclude *cache* 剪掉目录与文件（含嵌套目录整棵）', () => {
    write('extensions/pi-cache/nested/deep.js', 'deep\n');
    write('extensions/keep/sub/keep.js', 'keep\n');
    const { snapshot } = scanAdapter(PI_ADAPTER_ID, { ...DEFAULT_PI_ADAPTER, root: tmp });
    expect([...cat(snapshot, 'extensions').files.keys()]).toEqual([
      'keep/sub/keep.js',
      'tool/index.js',
    ]);
  });

  it('空目录 / 缺目录不报错（prompts、themes 可缺失）', () => {
    fs.rmSync(path.join(tmp, 'prompts'), { recursive: true, force: true });
    fs.rmSync(path.join(tmp, 'themes'), { recursive: true, force: true });
    fs.mkdirSync(path.join(tmp, 'themes'), { recursive: true }); // 存在但为空
    const { snapshot, errors } = scanAdapter(PI_ADAPTER_ID, { ...DEFAULT_PI_ADAPTER, root: tmp });
    expect(errors).toEqual([]);
    expect(cat(snapshot, 'prompts').files.size).toBe(0);
    expect(cat(snapshot, 'themes').files.size).toBe(0);
  });

  it('enabled:false 的分类被跳过；adapter enabled:false → 空 snapshot', () => {
    const cfg = {
      ...DEFAULT_PI_ADAPTER,
      categories: {
        ...DEFAULT_PI_ADAPTER.categories,
        themes: { paths: ['themes/'], mode: 'mirror' as const, enabled: false },
      },
    };
    const { snapshot } = scanAdapter(PI_ADAPTER_ID, { ...cfg, root: tmp });
    expect(snapshot.categories.map((c) => c.category)).not.toContain('themes');

    const off = scanAdapter(PI_ADAPTER_ID, { ...cfg, root: tmp, enabled: false });
    expect(off.snapshot.categories).toEqual([]);
    expect(off.errors).toEqual([]);
  });

  it('adapter 级 ignore 追加生效（trust.json / run-history.jsonl）', () => {
    write('trust.json', '{"trusted":true}');
    write('run-history.jsonl', 'line\n');
    const { snapshot } = scanAdapter(PI_ADAPTER_ID, { ...DEFAULT_PI_ADAPTER, root: tmp });
    const allKeys = snapshot.categories.flatMap((c) => [...c.files.keys()]);
    expect(allKeys).not.toContain('trust.json');
    expect(allKeys).not.toContain('run-history.jsonl');
  });
});

describe('scanAdapter — root 不存在 / 非目录', () => {
  it('root 不存在 → 空 snapshot + errors，不 throw', () => {
    const missing = path.join(tmp, 'nope', 'agent');
    let outcome: ReturnType<typeof scanAdapter> | undefined;
    expect(() => {
      outcome = scanAdapter(PI_ADAPTER_ID, { ...DEFAULT_PI_ADAPTER, root: missing });
    }).not.toThrow();
    const { snapshot, errors } = outcome!;
    expect(snapshot.adapterId).toBe('pi');
    expect(snapshot.categories).toEqual([]);
    expect(errors).toHaveLength(1);
    expect(errors[0]?.path).toBe(missing);
    expect(errors[0]?.message).toMatch(/ENOENT|no such file/);
  });

  it('root 是文件而非目录 → 空 snapshot + errors', () => {
    const file = path.join(tmp, 'not-a-dir');
    fs.writeFileSync(file, 'x');
    const { snapshot, errors } = scanAdapter(PI_ADAPTER_ID, { ...DEFAULT_PI_ADAPTER, root: file });
    expect(snapshot.categories).toEqual([]);
    expect(errors).toHaveLength(1);
    expect(errors[0]?.message).toBe('not a directory');
  });
});
