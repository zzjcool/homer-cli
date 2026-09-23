/**
 * P2-W6 · `src/core/sync/base.ts` 验收（docs/m2-plan.md §2.5 / §3-P2-W6）。
 *
 * 覆盖（计划 §3-P2-W6 逐条）：
 *   - git 模式（临时仓库 + 手工 commit + state）：base / remote 快照正确，`mode='git'`、
 *     `baseCommit` / `remoteRef` 填充；
 *   - 无 state → base = store 工作区（`mode='store'`，§1 决策 D3 的 fallback 链）；
 *   - state 指向不可读 commit → 回落 store 工作区 + warning；
 *   - root 不可读 → `local := base`（M-A 守卫，防全量假删除）+ `errors` 记录；
 *   - `opts.fetch=false` 跳过网络（用「远端前进但未 fetch」区分有无 fetch）；
 *   - 无 upstream / 非 git 仓库 → remote 视作 = base + warning；
 *   - fetch 失败（远端 URL 不可达）→ warning + remote = base；
 *   - local 是**原始**快照（不做 excludeKeys 剥离）。
 *
 * 全程隔离：临时 `HOMER_HOME` + 临时 adapter root（假 HOME）+ 临时 bare origin。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { collectSyncSources } from '../../../src/core/sync/base.js';
import { getHomerPaths, type HomerPaths } from '../../../src/core/paths.js';
import { saveState } from '../../../src/core/state.js';
import { gitExec } from '../../../src/core/git/index.js';
import type { AdapterSnapshot, HomerConfig } from '../../../src/core/types.js';

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
  gitOk(repo, ['config', 'user.email', 'homer-w6@example.invalid']);
  gitOk(repo, ['config', 'user.name', 'Homer W6']);
}

function write(file: string, content: string): void {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, content, 'utf8');
}

/** store 里落一个文件（含 `.homer-complete`，使 readSnapshotFromStore 不报半残）。 */
function writeStore(paths: HomerPaths, rel: string, content: string): void {
  write(path.join(paths.storeDir, rel), content);
  write(path.join(paths.storeDir, 'pi', '.homer-complete'), '');
}

interface Harness {
  paths: HomerPaths;
  agentRoot: string;
  config: HomerConfig;
  bare?: string;
}

/** 假 HOME 下的 pi adapter root + 最小 config（settings 单文件 merge / skills 目录 mirror）。 */
function setup(opts: { git?: boolean; agent?: boolean; remote?: boolean } = {}): Harness {
  const tmp = mkTmp('sync-base');
  const homerHome = path.join(tmp, 'homer');
  const agentRoot = path.join(tmp, 'home', '.pi', 'agent');
  fs.mkdirSync(homerHome, { recursive: true });
  if (opts.agent !== false) fs.mkdirSync(agentRoot, { recursive: true });

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

  const paths = getHomerPaths({ HOMER_HOME: homerHome });

  const harness: Harness = { paths, agentRoot, config };

  if (opts.git === true) {
    gitOk(paths.home, ['init', '-b', 'main']);
    configureUser(paths.home);
  }

  if (opts.remote === true) {
    const bare = path.join(mkTmp('sync-base-origin'), 'origin.git');
    gitOk(path.dirname(bare), ['init', '--bare', '-b', 'main', bare]);
    gitOk(paths.home, ['remote', 'add', 'origin', bare]);
    harness.bare = bare;
  }

  return harness;
}

/** 在 `home` 里用给定的 store 内容做一次 commit。 */
function commitStore(paths: HomerPaths, message: string): string {
  gitOk(paths.home, ['add', '-A', '--', 'store/']);
  gitOk(paths.home, ['commit', '-m', message, '--', 'store/']);
  return gitOk(paths.home, ['rev-parse', 'HEAD']).trim();
}

/** 快照 → 便于断言的 `category/relPath → content` 映射。 */
function flatten(snapshots: readonly AdapterSnapshot[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const snapshot of snapshots) {
    for (const category of snapshot.categories) {
      for (const [relPath, entry] of category.files) {
        out[`${snapshot.adapterId}/${category.category}/${relPath}`] = entry.content;
      }
    }
  }
  return out;
}

/** 从快照取某文件的 kind。 */
function kindOf(snapshots: readonly AdapterSnapshot[], category: string, relPath: string): string | undefined {
  return snapshots[0]?.categories.find((c) => c.category === category)?.files.get(relPath)?.kind;
}

