/**
 * M1 三路判定引擎 —— merge 半边（纯函数，零 fs）。
 *
 * 语义冻结于 docs/m1-plan.md §1.2（对应 DESIGN §2.7）：
 *   - 嵌套对象：递归合并
 *   - 数组：原子值（base≡local 取 remote；base≡remote 取 local；双方同改同值取该值；
 *           双方改且不同 → 冲突 'array-both-changed'）
 *   - local 删键 / remote 未动 → 删除生效（对称同理）
 *   - local 删键 / remote 改值（及对称）→ 冲突 'modify-vs-delete'
 *   - 双方改不同键 → 自动合并
 *   - 双方改同键不同值 → 冲突 'both-modified'
 *   - 一方新增键 → 取新增方
 *   - merge 分类内 kind:'file' 条目由 drift 层降级（本模块不做判定）
 *
 * 本模块只依赖冻结的 types.ts（纯类型）与 core/entry-kind.ts（零 fs 的 JSON 形态判定），
 * 保持可独立测试。
 */

import { isPlainObject } from '../entry-kind.js';

export interface MergeConflict {
  keyPath: string;  // 点路径，如 'models.openai'；数组整体记数组键名
  reason: 'both-modified' | 'modify-vs-delete' | 'array-both-changed';
  base?: unknown; local?: unknown; remote?: unknown;
}

export interface MergeResult {
  status: 'clean' | 'conflict';
  merged: Record<string, unknown>;  // 冲突键暂取 local 值并列入 conflicts（M2 交互再裁决）
  conflicts: MergeConflict[];
}

/** 单文件 JSON 三路合并（base/local/remote 为已解析值） */
export function mergeJson(base: unknown, local: unknown, remote: unknown): MergeResult {
  const conflicts: MergeConflict[] = [];
  const resolved = mergeSlot('', slotOf(base), slotOf(local), slotOf(remote), conflicts);

  // 冻结签名的 merged 是 Record：根值为普通对象时逐键合并结果即它；
  // 根值非对象（罕见）时无法表达为 Record，退化为空对象（M1 的 merge 文件均为 JSON 对象）。
  const merged: Record<string, unknown> =
    resolved.has && isPlainObject(resolved.value) ? resolved.value : {};

  return { status: conflicts.length > 0 ? 'conflict' : 'clean', merged, conflicts };
}

/**
 * 键级变更统计：local 相对 base 改/增/删了哪些键 —— status ↑ 计数来源。
 * keys = 变更键路径（changed ∪ added ∪ deleted，排序后）；嵌套对象递归到叶子键，
 * 整体新增/删除的子树只记该子树的根键（不拆分）。
 */
export function diffJson(base: unknown, local: unknown):
  { changed: number; added: number; deleted: number; keys: string[] } {
  const changed: string[] = [];
  const added: string[] = [];
  const deleted: string[] = [];
  walkDiff('', slotOf(base), slotOf(local), { changed, added, deleted });
  return {
    changed: changed.length,
    added: added.length,
    deleted: deleted.length,
    keys: [...changed, ...added, ...deleted].sort(),
  };
}

/**
 * excludeKeys 剥离（顶层键），drift 比较前对 local 与 base 各调一次。
 * 签名按 docs/m1-plan.md §1.2 冻结为 (unknown) => unknown —— 入参/出参都是任意 JSON 值，
 * 调用方自行 narrow，故此处显式豁免 no-unknown-returns 规则。
 *
 * 键存在性判定用 hasOwnProperty（原型键 `__proto__` / `constructor` 不得误命中），
 * 拷贝用普通赋值：`__proto__` 作为普通字符串键会被 `out[k] = v` 静默丢弃（不污染原型），
 * 这正好符合「不把原型键当配置项」的预期；其余键（含 constructor）正常保留。
 */
// pi-lens-ignore: no-unknown-returns
export function stripKeys(value: unknown, keys: string[]): unknown {
  if (!isPlainObject(value)) return value;
  const strip = new Set(keys);
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(value)) {
    if (strip.has(k)) continue;
    out[k] = v;
  }
  return out;
}

/* ------------------------------------------------------------------ */
/* 内部实现                                                            */
/* ------------------------------------------------------------------ */

interface Slot { has: boolean; value: unknown }

/** JSON 值不可能为 undefined，因此 undefined 等价于「键不存在」 */
function slotOf(value: unknown): Slot {
  return value === undefined ? { has: false, value: undefined } : { has: true, value };
}

/**
 * 深比较（JSON 值语义）。
 * 用 Object.is 而非 `===`：NaN 与自身不等，会让 base≡local 的 NaN 条目被误判为变更；
 * Object.is 同时把 +0 / -0 视为不同值（对 JSON 而言极少见，但「不同就当变了」更安全）。
 */
function deepEqual(a: unknown, b: unknown): boolean {
  if (Object.is(a, b)) return true;
  if (Array.isArray(a) && Array.isArray(b)) {
    return a.length === b.length && a.every((item, i) => deepEqual(item, b[i]));
  }
  if (isPlainObject(a) && isPlainObject(b)) {
    const ka = Object.keys(a);
    const kb = Object.keys(b);
    if (ka.length !== kb.length) return false;
    return ka.every((k) => Object.prototype.hasOwnProperty.call(b, k) && deepEqual(a[k], b[k]));
  }
  return false;
}

