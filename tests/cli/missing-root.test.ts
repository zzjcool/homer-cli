/**
 * M-A 回归：adapter root 不可读时 status / diff / init 的行为。
 *
 * 修复前的 bug：`collectSnapshotSources` 丢弃 `ScanOutcome.errors`，
 * root 不存在 → local 快照为空 → base 里每个文件都被算成 push-delete，
 * `homer status` 报出「全量 push」的假漂移。
 *
 * 修复后：
 *   - StatusReport / InitReport 带 `errors: string[]`（additive）；
 *   - 文本输出置顶 `⚠ adapter root 不可读: <id>`；
 *   - root 不可读的 adapter 判定时 local 视作 = base（零漂移，不产生假 push 计数）；
 *   - exit 码仍为 0（漂移 / 告警都是信息）；`homer init` 也打印明显提示。
 *
 * 隔离：`homer init` 走内置 DEFAULT_PI_ADAPTER（root `~/.pi/agent`），
 * 因此 init 相关用例把 `HOME` 指到临时目录；status/diff 用例直接给 homer.json 指定临时 root。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { run } from '../../src/cli/index.js';
import { runStatus, type StatusReport } from '../../src/cli/commands/status.js';
import type { HomerConfig } from '../../src/core/types.js';
import { writeSnapshotToStore } from '../../src/core/store/store.js';
import { getHomerPaths } from '../../src/core/paths.js';

let tmp: string;
let home: string;
let agentRoot: string;
let fakeHome: string;
let savedHome: string | undefined;
let savedHomerHome: string | undefined;

beforeEach(() => {
  tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-missing-root-'));
  home = path.join(tmp, 'homer');
  agentRoot = path.join(tmp, 'pi-agent');
  fakeHome = path.join(tmp, 'fake-home');
  fs.mkdirSync(home, { recursive: true });
  fs.mkdirSync(fakeHome, { recursive: true });

  savedHome = process.env['HOME'];
  savedHomerHome = process.env['HOMER_HOME'];
});

afterEach(() => {
  if (savedHome === undefined) delete process.env['HOME'];
  else process.env['HOME'] = savedHome;
  if (savedHomerHome === undefined) delete process.env['HOMER_HOME'];
  else process.env['HOMER_HOME'] = savedHomerHome;
  fs.rmSync(tmp, { recursive: true, force: true });
});

/** 写一个含 settings(merge) + skills(mirror) 的 homer.json（root 指向 agentRoot）。 */
function writeConfig(withRoot?: string): HomerConfig {
  const config: HomerConfig = {
    version: 1,
    adapters: {
      pi: {
        root: withRoot ?? agentRoot,
        enabled: true,
        categories: {
          settings: { paths: ['settings.json'], mode: 'merge' },
          skills: { paths: ['skills/'], mode: 'mirror' },
        },
      },
    },
  };
  fs.writeFileSync(path.join(home, 'homer.json'), `${JSON.stringify(config, null, 2)}\n`, 'utf8');
  return config;
}

/** 建 root + 文件。 */
function buildRoot(): void {
  fs.mkdirSync(path.join(agentRoot, 'skills'), { recursive: true });
  fs.writeFileSync(path.join(agentRoot, 'settings.json'), '{"theme":"light"}', 'utf8');
  fs.writeFileSync(path.join(agentRoot, 'skills', 'a.md'), 'A\n', 'utf8');
  fs.writeFileSync(path.join(agentRoot, 'skills', 'b.md'), 'B\n', 'utf8');
}

/** 手工把 root 内容写进 store（含 M-C 完整性标记）。 */
function seedStore(): void {
  const paths = getHomerPaths({ HOMER_HOME: home });
  writeSnapshotToStore(paths, {
    adapterId: 'pi',
    categories: [
      {
        adapterId: 'pi',
        category: 'settings',
        mode: 'merge',
        files: new Map([['settings.json', { kind: 'json', content: '{"theme":"light"}' }]]),
      },
      {
        adapterId: 'pi',
        category: 'skills',
        mode: 'mirror',
        files: new Map([
          ['a.md', { kind: 'file', content: 'A\n' }],
          ['b.md', { kind: 'file', content: 'B\n' }],
        ]),
      },
    ],
  });
}

/** 让 `homer init` 扫描 `<fakeHome>/.pi/agent`（HOME 驱动的 DEFAULT_PI_ADAPTER）。 */
function useFakeHome(): string {
  process.env['HOME'] = fakeHome;
  return path.join(fakeHome, '.pi', 'agent');
}

async function cli(args: string[]): Promise<{ code: number; out: string; err: string }> {
  const out: string[] = [];
  const err: string[] = [];
  const code = await run(args, { out: (l) => out.push(l), err: (l) => err.push(l) });
  return { code, out: out.join('\n'), err: err.join('\n') };
}

