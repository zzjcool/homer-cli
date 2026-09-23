/**
 * `homer push` —— 本地快照 → 密钥扫描 → store → git commit(+push) → state（docs/m2-plan.md §2.8）。
 *
 * P0 落地冻结接口（`PushOptions` / `PushDeps` / `PushReport` / `PUSH_USAGE`），
 * P3-W7 在本文件内完成实现 + 渲染（`renderPushReport` 是 P0 预置的钩子，
 * 故 W7 无需改 index.ts / args.ts / render.ts）。
 *
 * ## 冻结流程（§2.8 原文顺序，逐条落地）
 *
 *   1. load config（缺 homer.json → `status='error'`，提示 `homer init`）
 *   2. sources：`collectSyncSources`（fetch 失败 → warnings + remote := base）；
 *      测试注入 `deps.sources`（手工三方快照），生产走真实采集
 *   3. `scanSnapshots(prepareStoreSnapshot(local))` 密钥扫描 —— **扫的是将要写入 store 的字节**
 *      （excludeKeys 已换成 `__REQUIRED__` 占位符），再过 `filterIgnored(secrets.ignorePaths)` 豁免
 *   4. 命中 → `secrets-rejected` exit 1（store 未写入、无新 commit、不推远端）；
 *      `errors` 给一句话结论，逐条 `path:line [pattern] 脱敏摘录` 由 renderPushReport 从 `secrets` 打印
 *   5. `checkPushSafety`：`remote-ahead` → exit 1（提示 `homer pull`）；
 *      `conflicts` → exit 1（提示 `homer merge`）
 *   6. `changedFiles` 空 → `no-drift` exit 0（**先于**交互确认：无变更不必打扰用户）
 *   7. 非 `--yes` → port.confirm 预览（变更文件数汇总）；拒绝 → `aborted` exit 1
 *   8. `writeSnapshotToStore(prepareStoreSnapshot(local))`（逐 adapter）
 *   9. `ensureGitRepo` + `commitStoreIfNeeded` → state 更新
 *      （`lastSyncCommit` = 新 HEAD / `lastSyncAt` / `lastSyncCommand='push'`）
 *  10. 有 upstream 且非 `--no-push` → `gitPush`；失败 → warning + exit 1
 *      「本地 commit 已成功，远端推送失败」（`status='error'`，本地提交与 state 保留）
 *
 * **push 不写工具目录、无需备份**（store 本身在 git 版本化）。
 *
 * ## 退出码（§2.8 总表，由 index.ts 按 status 映射）
 *
 * `pushed` / `no-drift` → 0；其余（secrets-rejected / remote-ahead / conflicts / aborted / error）→ 1。
 *
 * ## 报告字段的归口（`PushReport` 形状冻结，无 dedicated 字段时的落点）
 *
 *   - `secrets`        —— 命中明细（含脱敏摘录），render 逐条打印
 *   - `warnings`       —— 非致命：`--no-push`、无 upstream（local-only）、
 *                         `sources.errors`（root 不可读等采集告警：push 仍可继续，故不是 error）、
 *                         远端会覆盖的文件清单、`store 与 HEAD 一致` 等
 *   - `errors`         —— 致命结论（含「下一步该跑什么」的提示：`homer pull` / `homer merge` / `--yes`）
 *
 * 非 `CliError` 的异常（编程错误）**不**吞：照旧冒泡给分发层统一处理。
 */

import process from 'node:process';

import { loadConfig } from '../../core/config.js';
import { CliError } from '../../core/errors.js';
import { filterIgnored, scanSnapshots } from '../../core/secrets/index.js';
import { loadState, saveState } from '../../core/state.js';
import { writeSnapshotToStore } from '../../core/store/store.js';
import { collectSyncSources, checkPushSafety, prepareStoreSnapshot } from '../../core/sync/index.js';
import type { PullConflictAction } from '../../core/sync/types.js';
import type { SecretFinding } from '../../core/secrets/types.js';
import type { SyncSources } from '../../core/sync/types.js';
import type { PromptPort } from '../ui.js';
import { createDefaultPromptPort } from '../ui.js';
import { resolveHomerPaths } from '../render.js';
import { resolveGitPort, type GitPort } from './git-port.js';

