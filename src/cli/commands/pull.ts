/**
 * `homer pull` —— git fetch → 三路判定 → 预览/确认 → 备份 → 应用到工具目录（docs/m2-plan.md §2.8）。
 *
 * P0 落地接口（冻结）：PullOptions / PullDeps / PullReport / PULL_USAGE / runPull / renderPullReport。
 * P3-W8 在本文件内完成实现与渲染（分发层 index.ts 不再改动）。
 *
 * ## 冻结流程（§2.8）
 *
 * ```
 * load config
 *   → 前置检查（全部先于任何写操作）：isGitRepo → hasUpstream → requireCleanStore
 *                                    → gitFetch → requireFastForwardable
 *   → collectSyncSources（base = state.lastSyncCommit，无 state → store 工作区）
 *   → planPull
 *   → 无动作 → no-drift（exit 0）
 *   → 预览（计数 + diffLines 行级预览，每文件截断前 20 行）
 *   → 非 --yes → confirm；拒绝 → aborted（exit 1）
 *   → applyPullActions(backup=true) → mergeFfUpstream → state = 新 HEAD（lastSyncCommand='pull'）
 *   → 残留 conflicts → conflicts-remain（exit 1，提示 homer merge）；否则 applied（exit 0）
 * ```
 *
 * 关键不变式：
 *   - **任何检查都先于任何写操作**：ff 失败后再回滚已写入的工具目录文件是做不到的
 *     （用户可能已改过其中一些），故宁可拒绝整个 pull。
 *   - `--yes` 时冲突默认策略 = **保留本地 + 标红列出**（DESIGN §2.8）；冲突项由
 *     `applyPullActions` 跳过写入，`homer merge` 再逐项裁决。
 *   - 一旦进入应用阶段，就按 `backup.keep`（默认 7）裁剪备份日期目录（§2.3 / e2e 第 8 组）。
 *
 * ## 测试注入（additive 可选字段，不改冻结形状）
 *
 *   - `deps.sources`：注入三方快照，跳过真实 store / git reader / 工具目录扫描；
 *   - `deps.ui`：注入 PromptPort（非交互 / 记录调用）；
 *   - `deps.git`：注入 git 端口（默认走真实 `src/core/git`），用于单测隔离；
 *   - `deps.noFetch`：跳过 `git fetch`（离线快路径 / 测试）；
 *   - `deps.noApply`：跳过应用阶段（不写工具目录、不 ff、不更新 state），只产出预览与冲突清单。
 */

import process from 'node:process';

import { CliError } from '../../core/errors.js';
import { loadConfig } from '../../core/config.js';
import { pruneBackups } from '../../core/backup/backup.js';
import { saveState } from '../../core/state.js';
import * as gitCore from '../../core/git/index.js';
import type { GitExecResult } from '../../core/git/index.js';
import { requireCleanStore, requireFastForwardable } from '../../core/sync/pipeline.js';
import { applyPullActions } from '../../core/sync/apply.js';
import { planPull } from '../../core/sync/plan.js';
import { collectSyncSources } from '../../core/sync/base.js';
import type {
  ApplyResult,
  PullAction,
  PullConflictAction,
  PullDeleteAction,
  PullPlan,
  PullWriteAction,
  SyncSources,
} from '../../core/sync/types.js';
import type { AdapterSnapshot } from '../../core/types.js';
import type { HomerPaths } from '../../core/paths.js';
import { diffLines, resolveHomerPaths } from '../render.js';
import { createDefaultPromptPort, type PromptPort } from '../ui.js';

export interface PullOptions { homerHome?: string; json?: boolean; yes?: boolean; }
export interface PullDeps {
  ui?: PromptPort;
  sources?: SyncSources;
  noFetch?: boolean;
  noApply?: boolean;
  /** git 端口注入（additive 可选字段，同 W7-push 做法；缺省走真实 `src/core/git`）。 */
  git?: PullGitPort;
}