/* ------------------------------------------------------------------ */
/* git 模式                                                            */
/* ------------------------------------------------------------------ */

describe('collectSyncSources — git 模式（临时仓库 + 手工 commit + state）', () => {
  it('base = state.lastSyncCommit 的 store，remote = upstream 的 store，local = 实时扫描', () => {
    const h = setup({ git: true, remote: true });
    const bare = h.bare as string;

    // ---- commit1（= 将成为 base）：store 里是 v1 ----
    writeStore(h.paths, 'pi/settings/settings.json', '{"theme":"light","apiKey":"BASE-SECRET"}\n');
    writeStore(h.paths, 'pi/skills/alpha/SKILL.md', '# alpha v1\n');
    gitOk(h.paths.home, ['add', '-A']);
    gitOk(h.paths.home, ['commit', '-m', 'seed v1']);
    gitOk(h.paths.home, ['push', '-u', 'origin', 'main']);
    const baseCommit = gitOk(h.paths.home, ['rev-parse', 'HEAD']).trim();

    // state 指向 base
    saveState(h.paths, { version: 1, lastSyncCommit: baseCommit, lastSyncCommand: 'push' });

    // ---- commit2（= 将成为 remote）：store 里是 v2 ----
    writeStore(h.paths, 'pi/settings/settings.json', '{"theme":"dark","apiKey":"REMOTE-SECRET"}\n');
    writeStore(h.paths, 'pi/skills/beta/SKILL.md', '# beta\n');
    gitOk(h.paths.home, ['add', '-A']);
    const remoteCommit = commitStore(h.paths, 'remote v2');
    gitOk(h.paths.home, ['push']);
    expect(remoteCommit).not.toBe(baseCommit);

    // ---- local：工具目录里是另一套内容 ----
    write(path.join(h.agentRoot, 'settings.json'), '{"theme":"local","apiKey":"LOCAL-SECRET"}\n');
    write(path.join(h.agentRoot, 'skills', 'alpha', 'SKILL.md'), '# alpha local\n');

    const sources = collectSyncSources(h.paths, h.config, { fetch: false });

    expect(sources.mode).toBe('git');
    expect(sources.baseCommit).toBe(baseCommit);
    expect(sources.remoteRef).toBe('origin/main');
    expect(sources.errors).toEqual([]);
    expect(sources.warnings).toEqual([]);

    // base = commit1 的 store（v1）
    expect(flatten(sources.base)).toEqual({
      'pi/settings/settings.json': '{"theme":"light","apiKey":"BASE-SECRET"}\n',
      'pi/skills/alpha/SKILL.md': '# alpha v1\n',
    });
    // remote = commit2 的 store（v2）；注意 base 里没有的 beta
    expect(flatten(sources.remote)).toEqual({
      'pi/settings/settings.json': '{"theme":"dark","apiKey":"REMOTE-SECRET"}\n',
      'pi/skills/alpha/SKILL.md': '# alpha v1\n',
      'pi/skills/beta/SKILL.md': '# beta\n',
    });
    // local = 实时扫描（原始，未剥离 apiKey）
    expect(flatten(sources.local)).toEqual({
      'pi/settings/settings.json': '{"theme":"local","apiKey":"LOCAL-SECRET"}\n',
      'pi/skills/alpha/SKILL.md': '# alpha local\n',
    });

    expect(bare).toContain('origin.git');
  });

  it('base 与 remote 的 kind 判定与 store 侧同构（merge 单文件 = json，mirror = file）', () => {
    const h = setup({ git: true, remote: true });
    writeStore(h.paths, 'pi/settings/settings.json', '{"a":1}\n');
    writeStore(h.paths, 'pi/skills/a/SKILL.md', 'not json\n');
    gitOk(h.paths.home, ['add', '-A']);
    gitOk(h.paths.home, ['commit', '-m', 'seed']);
    gitOk(h.paths.home, ['push', '-u', 'origin', 'main']);
    saveState(h.paths, { version: 1, lastSyncCommit: gitOk(h.paths.home, ['rev-parse', 'HEAD']).trim() });

    const sources = collectSyncSources(h.paths, h.config, { fetch: false });

    expect(kindOf(sources.base, 'settings', 'settings.json')).toBe('json');
    expect(kindOf(sources.base, 'skills', 'a/SKILL.md')).toBe('file');
  });

  it('local 是**原始**快照：excludeKeys（apiKey）不被剥离（剥离是 plan 层的事）', () => {
    const h = setup({ git: true, remote: true });
    writeStore(h.paths, 'pi/settings/settings.json', '{"apiKey":"X"}\n');
    gitOk(h.paths.home, ['add', '-A']);
    gitOk(h.paths.home, ['commit', '-m', 'seed']);
    gitOk(h.paths.home, ['push', '-u', 'origin', 'main']);
    saveState(h.paths, { version: 1, lastSyncCommit: gitOk(h.paths.home, ['rev-parse', 'HEAD']).trim() });

    write(path.join(h.agentRoot, 'settings.json'), '{"theme":"t","apiKey":"LOCAL-KEY"}\n');

    const sources = collectSyncSources(h.paths, h.config, { fetch: false });
    expect(flatten(sources.local)['pi/settings/settings.json']).toContain('LOCAL-KEY');
    // base / remote 也是原始字节（store 里就是密钥明文，剥离留给 planPull 的判定副本）
    expect(flatten(sources.base)['pi/settings/settings.json']).toContain('"apiKey":"X"');
  });

  it('state 存在但 store 为空（无文件）→ 仍走 git 模式，base 为空 Map 分类', () => {
    const h = setup({ git: true, remote: true });
    write(path.join(h.paths.home, 'README.md'), 'x\n');
    gitOk(h.paths.home, ['add', '-A']);
    gitOk(h.paths.home, ['commit', '-m', 'empty store']);
    gitOk(h.paths.home, ['push', '-u', 'origin', 'main']);
    const commit = gitOk(h.paths.home, ['rev-parse', 'HEAD']).trim();
    saveState(h.paths, { version: 1, lastSyncCommit: commit });

    const sources = collectSyncSources(h.paths, h.config, { fetch: false });
    expect(sources.mode).toBe('git');
    expect(sources.baseCommit).toBe(commit);
    expect(flatten(sources.base)).toEqual({});
    expect(sources.warnings).toEqual([]);
  });
});

