/**
 * sync 内核 —— 三路判定 → 动作计划（docs/m2-plan.md §2.5，P1-W4，纯函数零 fs）。
 *
 * 两个出口：
 *   - `planPull`：把 base/local/remote 三方快照翻译成**逐文件动作清单**
 *     （write / delete / conflict），命令层只负责预览、备份、应用，不再自行判定。
 *   - `checkPushSafety`：push 前的安全检查（remote-ahead / conflicts / ok）+ 变更文件清单。
 *
 * 判定口径与 `homer status` 保持**同一实现与同一结论**（本模块的核心不变式：
 * `planPull` 的 write+delete+conflict 数量，就是 `computeDrift` 报的 pull+conflicts 数量）：
 *   - mirror 分类（含 merge 分类里 JSON 损坏而降级的 file 条目）→ engine 的 `compareFile`；
 *   - merge 分类 → 三方 `mergeJson`（逐键；任一键冲突 → **整文件** conflict）；
 *   - base 缺失（首次对接 / 远端新文件）→ 沿用 engine 的降级矩阵：仅 local → push（无动作），
 *     仅 remote → pull（write），两侧都有且内容不同 → conflict（`modify-vs-modify`）。
 *
 * ## 原始快照 vs 剥离后快照（excludeKeys 三重语义的落地关键）
 *
 * 判定用**剥离后**快照（与 status 同口径），但有两处必须回到**原始**快照：
 *   1. `mergeJson` 之后要把 local **原文件**里的 excluded 键值植回 merged（本地密钥永不被远端覆盖）；
 *      剥离后的 local 已经没有这些键，不回到原始快照就无法植回。
 *   2. conflict action 的 `localContent` / `remoteContent` 是「给 merge 交互展示的全量文件内容」，
 *      故给原始内容（用户要看到自己真实的密钥键，才能判断保留哪边）。
 *
 * 而 **write action 的 content**（= 真正会落到工具目录的字节）恒来自剥离后的世界：
 *   - mirror → 剥离后 remote 原文；
 *   - merge → `mergeJson(剥离后三方)` + 植回 local 原始 excluded 键值。
 * 于是「远端 store 里的 `__REQUIRED__` 永不流入工具目录」由构造保证。
 *
 * 依赖面（plan §3-P1-W4 冻结）：`types.ts`（纯类型）、`engine/index.js`、`sync/types.js`、同目录 excluded-keys。
 */

import {
  compareCategory,
  compareFile,
  computeDrift,
  diffJson,
  mergeJson,
  type AdapterDrift,
  type MirrorOp,
} from '../engine/index.js';
import type { AdapterSnapshot, CategorySnapshot, HomerConfig, SnapshotEntry, SnapshotFiles } from '../types.js';
import type { PullAction, PullConflictAction, PullPlan } from './types.js';
import {
  excludedKeysFor,
  isJsonObject,
  parseJsonContent,
  plantExcludedKeys,
  serializeJsonContent,
  stripSnapshotExcludeKeys,
} from './excluded-keys.js';

/* ------------------------------------------------------------------ */
/* planPull                                                            */
/* ------------------------------------------------------------------ */

/** 三方快照按 adapterId 建索引，便于「剥离后判定 + 原始取内容」双线取数。 */
function indexByAdapter(snapshots: readonly AdapterSnapshot[]): Map<string, AdapterSnapshot> {
  const map = new Map<string, AdapterSnapshot>();
  for (const snapshot of snapshots) map.set(snapshot.adapterId, snapshot);
  return map;
}

/**
 * 生成 pull 动作计划。
 *
 * 语义矩阵（冻结，docs/m2-plan.md §2.5）：
 *   - mirror 分类：`pull` → write(remote 内容)、`pull-delete` → delete、`conflict` → conflict action
 *     （带 local/remote 全量内容供交互展示）；`push` / `push-delete` / `noop` 不产生动作。
 *   - merge 分类逐文件：三方 parse → `mergeJson`；任一键冲突 → **整文件**进 conflict
 *     （`keyPaths` = 冲突键清单，文件级保留本地）；无冲突且 merged ≠ local（或 local 缺失）→ write
 *     （`JSON.stringify(merged, null, 2) + '\n'`，确定性格式）；远端整文件删除且 local 未改 → delete；
 *     远端删除且 local 改过 → conflict（`local-modify-vs-remote-delete`）。远端相对 base 未变动 →
 *     不产生动作（local 的增删改都是 push 方向）。
 *   - merge 分类里 JSON 损坏 / 非 JSON 的条目（degraded）降级为 mirror 语义。
 *
 * 入参是**未剥离 excludeKeys** 的原始快照（plan 冻结签名），剥离在本函数内部完成。
 */
