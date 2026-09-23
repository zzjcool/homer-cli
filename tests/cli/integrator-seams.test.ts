/**
 * P4-W10 缝隙修复的定点回归（docs/m2-plan.md §3-P4「缝隙修复」）。
 *
 * 本文件只钉 P4 在集成 8 组 e2e 时暴露出的**跨命令接缝**，不重复各命令自己的测试：
 *
 *   1. `git-port.ts`：`resolveGitPort` 的默认/覆盖语义 + `hasPushTarget`
 *      （「刚 clone 空 origin」时 `@{upstream}` 不可解析但 push 仍应成功）。
 *   2. `push` 同步基线：`homer init` 后 store 未入库 + 仓库已有 HEAD → push 不能 no-op，
 *      必须落首个 store commit 并推到 origin（§3-P4 ① / §5 的 Done 判据）。
 *   3. `pull` 残留冲突：base **不**前移（否则随后 `homer merge` 看不到冲突，
 *      §3-P4 ⑤ 不可达）；无冲突时 base 照常前移到 ff HEAD。
 *   4. `collectSyncSources` 首次接入：upstream commit 里没有 `store/` 树 → remote := base
 *      （不能把「远端还没有 store」当成「远端删光了所有文件」）。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { resolveGitPort } from '../../src/cli/commands/git-port.js';
import { runPush, type PushReport } from '../../src/cli/commands/push.js';
import { runPull, type PullGitPort } from '../../src/cli/commands/pull.js';
import { gitExec } from '../../src/core/git/index.js';
import { getHomerPaths } from '../../src/core/paths.js';
import { loadState } from '../../src/core/state.js';
import { collectSyncSources } from '../../src/core/sync/base.js';
import type { GitExecResult } from '../../src/core/git/index.js';
import type { AdapterSnapshot, HomerConfig } from '../../src/core/types.js';
import type { SyncSources } from '../../src/core/sync/types.js';

/* ------------------------------------------------------------------ */
/* 隔离                                                                */
/* ------------------------------------------------------------------ */

const created: string[] = [];

function mkTmp(prefix: string): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), `homer-w10-${prefix}-`));
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

function write(abs: string, content: string): void {
  fs.mkdirSync(path.dirname(abs), { recursive: true });
  fs.writeFileSync(abs, content, 'utf8');
}

/* ------------------------------------------------------------------ */
/* 1. git-port：共享端口 + hasPushTarget                               */
/* ------------------------------------------------------------------ */

describe('P4-W10 git-port：resolveGitPort / hasPushTarget', () => {
  it('resolveGitPort 缺省填真实实现，覆盖项优先（含 additive 的 hasPushTarget）', () => {
    const resolved = resolveGitPort(undefined);
    expect(typeof resolved.isGitRepo).toBe('function');
    expect(typeof resolved.gitFetch).toBe('function');
    expect(typeof resolved.gitPush).toBe('function');
    expect(typeof resolved.commitStoreIfNeeded).toBe('function');
    expect(typeof resolved.requireFastForwardable).toBe('function');
    expect(typeof resolved.hasPushTarget).toBe('function');

    const overridden = resolveGitPort({ hasUpstream: () => true, isGitRepo: () => false });
    expect(overridden.hasUpstream('')).toBe(true);
    expect(overridden.isGitRepo('')).toBe(false);
    // 未覆盖项仍是真实实现（不共享可变状态）。
    expect(overridden).not.toBe(resolved);
  });

  it('hasPushTarget：clone 空 origin（branch.remote=origin，无远程跟踪引用）→ true', () => {
    const tmp = mkTmp('has-target');
    const origin = path.join(tmp, 'origin.git');
    const clone = path.join(tmp, 'clone');
    gitOk(tmp, ['init', '--bare', '-b', 'main', origin]);
    gitOk(tmp, ['clone', origin, clone]);
    gitOk(clone, ['config', 'user.email', 'w10@example.invalid']);
    gitOk(clone, ['config', 'user.name', 'W10']);

    const git = resolveGitPort(undefined);
    // 尚无 commit / 远程跟踪引用：`@{upstream}` 不可解析。
    expect(git.hasUpstream(clone)).toBe(false);
    // 但分支已配置 origin → push 是可成功的，不能被误判为 local-only。
    expect(git.hasPushTarget(clone)).toBe(true);
  });

  it('hasPushTarget：仓库无任何 remote → false', () => {
    const home = path.join(mkTmp('no-remote'), 'repo');
    fs.mkdirSync(home, { recursive: true });
    gitOk(home, ['init', '-b', 'main']);
    gitOk(home, ['config', 'user.email', 'w10@example.invalid']);
    gitOk(home, ['config', 'user.name', 'W10']);

    expect(resolveGitPort(undefined).hasPushTarget(home)).toBe(false);
  });
});

