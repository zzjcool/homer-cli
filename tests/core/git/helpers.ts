/**
 * W3-git 测试共享工具。
 *
 * 全部隔离：每个用例用 `mkdtemp` 造临时目录 + 临时 git 仓库（必要时 `git init --bare`
 * 假 origin + clone），并**总是**设置仓库级 user.email / user.name——
 * CI 环境可能没有全局 git 身份，缺身份会让 commit 失败而污染无关断言。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import { gitExec, type GitExecResult } from '../../../src/core/git/git.js';
import { getHomerPaths, type HomerPaths } from '../../../src/core/paths.js';

/** 本次测试进程创建的所有临时目录（afterEach 统一清理）。 */
const created: string[] = [];

export function trackDir(dir: string): string {
  created.push(dir);
  return dir;
}

export function cleanupTmp(): void {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) fs.rmSync(dir, { recursive: true, force: true });
  }
}

/** `mkdtemp` 出的临时根目录。 */
export function mkTmp(prefix: string): string {
  return trackDir(fs.mkdtempSync(path.join(os.tmpdir(), `homer-${prefix}-`)));
}

/** 临时根目录下的子路径（父目录不存在时按需创建）。 */
export function mkSubdir(parent: string, name: string): string {
  const dir = path.join(parent, name);
  fs.mkdirSync(dir, { recursive: true });
  return dir;
}

/** 断言 git 命令成功并返回 stdout。 */
export function gitOk(cwd: string, args: readonly string[], opts?: { timeoutMs?: number }): string {
  const result = gitExec(cwd, args, opts);
  if (!result.ok) {
    throw new Error(`git ${args.join(' ')} 失败 (cwd=${cwd}): ${result.stderr}`);
  }
  return result.stdout;
}

export function gitRaw(cwd: string, args: readonly string[]): GitExecResult {
  return gitExec(cwd, args);
}

/** 仓库级身份（不依赖全局 ~/.gitconfig）。 */
export function configureUser(repo: string): void {
  gitOk(repo, ['config', 'user.email', 'homer-test@example.invalid']);
  gitOk(repo, ['config', 'user.name', 'Homer Test']);
}

/** 初始化一个临时 git 仓库（分支名固定 main，orgin 侧断言才可确定）。 */
export function initRepo(prefix = 'repo'): string {
  const repo = mkTmp(prefix);
  gitOk(repo, ['init', '-b', 'main']);
  configureUser(repo);
  return repo;
}

/** `git init --bare` 假 origin。 */
export function initBare(prefix = 'origin'): string {
  const parent = mkTmp(prefix);
  const bare = path.join(parent, 'origin.git');
  gitOk(parent, ['init', '--bare', '-b', 'main', bare]);
  return bare;
}

/** 从假 origin clone 一个工作仓库（clone 的 origin 即 bare）。 */
export function cloneRepo(bare: string, prefix = 'clone'): string {
  const parent = mkTmp(prefix);
  const dest = path.join(parent, 'work');
  gitOk(parent, ['clone', bare, dest]);
  configureUser(dest);
  return dest;
}

/** 与某仓库 `home` 配套的 HomerPaths（storeDir 落在仓库内，符合 §2.1 布局）。 */
export function pathsFor(home: string): HomerPaths {
  return getHomerPaths({ HOMER_HOME: home });
}

/** 写文件（自动创建父目录）。 */
export function writeFile(abs: string, content: string): void {
  fs.mkdirSync(path.dirname(abs), { recursive: true });
  fs.writeFileSync(abs, content, 'utf8');
}

/** `HEAD` 的完整 SHA（测试内方便断言）。 */
export function shaOf(repo: string): string {
  return gitOk(repo, ['rev-parse', 'HEAD']).trim();
}