export function planPull(
  config: HomerConfig,
  base: AdapterSnapshot[],
  local: AdapterSnapshot[],
  remote: AdapterSnapshot[],
): PullPlan {
  const raw = { base: indexByAdapter(base), local: indexByAdapter(local), remote: indexByAdapter(remote) };
  const stripped = {
    base: indexByAdapter(base.map((snapshot) => stripSnapshotExcludeKeys(snapshot, config))),
    local: indexByAdapter(local.map((snapshot) => stripSnapshotExcludeKeys(snapshot, config))),
    remote: indexByAdapter(remote.map((snapshot) => stripSnapshotExcludeKeys(snapshot, config))),
  };

  const actions: PullAction[] = [];
  for (const adapterId of unionAdapterIds([...stripped.base.values()], [...stripped.local.values()], [...stripped.remote.values()])) {
    const ctx: AdapterContext = {
      adapterId,
      stripped: {
        base: stripped.base.get(adapterId),
        local: stripped.local.get(adapterId),
        remote: stripped.remote.get(adapterId),
      },
      rawLocal: raw.local.get(adapterId),
      rawRemote: raw.remote.get(adapterId),
      config,
    };

    for (const category of unionCategoryNames([ctx.stripped.base, ctx.stripped.local, ctx.stripped.remote])) {
      actions.push(...planCategory(ctx, category));
    }
  }

  return { actions };
}

interface AdapterContext {
  adapterId: string;
  stripped: { base: AdapterSnapshot | undefined; local: AdapterSnapshot | undefined; remote: AdapterSnapshot | undefined };
  rawLocal: AdapterSnapshot | undefined;
  rawRemote: AdapterSnapshot | undefined;
  config: HomerConfig;
}

/** 单个 classification（adapter × category）的全部 pull 动作。 */
function planCategory(ctx: AdapterContext, category: string): PullAction[] {
  const b = findCategory(ctx.stripped.base, category);
  const l = findCategory(ctx.stripped.local, category);
  const r = findCategory(ctx.stripped.remote, category);
  const mode = l?.mode ?? b?.mode ?? r?.mode;
  if (mode === undefined) return [];

  const baseFiles = b?.files ?? EMPTY_FILES;
  const localFiles = l?.files ?? EMPTY_FILES;
  const remoteFiles = r?.files ?? EMPTY_FILES;

  // 冲突展示用的原始内容（剥离前的真实文件）；缺条目 → undefined
  const rawLocalFiles = findCategory(ctx.rawLocal, category)?.files ?? EMPTY_FILES;
  const rawRemoteFiles = findCategory(ctx.rawRemote, category)?.files ?? EMPTY_FILES;

  const actions: PullAction[] = [];
  if (mode === 'mirror') {
    for (const op of compareCategory(baseFiles, localFiles, remoteFiles)) {
      const action = actionFromMirrorOp(
        op, ctx.adapterId, category,
        { judged: { local: localFiles, remote: remoteFiles }, display: { local: rawLocalFiles, remote: rawRemoteFiles } },
      );
      if (action !== undefined) actions.push(action);
    }
    return actions;
  }

  for (const relPath of unionPaths(baseFiles, localFiles, remoteFiles)) {
    const action = planMergeFile(
      ctx, category, relPath,
      {
        base: baseFiles.get(relPath),
        local: localFiles.get(relPath),
        remote: remoteFiles.get(relPath),
      },
      {
        local: rawLocalFiles.get(relPath),
        remote: rawRemoteFiles.get(relPath),
      },
    );
    if (action !== undefined) actions.push(action);
  }
  return actions;
}

/** 每个待判文件的两套取数：`judged`（剥离后，判定用）/ `display`（原始，冲突展示用）。 */
interface FileViews {
  judged: { local: SnapshotFiles; remote: SnapshotFiles };
  display: { local: SnapshotFiles; remote: SnapshotFiles };
}

