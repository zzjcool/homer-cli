/**
 * excludeKeys 的「读写两侧」语义（docs/m2-plan.md §2.5 / DESIGN §2.2、§2.7；W4 唯一 owner）。
 *
 * 三重语义在这里收口两处，第三处（植回）由 plan.ts 在合并完成后调用 `plantExcludedKeys`：
 *   1. **push 写 store 前**：`applyExcludeKeyPlaceholders` 把 excludeKeys 列出的顶层键值整体替换为
 *      `__REQUIRED__`，使密钥值永不进入 git。
 *   2. **pull / merge 判定前**：`stripSnapshotExcludeKeys` 按 category 剥离这些顶层键 —— 直接复用
 *      engine 冻结的 `stripExcludeKeys`，与 `homer status`（`src/cli/render.ts` 的
 *      `stripAdapterExcludedKeys`）**同一实现点**。于是 store 里的 `__REQUIRED__` 占位符既不会被
 *      当成「远端变更」，也不会流入工具目录。
 *   3. **merge 完成后**：`plantExcludedKeys` 把 local 原文件中的 excluded 键值植回 merged ——
 *      本地密钥永不被远端覆盖；local 缺该键 → 不植回（命令层据此记 warning「缺失必填项」）。
 *
 * 纯函数、零 fs。依赖面刻意收窄为 `types.ts`（纯类型）+ engine 的 `stripExcludeKeys`：
 * 连 `entry-kind.ts` 也不 import（`isJsonObject` 就地实现），保证 W4 只碰 plan §3-P1-W4 授权的依赖。
 */

import { stripExcludeKeys } from '../engine/index.js';
import type { AdapterSnapshot, CategorySnapshot, HomerConfig, SnapshotEntry, SnapshotFiles } from '../types.js';

/** excludeKeys 占位符字面量（DESIGN §2.2：参考 legout/pi-config 的 `__REQUIRED__` 方案）。 */
export const REQUIRED_PLACEHOLDER = '__REQUIRED__';

/** 冻结的确定性 JSON 落盘格式（plan §2.5：`JSON.stringify(merged, null, 2) + '\n'`）。 */
export function serializeJsonContent(value: unknown): string {
  return `${JSON.stringify(value, null, 2)}\n`;
}

/**
 * 非 null、非数组的对象（JSON 对象语义）。
 * 与 `core/entry-kind.ts` 的 `isPlainObject` 同义；此处就地实现是为了不扩大 W4 的依赖面
 * （plan §3-P1-W4 限定「只 import types / engine / sync-types」）。
 */
export function isJsonObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/** 可被 `JSON.parse` 接受的文本 → 值；失败 → `undefined`。 */
export function parseJsonContent(content: string): unknown {
  try {
    return JSON.parse(content) as unknown;
  } catch {
    return undefined;
  }
}

/**
 * `adapterId` → `{ categoryName → excludeKeys }`，只保留非空条目。
 * 与 `homer status` 的剥离口径同构（planPull / checkPushSafety 共用本函数）。
 */
export function excludeKeysByCategory(config: HomerConfig, adapterId: string): Record<string, string[]> {
  const adapter = config.adapters[adapterId];
  if (adapter === undefined) return {};

  const out: Record<string, string[]> = {};
  for (const [name, category] of Object.entries(adapter.categories)) {
    const keys = category.excludeKeys;
    if (keys !== undefined && keys.length > 0) out[name] = keys;
  }
  return out;
}

const EMPTY_KEYS: string[] = [];

/** 单个 category 的 excludeKeys（未配置 → 空数组，零分配）。 */
export function excludedKeysFor(config: HomerConfig, adapterId: string, category: string): readonly string[] {
  return config.adapters[adapterId]?.categories[category]?.excludeKeys ?? EMPTY_KEYS;
}

/** 按 config 剥离单快照的 excludeKeys（判定前置口径；无配置 → 原快照返回）。 */
export function stripSnapshotExcludeKeys(snapshot: AdapterSnapshot, config: HomerConfig): AdapterSnapshot {
  const byCategory = excludeKeysByCategory(config, snapshot.adapterId);
  if (Object.keys(byCategory).length === 0) return snapshot;
  return stripExcludeKeys(snapshot, byCategory);
}

/**
 * push 写 store 前：excludeKeys 顶层键 → 值 `__REQUIRED__`。
 *
 * 只替换**原本就存在**的键：不凭空造出用户从未拥有的配置键（「缺失必填项」由 pull 侧
 * 「local 缺该键 → 不植回」的反向校验负责）。文件中没有命中任何 excluded 键 → 条目字节不变，
 * 避免给 store 引入无意义的格式 diff。
 */
export function applyExcludeKeyPlaceholders(snapshot: AdapterSnapshot, config: HomerConfig): AdapterSnapshot {
  const byCategory = excludeKeysByCategory(config, snapshot.adapterId);
  return {
    adapterId: snapshot.adapterId,
    categories: snapshot.categories.map((category) => {
      const keys = byCategory[category.category];
      if (keys === undefined) return cloneCategory(category);
      return { ...category, files: placeholderFiles(category.files, keys) };
    }),
  };
}

/**
 * merge 完成后把 local 原文件中的 excluded 键值植回 merged（本地永不被远端覆盖）。
 * local 缺该键 → 不植回（保留 merged 中「无此键」的事实）；无可植回键 → 原值返回（零拷贝）。
 */
export function plantExcludedKeys(
  merged: Record<string, unknown>,
  localValue: unknown,
  keys: readonly string[],
): Record<string, unknown> {
  if (keys.length === 0 || !isJsonObject(localValue)) return merged;

  const planted: [string, unknown][] = [];
  let changed = false;
  for (const [key, value] of Object.entries(merged)) {
    if (keys.includes(key) && hasOwn(localValue, key)) {
      planted.push([key, localValue[key]]); // 本地值优先（防御：正常流程下 merged 已被 strip）
      changed = true;
      continue;
    }
    planted.push([key, value]);
  }
  for (const key of keys) {
    if (hasOwn(merged, key) || !hasOwn(localValue, key)) continue;
    planted.push([key, localValue[key]]);
    changed = true;
  }

  // Object.fromEntries 用 CreateDataProperty 落键（`__proto__` 当普通数据键，不污染原型）。
  return changed ? Object.fromEntries(planted) : merged;
}

/* ------------------------------------------------------------------ */
/* 内部实现                                                            */
/* ------------------------------------------------------------------ */

function cloneCategory(category: CategorySnapshot): CategorySnapshot {
  return { ...category, files: new Map(category.files) };
}

function hasOwn(value: object, key: string): boolean {
  // 原型键（`__proto__` / `constructor`）不得沿原型链取到值。
  return Object.prototype.hasOwnProperty.call(value, key);
}

function placeholderFiles(files: SnapshotFiles, keys: readonly string[]): SnapshotFiles {
  const out: SnapshotFiles = new Map();
  for (const [relPath, entry] of files) out.set(relPath, placeholderEntry(entry, keys));
  return out;
}

function placeholderEntry(entry: SnapshotEntry, keys: readonly string[]): SnapshotEntry {
  if (entry.kind !== 'json') return entry;
  const value = parseJsonContent(entry.content);
  if (!isJsonObject(value)) return entry;
  if (!Object.keys(value).some((key) => keys.includes(key))) return entry;

  const replaced = Object.entries(value).map(
    ([key, item]) => [key, keys.includes(key) ? REQUIRED_PLACEHOLDER : item] as const,
  );
  return { kind: 'json', content: serializeJsonContent(Object.fromEntries(replaced)) };
}