/* ------------------------------------------------------------------ */
/* 2. push 同步基线（no-drift 但 store 尚未入库）                        */
/* ------------------------------------------------------------------ */

interface SeamRepo {
  home: string;
  origin: string;
  agentRoot: string;
  config: HomerConfig;
}

/** 真实临时仓库：clone 空 bare origin → homer.json commit（**故意不 push**）→ 写 store。 */
function setupBaselineRepo(): SeamRepo {
  const tmp = mkTmp('baseline');
  const origin = path.join(tmp, 'origin.git');
  const home = path.join(tmp, 'homer');
  const agentRoot = path.join(tmp, 'home', '.pi', 'agent');
  fs.mkdirSync(agentRoot, { recursive: true });

  gitOk(tmp, ['init', '--bare', '-b', 'main', origin]);
  gitOk(tmp, ['clone', origin, home]);
  gitOk(home, ['config', 'user.email', 'w10@example.invalid']);
  gitOk(home, ['config', 'user.name', 'W10']);

  const config: HomerConfig = {
    version: 1,
    adapters: {
      pi: {
        root: agentRoot,
        enabled: true,
        categories: {
          settings: { paths: ['settings.json'], mode: 'merge' },
          skills: { paths: ['skills/'], mode: 'mirror' },
        },
      },
    },
  };
  write(path.join(home, 'homer.json'), `${JSON.stringify(config, null, 2)}\n`);
  gitOk(home, ['add', 'homer.json']);
  gitOk(home, ['commit', '-m', 'chore: add homer.json']);

  // store 已由 init 写好（未跟踪），工具目录与之一致 → 零漂移。
  write(path.join(home, 'store/pi/settings/settings.json'), '{"theme":"light"}\n');
  write(path.join(home, 'store/pi/skills/a/SKILL.md'), '# a\n');
  write(path.join(home, 'store/pi/.homer-complete'), '');
  write(path.join(agentRoot, 'settings.json'), '{"theme":"light"}\n');
  write(path.join(agentRoot, 'skills/a/SKILL.md'), '# a\n');

  return { home, origin, agentRoot, config };
}

