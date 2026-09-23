/**
 * W3-git / git.ts 验收（docs/m2-plan.md §3-P1-W3）。
 *
 * 全部用 mkdtemp 临时仓库（+ `git init --bare` 假 origin + clone）：
 * 绝不碰真实 `~/.homer`、`~/.pi`、真实 remote。
 */

import fs from 'node:fs';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import {
  commitAllStore,
  ensureGitRepo,
  gitExec,
  gitFetch,
  gitPush,
  hasUpstream,
  headCommit,
  isAncestorOf,
  isGitRepo,
  isStoreClean,
  mergeFfUpstream,
  upstreamRef,
  GITIGNORE_REQUIRED_LINES,
  GIT_DEFAULT_TIMEOUT_MS,
} from '../../../src/core/git/git.js';
import {
  cleanupTmp,
  cloneRepo,
  configureUser,
  gitOk,
  initBare,
  initRepo,
  mkSubdir,
  mkTmp,
  shaOf,
  writeFile,
} from './helpers.js';

afterEach(cleanupTmp);

describe('gitExec — 永不 throw', () => {
  it('缺省超时为 15s', () => {
    expect(GIT_DEFAULT_TIMEOUT_MS).toBe(15_000);
  });

  it('不存在的子命令 → ok:false + stderr（不抛）', () => {
    const repo = initRepo('exec-badcmd');
    const result = gitExec(repo, ['definitely-not-a-git-command']);
    expect(result.ok).toBe(false);
    expect(result.stderr).toMatch(/not a git command/);
    expect(result.stdout).toBe('');
  });

  it('非仓库目录 → 失败但不抛', () => {
    const dir = mkTmp('exec-nonrepo');
    expect(() => gitExec(dir, ['rev-parse', 'HEAD'])).not.toThrow();
    expect(gitExec(dir, ['rev-parse', 'HEAD']).ok).toBe(false);
  });

  it('超时 → ok:false（短 timeoutMs 注入，不抛不挂）', () => {
    const repo = initRepo('exec-timeout');
    // 借 git 自带 alias 造一个必挂的命令（无需外部脚本、可跨平台）。
    const started = Date.now();
    const result = gitExec(repo, ['-c', 'alias.hang=!sleep 30', 'hang'], { timeoutMs: 400 });
    const elapsed = Date.now() - started;
    expect(result.ok).toBe(false);
    expect(result.stderr).toMatch(/ETIMEDOUT|timed out/i);
    expect(elapsed).toBeLessThan(10_000);
  });

  it('git 可执行文件缺失（PATH 抹掉）→ ok:false + ENOENT（不抛）', () => {
    const repo = initRepo('exec-enoent');
    const saved = process.env['PATH'];
    try {
      process.env['PATH'] = '/nonexistent-homer-path';
      const result = gitExec(repo, ['status']);
      expect(result.ok).toBe(false);
      expect(result.stderr).toMatch(/ENOENT/);
    } finally {
      if (saved === undefined) delete process.env['PATH'];
      else process.env['PATH'] = saved;
    }
  });

  it('成功命令 → ok:true + stdout', () => {
    const repo = initRepo('exec-ok');
    const result = gitExec(repo, ['rev-parse', '--is-inside-work-tree']);
    expect(result.ok).toBe(true);
    expect(result.stdout.trim()).toBe('true');
  });
});

describe('isGitRepo', () => {
  it('普通目录 → false；init 后 → true', () => {
    const dir = mkTmp('isrepo');
    expect(isGitRepo(dir)).toBe(false);
    gitOk(dir, ['init', '-b', 'main']);
    expect(isGitRepo(dir)).toBe(true);
  });

  it('位于其它仓库内部的子目录 → false（不得把外层仓库当自己的）', () => {
    const repo = initRepo('isrepo-outer');
    const nested = mkSubdir(repo, 'nested');
    expect(isGitRepo(repo)).toBe(true);
    expect(isGitRepo(nested)).toBe(false);
  });
});