export interface PushOptions { homerHome?: string; json?: boolean; yes?: boolean; noPush?: boolean; }
export interface PushDeps { ui?: PromptPort; sources?: SyncSources; git?: GitPort; }
export interface PushReport {
  ok: boolean;
  status: 'pushed' | 'no-drift' | 'secrets-rejected' | 'remote-ahead' | 'conflicts' | 'aborted' | 'error';
  secrets: SecretFinding[];
  changedFiles: { adapterId: string; category: string; relPath: string }[];
  commit?: string;
  pushedToRemote: boolean;
  warnings: string[];
  errors: string[];
}

export const PUSH_USAGE = `用法: homer push [options]

把本地快照推送到 store 并提交到 git 仓库（有 upstream 时一并推送远端）。
推送前强制执行密钥扫描，命中即拒推（exit 1）。

选项:
  --home <dir>       homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --yes              跳过交互确认（非 TTY 环境必须显式给出）
  --no-push          只本地 commit，不推送远端
  --json             输出机器可读 JSON（PushReport）
  -h, --help         显示本帮助

退出码: pushed / no-drift → 0；其余（密钥命中 / remote-ahead / conflicts / aborted / error）→ 1。`;

/** 非交互环境确认失败时的提示（§2.7 约定 2 的固定措辞）。 */
const NON_INTERACTIVE_HINT = '非交互环境，请加 --yes';

/** `文件引用` 的规范文本（store 相对路径，§1.6 布局同构）。 */
export function fileRef(file: { adapterId: string; category: string; relPath: string }): string {
  return `${file.adapterId}/${file.category}/${file.relPath}`;
}

/** 同步 commit 的提交信息（`git log --oneline` 可读，见 §5 人工核验项）。 */
export function pushCommitMessage(changedCount: number): string {
  return changedCount === 0
    ? 'homer push: 建立同步基线（store 首次入库）'
    : `homer push: 同步 ${changedCount} 个变更文件`;
}

/** 统一构造报告（字段齐全，避免各出口漏字段）。 */
function makeReport(
  status: PushReport['status'],
  patch: Partial<PushReport> = {},
): PushReport {
  return {
    ok: status === 'pushed' || status === 'no-drift',
    status,
    secrets: [],
    changedFiles: [],
    pushedToRemote: false,
    warnings: [],
    errors: [],
    ...patch,
  };
}

/**
 * 推送。
 *
 * 只把 `CliError` 收敛为 `status='error'` 报告（用户级问题 → exit 1 + 可解析的 `--json`），
 * 其它异常照旧抛给分发层（编程错误不应被伪装成同步结果）。
 */
export async function runPush(opts: PushOptions, deps?: PushDeps): Promise<PushReport> {
  try {
    return await pushPipeline(opts, deps);
  } catch (err) {
    if (err instanceof CliError) {
      const errors = [err.message];
      if (err.hint !== undefined) errors.push(err.hint);
      return makeReport('error', { errors });
    }
    throw err;
  }
}

