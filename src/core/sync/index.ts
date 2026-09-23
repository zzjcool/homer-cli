/**
 * `src/core/sync/` 公共出口（barrel）。
 *
 * P0 落地 `types.ts`（冻结的数据形状）；W4 在此导出判定层（plan / excluded-keys）。
 * P2-W6 追加 `base.js` / `apply.js` / `pipeline.js` 的导出。
 *
 * 注意：本 barrel **不** re-export `cli/render.js` 的采集器；`collectSyncSources` 是 M2 的
 * 三方原料唯一入口（含 git base/remote），M1 的 store-only 采集器仍由命令层各自 import。
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
  planFirstContact,
  emptyBaseSnapshots,
  type FirstContactMode,
  type FirstContactPlan,
} from './first-sync.js';

export {
  applyExcludeKeyPlaceholders,
  plantExcludedKeys,
  stripSnapshotExcludeKeys,
  excludeKeysByCategory,
  excludedKeysFor,
  serializeJsonContent,
  REQUIRED_PLACEHOLDER,
} from './excluded-keys.js';

/* W6：IO 编排层（base / apply / pipeline）。不纯，但仍是 core 内部实现，命令层从这里取。 */

export { collectSyncSources, type CollectSyncSourcesOptions } from './base.js';

export { applyPullActions, type ApplyPullActionsOptions } from './apply.js';

export {
  prepareStoreSnapshot,
  commitStoreIfNeeded,
  requireCleanStore,
  requireFastForwardable,
  NOT_A_REPO_HINT,
  NO_UPSTREAM_HINT,
  NO_UPSTREAM_MESSAGE,
  notAGitRepoMessage,
} from './pipeline.js';