/**
 * git 端口（additive，`Deps` 的可选注入位，与 W7-push 同款做法）。
 *
 * 每一项都可单独覆盖；未覆盖的走 `src/core/git` / `src/core/sync/pipeline` 的真实实现。
 * 存在的意义：单测可以在**零 git 仓库**的前提下验证命令层的前置检查顺序与降级分支。
 */
export interface PullGitPort {
  isGitRepo?: (home: string) => boolean;
  hasUpstream?: (home: string) => boolean;
  requireCleanStore?: (paths: HomerPaths) => void;
  gitFetch?: (home: string) => GitExecResult;
  requireFastForwardable?: (paths: HomerPaths) => void;
  mergeFfUpstream?: (home: string) => GitExecResult;
  headCommit?: (home: string) => string | undefined;
}

export interface PullReport {
  ok: boolean;   // true = 应用完成且无残留冲突
  status: 'applied' | 'no-drift' | 'aborted' | 'conflicts-remain' | 'error';
  applied: ApplyResult;
  conflicts: PullConflictAction[];   // 残留冲突（已保留本地）
  commit?: string;
  warnings: string[];
  errors: string[];
}

export const PULL_USAGE = `用法: homer pull [options]

从 git upstream 拉取远端快照，三路判定后应用到本机工具目录。
应用前会把受影响的现有文件备份到 <home>/backups/<date>/。

选项:
  --home <dir>       homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --yes              跳过交互确认（非 TTY 环境必须显式给出）；
                     有冲突时默认策略 = 保留本地并标红列出
  --json             输出机器可读 JSON（PullReport）
  -h, --help         显示本帮助

前置条件: 工作区是 git 仓库、已配置 upstream、store 干净且 HEAD 是 upstream 的祖先。
退出码: applied / no-drift → 0；aborted / conflicts-remain / error → 1。`;

/* ------------------------------------------------------------------ */
/* 内部小工具                                                          */
/* ------------------------------------------------------------------ */

/** 空应用结果（no-drift / aborted / noApply 时报告里仍然是合法形状）。 */
function emptyApplyResult(): ApplyResult {
  return { written: [], deleted: [], conflicts: [] };
}

/** 若上游 `home` 不是 git 仓库根的提示（与 pipeline 的措辞保持一致）。 */
const NOT_A_REPO_HINT = '请先运行 `homer push` 建立 git 历史与 remote。';
const NO_UPSTREAM_HINT =
  '请先 `git push -u <remote> <branch>`（或在 `~/.homer` 内 `git branch --set-upstream-to`）配置远端。';

/** 把注入的 git 端口与真实实现合并（未覆盖项走真实实现）。 */
function resolveGitPort(port: PullGitPort | undefined): Required<PullGitPort> {
  return {
    isGitRepo: port?.isGitRepo ?? gitCore.isGitRepo,
    hasUpstream: port?.hasUpstream ?? gitCore.hasUpstream,
    requireCleanStore: port?.requireCleanStore ?? requireCleanStore,
    gitFetch: port?.gitFetch ?? ((home: string) => gitCore.gitFetch(home)),
    requireFastForwardable: port?.requireFastForwardable ?? requireFastForwardable,
    mergeFfUpstream: port?.mergeFfUpstream ?? ((home: string) => gitCore.mergeFfUpstream(home)),
    headCommit: port?.headCommit ?? gitCore.headCommit,
  };
}

function isConflict(action: PullAction): action is PullConflictAction {
  return action.type === 'conflict';
}

function isWrite(action: PullAction): action is PullWriteAction {
  return action.type === 'write';
}

function isDelete(action: PullAction): action is PullDeleteAction {
  return action.type === 'delete';
}

/** store 相对形式的动作目标（`pi/skills/alpha/SKILL.md`）。 */
function targetOf(action: PullAction): string {
  return `${action.adapterId}/${action.category}/${action.relPath}`;
}

/** 从快照里取某文件的原始内容；不存在 → undefined。 */
function contentOf(
  snapshots: readonly AdapterSnapshot[],
  adapterId: string,
  category: string,
  relPath: string,
): string | undefined {
  const snapshot = snapshots.find((item) => item.adapterId === adapterId);
  const categorySnapshot = snapshot?.categories.find((item) => item.category === category);
  return categorySnapshot?.files.get(relPath)?.content;
}

