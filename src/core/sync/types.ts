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

export type SyncBaseMode = 'git' | 'store';
export interface SyncSources {
  mode: SyncBaseMode;
  base: AdapterSnapshot[]; local: AdapterSnapshot[]; remote: AdapterSnapshot[];
  baseCommit?: string; remoteRef?: string;    // mode='git' 时填
  warnings: string[]; errors: string[];
}