/** mirror 语义的 op → 动作（push / push-delete / noop 无动作）。 */
function actionFromMirrorOp(
  op: MirrorOp,
  adapterId: string,
  category: string,
  views: FileViews,
): PullAction | undefined {
  switch (op.type) {
    case 'pull': {
      // write 内容取**剥离后** remote：store 里的 __REQUIRED__ 占位符不得流入工具目录。
      const remote = views.judged.remote.get(op.path);
      if (remote === undefined) return undefined; // 防御：pull 语义下 remote 必然存在
      return { type: 'write', adapterId, category, relPath: op.path, content: remote.content };
    }
    case 'pull-delete':
      return { type: 'delete', adapterId, category, relPath: op.path };
    case 'conflict':
      return mirrorConflictAction(adapterId, category, op.path, op.reason, views);
    case 'push':
    case 'push-delete':
    case 'noop':
      return undefined;
  }
}

/** mirror 冲突：带 local / remote 内容供 merge 交互展示（缺失侧该字段省略）。 */
function mirrorConflictAction(
  adapterId: string,
  category: string,
  relPath: string,
  reason: PullConflictAction['reason'],
  views: FileViews,
): PullConflictAction {
  const action: PullConflictAction = { type: 'conflict', adapterId, category, relPath, reason };
  const local = views.display.local.get(relPath) ?? views.judged.local.get(relPath);
  const remote = safeRemoteDisplay(views.display.remote.get(relPath), views.judged.remote.get(relPath));
  if (local !== undefined) action.localContent = local.content;
  if (remote !== undefined) action.remoteContent = remote.content;
  return action;
}

/**
 * 冲突展示用的「远端原文」：**仅当剥离不会改变它时**才给出。
 *
 * 不变量：`remoteContent` 一旦与剥离后内容不同，就说明它带着 store 里的 `__REQUIRED__` 占位符；
 * 下游（W9 merge）若做 `accept-remote` 而把 `remoteContent` 原样落盘，占位符就会流进工具目录。
 * 因此这种情形一律不给 `remoteContent`（此时「接受远端」的正确做法是重新决定而非照抄 store 文件）。
 * `localContent` 始终是**用户自己的真实文件**，不含占位符，照抄安全，故不受此限制。
 */
function safeRemoteDisplay(
  display: SnapshotEntry | undefined,
  judged: SnapshotEntry | undefined,
): SnapshotEntry | undefined {
  if (display === undefined || judged === undefined) return undefined;
  return display.content === judged.content ? display : undefined;
}

