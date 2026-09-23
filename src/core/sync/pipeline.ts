/**
 * 写 store 前的统一入口 + git 前置检查（docs/m2-plan.md §2.5，P2-W6）。
 *
 * 四个出口分两类：
 *
 * 1. **写 store 前的统一入口** —— `prepareStoreSnapshot`：
 *    push / merge 落盘 store 前必须先把 `excludeKeys` 顶层键值换成 `__REQUIRED__` 占位符
 *    （DESIGN §2.2 / §2.7），否则用户密钥会进 git 历史。本函数是这条规则的**唯一入口**，
 *    命令层不得自行 map（`plan §2.5` 原文：「= applyExcludeKeyPlaceholders」）。
 *
 * 2. **git 前置检查**（§1 降级矩阵 / §2.8 冻结顺序：全部先于任何写操作）：
 *    - `commitStoreIfNeeded`  → 提交 store 变更（无变更 / 失败 → `undefined`，不 throw）
 *    - `requireCleanStore`    → store 工作区脏 / 非 git 仓库 → `CliError`
 *    - `requireFastForwardable` → HEAD 不是 upstream 的祖先（分叉）/ 无 upstream → `CliError`
 *
 * 「该不该检查」由命令层决定（push 不需要 requireFastForwardable），本模块只负责
 * 「怎么检查、怎么报错」——报错一律 `CliError`，由分发层统一打印并 exit 1。
 */

import { applyExcludeKeyPlaceholders } from './excluded-keys.js';
import { commitAllStore, gitExec, headCommit, isAncestorOf, isGitRepo, isStoreClean, upstreamRef } from '../git/index.js';
import { CliError } from '../errors.js';
import type { HomerPaths } from '../paths.js';
import type { AdapterSnapshot, HomerConfig } from '../types.js';

/* ------------------------------------------------------------------ */
/* 前置检查的共享措辞（对抗式 review 修复 minor 2）                     */
/* ------------------------------------------------------------------ */

/**
 * 前置检查失败时的 `CliError` 文本**唯一来源**。
 *
 * 这些句子曾在 `pipeline.ts` / `commands/pull.ts` / `commands/merge.ts` 各写一份，
 * 任何一处改动都可能让「同一个前置失败」在不同命令里给出不同提示。统一收敛到这里，
 * 三个调用方（pipeline 的 requireCleanStore / requireFastForwardable、pull、merge）共用。
 * 纯文本常量，无行为依赖。
 */
export const NOT_A_REPO_HINT = '请先运行 `homer push` 建立 git 历史与 remote。';

export const NO_UPSTREAM_HINT =
  '请先 `git push -u <remote> <branch>`（或在 `~/.homer` 内 `git branch --set-upstream-to`）配置远端。';

/** `未配置 git upstream，无法确定远端`（三处逐字相同的主消息）。 */
export const NO_UPSTREAM_MESSAGE = '未配置 git upstream，无法确定远端';

/** `工作区不是 git 仓库: <home>`（带路径，主消息由函数生成，避免拼写漂移）。 */
export function notAGitRepoMessage(home: string): string {
  return `工作区不是 git 仓库: ${home}`;
}

/**
 * 写 store 前的快照准备：把 `excludeKeys` 列出的顶层键值替换为 `__REQUIRED__` 占位符。
 *
 * 只替换**原本就存在**的键（不凭空造键），且非 JSON / 无命中键的文件字节不变
 * （实现见 `excluded-keys.ts`，本函数只是对每个 adapter 逐一套用）。
 */
export function prepareStoreSnapshot(
  local: readonly AdapterSnapshot[],
  config: HomerConfig,
): AdapterSnapshot[] {
  return local.map((snapshot) => applyExcludeKeyPlaceholders(snapshot, config));
}

