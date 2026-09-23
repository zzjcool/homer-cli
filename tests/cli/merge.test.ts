/**
 * P3-W9 · `homer merge` 验收（docs/m2-plan.md §2.8 / §3-P3-W9）。
 *
 * 覆盖（计划 §3-P3-W9 专项 + 共同验收基线）：
 *   - `--accept-remote` 批量：工具目录 == remote 内容、state == upstream HEAD、备份目录含 `-merge`；
 *   - `--accept-local` 批量：本地原样保留、push 管线被触发（新 commit 落在 upstream 之上）；
 *   - 逐项交互混合裁决：fake port 记录 `select` 调用序列（顺序 / 选项 / fallback / 提示只含路径与冲突键）；
 *   - no-conflicts → exit 0（零写入）；
 *   - 混合裁决后 push 管线被触发（`git merge-base --is-ancestor <upstream> <new>`）；
 *   - aborted（非交互且无 `--accept-*`）→ exit 1 且零写入；
 *   - `--json` 可 `JSON.parse` 且不含任何文件内容（密钥不进 `--json`）；
 *   - 退出码表（resolved / no-conflicts → 0；aborted / error → 1）；
 *   - 前置检查失败（无 upstream / store 脏 / 非 git 仓库 / 无 config）→ exit 1；
 *   - merge 分类冲突「接受远端」：远端 store 原文 + 植回本地 excluded 键；本地缺失的
 *     `__REQUIRED__` 键被移除 + warning（占位符绝不流入工具目录）；
 *   - 远端删除 + 本地修改 → 接受远端 = 删除本地文件（含备份）。
 *
 * 全程隔离：`mkdtemp` 临时 `HOMER_HOME` + 假 HOME 下的 adapter root + `git init --bare` 假 origin，
 * 绝不碰真实 `~/.homer` / `~/.pi` / 真实 remote。三方快照**手工构造**并经 `deps.sources` 注入，
 * 内容与磁盘 / store 逐字节一致，故「工具目录 == remote 内容」这类断言是精确的。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { runMerge, renderMergeReport, type MergeReport } from '../../src/cli/commands/merge.js';
import { run } from '../../src/cli/index.js';
import { gitExec, isAncestorOf } from '../../src/core/git/index.js';
import { getHomerPaths, type HomerPaths } from '../../src/core/paths.js';
import { loadState, saveState } from '../../src/core/state.js';
import type { PromptPort } from '../../src/cli/ui.js';
import type { AdapterSnapshot, HomerConfig, SnapshotEntry } from '../../src/core/types.js';
import type { SyncSources } from '../../src/core/sync/types.js';

/* ------------------------------------------------------------------ */
/* 临时环境工具                                                        */
/* ------------------------------------------------------------------ */

const created: string[] = [];

function mkTmp(prefix: string): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), `homer-${prefix}-`));
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

function configureUser(repo: string): void {
  gitOk(repo, ['config', 'user.email', 'homer-w9@example.invalid']);
  gitOk(repo, ['config', 'user.name', 'Homer W9']);
}

function write(file: string, content: string): void {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, content, 'utf8');
}

/** store 相对路径（`pi/<category>/<relPath>`）→ 写入 `<home>/store/...`。 */
function writeStoreFiles(home: string, files: Record<string, string>): void {
  for (const [rel, content] of Object.entries(files)) {
    write(path.join(home, 'store', rel), content);
  }
  write(path.join(home, 'store', 'pi', '.homer-complete'), '');
}

/** 工具目录相对路径（`<category>/<relPath>`）→ 写入 adapter root。 */
function writeToolFiles(agentRoot: string, files: Record<string, string>): void {
  for (const [rel, content] of Object.entries(files)) write(path.join(agentRoot, rel), content);
}

interface Harness {
  tmp: string;
  paths: HomerPaths;
  agentRoot: string;
  config: HomerConfig;
  bare: string;
  seedCommit: string;
  remoteCommit: string;
}

interface HarnessOptions {
  /** seed commit（= base / state.lastSyncCommit）的 store 内容。 */
  seed: Record<string, string>;
  /** 远端 commit 的 store 内容。 */
  remote: Record<string, string>;
  /** 工具目录（= local）内容。 */
  local: Record<string, string>;
  /** 额外 category（如带 excludeKeys 的 models）配置。 */
  extraCategories?: Record<string, unknown>;
  /** 不建 upstream（测前置检查）。 */
  noUpstream?: boolean;
  /** 不建 git 仓库（测前置检查）。 */
  noGit?: boolean;
}

const DEFAULT_SETTINGS_CATEGORY = { paths: ['settings.json'], mode: 'merge' as const };
const DEFAULT_SKILLS_CATEGORY = { paths: ['skills/'], mode: 'mirror' as const };

function buildConfig(agentRoot: string, extraCategories: Record<string, unknown> = {}): HomerConfig {
  return {
    version: 1,
    adapters: {
      pi: {
        root: agentRoot,
        enabled: true,
        categories: {
          settings: DEFAULT_SETTINGS_CATEGORY,
          skills: DEFAULT_SKILLS_CATEGORY,
          ...extraCategories,
        },
      },
    },
  };
}

/**
 * 搭一个真实 git 环境：home 仓库（seed commit + upstream）+ 从 bare origin clone 出的
 * 「另一台机器」提交 remote commit 并 push → home 侧 HEAD = seed（remote 已前进）。
 */