function sameSlot(a: Slot, b: Slot): boolean {
  if (a.has !== b.has) return false;
  return !a.has || deepEqual(a.value, b.value);
}

function childSlot(parent: Slot, key: string): Slot {
  if (!parent.has || !isPlainObject(parent.value)) return { has: false, value: undefined };
  // hasOwnProperty 守卫：`__proto__` / `constructor` 这类原型键不能沿原型链取到值，
  // 否则会把 Object.prototype 上的成员当成文件内容（无中生有的漂移）。当作普通键处理，取不到即 absent。
  if (!Object.prototype.hasOwnProperty.call(parent.value, key)) return { has: false, value: undefined };
  return slotOf(parent.value[key]);
}

function mergeSlot(
  path: string,
  base: Slot,
  local: Slot,
  remote: Slot,
  conflicts: MergeConflict[],
): Slot {
  const absent: Slot = { has: false, value: undefined };

  // 1) 双方都未相对 base 变化（含三方都不存在）→ 无事
  if (sameSlot(local, base) && sameSlot(remote, base)) return local.has ? local : absent;

  // 2) base≡local → 取 remote（remote 改值 / 新增 / 删除都由此覆盖）
  if (sameSlot(local, base)) return remote;

  // 3) base≡remote → 取 local（对称）
  if (sameSlot(remote, base)) return local;

  // 4) 双方都相对 base 变了
  if (!local.has && !remote.has) return absent; // 双删 = 无事

  if (!local.has || !remote.has) {
    let conflict: MergeConflict;
    if (!base.has) {
      // base 不存在时，absent 的一方必然等于 base，已被 2)/3) 捕获；此处仅防御。
      return local.has ? local : remote;
    }
    // 删除 vs 改值：真歧义
    conflict = { keyPath: path, reason: 'modify-vs-delete' };
    pushConflict(conflicts, conflict, base, local, remote);
    return local.has ? local : absent; // 冲突键暂取 local（local 删除则键消失）
  }

  // 双方都 present 且都相对 base 变了
  if (deepEqual(local.value, remote.value)) return local; // 同改同值

  const bothObjects = isPlainObject(local.value) && isPlainObject(remote.value);
  if (bothObjects && (!base.has || isPlainObject(base.value))) {
    return mergeObject(path, base, local, remote, conflicts);
  }

  if (Array.isArray(local.value) || Array.isArray(remote.value)) {
    pushConflict(conflicts, { keyPath: path, reason: 'array-both-changed' }, base, local, remote);
    return local;
  }

  pushConflict(conflicts, { keyPath: path, reason: 'both-modified' }, base, local, remote);
  return local;
}

function mergeObject(
  path: string,
  base: Slot,
  local: Slot,
  remote: Slot,
  conflicts: MergeConflict[],
): Slot {
  const out: Record<string, unknown> = {};
  for (const key of unionKeys(base, local, remote)) {
    const childPath = path === '' ? key : `${path}.${key}`;
    const resolved = mergeSlot(
      childPath,
      childSlot(base, key),
      childSlot(local, key),
      childSlot(remote, key),
      conflicts,
    );
    if (resolved.has) out[key] = resolved.value;
  }
  return { has: true, value: out };
}

/** 键的并集，保持 base → local → remote 的稳定插入顺序 */
function unionKeys(...slots: Slot[]): string[] {
  const seen = new Set<string>();
  const keys: string[] = [];
  for (const slot of slots) {
    if (!slot.has || !isPlainObject(slot.value)) continue;
    for (const key of Object.keys(slot.value)) {
      if (!seen.has(key)) {
        seen.add(key);
        keys.push(key);
      }
    }
  }
  return keys;
}

function pushConflict(
  conflicts: MergeConflict[],
  partial: MergeConflict,
  base: Slot,
  local: Slot,
  remote: Slot,
): void {
  const conflict: MergeConflict = { keyPath: partial.keyPath, reason: partial.reason };
  if (base.has) conflict.base = base.value;
  if (local.has) conflict.local = local.value;
  if (remote.has) conflict.remote = remote.value;
  conflicts.push(conflict);
}

interface DiffBuckets { changed: string[]; added: string[]; deleted: string[] }

function walkDiff(path: string, base: Slot, local: Slot, buckets: DiffBuckets): void {
  const label = path === '' ? '$' : path;

  if (!base.has && !local.has) return;
  if (!local.has) {
    buckets.deleted.push(label);
    return;
  }
  if (!base.has) {
    buckets.added.push(label);
    return;
  }
  if (deepEqual(base.value, local.value)) return;

  const bothObjects = isPlainObject(base.value) && isPlainObject(local.value);
  if (bothObjects) {
    for (const key of unionKeys(base, local)) {
      const childPath = path === '' ? key : `${path}.${key}`;
      walkDiff(childPath, childSlot(base, key), childSlot(local, key), buckets);
    }
    return;
  }

  buckets.changed.push(label);
}
