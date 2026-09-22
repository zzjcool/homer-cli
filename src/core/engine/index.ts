/**
 * M1 判定引擎公共出口（barrel）。
 * 纯函数、零 fs；只 re-export merge / mirror / drift 的冻结接口。
 */

export {
  mergeJson,
  diffJson,
  stripKeys,
  type MergeConflict,
  type MergeResult,
} from './merge.js';

export {
  compareFile,
  compareCategory,
  type MirrorOp,
} from './mirror.js';

export {
  computeDrift,
  stripExcludeKeys,
  type AdapterDrift,
  type CategoryDrift,
} from './drift.js';
