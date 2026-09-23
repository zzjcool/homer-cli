/**
 * P3-W7 · `homer push` 验收（docs/m2-plan.md §2.8 / §3-P3 基线 + W7 专项）。
 *
 * 两组测试，边界划得很清：
 *
 *   1. **注入 deps.sources 的矩阵**（`runPush` 直调）：手工构造三方快照 +
 *      non-interactive port / `--yes`。绝不扫真实 `~/.pi`、不读真实 `~/.homer`
 *      （`--home` 一律指向 `mkdtemp` 临时目录）、不碰真实 remote
 *      （需要 git 的用例用本地 `git init --bare` 假 origin，纯文件系统路径，无网络）。
 *   2. **CLI 级**（`run()` 进程内分发）：覆盖 `--json` 可 `JSON.parse`、
 *      文本计数与 JSON 计数一致、退出码符合 §2.8 总表（pushed/no-drift → 0，其余 → 1）。
 *      CLI 级用例不能注入 sources（`index.ts` 的 dispatch 不传 deps，且 P3 禁改 index.ts），
 *      故它们的 local 来自真实 `scanAdapter`——root 指向 `mkdtemp` 的假 agent 目录。
 *
 * git 身份在**每个临时仓库**里显式设置（不依赖全局 ~/.gitconfig），
 * 所有临时目录 afterEach 统一清理。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { afterEach, describe, expect, it } from 'vitest';

import { run } from '../../src/cli/index.js';
import { renderPushReport, runPush, type PushReport } from '../../src/cli/commands/push.js';
import { gitExec } from '../../src/core/git/index.js';
import { getHomerPaths } from '../../src/core/paths.js';
import { loadState } from '../../src/core/state.js';
import type { PromptPort } from '../../src/cli/ui.js';
import type { AdapterSnapshot, SnapshotEntry } from '../../src/core/types.js';
import type { SyncSources } from '../../src/core/sync/types.js';
import { adapter, category, driftFixture, fileEntry, jsonEntry } from './helpers.js';

/* ------------------------------------------------------------------ */
/* 临时目录 + git harness                                              */
/* ------------------------------------------------------------------ */

const created: string[] = [];

function mkTmp(prefix: string): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), `homer-push-${prefix}-`));
  created.push(dir);
  return dir;
}

afterEach(() => {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) fs.rmSync(dir, { recursive: true, force: true });
  }
});

function gitOk(cwd: string, args: readonly string[]): string {
  const result = gitExec(cwd, args);
  if (!result.ok) throw new Error(`git ${args.join(' ')} 失败 (cwd=${cwd}): ${result.stderr}`);
  return result.stdout;
}

function gitFails(cwd: string, args: readonly string[]): void {
  expect(gitExec(cwd, args).ok).toBe(false);
}

function write(abs: string, content: string): void {
  fs.mkdirSync(path.dirname(abs), { recursive: true });
  fs.writeFileSync(abs, content, 'utf8');
}

/** 临时 `~/.homer`（可选 `git init -b main` + 假 origin + upstream）。 */
interface Home {
  home: string;
  paths: ReturnType<typeof getHomerPaths>;
  agentRoot: string;
  bare?: string;
}

function makeHome(opts: { git?: boolean; upstream?: boolean; root: string; excludeKeys?: string[]; ignorePaths?: string[] }): Home {
  const home = path.join(mkTmp('home'), 'homer');
  fs.mkdirSync(home, { recursive: true });

  const config: Record<string, unknown> = {
    version: 1,
    adapters: {
      pi: {
        root: opts.root,
        enabled: true,
        categories: {
          settings: {
            paths: ['settings.json'],
            mode: 'merge',
            ...(opts.excludeKeys === undefined ? {} : { excludeKeys: opts.excludeKeys }),
          },
          skills: { paths: ['skills/'], mode: 'mirror' },
        },
      },
    },
  };
  if (opts.ignorePaths !== undefined) config['secrets'] = { ignorePaths: opts.ignorePaths };
  write(path.join(home, 'homer.json'), `${JSON.stringify(config, null, 2)}\n`);

  const result: Home = { home, paths: getHomerPaths({ HOMER_HOME: home }), agentRoot: opts.root };

  if (opts.git === true) {
    gitOk(home, ['init', '-b', 'main']);
    gitOk(home, ['config', 'user.email', 'homer-push@example.invalid']);
    gitOk(home, ['config', 'user.name', 'Homer Push']);
  }

  if (opts.upstream === true) {
    const bare = path.join(mkTmp('origin'), 'origin.git');
    gitOk(path.dirname(bare), ['init', '--bare', '-b', 'main', bare]);
    gitOk(home, ['remote', 'add', 'origin', bare]);
    result.bare = bare;
  }

  return result;
}

