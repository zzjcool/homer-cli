/**
 * P3-W8 · `src/cli/commands/pull.ts` 验收（docs/m2-plan.md §2.8 / §3-P3 的 W8 专项）。
 *
 * 三条互不干扰的证据线：
 *
 * 1. **注入 sources + git 端口**（`PullDeps.sources` / `PullDeps.git`，additive 可选字段）：
 *    命令层的前置检查顺序、预览、确认、应用、ff、state 全部可在无仓库 / 无网络的条件下钉死；
 *    工具目录写盘仍走真实 fs（断言落在**绝对路径**上）。
 * 2. **纯函数**：`buildPullPreview`（diffLines 行级预览 + 每文件截断 20 行）、`renderPullReport`
 *    （冲突标红，DESIGN §2.8）。
 * 3. **真实临时仓库**（`mkdtemp` + 本地 bare origin）：`run()` 分发层的退出码总表
 *    （applied / no-drift → 0，aborted / conflicts-remain / error → 1）与 `--json` 可解析。
 *
 * 全程隔离：假 HOMER_HOME + 假 adapter root + 临时 git 仓库；绝不碰真实 `~/.homer` / `~/.pi`。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import {
  buildPullPreview,
  PREVIEW_MAX_LINES,
  renderPullReport,
  runPull,
  type PullGitPort,
  type PullReport,
} from '../../src/cli/commands/pull.js';
import { run, type CliIO } from '../../src/cli/index.js';
import { CliError } from '../../src/core/errors.js';
import { getHomerPaths, type HomerPaths } from '../../src/core/paths.js';
import { saveState } from '../../src/core/state.js';
import { gitExec } from '../../src/core/git/index.js';
import type { GitExecResult } from '../../src/core/git/index.js';
import type { HomerConfig } from '../../src/core/types.js';
import type { SyncSources } from '../../src/core/sync/types.js';
import type { PromptPort } from '../../src/cli/ui.js';

/* ------------------------------------------------------------------ */
/* 隔离与工具                                                          */
/* ------------------------------------------------------------------ */

const created: string[] = [];

function mkTmp(prefix: string): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), `homer-pull-${prefix}-`));
  created.push(dir);
  return dir;
}

afterEach(() => {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) fs.rmSync(dir, { recursive: true, force: true });
  }
});

interface Harness {
  home: string;
  agentRoot: string;
  paths: HomerPaths;
  config: HomerConfig;
}

/** 假 HOME 下的 pi adapter root + 最小 config（settings 单文件 merge，skills 目录 mirror）。 */
function setup(opts: { backup?: { keep?: number } } = {}): Harness {
  const tmp = mkTmp('unit');
  const home = path.join(tmp, 'homer');
  const agentRoot = path.join(tmp, 'home', '.pi', 'agent');
  fs.mkdirSync(home, { recursive: true });
  fs.mkdirSync(agentRoot, { recursive: true });

  const config: HomerConfig = {
    version: 1,
    adapters: {
      pi: {
        root: agentRoot,
        enabled: true,
        categories: {
          settings: { paths: ['settings.json'], mode: 'merge', excludeKeys: ['apiKey'] },
          skills: { paths: ['skills/'], mode: 'mirror' },
        },
      },
    },
  };
  if (opts.backup !== undefined) config.backup = opts.backup;
  fs.writeFileSync(path.join(home, 'homer.json'), `${JSON.stringify(config, null, 2)}\n`, 'utf8');

  return { home, agentRoot, paths: getHomerPaths({ HOMER_HOME: home }), config };
}

function write(file: string, content: string): void {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, content, 'utf8');
}

function read(file: string): string {
  return fs.readFileSync(file, 'utf8');
}

/* ---- 手工快照（不 import store / scan 侧实现，与 tests/cli/helpers.ts 同款构造） ---- */

interface SnapInput {
  settings?: string;
  skills?: Record<string, string>;
}

function snap(input: SnapInput): SyncSources['base'] {
  const settings = new Map<string, { kind: 'json'; content: string }>();
  if (input.settings !== undefined) settings.set('settings.json', { kind: 'json', content: input.settings });

  const skills = new Map<string, { kind: 'file'; content: string }>();
  for (const [relPath, content] of Object.entries(input.skills ?? {})) {
    skills.set(relPath, { kind: 'file', content });
  }

  return [
    {
      adapterId: 'pi',
      categories: [
        { adapterId: 'pi', category: 'settings', mode: 'merge', files: settings },
        { adapterId: 'pi', category: 'skills', mode: 'mirror', files: skills },
      ],
    },
  ];
}

function sources(base: SnapInput, local: SnapInput, remote: SnapInput, extra: Partial<SyncSources> = {}): SyncSources {
  return {
    mode: 'git',
    base: snap(base),
    local: snap(local),
    remote: snap(remote),
    warnings: [],
    errors: [],
    ...extra,
  };
}

/* ---- git 端口替身（记录调用顺序） ---- */

interface StubGit {
  port: PullGitPort;
  calls: string[];
  fetchCalls: string[];
  ffCalls: string[];
}

