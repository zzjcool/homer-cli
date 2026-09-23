/**
 * `homer pull` —— git fetch → 三路判定 → 预览/确认 → 备份 → 应用到工具目录（docs/m2-plan.md §2.8）。
 *
 * P0：接口逐字落地（冻结）+ stub。实现由 **P3-W8** 在本文件内完成。
 */

import { CliError } from '../../core/errors.js';
import type { ApplyResult, PullConflictAction, SyncSources } from '../../core/sync/types.js';
import type { PromptPort } from '../ui.js';

export interface PullOptions { homerHome?: string; json?: boolean; yes?: boolean; }
export interface PullDeps { ui?: PromptPort; sources?: SyncSources; noFetch?: boolean; noApply?: boolean; }
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

/**
 * 拉取并应用。
 * P0 stub：接口已冻结，实现见 W8（前置检查 → planPull → 预览确认 → applyPullActions → ff → state）。
 */
export async function runPull(opts: PullOptions, deps?: PullDeps): Promise<PullReport> {
  throw new CliError('尚未实现');
}

/**
 * 人类可读渲染（P0 占位：W8 补 diffLines 行级预览与冲突标红，无需改分发层）。
 */
export function renderPullReport(report: PullReport): string {
  const lines: string[] = [`homer pull: ${report.status}`];
  lines.push(`  写入: ${report.applied.written.length}  删除: ${report.applied.deleted.length}  冲突: ${report.conflicts.length}`);
  if (report.applied.backupDir !== undefined) lines.push(`  备份目录: ${report.applied.backupDir}`);
  if (report.commit !== undefined) lines.push(`  同步提交: ${report.commit}`);
  for (const conflict of report.conflicts) {
    lines.push(`  ⚡ 冲突: ${conflict.adapterId}/${conflict.category}/${conflict.relPath} (${conflict.reason})`);
  }
  for (const warning of report.warnings) lines.push(`  ⚠ ${warning}`);
  for (const error of report.errors) lines.push(`  ✗ ${error}`);
  return lines.join('\n');
}