function makeHarness(opts: HarnessOptions): Harness {
  const tmp = mkTmp('merge');
  const home = path.join(tmp, 'homer');
  const agentRoot = path.join(tmp, 'home', '.pi', 'agent');
  fs.mkdirSync(home, { recursive: true });
  fs.mkdirSync(agentRoot, { recursive: true });

  const config = buildConfig(agentRoot, opts.extraCategories ?? {});
  write(path.join(home, 'homer.json'), `${JSON.stringify(config, null, 2)}\n`);
  writeStoreFiles(home, opts.seed);
  writeToolFiles(agentRoot, opts.local);

  const paths = getHomerPaths({ HOMER_HOME: home });

  if (opts.noGit === true) {
    return { tmp, paths, agentRoot, config, bare: '', seedCommit: '', remoteCommit: '' };
  }

  gitOk(home, ['init', '-b', 'main']);
  configureUser(home);
  gitOk(home, ['add', '-A', '--', 'store/']);
  gitOk(home, ['commit', '-m', 'seed']);
  const seedCommit = gitOk(home, ['rev-parse', 'HEAD']).trim();

  if (opts.noUpstream === true) {
    return { tmp, paths, agentRoot, config, bare: '', seedCommit, remoteCommit: seedCommit };
  }

  const bare = path.join(tmp, 'origin.git');
  gitOk(tmp, ['init', '--bare', '-b', 'main', bare]);
  gitOk(home, ['remote', 'add', 'origin', bare]);
  gitOk(home, ['push', '-u', 'origin', 'main']);

  // 另一台机器的 commit：把 store 精确同步成 `opts.remote`（含删除）→ commit → push 到 bare。
  const clone = path.join(tmp, 'other-machine');
  gitOk(tmp, ['clone', bare, clone]);
  configureUser(clone);
  fs.rmSync(path.join(clone, 'store'), { recursive: true, force: true });
  writeStoreFiles(clone, opts.remote);
  gitOk(clone, ['add', '-A', '--', 'store/']);
  // `--allow-empty`：remote 内容与 seed 相同时（如 no-conflicts 用例）也要产生一个
  // 不同的 upstream commit，使「HEAD 是 upstream 祖先」的 ff 前置检查真实成立。
  gitOk(clone, ['commit', '-m', 'remote', '--allow-empty']);
  gitOk(clone, ['push']);
  const remoteCommit = gitOk(clone, ['rev-parse', 'HEAD']).trim();

  // state 指向 seed：merge 的 base = seed 的 store。
  saveState(paths, { version: 1, lastSyncCommit: seedCommit, lastSyncCommand: 'push' });

  return { tmp, paths, agentRoot, config, bare, seedCommit, remoteCommit };
}

/* ------------------------------------------------------------------ */
/* 手工三方快照                                                        */
/* ------------------------------------------------------------------ */

function fileEntry(content: string): SnapshotEntry {
  return { kind: 'file', content };
}

function jsonEntry(content: string): SnapshotEntry {
  return { kind: 'json', content };
}

interface SnapshotInput {
  settings?: string;
  skills?: Record<string, string>;
  models?: string;
}

/** 按固定 category 顺序（settings → skills → models，与 config 声明顺序一致）构造快照。 */
function snapshotsOf(input: SnapshotInput): AdapterSnapshot[] {
  const categories = [];

  if (input.settings !== undefined) {
    categories.push({
      adapterId: 'pi',
      category: 'settings',
      mode: 'merge' as const,
      files: new Map([['settings.json', jsonEntry(input.settings)]]),
    });
  }
  if (input.skills !== undefined) {
    categories.push({
      adapterId: 'pi',
      category: 'skills',
      mode: 'mirror' as const,
      files: new Map(Object.entries(input.skills).map(([rel, content]) => [rel, fileEntry(content)])),
    });
  }
  if (input.models !== undefined) {
    categories.push({
      adapterId: 'pi',
      category: 'models',
      mode: 'merge' as const,
      files: new Map([['models.json', jsonEntry(input.models)]]),
    });
  }

  return [{ adapterId: 'pi', categories }];
}

function sourcesOf(base: SnapshotInput, local: SnapshotInput, remote: SnapshotInput): SyncSources {
  return {
    mode: 'git',
    base: snapshotsOf(base),
    local: snapshotsOf(local),
    remote: snapshotsOf(remote),
    warnings: [],
    errors: [],
  };
}

/* ------------------------------------------------------------------ */
/* prompt port 替身                                                    */
/* ------------------------------------------------------------------ */

interface SelectCall {
  message: string;
  options: readonly { value: string; label: string }[];
  fallback: string;
}

function fakePort(decide: (message: string) => 'local' | 'remote'): { port: PromptPort; selects: SelectCall[] } {
  const selects: SelectCall[] = [];
  const port: PromptPort = {
    async confirm(): Promise<boolean> {
      throw new Error('homer merge 不应调用 confirm（无确认步骤）');
    },
    async select<T extends string>(
      message: string,
      options: readonly { value: T; label: string }[],
      fallback: T,
    ): Promise<T> {
      selects.push({ message, options, fallback });
      return decide(message) as T;
    },
  };
  return { port, selects };
}

/* ------------------------------------------------------------------ */
/* 断言工具                                                            */
/* ------------------------------------------------------------------ */

function readTool(agentRoot: string, rel: string): string {
  return fs.readFileSync(path.join(agentRoot, rel), 'utf8');
}

function target(resolution: { adapterId: string; category: string; relPath: string }): string {
  return `${resolution.adapterId}/${resolution.category}/${resolution.relPath}`;
}

function backupDirOf(report: MergeReport): string {
  const dir = report.applied.backupDir;
  expect(dir).toBeDefined();
  return dir as string;
}

/** CLI 子进程内驱动（含 JSON 输出 / 退出码），返回 { code, stdout, stderr }。 */
async function runCli(argv: readonly string[]): Promise<{ code: number; stdout: string; stderr: string }> {
  const stdout: string[] = [];
  const stderr: string[] = [];
  const code = await run([...argv], {
    out: (line) => stdout.push(line),
    err: (line) => stderr.push(line),
  });
  return { code, stdout: stdout.join('\n'), stderr: stderr.join('\n') };
}

/* ================================================================== */
/* A. --accept-remote（批量）                                          */
/* ================================================================== */