describe('P4-W10 push 基线：init 后首个 push 不能 no-op', () => {
  it('零漂移但 store 未入库 + 仓库已有 HEAD → pushed：落 commit 并推到 origin', async () => {
    const repo = setupBaselineRepo();
    const paths = getHomerPaths({ HOMER_HOME: repo.home });
    // 前置：仓库有 HEAD，但 origin 还没有任何 commit。
    expect(gitOk(repo.home, ['rev-parse', 'HEAD']).trim()).not.toBe('');
    expect(gitExec(repo.origin, ['rev-parse', '--verify', 'HEAD']).ok).toBe(false);

    const report: PushReport = await runPush({ homerHome: repo.home, yes: true });

    expect(report.status).toBe('pushed');
    expect(report.pushedToRemote).toBe(true);
    expect(report.commit).toBeDefined();
    // store 进了历史 + 推到 origin。
    expect(gitOk(repo.origin, ['ls-tree', '-r', '--name-only', 'HEAD'])).toContain('store/pi/settings/settings.json');
    expect(gitOk(repo.origin, ['rev-parse', 'HEAD']).trim()).toBe(report.commit);
    // §5 Done 判据。
    expect(loadState(paths).lastSyncCommit).toBe(gitOk(repo.home, ['rev-parse', 'HEAD']).trim());
  });

  it('已入库后再 push 零漂移 → no-drift（基线已建立，不再补 commit）', async () => {
    const repo = setupBaselineRepo();
    const first = await runPush({ homerHome: repo.home, yes: true });
    expect(first.status).toBe('pushed');

    const head = gitOk(repo.home, ['rev-parse', 'HEAD']).trim();
    const second = await runPush({ homerHome: repo.home, yes: true });

    expect(second.status).toBe('no-drift');
    expect(gitOk(repo.home, ['rev-parse', 'HEAD']).trim()).toBe(head);
  });
});

/* ------------------------------------------------------------------ */
/* 3. pull 残留冲突：base 不前移                                        */
/* ------------------------------------------------------------------ */

interface PullHarness {
  home: string;
  paths: ReturnType<typeof getHomerPaths>;
  agentRoot: string;
  config: HomerConfig;
}

function setupPull(): PullHarness {
  const tmp = mkTmp('pull');
  const home = path.join(tmp, 'homer');
  const agentRoot = path.join(tmp, 'agent');
  fs.mkdirSync(home, { recursive: true });
  fs.mkdirSync(agentRoot, { recursive: true });

  const config: HomerConfig = {
    version: 1,
    adapters: {
      pi: {
        root: agentRoot,
        enabled: true,
        categories: { skills: { paths: ['skills/'], mode: 'mirror' } },
      },
    },
  };
  write(path.join(home, 'homer.json'), `${JSON.stringify(config, null, 2)}\n`);
  return { home, paths: getHomerPaths({ HOMER_HOME: home }), agentRoot, config };
}

function snap(files: Record<string, string>): AdapterSnapshot[] {
  return [
    {
      adapterId: 'pi',
      categories: [
        {
          adapterId: 'pi',
          category: 'skills',
          mode: 'mirror',
          files: new Map(Object.entries(files).map(([k, v]) => [k, { kind: 'file' as const, content: v }])),
        },
      ],
    },
  ];
}

/** git 端口：headCommit 依次返回 [preFf, postFf]（模拟 ff 前后 HEAD 变化）。 */
function portWithHeads(heads: string[]): PullGitPort {
  let i = 0;
  const queue = [...heads];
  const ok = (): GitExecResult => ({ ok: true, stdout: '', stderr: '' });
  return {
    isGitRepo: () => true,
    hasUpstream: () => true,
    requireCleanStore: () => {},
    gitFetch: ok,
    requireFastForwardable: () => {},
    mergeFfUpstream: ok,
    headCommit: () => {
      const value = queue[i] ?? queue[queue.length - 1] ?? 'head';
      i += 1;
      return value;
    },
  };
}