describe('ensureGitRepo — init + 幂等 .gitignore', () => {
  it('首次运行：init + 写入全部必需项（M3 超 `keys/`）', () => {
    const home = mkTmp('ensure-first');
    ensureGitRepo(home);

    expect(isGitRepo(home)).toBe(true);
    const gitignore = fs.readFileSync(path.join(home, '.gitignore'), 'utf8');
    const lines = gitignore.split('\n');
    for (const required of GITIGNORE_REQUIRED_LINES) {
      expect(lines.filter((line) => line === required)).toHaveLength(1);
    }
    expect(gitignore).toContain('state.json');
    expect(gitignore).toContain('backups/');
    // M3 §2.0-5：age 私钥目录 `keys/` 不入库（D2）。
    expect(gitignore).toContain('keys/');
  });

  it('二次运行不重复追加（字节级不变）', () => {
    const home = mkTmp('ensure-idempotent');
    ensureGitRepo(home);
    const first = fs.readFileSync(path.join(home, '.gitignore'), 'utf8');

    ensureGitRepo(home);
    ensureGitRepo(home);
    const third = fs.readFileSync(path.join(home, '.gitignore'), 'utf8');

    expect(third).toBe(first);
    expect(third.split('\n').filter((line) => line === 'state.json')).toHaveLength(1);
    expect(third.split('\n').filter((line) => line === 'backups/')).toHaveLength(1);
    expect(third.split('\n').filter((line) => line === 'keys/')).toHaveLength(1);
  });

  it('保留用户已有规则 + 只追加缺失项（缺换行结尾也补齐）', () => {
    const home = mkTmp('ensure-preserve');
    fs.mkdirSync(home, { recursive: true });
    fs.writeFileSync(path.join(home, '.gitignore'), 'state.json\n*.log', 'utf8');

    ensureGitRepo(home);

    const content = fs.readFileSync(path.join(home, '.gitignore'), 'utf8');
    // M3 §2.0-5：必需行新增 `keys/`（age 私钥目录不入库）。
    expect(content).toBe('state.json\n*.log\nbackups/\nkeys/\n');
    expect(content.split('\n').filter((line) => line === 'state.json')).toHaveLength(1);
  });

  it('home 不存在时自建目录', () => {
    const home = path.join(mkTmp('ensure-mkdir'), 'deep', 'homer');
    expect(fs.existsSync(home)).toBe(false);
    ensureGitRepo(home);
    expect(isGitRepo(home)).toBe(true);
  });

  it('M3 §2.0-5：`keys/` 行真实生效（git check-ignore 命中 age.txt）', () => {
    const home = mkTmp('ensure-keys-ignored');
    ensureGitRepo(home);
    fs.mkdirSync(path.join(home, 'keys'), { recursive: true });
    writeFile(path.join(home, 'keys', 'age.txt'), 'AGE-nothing-real\n');

    const ignored = gitExec(home, ['check-ignore', '-v', 'keys/age.txt']);
    expect(ignored.ok).toBe(true);
    // 私钥文件不出现在未跟踪清单里（唯一可能出现的其它项是 .gitignore 自身）。
    const untracked = gitExec(home, ['status', '--porcelain']).stdout;
    expect(untracked).not.toContain('keys/');
  });
});

describe('commitAllStore', () => {
  it('无变更 → undefined（store 工作区干净）', () => {
    const repo = initRepo('commit-nodiff');
    expect(commitAllStore(repo, 'nothing to commit')).toBeUndefined();
  });

  it('有变更 → 新 SHA，且 git log 可见该 message', () => {
    const repo = initRepo('commit-diff');
    writeFile(path.join(repo, 'store', 'pi', 'settings.json'), '{"model":"sonnet"}\n');

    const sha = commitAllStore(repo, 'feat: first store commit');
    expect(sha).toBeDefined();
    expect(sha).toBe(shaOf(repo));
    expect(gitOk(repo, ['log', '-1', '--format=%s']).trim()).toBe('feat: first store commit');
    expect(gitOk(repo, ['log', '-1', '--format=%H']).trim()).toBe(sha);

    // 只覆盖 store/：store 外的未跟踪文件不掺进本次 commit。
    writeFile(path.join(repo, 'homer.json'), '{}\n');
    writeFile(path.join(repo, 'store', 'pi', 'settings.json'), '{"model":"opus"}\n');
    const second = commitAllStore(repo, 'feat: second');
    expect(second).not.toBe(sha);
    expect(gitOk(repo, ['show', '--name-only', '--format=', 'HEAD']).trim()).toBe(
      'store/pi/settings.json',
    );
    expect(gitOk(repo, ['status', '--porcelain'])).toContain('?? homer.json');

    // 无变更 → undefined（幂等）
    expect(commitAllStore(repo, 'again')).toBeUndefined();
  });

  it('非仓库 → undefined（不抛）', () => {
    const dir = mkTmp('commit-nonrepo');
    expect(() => commitAllStore(dir, 'nope')).not.toThrow();
    expect(commitAllStore(dir, 'nope')).toBeUndefined();
  });

  it('缺 git 身份 → undefined（不抛）', () => {
    const repo = mkTmp('commit-noidentity');
    gitOk(repo, ['init', '-b', 'main']);
    // 显式清掉身份（用空配置文件兜底，避免继承全局 ~/.gitconfig）
    const isolated = path.join(repo, 'empty-gitconfig');
    fs.writeFileSync(isolated, '', 'utf8');
    writeFile(path.join(repo, 'store', 'pi', 'x.txt'), 'x\n');
    const saved = process.env['GIT_CONFIG_GLOBAL'];
    try {
      process.env['GIT_CONFIG_GLOBAL'] = isolated;
      process.env['GIT_CONFIG_NOSYSTEM'] = '1';
      expect(commitAllStore(repo, 'should fail')).toBeUndefined();
    } finally {
      if (saved === undefined) delete process.env['GIT_CONFIG_GLOBAL'];
      else process.env['GIT_CONFIG_GLOBAL'] = saved;
      delete process.env['GIT_CONFIG_NOSYSTEM'];
    }
  });
});