/** 把 store 写成「最后一次同步态」（含完整性标记），并可选地落成 baseline commit + upstream。 */
function seedStore(h: Home, opts: { commit?: boolean } = {}): void {
  write(path.join(h.paths.storeDir, 'pi/settings/settings.json'), `${JSON.stringify({ theme: 'light', keep: 1 }, null, 2)}\n`);
  write(path.join(h.paths.storeDir, 'pi/skills/foo/SKILL.md'), '# foo\n');
  write(path.join(h.paths.storeDir, 'pi/skills/bar/SKILL.md'), '# bar\n');
  write(path.join(h.paths.storeDir, 'pi/.homer-complete'), '');

  if (opts.commit === true) {
    gitOk(h.home, ['add', '-A']);
    gitOk(h.home, ['commit', '-m', 'seed store']);
    gitOk(h.home, ['push', '-u', 'origin', 'main']);
  }
}

/** 假 agent root：与 seedStore 的内容一致（零漂移基线）。 */
function seedAgentRoot(h: Home): void {
  write(path.join(h.agentRoot, 'settings.json'), `${JSON.stringify({ theme: 'light', keep: 1 }, null, 2)}\n`);
  write(path.join(h.agentRoot, 'skills/foo/SKILL.md'), '# foo\n');
  write(path.join(h.agentRoot, 'skills/bar/SKILL.md'), '# bar\n');
}

/* ------------------------------------------------------------------ */
/* 注入快照的矩阵 fixture                                              */
/* ------------------------------------------------------------------ */

function sources(base: AdapterSnapshot[], local: AdapterSnapshot[], remote: AdapterSnapshot[]): SyncSources {
  return { mode: 'git', base, local, remote, baseCommit: 'a'.repeat(40), remoteRef: 'origin/main', warnings: [], errors: [] };
}

/** 记录 confirm / select 调用的假 port（confirm 恒为 false = 用户拒绝）。 */
function rejectingPort(): { port: PromptPort; confirms: { message: string; fallback: boolean }[] } {
  const confirms: { message: string; fallback: boolean }[] = [];
  const port: PromptPort = {
    async confirm(message, fallback): Promise<boolean> {
      confirms.push({ message, fallback });
      return false;
    },
    async select(_message, options, fallback) {
      return fallback ?? options[0]?.value ?? ('' as never);
    },
  };
  return { port, confirms };
}

/** `pi/settings/settings.json` 的三方快照（merge），skills 三侧一致（无漂移噪声）。 */
function conflictSources(kind: 'conflicts' | 'identical'): SyncSources {
  const skills = { 'foo.md': fileEntry('A\n') };
  const settings = kind === 'conflicts'
    ? {
        base: jsonEntry({ theme: 'light', keep: 1 }),
        local: jsonEntry({ theme: 'dark', keep: 1 }),
        remote: jsonEntry({ theme: 'blue', keep: 1 }),
      }
    : {
        base: jsonEntry({ theme: 'light' }),
        local: jsonEntry({ theme: 'light' }),
        remote: jsonEntry({ theme: 'light' }),
      };

  const build = (settingsEntry: SnapshotEntry): AdapterSnapshot[] => [
    adapter('pi', [
      category('pi', 'settings', 'merge', { 'settings.json': settingsEntry }),
      category('pi', 'skills', 'mirror', skills),
    ]),
  ];

  return sources(build(settings.base), build(settings.local), build(settings.remote));
}