describe('merge — --accept-remote 批量裁决', () => {
  it('两个冲突（mirror + merge-keys）全选远端：内容 / state / 备份 / 计数', async () => {
    const h = makeHarness({
      seed: {
        'pi/settings/settings.json': '{"theme":"base","keep":1}\n',
        'pi/skills/foo/SKILL.md': '# foo base\n',
      },
      remote: {
        'pi/settings/settings.json': '{"theme":"remote","keep":1}\n',
        'pi/skills/foo/SKILL.md': '# foo remote\n',
      },
      local: {
        'settings.json': '{"theme":"local","keep":1,"LOCAL_TOKEN":"LOCAL-ONLY-SECRET"}\n',
        'skills/foo/SKILL.md': '# foo local\n',
      },
    });

    const sources = sourcesOf(
      { settings: '{"theme":"base","keep":1}\n', skills: { 'foo/SKILL.md': '# foo base\n' } },
      { settings: '{"theme":"local","keep":1,"LOCAL_TOKEN":"LOCAL-ONLY-SECRET"}\n', skills: { 'foo/SKILL.md': '# foo local\n' } },
      { settings: '{"theme":"remote","keep":1}\n', skills: { 'foo/SKILL.md': '# foo remote\n' } },
    );

    const report = await runMerge({ homerHome: h.paths.home, acceptRemote: true }, { sources });

    expect(report.status).toBe('resolved');
    expect(report.ok).toBe(true);

    // 两条冲突都裁决为远端（顺序 = planPull 的动作顺序）
    expect(report.resolutions.map((r) => r.choice)).toEqual(['remote', 'remote']);
    expect(report.resolutions.map(target).sort()).toEqual(['pi/settings/settings.json', 'pi/skills/foo/SKILL.md']);

    // 工具目录 == remote 内容（逐字节）
    expect(readTool(h.agentRoot, 'settings.json')).toBe('{"theme":"remote","keep":1}\n');
    expect(readTool(h.agentRoot, 'skills/foo/SKILL.md')).toBe('# foo remote\n');

    // applied 计数
    expect(report.applied.written.length).toBe(2);
    expect(report.applied.deleted).toEqual([]);

    // 备份目录名含 '-merge'（W6 缺口：applyPullActions 的备份命令分量）
    const backupDir = backupDirOf(report);
    expect(path.basename(backupDir)).toMatch(/^\d{6}-merge$/);
    expect(backupDir.startsWith(h.paths.backupsDir)).toBe(true);
    // 备份内容 == 覆盖前（本地）内容
    expect(fs.readFileSync(path.join(backupDir, 'pi', 'skills', 'foo', 'SKILL.md'), 'utf8')).toBe('# foo local\n');

    // ff 后无 drift（local == remote）→ 不触发 push 管线 → state = upstream HEAD
    expect(report.commit).toBeUndefined();
    const state = loadState(h.paths);
    expect(state.lastSyncCommit).toBe(h.remoteCommit);
    expect(state.lastSyncCommand).toBe('merge');

    // store 已 ff 到远端（工作区 = remote commit 的内容）
    expect(fs.readFileSync(path.join(h.paths.storeDir, 'pi', 'skills', 'foo', 'SKILL.md'), 'utf8')).toBe('# foo remote\n');
  });

  it('远端删除 + 本地修改：接受远端 = 删除本地文件（含备份）', async () => {
    const h = makeHarness({
      seed: {
        'pi/settings/settings.json': '{"theme":"base"}\n',
        'pi/skills/foo/SKILL.md': '# foo base\n',
        'pi/skills/bar/SKILL.md': '# bar\n',
      },
      remote: {
        'pi/settings/settings.json': '{"theme":"base"}\n',
        'pi/skills/bar/SKILL.md': '# bar\n',
      },
      local: {
        'settings.json': '{"theme":"base"}\n',
        'skills/foo/SKILL.md': '# foo local\n',
        'skills/bar/SKILL.md': '# bar\n',
      },
    });

    const sources = sourcesOf(
      { settings: '{"theme":"base"}\n', skills: { 'foo/SKILL.md': '# foo base\n', 'bar/SKILL.md': '# bar\n' } },
      { settings: '{"theme":"base"}\n', skills: { 'foo/SKILL.md': '# foo local\n', 'bar/SKILL.md': '# bar\n' } },
      { settings: '{"theme":"base"}\n', skills: { 'bar/SKILL.md': '# bar\n' } },
    );

    const report = await runMerge({ homerHome: h.paths.home, acceptRemote: true }, { sources });

    expect(report.status).toBe('resolved');
    expect(report.resolutions).toHaveLength(1);
    expect(report.resolutions[0]?.choice).toBe('remote');
    expect(report.applied.deleted).toEqual([{ adapterId: 'pi', category: 'skills', relPath: 'foo/SKILL.md' }]);
    // 本地文件被删除且备份保留
    expect(fs.existsSync(path.join(h.agentRoot, 'skills', 'foo', 'SKILL.md'))).toBe(false);
    const backupDir = backupDirOf(report);
    expect(path.basename(backupDir)).toMatch(/^\d{6}-merge$/);
    expect(fs.readFileSync(path.join(backupDir, 'pi', 'skills', 'foo', 'SKILL.md'), 'utf8')).toBe('# foo local\n');
  });
});

/* ================================================================== */
/* B. --accept-local（批量）→ push 管线                                */
/* ================================================================== */

