/**
 * M1 里程碑级 e2e 验收（docs/m1-plan.md §2-P2 / §4）。
 *
 * 全程临时目录，绝不碰真实 `~/.pi` / `~/.homer`：
 *   <tmp>/home/.pi/agent   ← 假的 HOME（pi adapter root 靠 '~' 展开到这里）
 *   <tmp>/homer            ← HOMER_HOME
 *
 * 覆盖链路：pi fixture → homer init →（store / homer.json 断言）→ 制造漂移
 *          → homer status（精确计数）→ homer status --json（parse + 计数一致）
 *          → homer diff（键的新旧值 / 无漂移分类空输出）。
 *
 * 本文件用 `run()` 在进程内驱动 CLI（与 tests/cli/** 同款），另有一个用例用真实子进程
 * 跑 CLI 入口（argv 解析 + 退出码 + stdout JSON），保证「进程级」约定也被覆盖。
 */

import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { run } from '../../src/cli/index.js';
import { validateConfig } from '../../src/core/config.js';
import { computeDrift, type CategoryDrift } from '../../src/core/engine/index.js';
import type { AdapterSnapshot } from '../../src/core/types.js';
import type { StatusReport } from '../../src/cli/commands/status.js';

/* ------------------------------------------------------------------ */
/* fixture                                                             */
/* ------------------------------------------------------------------ */

/** 7 分类各有代表文件；外加 ignore 规则应吃掉的全部垃圾。 */
function buildPiFixture(agentRoot: string): void {
  for (const dir of [
    'skills/alpha',
    'skills/beta',
    'extensions',
    'agents',
    'prompts',
    'themes',
    'sessions',
    'npm',
  ]) {
    mkdirSync(path.join(agentRoot, dir), { recursive: true });
  }

  const write = (rel: string, content: string): void => {
    mkdirSync(path.dirname(path.join(agentRoot, rel)), { recursive: true });
    writeFileSync(path.join(agentRoot, rel), content, 'utf8');
  };

  // 7 分类
  write('settings.json', `${JSON.stringify({ theme: 'light', keep: 1 }, null, 2)}\n`);
  write('keybindings.json', `${JSON.stringify({ keys: ['ctrl+k'] }, null, 2)}\n`);
  write('skills/alpha/SKILL.md', '# alpha\n');
  write('skills/beta/SKILL.md', '# beta\n');
  write('extensions/tool.ts', 'export const x = 1;\n');
  write('extensions/tool-cache.json', '{"cached":true}\n'); // category exclude '*cache*'
  write('agents/scout.md', '# scout\n');
  write('prompts/review.md', '# review\n');
  write('themes/dark.json', `${JSON.stringify({ name: 'dark' })}\n`);
  write('models.json', `${JSON.stringify({ apiKeys: { openai: 'sk-a' }, models: { openai: { id: 'gpt' } } })}\n`);

  // 垃圾（adapter 级 ignore / exclude / 「不在任何分类 path 内」必须全部拦掉）
  // minor 8：内容对齐真实 ~/.pi/agent 的垃圾形态
  write('auth.json', '{"token":"secret"}\n');
  write('trust.json', '{"trusted":[]}\n');
  write('sessions/s1.jsonl', '{"turn":1}\n');
  write('npm/n1.json', '{}\n');
  write('settings.json.bak-predirect', '{}\n');
  write('models.json.bak2', '{"STALE":"bak2"}\n'); // `.bak*` 命中
  write('pi-tui-crash.log', 'boom\n');
  write('pi-tui-debug.log', 'debug\n');
  write('run-history.jsonl', '{}\n');
  write('settings.json.bak-defaultmodel-20260921-052734', '{}\n'); // `.bak-*` 命中
  // 以下不在任何分类声明的 path 内（不应被扫进任何 category）
  write('AGENTS.md', '# agents instructions\n');
  write('cursor-sdk-model-list.json', '{"models":[]}\n');
  write('cursor-sdk-context-windows.json', '{"windows":[]}\n');
  write('extensions-removed/tps.ts', 'export const tps = 1;\n');
  write('extensions-removed/tps.ts.bak-20260916-132621', 'export const old = 1;\n');
}

