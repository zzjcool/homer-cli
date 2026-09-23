/**
 * `homer merge` —— 交互式冲突解决（docs/m2-plan.md §2.8）。
 *
 * 冻结流程（§2.8「merge 流程（冻结）」）：
 *   load config → 前置（isGitRepo / hasUpstream / requireCleanStore / gitFetch /
 *   requireFastForwardable）→ sources → `planPull` 取 conflict actions → 空 → `no-conflicts` exit 0
 *   → 裁决（`--accept-local` / `--accept-remote` 批量；否则逐项 `ui.select`，**无 skip**）
 *   → `requireCleanStore` + `mergeFfUpstream` → `state.lastSyncCommit = upstream HEAD`（base 前进）
 *   → 选 remote 的项备份后写入工具目录（构造 write-only 子 plan 走 `applyPullActions`）
 *   → 重扫 local → `checkPushSafety(base=remote, local, remote=remote)`（应为 ok）
 *   → 有 drift 则走 push 管线（store + commit + push）→ `state = 最终 HEAD` → exit 0。
 *
 * ## merge 完成语义（§1 关键不变式）
 *
 * 「任何 resolution 之后 `local == 意图真相`，下一轮 sync 三方一致」：
 *   - `mergeFfUpstream` 让 store 对齐远端，base 前进到 upstream HEAD（远端变更**已被看见**）；
 *   - 裁决把用户意图写进工具目录（本地保留自己的改动与本地密钥；选远端的项落远端内容）；
 *   - push 管线把这份意图发布到 store（+ 远端），于是下一轮三方判定的 base 与 store / local 自洽。
 *
 * ## 三个必须显式记录的实现裁定
 *
 * 1. **clean 动作一并应用**（避免静默丢远端数据）。`planPull` 除冲突外还给出「远端改动 / 本地未改动」
 *    的 write 与 delete 动作（远端新增文件、远端删除本地未改文件、merge 分类的无冲突键级合并……）。
 *    若只写「选 remote 的冲突项」而丢弃这些 clean 动作，接着的 push 管线会把**远端这些 clean 改动
 *    当作本地状态写回 store**（远端新增被回删、远端删除被复活）——即静默丢掉远端数据，与
 *    「远端变更已被看见」直接冲突。故子 plan = clean 动作 ∪ 裁决为 remote 的冲突项，一次
 *    `applyPullActions` 落盘（同一个 `<HHmmss>-merge` 备份目录）。用户自己的本地改动仍然保留
 *    （它们在 plan 里是 push 方向、不产生 clean 动作），所以 push 管线该触发时照常触发。
 *
 * 2. **merge 分类冲突的「接受远端」取远端快照内容 + 植回本地 excluded 键**。W4 的约定是 merge 分类
 *    conflict **不带** `localContent` / `remoteContent`（防密钥进 `--json`、防 store 的
 *    `__REQUIRED__` 占位符被照抄落盘），交互展示只用 `keyPaths`。命令层要落盘就得自己取内容：
 *    从 `sources.remote` 快照取该文件，`plantExcludedKeys` 把本地原文件的 excluded 键值植回；
 *    若植回后仍是 `__REQUIRED__`（本地缺该必填项）则**删掉该键 + warning**，绝不让占位符流进工具目录。
 *    mirror 冲突带全量内容（W4 的 `safeRemoteDisplay` 已保证其不含占位符）→ 直接采用。
 *
 * 3. **无 `--accept-*` 且非交互 → `aborted`**（exit 1，零写入）。§2.7 约定 2：非交互环境不能替用户
 *    默认同意破坏性操作，只能明确拒绝并要求显式 `--accept-*`。注入了 `deps.ui`（测试 / 编程调用）
 *    或处于 TTY 时走真正的逐项 `ui.select`（fallback = `local`，即「保留本地」——冻结语义里
 *    「无 skip，保留本地等价 accept-local」）。
 *
 * `--json` 输出（MergeReport）**只含 keyPaths / 路径 / 计数**，绝不含文件内容，故不会把密钥带出去。
 */

import process from 'node:process';