function stubGit(overrides: Partial<PullGitPort> = {}): StubGit {
  const calls: string[] = [];
  const fetchCalls: string[] = [];
  const ffCalls: string[] = [];
  const okResult = (): GitExecResult => ({ ok: true, stdout: '', stderr: '' });

  const port: PullGitPort = {
    isGitRepo: () => {
      calls.push('isGitRepo');
      return true;
    },
    hasUpstream: () => {
      calls.push('hasUpstream');
      return true;
    },
    requireCleanStore: () => {
      calls.push('requireCleanStore');
    },
    gitFetch: () => {
      calls.push('gitFetch');
      fetchCalls.push('gitFetch');
      return okResult();
    },
    requireFastForwardable: () => {
      calls.push('requireFastForwardable');
    },
    mergeFfUpstream: () => {
      calls.push('mergeFfUpstream');
      ffCalls.push('mergeFfUpstream');
      return okResult();
    },
    headCommit: () => {
      calls.push('headCommit');
      return 'new-head-sha';
    },
    ...overrides,
  };
  return { port, calls, fetchCalls, ffCalls };
}

/** 记录 confirm 调用的非交互 port 替身。 */
function recordingPort(answer: boolean): { port: PromptPort; calls: { message: string; fallback: boolean }[] } {
  const calls: { message: string; fallback: boolean }[] = [];
  return {
    calls,
    port: {
      async confirm(message: string, fallback: boolean): Promise<boolean> {
        calls.push({ message, fallback });
        return answer;
      },
      async select<T extends string>(_message: string, options: readonly { value: T; label: string }[], fallback: T): Promise<T> {
        return options[0]?.value ?? fallback;
      },
    },
  };
}

function captureIO(): { io: CliIO; out: string[]; err: string[] } {
  const out: string[] = [];
  const err: string[] = [];
  return { io: { out: (line) => out.push(line), err: (line) => err.push(line) }, out, err };
}

function parseJsonOut(out: string[]): PullReport {
  return JSON.parse(out.join('\n')) as PullReport;
}

/* ------------------------------------------------------------------ */
/* 1. 注入 sources + git 端口                                          */
/* ------------------------------------------------------------------ */