/**
 * 递归列出目录下所有文件（相对该目录，posix 分隔符），排序。
 * store 的完整性标记 `.homer-complete` 是内部元数据（M-C），从清单里滤掉，
 * 让各用例的断言聚焦在「快照内容」上。
 */
function listFiles(dir: string, prefix = ''): string[] {
  if (!existsSync(dir)) return [];
  return readdirSync(dir, { withFileTypes: true })
    .filter((entry) => entry.name !== '.homer-complete')
    .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))
    .flatMap((entry) =>
      entry.isDirectory()
        ? listFiles(path.join(dir, entry.name), `${prefix}${entry.name}/`)
        : [`${prefix}${entry.name}`],
    );
}

/** init 后 store 中应当出现的精确文件清单（§1.6 布局，硬编码断言）。 */
const EXPECTED_STORE_FILES = [
  'pi/agents/scout.md',
  'pi/extensions/tool.ts',
  'pi/models/models.json',
  'pi/prompts/review.md',
  'pi/settings/keybindings.json',
  'pi/settings/settings.json',
  'pi/skills/alpha/SKILL.md',
  'pi/skills/beta/SKILL.md',
  'pi/themes/dark.json',
];

const EXPECTED_CATEGORIES = ['settings', 'skills', 'extensions', 'agents', 'models', 'prompts', 'themes'];

/* ------------------------------------------------------------------ */
/* harness                                                             */
/* ------------------------------------------------------------------ */

interface Harness {
  tmp: string;
  fakeHome: string;
  agentRoot: string;
  homerHome: string;
}

let h: Harness;
let savedHome: string | undefined;
let savedHomerHome: string | undefined;

function setupHarness(opts: { withPi: boolean } = { withPi: true }): void {
  const tmp = mkdtempSync(path.join(tmpdir(), 'homer-e2e-'));
  const fakeHome = path.join(tmp, 'home');
  const agentRoot = path.join(fakeHome, '.pi', 'agent');
  const homerHome = path.join(tmp, 'homer');
  mkdirSync(fakeHome, { recursive: true });
  if (opts.withPi) buildPiFixture(agentRoot);

  h = { tmp, fakeHome, agentRoot, homerHome };
  savedHome = process.env.HOME;
  savedHomerHome = process.env.HOMER_HOME;
  process.env.HOME = fakeHome;
  process.env.HOMER_HOME = homerHome;
}

/** 驱动 CLI（进程内），捕获 stdout / stderr / 退出码。 */
async function cli(args: string[]): Promise<{ code: number; out: string; err: string }> {
  const out: string[] = [];
  const err: string[] = [];
  const code = await run(args, { out: (l) => out.push(l), err: (l) => err.push(l) });
  return { code, out: out.join('\n'), err: err.join('\n') };
}

async function initCli(): Promise<void> {
  // M3-P4-W9 起 `init` 默认注册 pi + herdr + opencode（见 tests/e2e/m3.test.ts）。
  // 本文件的断言全部针对 **pi adapter 的扫描语义**，故显式限定 `--adapters pi`，保持 7 分类口径。
  const result = await cli(['init', '--json', '--adapters', 'pi']);
  expect(result.code).toBe(0);
  expect(result.err).toBe('');
}

async function statusJson(): Promise<StatusReport> {
  const result = await cli(['status', '--json']);
  expect(result.code).toBe(0);
  return JSON.parse(result.out) as StatusReport;
}

function categoriesOf(report: StatusReport): Record<string, { push: number; pull: number; conflicts: number }> {
  const adapter = report.adapters[0];
  if (adapter === undefined) throw new Error('status 报告里没有 adapter');
  return Object.fromEntries(adapter.categories.map((c) => [c.name, { push: c.push, pull: c.pull, conflicts: c.conflicts }]));
}

beforeEach(() => {
  setupHarness();
});

afterEach(() => {
  if (savedHome === undefined) delete process.env.HOME;
  else process.env.HOME = savedHome;
  if (savedHomerHome === undefined) delete process.env.HOMER_HOME;
  else process.env.HOMER_HOME = savedHomerHome;
  rmSync(h.tmp, { recursive: true, force: true });
});