import { loadConfig } from '../../core/config.js';
import { CliError } from '../../core/errors.js';
import { resolveHomerPaths, splitLines } from '../render.js';
import { saveState } from '../../core/state.js';
import { writeSnapshotToStore } from '../../core/store/store.js';
import { scanAdapter } from '../../adapters/pi/index.js';
import {
  applyPullActions,
  checkPushSafety,
  collectSyncSources,
  excludedKeysFor,
  planPull,
  plantExcludedKeys,
  prepareStoreSnapshot,
  serializeJsonContent,
  REQUIRED_PLACEHOLDER,
  type ApplyResult,
  type PullAction,
  type PullConflictAction,
  type PullDeleteAction,
  type PullPlan,
  type PullWriteAction,
  type SyncSources,
} from '../../core/sync/index.js';
import { isJsonObject, parseJsonContent } from '../../core/sync/excluded-keys.js';
import { createDefaultPromptPort, type PromptPort } from '../ui.js';
import { resolveGitPort, type GitPort, type ResolvedGitPort } from './git-port.js';
import type { AdapterSnapshot, HomerConfig, SnapshotEntry } from '../../core/types.js';
import type { HomerPaths } from '../../core/paths.js';

export interface MergeOptions { homerHome?: string; json?: boolean; acceptLocal?: boolean; acceptRemote?: boolean; }
export interface MergeDeps { ui?: PromptPort; sources?: SyncSources; noFetch?: boolean; git?: GitPort; }
export interface MergeReport {
  ok: boolean;
  status: 'resolved' | 'no-conflicts' | 'aborted' | 'error';
  resolutions: { adapterId: string; category: string; relPath: string; keyPaths?: string[]; choice: 'local' | 'remote' }[];
  applied: ApplyResult;   // 选择 remote 的项写入工具目录（含备份）
  commit?: string;        // ff 后有新 commit 时
  warnings: string[];
  errors: string[];
}

export const MERGE_USAGE = `用法: homer merge [options]

逐项裁决本地与远端的冲突，并把裁决结果写回工具目录（选择远端时先备份到
<home>/backups/<date>/<time>-merge/），随后同步到 store（+ 远端）。

裁决选项（互斥，二选一）:
  --accept-local      批量裁决：全部保留本地（等价逐个选 Accept Local）
  --accept-remote     批量裁决：全部采用远端（等价逐个选 Accept Remote）

未给出裁决选项时逐项询问 Accept Local / Accept Remote（**没有 skip**：不表态等价于保留本地，
并会在随后的同步中覆盖远端）。非交互环境（无 TTY）下必须显式给出裁决选项，否则中止（零写入）。

其它选项:
  --home <dir>        homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --json              输出机器可读 JSON（MergeReport；只含路径 / 冲突键，不含文件内容）
  -h, --help          显示本帮助

语义: 先把 store 快进到 upstream（base 前进，远端变更被看见），再按裁决写工具目录，
然后重扫本地并把结果同步到 store（+ 远端）。仅处理冲突；无冲突时不做任何写入
（非冲突的远端变更请用 \`homer pull\` 应用）。

前置条件: 工作区是 git 仓库、已配置 upstream、store 干净且 HEAD 是 upstream 的祖先。
退出码: resolved / no-conflicts → 0；aborted / error → 1。`;

/* ------------------------------------------------------------------ */
/* 裁决选择                                                            */
/* ------------------------------------------------------------------ */

type Choice = 'local' | 'remote';

/** 执行阶段进度（供 `error` 报告如实说明「已 ff / base 已前进」）。 */
interface Progress {
  ffDone: boolean;
  stateAdvanced: boolean;
}

const LOCAL: Choice = 'local';
const REMOTE: Choice = 'remote';

/** 提示选项标签（冻结措辞：Accept Local / Accept Remote）。 */
const SELECT_OPTIONS: readonly { value: Choice; label: string }[] = [
  { value: LOCAL, label: 'Accept Local' },
  { value: REMOTE, label: 'Accept Remote' },
];

const REASON_TEXT: Record<PullConflictAction['reason'], string> = {
  'modify-vs-modify': '双方各自修改了这个文件',
  'local-delete-vs-remote-modify': '本地删除了这个文件，远端修改了它',
  'local-modify-vs-remote-delete': '本地修改了这个文件，远端删除了它',
  'merge-keys': '同一个配置键被双方改成了不同值',
};