/** ANSI 之外的纯文本断言辅助：报告 → 文本。 */
function textOf(report: PushReport): string {
  return renderPushReport(report);
}

/* ------------------------------------------------------------------ */
/* 1. 注入快照：拒绝路径（store 未被写入、无新 commit）                */
/* ------------------------------------------------------------------ */

describe('runPush（注入 deps.sources）：密钥扫描', () => {
  it('命中密钥 → secrets-rejected，store 未写入、无 commit、不建仓库', async () => {
    const root = path.join(mkTmp('agent'), 'agent');
    const h = makeHome({ root });
    const secret = 'sk-ant-api03-abcdefghijklmnopqrstuvwxyz';

    const local = [
      adapter('pi', [
        category('pi', 'settings', 'merge', { 'settings.json': rawJson(`{"apiKey":"${secret}"}`) }),
        category('pi', 'skills', 'mirror', { 'foo.md': fileEntry('# foo\n') }),
      ]),
    ];

    const report = await runPush(
      { homerHome: h.home, yes: true },
      { sources: sources(local, local, local) },
    );

    expect(report.status).toBe('secrets-rejected');
    expect(report.ok).toBe(false);
    expect(report.secrets).toHaveLength(1);
    expect(report.secrets[0]).toMatchObject({
      patternId: 'anthropic-api-key',
      path: 'pi/settings/settings.json',
      line: 1,
    });
    // 脱敏摘录：命中串首尾各留 4 字符，中段 *，原文不得出现。
    expect(report.secrets[0]?.excerpt).toContain('sk-a');
    expect(report.secrets[0]?.excerpt).not.toContain(secret);
    expect(report.errors.join(' ')).toContain('拒绝推送');

    // store 未被写入；没有 git 仓库 = ensureGitRepo 从未被调用（不产生 commit）。
    expect(fs.existsSync(path.join(h.paths.storeDir, 'pi'))).toBe(false);
    expect(fs.existsSync(path.join(h.home, '.git'))).toBe(false);
    expect(loadState(h.paths).lastSyncCommit).toBeUndefined();

    const text = textOf(report);
    expect(text).toContain('secrets-rejected');
    expect(text).toContain('pi/settings/settings.json:1');
    expect(text).toContain('[anthropic-api-key]');
  });

  it('命中密钥时有 git 仓库：HEAD / 假 origin 均无新 commit', async () => {
    const root = path.join(mkTmp('agent'), 'agent');
    const h = makeHome({ root, git: true, upstream: true });
    seedStore(h, { commit: true });
    seedAgentRoot(h);
    const headBefore = gitOk(h.home, ['rev-parse', 'HEAD']).trim();

    // 本地植入密钥（模拟「改 settings 时顺手贴了真 key」）
    write(path.join(root, 'settings.json'), `${JSON.stringify({ theme: 'dark', apiKey: 'sk-ant-api03-abcdefghijklmnopqrstuvwxyz' }, null, 2)}\n`);

    const report = await runPush(
      { homerHome: h.home, yes: true },
      {
        sources: sources(
          [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ theme: 'light' }) })])],
          [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': rawJson(`{"theme":"dark","apiKey":"sk-ant-api03-abcdefghijklmnopqrstuvwxyz"}`) })])],
          [adapter('pi', [category('pi', 'settings', 'merge', { 'settings.json': jsonEntry({ theme: 'light' }) })])],
        ),
      },
    );

    expect(report.status).toBe('secrets-rejected');
    expect(gitOk(h.home, ['rev-parse', 'HEAD']).trim()).toBe(headBefore);
    expect(gitOk(h.bare as string, ['rev-parse', 'HEAD']).trim()).toBe(headBefore);
    expect(gitOk(h.home, ['status', '--porcelain', '--', 'store/']).trim()).toBe('');
    expect(loadState(h.paths).lastSyncCommit).toBeUndefined();
  });

  it('secrets.ignorePaths 豁免命中路径 → 继续走 push 管线', async () => {
    const h = makeHome({ root: path.join(mkTmp('agent'), 'agent'), ignorePaths: ['pi/settings/'] });
    const secret = 'sk-ant-api03-abcdefghijklmnopqrstuvwxyz';
    const local = [
      adapter('pi', [
        category('pi', 'settings', 'merge', { 'settings.json': rawJson(`{"apiKey":"${secret}"}`) }),
      ]),
    ];

    const report = await runPush(
      { homerHome: h.home, yes: true },
      { sources: sources(local, local, local) },
    );

    // remote == base == local → 无漂移；关键是被豁免后不再判 secrets-rejected。
    expect(report.status).toBe('no-drift');
    expect(report.secrets).toEqual([]);
  });

  it('excludeKeys 的值在扫描前已换成 __REQUIRED__ 占位符，且写进 store 的是占位符', async () => {
    const h = makeHome({
      root: path.join(mkTmp('agent'), 'agent'),
      git: true,
      upstream: false,
      excludeKeys: ['apiKeys'],
    });
    const secret = 'sk-abcdefghijklmnopqrstuvwxyz012345';

    const local = [
      adapter('pi', [
        category('pi', 'settings', 'merge', {
          'settings.json': jsonEntry({ theme: 'dark', apiKeys: { openai: secret } }),
        }),
      ]),
    ];
    const base = [
      adapter('pi', [
        category('pi', 'settings', 'merge', {
          'settings.json': jsonEntry({ theme: 'light', apiKeys: { openai: secret } }),
        }),
      ]),
    ];

    const report = await runPush(
      { homerHome: h.home, yes: true, noPush: true },
      { sources: sources(base, local, local) },
    );

    expect(report.status).toBe('pushed');
    expect(report.secrets).toEqual([]);
    const written = fs.readFileSync(path.join(h.paths.storeDir, 'pi/settings/settings.json'), 'utf8');
    expect(written).toContain('__REQUIRED__');
    expect(written).not.toContain(secret);
    expect(written).toContain('dark');
  });
});