/* ------------------------------------------------------------------ */
/* 1. init                                                             */
/* ------------------------------------------------------------------ */

describe('e2e: homer init（临时 HOME + HOMER_HOME）', () => {
  it('store 出现 7 分类文件（§1.6 布局精确匹配），ignore 垃圾 0 条', async () => {
    await initCli();

    expect(listFiles(path.join(h.homerHome, 'store'))).toEqual(EXPECTED_STORE_FILES);

    // 7 个分类目录齐全（含 extensions 的 exclude 生效后仍保留目录）
    const piStore = path.join(h.homerHome, 'store', 'pi');
    expect(
      readdirSync(piStore, { withFileTypes: true })
        .filter((e) => e.isDirectory())
        .map((e) => e.name)
        .sort(),
    ).toEqual([...EXPECTED_CATEGORIES].sort());

    // 硬编码绝对断言：目录型 category 的 relPath 是「目录内相对路径」
    expect(readFileSync(path.join(piStore, 'skills/alpha/SKILL.md'), 'utf8')).toBe('# alpha\n');
    expect(readFileSync(path.join(piStore, 'settings/settings.json'), 'utf8')).toContain('"theme": "light"');
    expect(readFileSync(path.join(piStore, 'models/models.json'), 'utf8')).toContain('"apiKeys"');

    // 垃圾一条都不许进 store
    const storeRoot = path.join(h.homerHome, 'store');
    for (const forbidden of [
      'pi/auth.json',
      'pi/trust.json',
      'pi/sessions',
      'pi/npm',
      'pi/settings/settings.json.bak-predirect',
      'pi/settings/settings.json.bak-defaultmodel-20260921-052734',
      'pi/pi-tui-crash.log',
      'pi/pi-tui-debug.log',
      'pi/run-history.jsonl',
      'pi/models/models.json.bak2',
      'pi/extensions/tool-cache.json',
      // minor 8：不在任何分类 path 内的文件同样不得进 store
      'pi/AGENTS.md',
      'pi/cursor-sdk-model-list.json',
      'pi/cursor-sdk-context-windows.json',
      'pi/extensions-removed/tps.ts',
    ]) {
      expect(existsSync(path.join(storeRoot, forbidden)), `${forbidden} 不应出现在 store`).toBe(false);
    }
    expect(listFiles(storeRoot).some((f) => /\.bak|\.log$|run-history\.jsonl|tool-cache|cursor-sdk|AGENTS\.md|extensions-removed/.test(f))).toBe(false);
  });

  it('homer.json 合法：version 1 / root 保留 ~ 写法 / 7 分类与默认配置逐字一致', async () => {
    await initCli();

    const raw = readFileSync(path.join(h.homerHome, 'homer.json'), 'utf8');
    const parsed = JSON.parse(raw) as unknown;

    const validated = validateConfig(parsed);
    expect(validated.ok).toBe(true);

    const config = (parsed as { adapters: Record<string, { root: string; enabled: boolean; categories: Record<string, unknown> }> });
    expect((parsed as { version: number }).version).toBe(1);
    // `--adapters pi` 限定（三 adapter 全注册由 tests/e2e/m3.test.ts 覆盖）
    expect(Object.keys(config.adapters)).toEqual(['pi']);
    expect(config.adapters['pi']?.root).toBe('~/.pi/agent');
    expect(config.adapters['pi']?.enabled).toBe(true);
    expect(Object.keys(config.adapters['pi']?.categories ?? {})).toEqual(EXPECTED_CATEGORIES);
  });

  it('init 后立刻 status → 无漂移（↑0 ↓0）', async () => {
    await initCli();

    const report = await statusJson();
    expect(report.adapters).toHaveLength(1);
    expect(report.adapters[0]).toMatchObject({ id: 'pi', push: 0, pull: 0, conflicts: 0 });
    expect(Object.values(categoriesOf(report)).every((c) => c.push === 0 && c.pull === 0 && c.conflicts === 0)).toBe(true);

    const text = await cli(['status']);
    expect(text.out).toContain('pi  ↑0 ↓0');
    expect(text.out).toContain('无漂移');

    // 无漂移 → diff 空输出
    expect((await cli(['diff'])).out).toBe('');
  });
});