/* ------------------------------------------------------------------ */
/* 预览                                                                */
/* ------------------------------------------------------------------ */

/** 每个文件最多展示的 diff 行数（§2.8：预览「每文件截断前 20 行」）。 */
export const PREVIEW_MAX_LINES = 20;

/**
 * 参与逐行 diff 的输入上限（行数）。`diffLines` 是 O(n·m) 的 LCS，两侧都上万行时
 * 内存/耗时都会失控（预览本就只展示 20 行）。超过这个规模的文件只报规模，不做逐行 diff。
 */
export const PREVIEW_DIFF_MAX_INPUT_LINES = 2000;

/** 截断到前 `PREVIEW_MAX_LINES` 行，并追加省略提示。 */
function truncateLines(lines: readonly string[]): string[] {
  if (lines.length <= PREVIEW_MAX_LINES) return [...lines];
  const kept = lines.slice(0, PREVIEW_MAX_LINES);
  kept.push(`… 省略 ${lines.length - PREVIEW_MAX_LINES} 行`);
  return kept;
}

/** 单个文件的 diff 预览行（超大文件跳过逐行 diff，避免 O(n·m) 的 LCS 拖垮 CLI）。 */
function previewDiff(before: string, after: string): string[] {
  const beforeLines = splitLinesCount(before);
  const afterLines = splitLinesCount(after);
  if (beforeLines > PREVIEW_DIFF_MAX_INPUT_LINES || afterLines > PREVIEW_DIFF_MAX_INPUT_LINES) {
    return [`… 文件过大（${beforeLines} → ${afterLines} 行），跳过逐行预览`];
  }
  return truncateLines(diffLines(before, after));
}

/** 行数（末尾单个换行不产生空行，与 diffLines 的 splitLines 同口径）。 */
function splitLinesCount(text: string): number {
  if (text === '') return 0;
  const lines = text.split('\n');
  return lines[lines.length - 1] === '' ? lines.length - 1 : lines.length;
}

/** diff 行缩进到文件标题之下。 */
function indentLines(lines: readonly string[]): string[] {
  return lines.map((line) => `    ${line}`);
}

/**
 * 构造 pull 预览文本（纯函数：只读入参，不碰磁盘 / stdout）。
 *
 * 行级 diff 走 `cli/render.ts` 的 `diffLines`（LCS，`-` = 本地现状、`+` = 将要写入）：
 *   - write  → `diffLines(local 现状, remote 内容)`；
 *   - delete → `diffLines(local 现状, '')`（全文 `-`）；
 *   - conflict → `diffLines(localContent, remoteContent)`（供用户判断裁决方向）。
 * 每个文件最多 20 行 diff，其余以 `… 省略 N 行` 收尾。
 */
export function buildPullPreview(sources: SyncSources, plan: PullPlan): string {
  const writes = plan.actions.filter(isWrite);
  const deletes = plan.actions.filter(isDelete);
  const conflicts = plan.actions.filter(isConflict);

  const lines: string[] = [
    `预览: 写入 ${writes.length}  删除 ${deletes.length}  冲突 ${conflicts.length}`,
  ];

  for (const action of writes) {
    lines.push(`  写入 ${targetOf(action)}`);
    const before = contentOf(sources.local, action.adapterId, action.category, action.relPath) ?? '';
    lines.push(...indentLines(previewDiff(before, action.content)));
  }

  for (const action of deletes) {
    lines.push(`  删除 ${targetOf(action)}`);
    const before = contentOf(sources.local, action.adapterId, action.category, action.relPath) ?? '';
    lines.push(...indentLines(previewDiff(before, '')));
  }

  for (const action of conflicts) {
    lines.push(`  冲突 ${targetOf(action)} (${action.reason})`);
    if (action.keyPaths !== undefined && action.keyPaths.length > 0) {
      lines.push(`    冲突键: ${action.keyPaths.join(', ')}`);
    }
    const local =
      action.localContent ?? contentOf(sources.local, action.adapterId, action.category, action.relPath) ?? '';
    const remote =
      action.remoteContent ?? contentOf(sources.remote, action.adapterId, action.category, action.relPath) ?? '';
    lines.push(...indentLines(previewDiff(local, remote)));
  }

  return lines.join('\n');
}