describe('merge — --accept-local 批量裁决', () => {
  it('本地原样保留；push 管线被触发（新 commit 落在 upstream 之上），state = 新 HEAD', async () => {
    const h = makeHarness({
      seed: {
        'pi/settings/settings.json': '{"theme":"base","keep":1}\n',
        'pi/skills/foo/SKILL.md': '# foo base\n',
      },
      remote: {
        'pi/settings/settings.json': '{"theme":"remote","keep":1}\n',
        'pi/skills/foo/SKILL.md': '# foo remote\n',
      },
      local: {
        'settings.json': '{"theme":"local","keep":1}\n',
        'skills/foo/SKILL.md': '# foo local\n',
      },
    });

    const sources = sourcesOf(
      { settings: '{"theme":"base","keep":1}\n', skills: { 'foo/SKILL.md': '# foo base\n' } },
      { settings: '{"theme":"local","keep":1}\n', skills: { 'foo/SKILL.md': '# foo local\n' } },
      { settings: '{"theme":"remote","keep":1}\n', skills: { 'foo/SKILL.md': '# foo remote\n' } },
    );

    const report = await runMerge({ homerHome: h.paths.home, acceptLocal: true }, { sources });

    expect(report.status).toBe('resolved');
    expect(report.resolutions.map((r) => r.choice)).toEqual(['local', 'local']);

    // 工具目录未被触碰（保留本地），因此无写入 / 无备份
    expect(readTool(h.agentRoot, 'settings.json')).toBe('{"theme":"local","keep":1}\n');
    expect(readTool(h.agentRoot, 'skills/foo/SKILL.md')).toBe('# foo local\n');
    expect(report.applied.written).toEqual([]);
    expect(report.applied.backupDir).toBeUndefined();

    // push 管线：新 commit 在 upstream 之上（ff 后的 HEAD 之上）
    const commit = report.commit as string;
    expect(commit).toBeDefined();
    expect(commit).not.toBe(h.remoteCommit);
    expect(isAncestorOf(h.paths.home, h.remoteCommit, commit)).toBe(true);
    expect(gitOk(h.paths.home, ['rev-parse', 'HEAD']).trim()).toBe(commit);

    // store 里现在记录的是本地版本的意图真相
    expect(fs.readFileSync(path.join(h.paths.storeDir, 'pi', 'skills', 'foo', 'SKILL.md'), 'utf8')).toBe('# foo local\n');
    // state = 最终 HEAD
    expect(loadState(h.paths).lastSyncCommit).toBe(commit);
  });

  it('accept-local 也应用远端 clean 动作（不静默丢远端新增文件）', async () => {
    const h = makeHarness({
      seed: {
        'pi/settings/settings.json': '{"theme":"base"}\n',
        'pi/skills/foo/SKILL.md': '# foo base\n',
      },
      remote: {
        'pi/settings/settings.json': '{"theme":"remote"}\n',
        'pi/skills/foo/SKILL.md': '# foo base\n',
        'pi/skills/added-remote/SKILL.md': '# added by remote\n',
      },
      local: {
        'settings.json': '{"theme":"local"}\n',
        'skills/foo/SKILL.md': '# foo base\n',
      },
    });

    const sources = sourcesOf(
      { settings: '{"theme":"base"}\n', skills: { 'foo/SKILL.md': '# foo base\n' } },
      { settings: '{"theme":"local"}\n', skills: { 'foo/SKILL.md': '# foo base\n' } },
      { settings: '{"theme":"remote"}\n', skills: { 'foo/SKILL.md': '# foo base\n', 'added-remote/SKILL.md': '# added by remote\n' } },
    );

    const report = await runMerge({ homerHome: h.paths.home, acceptLocal: true }, { sources });

    // 冲突（settings theme）保留本地；远端新增的 clean 文件被写入工具目录
    expect(report.resolutions.map((r) => r.choice)).toEqual(['local']);
    expect(readTool(h.agentRoot, 'settings.json')).toBe('{"theme":"local"}\n');
    expect(readTool(h.agentRoot, 'skills/added-remote/SKILL.md')).toBe('# added by remote\n');
    expect(report.applied.written).toEqual([{ adapterId: 'pi', category: 'skills', relPath: 'added-remote/SKILL.md' }]);
    // push 管线照常（本地冲突项仍是 push 方向）
    expect(report.commit).toBeDefined();
  });

  it('accept-local 也应用远端 clean 删除（远端删除 + 本地未改）', async () => {
    const h = makeHarness({
      seed: {
        'pi/settings/settings.json': '{"theme":"base"}\n',
        'pi/skills/foo/SKILL.md': '# foo base\n',
        'pi/skills/gone/SKILL.md': '# gone\n',
      },
      remote: {
        'pi/settings/settings.json': '{"theme":"remote"}\n',
        'pi/skills/foo/SKILL.md': '# foo base\n',
      },
      local: {
        'settings.json': '{"theme":"local"}\n',
        'skills/foo/SKILL.md': '# foo base\n',
        'skills/gone/SKILL.md': '# gone\n',
      },
    });

    const sources = sourcesOf(
      { settings: '{"theme":"base"}\n', skills: { 'foo/SKILL.md': '# foo base\n', 'gone/SKILL.md': '# gone\n' } },
      { settings: '{"theme":"local"}\n', skills: { 'foo/SKILL.md': '# foo base\n', 'gone/SKILL.md': '# gone\n' } },
      { settings: '{"theme":"remote"}\n', skills: { 'foo/SKILL.md': '# foo base\n' } },
    );

    const report = await runMerge({ homerHome: h.paths.home, acceptLocal: true }, { sources });

    expect(report.status).toBe('resolved');
    // 远端删除传播到工具目录（clean delete），并备份被删除的本地文件
    expect(report.applied.deleted).toEqual([{ adapterId: 'pi', category: 'skills', relPath: 'gone/SKILL.md' }]);
    expect(fs.existsSync(path.join(h.agentRoot, 'skills', 'gone', 'SKILL.md'))).toBe(false);
    const backupDir = backupDirOf(report);
    expect(path.basename(backupDir)).toMatch(/^\d{6}-merge$/);
    expect(fs.readFileSync(path.join(backupDir, 'pi', 'skills', 'gone', 'SKILL.md'), 'utf8')).toBe('# gone\n');
    // 冲突（settings）保留本地 → 新 commit 落在 upstream 之上
    expect(isAncestorOf(h.paths.home, h.remoteCommit, report.commit as string)).toBe(true);
  });
});

/* ================================================================== */
/* C. 逐项交互（fake port）                                            */
/* ================================================================== */