/* ------------------------------------------------------------------ */
/* 无 state → base = store 工作区                                       */
/* ------------------------------------------------------------------ */

describe('collectSyncSources — 无 state 时回落 store 工作区', () => {
  it('无 state.json（git 仓库里）→ mode=store，base = 工作区 store，无 baseCommit', () => {
    const h = setup({ git: true, remote: true });
    writeStore(h.paths, 'pi/settings/settings.json', '{"theme":"ws"}\n');
    writeStore(h.paths, 'pi/skills/a/SKILL.md', '# ws a\n');
    // 有 commit 但**不分配** state（未 push 过的场景）
    gitOk(h.paths.home, ['add', '-A']);
    gitOk(h.paths.home, ['commit', '-m', 'ws']);
    gitOk(h.paths.home, ['push', '-u', 'origin', 'main']);

    write(path.join(h.agentRoot, 'settings.json'), '{"theme":"local"}\n');

    const sources = collectSyncSources(h.paths, h.config, { fetch: false });

    expect(sources.mode).toBe('store');
    expect(sources.baseCommit).toBeUndefined();
    expect(flatten(sources.base)).toEqual({
      'pi/settings/settings.json': '{"theme":"ws"}\n',
      'pi/skills/a/SKILL.md': '# ws a\n',
    });
    expect(flatten(sources.local)).toEqual({ 'pi/settings/settings.json': '{"theme":"local"}\n' });
    // remote 有 upstream → 读的是 upstream（内容与工作区 commit 相同）
    expect(sources.remoteRef).toBe('origin/main');
    expect(flatten(sources.remote)['pi/skills/a/SKILL.md']).toBe('# ws a\n');
  });

  it('state 指向不可读的 commit → 回落 store 工作区 + warning（不静默当空 base）', () => {
    const h = setup({ git: true, remote: true });
    writeStore(h.paths, 'pi/skills/a/SKILL.md', '# ws\n');
    gitOk(h.paths.home, ['add', '-A']);
    gitOk(h.paths.home, ['commit', '-m', 'ws']);
    gitOk(h.paths.home, ['push', '-u', 'origin', 'main']);

    saveState(h.paths, { version: 1, lastSyncCommit: '0'.repeat(40) });

    const sources = collectSyncSources(h.paths, h.config, { fetch: false });

    expect(sources.mode).toBe('store');
    expect(sources.baseCommit).toBeUndefined();
    expect(flatten(sources.base)['pi/skills/a/SKILL.md']).toBe('# ws\n');
    expect(sources.warnings.some((w) => /lastSyncCommit.*不可读/.test(w))).toBe(true);
  });

  it('非 git 仓库 + 无 state → 仍能读 store 工作区（mode=store）', () => {
    const h = setup();
    writeStore(h.paths, 'pi/settings/settings.json', '{"theme":"plain"}\n');

    const sources = collectSyncSources(h.paths, h.config, { fetch: false });

    expect(sources.mode).toBe('store');
    expect(flatten(sources.base)).toEqual({ 'pi/settings/settings.json': '{"theme":"plain"}\n' });
    expect(sources.warnings.some((w) => /不是 git 仓库/.test(w))).toBe(true);
  });
});