describe('runPull — 注入 sources + git 端口', () => {
  it('前置检查按冻结顺序执行，全部先于任何写操作', async () => {
    const h = setup();
    const git = stubGit();
    const src = sources({ settings: '{"theme":"light"}\n' }, { settings: '{"theme":"light"}\n' }, { settings: '{"theme":"light"}\n' });

    const report = await runPull({ homerHome: h.home, yes: true }, { sources: src, git: git.port, noApply: true });

    expect(report.status).toBe('no-drift');
    expect(git.calls).toEqual(['isGitRepo', 'hasUpstream', 'requireCleanStore', 'gitFetch', 'requireFastForwardable']);
  });

  it('无 config → CliError 提示先 init', async () => {
    const tmp = mkTmp('nocfg');
    const error = await runPull({ homerHome: tmp, yes: true }).catch((err: unknown) => err);
    expect(error).toBeInstanceOf(CliError);
    expect((error as CliError).message).toMatch(/未找到 .*homer\.json/);
    expect((error as CliError).hint).toMatch(/homer init/);
  });

  it('非 git 仓库 → CliError（提示先 push 建立历史）', async () => {
    const h = setup();
    const git = stubGit({ isGitRepo: () => false });
    await expect(runPull({ homerHome: h.home, yes: true }, { sources: sources({}, {}, {}), git: git.port })).rejects.toThrow(
      /不是 git 仓库/,
    );
    // 后续检查不得被触发
    expect(git.calls).toEqual([]);
  });

  it('无 upstream → CliError 提示配置 remote（且不 fetch）', async () => {
    const h = setup();
    const git = stubGit({ hasUpstream: () => false });
    await expect(
      runPull({ homerHome: h.home, yes: true }, { sources: sources({}, {}, {}), git: git.port }),
    ).rejects.toThrow(/未配置 git upstream/);
    expect(git.calls).toEqual(['isGitRepo']);
    expect(git.fetchCalls).toEqual([]);
  });

  it('store 脏 → 透传 requireCleanStore 的 CliError（提示先 push），且不 fetch', async () => {
    const h = setup();
    const git = stubGit({
      requireCleanStore: () => {
        throw new Error('store 工作区有未提交的改动');
      },
    });
    await expect(
      runPull({ homerHome: h.home, yes: true }, { sources: sources({}, {}, {}), git: git.port }),
    ).rejects.toThrow(/未提交的改动/);
    expect(git.calls).toEqual(['isGitRepo', 'hasUpstream']);
    expect(git.fetchCalls).toEqual([]);
  });

  it('分叉 → 透传 requireFastForwardable 的 CliError（且不写工具目录）', async () => {
    const h = setup();
    write(path.join(h.agentRoot, 'settings.json'), '{"theme":"local"}\n');
    const git = stubGit({
      requireFastForwardable: () => {
        throw new Error('本地与远端已分叉');
      },
    });
    const src = sources({ settings: '{"theme":"light"}\n' }, { settings: '{"theme":"local"}\n' }, { settings: '{"theme":"dark"}\n' });

    await expect(runPull({ homerHome: h.home, yes: true }, { sources: src, git: git.port })).rejects.toThrow(/已分叉/);
    expect(git.ffCalls).toEqual([]);
    expect(read(path.join(h.agentRoot, 'settings.json'))).toBe('{"theme":"local"}\n');
  });

  it('noFetch=true → 不调用 gitFetch；缺省 → 调用一次', async () => {
    const h = setup();
    const src = sources({}, {}, {});

    const withFetch = stubGit();
    await runPull({ homerHome: h.home, yes: true }, { sources: src, git: withFetch.port, noApply: true });
    expect(withFetch.fetchCalls).toEqual(['gitFetch']);

    const withoutFetch = stubGit();
    await runPull({ homerHome: h.home, yes: true }, { sources: src, git: withoutFetch.port, noFetch: true, noApply: true });
    expect(withoutFetch.fetchCalls).toEqual([]);
  });

  it('fetch 失败 → CliError（pull 不降级）', async () => {
    const h = setup();
    const git = stubGit({ gitFetch: () => ({ ok: false, stdout: '', stderr: 'Could not resolve host' }) });
    await expect(
      runPull({ homerHome: h.home, yes: true }, { sources: sources({}, {}, {}), git: git.port }),
    ).rejects.toThrow(/fetch 失败.*Could not resolve host/);
    expect(git.calls).toEqual(['isGitRepo', 'hasUpstream', 'requireCleanStore']);
    expect(git.ffCalls).toEqual([]);
  });

  it('无动作 → no-drift：不写盘、不 ff、不改 state', async () => {
    const h = setup();
    const git = stubGit();
    const same = { settings: '{"theme":"light"}\n', skills: { 'a.md': 'A\n' } };

    const report = await runPull({ homerHome: h.home, yes: true }, { sources: sources(same, same, same), git: git.port });

    expect(report).toMatchObject({ ok: true, status: 'no-drift', conflicts: [] });
    expect(report.applied).toEqual({ written: [], deleted: [], conflicts: [] });
    expect(git.ffCalls).toEqual([]);
    expect(fs.existsSync(h.paths.stateFile)).toBe(false);
  });

  it('成功路径：write + delete 落到工具目录，旧内容先备份，ff 后 state = 新 HEAD', async () => {
    const h = setup();
    // 本地现状（要有真实文件，applyPullActions 才可能产生备份）
    write(path.join(h.agentRoot, 'settings.json'), '{"theme":"light","apiKey":"LOCAL-KEY"}\n');
    write(path.join(h.agentRoot, 'skills', 'alpha', 'SKILL.md'), '# alpha v1\n');

    const src = sources(
      { settings: '{"theme":"light","apiKey":"BASE-KEY"}\n', skills: { 'alpha/SKILL.md': '# alpha v1\n' } },
      { settings: '{"theme":"light","apiKey":"LOCAL-KEY"}\n', skills: { 'alpha/SKILL.md': '# alpha v1\n' } },
      { settings: '{"theme":"dark","apiKey":"REMOTE-KEY"}\n', skills: { 'beta/SKILL.md': '# beta\n' } },
    );

    const git = stubGit({ headCommit: () => 'ff-head-sha' });
    const report = await runPull({ homerHome: h.home, yes: true }, { sources: src, git: git.port });

    expect(report.status).toBe('applied');
    expect(report.ok).toBe(true);
    expect(report.conflicts).toEqual([]);
    expect(report.commit).toBe('ff-head-sha');

    // 动作计数
    expect(report.applied.written).toEqual([
      { adapterId: 'pi', category: 'settings', relPath: 'settings.json' },
      { adapterId: 'pi', category: 'skills', relPath: 'beta/SKILL.md' },
    ]);
    expect(report.applied.deleted).toEqual([{ adapterId: 'pi', category: 'skills', relPath: 'alpha/SKILL.md' }]);

    // 工具目录内容（merge 键生效 + 本地 excluded 键植回 + 新文件 + 删除传播）
    const merged = JSON.parse(read(path.join(h.agentRoot, 'settings.json'))) as Record<string, unknown>;
    expect(merged).toEqual({ apiKey: 'LOCAL-KEY', theme: 'dark' });
    expect(read(path.join(h.agentRoot, 'skills', 'beta', 'SKILL.md'))).toBe('# beta\n');
    expect(fs.existsSync(path.join(h.agentRoot, 'skills', 'alpha', 'SKILL.md'))).toBe(false);
    // skills/ 是 category 根，必须保留
    expect(fs.existsSync(path.join(h.agentRoot, 'skills'))).toBe(true);

    // 备份：覆盖前内容（settings.json 的 LOCAL 版 + alpha）
    const backupDir = report.applied.backupDir;
    expect(backupDir).toBeDefined();
    expect(fs.existsSync(path.join(backupDir as string, 'pi', 'settings', 'settings.json'))).toBe(true);
    expect(read(path.join(backupDir as string, 'pi', 'settings', 'settings.json'))).toBe(
      '{"theme":"light","apiKey":"LOCAL-KEY"}\n',
    );
    expect(read(path.join(backupDir as string, 'pi', 'skills', 'alpha', 'SKILL.md'))).toBe('# alpha v1\n');

    // ff 之后 state 前移（base 前进），lastSyncCommand='pull'
    const state = JSON.parse(read(h.paths.stateFile)) as Record<string, unknown>;
    expect(state).toMatchObject({ version: 1, lastSyncCommit: 'ff-head-sha', lastSyncCommand: 'pull' });
    expect(typeof state['lastSyncAt']).toBe('string');
  });

  it('--yes + 冲突：保留本地、conflicts-remain、ok=false，磁盘内容不变', async () => {
    const h = setup();
    write(path.join(h.agentRoot, 'skills', 'alpha', 'SKILL.md'), '# alpha local\n');

    const src = sources(
      { skills: { 'alpha/SKILL.md': '# alpha v1\n' } },
      { skills: { 'alpha/SKILL.md': '# alpha local\n' } },
      { skills: { 'alpha/SKILL.md': '# alpha remote\n' } },
    );

    const git = stubGit();
    const report = await runPull({ homerHome: h.home, yes: true }, { sources: src, git: git.port });

    expect(report.status).toBe('conflicts-remain');
    expect(report.ok).toBe(false);
    expect(report.conflicts).toHaveLength(1);
    expect(report.conflicts[0]).toMatchObject({
      adapterId: 'pi',
      category: 'skills',
      relPath: 'alpha/SKILL.md',
      reason: 'modify-vs-modify',
    });
    expect(report.applied.conflicts).toEqual([{ adapterId: 'pi', category: 'skills', relPath: 'alpha/SKILL.md' }]);
    expect(report.errors.join('\n')).toMatch(/homer merge/);
    // 本地保留
    expect(read(path.join(h.agentRoot, 'skills', 'alpha', 'SKILL.md'))).toBe('# alpha local\n');
    // 应用阶段仍然跑完（ff + state 前移：远端变更已被看见）
    expect(git.ffCalls).toEqual(['mergeFfUpstream']);
    expect(report.commit).toBe('new-head-sha');
  });

  it('非 --yes + confirm=false → aborted，零写入、不 ff、不改 state', async () => {
    const h = setup();
    write(path.join(h.agentRoot, 'skills', 'beta', 'SKILL.md'), 'old\n');
    const src = sources(
      { skills: { 'beta/SKILL.md': 'old\n' } },
      { skills: { 'beta/SKILL.md': 'old\n' } },
      { skills: { 'beta/SKILL.md': 'new\n' } },
    );
    const git = stubGit();
    const port = recordingPort(false);

    const report = await runPull({ homerHome: h.home, yes: false }, { sources: src, git: git.port, ui: port.port });

    expect(report.status).toBe('aborted');
    expect(report.ok).toBe(false);
    expect(report.applied.written).toEqual([]);
    expect(git.ffCalls).toEqual([]);
    expect(fs.existsSync(h.paths.stateFile)).toBe(false);
    expect(read(path.join(h.agentRoot, 'skills', 'beta', 'SKILL.md'))).toBe('old\n');
    // confirm 的 fallback 必须是 false（非交互下默认拒绝）
    expect(port.calls).toHaveLength(1);
    expect(port.calls[0]?.fallback).toBe(false);
    // 预览（含行级 diff）进了 confirm 的 message
    expect(port.calls[0]?.message).toContain('-old');
    expect(port.calls[0]?.message).toContain('+new');
  });

  it('非 --yes + confirm=true → applied', async () => {
    const h = setup();
    const src = sources({}, {}, { skills: { 'beta/SKILL.md': '# beta\n' } });
    const git = stubGit();
    const port = recordingPort(true);

    const report = await runPull({ homerHome: h.home, yes: false }, { sources: src, git: git.port, ui: port.port });

    expect(report.status).toBe('applied');
    expect(port.calls).toHaveLength(1);
    expect(read(path.join(h.agentRoot, 'skills', 'beta', 'SKILL.md'))).toBe('# beta\n');
  });

  it('--yes 时不创建 port：注入的 ui 永不被调用', async () => {
    const h = setup();
    const src = sources({}, {}, { skills: { 'beta/SKILL.md': '# beta\n' } });
    const git = stubGit();
    const port = recordingPort(false);

    const report = await runPull({ homerHome: h.home, yes: true }, { sources: src, git: git.port, ui: port.port });

    expect(report.status).toBe('applied');
    expect(port.calls).toEqual([]);
  });

  it('noApply=true → 不写工具目录、不 ff、不改 state，但冲突与警告照常汇报', async () => {
    const h = setup();
    write(path.join(h.agentRoot, 'skills', 'alpha', 'SKILL.md'), '# alpha local\n');
    const src = sources(
      { skills: { 'alpha/SKILL.md': '# alpha v1\n' } },
      { skills: { 'alpha/SKILL.md': '# alpha local\n' } },
      { skills: { 'alpha/SKILL.md': '# alpha remote\n' } },
    );
    const git = stubGit();

    const report = await runPull({ homerHome: h.home, yes: true }, { sources: src, git: git.port, noApply: true });

    expect(report.status).toBe('conflicts-remain');
    expect(report.conflicts).toHaveLength(1);
    expect(report.applied).toEqual({ written: [], deleted: [], conflicts: [] });
    expect(git.ffCalls).toEqual([]);
    expect(fs.existsSync(h.paths.stateFile)).toBe(false);
    expect(read(path.join(h.agentRoot, 'skills', 'alpha', 'SKILL.md'))).toBe('# alpha local\n');
  });

  it('ff 失败 → status error / ok=false，state 不前移', async () => {
    const h = setup();
    const src = sources({}, {}, { skills: { 'beta/SKILL.md': '# beta\n' } });
    const git = stubGit({
      mergeFfUpstream: () => ({ ok: false, stdout: '', stderr: 'Not possible to fast-forward' }),
      headCommit: () => 'should-not-be-used',
    });

    const report = await runPull({ homerHome: h.home, yes: true }, { sources: src, git: git.port });

    expect(report.status).toBe('error');
    expect(report.ok).toBe(false);
    expect(report.errors.join('\n')).toMatch(/ff-only 失败/);
    expect(report.applied.written).toHaveLength(1); // 已写入（前置检查已放行，属并发边界）
    expect(fs.existsSync(h.paths.stateFile)).toBe(false);
  });

  it('sources 的 warnings / errors 分别透传到报告的 warnings / errors', async () => {
    const h = setup();
    const src = sources({}, {}, {}, { warnings: ['state 不可读，base 回落 store'], errors: ['adapter root 不可读: pi'] });
    const git = stubGit();

    const report = await runPull({ homerHome: h.home, yes: true }, { sources: src, git: git.port, noApply: true });

    expect(report.warnings).toEqual(['state 不可读，base 回落 store']);
    expect(report.errors).toEqual(['adapter root 不可读: pi']);
    // 采集期问题不改变「无漂移」结论（漂移是信息不是错误）
    expect(report.status).toBe('no-drift');
  });

  it('backup.keep 生效：应用后只保留最近 7 个日期目录', async () => {
    const h = setup({ backup: { keep: 7 } });
    write(path.join(h.agentRoot, 'skills', 'beta', 'SKILL.md'), 'old\n');
    for (const name of ['20240101', '20240102', '20240103', '20240104', '20240105', '20240106', '20240107', '20240108', '20240109', '20240110']) {
      fs.mkdirSync(path.join(h.paths.backupsDir, name), { recursive: true });
    }

    const src = sources(
      { skills: { 'beta/SKILL.md': 'old\n' } },
      { skills: { 'beta/SKILL.md': 'old\n' } },
      { skills: { 'beta/SKILL.md': 'new\n' } },
    );
    const report = await runPull({ homerHome: h.home, yes: true }, { sources: src, git: stubGit().port });

    expect(report.applied.backupDir).toBeDefined();
    const dates = fs.readdirSync(h.paths.backupsDir).sort();
    // 10 个历史日期目录 + 今天的备份目录 → 保留名次最高的 7 个
    expect(dates).toHaveLength(7);
    expect(dates).not.toContain('20240101');
    expect(dates).not.toContain('20240104');
    expect(dates).toContain('20240105');
    expect(dates).toContain('20240110');
    // 今天的新备份目录也在里面（prune 保留最近 7 个日期目录，今天必居其一）
    const today = new Date();
    const todayName = `${today.getFullYear()}${String(today.getMonth() + 1).padStart(2, '0')}${String(today.getDate()).padStart(2, '0')}`;
    expect(dates).toContain(todayName);
  });
});