describe('merge — 逐项交互裁决', () => {
  it('按 select 调用序列裁决，提示只含路径 / 原因 / 冲突键（不含文件内容）', async () => {
    const h = makeHarness({
      seed: {
        'pi/settings/settings.json': '{"theme":"base","keep":1}\n',
        'pi/skills/foo/SKILL.md': '# foo base\n',
      },
      remote: {
        'pi/settings/settings.json': '{"theme":"remote","keep":1}\n',
        'pi/skills/foo/SKILL.md': '# foo remote\n',
      },
      local: {
        'settings.json': '{"theme":"local","keep":1,"LOCAL_TOKEN":"LOCAL-ONLY-SECRET"}\n',
        'skills/foo/SKILL.md': '# foo local\n',
      },
    });

    const sources = sourcesOf(
      { settings: '{"theme":"base","keep":1}\n', skills: { 'foo/SKILL.md': '# foo base\n' } },
      { settings: '{"theme":"local","keep":1,"LOCAL_TOKEN":"LOCAL-ONLY-SECRET"}\n', skills: { 'foo/SKILL.md': '# foo local\n' } },
      { settings: '{"theme":"remote","keep":1}\n', skills: { 'foo/SKILL.md': '# foo remote\n' } },
    );

    // merge 分类（settings）→ 远端；mirror 分类（skills）→ 本地（混合裁决）
    const { port, selects } = fakePort((message) => (message.includes('settings') ? 'remote' : 'local'));

    const report = await runMerge({ homerHome: h.paths.home }, { sources, ui: port });

    expect(report.status).toBe('resolved');
    expect(selects).toHaveLength(2);

    // 调用序列与裁决顺序一致；选项恒为 Accept Local / Accept Remote（无 skip）；fallback = local
    for (const [index, call] of selects.entries()) {
      const resolution = report.resolutions[index];
      expect(resolution).toBeDefined();
      expect(call.message).toContain(target(resolution as never));
      expect(call.options.map((option) => option.label)).toEqual(['Accept Local', 'Accept Remote']);
      expect(call.options.map((option) => option.value)).toEqual(['local', 'remote']);
      expect(call.fallback).toBe('local');
    }

    // 提示里带原因与冲突键，但绝不带文件内容（密钥不进终端 / json）
    const settingsSelect = selects.find((call) => call.message.includes('settings'));
    expect(settingsSelect?.message).toContain('冲突键: theme');
    for (const call of selects) {
      expect(call.message).not.toContain('LOCAL-ONLY-SECRET');
      expect(call.message).not.toContain('# foo local\n');
    }
    const skillsSelect = selects.find((call) => call.message.includes('skills'));
    expect(skillsSelect?.message).toContain('双方各自修改');

    // 裁决结果：settings 落远端、skills 保留本地
    expect(readTool(h.agentRoot, 'settings.json')).toBe('{"theme":"remote","keep":1}\n');
    expect(readTool(h.agentRoot, 'skills/foo/SKILL.md')).toBe('# foo local\n');

    // 混合裁决 → 有本地意图要发布 → push 管线触发，新 commit 落在 upstream 之上
    const commit = report.commit as string;
    expect(commit).toBeDefined();
    expect(isAncestorOf(h.paths.home, h.remoteCommit, commit)).toBe(true);
    expect(loadState(h.paths).lastSyncCommit).toBe(commit);
  });

  it('非交互且未给裁决选项 → aborted（exit 1 语义），零写入', async () => {
    const h = makeHarness({
      seed: {
        'pi/settings/settings.json': '{"theme":"base"}\n',
        'pi/skills/foo/SKILL.md': '# foo base\n',
      },
      remote: {
        'pi/settings/settings.json': '{"theme":"remote"}\n',
        'pi/skills/foo/SKILL.md': '# foo remote\n',
      },
      local: {
        'settings.json': '{"theme":"local"}\n',
        'skills/foo/SKILL.md': '# foo local\n',
      },
    });

    const sources = sourcesOf(
      { settings: '{"theme":"base"}\n', skills: { 'foo/SKILL.md': '# foo base\n' } },
      { settings: '{"theme":"local"}\n', skills: { 'foo/SKILL.md': '# foo local\n' } },
      { settings: '{"theme":"remote"}\n', skills: { 'foo/SKILL.md': '# foo remote\n' } },
    );

    const report = await runMerge({ homerHome: h.paths.home }, { sources });

    expect(report.status).toBe('aborted');
    expect(report.ok).toBe(false);
    expect(report.resolutions).toEqual([]);
    expect(report.applied).toEqual({ written: [], deleted: [], conflicts: [] });
    expect(report.warnings.join(' ')).toContain('--accept-local');

    // 零写入：工具目录 / 备份 / store / state 全部原样
    expect(readTool(h.agentRoot, 'settings.json')).toBe('{"theme":"local"}\n');
    expect(fs.existsSync(h.paths.backupsDir)).toBe(false);
    expect(gitOk(h.paths.home, ['rev-parse', 'HEAD']).trim()).toBe(h.seedCommit);
    expect(loadState(h.paths).lastSyncCommit).toBe(h.seedCommit);
  });

  it('同时给出 --accept-local 与 --accept-remote → CliError（矛盾选项）', async () => {
    const h = makeHarness({
      seed: { 'pi/settings/settings.json': '{"theme":"base"}\n' },
      remote: { 'pi/settings/settings.json': '{"theme":"remote"}\n' },
      local: { 'settings.json': '{"theme":"local"}\n' },
    });

    await expect(
      runMerge(
        { homerHome: h.paths.home, acceptLocal: true, acceptRemote: true },
        { sources: sourcesOf({ settings: '{"theme":"base"}\n' }, { settings: '{"theme":"local"}\n' }, { settings: '{"theme":"remote"}\n' }) },
      ),
    ).rejects.toThrow(/不能同时指定/);
  });
});

