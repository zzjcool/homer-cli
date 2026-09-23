/**
 * P2-W6 · `src/core/sync/pipeline.ts` 验收（docs/m2-plan.md §2.5 / §3-P2-W6）。
 *
 * 三组：
 *   1. `prepareStoreSnapshot` —— excludeKeys → `__REQUIRED__` 占位符（写 store 前唯一入口）；
 *   2. `commitStoreIfNeeded` —— 无变更 → undefined、有变更 → SHA 且 `git log` 可见；
 *   3. `requireCleanStore` / `requireFastForwardable` —— 脏 / 非仓库 / 无 upstream /
 *      不可解析 / 无 commit / 分叉 → `CliError`；干净 + 可 ff → 静默通过。
 *
 * git 相关用例全部用 `mkdtemp` 临时仓库（`git init --bare` 假 origin + clone），
 * 仓库级 user.email / user.name 一律显式设置（CI 无全局身份也不影响断言）。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import {
  commitStoreIfNeeded,
  prepareStoreSnapshot,
  requireCleanStore,
  requireFastForwardable,
} from '../../../src/core/sync/pipeline.js';
import { CliError } from '../../../src/core/errors.js';
import { getHomerPaths, type HomerPaths } from '../../../src/core/paths.js';
import { readSnapshotFromStore } from '../../../src/core/store/store.js';
import { gitExec } from '../../../src/core/git/index.js';
import type { AdapterSnapshot, HomerConfig, SnapshotEntry } from '../../../src/core/types.js';

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
  if (!result.ok) throw new Error(`git ${args.join(' ')} 失败: ${result.stderr}`);
  return result.stdout;
}

function configureUser(repo: string): void {
  gitOk(repo, ['config', 'user.email', 'homer-w6@example.invalid']);
  gitOk(repo, ['config', 'user.name', 'Homer W6']);
}

/** 临时 `~/.homer` 工作区（可选地 `git init -b main`）。 */
function makeHome(opts: { git?: boolean } = {}): HomerPaths {
  const home = path.join(mkTmp('w6-home'), 'homer');
  fs.mkdirSync(home, { recursive: true });
  if (opts.git === true) {
    gitOk(home, ['init', '-b', 'main']);
    configureUser(home);
  }
  return getHomerPaths({ HOMER_HOME: home });
}

/** 让 `home` 连上假 origin 并建立 upstream（返回 bare 仓库路径）。 */
function attachOrigin(home: string): string {
  const bare = path.join(mkTmp('w6-origin'), 'origin.git');
  gitOk(path.dirname(bare), ['init', '--bare', '-b', 'main', bare]);
  gitOk(home, ['remote', 'add', 'origin', bare]);
  return bare;
}

/** 写一个文件（自动建父目录）。 */
function write(file: string, content: string): void {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, content, 'utf8');
}

/** 往 store 里落一个文件（绕过 writeSnapshotToStore，直接构造 git 可见的变更）。 */
function writeStoreFile(paths: HomerPaths, rel: string, content: string): void {
  write(path.join(paths.storeDir, rel), content);
}

/** 首次 commit + 推 upstream（建立可 ff 的基线）。 */
function seedUpstream(paths: HomerPaths, bare: string): string {
  writeStoreFile(paths, 'pi/skills/a/SKILL.md', '# a\n');
  gitOk(paths.home, ['add', '-A']);
  gitOk(paths.home, ['commit', '-m', 'seed']);
  gitOk(paths.home, ['push', '-u', 'origin', 'main']);
  return gitOk(paths.home, ['rev-parse', 'HEAD']).trim();
}

/* ------------------------------------------------------------------ */
/* prepareStoreSnapshot                                                */
/* ------------------------------------------------------------------ */

function jsonEntry(value: unknown): SnapshotEntry {
  return { kind: 'json', content: `${JSON.stringify(value, null, 2)}\n` };
}

function snapshotOf(files: Record<string, SnapshotEntry>): AdapterSnapshot {
  return {
    adapterId: 'pi',
    categories: [{ adapterId: 'pi', category: 'models', mode: 'merge', files: new Map(Object.entries(files)) }],
  };
}

const SECRET_CONFIG: HomerConfig = {
  version: 1,
  adapters: {
    pi: {
      root: '/tmp/pi-root',
      enabled: true,
      categories: { models: { paths: ['models.json'], mode: 'merge', excludeKeys: ['apiKeys'] } },
    },
  },
};

