/**
 * 本地同步状态 `~/.homer/state.json`（docs/m2-plan.md §2.1，P0 完整实现）。
 *
 * 冻结签名：
 *   HomerState / loadState(paths): HomerState / saveState(paths, state): void
 *
 * 语义（计划原文）：
 *   - `loadState`：文件缺失 / 损坏 → `{ version: 1 }`，**不 throw**（同步状态是加速信息，
 *     任何读取失败都必须降级为「无 base」而不是让 CLI 崩掉；对应 §1 决策 D3 的 fallback 链）。
 *   - `saveState`：tmp + rename 原子写（同 config.saveConfig），避免写到一半崩溃留下坏 state
 *     被下一轮读成 `lastSyncCommit` 缺失 / 截断的脏值。
 *
 * state.json 与 `backups/` 一样**不入库**（§1 决策 D5）：同步策略本身不参与同步。
 */

import fs from 'node:fs';
import path from 'node:path';

import type { HomerPaths } from './paths.js';
import { isPlainObject } from './entry-kind.js';

/** `lastSyncCommand` 的取值集合（冻结，与 sync/state 记录口径一致）。 */
const SYNC_COMMANDS = ['push', 'pull', 'merge'] as const;

export interface HomerState {
  version: 1;
  lastSyncCommit?: string;                      // 上次同步成功后的 HEAD SHA
  lastSyncAt?: string;                          // ISO 8601
  lastSyncCommand?: 'push' | 'pull' | 'merge';
}

/** 空状态（文件缺失 / 损坏时的降级值）。 */
function emptyState(): HomerState {
  return { version: 1 };
}

/**
 * 读取 state.json。
 * 任何失败（ENOENT / 非 JSON / 顶层非对象）→ `{ version: 1 }`。
 * 字段级容忍：类型不对的可选字段直接丢弃（不抛、不猜），未知键忽略。
 */
export function loadState(paths: HomerPaths): HomerState {
  let text: string;
  try {
    text = fs.readFileSync(paths.stateFile, 'utf8');
  } catch {
    return emptyState();
  }

  let raw: unknown;
  try {
    raw = JSON.parse(text) as unknown;
  } catch {
    return emptyState();
  }

  if (!isPlainObject(raw)) return emptyState();

  const state = emptyState();

  const commit = raw['lastSyncCommit'];
  if (typeof commit === 'string' && commit.trim() !== '') state.lastSyncCommit = commit;

  const at = raw['lastSyncAt'];
  if (typeof at === 'string' && at.trim() !== '') state.lastSyncAt = at;

  const command = raw['lastSyncCommand'];
  if (typeof command === 'string' && (SYNC_COMMANDS as readonly string[]).includes(command)) {
    state.lastSyncCommand = command as HomerState['lastSyncCommand'];
  }

  return state;
}

/**
 * 写入 state.json（2 空格缩进 + 末尾换行，与 homer.json 手写格式一致）。
 * 先写 `<stateFile>.tmp-<pid>` 再 rename：同目录 rename 在同一文件系统上是原子的，
 * 读侧要么看到旧版完整内容、要么看到新版完整内容。
 */
export function saveState(paths: HomerPaths, state: HomerState): void {
  fs.mkdirSync(path.dirname(paths.stateFile), { recursive: true });
  const tmp = `${paths.stateFile}.tmp-${process.pid}`;
  fs.writeFileSync(tmp, `${JSON.stringify(state, null, 2)}\n`, 'utf8');
  fs.renameSync(tmp, paths.stateFile);
}