/* ================================================================== */
/* D. no-conflicts                                                     */
/* ================================================================== */

describe('merge — 无冲突', () => {
  it('三方一致 → no-conflicts，零写入，exit 0', async () => {
    const h = makeHarness({
      seed: { 'pi/settings/settings.json': '{"theme":"same"}\n' },
      remote: { 'pi/settings/settings.json': '{"theme":"same"}\n' },
      local: { 'settings.json': '{"theme":"same"}\n' },
    });

    const sources = sourcesOf(
      { settings: '{"theme":"same"}\n' },
      { settings: '{"theme":"same"}\n' },
      { settings: '{"theme":"same"}\n' },
    );

    const report = await runMerge({ homerHome: h.paths.home }, { sources, noFetch: true });

    expect(report.status).toBe('no-conflicts');
    expect(report.applied.written).toEqual([]);
    expect(fs.existsSync(h.paths.backupsDir)).toBe(false);
    // 无冲突不 ff、不动 state（只报「无需 merge」）
    expect(loadState(h.paths).lastSyncCommit).toBe(h.seedCommit);

    const cli = await runCli(['merge', '--home', h.paths.home, '--accept-remote']);
    expect(cli.code).toBe(0);
    expect(cli.stdout).toContain('no-conflicts');
  });

  it('只有本地 push 方向漂移 → no-conflicts（merge 只处理冲突）', async () => {
    const h = makeHarness({
      seed: { 'pi/settings/settings.json': '{"theme":"base"}\n' },
      remote: { 'pi/settings/settings.json': '{"theme":"base"}\n' },
      local: { 'settings.json': '{"theme":"local"}\n' },
    });

    const sources = sourcesOf(
      { settings: '{"theme":"base"}\n' },
      { settings: '{"theme":"local"}\n' },
      { settings: '{"theme":"base"}\n' },
    );

    const report = await runMerge({ homerHome: h.paths.home, acceptRemote: true }, { sources });
    expect(report.status).toBe('no-conflicts');
    expect(report.resolutions).toEqual([]);
    expect(readTool(h.agentRoot, 'settings.json')).toBe('{"theme":"local"}\n');
  });

  it('无冲突但远端有 clean 变更 → no-conflicts 且零写入（clean 变更归 `homer pull`）', async () => {
    const h = makeHarness({
      seed: {
        'pi/settings/settings.json': '{"theme":"base"}\n',
        'pi/skills/foo/SKILL.md': '# foo base\n',
      },
      remote: {
        'pi/settings/settings.json': '{"theme":"base"}\n',
        'pi/skills/foo/SKILL.md': '# foo base\n',
        'pi/skills/added-remote/SKILL.md': '# added by remote\n',
      },
      local: {
        'settings.json': '{"theme":"base"}\n',
        'skills/foo/SKILL.md': '# foo base\n',
      },
    });

    const sources = sourcesOf(
      { settings: '{"theme":"base"}\n', skills: { 'foo/SKILL.md': '# foo base\n' } },
      { settings: '{"theme":"base"}\n', skills: { 'foo/SKILL.md': '# foo base\n' } },
      { settings: '{"theme":"base"}\n', skills: { 'foo/SKILL.md': '# foo base\n', 'added-remote/SKILL.md': '# added by remote\n' } },
    );

    const report = await runMerge({ homerHome: h.paths.home, acceptRemote: true }, { sources });

    expect(report.status).toBe('no-conflicts');
    expect(report.applied).toEqual({ written: [], deleted: [], conflicts: [] });
    // 远端新增的 clean 文件**没有**被写进工具目录（那是 pull 的职责），也未 ff / 未动 state
    expect(fs.existsSync(path.join(h.agentRoot, 'skills', 'added-remote', 'SKILL.md'))).toBe(false);
    expect(gitOk(h.paths.home, ['rev-parse', 'HEAD']).trim()).toBe(h.seedCommit);
    expect(fs.existsSync(h.paths.backupsDir)).toBe(false);
  });
});

/* ================================================================== */
/* E. merge 分类冲突的 excludeKeys 语义（接受远端）                    */
/* ================================================================== */