async function pushPipeline(opts: PushOptions, deps?: PushDeps): Promise<PushReport> {
  const paths = resolveHomerPaths(opts.homerHome);
  // git 端口（P4-W10 收敛为命令层共享类型；测试可注入，缺省走真实 `src/core/git`）。
  const git = resolveGitPort(deps?.git);
  const config = loadConfig(paths);
  if (config === undefined) {
    throw new CliError(
      `未找到 homer 配置: ${paths.configFile}`,
      '请先运行 `homer init` 生成 homer.json 与 store 快照。',
    );
  }

  // ---- 1. 三方原料 ---------------------------------------------------------
  // 注入优先（测试：手工快照，绝不碰真实 ~/.pi / remote）；生产走 collectSyncSources
  // （内部对 fetch 失败降级为 warnings + remote := base，§1 降级矩阵）。
  const sources = deps?.sources ?? collectSyncSources(paths, config);
  const warnings = [...sources.warnings];
  // 采集告警（root 不可读 → local := base 等）不阻断 push（降级语义），故落 warnings。
  for (const error of sources.errors) warnings.push(error);

  // ---- 2. 密钥扫描（扫「将要写入 store 的字节」）-----------------------------
  const prepared = prepareStoreSnapshot(sources.local, config);
  const findings = filterIgnored(scanSnapshots(prepared), config.secrets?.ignorePaths);
  if (findings.length > 0) {
    return makeReport('secrets-rejected', {
      secrets: findings,
      warnings,
      errors: [
        `检测到 ${findings.length} 处疑似密钥，已拒绝推送（store 未写入，未产生 commit）`,
        '请移除密钥，或在 homer.json 的 secrets.ignorePaths 中显式豁免该路径。',
      ],
    });
  }

  // ---- 3. 落盘前的安全判定 -------------------------------------------------
  const check = checkPushSafety(config, sources.base, sources.local, sources.remote);

  if (check.status === 'remote-ahead') {
    for (const file of check.remoteAheadFiles) warnings.push(`远端变更: ${fileRef(file)}`);
    return makeReport('remote-ahead', {
      warnings,
      errors: [
        `远端有 ${check.remoteAheadFiles.length} 处本地未见的变更，已拒绝推送（请先运行 \`homer pull\`）`,
      ],
    });
  }

  if (check.status === 'conflicts') {
    for (const conflict of check.conflictItems) warnings.push(`冲突: ${describeConflict(conflict)}`);
    return makeReport('conflicts', {
      warnings,
      errors: [
        `本地与远端有 ${check.conflictItems.length} 处冲突，已拒绝推送（请运行 \`homer merge\` 逐项裁决）`,
      ],
    });
  }

  const changedFiles = check.changedFiles;
  // 无漂移的两种含义必须分开：
  //   (a) 已有同步基线（store 已在某个 commit 中）→ 真正的 no-op（§2.8「changedFiles 空 → no-drift exit 0」）；
  //   (b) **尚未建立基线**（`homer init` 只写了 store 工作区，仓库已有历史但 store 还没进版本控制）
  //       → 必须把 store 补进 git 历史：否则用户永远拿不到第一个 store commit 与
  //       `state.lastSyncCommit`（§5 的 Done 判据），pull / merge 的 `requireFastForwardable`
  //       也会因「无 store 历史」而不可用。
  // 判据 = `仓库已有 commit（HEAD 可解析）∩ store 相对 HEAD 未提交`：
  //   - 无仓库 / 无 HEAD（未提交的 local-only 工作区）→ 保持 no-drift（W7 已钉的语义）；
  //   - 已 clone（有 homer.json 的 commit）+ init 后首次 push → 补 store commit（§1-D1 的首次基线；
  //     §3-P4 ①「init → push --yes → origin 有 commit」、§5「state.lastSyncCommit == git rev-parse HEAD」）。
  const needsBaseline = git.headCommit(paths.home) !== undefined && !git.isStoreClean(paths.home);
  if (changedFiles.length === 0 && !needsBaseline) {
    return makeReport('no-drift', { warnings });
  }
  if (changedFiles.length === 0) {
    warnings.push('本地快照已在 store 中，本次不重写 store 内容；仅把 store 补进 git 历史（建立同步基线）');
  }

  // ---- 4. 交互确认（--yes 时完全不创建 port，§2.7 约定 1）-------------------
  if (opts.yes !== true) {
    const port = deps?.ui ?? createDefaultPromptPort();
    const approved = await port.confirm(previewMessage(changedFiles), false);
    if (!approved) {
      // 非交互（默认 port 且非 TTY）时补一句「怎么才能继续」；注入 port 的测试不补。
      if (deps?.ui === undefined && process.stdout.isTTY !== true) warnings.push(NON_INTERACTIVE_HINT);
      return makeReport('aborted', {
        changedFiles,
        warnings,
        errors: ['已取消：未确认推送（store 未写入，未产生 commit）'],
      });
    }
  }

  // ---- 5. 写 store + git commit -------------------------------------------
  // 逐 adapter 原子覆盖（writeSnapshotToStore）；push 不碰工具目录、不做备份。
  for (const snapshot of prepared) writeSnapshotToStore(paths, snapshot);

  git.ensureGitRepo(paths.home);
  const commit = git.commitStoreIfNeeded(paths, pushCommitMessage(changedFiles.length));
  if (commit === undefined && !git.isStoreClean(paths.home)) {
    // store 有未提交改动 = commit 真的失败了（缺 git 身份 / git 不可用），不能谎报成功。
    return makeReport('error', {
      changedFiles,
      warnings,
      errors: [
        `store 已写入但 git commit 失败: ${paths.home}`,
        '请检查 git 是否可用与 user.name / user.email 配置（`git -C <home> config user.email`），然后重试。',
      ],
    });
  }

  const head = commit ?? git.headCommit(paths.home);
  if (head === undefined) {
    return makeReport('error', {
      changedFiles,
      warnings,
      errors: [`无法确定 HEAD commit: ${paths.home}（store 已写入）`],
    });
  }
  if (commit === undefined) {
    // store 内容与 HEAD 一致（幂等重跑 / state 丢失后的重推）：没有新 commit，但 HEAD 已是同步态。
    warnings.push('store 内容与 HEAD 一致，未产生新 commit');
  }

  // ---- 6. state 更新（先于 push：本地提交已成立，远端失败不应丢 base）--------
  saveState(paths, {
    ...loadState(paths),
    version: 1,
    lastSyncCommit: head,
    lastSyncAt: new Date().toISOString(),
    lastSyncCommand: 'push',
  });

  const report = makeReport('pushed', { changedFiles, commit: head, warnings });

  // ---- 7. 远端推送 --------------------------------------------------------
  if (opts.noPush === true) {
    warnings.push('--no-push: 只做本地 commit，未推送远端');
    return report;
  }

  if (!git.hasPushTarget(paths.home)) {
    warnings.push('未配置 git upstream，仅本地 commit（local-only 模式；如需推送请 `git push -u <remote> <branch>`）');
    return report;
  }

  const pushed = git.gitPush(paths.home);
  if (!pushed.ok) {
    const reason = pushed.stderr.trim() === '' ? '未知错误' : pushed.stderr.trim();
    return makeReport('error', {
      changedFiles,
      commit: head,
      warnings,
      errors: [`本地 commit 已成功（${head.slice(0, 7)}），远端推送失败: ${reason}`],
    });
  }

  report.pushedToRemote = true;
  return report;
}