/* ------------------------------------------------------------------ */
/* root 不可读 → local := base（M-A 守卫）                              */
/* ------------------------------------------------------------------ */

describe('collectSyncSources — root 不可读时 local := base（M-A 守卫）', () => {
  it('agent root 不存在 → local 与 base 完全相同（不产生假删除）+ errors 含 root 不可读', () => {
    const h = setup({ git: true, remote: true, agent: false }); // 不创建 agentRoot
    writeStore(h.paths, 'pi/settings/settings.json', '{"theme":"x"}\n');
    writeStore(h.paths, 'pi/skills/a/SKILL.md', '# a\n');
    gitOk(h.paths.home, ['add', '-A']);
    gitOk(h.paths.home, ['commit', '-m', 'seed']);
    gitOk(h.paths.home, ['push', '-u', 'origin', 'main']);
    saveState(h.paths, { version: 1, lastSyncCommit: gitOk(h.paths.home, ['rev-parse', 'HEAD']).trim() });

    const sources = collectSyncSources(h.paths, h.config, { fetch: false });

    // local := base（同一份内容），因此对比 base 不会有任何 pull-delete
    expect(flatten(sources.local)).toEqual(flatten(sources.base));
    expect(sources.errors.some((e) => /adapter root 不可读: pi/.test(e))).toBe(true);
    // 格式与 `homer status` 的 sourceErrorMessages 一致（含 path: message）
    expect(sources.errors.some((e) => /adapter root 不可读: pi \(.*: .*\)/.test(e))).toBe(true);
  });

  it('root 不可读 + 无 state（base 也是 store 工作区）→ local := 工作区 store', () => {
    const h = setup({ git: false, agent: false });
    writeStore(h.paths, 'pi/skills/a/SKILL.md', '# a\n');

    const sources = collectSyncSources(h.paths, h.config, { fetch: false });

    expect(flatten(sources.local)).toEqual(flatten(sources.base));
    expect(sources.errors.some((e) => /adapter root 不可读/.test(e))).toBe(true);
  });

  it('root 存在但为空目录 → local 是真实的空快照（不算 root 不可读）', () => {
    const h = setup({ git: true, remote: true });
    writeStore(h.paths, 'pi/skills/a/SKILL.md', '# a\n');
    gitOk(h.paths.home, ['add', '-A']);
    gitOk(h.paths.home, ['commit', '-m', 'seed']);
    gitOk(h.paths.home, ['push', '-u', 'origin', 'main']);
    saveState(h.paths, { version: 1, lastSyncCommit: gitOk(h.paths.home, ['rev-parse', 'HEAD']).trim() });

    const sources = collectSyncSources(h.paths, h.config, { fetch: false });

    // 空目录是合法状态：local 分类齐全但 files 为空 → 会产生 pull-delete（正确的同步意图）
    expect(sources.errors).toEqual([]);
    expect(flatten(sources.local)).toEqual({});
    expect(sources.local[0]?.categories.map((c) => c.category)).toEqual(['settings', 'skills']);
  });
});

/* ------------------------------------------------------------------ */
/* fetch 行为                                                          */
/* ------------------------------------------------------------------ */