describe('merge — 接受远端时的 excludeKeys 语义', () => {
  const MODELS = { models: { paths: ['models.json'], mode: 'merge' as const, excludeKeys: ['apiKey'] } };

  it('远端 store 原文 + 植回本地 excluded 键（本地密钥不被覆盖，占位符不落盘）', async () => {
    const h = makeHarness({
      seed: { 'pi/models/models.json': '{"model":"base","apiKey":"BASE-KEY"}\n' },
      remote: { 'pi/models/models.json': '{"model":"remote","apiKey":"__REQUIRED__"}\n' },
      local: { 'models.json': '{"model":"local","apiKey":"LOCAL-KEY"}\n' },
      extraCategories: MODELS,
    });

    const sources = sourcesOf(
      { models: '{"model":"base","apiKey":"BASE-KEY"}\n' },
      { models: '{"model":"local","apiKey":"LOCAL-KEY"}\n' },
      { models: '{"model":"remote","apiKey":"__REQUIRED__"}\n' },
    );

    const report = await runMerge({ homerHome: h.paths.home, acceptRemote: true }, { sources });

    expect(report.status).toBe('resolved');
    expect(report.resolutions[0]?.keyPaths).toEqual(['model']);

    const written = readTool(h.agentRoot, 'models.json');
    expect(written).toContain('"model": "remote"');   // 远端值生效
    expect(written).toContain('"apiKey": "LOCAL-KEY"'); // 本地密钥植回，永不被远端覆盖
    expect(written).not.toContain('__REQUIRED__');     // store 占位符绝不流入工具目录
    expect(report.warnings).toEqual([]);
  });

  it('本地删除 + 远端修改（含占位符）→ 接受远端：重建内容、无占位符 + warning', async () => {
    const h = makeHarness({
      seed: { 'pi/models/models.json': '{"model":"base","apiKey":"BASE-KEY"}\n' },
      remote: { 'pi/models/models.json': '{"model":"remote","apiKey":"__REQUIRED__"}\n' },
      // 工具目录里根本没有 models.json（本地删除）
      local: {},
      extraCategories: MODELS,
    });

    const sources = sourcesOf(
      { models: '{"model":"base","apiKey":"BASE-KEY"}\n' },
      {}, // local 缺失
      { models: '{"model":"remote","apiKey":"__REQUIRED__"}\n' },
    );

    const report = await runMerge({ homerHome: h.paths.home, acceptRemote: true }, { sources });

    expect(report.status).toBe('resolved');
    expect(report.resolutions[0]?.choice).toBe('remote');
    const written = readTool(h.agentRoot, 'models.json');
    expect(written).toContain('"model": "remote"');
    // 本地缺失该必填项 → 占位符键被摘掉 + warning；store 占位符绝不落盘
    expect(written).not.toContain('__REQUIRED__');
    expect(report.warnings.join(' ')).toContain('apiKey');
  });

  it('本地删除该必填项 → 占位符键被移除 + warning', async () => {
    const h = makeHarness({
      seed: { 'pi/models/models.json': '{"model":"base"}\n' },
      remote: { 'pi/models/models.json': '{"model":"remote","apiKey":"__REQUIRED__"}\n' },
      local: { 'models.json': '{"model":"local"}\n' },
      extraCategories: MODELS,
    });

    const sources = sourcesOf(
      { models: '{"model":"base"}\n' },
      { models: '{"model":"local"}\n' },
      { models: '{"model":"remote","apiKey":"__REQUIRED__"}\n' },
    );

    const report = await runMerge({ homerHome: h.paths.home, acceptRemote: true }, { sources });

    const written = readTool(h.agentRoot, 'models.json');
    expect(written).not.toContain('__REQUIRED__');
    expect(written).not.toContain('apiKey');
    expect(report.warnings.join(' ')).toContain('apiKey');
  });
});

/* ================================================================== */
/* F. CLI：--json / 退出码表 / 前置检查                                */
/* ================================================================== */

