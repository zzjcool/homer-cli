/**
 * `src/core/sync/` 公共出口（barrel）。
 *
 * P0 落地 `types.ts`（冻结的数据形状）；W4 在此导出判定层（plan / excluded-keys）。
 * P2-W6 会在本文件追加 `base.js` / `apply.js` / `pipeline.js` 的导出（本 worker 不动）。
 *
 * 纯函数、零 fs：不 re-export 任何 IO 模块。
 */

export type {
  PullWriteAction,
  PullDeleteAction,
  PullConflictAction,
  PullAction,
  PullPlan,
  ApplyResult,
  SyncBaseMode,
  SyncSources,
} from './types.js';

export { planPull, checkPushSafety, type PushCheck } from './plan.js';

export {
  applyExcludeKeyPlaceholders,
  plantExcludedKeys,
  stripSnapshotExcludeKeys,
  excludeKeysByCategory,
  excludedKeysFor,
  serializeJsonContent,
  REQUIRED_PLACEHOLDER,
} from './excluded-keys.js';
