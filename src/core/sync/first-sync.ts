/**
 * 首次对接（first contact）计划 —— docs/m3-plan.md §2.4（P1-W5，纯函数、零 fs）。
 *
 * `homer home <repo-url>` 的第六/七步（§2.7）：clone 完配置仓库、扫完本机现状之后，
 * 用户对新机器做一次「Pull / Merge / Skip」三选一，本模块把选择翻译成**逐文件动作清单**，
 * 命令层（W8）只负责预览、备份、应用。
 *
 * ## 三模式语义矩阵（冻结，§2.4 / D5）
 *
 * | mode    | 结果                                                                             |
 * |---------|----------------------------------------------------------------------------------|
 * | `pull`  | remote 每个文件 → `write`（含双方都有且不同的项，content = remote 内容）；         |
 * |         | local-only 文件**不产生任何动作**（空 base 下「删除」无依据）；无 delete。         |
 * | `merge` | remote-only → `write`；local-only → 无动作；双方都有 → 语义见下；无 delete。      |
 * | `skip`  | `actions: []`（不应用）。                                                        |
 *
 * ## 「无 base」的落地：mirror 走并集 / merge 走键级并集（两份权威文本的收口）
 *
 * DESIGN §2.7「首次同步（无 base）」原文：
 *   > 选 Merge 且无 base 时，merge 退化为**并集合并 + 同键冲突列表**（VS Code 首次对接同款），
 *   > mirror 退化为并集 + 同路径不同内容进冲突列表。
 *
 * 于是两条路径的「空 base」表示不同，这是本模块唯一需要解释的机制：
 *   - **mirror 分类**：base 保持**空 Map**（W4/M1 裁定的「缺目录 → 空 Map」语义）→
 *     `planPull` 走 `compareFile` 的「base 缺失」分支 = 路径并集：仅 remote → write、
 *     仅 local → 无动作、同路径不同内容 → conflict（file 级）。
 *   - **merge 分类**：双方都存在的 JSON 对象文件，base 取**空对象 `{}`** → `planPull` 走
 *     `mergeJson` 键级三路合并 = **键级并集**：不同键自动并集（无冲突）、同键不同值 →
 *     conflict（`reason='merge-keys'` + `keyPaths`），local 独有键保留。
 *
 * `plan §2.4` 的 merge 条目写作「`planPull(config, emptyBase, local, remote)` 原样」，并紧接着
 * 括注「merge JSON 键级并集冲突进 conflict action」；§3-P1-W5 验收进一步要求
 * 「键级并集冲突 keyPaths 正确」。但**逐字**传空 Map base 时 `planPull` 会把 merge 分类降级为
 * mirror 文件级冲突（`modify-vs-modify`，**不带 keyPaths**，见 `plan.ts` 的 base 缺失分支），
 * 与括注 / §3 验收 / DESIGN §2.7 三处相冲突（实现前已实测确认）。
 * 本模块因此按 DESIGN §2.7 + §3 验收落地：merge 分类仍**经 `planPull` 原样判定**（不复制
 * 合并逻辑、不私改 `plan.ts`），只把「无 base」在 merge 分类上表示为空对象基线。
 * 该机制裁定已在 W5 报告中显式上报 orchestrator。
 *
 * 依赖面（§2.4 冻结）：`types.js`（纯类型）、`./types.js`、`./plan.js`（同目录 W4 判定层）、
 * `./excluded-keys.js`（JSON 形态共享判定）。零 fs、零 CLI、零进程副作用。
 */

import { isJsonObject, parseJsonContent } from './excluded-keys.js';
import { planPull } from './plan.js';
import type { AdapterSnapshot, CategorySnapshot, HomerConfig, SnapshotEntry, SnapshotFiles } from '../types.js';
import type { PullAction } from './types.js';

/* ------------------------------------------------------------------ */
/* 类型（§2.4 冻结）                                                   */
/* ------------------------------------------------------------------ */

/** 首次对接模式（§2.4；`homer home --mode` 的取值集合，与 `cli/index.ts` 同集合）。 */
export type FirstContactMode = 'pull' | 'merge' | 'skip';

/** 首次对接计划：模式回填 + 待应用的 pull 动作清单（与 `PullPlan` 同构）。 */
export interface FirstContactPlan {
  mode: FirstContactMode;
  actions: PullAction[];
}

/* ------------------------------------------------------------------ */
/* 空 base                                                             */
/* ------------------------------------------------------------------ */

/**
 * 按 **config 结构**生成全空 base 快照（§2.4 / D5「base = 按 config 结构的空 Map」）。
 *
 * 结构对齐口径与 `readSnapshotFromStore` / `scanAdapter` 逐条同构，保证三方判定与
 * `homer status` 同源：
 *   - 顺序 = config 中 `adapters` / `categories` 的声明顺序；
 *   - 跳过 `enabled === false` 的 adapter 与 category（它们不参与同步，也不出现在任何快照里）；
 *   - 每个分类一个**空 Map**（M1 裁定语义：缺目录 = 该分类 base 无内容，local 新增据此判为 push）；
 *   - `mode` 从 config 原样带出，用于给 `planPull` 钉死 mirror / merge 判定分支。
 */
export function emptyBaseSnapshots(config: HomerConfig): AdapterSnapshot[] {
  const snapshots: AdapterSnapshot[] = [];

  for (const [adapterId, adapter] of Object.entries(config.adapters)) {
    if (adapter.enabled === false) continue;

    const categories: CategorySnapshot[] = [];
    for (const [categoryName, category] of Object.entries(adapter.categories)) {
      if (category.enabled === false) continue;
      categories.push({
        adapterId,
        category: categoryName,
        mode: category.mode,
        files: new Map<string, SnapshotEntry>(),
      });
    }

    snapshots.push({ adapterId, categories });
  }

  return snapshots;
}