describe('headCommit', () => {
  it('unborn HEAD → undefined；commit 后 → SHA', () => {
    const repo = initRepo('head');
    expect(headCommit(repo)).toBeUndefined();
    writeFile(path.join(repo, 'store', 'pi', 'x.txt'), 'x\n');
    const sha = commitAllStore(repo, 'init store');
    expect(headCommit(repo)).toBe(sha);
  });
});

describe('isStoreClean', () => {
  it('已提交 → true；改动 store → false；非仓库 → false', () => {
    const repo = initRepo('clean');
    // 空仓库 + 尚无 store/ 目录 → git status 无输出（如实反映「无待提交内容」）
    expect(isStoreClean(repo)).toBe(true);
    writeFile(path.join(repo, 'store', 'pi', 'settings.json'), '{}\n');
    expect(isStoreClean(repo)).toBe(false); // 未提交的 store 内容 → 不干净
    commitAllStore(repo, 'store init');
    expect(isStoreClean(repo)).toBe(true);

    writeFile(path.join(repo, 'store', 'pi', 'settings.json'), '{"a":1}\n');
    expect(isStoreClean(repo)).toBe(false);

    commitAllStore(repo, 'store change');
    expect(isStoreClean(repo)).toBe(true);

    // store 外的改动不影响判定（state.json / backups/ 本就不入库）
    writeFile(path.join(repo, 'state.json'), '{}\n');
    writeFile(path.join(repo, 'backups', '20260101', 'x'), 'x\n');
    expect(isStoreClean(repo)).toBe(true);

    expect(isStoreClean(mkTmp('clean-nonrepo'))).toBe(false);
  });
});

describe('upstream / fetch / push', () => {
  it('无 upstream → hasUpstream false, upstreamRef undefined；push 失败但不抛', () => {
    const repo = initRepo('upstream-none');
    writeFile(path.join(repo, 'store', 'pi', 'x.txt'), 'x\n');
    commitAllStore(repo, 'init');

    expect(hasUpstream(repo)).toBe(false);
    expect(upstreamRef(repo)).toBeUndefined();

    // push 必然失败（无 remote 可推）；「无 upstream」的判定依据是 hasUpstream，
    // 不是 fetch 的成败——`git fetch` 在无 remote 时是**静默成功的 no-op**（git 自身语义，
    // 各版本一致），故这里只断言它不抛、且其结果不妨碍 hasUpstream 的门禁作用。
    expect(gitPush(repo).ok).toBe(false);
    expect(() => gitFetch(repo)).not.toThrow();
    expect(hasUpstream(repo)).toBe(false);
  });

  it('clone + push -u 后：upstreamRef == origin/main，fetch/push ok', () => {
    const bare = initBare('upstream-bare');
    const clone = cloneRepo(bare, 'upstream-clone');
    expect(hasUpstream(clone)).toBe(false); // 空 clone 尚无 upstream（push -u 才建立）

    writeFile(path.join(clone, 'store', 'pi', 'x.txt'), 'x\n');
    commitAllStore(clone, 'init');
    expect(gitOk(clone, ['push', '-u', 'origin', 'main'])).toBeDefined();

    expect(hasUpstream(clone)).toBe(true);
    expect(upstreamRef(clone)).toBe('origin/main');
    expect(gitFetch(clone).ok).toBe(true);
    expect(gitPush(clone).ok).toBe(true); // 无新 commit 的 push 仍成功
    expect(shaOf(clone)).toBe(shaOf(bare));
  });
});

