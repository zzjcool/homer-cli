/**
 * `homer init` 测试（§2-P1-M4 验收）：
 *   - 打桩 scan + 真 store → InitReport 正确（store / homer.json 真实落盘）
 *   - 已存在 homer.json → 拒绝覆盖（提示 + exit 1），--force 可覆盖
 *
 * 打桩方式：`vi.mock` 替换 pi adapter 的 barrel（scan / defaults），
 * store / config 用真实实现（它们已合入 master 且行为稳定）。
 */

import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { AdapterSnapshot } from '../../src/core/types.js';

const SCAN_SNAPSHOT: AdapterSnapshot = {
  adapterId: 'pi',
  categories: [
    {
      adapterId: 'pi',
      category: 'settings',
      mode: 'merge',
      files: new Map([['settings.json', { kind: 'json', content: '{"theme":"dark"}' }]]),
    },
    {
      adapterId: 'pi',
      category: 'skills',
      mode: 'mirror',
      files: new Map([
        ['foo/SKILL.md', { kind: 'file', content: '# foo\n' }],
        ['bar/SKILL.md', { kind: 'file', content: '# bar\n' }],
      ]),
    },
  ],
};

const STUB_ADAPTER = {
  root: '~/.pi/agent',
  enabled: true,
  categories: {
    settings: { paths: ['settings.json'], mode: 'merge' as const },
    skills: { paths: ['skills/'], mode: 'mirror' as const },
  },
};

vi.mock('../../src/adapters/pi/index.js', () => ({
  PI_ADAPTER_ID: 'pi',
  DEFAULT_PI_ADAPTER: STUB_ADAPTER,
  scanAdapter: () => ({ snapshot: SCAN_SNAPSHOT, errors: [] }),
}));

const { runInit } = await import('../../src/cli/commands/init.js');
const { run } = await import('../../src/cli/index.js');

describe('runInit（打桩 scan + 真 store）', () => {
  let home: string;

  beforeEach(() => {
    home = mkdtempSync(join(tmpdir(), 'homer-init-'));
  });

  afterEach(() => {
    rmSync(home, { recursive: true, force: true });
  });

  it('InitReport 计数正确', async () => {
    const report = await runInit({ homerHome: home, adapters: ['pi'] });

    expect(report.homerHome).toBe(home);
    expect(report.adapters).toEqual([
      {
        id: 'pi',
        categories: [
          { name: 'settings', fileCount: 1 },
          { name: 'skills', fileCount: 2 },
        ],
      },
    ]);
  });

  it('真实写 store（§1.6 布局）与 homer.json', async () => {
    await runInit({ homerHome: home });

    expect(existsSync(join(home, 'store/pi/settings/settings.json'))).toBe(true);
    expect(existsSync(join(home, 'store/pi/skills/foo/SKILL.md'))).toBe(true);
    expect(readFileSync(join(home, 'store/pi/skills/foo/SKILL.md'), 'utf8')).toBe('# foo\n');

    const config = JSON.parse(readFileSync(join(home, 'homer.json'), 'utf8')) as {
      version: number;
      adapters: Record<string, unknown>;
    };
    expect(config.version).toBe(1);
    expect(Object.keys(config.adapters)).toEqual(['pi']);
  });

  it('已存在 homer.json → 拒绝覆盖（CliError）', async () => {
    await runInit({ homerHome: home });
    await expect(runInit({ homerHome: home })).rejects.toThrowError(/拒绝覆盖|已存在/);
  });

  it('--force（force: true）可覆盖', async () => {
    await runInit({ homerHome: home });
    // 人为破坏 store，确认 force 会重写
    rmSync(join(home, 'store'), { recursive: true, force: true });
    const report = await runInit({ homerHome: home }, { force: true });
    expect(report.adapters[0]?.id).toBe('pi');
    expect(existsSync(join(home, 'store/pi/settings/settings.json'))).toBe(true);
  });

  it('未知 adapter → CliError', async () => {
    await expect(runInit({ homerHome: home, adapters: ['nope'] })).rejects.toThrowError(/未知 adapter/);
  });
});

describe('CLI dispatch: homer init', () => {
  let home: string;

  beforeEach(() => {
    home = mkdtempSync(join(tmpdir(), 'homer-init-cli-'));
  });

  afterEach(() => {
    rmSync(home, { recursive: true, force: true });
  });

  function capture(): { io: { out: (l: string) => void; err: (l: string) => void }; out: string[]; err: string[] } {
    const out: string[] = [];
    const err: string[] = [];
    return { io: { out: (l) => out.push(l), err: (l) => err.push(l) }, out, err };
  }

  it('init --json 输出 InitReport（exit 0）', async () => {
    const cap = capture();
    const code = await run(['init', '--json', '--home', home], cap.io);
    expect(code).toBe(0);

    const report = JSON.parse(cap.out.join('\n')) as {
      homerHome: string;
      adapters: { id: string; categories: { name: string; fileCount: number }[] }[];
    };
    expect(report.homerHome).toBe(home);
    expect(report.adapters[0]).toEqual({
      id: 'pi',
      categories: [
        { name: 'settings', fileCount: 1 },
        { name: 'skills', fileCount: 2 },
      ],
    });
  });

  it('重复 init → exit 1 且提示 --force', async () => {
    const first = capture();
    await run(['init', '--home', home], first.io);
    expect(existsSync(join(home, 'homer.json'))).toBe(true);

    const second = capture();
    const code = await run(['init', '--home', home], second.io);
    expect(code).toBe(1);
    expect(second.err.join('\n')).toMatch(/--force/);
  });

  it('重复 init --force → exit 0', async () => {
    await run(['init', '--home', home], capture().io);
    const cap = capture();
    const code = await run(['init', '--force', '--home', home], cap.io);
    expect(code).toBe(0);
  });

  it('init --help 可用', async () => {
    const cap = capture();
    const code = await run(['init', '--help'], cap.io);
    expect(code).toBe(0);
    expect(cap.out.join('\n')).toContain('用法: homer init');
  });

  it('init 人类可读输出包含文件数', async () => {
    const cap = capture();
    await run(['init', '--home', home], cap.io);
    const text = cap.out.join('\n');
    expect(text).toContain(`homer init: ${home}`);
    expect(text).toContain('pi: 3 个文件');
    expect(text).toContain('settings: 1');
    expect(text).toContain('skills: 2');
  });
});

describe('CLI dispatch: 顶层用法', () => {
  function capture(): { io: { out: (l: string) => void; err: (l: string) => void }; out: string[]; err: string[] } {
    const out: string[] = [];
    const err: string[] = [];
    return { io: { out: (l) => out.push(l), err: (l) => err.push(l) }, out, err };
  }

  it('无参数 → exit 1 + 用法', async () => {
    const cap = capture();
    expect(await run([], cap.io)).toBe(1);
    expect(cap.err.join('\n')).toContain('homer <command>');
  });

  it('--help → exit 0', async () => {
    const cap = capture();
    expect(await run(['--help'], cap.io)).toBe(0);
    expect(cap.out.join('\n')).toContain('homer <command>');
  });

  it('未知命令 → exit 1', async () => {
    const cap = capture();
    expect(await run(['frobnicate'], cap.io)).toBe(1);
    expect(cap.err.join('\n')).toContain('未知命令');
  });
});