/* ------------------------------------------------------------------ */
/* 2. 预览（diffLines 行级）与渲染（冲突标红）                          */
/* ------------------------------------------------------------------ */

describe('buildPullPreview — 行级预览', () => {
  it('write / delete / conflict 都输出 diffLines 行级内容并带计数头', async () => {
    const h = setup();
    // 预览是纯函数（只吃 sources + plan），故这里直接调 planPull 造 plan，无需 git 端口。
    const { planPull } = await import('../../src/core/sync/plan.js');

    const src = sources(
      { skills: { 'gone.md': 'G\n' }, settings: '{"theme":"light"}\n' },
      { skills: { 'gone.md': 'G\n' }, settings: '{"theme":"light"}\n' },
      {
        skills: { 'new.md': 'N\n' },
        settings: '{"theme":"dark"}\n',
      },
    );
    const plan = planPull(h.config, src.base, src.local, src.remote);

    const preview = buildPullPreview(src, plan);

    expect(preview.split('\n')[0]).toBe('预览: 写入 2  删除 1  冲突 0');
    expect(preview).toContain('  写入 pi/skills/new.md');
    expect(preview).toContain('    +N');
    expect(preview).toContain('  删除 pi/skills/gone.md');
    expect(preview).toContain('    -G');
    // merge 单文件：write 的内容是 planPull 产出的确定性格式（2 空格缩进 + 末尾换行）
    expect(preview).toContain('    -{"theme":"light"}');
    expect(preview).toContain('    +  "theme": "dark"');
  });

  it('冲突项展示 reason / keyPaths 与 local↔remote 行级 diff', async () => {
    const h = setup();
    const { planPull } = await import('../../src/core/sync/plan.js');
    const src = sources(
      { skills: { 'alpha/SKILL.md': '# v1\n' } },
      { skills: { 'alpha/SKILL.md': '# local line\n' } },
      { skills: { 'alpha/SKILL.md': '# remote line\n' } },
    );
    const plan = planPull(h.config, src.base, src.local, src.remote);

    const preview = buildPullPreview(src, plan);

    expect(preview.split('\n')[0]).toBe('预览: 写入 0  删除 0  冲突 1');
    expect(preview).toContain('  冲突 pi/skills/alpha/SKILL.md (modify-vs-modify)');
    expect(preview).toContain('    -# local line');
    expect(preview).toContain('    +# remote line');
  });

  it('merge 键冲突展示 keyPaths', async () => {
    const h = setup();
    const { planPull } = await import('../../src/core/sync/plan.js');
    const src = sources(
      { settings: '{"theme":"light"}\n' },
      { settings: '{"theme":"local"}\n' },
      { settings: '{"theme":"remote"}\n' },
    );
    const plan = planPull(h.config, src.base, src.local, src.remote);

    const preview = buildPullPreview(src, plan);

    expect(preview).toContain('  冲突 pi/settings/settings.json (merge-keys)');
    expect(preview).toContain('冲突键: theme');
  });

  it(`每个文件的 diff 截断到前 ${PREVIEW_MAX_LINES} 行，并提示省略行数`, async () => {
    const h = setup();
    const { planPull } = await import('../../src/core/sync/plan.js');
    const long = Array.from({ length: 60 }, (_, i) => `line ${i}`).join('\n') + '\n';
    const src = sources(
      { skills: { 'big.md': 'old\n' } },
      { skills: { 'big.md': 'old\n' } },
      { skills: { 'big.md': long } },
    );
    const plan = planPull(h.config, src.base, src.local, src.remote);

    const preview = buildPullPreview(src, plan);
    const diffLinesOut = preview.split('\n').filter((line) => line.startsWith('    '));

    // 20 行 diff + 1 行省略提示 + （删除 old 的那一行也在其中）
    expect(diffLinesOut.some((line) => line.includes('… 省略'))).toBe(true);
    const omitted = diffLinesOut.find((line) => line.includes('… 省略')) as string;
    const omittedCount = Number(/省略 (\d+) 行/.exec(omitted)?.[1]);
    expect(omittedCount).toBeGreaterThan(0);
    // 未截断时行数 = 1(old '-') + 1(+line 0) ... 总之远超 20
    expect(diffLinesOut.length).toBeLessThanOrEqual(PREVIEW_MAX_LINES + 1);
  });
});