describe('collectSyncSources — opts.fetch 与降级', () => {
  it('fetch=false 时不联网：远端前进后 remote 仍是旧的（@{upstream} 引用未更新）', () => {
    const h = setup({ git: true, remote: true });
    const bare = h.bare as string;

    writeStore(h.paths, 'pi/skills/a/SKILL.md', '# v1\n');
    gitOk(h.paths.home, ['add', '-A']);
    gitOk(h.paths.home, ['commit', '-m', 'v1']);
    gitOk(h.paths.home, ['push', '-u', 'origin', 'main']);

    // 另一个 clone 推一个新 commit 到 bare（本地未 fetch）
    const other = path.join(mkTmp('sync-base-other'), 'work');
    gitOk(path.dirname(other), ['clone', bare, other]);
    configureUser(other);
    write(path.join(other, 'store', 'pi', 'skills', 'remote-only.md'), '# remote only\n');
    gitOk(other, ['add', '-A']);
    gitOk(other, ['commit', '-m', 'remote v2']);
    gitOk(other, ['push']);

    // fetch=false：@{upstream} 仍指向旧 commit → remote 看不到 remote-only.md
    const noFetch = collectSyncSources(h.paths, h.config, { fetch: false });
    expect(flatten(noFetch.remote)['pi/skills/remote-only.md']).toBeUndefined();
    expect(noFetch.remoteRef).toBe('origin/main');

    // fetch=true（默认）：拉到新 commit → remote 含 remote-only.md
    const withFetch = collectSyncSources(h.paths, h.config);
    expect(flatten(withFetch.remote)['pi/skills/remote-only.md']).toBe('# remote only\n');
  });

  it('默认（不传 opts）会 fetch', () => {
    const h = setup({ git: true, remote: true });
    writeStore(h.paths, 'pi/skills/a/SKILL.md', '# a\n');
    gitOk(h.paths.home, ['add', '-A']);
    gitOk(h.paths.home, ['commit', '-m', 'seed']);
    gitOk(h.paths.home, ['push', '-u', 'origin', 'main']);

    const sources = collectSyncSources(h.paths, h.config);
    expect(sources.errors).toEqual([]);
    expect(sources.remoteRef).toBe('origin/main');
    expect(flatten(sources.remote)['pi/skills/a/SKILL.md']).toBe('# a\n');
  });

  it('fetch 失败（远端 URL 不可达）→ warning + remote := base', () => {
    const h = setup({ git: true, remote: true });
    writeStore(h.paths, 'pi/skills/a/SKILL.md', '# base\n');
    gitOk(h.paths.home, ['add', '-A']);
    gitOk(h.paths.home, ['commit', '-m', 'seed']);
    gitOk(h.paths.home, ['push', '-u', 'origin', 'main']);
    saveState(h.paths, { version: 1, lastSyncCommit: gitOk(h.paths.home, ['rev-parse', 'HEAD']).trim() });

    // 把 origin 指向不存在的路径 → fetch 必失败
    gitOk(h.paths.home, ['remote', 'set-url', 'origin', path.join(mkTmp('sync-base-nowhere'), 'nope.git')]);

    const sources = collectSyncSources(h.paths, h.config); // fetch=true

    expect(sources.warnings.some((w) => /git fetch 失败/.test(w))).toBe(true);
    expect(flatten(sources.remote)).toEqual(flatten(sources.base));
    expect(sources.remoteRef).toBe('origin/main');
  });

  it('无 upstream（git 仓库）→ warning + remote := base，remoteRef 不填', () => {
    const h = setup({ git: true });
    writeStore(h.paths, 'pi/skills/a/SKILL.md', '# a\n');
    gitOk(h.paths.home, ['add', '-A']);
    gitOk(h.paths.home, ['commit', '-m', 'seed']);

    const sources = collectSyncSources(h.paths, h.config, { fetch: false });

    expect(sources.remoteRef).toBeUndefined();
    expect(flatten(sources.remote)).toEqual(flatten(sources.base));
    expect(sources.warnings.some((w) => /未配置 git upstream/.test(w))).toBe(true);
  });

  it('配了 upstream 但远端跟踪引用不存在 → 视作「未配置 upstream」→ warning + remote := base', () => {
    const h = setup({ git: true, remote: true });
    writeStore(h.paths, 'pi/skills/a/SKILL.md', '# a\n');
    gitOk(h.paths.home, ['add', '-A']);
    gitOk(h.paths.home, ['commit', '-m', 'seed']);
    // 手工配 upstream，但 bare 里没有 main（从未 push）→ 远端跟踪引用不存在。
    // git 的 `@{upstream}` 解析依赖远端跟踪引用，故此时 upstreamRef 返回 undefined ——
    // 本模块按「无 upstream」降级（而不是报错）。
    gitOk(h.paths.home, ['config', 'branch.main.remote', 'origin']);
    gitOk(h.paths.home, ['config', 'branch.main.merge', 'refs/heads/main']);

    const sources = collectSyncSources(h.paths, h.config, { fetch: false });

    expect(sources.remoteRef).toBeUndefined();
    expect(flatten(sources.remote)).toEqual(flatten(sources.base));
    expect(sources.warnings.some((w) => /未配置 git upstream/.test(w))).toBe(true);
  });

  it('upstream 引用存在但不是 commit（指向 blob）→ warning + remote := base（防御分支）', () => {
    const h = setup({ git: true, remote: true });
    writeStore(h.paths, 'pi/skills/a/SKILL.md', '# a\n');
    gitOk(h.paths.home, ['add', '-A']);
    gitOk(h.paths.home, ['commit', '-m', 'seed']);
    gitOk(h.paths.home, ['push', '-u', 'origin', 'main']);

    // 把远端跟踪引用改指向一个 blob：@{upstream} 能解析出名字，但 `^{commit}` peel 失败。
    const blob = gitOk(h.paths.home, ['hash-object', '-w', '--stdin'], ).trim();
    expect(blob).not.toBe('');
    gitOk(h.paths.home, ['update-ref', 'refs/remotes/origin/main', blob]);

    const sources = collectSyncSources(h.paths, h.config, { fetch: false });

    expect(sources.remoteRef).toBe('origin/main');
    expect(flatten(sources.remote)).toEqual(flatten(sources.base));
    expect(sources.warnings.some((w) => /不可解析/.test(w))).toBe(true);
  });
});