describe('runPush（注入 deps.sources）：checkPushSafety 拒绝路径', () => {
  it('remote-ahead → exit 1 语义（status=remote-ahead，提示 homer pull），store 未写', async () => {
    const h = makeHome({ root: path.join(mkTmp('agent'), 'agent') });
    const fixture = driftFixture('pull'); // base == local，remote 前进 → pull > 0

    const report = await runPush(
      { homerHome: h.home, yes: true },
      { sources: sources(fixture.base, fixture.local, fixture.remote) },
    );

    expect(report.status).toBe('remote-ahead');
    expect(report.ok).toBe(false);
    expect(report.errors.join(' ')).toContain('homer pull');
    expect(report.warnings.some((w) => w.includes('远端变更:'))).toBe(true);
    expect(fs.existsSync(path.join(h.paths.storeDir, 'pi'))).toBe(false);
    expect(fs.existsSync(path.join(h.home, '.git'))).toBe(false);
  });

  it('conflicts → exit 1 语义（status=conflicts，提示 homer merge，含 keyPaths）', async () => {
    const h = makeHome({ root: path.join(mkTmp('agent'), 'agent') });
    const src = conflictSources('conflicts');

    const report = await runPush({ homerHome: h.home, yes: true }, { sources: src });

    expect(report.status).toBe('conflicts');
    expect(report.errors.join(' ')).toContain('homer merge');
    expect(report.warnings).toContain('冲突: pi/settings/settings.json (merge-keys) [theme]');
    expect(fs.existsSync(path.join(h.paths.storeDir, 'pi'))).toBe(false);
  });
});