/**
 * 提交 store 变更（`git add -A store/` + `git commit`）。
 *
 * `undefined` 的三种情形（调用方语义相同：「没有需要记录的新 commit」）：
 *   - store 无变更（幂等重跑）；
 *   - store 工作区是脏的但 commit 失败（如缺 `user.email`）；
 *   - 不是 git 仓库。
 * 后两种会让用户以为「已同步」而实际没有 —— 故命令层必须把 `undefined` 与
 * 「store 本来就没变」区分对待时，应先自行 `requireCleanStore` / 检查仓库存在。
 *
 * 注意：本函数**不**调 `ensureGitRepo`（D1：首次 push 的 `git init` 由 push 命令显式执行），
 * 保持「是否进入 local-only 模式」这一决策留在命令层可见的位置。
 */
export function commitStoreIfNeeded(paths: HomerPaths, message: string): string | undefined {
  return commitAllStore(paths.home, message);
}

/**
 * 断言 store 工作区干净（D6：pull 前置检查——带着未提交的 store 改动做 ff 会丢改动）。
 * 脏 / 不可判定 → `CliError`（提示先 `homer push`）。
 *
 * 非 git 仓库同样抛 `CliError`（§1 降级矩阵：非仓库的 pull / merge 直接报错，
 * 而不是让「git status 失败」伪装成「store 脏」这种误导性提示）。
 */
export function requireCleanStore(paths: HomerPaths): void {
  if (!isGitRepo(paths.home)) {
    throw new CliError(notAGitRepoMessage(paths.home), NOT_A_REPO_HINT);
  }
  if (!isStoreClean(paths.home)) {
    throw new CliError(
      'store 工作区有未提交的改动',
      '请先运行 `homer push` 提交这些改动，再执行 pull / merge（否则 ff 会覆盖它们）。',
    );
  }
}

/** `@{upstream}` 解析成 commit SHA；无 upstream / 未 fetch / 非仓库 → undefined。 */
function upstreamCommit(paths: HomerPaths): string | undefined {
  const ref = upstreamRef(paths.home);
  if (ref === undefined) return undefined;
  const result = gitExec(paths.home, ['rev-parse', '--verify', `${ref}^{commit}`]);
  if (!result.ok) return undefined;
  const sha = result.stdout.trim();
  return sha === '' ? undefined : sha;
}

/**
 * 断言当前 HEAD 是 upstream 的祖先（D6：`git merge --ff-only` 能成功）。
 *
 * 分叉（本地有未推送 commit 且远端前进）→ `CliError` 提示先 `homer push`。
 * 该检查必须在**应用工具目录之前**完成：ff 失败后再回滚已写入的文件是做不到的
 * （用户可能已改过其中一些），故宁可拒绝整个 pull。
 *
 * 无 upstream / 仓库无 commit → 同样 `CliError`（pull 语义上无远端可拉）。
 */
export function requireFastForwardable(paths: HomerPaths): void {
  if (!isGitRepo(paths.home)) {
    throw new CliError(notAGitRepoMessage(paths.home), NOT_A_REPO_HINT);
  }

  const ref = upstreamRef(paths.home);
  if (ref === undefined) {
    throw new CliError(NO_UPSTREAM_MESSAGE, NO_UPSTREAM_HINT);
  }

  const remote = upstreamCommit(paths);
  if (remote === undefined) {
    throw new CliError(
      `git upstream ${ref} 不可解析（是否尚未 fetch？）`,
      '请检查网络与 remote 配置后重试。',
    );
  }

  const head = headCommit(paths.home);
  if (head === undefined) {
    throw new CliError(
      '本地仓库没有 commit，无法快进到远端',
      '请先运行 `homer push` 建立本地提交。',
    );
  }

  if (head === remote) return; // 已同步，无 drift
  if (!isAncestorOf(paths.home, head, remote)) {
    throw new CliError(
      `本地与远端已分叉（HEAD ${head.slice(0, 7)} 不是 ${ref} 的祖先）`,
      '请先运行 `homer push` 推送本地提交，再执行 pull / merge。',
    );
  }
}