/** merge 分类里单文件的三方判定（返回 undefined = 无动作）。 */
function planMergeFile(
  ctx: AdapterContext,
  category: string,
  relPath: string,
  judged: { base: SnapshotEntry | undefined; local: SnapshotEntry | undefined; remote: SnapshotEntry | undefined },
  display: { local: SnapshotEntry | undefined; remote: SnapshotEntry | undefined },
): PullAction | undefined {
  const { adapterId } = ctx;

  // degraded（非 JSON / 解析失败 / 根值非对象）→ 降级 mirror 语义：
  //   kind:'file' 与解析失败与 computeDrift 的降级口径逐字一致；根值非对象（数组 / 标量）也走
  //   同一路 —— mergeJson 的冻结 `merged` 是 Record，键级合并无法表达，强行返回会把用户文件
  //   覆写成 `{}`（数据丢失）。降级后要么不动作、要么整文件覆盖，永不丢数据。
  if (isDegraded(judged.base) || isDegraded(judged.local) || isDegraded(judged.remote)) {
    return actionFromMirrorOp(
      compareFile(judged.base, judged.local, judged.remote, relPath),
      adapterId, category,
      {
        judged: {
          local: filesOf(judged.local, relPath),
          remote: filesOf(judged.remote, relPath),
        },
        display: {
          local: filesOf(display.local ?? judged.local, relPath),
          remote: filesOf(display.remote ?? judged.remote, relPath),
        },
      },
    );
  }

  // 双删 = 收敛，无事
  if (judged.local === undefined && judged.remote === undefined) return undefined;

  // 远端相对 base 未变动 → local 的增删改都是 push 方向，pull 不做任何事
  if (!remoteChanged(judged.base, judged.remote)) return undefined;

  // base 缺失（首次对接 / 远端新文件）：沿用 engine §2.7 降级矩阵，保证与 computeDrift 同结论
  if (judged.base === undefined) {
    const remote = judged.remote;
    if (judged.local === undefined) {
      // 仅远端有 → pull 新文件（write 内容取剥离后 remote，占位符不流入）
      return { type: 'write', adapterId, category, relPath, content: remote?.content ?? '' };
    }
    if (remote === undefined) return undefined; // 仅 local 有 → push，无 pull 动作
    if (judged.local.content === remote.content) return undefined;
    return {
      type: 'conflict', adapterId, category, relPath, reason: 'modify-vs-modify',
      localContent: (display.local ?? judged.local).content,
      remoteContent: safeRemoteDisplay(display.remote, remote)?.content,
    };
  }

  // 远端整文件删除（base 存在）：local 未改 → 删除生效；local 改过 → 真歧义（modify-vs-delete）
  if (judged.remote === undefined) {
    if (judged.local === undefined) return undefined; // 双删（已在上方短路，防御）
    const localDiff = diffJson(parseJsonContent(judged.base.content), parseJsonContent(judged.local.content));
    if (localDiff.keys.length === 0) return { type: 'delete', adapterId, category, relPath };
    return {
      type: 'conflict', adapterId, category, relPath, reason: 'local-modify-vs-remote-delete',
      // remoteContent 省略：远端已删除该文件，没有可供「接受远端」落盘的内容
      localContent: (display.local ?? judged.local).content,
    };
  }

  const remote = judged.remote;

  // local 整文件删除 + remote 改动 → 冲突（删除 vs 改值是真歧义，不静默复活 / 不静默丢）
  if (judged.local === undefined) {
    return {
      type: 'conflict', adapterId, category, relPath, reason: 'local-delete-vs-remote-modify',
      remoteContent: safeRemoteDisplay(display.remote, remote)?.content,
    };
  }

  const local = judged.local;

  // 三方俱在且远端确实改了 → 键级三路合并
  const result = mergeJson(
    parseJsonContent(judged.base.content),
    parseJsonContent(local.content),
    parseJsonContent(remote.content),
  );
  if (result.conflicts.length > 0) {
    // 任一键冲突 → 整文件进 conflict（文件级保留本地），keyPaths 供命令层展示。
    // 刻意不给 localContent / remoteContent：merge 分类是**键级**裁决，凭整文件内容照抄落盘
    // 会把 store 里的 `__REQUIRED__` 占位符写进工具目录（plan §2.5 只要求 keyPaths）。
    return {
      type: 'conflict', adapterId, category, relPath, reason: 'merge-keys',
      keyPaths: result.conflicts.map((conflict) => conflict.keyPath),
    };
  }

  // 植回 local **原文件**中的 excluded 键值（本地密钥永不被远端覆盖；local 缺该键 → 不植回）
  const planted = plantExcludedKeys(
    result.merged,
    parseJsonContent((display.local ?? local).content),
    excludedKeysFor(ctx.config, adapterId, category),
  );

  // 「merged ≠ local（或 local 缺失）」→ write。比较用**语义等价**（键序无关），
  // 避免用户自己格式化的 JSON（缩进 / 键序不同）被误判成变更而产生无意义重写。
  if (!jsonEqual(planted, parseJsonContent((display.local ?? local).content))) {
    return { type: 'write', adapterId, category, relPath, content: serializeJsonContent(planted) };
  }
  return undefined;
}

/** 远端相对 base 是否变动（字符串全等口径，与 mirror / drift 一致）。 */
function remoteChanged(b: SnapshotEntry | undefined, r: SnapshotEntry | undefined): boolean {
  if (b === undefined) return r !== undefined;   // 远端新增
  if (r === undefined) return true;              // 远端删除
  return r.content !== b.content;
}

/* ------------------------------------------------------------------ */
/* checkPushSafety                                                     */
/* ------------------------------------------------------------------ */

export interface PushCheck {
  status: 'ok' | 'remote-ahead' | 'conflicts';
  /** status='ok' 时：本次 push 将写入 store 的文件清单（mirror push ops + merge changedKeys 的文件集合）。 */
  changedFiles: { adapterId: string; category: string; relPath: string }[];
  /** status='conflicts' 时：需要 `homer merge` 交互裁决的冲突项。 */
  conflictItems: PullConflictAction[];
  /** status='remote-ahead' 时：远端会覆盖 / 删除的本地文件。 */
  remoteAheadFiles: { adapterId: string; category: string; relPath: string }[];
}

/**
 * push 前的安全检查：只要远端有本地未见的变更就不能推。
 *
 * 判定基于 `computeDrift`（先做与 `homer status` 同口径的 excludeKeys 剥离）：
 *   任一分类 `pull > 0` → `'remote-ahead'`；否则任一分类 `conflicts > 0` → `'conflicts'`；否则 `'ok'`。
 * 清单字段按状态填充（与 `planPull` 共用同一判定，故 pull 计划与状态永远自洽）。
 */