describe('runPush（注入 deps.sources）：no-drift / aborted', () => {
  it('三方一致 → no-drift（exit 0 语义），不写 store、不建仓库', async () => {
    const h = makeHome({ root: path.join(mkTmp('agent'), 'agent') });
    const src = conflictSources('identical');

    const report = await runPush({ homerHome: h.home, yes: true }, { sources: src });

    expect(report.status).toBe('no-drift');
    expect(report.ok).toBe(true);
    expect(report.changedFiles).toEqual([]);
    expect(report.commit).toBeUndefined();
    expect(report.pushedToRemote).toBe(false);
    expect(fs.existsSync(path.join(h.paths.storeDir, 'pi'))).toBe(false);
  });

  it('confirm=false → aborted（exit 1 语义），store 未写、无 commit', async () => {
    const h = makeHome({ root: path.join(mkTmp('agent'), 'agent') });
    const fixture = driftFixture('push');
    const { port, confirms } = rejectingPort();

    const report = await runPush(
      { homerHome: h.home }, // 无 --yes
      { ui: port, sources: sources(fixture.base, fixture.local, fixture.remote) },
    );

    expect(report.status).toBe('aborted');
    expect(report.ok).toBe(false);
    expect(confirms).toHaveLength(1);
    expect(confirms[0]?.fallback).toBe(false); // 安全默认：不确认
    expect(confirms[0]?.message).toContain(`3 个变更文件`); // driftFixture('push') → 3
    expect(report.changedFiles).toHaveLength(3);
    expect(fs.existsSync(path.join(h.paths.storeDir, 'pi'))).toBe(false);
    expect(fs.existsSync(path.join(h.home, '.git'))).toBe(false);
  });

  it('--yes 完全不创建 port（不提问，直接走默认策略）', async () => {
    const h = makeHome({ root: path.join(mkTmp('agent'), 'agent'), git: true });
    const fixture = driftFixture('push');
    const { port, confirms } = rejectingPort();

    const report = await runPush(
      { homerHome: h.home, yes: true, noPush: true },
      { ui: port, sources: sources(fixture.base, fixture.local, fixture.remote) },
    );

    expect(report.status).toBe('pushed');
    expect(confirms).toHaveLength(0);
  });
});

/* ------------------------------------------------------------------ */
/* 2. 注入快照：完整成功路径（临时 git 仓库 + 假 origin）              */
/* ------------------------------------------------------------------ */