/* ------------------------------------------------------------------ */
/* 2-4. 漂移 → status / status --json / diff                           */
/* ------------------------------------------------------------------ */

/**
 * 制造 4 处漂移（每处 1 个 push 计数）：
 *   settings.json 改一个键（theme）        → settings ↑1（merge 键级）
 *   新增 skills/gamma/SKILL.md             → skills ↑1（mirror 新增）
 *   删除 skills/beta/ 整个目录             → skills ↑1（mirror push-delete）
 *   直改 store 中 themes/dark.json         → themes ↑1（M1: remote 缺省 = base，故记 push）
 * 另有 models.json 只改 excludeKeys 里的 apiKeys → 必须不产生任何漂移。
 */
function introduceDrift(hh: Harness): void {
  writeFileSync(
    path.join(hh.agentRoot, 'settings.json'),
    `${JSON.stringify({ theme: 'dark', keep: 1 }, null, 2)}\n`,
    'utf8',
  );
  mkdirSync(path.join(hh.agentRoot, 'skills', 'gamma'), { recursive: true });
  writeFileSync(path.join(hh.agentRoot, 'skills', 'gamma', 'SKILL.md'), '# gamma\n', 'utf8');
  rmSync(path.join(hh.agentRoot, 'skills', 'beta'), { recursive: true, force: true });
  writeFileSync(
    path.join(hh.homerHome, 'store', 'pi', 'themes', 'dark.json'),
    `${JSON.stringify({ name: 'dark-edited-in-store' })}\n`,
    'utf8',
  );
  writeFileSync(
    path.join(hh.agentRoot, 'models.json'),
    `${JSON.stringify({ apiKeys: { openai: 'sk-CHANGED' }, models: { openai: { id: 'gpt' } } })}\n`,
    'utf8',
  );
}

describe('e2e: 漂移 → status / diff', () => {
  beforeEach(async () => {
    await initCli();
    introduceDrift(h);
  });

  it('status 精确计数：settings ↑1、skills ↑2、themes ↑1，合计 ↑4 ↓0，无冲突', async () => {
    const report = await statusJson();

    expect(report.adapters).toHaveLength(1);
    expect(report.adapters[0]).toMatchObject({ id: 'pi', push: 4, pull: 0, conflicts: 0 });
    expect(categoriesOf(report)).toEqual({
      settings: { push: 1, pull: 0, conflicts: 0 },
      skills: { push: 2, pull: 0, conflicts: 0 },
      extensions: { push: 0, pull: 0, conflicts: 0 },
      agents: { push: 0, pull: 0, conflicts: 0 },
      models: { push: 0, pull: 0, conflicts: 0 }, // 只改 apiKeys（excludeKeys）→ 不算漂移
      prompts: { push: 0, pull: 0, conflicts: 0 },
      themes: { push: 1, pull: 0, conflicts: 0 },
    });

    // 人类可读输出与 JSON 计数一致
    const text = await cli(['status', '--verbose']);
    expect(text.code).toBe(0);
    expect(text.out).toContain('pi  ↑4 ↓0');
    expect(text.out).toContain('settings  ↑1 ↓0');
    expect(text.out).toContain('skills  ↑2 ↓0');
    expect(text.out).toContain('themes  ↑1 ↓0');
    expect(text.out).not.toContain('无漂移');
  });

  it('status --json 可 JSON.parse 且计数与文本一致（--json 只输出 JSON）', async () => {
    const jsonResult = await cli(['status', '--json']);
    expect(jsonResult.code).toBe(0);

    const parsed = JSON.parse(jsonResult.out) as StatusReport;
    expect(parsed.adapters[0]?.push).toBe(4);
    expect(parsed.adapters[0]?.pull).toBe(0);
    expect(parsed.adapters[0]?.categories.map((c) => c.name)).toEqual(EXPECTED_CATEGORIES);

    const fromText = await cli(['status', '--verbose']);
    for (const category of parsed.adapters[0]?.categories ?? []) {
      expect(fromText.out).toContain(`${category.name}  ↑${category.push} ↓${category.pull}`);
    }
  });

  it('diff 含 settings 键的新旧值（`theme: light → dark`）', async () => {
    const result = await cli(['diff']);
    expect(result.code).toBe(0);
    expect(result.out).toContain('pi/settings');
    expect(result.out).toContain('theme: light → dark');

    // merge 分类键行只该有 theme 一个键
    const keyLines = result.out.split('\n').filter((line) => /^\s{4}\S.*→/.test(line));
    expect(keyLines).toEqual(['    theme: light → dark']);
  });

  it('diff 覆盖 skills（删除 + 新增）与 themes（store 侧被改）', async () => {
    const result = await cli(['diff']);

    expect(result.out).toContain('pi/skills');
    expect(result.out).toContain('beta/SKILL.md');
    expect(result.out).toContain('-# beta');
    expect(result.out).toContain('gamma/SKILL.md');
    expect(result.out).toContain('+# gamma');

    expect(result.out).toContain('pi/themes');
    // minor 8：行级精确匹配（不再用 toContain 比子串）
    expect(result.out.split('\n')).toContain('-{"name":"dark-edited-in-store"}');
    expect(result.out.split('\n')).toContain('+{"name":"dark"}');
  });

  it('diff 对无漂移分类输出空（prompts / extensions / agents / models）', async () => {
    for (const category of ['prompts', 'extensions', 'agents', 'models']) {
      const result = await cli(['diff', '--category', category]);
      expect(result.code).toBe(0);
      expect(result.out, `${category} 无漂移应输出空`).toBe('');
    }
    // 无漂移分类的 status 计数同样为 0
    const report = await statusJson();
    expect(categoriesOf(report)['models']).toEqual({ push: 0, pull: 0, conflicts: 0 });
  });

  it('diff --adapter / --category 过滤生效', async () => {
    expect((await cli(['diff', '--category', 'settings'])).out).not.toContain('pi/skills');
    expect((await cli(['diff', '--adapter', 'pi'])).out).toContain('pi/settings');
    expect((await cli(['diff', '--adapter', 'nope'])).out).toBe('');
  });
});