describe('renderPullReport — 冲突标红（DESIGN §2.8）', () => {
  const report: PullReport = {
    ok: false,
    status: 'conflicts-remain',
    applied: { written: [], deleted: [], conflicts: [{ adapterId: 'pi', category: 'skills', relPath: 'a.md' }] },
    conflicts: [{ type: 'conflict', adapterId: 'pi', category: 'skills', relPath: 'a.md', reason: 'modify-vs-modify' }],
    warnings: [],
    errors: ['请运行 `homer merge`'],
  };

  it('color=true → 冲突行与头行带 ANSI 红', () => {
    const text = renderPullReport(report, { color: true });
    expect(text).toContain('\u001b[31mhomer pull: conflicts-remain\u001b[0m');
    expect(text).toContain('\u001b[31m  ⚡ 冲突: pi/skills/a.md (modify-vs-modify)\u001b[0m');
  });

  it('color=false → 无 ANSI（管道 / CI / 测试输出干净）', () => {
    const text = renderPullReport(report, { color: false });
    expect(text).not.toContain('\u001b[');
    expect(text).toContain('  ⚡ 冲突: pi/skills/a.md (modify-vs-modify)');
  });
});

/* ------------------------------------------------------------------ */
/* 3. 真实临时仓库：退出码总表 + --json                                */
/* ------------------------------------------------------------------ */