interface Resolution {
  action: PullConflictAction;
  choice: Choice;
}

/** 冲突项的目标三元组文本。 */
function targetOf(action: { adapterId: string; category: string; relPath: string }): string {
  return `${action.adapterId}/${action.category}/${action.relPath}`;
}

/** 单条冲突的提示文本：路径 + 原因 + 冲突键（+ 可用内容的行数，绝不打印内容本身）。 */
function conflictPrompt(action: PullConflictAction): string {
  const keyPaths =
    action.keyPaths !== undefined && action.keyPaths.length > 0
      ? `（冲突键: ${action.keyPaths.join(', ')}）`
      : '';
  return `${targetOf(action)} — ${REASON_TEXT[action.reason]}${keyPaths}${contentSizes(action)}`;
}

/** 有全量内容时给出行数提示（mirror 冲突有；merge 分类冲突刻意没有内容）。 */
function contentSizes(action: PullConflictAction): string {
  if (action.localContent === undefined && action.remoteContent === undefined) return '';
  const local = action.localContent === undefined ? '（本地已删除）' : `${splitLines(action.localContent).length} 行`;
  const remote = action.remoteContent === undefined ? '（远端已删除）' : `${splitLines(action.remoteContent).length} 行`;
  return `（本地 ${local} / 远端 ${remote}）`;
}

/* ------------------------------------------------------------------ */
/* runMerge                                                           */
/* ------------------------------------------------------------------ */

/**
 * 冲突裁决 + 应用 + 同步。
 *
 * 前置检查失败 / 配置缺失 → `CliError`（分发层打印并 exit 1，**任何写入之前**）。
 * 裁决之后的执行阶段（ff / 应用 / push）抛错 → `status='error'`（exit 1），已发生的写入如实反映在
 * `applied` / `warnings` 里，不伪造成功。
 */
export async function runMerge(opts: MergeOptions, deps: MergeDeps = {}): Promise<MergeReport> {
  const paths = resolveHomerPaths(opts.homerHome);
  // git 端口（P4-W10 收敛为命令层共享类型；测试可注入，缺省走真实 `src/core/git`）。
  const git = resolveGitPort(deps.git);
  const config = loadConfig(paths);
  if (config === undefined) {
    throw new CliError(
      `未找到 ${paths.configFile}`,
      '请先运行 `homer init` 生成配置（merge 需要 homer.json 才能映射冲突文件）。',
    );
  }

  // ---- 前置（§2.8 冻结顺序；全部先于任何写操作）----
  requireMergePreconditions(paths, deps, git);

  if (opts.acceptLocal === true && opts.acceptRemote === true) {
    throw new CliError(
      '不能同时指定 --accept-local 与 --accept-remote',
      '二者语义相反，请只保留一个（或都不给以进入逐项裁决）。',
    );
  }

  const warnings: string[] = [];
  const errors: string[] = [];

  // ---- 三方原料（测试注入 deps.sources；fetch 已在前置阶段做过）----
  const sources = deps.sources ?? collectSyncSources(paths, config, { fetch: deps.noFetch !== true });
  warnings.push(...sources.warnings);
  errors.push(...sources.errors);

  // ---- 冲突清单（planPull 的三方判定，与 status / pull 同一实现点）----
  const plan = planPull(config, sources.base, sources.local, sources.remote);
  const conflicts = plan.actions.filter(isConflictAction);

  if (conflicts.length === 0) {
    return {
      ok: true,
      status: 'no-conflicts',
      resolutions: [],
      applied: emptyApplyResult(),
      warnings,
      errors,
    };
  }

  // ---- 裁决 ----
  const batch: Choice | undefined =
    opts.acceptRemote === true ? REMOTE : opts.acceptLocal === true ? LOCAL : undefined;

  let resolutions: Resolution[];
  if (batch !== undefined) {
    resolutions = conflicts.map((action) => ({ action, choice: batch }));
  } else {
    // 非交互环境不能替用户默认裁决破坏性操作（§2.7 约定 2）→ 中止并要求显式 --accept-*。
    const interactive = deps.ui !== undefined || process.stdout.isTTY === true;
    if (!interactive) {
      return {
        ok: false,
        status: 'aborted',
        resolutions: [],
        applied: emptyApplyResult(),
        warnings: [
          ...warnings,
          '非交互环境无法逐项裁决：请显式指定 --accept-local 或 --accept-remote（本次未做任何写入）',
        ],
        errors,
      };
    }

    const ui = deps.ui ?? createDefaultPromptPort();
    resolutions = [];
    for (const action of conflicts) {
      const answer = await ui.select(conflictPrompt(action), SELECT_OPTIONS, LOCAL);
      resolutions.push({ action, choice: answer === REMOTE ? REMOTE : LOCAL });
    }
  }

  const progress: Progress = { ffDone: false, stateAdvanced: false };

  try {
    return executeResolutions({ paths, config, sources, plan, resolutions, warnings, errors, progress, git });
  } catch (err) {
    errors.push(err instanceof Error ? err.message : String(err));
    if (progress.ffDone) {
      warnings.push(
        progress.stateAdvanced
          ? 'store 已快进到 upstream 且 base 已前进（远端变更已被看见），但工具目录写入 / 同步未完成；请重跑 `homer merge` 或 `homer pull` 收敛'
          : 'store 已快进到 upstream，但后续步骤未完成；请重跑 `homer merge` 或 `homer pull` 收敛',
      );
    }
    return {
      ok: false,
      status: 'error',
      resolutions: resolutions.map(toReportResolution),
      applied: emptyApplyResult(),
      warnings,
      errors,
    };
  }
}

