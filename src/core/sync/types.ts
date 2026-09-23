/**
 * 同步内核类型（docs/m2-plan.md §2.5，P0 落地，**冻结**）。
 *
 * 只放「三方判定 → 动作」的数据形状；`planPull` / `checkPushSafety`（P1-W4）、
 * `collectSyncSources` / `applyPullActions` / pipeline（P2-W6）分别在各自模块实现。
 * 所有消费者（命令层 W7/W8/W9、引擎 W4/W6）共用本文件的形状，杜绝各写一套。
 */

import type { AdapterSnapshot } from '../types.js';

/* ---- pull 动作 ---- */

export interface PullWriteAction  { type: 'write';  adapterId: string; category: string; relPath: string; content: string; }
export interface PullDeleteAction { type: 'delete'; adapterId: string; category: string; relPath: string; }
export interface PullConflictAction {
  type: 'conflict'; adapterId: string; category: string; relPath: string;
  reason: 'modify-vs-modify' | 'local-delete-vs-remote-modify' | 'local-modify-vs-remote-delete' | 'merge-keys';
  keyPaths?: string[];          // reason='merge-keys' 时的冲突键点路径
  localContent?: string; remoteContent?: string;   // mirror 文件冲突时给全量内容供 merge 交互展示
}
export type PullAction = PullWriteAction | PullDeleteAction | PullConflictAction;
export interface PullPlan { actions: PullAction[]; }

/* ---- 应用结果 ---- */

export interface ApplyResult {
  written:  { adapterId: string; category: string; relPath: string }[];
  deleted:  { adapterId: string; category: string; relPath: string }[];
  conflicts:{ adapterId: string; category: string; relPath: string }[]; // 跳过保留本地
  backupDir?: string;
}

/* ---- 三方原料 ---- */

/**
 * base 的来源（`'git'` = 来自 git 历史里的 `lastSyncCommit`；`'store'` = 回落 store 工作区）。
 *
 * **M2 仅填充不消费，消费归 M3**（对抗式 review rev-simplicity major）：三个描述性字段
 * `mode` / `baseCommit` / `remoteRef` 目前只在 `collectSyncSources` 里被写入、被测试断言，
 * 命令层没有任何分支依赖它们。M2 保留它们是为了让 `homer status --json` 的消费者与 M3 的
 * `doctor` / `--offline` 有现成的诊断面（降级判定已经在 base.ts 里真实发生）。M3 再做消费者，
 * 不在此刻提前加无用的分支。
 */
export type SyncBaseMode = 'git' | 'store';
export interface SyncSources {
  mode: SyncBaseMode;      // M2 仅填充不消费（诊断面），消费归 M3
  base: AdapterSnapshot[]; local: AdapterSnapshot[]; remote: AdapterSnapshot[];
  baseCommit?: string;     // mode='git' 时填；M2 仅填充不消费（诊断面），消费归 M3
  remoteRef?: string;      // 仅填充不消费（诊断面），消费归 M3
  warnings: string[]; errors: string[];
}