describe('merge — CLI 集成（--json / 退出码 / 前置检查）', () => {
  it('--json 可 parse、与文本计数一致、不含任何文件内容', async () => {
    const h = makeHarness({
      seed: {
        'pi/settings/settings.json': '{"theme":"base"}\n',
        'pi/skills/foo/SKILL.md': '# foo base\n',
      },
      remote: {
        'pi/settings/settings.json': '{"theme":"remote"}\n',
        'pi/skills/foo/SKILL.md': '# foo remote\n',
      },
      local: {
        'settings.json': '{"theme":"local","LOCAL_TOKEN":"LOCAL-ONLY-SECRET"}\n',
        'skills/foo/SKILL.md': '# foo local\n',
      },
    });

    const cli = await runCli(['merge', '--home', h.paths.home, '--accept-remote', '--json']);
    expect(cli.code).toBe(0);

    const report = JSON.parse(cli.stdout) as MergeReport;
    expect(report.status).toBe('resolved');
    expect(report.resolutions).toHaveLength(2);
    expect(report.applied.written).toHaveLength(2);

    // 与文本渲染的计数一致
    const text = renderMergeReport(report);
    expect(text).toContain(`写入: ${report.applied.written.length}`);

    // --json 不含文件内容（密钥 / 文件正文都不出去）；只给路径与冲突键
    expect(cli.stdout).not.toContain('LOCAL-ONLY-SECRET');
    expect(cli.stdout).not.toContain('# foo local');
    expect(report.resolutions[0]?.keyPaths).toEqual(['theme']);
    expect(path.basename(report.applied.backupDir as string)).toMatch(/^\d{6}-merge$/);
  });

  it('退出码表：resolved / no-conflicts → 0；aborted → 1；无 config → 1', async () => {
    const conflicting = makeHarness({
      seed: { 'pi/settings/settings.json': '{"theme":"base"}\n' },
      remote: { 'pi/settings/settings.json': '{"theme":"remote"}\n' },
      local: { 'settings.json': '{"theme":"local"}\n' },
    });

    const resolved = await runCli(['merge', '--home', conflicting.paths.home, '--accept-local']);
    expect(resolved.code).toBe(0);
    expect(resolved.stdout).toContain('resolved');

    // aborted：冲突存在、非交互（vitest 无 TTY）且未给任何 --accept-* → exit 1、零写入
    const abortedHarness = makeHarness({
      seed: { 'pi/settings/settings.json': '{"theme":"base"}\n' },
      remote: { 'pi/settings/settings.json': '{"theme":"remote"}\n' },
      local: { 'settings.json': '{"theme":"local"}\n' },
    });
    const aborted = await runCli(['merge', '--home', abortedHarness.paths.home]);
    expect(aborted.code).toBe(1);
    expect(aborted.stdout).toContain('aborted');
    expect(readTool(abortedHarness.agentRoot, 'settings.json')).toBe('{"theme":"local"}\n');
    expect(gitOk(abortedHarness.paths.home, ['rev-parse', 'HEAD']).trim()).toBe(abortedHarness.seedCommit);

    // no-conflicts → 0
    const clean = makeHarness({
      seed: { 'pi/settings/settings.json': '{"theme":"same"}\n' },
      remote: { 'pi/settings/settings.json': '{"theme":"same"}\n' },
      local: { 'settings.json': '{"theme":"same"}\n' },
    });
    const noConflicts = await runCli(['merge', '--home', clean.paths.home, '--accept-remote']);
    expect(noConflicts.code).toBe(0);
    expect(noConflicts.stdout).toContain('no-conflicts');

    // error：无 homer.json → CliError → 打印 + exit 1
    const emptyHome = path.join(mkTmp('merge-empty'), 'homer');
    fs.mkdirSync(emptyHome, { recursive: true });
    const errored = await runCli(['merge', '--home', emptyHome, '--accept-remote']);
    expect(errored.code).toBe(1);
    expect(errored.stderr).toContain('homer init');
  });

  it('前置检查：无 upstream / store 脏 / 非 git 仓库 → exit 1（任何写入之前）', async () => {
    const noUpstream = makeHarness({
      seed: { 'pi/settings/settings.json': '{"theme":"base"}\n' },
      remote: { 'pi/settings/settings.json': '{"theme":"base"}\n' },
      local: { 'settings.json': '{"theme":"local"}\n' },
      noUpstream: true,
    });
    const upstreamResult = await runCli(['merge', '--home', noUpstream.paths.home, '--accept-remote']);
    expect(upstreamResult.code).toBe(1);
    expect(upstreamResult.stderr).toContain('upstream');

    const dirty = makeHarness({
      seed: { 'pi/settings/settings.json': '{"theme":"base"}\n' },
      remote: { 'pi/settings/settings.json': '{"theme":"remote"}\n' },
      local: { 'settings.json': '{"theme":"local"}\n' },
    });
    write(path.join(dirty.paths.storeDir, 'pi', 'skills', 'uncommitted.md'), 'dirty\n');
    const dirtyResult = await runCli(['merge', '--home', dirty.paths.home, '--accept-remote']);
    expect(dirtyResult.code).toBe(1);
    expect(dirtyResult.stderr).toContain('未提交');
    // 脏 store 时零写入：local 文件与 backups 都没动
    expect(readTool(dirty.agentRoot, 'settings.json')).toBe('{"theme":"local"}\n');
    expect(fs.existsSync(dirty.paths.backupsDir)).toBe(false);

    const noGit = makeHarness({
      seed: { 'pi/settings/settings.json': '{"theme":"base"}\n' },
      remote: {},
      local: { 'settings.json': '{"theme":"local"}\n' },
      noGit: true,
    });
    const noGitResult = await runCli(['merge', '--home', noGit.paths.home, '--accept-remote']);
    expect(noGitResult.code).toBe(1);
    expect(noGitResult.stderr).toContain('不是 git 仓库');
  });

  it('应用阶段失败（plan 与 config 失配）→ status=error、exit 1，且如实报告已落地的写入', async () => {
    const h = makeHarness({
      seed: { 'pi/settings/settings.json': '{"theme":"base"}\n' },
      remote: { 'pi/settings/settings.json': '{"theme":"remote"}\n' },
      local: { 'settings.json': '{"theme":"local"}\n' },
    });

    // remote 里多一个 config 未声明的 category（'ghost'）→ clean write 动作 → applyPullActions 抛 CliError。
    const sources = sourcesOf(
      { settings: '{"theme":"base"}\n' },
      { settings: '{"theme":"local"}\n' },
      { settings: '{"theme":"remote"}\n' },
    );
    sources.remote = [
      ...sources.remote,
      {
        adapterId: 'pi',
        categories: [
          { adapterId: 'pi', category: 'ghost', mode: 'mirror', files: new Map([['x.md', fileEntry('x\n')]]) },
        ],
      },
    ];

    const report = await runMerge({ homerHome: h.paths.home, acceptRemote: true }, { sources });

    expect(report.status).toBe('error');
    expect(report.ok).toBe(false);
    expect(report.errors.join(' ')).toContain('plan 与 config 失配');
    // 裁决已算完（报告里看得到），但零工具目录写入 / 零备份
    expect(report.resolutions).toHaveLength(1);
    expect(report.applied).toEqual({ written: [], deleted: [], conflicts: [] });
    expect(fs.existsSync(h.paths.backupsDir)).toBe(false);
    // ff 已发生 → 明确告警「base 已前进」
    expect(report.warnings.join(' ')).toContain('base 已前进');
    expect(loadState(h.paths).lastSyncCommit).toBe(h.remoteCommit);
  });

  it('应用阶段真错误（写入目标被目录占位）→ error、exit 1，ff 后 base 已前进的告警如实给出', async () => {
    const h = makeHarness({
      seed: { 'pi/skills/foo/SKILL.md': '# foo base\n' },
      remote: { 'pi/skills/foo/SKILL.md': '# foo remote\n' },
      // 工具目录里 foo/SKILL.md 是个**目录** → scan 判为「本地已删除」→ local-delete-vs-remote-modify 冲突；
      // accept-remote 的写目标被目录占位 → fs 写失败 → 应用阶段真错误。
      local: { 'skills/foo/SKILL.md/inner.txt': 'occupied\n' },
    });

    const cli = await runCli(['merge', '--home', h.paths.home, '--accept-remote']);

    expect(cli.code).toBe(1);
    expect(cli.stdout).toContain('error');
    expect(cli.stdout).toContain('base 已前进');
    // ff 已发生（store 对齐远端），state 已指向 upstream HEAD —— 不伪造成功，但也不丢「已看见远端」的事实
    expect(loadState(h.paths).lastSyncCommit).toBe(h.remoteCommit);
  });

  it('分叉（HEAD 非 upstream 祖先且已前进）→ exit 1，提示先 push', async () => {
    const h = makeHarness({
      seed: { 'pi/settings/settings.json': '{"theme":"base"}\n' },
      remote: { 'pi/settings/settings.json': '{"theme":"remote"}\n' },
      local: { 'settings.json': '{"theme":"local"}\n' },
    });

    // 本地再做一个未推送的 commit → HEAD 与 upstream 分叉
    gitOk(h.paths.home, ['fetch']);
    writeStoreFiles(h.paths.home, { 'pi/settings/settings.json': '{"theme":"local-commit"}\n' });
    gitOk(h.paths.home, ['add', '-A', '--', 'store/']);
    gitOk(h.paths.home, ['commit', '-m', 'local divergence', '--', 'store/']);

    const cli = await runCli(['merge', '--home', h.paths.home, '--accept-remote']);
    expect(cli.code).toBe(1);
    expect(cli.stderr).toContain('分叉');
  });
});