/* ------------------------------------------------------------------ */
/* 结构不变式                                                          */
/* ------------------------------------------------------------------ */

describe('collectSyncSources — 结构不变式', () => {
  it('三方快照的分类结构都对齐 config（enabled adapter / category 齐全）', () => {
    const h = setup({ git: true, remote: true });
    writeStore(h.paths, 'pi/settings/settings.json', '{}');
    gitOk(h.paths.home, ['add', '-A']);
    gitOk(h.paths.home, ['commit', '-m', 'seed']);
    gitOk(h.paths.home, ['push', '-u', 'origin', 'main']);

    const sources = collectSyncSources(h.paths, h.config, { fetch: false });
    for (const side of [sources.base, sources.local, sources.remote]) {
      expect(side).toHaveLength(1);
      expect(side[0]?.adapterId).toBe('pi');
      expect(side[0]?.categories.map((c) => c.category)).toEqual(['settings', 'skills']);
    }
  });

  it('禁用的 adapter / category 不出现在任何一侧', () => {
    const h = setup({ git: true, remote: true });
    h.config = {
      version: 1,
      adapters: {
        pi: { ...(h.config.adapters['pi'] as NonNullable<HomerConfig['adapters']['pi']>) },
        off: { root: '/tmp/nope', enabled: false, categories: { data: { paths: ['data/'], mode: 'mirror' } } },
      },
    };
    (h.config.adapters['pi'] as { categories: Record<string, unknown> }).categories = {
      settings: { paths: ['settings.json'], mode: 'merge' },
      disabled: { paths: ['disabled/'], mode: 'mirror', enabled: false },
    };
    writeStore(h.paths, 'pi/settings/settings.json', '{}');
    gitOk(h.paths.home, ['add', '-A']);
    gitOk(h.paths.home, ['commit', '-m', 'seed']);
    gitOk(h.paths.home, ['push', '-u', 'origin', 'main']);

    const sources = collectSyncSources(h.paths, h.config, { fetch: false });
    for (const side of [sources.base, sources.local, sources.remote]) {
      expect(side.map((s) => s.adapterId)).toEqual(['pi']);
      expect(side[0]?.categories.map((c) => c.category)).toEqual(['settings']);
    }
    // 被禁用的 adapter 不产生 root 不可读告警
    expect(sources.errors).toEqual([]);
  });

  it('warnings / errors 是可变数组且不共享引用（各次调用独立）', () => {
    const h = setup();
    const first = collectSyncSources(h.paths, h.config, { fetch: false });
    const second = collectSyncSources(h.paths, h.config, { fetch: false });
    expect(first.warnings).not.toBe(second.warnings);
    expect(first.errors).not.toBe(second.errors);
    first.warnings.push('pollute');
    expect(second.warnings).not.toContain('pollute');
  });
});
