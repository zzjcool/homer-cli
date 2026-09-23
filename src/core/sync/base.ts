/**
 * 三方同步原料采集：base / local / remote（docs/m2-plan.md §2.5，P2-W6）。
 *
 * 冻结签名：`collectSyncSources(paths, config, opts?): SyncSources`
 *
 * ## 三方来源（§1 决策 D3 / D4）
 *
 * | 侧     | git 模式（`state.lastSyncCommit` 在 git 历史中可读）  | 降级（无 state / commit 不可读）          |
 * |--------|------------------------------------------------------|-------------------------------------------|
 * | base   | `readStoreSnapshotAtCommit(lastSyncCommit)`           | `readSnapshotFromStore`（store 工作区）   |
 * | remote | `readStoreSnapshotAtCommit(@{upstream} 的 SHA)`        | 同 base                                   |
 * | local  | `scanAdapter` 的**原始**快照（不做 excludeKeys 剥离） | 同左                                      |
 *
 * - **不剥离 excludeKeys**（plan §2.5 原文）：剥离是判定层的事（`planPull` / `checkPushSafety`
 *   / `status` 各自按同一实现点做），且 `planPull` 的「植回本地密钥键」必须回到**原始** local
 *   快照，故此处 base / local / remote 一律保持原始字节。
 * - **`mode`** 表达 **base 的来源**：来自 git 历史 → `'git'`（并填 `baseCommit`）；
 *   回落到 store 工作区 → `'store'`。`remoteRef` 只要解析到 upstream 短名就填（报告 / 降级判定用）。
 * - **`opts.fetch=false`**：测试 / 离线快路径，跳过网络（不跑 `git fetch`），仍读 `@{upstream}` 引用。
 * - **fetch 失败**（离线）：记 `warnings` + `remote := base`（§1 降级矩阵：退化为 M1 语义，仅 push 可用）。
 *   pull / merge 的「无 upstream / 离线 → CliError」由命令层在**调用本函数前**用
 *   `isGitRepo` + `hasUpstream` + `gitFetch` 自行拦截（§2.8 冻结的检查顺序，全部先于任何写操作）——
 *   本函数不替它们做决定，只提供降级后的原料。
 * - **root 不可读的 adapter → `local := base`**（沿用 M-A 守卫）：扫不到 ≠ 本地全空，
 *   否则 base 里每个文件都会被判成假删除（`pull-delete`），把用户目录清空。原因记入 `errors`。
 *
 * 本模块只做 IO 编排，不含任何判定逻辑。
 */

import { gitExec, isGitRepo, readStoreSnapshotAtCommit, upstreamRef } from '../git/index.js';
import { loadState } from '../state.js';
import { readSnapshotFromStore } from '../store/store.js';
import { scanAdapter } from '../../adapters/pi/index.js';
import { isRootUnreadable, scanWarningMessages } from '../scan-guard.js';
import type { HomerPaths } from '../paths.js';
import type { AdapterSnapshot, HomerConfig } from '../types.js';
import type { SyncSources } from './types.js';

export interface CollectSyncSourcesOptions {
  /**
   * 是否执行 `git fetch`（默认 true）。
   * `false` = 跳过网络，直接读当前 `@{upstream}` 引用（测试 / 离线快路径）。
   */
  fetch?: boolean;
}

/** `@{upstream}` 的短名（如 `origin/main`）；无 upstream / 非仓库 → undefined。 */
function syncUpstreamRef(paths: HomerPaths): string | undefined {
  return upstreamRef(paths.home);
}

/** 把 ref 解析成 commit SHA；不可解析（无 upstream / 未 fetch / 非仓库）→ undefined。 */
function revParse(paths: HomerPaths, ref: string): string | undefined {
  const result = gitExec(paths.home, ['rev-parse', '--verify', `${ref}^{commit}`]);
  if (!result.ok) return undefined;
  const sha = result.stdout.trim();
  return sha === '' ? undefined : sha;
}

/** `commitish` 是否是 git 历史里真实可读的 commit（state 指向的对象可能被 GC / 克隆不全）。 */
function isReadableCommit(paths: HomerPaths, commitish: string): boolean {
  return gitExec(paths.home, ['cat-file', '-e', `${commitish}^{commit}`]).ok;
}

interface CollectedBase {
  base: AdapterSnapshot[];
  mode: SyncSources['mode'];
  baseCommit?: string;
}

/** base 采集：git 历史优先，回落到 store 工作区（§1 决策 D3 的 fallback 链）。 */
function collectBase(paths: HomerPaths, config: HomerConfig, warnings: string[]): CollectedBase {
  const commitish = loadState(paths).lastSyncCommit;

  if (commitish === undefined) {
    // 无 state（init 后未 push / M1 老工作区）→ M1 语义：base = store 工作区。
    return { base: readSnapshotFromStore(paths, config), mode: 'store' };
  }

  if (!isReadableCommit(paths, commitish)) {
    // commit 不可读 → 回落 store 工作区（绝不静默当「空 base」，那会让每个本地文件都变新增）。
    warnings.push(`state.lastSyncCommit=${commitish} 在 git 历史中不可读，base 回落到 store 工作区`);
    return { base: readSnapshotFromStore(paths, config), mode: 'store' };
  }

  return { base: readStoreSnapshotAtCommit(paths, config, commitish), mode: 'git', baseCommit: commitish };
}

