/**
 * `homer push` —— 本地快照 → 密钥扫描 → store → git commit(+push) → state（docs/m2-plan.md §2.8）。
 *
 * P0：接口逐字落地（冻结）+ stub。实现由 **P3-W7** 在本文件内完成
 * （含渲染：`renderPushReport` 是 P0 预置的渲染钩子，使 W7 无需改 index.ts / args.ts）。
 */

import { CliError } from '../../core/errors.js';
import type { SecretFinding } from '../../core/secrets/types.js';
import type { SyncSources } from '../../core/sync/types.js';
import type { PromptPort } from '../ui.js';

export interface PushOptions { homerHome?: string; json?: boolean; yes?: boolean; noPush?: boolean; }
export interface PushDeps { ui?: PromptPort; sources?: SyncSources; }
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

/**
 * 推送。
 * P0 stub：接口已冻结，实现见 W7（密钥扫描 → checkPushSafety → 预览确认 → writeStore → commit → push）。
 */
export async function runPush(opts: PushOptions, deps?: PushDeps): Promise<PushReport> {
  throw new CliError('尚未实现');
}

/**
 * 人类可读渲染（P0 占位：W7 按 §2.8 push 流程逐条细化，无需改分发层）。
 */
export function renderPushReport(report: PushReport): string {
  const lines: string[] = [`homer push: ${report.status}`];
  lines.push(`  变更文件: ${report.changedFiles.length}`);
  if (report.commit !== undefined) lines.push(`  本地提交: ${report.commit}`);
  lines.push(`  远端推送: ${report.pushedToRemote ? '已推送' : '未推送'}`);
  for (const finding of report.secrets) {
    lines.push(`  ⚠ 密钥命中: ${finding.path}:${finding.line} ${finding.patternId}`);
  }
  for (const warning of report.warnings) lines.push(`  ⚠ ${warning}`);
  for (const error of report.errors) lines.push(`  ✗ ${error}`);
  return lines.join('\n');
}