export function checkPushSafety(
  config: HomerConfig,
  base: AdapterSnapshot[],
  local: AdapterSnapshot[],
  remote: AdapterSnapshot[],
): PushCheck {
  const stripped = {
    base: base.map((snapshot) => stripSnapshotExcludeKeys(snapshot, config)),
    local: local.map((snapshot) => stripSnapshotExcludeKeys(snapshot, config)),
    remote: remote.map((snapshot) => stripSnapshotExcludeKeys(snapshot, config)),
  };

  const drifts = computeDrift(stripped.base, stripped.local, stripped.remote);
  let totalPull = 0;
  let totalConflicts = 0;
  for (const adapter of drifts) {
    for (const category of adapter.categories) {
      totalPull += category.pull;
      totalConflicts += category.conflicts;
    }
  }

  if (totalPull > 0) {
    const plan = planPull(config, base, local, remote);
    return {
      status: 'remote-ahead',
      changedFiles: [],
      conflictItems: [],
      remoteAheadFiles: remoteDrivenFiles(plan.actions),
    };
  }

  if (totalConflicts > 0) {
    const plan = planPull(config, base, local, remote);
    return {
      status: 'conflicts',
      changedFiles: [],
      conflictItems: plan.actions.filter(isConflictAction),
      remoteAheadFiles: [],
    };
  }

  return {
    status: 'ok',
    changedFiles: pushedFiles(drifts, [stripped.base, stripped.local, stripped.remote]),
    conflictItems: [],
    remoteAheadFiles: [],
  };
}

interface FileRef { adapterId: string; category: string; relPath: string }

function isConflictAction(action: PullAction): action is PullConflictAction {
  return action.type === 'conflict';
}

/**
 * 远端会触碰本地哪些文件：write / delete 动作即远端覆盖 / 删除的目标。
 * 若 pull 计数全部来自「跳过保留本地」的冲突（无 write/delete 动作）则退回冲突文件清单，
 * 保证 status='remote-ahead' 时 remoteAheadFiles 不为空（否则用户看不到「卡在哪」）。
 */
function remoteDrivenFiles(actions: readonly PullAction[]): FileRef[] {
  const direct = dedupFiles(
    actions.filter((action) => action.type === 'write' || action.type === 'delete').map(fileRefOf),
  );
  if (direct.length > 0) return direct;
  return dedupFiles(actions.filter(isConflictAction).map(fileRefOf));
}

/**
 * 本次 push 将写入 store 的文件清单：
 *   - mirror 分类（含 merge 降级文件）→ `push` / `push-delete` op 的路径；
 *   - merge 分类 → `changedKeys`（drift 冻结格式 `<relPath>:<keyPath>`）的 relPath 集合。
 *
 * `<relPath>` 可能自带 ':'（如 `a:b.json`），故不按首个 ':' 硬切，而是拿该分类的已知文件
 * 路径集合做**最长前缀匹配**（路径来自剥离后三方快照的并集，必然覆盖 changedKeys 的 relPath）。
 */
function pushedFiles(drifts: readonly AdapterDrift[], snapshotGroups: AdapterSnapshot[][]): FileRef[] {
  const knownPaths = knownPathsIndex(snapshotGroups);
  const out: FileRef[] = [];

  for (const adapter of drifts) {
    for (const category of adapter.categories) {
      const base = { adapterId: adapter.adapterId, category: category.category };
      const paths = knownPaths.get(indexKey(adapter.adapterId, category.category)) ?? [];

      for (const op of category.ops) {
        if (op.type === 'push' || op.type === 'push-delete') out.push({ ...base, relPath: op.path });
      }
      for (const changedKey of category.changedKeys) {
        const relPath = matchKnownPath(changedKey, paths);
        if (relPath !== undefined) out.push({ ...base, relPath });
      }
    }
  }
  return dedupFiles(out);
}

/** `adapterId\0category` → 该分类在三方快照里的全部 relPath（长路径优先，供前缀匹配）。 */
function knownPathsIndex(groups: AdapterSnapshot[][]): Map<string, string[]> {
  const index = new Map<string, string[]>();
  for (const group of groups) {
    for (const adapter of group) {
      for (const category of adapter.categories) {
        const key = indexKey(adapter.adapterId, category.category);
        const existing = index.get(key);
        if (existing === undefined) {
          index.set(key, [...category.files.keys()]);
          continue;
        }
        for (const path of category.files.keys()) {
          if (!existing.includes(path)) existing.push(path);
        }
      }
    }
  }
  for (const paths of index.values()) paths.sort((a, b) => b.length - a.length);
  return index;
}