/* ------------------------------------------------------------------ */
/* 前置检查                                                            */
/* ------------------------------------------------------------------ */

/** 冻结前置链：isGitRepo → hasUpstream → requireCleanStore → gitFetch → requireFastForwardable。 */
function requireMergePreconditions(paths: HomerPaths, deps: MergeDeps, git: ResolvedGitPort): void {
  if (!git.isGitRepo(paths.home)) {
    throw new CliError(
      `工作区不是 git 仓库: ${paths.home}`,
      'merge 需要 git 历史与 upstream；请先运行 `homer push` 建立。',
    );
  }

  if (!git.hasUpstream(paths.home)) {
    throw new CliError(
      '未配置 git upstream，无法确定远端',
      '请先 `git push -u <remote> <branch>`（或在 `~/.homer` 内 `git branch --set-upstream-to`）配置远端。',
    );
  }

  git.requireCleanStore(paths);

  if (deps.noFetch !== true) {
    const fetched = git.gitFetch(paths.home);
    if (!fetched.ok) {
      throw new CliError(
        `git fetch 失败: ${firstLine(fetched.stderr)}`,
        '远端状态不可确定，merge 已中止（未做任何写入）；请检查网络 / remote 后重试。',
      );
    }
  }

  git.requireFastForwardable(paths);
}

/* ------------------------------------------------------------------ */
/* 执行：ff → 应用裁决 → 重扫 → push 管线 → state                      */
/* ------------------------------------------------------------------ */

interface ExecuteInput {
  paths: HomerPaths;
  config: HomerConfig;
  sources: SyncSources;
  plan: PullPlan;
  resolutions: readonly Resolution[];
  warnings: string[];
  errors: string[];
  progress: Progress;
  git: ResolvedGitPort;
}