describe('prepareStoreSnapshot — excludeKeys → __REQUIRED__ 占位符', () => {
  it('excluded 顶层键的值被替换为 "__REQUIRED__"（密钥不落 store）', () => {
    const [snapshot] = prepareStoreSnapshot(
      [snapshotOf({ 'models.json': jsonEntry({ apiKeys: { openai: 'sk-real-secret' }, models: { a: 1 } }) })],
      SECRET_CONFIG,
    );

    const content = snapshot?.categories[0]?.files.get('models.json')?.content ?? '';
    expect(content).toContain('"apiKeys": "__REQUIRED__"');
    expect(content).not.toContain('sk-real-secret');
    // 非 excluded 键原样保留
    expect(content).toContain('"models"');
  });

  it('不含 excluded 键的文件字节不变（不引入无意义 store diff）', () => {
    const original = jsonEntry({ models: { a: 1 } });
    const [snapshot] = prepareStoreSnapshot([snapshotOf({ 'models.json': original })], SECRET_CONFIG);
    expect(snapshot?.categories[0]?.files.get('models.json')).toEqual(original);
  });

  it('不凭空造出用户从未拥有的 excluded 键', () => {
    const [snapshot] = prepareStoreSnapshot([snapshotOf({ 'models.json': jsonEntry({ models: 1 }) })], SECRET_CONFIG);
    expect(snapshot?.categories[0]?.files.get('models.json')?.content).not.toContain('__REQUIRED__');
  });

  it('逐 adapter 处理，未在 config 声明的 adapter 原样返回', () => {
    const other: AdapterSnapshot = {
      adapterId: 'other',
      categories: [
        {
          adapterId: 'other',
          category: 'models',
          mode: 'merge',
          files: new Map([['models.json', jsonEntry({ apiKeys: 'leak' })]]),
        },
      ],
    };
    const result = prepareStoreSnapshot(
      [snapshotOf({ 'models.json': jsonEntry({ apiKeys: 'leak' }) }), other],
      SECRET_CONFIG,
    );
    expect(result).toHaveLength(2);
    expect(result[1]).toEqual(other);
  });

  it('多个 adapter / 多个分类都被处理，且不修改入参（纯函数语义）', () => {
    const input = snapshotOf({ 'models.json': jsonEntry({ apiKeys: 'x' }) });
    const before = JSON.stringify(input);
    const result = prepareStoreSnapshot([input], SECRET_CONFIG);

    // 不修改入参对象本身（结构引用也不同：占位符替换产出新 Map / 新 Entry）
    expect(JSON.stringify(input)).toBe(before);
    expect(input.categories[0]?.files.get('models.json')?.content).toContain('"x"');
    expect((input.categories[0]?.files ?? new Map()).get('models.json')).not.toBe(
      result[0]?.categories[0]?.files.get('models.json'),
    );

    // 结果里密钥值已被占位符取代
    expect(result[0]?.categories[0]?.files.get('models.json')?.content).not.toContain('"x"');
    expect(result[0]?.categories[0]?.files.get('models.json')?.content).toContain('__REQUIRED__');
  });

  it('空输入 → 空输出', () => {
    expect(prepareStoreSnapshot([], SECRET_CONFIG)).toEqual([]);
  });
});

/* ------------------------------------------------------------------ */
/* commitStoreIfNeeded                                                 */
/* ------------------------------------------------------------------ */