/** `<relPath>:<keyPath>` → relPath（最长前缀匹配；无匹配 → undefined，不猜）。 */
function matchKnownPath(changedKey: string, paths: readonly string[]): string | undefined {
  return paths.find((path) => changedKey.startsWith(`${path}:`));
}

function indexKey(adapterId: string, category: string): string {
  return `${adapterId}\u0000${category}`;
}

function fileRefOf(action: PullAction): FileRef {
  return { adapterId: action.adapterId, category: action.category, relPath: action.relPath };
}

function dedupFiles(refs: readonly FileRef[]): FileRef[] {
  const seen = new Set<string>();
  const out: FileRef[] = [];
  for (const ref of refs) {
    const key = `${indexKey(ref.adapterId, ref.category)}\u0000${ref.relPath}`;
    if (seen.has(key)) continue;
    seen.add(key);
    out.push(ref);
  }
  return out;
}

/* ------------------------------------------------------------------ */
/* 集合 / 解析工具                                                     */
/* ------------------------------------------------------------------ */

const EMPTY_FILES: SnapshotFiles = new Map<string, SnapshotEntry>();

/** 单条目 → 单文件表（mirror 降级路径按文件粒度复用 compareFile 的结果）。 */
function filesOf(entry: SnapshotEntry | undefined, relPath: string): SnapshotFiles {
  return entry === undefined ? EMPTY_FILES : new Map([[relPath, entry]]);
}

/**
 * degraded（交给 mirror 语义）判定：
 *   - `kind === 'file'`（JSON 损坏 / 本就不是 JSON）与解析失败 —— 与 `computeDrift` 的降级口径逐字一致；
 *   - 或根值不是 JSON 对象（数组 / 标量）：`mergeJson` 的冻结 `merged` 是 Record，键级合并无法表达，
 *     强行返回会把用户文件覆写成 `{}`。降级 mirror 既不丢数据，pull / conflict 的分类也与 drift 同侧。
 */
function isDegraded(entry: SnapshotEntry | undefined): boolean {
  if (entry === undefined) return false;
  if (entry.kind === 'file') return true;
  return !isJsonObject(parseJsonContent(entry.content));
}

/**
 * JSON 值语义相等（键序无关）。
 * 用于「merge 结果是否真的需要写回」——local 文件由用户 / 工具写出，格式未必是我们的
 * canonical 序列化；若 merged 与 local **语义**相同就无需产生（会引入无意义重写的）write 动作。
 */
function jsonEqual(a: unknown, b: unknown): boolean {
  if (Object.is(a, b)) return true;
  if (Array.isArray(a) && Array.isArray(b)) {
    return a.length === b.length && a.every((item, index) => jsonEqual(item, b[index]));
  }
  if (isJsonObject(a) && isJsonObject(b)) {
    const keysA = Object.keys(a);
    const keysB = Object.keys(b);
    if (keysA.length !== keysB.length) return false;
    return keysA.every(
      (key) => Object.prototype.hasOwnProperty.call(b, key) && jsonEqual(a[key], b[key]),
    );
  }
  return false;
}

function unionAdapterIds(...groups: AdapterSnapshot[][]): string[] {
  const seen = new Set<string>();
  const ids: string[] = [];
  for (const group of groups) {
    for (const snapshot of group) {
      if (seen.has(snapshot.adapterId)) continue;
      seen.add(snapshot.adapterId);
      ids.push(snapshot.adapterId);
    }
  }
  return ids;
}

function unionCategoryNames(adapters: (AdapterSnapshot | undefined)[]): string[] {
  const seen = new Set<string>();
  const names: string[] = [];
  for (const adapter of adapters) {
    if (adapter === undefined) continue;
    for (const category of adapter.categories) {
      if (seen.has(category.category)) continue;
      seen.add(category.category);
      names.push(category.category);
    }
  }
  return names;
}

function unionPaths(...fileSets: SnapshotFiles[]): string[] {
  const seen = new Set<string>();
  for (const files of fileSets) for (const path of files.keys()) seen.add(path);
  return [...seen].sort();
}

function findCategory(adapter: AdapterSnapshot | undefined, category: string): CategorySnapshot | undefined {
  return adapter?.categories.find((item) => item.category === category);
}