function executeResolutions(input: ExecuteInput): MergeReport {
  const { paths, config, sources, plan, resolutions, warnings, errors, progress, git } = input;

  // ---- 再次校验（裁决期间用户可能弄脏 store）+ ff：store 对齐远端、base 前进 ----
  git.requireCleanStore(paths);
  git.requireFastForwardable(paths);

  const ff = git.mergeFfUpstream(paths.home);
  if (!ff.ok) {
    throw new CliError(
      `git merge --ff-only @{upstream} 失败: ${firstLine(ff.stderr)}`,
      '未对工具目录做任何写入；请检查 git 状态后重试。',
    );
  }
  progress.ffDone = true;

  // base 前进：ff 之后立刻记 upstream HEAD，**先于**工具目录写入（§2.8 冻结顺序）——
  // 即使后续应用 / 同步失败，远端变更也已被「看见」，下一轮不会再被当成新冲突。
  const upstreamHead = git.headCommit(paths.home);
  if (upstreamHead !== undefined) {
    saveMergeState(paths, upstreamHead);
    progress.stateAdvanced = true;
  }

  // ---- 子 plan = clean 动作 ∪ 裁决为 remote 的冲突项（见文件头裁定 1 / 2）----
  const subPlan = buildResolutionPlan(plan, resolutions, config, sources, warnings);
  const applied = applyPullActions(paths, config, subPlan, { command: 'merge' });

  // ---- 重扫 local → 安全校验（base = remote）→ 有 drift 走 push 管线 ----
  const localAfter = rescanLocal(config, sources.remote, errors);
  const safety = checkPushSafety(config, sources.remote, localAfter, sources.remote);

  let commit: string | undefined;
  if (safety.status === 'ok' && safety.changedFiles.length > 0) {
    for (const snapshot of prepareStoreSnapshot(localAfter, config)) {
      writeSnapshotToStore(paths, snapshot);
    }

    commit = commitStoreIfNeededForMerge(paths, resolutions, warnings, git);
    if (commit !== undefined) {
      if (git.hasUpstream(paths.home)) {
        const pushed = git.gitPush(paths.home);
        if (!pushed.ok) {
          warnings.push(
            `git push 失败：本地合并提交已生成但未推送到远端（${firstLine(pushed.stderr)}）`,
          );
        }
      }
      saveMergeState(paths, commit);
    }
  } else if (safety.status !== 'ok') {
    // 防御：base = remote 时不该出现 remote-ahead / conflicts（local 的任何差异都只是 push 方向）。
    warnings.push(
      `合并后重扫仍检测到远端未见的变更（${safety.status}），请运行 \`homer status\` 复核`,
    );
  }

  return {
    ok: true,
    status: 'resolved',
    resolutions: resolutions.map(toReportResolution),
    applied,
    commit,
    warnings,
    errors,
  };
}

/** store 提交（`undefined` 有两种含义，这里显式区分，避免把「本来就干净」误报成失败）。 */
function commitStoreIfNeededForMerge(
  paths: HomerPaths,
  resolutions: readonly Resolution[],
  warnings: string[],
  git: ResolvedGitPort,
): string | undefined {
  if (git.isStoreClean(paths.home)) {
    // 重扫出的 drift 与实际 store 字节不一致（保守情况）：交回 warning 而不当成 commit 失败。
    warnings.push('重扫出的变更与 store 当前内容一致，无需新的提交');
    return undefined;
  }

  const remote = resolutions.filter((item) => item.choice === REMOTE).length;
  const message = `homer merge: resolve ${resolutions.length} conflict(s) (remote ${remote}, local ${resolutions.length - remote})`;
  const commit = git.commitStoreIfNeeded(paths, message);
  if (commit === undefined) {
    warnings.push(
      'store 已更新但未能生成 git commit（检查仓库的 user.name / user.email）；请手动提交或重跑 `homer push`',
    );
  }
  return commit;
}

/** state 原子写（base 前进 / 最终 HEAD 两处共用）。 */
function saveMergeState(paths: HomerPaths, commit: string): void {
  saveState(paths, {
    version: 1,
    lastSyncCommit: commit,
    lastSyncAt: new Date().toISOString(),
    lastSyncCommand: 'merge',
  });
}

/* ------------------------------------------------------------------ */
/* 子 plan 构造                                                        */
/* ------------------------------------------------------------------ */

/**
 * 裁决 → write-only（+ delete）子 plan：
 *   - `plan` 里的 clean 动作（远端改动 / 本地未改动的 write / delete）**全部保留**（裁定 1）；
 *   - 裁决为 remote 的冲突项 → 取远端内容落盘（裁定 2）；裁决为 local 的项不产生动作（保留本地现状）。
 */