describe('isAncestorOf', () => {
  it('祖先 → true；兄弟分支 / 无关 commit → false', () => {
    const repo = initRepo('ancestor');
    writeFile(path.join(repo, 'store', 'pi', 'x.txt'), 'x\n');
    const first = commitAllStore(repo, 'c1');
    writeFile(path.join(repo, 'store', 'pi', 'x.txt'), 'y\n');
    const second = commitAllStore(repo, 'c2');

    expect(isAncestorOf(repo, first as string, second as string)).toBe(true);
    expect(isAncestorOf(repo, second as string, first as string)).toBe(false);
    expect(isAncestorOf(repo, first as string, first as string)).toBe(true);

    gitOk(repo, ['checkout', '-b', 'side', first as string]);
    writeFile(path.join(repo, 'store', 'pi', 'side.txt'), 's\n');
    const side = commitAllStore(repo, 'side');
    gitOk(repo, ['checkout', 'main']);
    expect(isAncestorOf(repo, second as string, side as string)).toBe(false);
    expect(isAncestorOf(repo, side as string, second as string)).toBe(false);
  });

  it('未知 ref → false（不抛）', () => {
    const repo = initRepo('ancestor-unknown');
    expect(isAncestorOf(repo, 'deadbeef'.repeat(5), 'HEAD')).toBe(false);
  });
});

describe('mergeFfUpstream', () => {
  it('落后 upstream → ff 成功，HEAD == upstream', () => {
    const bare = initBare('ff-bare');
    const writer = cloneRepo(bare, 'ff-writer');
    writeFile(path.join(writer, 'store', 'pi', 'x.txt'), 'v1\n');
    commitAllStore(writer, 'c1');
    gitOk(writer, ['push', '-u', 'origin', 'main']);

    const reader = cloneRepo(bare, 'ff-reader');
    expect(hasUpstream(reader)).toBe(true);
    expect(headCommit(reader)).toBe(shaOf(writer));

    // writer 前进一个 commit 并 push
    writeFile(path.join(writer, 'store', 'pi', 'x.txt'), 'v2\n');
    const ahead = commitAllStore(writer, 'c2');
    gitOk(writer, ['push']);

    expect(gitFetch(reader).ok).toBe(true);
    expect(isAncestorOf(reader, headCommit(reader) as string, '@{upstream}')).toBe(true);
    const merge = mergeFfUpstream(reader);
    expect(merge.ok).toBe(true);
    expect(headCommit(reader)).toBe(ahead);
    expect(headCommit(reader)).toBe(shaOf(bare));
    expect(fs.readFileSync(path.join(reader, 'store', 'pi', 'x.txt'), 'utf8')).toBe('v2\n');
  });

  it('分叉（本地有未推送 commit）→ ff 失败，HEAD 不变', () => {
    const bare = initBare('diverge-bare');
    const local = cloneRepo(bare, 'diverge-local');
    writeFile(path.join(local, 'store', 'pi', 'x.txt'), 'base\n');
    commitAllStore(local, 'c1');
    gitOk(local, ['push', '-u', 'origin', 'main']);

    // 本地生成一个不推送的 commit
    writeFile(path.join(local, 'store', 'pi', 'local.txt'), 'mine\n');
    const localOnly = commitAllStore(local, 'local-only');
    expect(localOnly).toBeDefined();

    // origin 侧（另一 clone）前进
    const other = cloneRepo(bare, 'diverge-other');
    writeFile(path.join(other, 'store', 'pi', 'remote.txt'), 'theirs\n');
    commitAllStore(other, 'remote-only');
    gitOk(other, ['push']);

    expect(gitFetch(local).ok).toBe(true);
    expect(isAncestorOf(local, headCommit(local) as string, '@{upstream}')).toBe(false);
    const merge = mergeFfUpstream(local);
    expect(merge.ok).toBe(false);
    expect(merge.stderr).toMatch(/fast-forward|not possible|fatal/i);
    expect(headCommit(local)).toBe(localOnly);
  });

  it('无 upstream → 失败（不抛）', () => {
    const repo = initRepo('ff-none');
    writeFile(path.join(repo, 'store', 'pi', 'x.txt'), 'x\n');
    commitAllStore(repo, 'c1');
    const merge = mergeFfUpstream(repo);
    expect(merge.ok).toBe(false);
  });
});

describe('ensureGitRepo 与 commit 的组合（local-only 模式，D1）', () => {
  it('非仓库 → ensureGitRepo → commitAllStore 得到可读历史', () => {
    const home = mkTmp('localonly');
    ensureGitRepo(home);
    // 无身份时显式配置（测试环境防御；CI 不保证全局 git 身份）
    configureUser(home);

    expect(commitAllStore(home, 'empty store')).toBeUndefined();
    writeFile(path.join(home, 'store', 'pi', 'skills', 'foo', 'SKILL.md'), '# foo\n');
    const sha = commitAllStore(home, 'store init');
    expect(sha).toBeDefined();
    expect(gitOk(home, ['log', '-1', '--format=%s']).trim()).toBe('store init');
    expect(headCommit(home)).toBe(sha);
    expect(isStoreClean(home)).toBe(true);
    expect(hasUpstream(home)).toBe(false);
  });
});
