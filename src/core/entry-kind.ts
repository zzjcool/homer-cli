/**
 * M1 共享的 JSON 形态判定（单一实现点，零 fs）。
 *
 * 背景（对抗式 review 修复项 4 / 6）：`isPlainObject` 曾在 merge / config / diff 三处各自复制，
 * `parse 成功 → kind:'json'` 的判定也曾在 scan.ts 与 store.ts 各写一份——两侧一旦漂移，
 * 「scan 写出的 kind」与「store 读回的 kind」就会不一致，roundtrip 判定失真。
 * 因此统一收敛到本模块。
 *
 * 依赖：仅 `types.js`（纯类型，`import type` 编译期擦除），运行时零 import、零副作用，
 * 故 engine（merge.ts）也可以安全引用，不破坏「引擎纯函数」约束。
 */

import type { SnapshotEntry, SyncMode } from './types.js';

/** 可被 `JSON.parse` 接受的文本。 */
export function isParsableJson(content: string): boolean {
  try {
    JSON.parse(content);
    return true;
  } catch {
    return false;
  }
}

/**
 * 冻结的 kind 约定（plan §1.1 注释 / §1.6）：
 * merge 分类中可 parse 的条目 → `'json'`，其余（mirror 分类、或 merge 里解析失败）→ `'file'`。
 * scan 侧（实时文件）与 store 侧（落盘快照）必须用同一函数，保证 roundtrip 的 kind 稳定。
 */
export function entryKindFor(mode: SyncMode, content: string): SnapshotEntry['kind'] {
  return mode === 'merge' && isParsableJson(content) ? 'json' : 'file';
}

/** 非 null、非数组的对象（JSON 对象语义）。 */
export function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