/* ------------------------------------------------------------------ */
/* 5. 缝合点回归：init 时缺失的 merge 文件之后被新增                     */
/* ------------------------------------------------------------------ */

/**
 * 引擎级回归（base 缺失时 merge 分类的文件级三态）。
 * 修复前的 bug：`r === undefined` 分支先于 base 缺失分支，导致「init 时文件不存在、
 * 之后本地新增」（remote 缺省 = base，同样不存在）被误判成 modify-vs-delete 冲突。
 */
describe('e2e: 引擎回归 — base 缺失时 merge 分类文件级三态', () => {
  const json = (value: unknown): { kind: 'json'; content: string } => ({ kind: 'json', content: JSON.stringify(value) });
  const file = (content: string): { kind: 'file'; content: string } => ({ kind: 'file', content });
  const cat = (files: Record<string, { kind: 'json' | 'file'; content: string }>): AdapterSnapshot => ({
    adapterId: 'pi',
    categories: [{ adapterId: 'pi', category: 'settings', mode: 'merge', files: new Map(Object.entries(files)) }],
  });
  const only = (drift: ReturnType<typeof computeDrift>): CategoryDrift => drift[0]!.categories[0]!;

  it('base 缺失 + 仅 local 有 → push 1、无冲突', () => {
    const drift = only(computeDrift([cat({})], [cat({ 'settings.json': json({ theme: 'dark' }) })]));
    expect(drift).toMatchObject({ push: 1, pull: 0, conflicts: 0 });
    expect(drift.mergeConflicts).toEqual([]);
  });

  it('base 缺失 + 仅 remote 有 → pull 1、无冲突', () => {
    const drift = only(
      computeDrift([cat({})], [cat({})], [cat({ 'settings.json': json({ theme: 'dark' }) })]),
    );
    expect(drift).toMatchObject({ push: 0, pull: 1, conflicts: 0 });
    expect(drift.mergeConflicts).toEqual([]);
  });

  it('base 缺失 + 两侧都有且内容不同 → both-modified 冲突', () => {
    const drift = only(
      computeDrift(
        [cat({})],
        [cat({ 'settings.json': json({ theme: 'dark' }) })],
        [cat({ 'settings.json': json({ theme: 'light' }) })],
      ),
    );
    expect(drift.conflicts).toBe(1);
    expect(drift.mergeConflicts).toContainEqual({ keyPath: 'settings.json', reason: 'both-modified' });
  });

  it('base 缺失 + 两侧都有但内容相同 → 不算冲突（push 仍记 local 新增）', () => {
    const drift = only(
      computeDrift(
        [cat({})],
        [cat({ 'settings.json': json({ theme: 'dark' }) })],
        [cat({ 'settings.json': json({ theme: 'dark' }) })],
      ),
    );
    // base 缺失 → local 相对空 base 确实是待 push 的新增；内容相同不构成冲突。
    expect(drift).toMatchObject({ push: 1, pull: 0, conflicts: 0 });
    expect(drift.mergeConflicts).toEqual([]);
  });

  it('base 存在而 remote 删除文件、local 未改 → pull 1（不误报冲突）', () => {
    const drift = only(
      computeDrift(
        [cat({ 'settings.json': json({ a: 1 }) })],
        [cat({ 'settings.json': json({ a: 1 }) })],
        [cat({})],
      ),
    );
    expect(drift).toMatchObject({ push: 0, pull: 1, conflicts: 0 });
  });

  it('降级（非 JSON）条目的 base 缺失分支仍是 push，不炸', () => {
    const drift = only(computeDrift([cat({})], [cat({ 'notes.txt': file('改过') })]));
    expect(drift).toMatchObject({ push: 1, pull: 0, conflicts: 0 });
    expect(drift.ops).toEqual([{ type: 'push', path: 'notes.txt' }]);
  });
});