describe('runPush：完整成功路径', () => {
  it('写入 store → commit → 推 upstream → state.lastSyncCommit == HEAD', async () => {
    const h = makeHome({ root: path.join(mkTmp('agent'), 'agent'), git: true, upstream: true });
    seedStore(h);
    gitOk(h.home, ['add', '-A']);
    gitOk(h.home, ['commit', '-m', 'seed']);
    gitOk(h.home, ['push', '-u', 'origin', 'main']);
    const seedHead = gitOk(h.home, ['rev-parse', 'HEAD']).trim();

    const fixture = driftFixture('push');
    const report = await runPush(
      { homerHome: h.home, yes: true },
      { sources: sources(fixture.base, fixture.local, fixture.remote) },
    );

    expect(report.status).toBe('pushed');
    expect(report.ok).toBe(true);
    expect(report.changedFiles).toHaveLength(3);
    expect(report.pushedToRemote).toBe(true);
    expect(report.commit).toBeDefined();
    expect(report.commit).not.toBe(seedHead);
    expect(report.errors).toEqual([]);

    // store 落盘：snapshot 精确镜像（local 的增删都反映出来）
    expect(fs.readFileSync(path.join(h.paths.storeDir, 'pi/skills/new.md'), 'utf8')).toBe('N\n');
    expect(fs.existsSync(path.join(h.paths.storeDir, 'pi/skills/bar.md'))).toBe(false);
    expect(fs.readFileSync(path.join(h.paths.storeDir, 'pi/settings/settings.json'), 'utf8')).toContain('dark');

    // 新 commit 落在 seed 之上；假 origin 也拿到了它
    const head = gitOk(h.home, ['rev-parse', 'HEAD']).trim();
    expect(report.commit).toBe(head);
    expect(gitOk(h.bare as string, ['rev-parse', 'HEAD']).trim()).toBe(head);
    expect(gitOk(h.home, ['log', '-1', '--pretty=%s']).trim()).toContain('homer push');

    // state 更新
    const state = loadState(h.paths);
    expect(state.lastSyncCommit).toBe(head);
    expect(state.lastSyncCommand).toBe('push');
    expect(state.lastSyncAt).toMatch(/^\d{4}-\d{2}-\d{2}T/);

    // push 不写工具目录、不做备份
    expect(fs.existsSync(path.join(h.home, 'backups'))).toBe(false);
  });

  it('--no-push：本地 commit + state 更新，远端 HEAD 不变', async () => {
    const h = makeHome({ root: path.join(mkTmp('agent'), 'agent'), git: true, upstream: true });
    seedStore(h);
    gitOk(h.home, ['add', '-A']);
    gitOk(h.home, ['commit', '-m', 'seed']);
    gitOk(h.home, ['push', '-u', 'origin', 'main']);
    const seedHead = gitOk(h.home, ['rev-parse', 'HEAD']).trim();

    const fixture = driftFixture('push');
    const report = await runPush(
      { homerHome: h.home, yes: true, noPush: true },
      { sources: sources(fixture.base, fixture.local, fixture.remote) },
    );

    expect(report.status).toBe('pushed');
    expect(report.pushedToRemote).toBe(false);
    expect(report.commit).toBeDefined();
    expect(report.commit).not.toBe(seedHead);
    expect(report.warnings.join(' ')).toContain('--no-push');
    expect(gitOk(h.bare as string, ['rev-parse', 'HEAD']).trim()).toBe(seedHead);
    expect(loadState(h.paths).lastSyncCommit).toBe(report.commit);
  });

  it('无 upstream → local-only：status=pushed + warning，本地 commit 成立', async () => {
    const h = makeHome({ root: path.join(mkTmp('agent'), 'agent'), git: true });
    const fixture = driftFixture('push');

    const report = await runPush(
      { homerHome: h.home, yes: true },
      { sources: sources(fixture.base, fixture.local, fixture.remote) },
    );

    expect(report.status).toBe('pushed');
    expect(report.pushedToRemote).toBe(false);
    expect(report.warnings.join(' ')).toContain('未配置 git upstream');
    expect(loadState(h.paths).lastSyncCommit).toBe(report.commit);
  });

  it('git push 失败 → status=error + 「本地 commit 已成功，远端推送失败」（state 仍前进）', async () => {
    const h = makeHome({ root: path.join(mkTmp('agent'), 'agent'), git: true, upstream: true });
    seedStore(h);
    gitOk(h.home, ['add', '-A']);
    gitOk(h.home, ['commit', '-m', 'seed']);
    gitOk(h.home, ['push', '-u', 'origin', 'main']);

    // 让远端消失：push 必然失败（无网络依赖，纯本地路径）
    fs.rmSync(h.bare as string, { recursive: true, force: true });
    gitFails(h.home, ['push']);

    const fixture = driftFixture('push');
    const report = await runPush(
      { homerHome: h.home, yes: true },
      { sources: sources(fixture.base, fixture.local, fixture.remote) },
    );

    expect(report.status).toBe('error');
    expect(report.ok).toBe(false);
    expect(report.commit).toBeDefined();
    expect(report.pushedToRemote).toBe(false);
    expect(report.errors.join(' ')).toContain('本地 commit 已成功');
    expect(report.errors.join(' ')).toContain('远端推送失败');
    // 本地提交成立 → state 必须前进（base 不能丢）
    expect(loadState(h.paths).lastSyncCommit).toBe(report.commit);
    expect(gitOk(h.home, ['rev-parse', 'HEAD']).trim()).toBe(report.commit);
  });

  it('缺 homer.json → status=error（提示 homer init），不抛异常', async () => {
    const home = path.join(mkTmp('empty'), 'homer');
    fs.mkdirSync(home, { recursive: true });
    const report = await runPush({ homerHome: home, yes: true }, { sources: sources([], [], []) });

    expect(report.status).toBe('error');
    expect(report.errors.join(' ')).toContain('homer init');
  });
});

/* ------------------------------------------------------------------ */
/* 3. 采集告警 / 渲染计数                                              */
/* ------------------------------------------------------------------ */