describe('M-A: runStatus 对不可读 root 的处理', () => {
  it('root 缺失 → errors 非空，且不产生假 push（local 视作 base）', () => {
    writeConfig();
    seedStore();
    expect(fs.existsSync(agentRoot)).toBe(false);

    const report = runStatus({ homerHome: home });

    expect(report.errors).toHaveLength(1);
    expect(report.errors[0]).toMatch(/adapter root 不可读: pi/);
    expect(report.adapters[0]).toMatchObject({ id: 'pi', push: 0, pull: 0, conflicts: 0 });
  });

  it('root 缺失 → 文本输出含 ⚠ 头行，且不是「假 ↑N」', async () => {
    writeConfig();
    seedStore();

    const result = await cli(['status', '--home', home]);

    expect(result.code).toBe(0); // 漂移/告警都是信息
    expect(result.out).toMatch(/⚠ adapter root 不可读: pi/);
    expect(result.out).toContain('pi  ↑0 ↓0');
    expect(result.out).not.toMatch(/↑[1-9]/); // 修复前会报 ↑3
    expect(result.out).toContain('无漂移');
  });

  it('root 缺失 → status --json 含 errors 数组，计数仍为 0', async () => {
    writeConfig();
    seedStore();

    const result = await cli(['status', '--json', '--home', home]);
    expect(result.code).toBe(0);
    const parsed = JSON.parse(result.out) as StatusReport;
    expect(Array.isArray(parsed.errors)).toBe(true);
    expect(parsed.errors[0]).toMatch(/adapter root 不可读/);
    expect(parsed.adapters[0]).toMatchObject({ push: 0, pull: 0, conflicts: 0 });
  });

  it('root 从有到无 → status 从 ↑0 变 ⚠ 而不是 ↑N', () => {
    writeConfig();
    buildRoot();
    seedStore();

    const before = runStatus({ homerHome: home });
    expect(before.errors).toEqual([]);
    expect(before.adapters[0]).toMatchObject({ push: 0, pull: 0 });

    fs.rmSync(agentRoot, { recursive: true, force: true });

    const after = runStatus({ homerHome: home });
    expect(after.errors[0]).toMatch(/adapter root 不可读: pi/);
    expect(after.adapters[0]).toMatchObject({ push: 0, pull: 0, conflicts: 0 });
  });

  it('root 可读时 errors 为空（不误报告警）', () => {
    writeConfig();
    buildRoot();
    const report = runStatus({ homerHome: home });
    expect(report.errors).toEqual([]);
  });

  it('注入 sources（单测路径）时 errors 缺省为空数组，不抛错', () => {
    writeConfig();
    const report = runStatus({ homerHome: home }, { base: [], local: [] });
    expect(report.errors).toEqual([]);
    expect(report.adapters).toEqual([]);
  });
});

describe('M-A: homer diff 对不可读 root 的处理', () => {
  it('root 缺失 → diff 输出 ⚠ 行（不静默输出空，避免被当成「无漂移」）', async () => {
    writeConfig();
    seedStore();

    const result = await cli(['diff', '--home', home]);
    expect(result.code).toBe(0);
    expect(result.out).toMatch(/⚠ adapter root 不可读: pi/);
  });
});

describe('M-A: homer init 对不可读 root 的处理（HOME 隔离）', () => {
  it('root 缺失 → init 仍 exit 0，但文本带 ⚠ 提示', async () => {
    const fakeAgentRoot = useFakeHome();
    expect(fs.existsSync(fakeAgentRoot)).toBe(false);

    const result = await cli(['init', '--home', home]);
    expect(result.code).toBe(0);
    expect(result.out).toMatch(/⚠ adapter root 不可读: pi/);
  });

  it('root 缺失 → init --json 的 errors 非空', async () => {
    useFakeHome();

    const result = await cli(['init', '--json', '--home', home]);
    const report = JSON.parse(result.out) as { errors: string[]; adapters: unknown[] };
    expect(report.errors).toHaveLength(1);
    expect(report.errors[0]).toMatch(/adapter root 不可读/);
  });

  it('root 可读 → init 报告 errors 为空', async () => {
    const fakeAgentRoot = useFakeHome();
    fs.mkdirSync(path.join(fakeAgentRoot, 'skills'), { recursive: true });
    fs.writeFileSync(path.join(fakeAgentRoot, 'settings.json'), '{"theme":"light"}', 'utf8');
    fs.writeFileSync(path.join(fakeAgentRoot, 'skills', 'a.md'), 'A\n', 'utf8');

    const result = await cli(['init', '--json', '--home', home]);
    const report = JSON.parse(result.out) as { errors: string[] };
    expect(report.errors).toEqual([]);
  });

  it('root 内含逃逸 symlink → init 带 ⚠（不静默把外部内容算进快照）', async () => {
    const fakeAgentRoot = useFakeHome();
    const outside = path.join(tmp, 'outside');
    fs.mkdirSync(path.join(fakeAgentRoot, 'skills'), { recursive: true });
    fs.mkdirSync(outside, { recursive: true });
    fs.writeFileSync(path.join(outside, 'secret.md'), 'SECRET\n', 'utf8');
    fs.writeFileSync(path.join(fakeAgentRoot, 'skills', 'a.md'), 'A\n', 'utf8');
    fs.symlinkSync(outside, path.join(fakeAgentRoot, 'skills', 'escape'));

    const result = await cli(['init', '--json', '--home', home]);
    const report = JSON.parse(result.out) as { errors: string[] };
    expect(report.errors.length).toBeGreaterThan(0);
    expect(fs.readFileSync(path.join(home, 'store', 'pi', 'skills', 'a.md'), 'utf8')).toBe('A\n');
  });
});