describe('commitStoreIfNeeded — store 提交', () => {
  it('store 无变更 → undefined（幂等，不产生空 commit）', () => {
    const paths = makeHome({ git: true });
    const bare = attachOrigin(paths.home);
    seedUpstream(paths, bare);

    expect(commitStoreIfNeeded(paths, 'homer sync: 无变更')).toBeUndefined();
    // HEAD 未动
    expect(gitOk(paths.home, ['rev-list', '--count', 'HEAD']).trim()).toBe('1');
  });

  it('store 有变更 → 返回新 SHA 且 commit message 在 git log 中可见', () => {
    const paths = makeHome({ git: true });
    const bare = attachOrigin(paths.home);
    const before = seedUpstream(paths, bare);

    writeStoreFile(paths, 'pi/settings/settings.json', '{"theme":"dark"}\n');
    const sha = commitStoreIfNeeded(paths, 'homer push: 同步 pi 快照');

    expect(sha).toBeDefined();
    expect(sha).not.toBe(before);
    expect(sha).toBe(gitOk(paths.home, ['rev-parse', 'HEAD']).trim());

    const log = gitOk(paths.home, ['log', '--format=%s%n%H']);
    expect(log).toContain('homer push: 同步 pi 快照');
    expect(log).toContain(sha as string);
    // 提交内容确实包含新文件
    expect(gitOk(paths.home, ['show', '--name-only', '--format=', sha as string])).toContain(
      'store/pi/settings/settings.json',
    );
  });

  it('非 git 仓库 → undefined（不 throw；命令层自行决定是否先 ensureGitRepo）', () => {
    const paths = makeHome();
    writeStoreFile(paths, 'pi/skills/a/SKILL.md', '# a\n');
    expect(commitStoreIfNeeded(paths, 'msg')).toBeUndefined();
  });

  it('只提交 store/，homer.json 等其它路径的改动不入本次 commit', () => {
    const paths = makeHome({ git: true });
    const bare = attachOrigin(paths.home);
    seedUpstream(paths, bare);

    write(paths.configFile, '{"version":1,"adapters":{}}\n');
    writeStoreFile(paths, 'pi/skills/b/SKILL.md', '# b\n');
    const sha = commitStoreIfNeeded(paths, 'homer push: 只提交 store');

    const files = gitOk(paths.home, ['show', '--name-only', '--format=', sha as string]);
    expect(files).toContain('store/pi/skills/b/SKILL.md');
    expect(files).not.toContain('homer.json');
    // homer.json 仍是未跟踪状态
    expect(gitOk(paths.home, ['status', '--porcelain'])).toContain('?? homer.json');
  });
});

/* ------------------------------------------------------------------ */
/* requireCleanStore                                                   */
/* ------------------------------------------------------------------ */

describe('requireCleanStore — store 工作区必须干净', () => {
  it('干净 store → 静默通过（不抛）', () => {
    const paths = makeHome({ git: true });
    const bare = attachOrigin(paths.home);
    seedUpstream(paths, bare);

    expect(() => requireCleanStore(paths)).not.toThrow();
  });

  it('store 脏（未提交改动）→ CliError 且提示先 push', () => {
    const paths = makeHome({ git: true });
    const bare = attachOrigin(paths.home);
    seedUpstream(paths, bare);

    writeStoreFile(paths, 'pi/skills/a/SKILL.md', '# a modified\n');

    let error: unknown;
    try {
      requireCleanStore(paths);
    } catch (err) {
      error = err;
    }
    expect(error).toBeInstanceOf(CliError);
    expect((error as CliError).message).toMatch(/store 工作区有未提交的改动/);
    expect((error as CliError).hint).toMatch(/homer push/);
  });

  it('store 脏（已暂存未提交）同样被拦', () => {
    const paths = makeHome({ git: true });
    const bare = attachOrigin(paths.home);
    seedUpstream(paths, bare);

    writeStoreFile(paths, 'pi/skills/c/SKILL.md', '# c\n');
    gitOk(paths.home, ['add', '-A', '--', 'store/']);

    expect(() => requireCleanStore(paths)).toThrow(/未提交的改动/);
  });

  it('非 git 仓库 → CliError（不伪装成「store 脏」）', () => {
    const paths = makeHome();
    let error: unknown;
    try {
      requireCleanStore(paths);
    } catch (err) {
      error = err;
    }
    expect(error).toBeInstanceOf(CliError);
    expect((error as CliError).message).toMatch(/不是 git 仓库/);
  });

  it('非 store 路径的脏改动（未跟踪的 state.json）不影响判定', () => {
    const paths = makeHome({ git: true });
    const bare = attachOrigin(paths.home);
    seedUpstream(paths, bare);

    write(paths.stateFile, '{"version":1}\n');
    write(path.join(paths.home, 'notes.txt'), 'random\n');
    expect(() => requireCleanStore(paths)).not.toThrow();
  });
});

/* ------------------------------------------------------------------ */
/* requireFastForwardable                                              */
/* ------------------------------------------------------------------ */