describe('P4-W10 pull 残留冲突：base 不前移（merge 才能看到冲突）', () => {
  it('conflicts-remain → state.lastSyncCommit 停在 preFfHead；ff 已发生', async () => {
    const h = setupPull();
    write(path.join(h.agentRoot, 'skills/a/SKILL.md'), '# local\n');

    const sources: SyncSources = {
      mode: 'git',
      base: snap({ 'a/SKILL.md': '# base\n' }),
      local: snap({ 'a/SKILL.md': '# local\n' }),
      remote: snap({ 'a/SKILL.md': '# remote\n' }),
      warnings: [],
      errors: [],
    };

    const report = await runPull(
      { homerHome: h.home, yes: true },
      { sources, git: portWithHeads(['preFfHead', 'postFfHead']) },
    );

    expect(report.status).toBe('conflicts-remain');
    // 本地保留。
    expect(fs.readFileSync(path.join(h.agentRoot, 'skills/a/SKILL.md'), 'utf8')).toBe('# local\n');
    // base 不上前移（= 本轮判定用的 base），否则下一轮 merge 会误报 no-conflicts。
    expect(loadState(h.paths).lastSyncCommit).toBe('preFfHead');
  });

  it('无冲突 → base 前移到 ff 后的 HEAD（§1 D6 原语义不变）', async () => {
    const h = setupPull();
    write(path.join(h.agentRoot, 'skills/a/SKILL.md'), '# base\n');
    write(path.join(h.agentRoot, 'skills/b/SKILL.md'), '# b\n');

    const sources: SyncSources = {
      mode: 'git',
      base: snap({ 'a/SKILL.md': '# base\n', 'b/SKILL.md': '# b\n' }),
      local: snap({ 'a/SKILL.md': '# base\n', 'b/SKILL.md': '# b\n' }),
      remote: snap({ 'a/SKILL.md': '# base\n', 'b/SKILL.md': '# b2\n' }),
      warnings: [],
      errors: [],
    };

    const report = await runPull(
      { homerHome: h.home, yes: true },
      { sources, git: portWithHeads(['preFfHead', 'postFfHead']) },
    );

    expect(report.status).toBe('applied');
    expect(report.commit).toBe('postFfHead');
    expect(loadState(h.paths).lastSyncCommit).toBe('postFfHead');
  });
});

/* ------------------------------------------------------------------ */
/* 4. collectSyncSources：upstream 无 store 树 → remote := base         */
/* ------------------------------------------------------------------ */

describe('P4-W10 collectSyncSources 首次接入：upstream 缺 store/ 树', () => {
  it('远端只有 homer.json（无 store）→ remote := base，不产生假 pull-delete', () => {
    const h = setupPull();
    const origin = path.join(path.dirname(h.home), 'origin.git');
    gitOk(path.dirname(h.home), ['init', '--bare', '-b', 'main', origin]);
    gitOk(h.home, ['init', '-b', 'main']);
    gitOk(h.home, ['config', 'user.email', 'w10@example.invalid']);
    gitOk(h.home, ['config', 'user.name', 'W10']);
    gitOk(h.home, ['remote', 'add', 'origin', origin]);

    // 本地 store 有内容，但远端 commit 只有 homer.json（从没有 store/）。
    write(path.join(h.paths.storeDir, 'pi/skills/a/SKILL.md'), '# a\n');
    write(path.join(h.paths.storeDir, 'pi/.homer-complete'), '');
    gitOk(h.home, ['add', '-A']);
    gitOk(h.home, ['commit', '-m', 'local store']);

    // 远端分支只含 homer.json：另起一个 clone 提交并推送 homer.json。
    const other = mkTmp('other');
    gitOk(other, ['clone', origin, '.']);
    gitOk(other, ['config', 'user.email', 'w10@example.invalid']);
    gitOk(other, ['config', 'user.name', 'W10']);
    write(path.join(other, 'homer.json'), '{"version":1,"adapters":{}}\n');
    gitOk(other, ['add', 'homer.json']);
    gitOk(other, ['commit', '-m', 'remote homer.json only']);
    gitOk(other, ['push', 'origin', 'main']);

    gitOk(h.home, ['fetch']);
    gitOk(h.home, ['branch', '--set-upstream-to=origin/main', 'main']);

    const sources = collectSyncSources(h.paths, h.config, { fetch: false });

    expect(sources.remoteRef).toBe('origin/main');
    // remote 回落 = base：远端「没有 store」不被当成「删光了 store」。
    expect([...(sources.remote[0]?.categories[0]?.files.keys() ?? [])]).toEqual(
      [...(sources.base[0]?.categories[0]?.files.keys() ?? [])],
    );
    expect(sources.warnings.some((w) => /尚无 store 内容/.test(w))).toBe(true);
  });
});
