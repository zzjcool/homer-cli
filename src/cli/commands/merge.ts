/**
 * `homer merge` —— 交互式冲突解决（docs/m2-plan.md §2.8）。
 *
 * P0：接口逐字落地（冻结）+ stub。实现由 **P3-W9** 在本文件内完成。
 */

import { CliError } from '../../core/errors.js';
import type { ApplyResult, SyncSources } from '../../core/sync/types.js';
import type { PromptPort } from '../ui.js';

export interface MergeOptions { homerHome?: string; json?: boolean; acceptLocal?: boolean; acceptRemote?: boolean; }
export interface MergeDeps { ui?: PromptPort; sources?: SyncSources; noFetch?: boolean; }
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

逐项裁决本地与远端的冲突，并把裁决结果写回工具目录（选择远端时先备份）。
无 --accept-* 且在 TTY 下会逐项询问 Accept Local / Accept Remote。

选项:
  --home <dir>         homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --accept-local      批量裁决：全部保留本地
  --accept-remote     批量裁决：全部采用远端
  --json              输出机器可读 JSON（MergeReport）
  -h, --help          显示本帮助

退出码: resolved / no-conflicts → 0；aborted / error → 1。`;

/**
 * 冲突裁决。
 * P0 stub：接口已冻结，实现见 W9（planPull 取 conflict → 裁决 → ff → 应用 → push 管线）。
 */
export async function runMerge(opts: MergeOptions, deps?: MergeDeps): Promise<MergeReport> {
  throw new CliError('尚未实现');
}

/**
 * 人类可读渲染（P0 占位：W9 补逐项展示，无需改分发层）。
 */
export function renderMergeReport(report: MergeReport): string {
  const lines: string[] = [`homer merge: ${report.status}`];
  for (const resolution of report.resolutions) {
    const target = `${resolution.adapterId}/${resolution.category}/${resolution.relPath}`;
    lines.push(`  ${resolution.choice === 'local' ? '←' : '→'} ${target}`);
  }
  lines.push(`  写入: ${report.applied.written.length}  删除: ${report.applied.deleted.length}`);
  if (report.commit !== undefined) lines.push(`  同步提交: ${report.commit}`);
  for (const warning of report.warnings) lines.push(`  ⚠ ${warning}`);
  for (const error of report.errors) lines.push(`  ✗ ${error}`);
  return lines.join('\n');
}