/* ------------------------------------------------------------------ */
/* runPull                                                             */
/* ------------------------------------------------------------------ */

/**
 * 拉取并应用（§2.8 冻结流程）。
 *
 * 前置检查失败一律抛 `CliError`（分发层打印后 exit 1）；应用阶段返回结构化报告，
 * 由 `index.ts` 按 `status` 映射退出码（applied / no-drift → 0，其余 → 1）。
 */
export async function runPull(opts: PullOptions, deps?: PullDeps): Promise<PullReport> {
  const paths = resolveHomerPaths(opts.homerHome);
  const config = loadConfig(paths);
  if (config === undefined) {
    throw new CliError(
      `未找到 ${paths.configFile}`,
      '请先运行 `homer init` 生成 homer.json 与 store 快照。',
    );
  }

  const git = resolveGitPort(deps?.git);

  // ---- 前置检查（§2.8 冻结顺序：全部先于任何写操作）----
  if (!git.isGitRepo(paths.home)) {
    throw new CliError(`工作区不是 git 仓库: ${paths.home}`, NOT_A_REPO_HINT);
  }
  if (!git.hasUpstream(paths.home)) {
    throw new CliError('未配置 git upstream，无法确定远端', NO_UPSTREAM_HINT);
  }
  git.requireCleanStore(paths);

  if (deps?.noFetch !== true) {
    const fetched = git.gitFetch(paths.home);
    if (!fetched.ok) {
      // §1 降级矩阵：pull 的 fetch 失败不降级（远端不可见就无法判定），直接报错。
      const reason = fetched.stderr.trim() === '' ? '未知错误' : fetched.stderr.trim();
      throw new CliError(`git fetch 失败: ${reason}`, '请检查网络与 remote 配置后重试。');
    }
  }

  git.requireFastForwardable(paths);

  // ---- 三方原料 → 动作计划 ----
  // 上面已 fetch 过，故这里不再重复拉网络（fetch:false 只读 @{upstream} 引用）。
  const sources = deps?.sources ?? collectSyncSources(paths, config, { fetch: false });

  const warnings: string[] = [...sources.warnings];
  // sources.errors 是「扫不到某个 adapter root」这类采集期问题（local 已按 M-A 守卫回落到 base，
  // 不会造成假删除），但仍然是要让用户看见的异常 → 进报告的 errors。
  const sourceErrors: string[] = [...sources.errors];
  const plan = planPull(config, sources.base, sources.local, sources.remote);

  if (plan.actions.length === 0) {
    return {
      ok: true,
      status: 'no-drift',
      applied: emptyApplyResult(),
      conflicts: [],
      warnings,
      errors: sourceErrors,
    };
  }

  // ---- 预览 + 确认 ----
  const preview = buildPullPreview(sources, plan);

  if (opts.yes !== true) {
    // §2.7 约定 1：--yes 时完全不创建 port。非 TTY 下 port 的 confirm 返回 fallback=false → abort。
    const port = deps?.ui ?? createDefaultPromptPort();
    const confirmed = await port.confirm(
      `${preview}\n\n以上变更将应用到本机工具目录（受影响文件会先备份）。是否继续？`,
      false,
    );
    if (!confirmed) {
      if (process.stdout.isTTY !== true) {
        warnings.push('非交互环境无法确认，已中止；如需自动应用请加 --yes');
      }
      return {
        ok: false,
        status: 'aborted',
        applied: emptyApplyResult(),
        conflicts: [],
        warnings,
        errors: sourceErrors,
      };
    }
  }

  // ---- 应用 → ff → state ----
  const noApply = deps?.noApply === true;
  let applied = emptyApplyResult();
  let commit: string | undefined;

  if (!noApply) {
    // conflict 项被 applyPullActions 跳过（保留本地），只记入 applied.conflicts。
    applied = applyPullActions(paths, config, plan, { backup: true });

    // 备份保留策略（§2.3：按日期保留最近 N 个，默认 7）。
    // 与「写操作」绑定：走到这一步就说明本轮真的应用过（否则 no-drift / aborted 早已返回），
    // 故即使本轮无可备份文件（全新增 / 全冲突）也照常裁剪，避免 backups/ 无限增长。
    pruneBackups(paths, config.backup?.keep);

    const ff = git.mergeFfUpstream(paths.home);
    if (!ff.ok) {
      // ff 本应被 requireFastForwardable 挡住（并发 fetch 等边界）。
      // 工具目录已写入但 store 未前移 → 报告 error（ok=false，exit 1），绝不假装成功。
      const reason = ff.stderr.trim() === '' ? '未知错误' : ff.stderr.trim();
      return {
        ok: false,
        status: 'error',
        applied,
        conflicts: plan.actions.filter(isConflict),
        warnings,
        errors: [...sourceErrors, `git merge --ff-only 失败（工具目录已应用，store 未前移）: ${reason}`],
      };
    }

    commit = git.headCommit(paths.home);
    if (commit === undefined) {
      warnings.push('ff 后无法解析 HEAD，state.lastSyncCommit 未更新');
    } else {
      // 远端变更已被看见：base 前移到新 HEAD（§1 D6）。
      saveState(paths, {
        version: 1,
        lastSyncCommit: commit,
        lastSyncAt: new Date().toISOString(),
        lastSyncCommand: 'pull',
      });
    }
  }

  const conflicts = plan.actions.filter(isConflict);
  const errors: string[] = [...sourceErrors];
  if (conflicts.length > 0) {
    errors.push(`检测到 ${conflicts.length} 个冲突（已保留本地）：请运行 \`homer merge\` 逐项裁决。`);
  }

  return {
    ok: conflicts.length === 0,
    status: conflicts.length === 0 ? 'applied' : 'conflicts-remain',
    applied,
    conflicts,
    commit,
    warnings,
    errors,
  };
}