describe('e2e: 缝合点回归（store 缺目录 + merge 分类新增文件）', () => {
  it('init 时无 settings.json，之后本地新增 → 只算 push 1，不误报 modify-vs-delete 冲突', async () => {
    rmSync(path.join(h.agentRoot, 'settings.json'), { force: true });
    rmSync(path.join(h.agentRoot, 'keybindings.json'), { force: true });
    await initCli();

    // store 里 settings 分类目录存在但为空（readSnapshotFromStore 返回空 Map）
    expect(existsSync(path.join(h.homerHome, 'store', 'pi', 'settings'))).toBe(true);
    expect(listFiles(path.join(h.homerHome, 'store', 'pi', 'settings'))).toEqual([]);

    writeFileSync(path.join(h.agentRoot, 'settings.json'), `${JSON.stringify({ theme: 'dark' }, null, 2)}\n`, 'utf8');

    const report = await statusJson();
    expect(categoriesOf(report)['settings']).toEqual({ push: 1, pull: 0, conflicts: 0 });
    expect(report.adapters[0]).toMatchObject({ push: 1, pull: 0, conflicts: 0 });

    const diff = await cli(['diff', '--category', 'settings']);
    expect(diff.out).toContain('$: (无) → {"theme":"dark"}');
  });

  it('pi root 不存在时 init 仍 exit 0，且报告/文本显式 ⚠ 提示（M-A）', async () => {
    rmSync(h.agentRoot, { recursive: true, force: true });

    const init = await cli(['init', '--json', '--adapters', 'pi']);
    expect(init.code).toBe(0);
    const report = JSON.parse(init.out) as { homerHome: string; adapters: { categories: unknown[] }[]; errors: string[] };
    expect(report.homerHome).toBe(h.homerHome);
    expect(report.adapters[0]?.categories).toEqual([]);
    // M-A：不再静默——root 不可读必须在报告里可见
    expect(report.errors[0]).toMatch(/adapter root 不可读: pi/);

    const initText = await cli(['init', '--force', '--adapters', 'pi']);
    expect(initText.out).toMatch(/⚠ adapter root 不可读: pi/);

    // M-A：随后 status 必须报 ⚠（不产生假 push），计数全零
    const status = await cli(['status', '--json']);
    const parsed = JSON.parse(status.out) as StatusReport;
    expect(parsed.errors[0]).toMatch(/adapter root 不可读: pi/);
    expect(Object.values(categoriesOf(parsed)).every((c) => c.push === 0 && c.pull === 0 && c.conflicts === 0)).toBe(true);

    const statusText = await cli(['status']);
    expect(statusText.out).toMatch(/⚠ adapter root 不可读: pi/);
    expect(statusText.out).not.toMatch(/↑[1-9]/);

    // diff 也不能静默输出空（会被脚本当成「无漂移」）
    expect((await cli(['diff'])).out).toMatch(/⚠ adapter root 不可读: pi/);

    expect(existsSync(path.join(h.homerHome, 'homer.json'))).toBe(true);
  });

  /**
   * M3-store 上报的计划歧义：readSnapshotFromStore 遇到「store 里缺某分类目录」时，
   * 是返回**空 Map 的分类条目**（M3 的选择）还是**跳过该分类**？
   *
   * 本用例以 e2e 实际行为裁定：删掉 store 里的 themes 目录后，status/diff 必须
   *   1) 仍列出 themes（7 分类结构完整，不因 store 缺目录而消失）；
   *   2) local 侧的主题文件被算成 push（base 视作空 = 新增），而不是静默漏报。
   * 结论：M3 的「空 Map」选择在 e2e 语义下是正确的。
   */
  it('store 缺分类目录 → 该分类仍出现且 local 内容计为 push（M3 空 Map 语义）', async () => {
    await initCli();
    rmSync(path.join(h.homerHome, 'store', 'pi', 'themes'), { recursive: true, force: true });

    const report = await statusJson();
    const categories = categoriesOf(report);
    expect(Object.keys(categories)).toEqual(EXPECTED_CATEGORIES);
    expect(categories['themes']).toEqual({ push: 1, pull: 0, conflicts: 0 });
    expect(report.adapters[0]).toMatchObject({ push: 1, pull: 0, conflicts: 0 });

    const diff = await cli(['diff', '--category', 'themes']);
    expect(diff.out).toContain('pi/themes');
    expect(diff.out).toContain('dark.json');
  });
});