interface RepoHarness {
  home: string;
  agentRoot: string;
  paths: HomerPaths;
  config: HomerConfig;
  bare: string;
}

function gitOk(cwd: string, args: readonly string[]): string {
  const result = gitExec(cwd, args);
  if (!result.ok) throw new Error(`git ${args.join(' ')} 失败 (cwd=${cwd}): ${result.stderr}`);
  return result.stdout;
}

function commitStore(paths: HomerPaths, message: string): string {
  gitOk(paths.home, ['add', '-A', '--', 'store/']);
  gitOk(paths.home, ['commit', '-m', message, '--', 'store/']);
  return gitOk(paths.home, ['rev-parse', 'HEAD']).trim();
}

/** 在 store 里落文件 + `.homer-complete` 标记。 */
function writeStore(paths: HomerPaths, rel: string, content: string): void {
  write(path.join(paths.storeDir, rel), content);
  write(path.join(paths.storeDir, 'pi', '.homer-complete'), '');
}

/**
 * 真实临时仓库：`git init` + 本地 bare origin + 首次 push -u（upstream 就绪）。
 * `storeV1` 内容提交并推送，state 指向该 commit。
 */
function setupRepo(storeV1: Record<string, string>): RepoHarness {
  const tmp = mkTmp('repo');
  const home = path.join(tmp, 'homer');
  const agentRoot = path.join(tmp, 'home', '.pi', 'agent');
  fs.mkdirSync(home, { recursive: true });
  fs.mkdirSync(agentRoot, { recursive: true });

  const config: HomerConfig = {
    version: 1,
    adapters: {
      pi: {
        root: agentRoot,
        enabled: true,
        categories: {
          settings: { paths: ['settings.json'], mode: 'merge', excludeKeys: ['apiKey'] },
          skills: { paths: ['skills/'], mode: 'mirror' },
        },
      },
    },
  };
  fs.writeFileSync(path.join(home, 'homer.json'), `${JSON.stringify(config, null, 2)}\n`, 'utf8');

  const paths = getHomerPaths({ HOMER_HOME: home });

  gitOk(home, ['init', '-b', 'main']);
  gitOk(home, ['config', 'user.email', 'homer-pull@example.invalid']);
  gitOk(home, ['config', 'user.name', 'Homer Pull Test']);

  const bare = path.join(tmp, 'origin.git');
  gitOk(tmp, ['init', '--bare', '-b', 'main', bare]);
  gitOk(home, ['remote', 'add', 'origin', bare]);

  for (const [rel, content] of Object.entries(storeV1)) writeStore(paths, rel, content);
  gitOk(home, ['add', '-A', '--', 'store/']);
  gitOk(home, ['commit', '-m', 'store v1', '--', 'store/']);
  gitOk(home, ['push', '-u', 'origin', 'main']);
  saveState(paths, { version: 1, lastSyncCommit: gitOk(home, ['rev-parse', 'HEAD']).trim(), lastSyncCommand: 'push' });

  return { home, agentRoot, paths, config, bare };
}