/* ------------------------------------------------------------------ */
/* 渲染                                                                */
/* ------------------------------------------------------------------ */

const ANSI_RED = '\u001b[31m';
const ANSI_RESET = '\u001b[0m';

export interface RenderPullOptions {
  /**
   * 是否用 ANSI 红色标出冲突（DESIGN §2.8「`--yes` 时冲突默认策略 = 保留本地 + 标红列出」）。
   * 缺省：TTY 下开，管道 / CI / 测试里关（避免把转义码写进日志）。
   */
  color?: boolean;
}

function paint(text: string, color: boolean): string {
  return color ? `${ANSI_RED}${text}${ANSI_RESET}` : text;
}

/**
 * 人类可读渲染。
 *
 * 冲突行（含 `conflicts-remain` 头行）按需标红；预览由 `runPull` 在交互确认时展示，
 * 报告本身只汇总结果与计数。
 */
export function renderPullReport(report: PullReport, opts?: RenderPullOptions): string {
  const color = opts?.color ?? process.stdout.isTTY === true;

  const lines: string[] = [
    report.status === 'conflicts-remain'
      ? paint(`homer pull: ${report.status}`, color)
      : `homer pull: ${report.status}`,
  ];
  lines.push(`  写入: ${report.applied.written.length}  删除: ${report.applied.deleted.length}  冲突: ${report.conflicts.length}`);
  if (report.applied.backupDir !== undefined) lines.push(`  备份目录: ${report.applied.backupDir}`);
  if (report.commit !== undefined) lines.push(`  同步提交: ${report.commit}`);
  for (const conflict of report.conflicts) {
    lines.push(paint(`  ⚡ 冲突: ${conflict.adapterId}/${conflict.category}/${conflict.relPath} (${conflict.reason})`, color));
  }
  for (const warning of report.warnings) lines.push(`  ⚠ ${warning}`);
  for (const error of report.errors) lines.push(paint(`  ✗ ${error}`, color));
  return lines.join('\n');
}