/** 冲突项的一句话描述（reason + 冲突键点）。 */
function describeConflict(conflict: PullConflictAction): string {
  const keys = conflict.keyPaths !== undefined && conflict.keyPaths.length > 0
    ? ` [${conflict.keyPaths.join(', ')}]`
    : '';
  return `${fileRef(conflict)} (${conflict.reason})${keys}`;
}

/** 交互确认的预览文本：变更文件数汇总 + 前几个文件名（不刷屏）。 */
function previewMessage(changedFiles: readonly { adapterId: string; category: string; relPath: string }[]): string {
  const preview = changedFiles.slice(0, PREVIEW_FILE_LIMIT).map(fileRef).join(', ');
  const more = changedFiles.length > PREVIEW_FILE_LIMIT ? ` 等 ${changedFiles.length} 个` : '';
  return `将写入 store 并提交 ${changedFiles.length} 个变更文件（${preview}${more}），继续？`;
}

/** 预览里列出的文件名上限。 */
const PREVIEW_FILE_LIMIT = 5;

/** status → 人类可读标题。 */
const STATUS_TITLE: Record<PushReport['status'], string> = {
  pushed: '已推送',
  'no-drift': '无漂移，无需推送',
  'secrets-rejected': '密钥命中，已拒绝推送',
  'remote-ahead': '远端有本地未见的变更，已拒绝推送',
  conflicts: '存在冲突，已拒绝推送',
  aborted: '已取消',
  error: '失败',
};

/**
 * 人类可读渲染（§2.8：逐条打印 `path:line` + pattern + 脱敏摘录；计数与 `--json` 一致）。
 *
 * 计数口径与 `PushReport` 字段一一对应（变更文件数 = `changedFiles.length`，
 * 密钥命中数 = `secrets.length`），保证「文本与 --json 计数一致」可被断言。
 */
export function renderPushReport(report: PushReport): string {
  const lines: string[] = [`homer push: ${report.status}（${STATUS_TITLE[report.status]}）`];

  lines.push(`  变更文件: ${report.changedFiles.length}`);
  for (const file of report.changedFiles) lines.push(`    ${fileRef(file)}`);

  if (report.commit !== undefined) lines.push(`  本地提交: ${report.commit.slice(0, 7)}`);
  lines.push(`  远端推送: ${report.pushedToRemote ? '已推送' : '未推送'}`);

  // 计数行**无条件**打印（即使为 0）：文本计数与 `--json` 的 `secrets.length` 恒一致，
  // 脚本可以按 grep 判定，不必区分「有命中」与「无命中」两种文本形状。
  lines.push(`  密钥命中: ${report.secrets.length}`);
  for (const finding of report.secrets) {
    lines.push(`    ${finding.path}:${finding.line} [${finding.patternId}] ${finding.description}`);
    lines.push(`      摘录: ${finding.excerpt}`);
  }

  for (const warning of report.warnings) lines.push(`  ⚠ ${warning}`);
  for (const error of report.errors) lines.push(`  ✗ ${error}`);
  return lines.join('\n');
}