describe('runPush：报告与渲染', () => {
  it('sources.warnings / errors 透传到报告（不阻断 push）', async () => {
    const h = makeHome({ root: path.join(mkTmp('agent'), 'agent'), git: true });
    const local = [adapter('pi', [category('pi', 'skills', 'mirror', { 'a.md': fileEntry('A\n') })])];
    const src: SyncSources = {
      ...sources([], local, []),
      warnings: ['git fetch 失败（远端变更不可见，remote 视作 = base）: offline'],
      errors: ['adapter root 不可读: pi (/nope: ENOENT)'],
    };

    const report = await runPush({ homerHome: h.home, yes: true, noPush: true }, { sources: src });

    expect(report.status).toBe('pushed');
    expect(report.warnings).toContain('git fetch 失败（远端变更不可见，remote 视作 = base）: offline');
    expect(report.warnings).toContain('adapter root 不可读: pi (/nope: ENOENT)');
  });

  it('渲染文本的计数与报告字段一致（变更文件 / 密钥命中）', async () => {
    const h = makeHome({ root: path.join(mkTmp('agent'), 'agent'), git: true });
    const fixture = driftFixture('push');
    const report = await runPush(
      { homerHome: h.home, yes: true, noPush: true },
      { sources: sources(fixture.base, fixture.local, fixture.remote) },
    );

    const text = textOf(report);
    expect(text).toContain(`变更文件: ${report.changedFiles.length}`);
    expect(text).toContain('密钥命中: 0');
    for (const file of report.changedFiles) {
      expect(text).toContain(`${file.adapterId}/${file.category}/${file.relPath}`);
    }
    // 报告本身可 JSON 往返（--json 的形状契约）
    expect(JSON.parse(JSON.stringify(report))).toEqual(report);
  });
});

/* ------------------------------------------------------------------ */
/* 4. CLI 级：退出码总表 + --json                                      */
/* ------------------------------------------------------------------ */

interface Captured { code: number; out: string; err: string }

async function cli(args: string[]): Promise<Captured> {
  const out: string[] = [];
  const err: string[] = [];
  const code = await run(args, { out: (l) => out.push(l), err: (l) => err.push(l) });
  return { code, out: out.join('\n'), err: err.join('\n') };
}