function buildResolutionPlan(
  plan: PullPlan,
  resolutions: readonly Resolution[],
  config: HomerConfig,
  sources: SyncSources,
  warnings: string[],
): PullPlan {
  const actions: PullAction[] = plan.actions.filter((action) => action.type !== 'conflict');

  for (const { action, choice } of resolutions) {
    if (choice !== REMOTE) continue;
    const adopted = adoptRemoteAction(action, config, sources, warnings);
    if (adopted !== undefined) actions.push(adopted);
  }

  return { actions };
}

/** 单条冲突「接受远端」→ write / delete 动作。 */
function adoptRemoteAction(
  action: PullConflictAction,
  config: HomerConfig,
  sources: SyncSources,
  warnings: string[],
): PullWriteAction | PullDeleteAction | undefined {
  const remoteEntry = lookupEntry(sources.remote, action);

  if (remoteEntry === undefined) {
    // 远端已删除该文件（local-modify-vs-remote-delete）→ 接受远端 = 删除本地文件。
    return {
      type: 'delete',
      adapterId: action.adapterId,
      category: action.category,
      relPath: action.relPath,
    };
  }

  // 配了 excludeKeys 的 category（merge 模式、或罕见的 mirror + excludeKeys）**总是**从原始远端
  // 快照重新构造：远端 store 原文 + 植回本地 excluded 键 + 摘掉残留占位符。
  // 不走 `action.remoteContent` 捷径：即使在 W4 给出它的场合（剥离不改变远端内容），直接照抄也会
  // 丢掉本地文件里的 excluded 键值（“本地密钥永不被远端覆盖”这一不变式就破了）；
  // 反过来，W4 因内容含占位符而**不给** remoteContent 时，捷径退化到原始快照就会漏占位符。
  if (excludedKeysFor(config, action.adapterId, action.category).length > 0) {
    return {
      type: 'write',
      adapterId: action.adapterId,
      category: action.category,
      relPath: action.relPath,
      content: remoteContentForWrite(action, config, sources, remoteEntry, warnings),
    };
  }

  // 无 excludeKeys（mirror 常态）：W4 给出的显示用远端全文即剥离后（= 判定用）字节；
  // 未给出时（远端已删除由上面的分支处理，这里只剩防御）退回原始快照。
  return {
    type: 'write',
    adapterId: action.adapterId,
    category: action.category,
    relPath: action.relPath,
    content: action.remoteContent ?? remoteEntry.content,
  };
}

/**
 * merge 分类冲突的远端内容：远端 store 原文 + 植回本地 excluded 键（本地密钥永不被覆盖），
 * 并把「本地缺失但仍为 `__REQUIRED__`」的键删掉（占位符绝不流入工具目录）。
 * 非 JSON 对象（降级 / 损坏）→ 原样采用远端 content（mirror 语义下没什么可植回的）。
 */
function remoteContentForWrite(
  action: PullConflictAction,
  config: HomerConfig,
  sources: SyncSources,
  remoteEntry: SnapshotEntry,
  warnings: string[],
): string {
  const keys = excludedKeysFor(config, action.adapterId, action.category);
  if (keys.length === 0) return remoteEntry.content;

  const remoteValue = parseJsonContent(remoteEntry.content);
  if (!isJsonObject(remoteValue)) {
    // 降级文件（非 JSON 对象）：无键可植回 / 无占位符可摘，原样采用远端。
    return remoteEntry.content;
  }

  const localEntry = lookupEntry(sources.local, action);
  const localValue = localEntry === undefined ? undefined : parseJsonContent(localEntry.content);

  const planted = plantExcludedKeys(remoteValue, localValue, keys);
  for (const key of keys) {
    if (planted[key] !== REQUIRED_PLACEHOLDER) continue;
    delete planted[key];
    warnings.push(
      `${targetOf(action)} 的必填项 "${key}" 在 store 中为占位符且本地缺失，已从结果中移除；请重新填写后再 push`,
    );
  }
  return serializeJsonContent(planted);
}

/* ------------------------------------------------------------------ */
/* 重扫 / 查询工具                                                     */
/* ------------------------------------------------------------------ */

function isConflictAction(action: PullAction): action is PullConflictAction {
  return action.type === 'conflict';
}