/**
 * local 采集：逐 adapter 实时扫描；root 不可读 → 沿用 base 快照（M-A 守卫）。
 *
 * 错误文本由 `core/scan-guard.ts` 统一生成（`adapter root 不可读` / `扫描告警`），
 * 与 `homer status` 的 `sourceErrorMessages` 同一形状：`<prefix>: <id> (<path>: <msg>)`。
 * 就地实现而非 import `cli/render.js`：`core` 不得反向依赖 `cli`（同 CliError 拆到 core/errors.ts 的理由）。
 */
function collectLocal(
  config: HomerConfig,
  base: AdapterSnapshot[],
  errors: string[],
): AdapterSnapshot[] {
  const local: AdapterSnapshot[] = [];

  for (const [adapterId, adapterConfig] of Object.entries(config.adapters)) {
    if (adapterConfig.enabled === false) continue;

    const outcome = scanAdapter(adapterId, adapterConfig);

    // root 级失败判定（唯一实现点：core/scan-guard.ts 的 isRootUnreadable）：
    // 空分类 + 有错 = 整个 root 读不到（不存在 / 不是目录）。
    errors.push(...scanWarningMessages(adapterId, outcome));

    if (isRootUnreadable(outcome)) {
      const baseSnapshot = base.find((snapshot) => snapshot.adapterId === adapterId);
      // base 里必然有该 adapter（两路 base 都严格对齐 config）——防御性兜底：
      // 万一没有，宁可交回空快照也不能凭空造一个（那会变成假 pull-delete，由 errors 提示命令层拦截）。
      if (baseSnapshot !== undefined) {
        local.push(baseSnapshot);
        continue;
      }
    }

    local.push(outcome.snapshot);
  }

  return local;
}

/**
 * 采集 base / local / remote 三方原料。
 *
 * 调用方（命令层 W7/W8/W9）负责 pull / merge 前的 `isGitRepo` / `hasUpstream` /
 * `requireCleanStore` / `requireFastForwardable` 前置检查（§2.8 冻结顺序）。
 */
export function collectSyncSources(
  paths: HomerPaths,
  config: HomerConfig,
  opts?: CollectSyncSourcesOptions,
): SyncSources {
  const warnings: string[] = [];
  const errors: string[] = [];

  const { base, mode, baseCommit } = collectBase(paths, config, warnings);
  const local = collectLocal(config, base, errors);

  /** 组装结果：remote 有值 → 填 remoteRef；baseCommit 仅 git 模式填。 */
  const build = (remote: AdapterSnapshot[], remoteRef?: string): SyncSources => {
    const sources: SyncSources = { mode, base, local, remote, warnings, errors };
    if (baseCommit !== undefined) sources.baseCommit = baseCommit;
    if (remoteRef !== undefined) sources.remoteRef = remoteRef;
    return sources;
  };

  // ---- remote：fetch 后的 upstream（D4）；无 upstream / fetch 失败 → 降级为 base ----
  const upstream = syncUpstreamRef(paths);
  if (upstream === undefined) {
    warnings.push(
      isGitRepo(paths.home)
        ? '未配置 git upstream，remote 视作 = base（M1 语义）'
        : '工作区不是 git 仓库，remote 视作 = base（M1 语义）',
    );
    return build(base);
  }

  if (opts?.fetch !== false) {
    const fetched = gitExec(paths.home, ['fetch']);
    if (!fetched.ok) {
      // §1 降级矩阵：fetch 失败（离线）→ remote = base（远端变更不可见，但本地仍可 push）。
      const reason = fetched.stderr.trim() === '' ? '未知错误' : fetched.stderr.trim();
      warnings.push(`git fetch 失败（远端变更不可见，remote 视作 = base）: ${reason}`);
      return build(base, upstream);
    }
  }

  const remoteCommit = revParse(paths, upstream);
  if (remoteCommit === undefined) {
    warnings.push(`git upstream ${upstream} 不可解析，remote 视作 = base`);
    return build(base, upstream);
  }

  // 首次接入：upstream commit 里**根本没有 store/ 树**（远端还是一个不含同步内容的仓库，
  // 例如刚 clone 下只有 homer.json 的配置中心）→ 不能把「远端没有 store」当成
  // 「远端把每个文件都删了」（那会把首次 `homer push` 误拦成 remote-ahead，永远建不了基线）。
  // 此时按 §1 D3 的 fallback 语义（base = store 工作区「= M1 语义」）令 remote := base。
  // 告警只在 base 确实有内容时发（空 base 下「remote := base」无观察差异，不骚扰）。
  if (!hasStoreTree(paths, remoteCommit)) {
    if (base.some((snapshot) => snapshot.categories.some((category) => category.files.size > 0))) {
      warnings.push(`git upstream ${upstream} 中尚无 store 内容（首次同步？），remote 视作 = base（M1 语义）`);
    }
    return build(base, upstream);
  }

  return build(readStoreSnapshotAtCommit(paths, config, remoteCommit), upstream);
}

/** upstream commit 里是否存在 `store/` 树（= 是否已有同步内容）。 */
function hasStoreTree(paths: HomerPaths, commit: string): boolean {
  const result = gitExec(paths.home, ['ls-tree', '-r', '--name-only', '-z', commit, '--', 'store/']);
  return result.ok && result.stdout.trim() !== '';
}