describe('requireFastForwardable — HEAD 必须是 upstream 的祖先', () => {
  it('HEAD == upstream → 已同步，静默通过', () => {
    const paths = makeHome({ git: true });
    const bare = attachOrigin(paths.home);
    seedUpstream(paths, bare);
    expect(() => requireFastForwardable(paths)).not.toThrow();
  });

  it('HEAD 是 upstream 的祖先（远端前进）→ 可 ff，静默通过', () => {
    const paths = makeHome({ git: true });
    const bare = attachOrigin(paths.home);
    seedUpstream(paths, bare);

    // 模拟「远端前进」：另起一个 clone 推一个新 commit 到 bare，本地 fetch 但 reset 回旧 HEAD
    const other = mkTmp('w6-other');
    gitOk(path.dirname(other), ['clone', bare, other]);
    configureUser(other);
    write(path.join(other, 'store', 'pi', 'skills', 'remote.md'), '# remote\n');
    gitOk(other, ['add', '-A']);
    gitOk(other, ['commit', '-m', 'remote ahead']);
    gitOk(other, ['push']);

    gitOk(paths.home, ['fetch']);

    expect(() => requireFastForwardable(paths)).not.toThrow();
  });

  it('分叉（本地有未推送 commit + 远端前进）→ CliError 提示先 push', () => {
    const paths = makeHome({ git: true });
    const bare = attachOrigin(paths.home);
    seedUpstream(paths, bare);

    const other = mkTmp('w6-other');
    gitOk(path.dirname(other), ['clone', bare, other]);
    configureUser(other);
    write(path.join(other, 'store', 'pi', 'skills', 'remote.md'), '# remote\n');
    gitOk(other, ['add', '-A']);
    gitOk(other, ['commit', '-m', 'remote ahead']);
    gitOk(other, ['push']);

    // 本地也提交（未推送）→ 真分叉
    writeStoreFile(paths, 'pi/skills/local.md', '# local\n');
    gitOk(paths.home, ['add', '-A', '--', 'store/']);
    gitOk(paths.home, ['commit', '-m', 'local ahead']);

    gitOk(paths.home, ['fetch']);

    let error: unknown;
    try {
      requireFastForwardable(paths);
    } catch (err) {
      error = err;
    }
    expect(error).toBeInstanceOf(CliError);
    expect((error as CliError).message).toMatch(/分叉/);
    expect((error as CliError).hint).toMatch(/homer push/);
  });

  it('无 upstream → CliError 提示配置远端', () => {
    const paths = makeHome({ git: true });
    writeStoreFile(paths, 'pi/skills/a/SKILL.md', '# a\n');
    gitOk(paths.home, ['add', '-A']);
    gitOk(paths.home, ['commit', '-m', 'seed']);

    let error: unknown;
    try {
      requireFastForwardable(paths);
    } catch (err) {
      error = err;
    }
    expect(error).toBeInstanceOf(CliError);
    expect((error as CliError).message).toMatch(/未配置 git upstream/);
  });

  it('有 upstream 但未 fetch（引用不可解析）→ CliError', () => {
    const paths = makeHome({ git: true });
    const bare = attachOrigin(paths.home);
    gitOk(paths.home, ['remote', 'add', 'upstream', bare]);
    // 手工配 upstream 指向不存在的远端分支
    writeStoreFile(paths, 'pi/skills/a/SKILL.md', '# a\n');
    gitOk(paths.home, ['add', '-A']);
    gitOk(paths.home, ['commit', '-m', 'seed']);
    gitOk(paths.home, ['config', 'branch.main.remote', 'upstream']);
    gitOk(paths.home, ['config', 'branch.main.merge', 'refs/heads/main']);

    let error: unknown;
    try {
      requireFastForwardable(paths);
    } catch (err) {
      error = err;
    }
    expect(error).toBeInstanceOf(CliError);
    expect((error as CliError).message).toMatch(/不可解析|未配置/);
  });

  it('非 git 仓库 → CliError（不是 git 仓库）', () => {
    const paths = makeHome();
    expect(() => requireFastForwardable(paths)).toThrow(/不是 git 仓库/);
  });

  it('仓库无 commit（unborn HEAD）→ CliError', () => {
    const paths = makeHome({ git: true });
    const bare = attachOrigin(paths.home);
    gitOk(paths.home, ['remote', 'add', 'upstream', bare]);
    gitOk(paths.home, ['config', 'branch.main.remote', 'upstream']);
    gitOk(paths.home, ['config', 'branch.main.merge', 'refs/heads/main']);
    // bare 里也没有 main → 引用不可解析；先造一个远端 main，再看本地无 commit 的分支
    const other = mkTmp('w6-other');
    gitOk(path.dirname(other), ['clone', bare, other]);
    configureUser(other);
    write(path.join(other, 'store', 'x.md'), 'x\n');
    gitOk(other, ['add', '-A']);
    gitOk(other, ['commit', '-m', 'remote seed']);
    gitOk(other, ['push']);
    gitOk(paths.home, ['fetch']);

    let error: unknown;
    try {
      requireFastForwardable(paths);
    } catch (err) {
      error = err;
    }
    expect(error).toBeInstanceOf(CliError);
    // unborn HEAD：无法比较 → 必须报错而不是静默放行
    expect((error as CliError).message).toMatch(/没有 commit|不可解析|分叉/);
  });
});