/** 远端前进：改 store → commit → push（本地工作区也跟着变，pull 时会 ff 对齐）。 */
function advanceRemote(h: RepoHarness, changes: Record<string, string>, message = 'remote v2'): string {
  for (const [rel, content] of Object.entries(changes)) writeStore(h.paths, rel, content);
  const sha = commitStore(h.paths, message);
  gitOk(h.paths.home, ['push']);
  return sha;
}

describe('退出码总表 / --json（run() 分发层，真实临时仓库）', () => {
  it('applied → 0（--json 可 parse 且与文本计数一致）；再跑 → no-drift → 0', async () => {
    const h = setupRepo({
      'pi/settings/settings.json': '{"theme":"light","apiKey":"BASE"}\n',
      'pi/skills/alpha/SKILL.md': '# alpha v1\n',
    });
    // 本地工具目录：旧版 settings（含本地密钥）+ 未变的 alpha
    write(path.join(h.agentRoot, 'settings.json'), '{"theme":"light","apiKey":"LOCAL"}\n');
    write(path.join(h.agentRoot, 'skills', 'alpha', 'SKILL.md'), '# alpha v1\n');

    const remoteHead = advanceRemote(h, {
      'pi/settings/settings.json': '{"theme":"dark","apiKey":"REMOTE"}\n',
      'pi/skills/beta/SKILL.md': '# beta\n',
    });

    const first = captureIO();
    const code = await run(['pull', '--home', h.home, '--yes', '--json'], first.io);
    expect(code).toBe(0);

    const report = parseJsonOut(first.out);
    expect(report.status).toBe('applied');
    expect(report.ok).toBe(true);
    expect(report.applied.written.map((item) => item.relPath).sort()).toEqual(['beta/SKILL.md', 'settings.json']);
    expect(report.commit).toBe(remoteHead);
    expect(JSON.parse(fs.readFileSync(h.paths.stateFile, 'utf8'))).toMatchObject({ lastSyncCommit: remoteHead });

    // --json 与文本计数一致
    const second = captureIO();
    const code2 = await run(['pull', '--home', h.home, '--yes'], second.io);
    expect(code2).toBe(0);
    expect(second.out.join('\n')).toContain('homer pull: no-drift');
    expect(second.out.join('\n')).toContain('写入: 0  删除: 0  冲突: 0');
  });

  it('pull-delete 传播：远端删除 → 本地文件被删且备份存在（exit 0）', async () => {
    const h = setupRepo({ 'pi/skills/alpha/SKILL.md': '# alpha v1\n' });
    write(path.join(h.agentRoot, 'skills', 'alpha', 'SKILL.md'), '# alpha v1\n');

    // 远端删掉 alpha（git rm 语义：直接让 store 里不再有该文件）
    fs.rmSync(path.join(h.paths.storeDir, 'pi', 'skills', 'alpha', 'SKILL.md'));
    const sha = commitStore(h.paths, 'remote delete alpha');
    gitOk(h.paths.home, ['push']);

    const io = captureIO();
    const code = await run(['pull', '--home', h.home, '--yes', '--json'], io.io);

    expect(code).toBe(0);
    const report = parseJsonOut(io.out);
    expect(report.applied.deleted).toEqual([{ adapterId: 'pi', category: 'skills', relPath: 'alpha/SKILL.md' }]);
    expect(report.commit).toBe(sha);
    expect(fs.existsSync(path.join(h.agentRoot, 'skills', 'alpha', 'SKILL.md'))).toBe(false);
    expect(read(path.join(report.applied.backupDir as string, 'pi', 'skills', 'alpha', 'SKILL.md'))).toBe('# alpha v1\n');
  });

  it('conflicts-remain → 1（--yes 保留本地）', async () => {
    const h = setupRepo({ 'pi/skills/alpha/SKILL.md': '# alpha v1\n' });
    write(path.join(h.agentRoot, 'skills', 'alpha', 'SKILL.md'), '# alpha local\n');
    advanceRemote(h, { 'pi/skills/alpha/SKILL.md': '# alpha remote\n' });

    const io = captureIO();
    const code = await run(['pull', '--home', h.home, '--yes', '--json'], io.io);

    expect(code).toBe(1);
    const report = parseJsonOut(io.out);
    expect(report.status).toBe('conflicts-remain');
    expect(report.conflicts).toHaveLength(1);
    expect(report.errors.join('\n')).toContain('homer merge');
    expect(read(path.join(h.agentRoot, 'skills', 'alpha', 'SKILL.md'))).toBe('# alpha local\n');
  });

  it('aborted → 1（非 TTY 下没有 --yes 时 confirm 默认拒绝）', async () => {
    const h = setupRepo({ 'pi/skills/alpha/SKILL.md': '# alpha v1\n' });
    write(path.join(h.agentRoot, 'skills', 'alpha', 'SKILL.md'), '# alpha v1\n');
    advanceRemote(h, { 'pi/skills/beta/SKILL.md': '# beta\n' });

    const io = captureIO();
    const code = await run(['pull', '--home', h.home, '--json'], io.io);

    expect(code).toBe(1);
    const report = parseJsonOut(io.out);
    expect(report.status).toBe('aborted');
    expect(fs.existsSync(path.join(h.agentRoot, 'skills', 'beta', 'SKILL.md'))).toBe(false);
  });

  it('前置检查失败 → 1：store 脏（提示先 push）', async () => {
    const h = setupRepo({ 'pi/skills/alpha/SKILL.md': '# alpha v1\n' });
    advanceRemote(h, { 'pi/skills/beta/SKILL.md': '# beta\n' });
    // 本地 store 又脏了（未提交）
    writeStore(h.paths, 'pi/skills/dirty.md', 'dirty\n');

    const io = captureIO();
    const code = await run(['pull', '--home', h.home, '--yes'], io.io);

    expect(code).toBe(1);
    expect(io.err.join('\n')).toContain('store 工作区有未提交的改动');
    expect(io.err.join('\n')).toContain('homer push');
  });

  it('前置检查失败 → 1：无 upstream（提示配置 remote）', async () => {
    const tmp = mkTmp('noupstream');
    const home = path.join(tmp, 'homer');
    const agentRoot = path.join(tmp, 'agent');
    fs.mkdirSync(home, { recursive: true });
    fs.mkdirSync(agentRoot, { recursive: true });
    fs.writeFileSync(
      path.join(home, 'homer.json'),
      `${JSON.stringify({ version: 1, adapters: { pi: { root: agentRoot, enabled: true, categories: { skills: { paths: ['skills/'], mode: 'mirror' } } } } }, null, 2)}\n`,
      'utf8',
    );
    gitOk(home, ['init', '-b', 'main']);
    gitOk(home, ['config', 'user.email', 'homer-pull@example.invalid']);
    gitOk(home, ['config', 'user.name', 'Homer Pull Test']);
    writeStore(getHomerPaths({ HOMER_HOME: home }), 'pi/skills/a.md', 'A\n');
    commitStore(getHomerPaths({ HOMER_HOME: home }), 'store v1');

    const io = captureIO();
    const code = await run(['pull', '--home', home, '--yes'], io.io);

    expect(code).toBe(1);
    expect(io.err.join('\n')).toContain('未配置 git upstream');
  });

  it('前置检查失败 → 1：非 git 仓库（提示先 push 建立历史）', async () => {
    const tmp = mkTmp('norepo');
    fs.mkdirSync(tmp, { recursive: true });
    fs.writeFileSync(
      path.join(tmp, 'homer.json'),
      `${JSON.stringify({ version: 1, adapters: {} }, null, 2)}\n`,
      'utf8',
    );

    const io = captureIO();
    const code = await run(['pull', '--home', tmp, '--yes'], io.io);

    expect(code).toBe(1);
    expect(io.err.join('\n')).toContain('不是 git 仓库');
  });

  it('前置检查失败 → 1：分叉（本地未推送 commit + 远端前进）', async () => {
    const h = setupRepo({ 'pi/skills/alpha/SKILL.md': '# alpha v1\n' });
    // 本地提交一个未推送的 store 变更
    writeStore(h.paths, 'pi/skills/local-only.md', 'local\n');
    commitStore(h.paths, 'local only');
    // 远端也前进（另一个 clone 的视角：直接往 bare 推一个新的 store commit）
    const other = mkTmp('other');
    gitOk(other, ['clone', h.bare, '.']);
    gitOk(other, ['config', 'user.email', 'homer-pull@example.invalid']);
    gitOk(other, ['config', 'user.name', 'Homer Pull Test']);
    write(path.join(other, 'store', 'pi', 'skills', 'remote-only.md'), 'remote\n');
    write(path.join(other, 'store', 'pi', '.homer-complete'), '');
    gitOk(other, ['add', '-A', '--', 'store/']);
    gitOk(other, ['commit', '-m', 'remote only', '--', 'store/']);
    gitOk(other, ['push', 'origin', 'main']);

    const io = captureIO();
    const code = await run(['pull', '--home', h.home, '--yes'], io.io);

    expect(code).toBe(1);
    expect(io.err.join('\n')).toContain('已分叉');
    expect(io.err.join('\n')).toContain('homer push');
    expect(fs.existsSync(path.join(h.agentRoot, 'skills', 'remote-only.md'))).toBe(false);
  });
});