describe('homer push（CLI 分发）：退出码与 --json', () => {
  it('密钥命中：exit 1，--json 可 parse 且含 path:line', async () => {
    const root = path.join(mkTmp('agent'), 'agent');
    const h = makeHome({ root });
    seedStore(h);
    write(path.join(root, 'settings.json'), `{"apiKey":"sk-ant-api03-abcdefghijklmnopqrstuvwxyz"}\n`);
    write(path.join(root, 'skills/foo/SKILL.md'), '# foo\n');

    const result = await cli(['push', '--yes', '--json', '--home', h.home]);

    expect(result.code).toBe(1);
    const report = JSON.parse(result.out) as PushReport;
    expect(report.status).toBe('secrets-rejected');
    expect(report.secrets[0]?.path).toBe('pi/settings/settings.json');
    expect(report.secrets[0]?.line).toBe(1);
    // 拒推 → store 内容未被覆写（仍是 seed 的 base 内容），且未建立 git 仓库 = 无新 commit。
    expect(fs.readFileSync(path.join(h.paths.storeDir, 'pi/settings/settings.json'), 'utf8')).toContain('"theme": "light"');
    expect(fs.existsSync(path.join(h.home, '.git'))).toBe(false);
  });

  it('无漂移：exit 0，no-drift（--yes 与无 --yes 同结论）', async () => {
    const root = path.join(mkTmp('agent'), 'agent');
    const h = makeHome({ root, git: true });
    seedStore(h);
    seedAgentRoot(h);

    const withYes = await cli(['push', '--yes', '--json', '--home', h.home]);
    expect(withYes.code).toBe(0);
    expect((JSON.parse(withYes.out) as PushReport).status).toBe('no-drift');

    const withoutYes = await cli(['push', '--home', h.home]);
    expect(withoutYes.code).toBe(0);
    expect(withoutYes.out).toContain('no-drift');
  });

  it('完整成功：exit 0，--json 计数与文本一致、origin 有新 commit、state 前进', async () => {
    // 场景 A：--json
    const rootA = path.join(mkTmp('agent'), 'agent');
    const hA = makeHome({ root: rootA, git: true, upstream: true });
    seedStore(hA, { commit: true });
    seedAgentRoot(hA);
    write(path.join(rootA, 'settings.json'), `${JSON.stringify({ theme: 'dark', keep: 1 }, null, 2)}\n`);
    write(path.join(rootA, 'skills/new/SKILL.md'), '# new\n');

    const jsonResult = await cli(['push', '--yes', '--json', '--home', hA.home]);
    expect(jsonResult.code).toBe(0);
    const report = JSON.parse(jsonResult.out) as PushReport;
    expect(report.status).toBe('pushed');
    expect(report.pushedToRemote).toBe(true);
    expect(report.changedFiles.length).toBeGreaterThan(0);
    expect(report.commit).toBe(loadState(hA.paths).lastSyncCommit);
    expect(gitOk(hA.bare as string, ['rev-parse', 'HEAD']).trim()).toBe(report.commit);

    // 场景 B（同一构造、另一个临时工作区）：文本输出的计数与场景 A 的 JSON 一致
    const rootB = path.join(mkTmp('agent'), 'agent');
    const hB = makeHome({ root: rootB, git: true, upstream: true });
    seedStore(hB, { commit: true });
    seedAgentRoot(hB);
    write(path.join(rootB, 'settings.json'), `${JSON.stringify({ theme: 'dark', keep: 1 }, null, 2)}\n`);
    write(path.join(rootB, 'skills/new/SKILL.md'), '# new\n');

    const textResult = await cli(['push', '--yes', '--home', hB.home]);
    expect(textResult.code).toBe(0);
    expect(textResult.out).toContain(`变更文件: ${report.changedFiles.length}`);
    expect(textResult.out).toContain('homer push: pushed');
    // 两个工作区的 commit SHA 必然不同（独立临时仓库），故只钉格式：7 位短 SHA。
    expect(textResult.out).toMatch(/本地提交: [0-9a-f]{7}/);
    expect(textResult.out).toContain('远端推送: 已推送');

    // 文本计数与 JSON 计数一致（用另一个工作区再取一次 JSON 交叉核对）。
    const jsonB = await cli(['status', '--json', '--home', hB.home]);
    expect(jsonB.code).toBe(0);
    const jsonResultB = JSON.parse(jsonB.out) as { adapters: { push: number }[] };
    expect(jsonResultB.adapters[0]?.push).toBe(0); // push 后已同步（无残留漂移）
  });

  it('非交互且无 --yes：confirm=false → aborted exit 1 + 提示加 --yes', async () => {
    const root = path.join(mkTmp('agent'), 'agent');
    const h = makeHome({ root, git: true });
    seedStore(h);
    seedAgentRoot(h);
    write(path.join(root, 'skills/new/SKILL.md'), '# new\n');

    expect(process.stdout.isTTY === true).toBe(false); // 测试进程必须是非 TTY，否则本用例无意义
    const result = await cli(['push', '--home', h.home]);

    expect(result.code).toBe(1);
    expect(result.out).toContain('aborted');
    expect(result.out).toContain('非交互环境，请加 --yes');
    expect(fs.existsSync(path.join(h.paths.storeDir, 'pi/skills/new/SKILL.md'))).toBe(false);
  });

  it('usage / unknown option 仍走既有分发层（exit 1）', async () => {
    const help = await cli(['push', '--help']);
    expect(help.code).toBe(0);
    expect(help.out).toContain('homer push');

    const bad = await cli(['push', '--nope']);
    expect(bad.code).toBe(1);
    expect(bad.err).toContain('--nope');
  });
});

/* ------------------------------------------------------------------ */
/* 小工具                                                              */
/* ------------------------------------------------------------------ */

/** 构造「带 secret 的 JSON 原文」条目（kind='json'，内容就是给定的原始文本）。 */
function rawJson(content: string): SnapshotEntry {
  return { kind: 'json', content };
}