/* ------------------------------------------------------------------ */
/* barrel 出口                                                         */
/* ------------------------------------------------------------------ */

describe('sync barrel（src/core/sync/index.ts）', () => {
  it('W6 的出口都可从 barrel 取到（命令层 W7/W8/W9 的 import 面）', async () => {
    const barrel = await import('../../../src/core/sync/index.js');
    expect(typeof barrel.collectSyncSources).toBe('function');
    expect(typeof barrel.applyPullActions).toBe('function');
    expect(typeof barrel.prepareStoreSnapshot).toBe('function');
    expect(typeof barrel.commitStoreIfNeeded).toBe('function');
    expect(typeof barrel.requireCleanStore).toBe('function');
    expect(typeof barrel.requireFastForwardable).toBe('function');
    // W4 的判定层仍同时可用（barrel 不退化）
    expect(typeof barrel.planPull).toBe('function');
    expect(typeof barrel.checkPushSafety).toBe('function');
    expect(typeof barrel.applyExcludeKeyPlaceholders).toBe('function');
  });

  it('barrel 的出口与各模块是同一实现（无重复定义）', async () => {
    const barrel = await import('../../../src/core/sync/index.js');
    expect(barrel.prepareStoreSnapshot).toBe(
      (await import('../../../src/core/sync/pipeline.js')).prepareStoreSnapshot,
    );
    expect(barrel.commitStoreIfNeeded).toBe(
      (await import('../../../src/core/sync/pipeline.js')).commitStoreIfNeeded,
    );
    expect(barrel.requireCleanStore).toBe(
      (await import('../../../src/core/sync/pipeline.js')).requireCleanStore,
    );
    expect(barrel.requireFastForwardable).toBe(
      (await import('../../../src/core/sync/pipeline.js')).requireFastForwardable,
    );
    expect(barrel.applyPullActions).toBe((await import('../../../src/core/sync/apply.js')).applyPullActions);
    expect(barrel.collectSyncSources).toBe((await import('../../../src/core/sync/base.js')).collectSyncSources);
  });
});

/* ------------------------------------------------------------------ */
/* 缝合：pipeline 与 store 读写 + apply 的协作                           */
/* ------------------------------------------------------------------ */

describe('pipeline 与 store / apply 的协作', () => {
  it('prepareStoreSnapshot → 写文件 → commitStoreIfNeeded → readSnapshotFromStore 读回的是占位符', () => {
    const paths = makeHome({ git: true });
    const bare = attachOrigin(paths.home);
    seedUpstream(paths, bare);

    const config: HomerConfig = {
      version: 1,
      adapters: {
        pi: {
          root: '/tmp/pi-root',
          enabled: true,
          categories: { models: { paths: ['models.json'], mode: 'merge', excludeKeys: ['apiKeys'] } },
        },
      },
    };

    const snapshot = snapshotOf({
      'models.json': jsonEntry({ apiKeys: { openai: 'sk-real-secret' }, models: { openai: { id: 'gpt' } } }),
    });
    const [prepared] = prepareStoreSnapshot([snapshot], config);
    // store 布局：<storeDir>/pi/models/models.json
    write(path.join(paths.storeDir, 'pi', 'models', 'models.json'), `${prepared?.categories[0]?.files.get('models.json')?.content ?? ''}`, 'utf8');
    write(path.join(paths.storeDir, 'pi', '.homer-complete'), '');

    expect(commitStoreIfNeeded(paths, 'homer push: models')).toBeDefined();

    const readBack = readSnapshotFromStore(paths, config);
    const content = readBack[0]?.categories[0]?.files.get('models.json')?.content ?? '';
    expect(content).toContain('"apiKeys": "__REQUIRED__"');
    expect(content).not.toContain('sk-real-secret');
    // 提交后的 store 是干净的 → requireCleanStore 通过
    expect(() => requireCleanStore(paths)).not.toThrow();
  });
});