/** 三方快照里取单个文件条目（adapter / category / relPath）。 */
function lookupEntry(
  snapshots: readonly AdapterSnapshot[],
  ref: { adapterId: string; category: string; relPath: string },
): SnapshotEntry | undefined {
  const adapter = snapshots.find((snapshot) => snapshot.adapterId === ref.adapterId);
  const category = adapter?.categories.find((item) => item.category === ref.category);
  return category?.files.get(ref.relPath);
}

/**
 * 应用裁决后重扫工具目录（§2.8「重扫 local」）。
 *
 * 与 `collectSyncSources` 的 local 采集同口径（base.ts 的 collectLocal）：root 不可读
 * （空分类 + 有错）→ `local := remote`（M-A 守卫，防全量假删除），原因记入 `errors`。
 */
function rescanLocal(
  config: HomerConfig,
  fallback: readonly AdapterSnapshot[],
  errors: string[],
): AdapterSnapshot[] {
  const local: AdapterSnapshot[] = [];

  for (const [adapterId, adapterConfig] of Object.entries(config.adapters)) {
    if (adapterConfig.enabled === false) continue;

    const outcome = scanAdapter(adapterId, adapterConfig);
    const rootUnreadable = outcome.snapshot.categories.length === 0 && outcome.errors.length > 0;
    const prefix = rootUnreadable ? 'adapter root 不可读' : '扫描告警';
    for (const error of outcome.errors) {
      errors.push(`${prefix}: ${adapterId} (${error.path}: ${error.message})`);
    }

    if (rootUnreadable) {
      const fallbackSnapshot = fallback.find((snapshot) => snapshot.adapterId === adapterId);
      if (fallbackSnapshot !== undefined) {
        local.push(fallbackSnapshot);
        continue;
      }
    }

    local.push(outcome.snapshot);
  }

  return local;
}

function toReportResolution({ action, choice }: Resolution): MergeReport['resolutions'][number] {
  const out: MergeReport['resolutions'][number] = {
    adapterId: action.adapterId,
    category: action.category,
    relPath: action.relPath,
    choice,
  };
  if (action.keyPaths !== undefined && action.keyPaths.length > 0) out.keyPaths = [...action.keyPaths];
  return out;
}

function emptyApplyResult(): ApplyResult {
  return { written: [], deleted: [], conflicts: [] };
}

/** stderr 的第一行非空内容（多行 git 输出压缩成一条可读提示）。 */
function firstLine(text: string): string {
  const line = text.split('\n').map((item) => item.trim()).find((item) => item !== '');
  return line ?? '未知错误';
}

/**
 * 人类可读渲染（父级只调用本函数，渲染逻辑留在命令文件内）。
 * 只打印路径 / 冲突键 / 计数 —— 绝不打印文件内容（避免密钥进日志）。
 */
export function renderMergeReport(report: MergeReport): string {
  const lines: string[] = [`homer merge: ${report.status}`];

  for (const resolution of report.resolutions) {
    const keys =
      resolution.keyPaths !== undefined && resolution.keyPaths.length > 0
        ? ` [${resolution.keyPaths.join(', ')}]`
        : '';
    lines.push(
      `  ${resolution.choice === 'remote' ? '→ 采用远端' : '← 保留本地'}  ${targetOf(resolution)}${keys}`,
    );
  }

  lines.push(`  写入: ${report.applied.written.length}  删除: ${report.applied.deleted.length}`);
  if (report.applied.backupDir !== undefined) lines.push(`  备份目录: ${report.applied.backupDir}`);
  if (report.commit !== undefined) lines.push(`  同步提交: ${report.commit}`);

  if (report.status === 'no-conflicts') {
    lines.push('  无冲突：非冲突的远端变更请用 `homer pull` 应用（merge 只处理冲突）。');
  }
  if (report.status === 'aborted') {
    lines.push('  本次未做任何写入；请显式指定 --accept-local / --accept-remote，或在 TTY 下逐项裁决。');
  }

  for (const warning of report.warnings) lines.push(`  ⚠ ${warning}`);
  for (const error of report.errors) lines.push(`  ✗ ${error}`);
  return lines.join('\n');
}
