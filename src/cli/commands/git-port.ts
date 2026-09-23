/**
 * 命令层共享的 git 端口（P4-W10 缝隙修复）。
 *
 * ## 为什么需要它
 *
 * P3 是三个并行 worker 分别实现 push / pull / merge，三者都需要「单测可替换 git 行为」：
 *   - **W8-pull** 定义了 `PullGitPort`（7 个可选成员）并自带 `resolveGitPort`；
 *   - **W7-push** / **W9-merge** 直接 `import { ... } from '../../core/git/index.js'`，
 *     没有任何注入位（各自的测试改为走真实临时仓库 + PATH shim）。
 *
 * 两份形状各写一份的代价是它们会漂移：同一个「前置换进」在不同命令里可能被替换成不同的
 * 子集，测试断言的就再也不是同一条代码路径。本模块把端口提升为**唯一类型** `GitPort`：
 * 成员全部可选，未覆盖的走 `src/core/git` 与 `src/core/sync/pipeline` 的真实实现
 * （`resolveGitPort` 负责合并），push / pull / merge 三者共用同一个解析函数。
 *
 * ## 与冻结签名/既有测试的关系
 *
 * - `PullDeps.git` / `MergeDeps.git` / `PushDeps.git` 都是**可选**字段（additive），
 *   不传时行为与改动前逐字相同（全部走真实实现）。
 * - `PullGitPort` 保留为 `GitPort` 的别名（既有 `import type { PullGitPort }` 无需改动）。
 * - 本模块不引入任何新依赖，只做「类型 + 一次合并」。
 */

import * as gitCore from '../../core/git/index.js';
import { commitStoreIfNeeded, requireCleanStore, requireFastForwardable } from '../../core/sync/pipeline.js';
import type { GitExecResult } from '../../core/git/index.js';
import type { HomerPaths } from '../../core/paths.js';

/**
 * git 端口：命令层用到的全部 git / store-git 操作，逐项可选。
 *
 * 每项语义与其真实实现一一对应（见各字段注释）；`undefined` 的成员由 `resolveGitPort`
 * 填成真实实现，因此命令层可以无条件调用。
 */
export interface GitPort {
  /** 是否 git 仓库根（`git rev-parse --show-toplevel` == home）。 */
  isGitRepo?: (home: string) => boolean;
  /** 是否配置了 upstream（`@{upstream}`）。 */
  hasUpstream?: (home: string) => boolean;
  /**
   * 是否有可推送的目标（additive，P4-W10 缝隙修复）：
   * 已有 upstream，**或**当前分支配置了存在的 remote（`branch.<name>.remote`）——
   * 后者是「刚 clone 下的配置中心：分支已指向 origin，但远程跟踪引用尚未建立」的首次 push。
   * 没有这个判定，`homer push` 在首次运行时会因 `hasUpstream === false` 误入 local-only，
   * 而 `git push` 本身（`push.default=simple` + branch 配置）是可以成功的。
   */
  hasPushTarget?: (home: string) => boolean;
  /** upstream 短名（`origin/main`）；无 upstream → undefined。 */
  upstreamRef?: (home: string) => string | undefined;
  /** `git fetch`（永不 throw，失败 → `{ok:false}`）。 */
  gitFetch?: (home: string) => GitExecResult;
  /** `git push`（永不 throw）。 */
  gitPush?: (home: string) => GitExecResult;
  /** HEAD SHA；unborn HEAD / 非仓库 → undefined。 */
  headCommit?: (home: string) => string | undefined;
  /** `git init`（若缺）+ 幂等维护 .gitignore。 */
  ensureGitRepo?: (home: string) => void;
  /** 提交 store 变更；无变更 / 失败 → undefined。 */
  commitStoreIfNeeded?: (paths: HomerPaths, message: string) => string | undefined;
  /** store 工作区是否干净。 */
  isStoreClean?: (home: string) => boolean;
  /** `git merge --ff-only @{upstream}`。 */
  mergeFfUpstream?: (home: string) => GitExecResult;
  /** 断言 store 干净（脏 / 非仓库 → CliError）。 */
  requireCleanStore?: (paths: HomerPaths) => void;
  /** 断言 HEAD 是 upstream 祖先（分叉 / 无 upstream → CliError）。 */
  requireFastForwardable?: (paths: HomerPaths) => void;
}

/** `resolveGitPort` 的返回值：所有成员都可直接调用。 */
export type ResolvedGitPort = Required<GitPort>;

/**
 * 把注入的端口与真实实现合并（未覆盖项走 `src/core/git` / `src/core/sync/pipeline`）。
 *
 * 每次调用都返回一个新对象，故调用方不必担心共享可变状态；注入对象本身不被修改。
 */
export function resolveGitPort(port?: GitPort): ResolvedGitPort {
  return {
    isGitRepo: port?.isGitRepo ?? gitCore.isGitRepo,
    hasUpstream: port?.hasUpstream ?? gitCore.hasUpstream,
    hasPushTarget: port?.hasPushTarget ?? defaultHasPushTarget,
    upstreamRef: port?.upstreamRef ?? gitCore.upstreamRef,
    gitFetch: port?.gitFetch ?? ((home: string) => gitCore.gitFetch(home)),
    gitPush: port?.gitPush ?? ((home: string) => gitCore.gitPush(home)),
    headCommit: port?.headCommit ?? gitCore.headCommit,
    ensureGitRepo: port?.ensureGitRepo ?? gitCore.ensureGitRepo,
    commitStoreIfNeeded: port?.commitStoreIfNeeded ?? commitStoreIfNeeded,
    isStoreClean: port?.isStoreClean ?? gitCore.isStoreClean,
    mergeFfUpstream: port?.mergeFfUpstream ?? ((home: string) => gitCore.mergeFfUpstream(home)),
    requireCleanStore: port?.requireCleanStore ?? requireCleanStore,
    requireFastForwardable: port?.requireFastForwardable ?? requireFastForwardable,
  };
}

/**
 * 默认的可推送目标判定（真实实现）：
 *   1. `@{upstream}` 已可解析 → true；
 *   2. 否则看当前分支的 `branch.<name>.remote` 是否指向一个真实存在的 remote。
 * 不做任何写操作，也不发网络请求（`git remote` 仅读本地配置）。
 */
function defaultHasPushTarget(home: string): boolean {
  if (gitCore.hasUpstream(home)) return true;

  const branch = gitCore.gitExec(home, ['symbolic-ref', '--short', 'HEAD']);
  if (!branch.ok) return false;
  const name = branch.stdout.trim();
  if (name === '') return false;

  const configured = gitCore.gitExec(home, ['config', '--get', `branch.${name}.remote`]);
  const remote = configured.ok ? configured.stdout.trim() : '';
  if (remote === '') return false;

  const remotes = gitCore.gitExec(home, ['remote']);
  if (!remotes.ok) return false;
  return remotes.stdout.split('\n').map((line) => line.trim()).includes(remote);
}