/* ------------------------------------------------------------------ */
/* planFirstContact                                                    */
/* ------------------------------------------------------------------ */

/**
 * 首次对接计划：把三选一模式翻译成 pull 动作清单（§2.4 语义矩阵见文件头）。
 *
 * `local` 是**本机现状**（未做 excludeKeys 剥离的原始快照），`remote` 是 clone 后的
 * store 工作区（== HEAD）；剥离 / 键级判定由 `planPull` 内部按 M2 口径完成。
 */
export function planFirstContact(
  config: HomerConfig,
  local: AdapterSnapshot[],
  remote: AdapterSnapshot[],
  mode: FirstContactMode,
): FirstContactPlan {
  if (mode === 'skip') return { mode, actions: [] };

  if (mode === 'pull') {
    // 远端全量覆盖：local **不参与判定**——传空 local 后「远端每个文件」都成为 remote-only，
    // 于是 `planPull` 逐文件产出 write（含双方都有且不同的项），且不可能产出 delete /
    // local-only 动作（它们都需要 local 侧的存在性）。write 内容仍是**剥离后**的 remote
    // 原文，故 store 里的 `__REQUIRED__` 占位符永不流入工具目录（M2 不变量）。
    return { mode, actions: planPull(config, emptyBaseSnapshots(config), [], remote).actions };
  }

  // merge：空 base 在 merge 分类上表示为「空对象基线」→ `planPull` 原样给出键级并集合并
  // （remote-only → write、local-only → 无动作、同键冲突 → conflict `merge-keys` + keyPaths）。
  return { mode, actions: planPull(config, mergeBaseSnapshots(config, local, remote), local, remote).actions };
}

/* ------------------------------------------------------------------ */
/* merge 的「无 base」基线                                             */
/* ------------------------------------------------------------------ */

/** adapterId → 快照（只用于本文件的 merge 基线构造）。 */
function indexByAdapter(snapshots: readonly AdapterSnapshot[]): Map<string, AdapterSnapshot> {
  const index = new Map<string, AdapterSnapshot>();
  for (const snapshot of snapshots) index.set(snapshot.adapterId, snapshot);
  return index;
}

/** 取某 adapter 某分类的 files（缺 adapter / 分类 → undefined，不造空 Map）。 */
function findFiles(adapter: AdapterSnapshot | undefined, category: string): SnapshotFiles | undefined {
  return adapter?.categories.find((item) => item.category === category)?.files;
}

/**
 * merge 模式的 base：`emptyBaseSnapshots(config)` + 对 **双方都存在且都是 JSON 对象** 的
 * merge 分类文件补一个空对象 `{}` 条目（DESIGN §2.7「并集合并」的基线表示）。
 *
 * 三个刻意的边界：
 *   - 只补 `mode === 'merge'` 的分类：mirror 分类保持空 Map，维持「路径并集」语义；
 *   - 只补**双方都在**的文件：仅 remote 有 → base 缺失 → `planPull` 给 write；
 *     仅 local 有 → base 缺失 → 无动作（push 方向）。若给双方都补，remote-only 会被
 *     `planPull` 误判成「local 删除 vs remote 改」冲突；
 *   - 只补**可参与键级合并**的两侧（与 `planPull` 的 `isDegraded` 互补：`kind === 'file'`、
 *     解析失败、根值为数组 / 标量一律排除）：degraded 文件保持 base 缺失 → 由 `planPull`
 *     降级为 mirror 并集语义（同内容 = 无动作，不同内容 = file 级冲突），不把降级文件
 *     强行送进 `mergeJson`（那会把数组 / 标量类文件覆写成 `{}`，数据丢失）。
 */
function mergeBaseSnapshots(
  config: HomerConfig,
  local: readonly AdapterSnapshot[],
  remote: readonly AdapterSnapshot[],
): AdapterSnapshot[] {
  const localIndex = indexByAdapter(local);
  const remoteIndex = indexByAdapter(remote);

  return emptyBaseSnapshots(config).map((snapshot) => ({
    adapterId: snapshot.adapterId,
    categories: snapshot.categories.map((category) => {
      if (category.mode !== 'merge') return category;

      const localFiles = findFiles(localIndex.get(snapshot.adapterId), category.category);
      const remoteFiles = findFiles(remoteIndex.get(snapshot.adapterId), category.category);
      if (localFiles === undefined || remoteFiles === undefined) return category;

      const files = new Map<string, SnapshotEntry>();
      for (const [relPath, localEntry] of localFiles) {
        const remoteEntry = remoteFiles.get(relPath);
        if (remoteEntry === undefined) continue;
        if (!isMergeableEntry(localEntry) || !isMergeableEntry(remoteEntry)) continue;
        files.set(relPath, emptyJsonObjectEntry());
      }
      if (files.size === 0) return category;
      return { ...category, files };
    }),
  }));
}

/** 条目是否「可参与键级合并」（与 `planPull` 的 `isDegraded` **逐条互补**，防两处口径漂移）。 */
function isMergeableEntry(entry: SnapshotEntry): boolean {
  if (entry.kind === 'file') return false;
  return isJsonObject(parseJsonContent(entry.content));
}

/** 空对象基线条目（每次新造，避免共享可变对象）。 */
function emptyJsonObjectEntry(): SnapshotEntry {
  return { kind: 'json', content: '{}' };
}