/* ------------------------------------------------------------------ */
/* 6. 进程级入口（argv / 退出码 / stdout JSON）                         */
/* ------------------------------------------------------------------ */

describe('e2e: CLI 进程入口（node --import tsx src/cli/index.ts）', () => {
  function childEnv(overrides: NodeJS.ProcessEnv = {}): NodeJS.ProcessEnv {
    const env: NodeJS.ProcessEnv = { ...process.env, HOME: h.fakeHome, HOMER_HOME: h.homerHome, ...overrides };
    // 子进程必须不带 VITEST：src/cli/index.ts 据此跳过入口副作用（否则不会真正跑命令）。
    delete env['VITEST'];
    return env;
  }

  function spawnCli(args: string[]): string {
    const env = childEnv();
    return execFileSync(process.execPath, ['--import', 'tsx', 'src/cli/index.ts', ...args], {
      cwd: process.cwd(),
      env,
      encoding: 'utf8',
    });
  }

  it('init --json → 退出码 0 + stdout 合法 JSON；status --json → parseable', async () => {
    const initOut = spawnCli(['init', '--json', '--adapters', 'pi']);
    const initReport = JSON.parse(initOut) as { homerHome: string; adapters: { id: string }[] };
    expect(initReport.homerHome).toBe(h.homerHome);
    expect(initReport.adapters[0]?.id).toBe('pi');
    expect(listFiles(path.join(h.homerHome, 'store'))).toEqual(EXPECTED_STORE_FILES);

    const statusOut = spawnCli(['status', '--json']);
    const statusReport = JSON.parse(statusOut) as StatusReport;
    expect(statusReport.adapters[0]).toMatchObject({ id: 'pi', push: 0, pull: 0, conflicts: 0 });

    // 无 config 的目录 → 退出码 1（真错误）
    expect(() =>
      execFileSync(process.execPath, ['--import', 'tsx', 'src/cli/index.ts', 'status'], {
        cwd: process.cwd(),
        env: childEnv({ HOMER_HOME: path.join(h.tmp, 'nope') }),
        encoding: 'utf8',
        stdio: 'pipe',
      }),
    ).toThrowError();
  }, 60_000);
});
