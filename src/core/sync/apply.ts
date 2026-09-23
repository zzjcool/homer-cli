/**
 * 把 pull 动作计划落到用户工具目录（docs/m2-plan.md §2.5，P2-W6）。
 *
 * 冻结签名：`applyPullActions(paths, config, plan, opts?): ApplyResult`
 *
 * 本模块是**唯一**会写用户工具目录的地方，故规则刻意朴素且可审计：
 *   - `write`  → 目标文件已存在则**先备份**，再 `mkdir -p` 父目录 → 写内容；
 *   - `delete` → 已存在则先备份，再 `rm`，随后**清理空父目录**（逐级向上，止于 category 目录根之前）；
 *   - `conflict` → **跳过，保留本地**，只记入 `result.conflicts`（逐项裁决在 `homer merge` 做）；
 *   - `opts.backup` 默认 `true`；`false` = 完全不创建 `backups/`（纯测试 / dry 场景）。
 *
 * 备份标签（`BackupTarget.label`）统一为 `<adapterId>/<category>/<relPath>`：既是 store 相对路径的
 * 自然形式，也天然满足 `backupFiles` 的「禁 `'..'` / 禁绝对路径」约束（`resolveCategoryFilePath`
 * 已先行校验，此处是双保险）。
 *
 * ## 父目录清理的边界（风险清单第 1 条：绝不污染用户真实目录）
 *
 * 只清理**本次删除造成的空目录链**，且**止于 category 目录根之前**：
 * `skills/foo/SKILL.md` 被删后 `skills/foo` 若为空则删掉，但 `skills/`（category 根）保留。
 * 单文件型 category（如 `settings.json`）的目标父目录就是 adapter root，故**不做任何清理**。
 */

import fs from 'node:fs';
import path from 'node:path';

import { backupFiles, type BackupTarget } from '../backup/backup.js';
import { CliError } from '../errors.js';
import { expandHome, type HomerPaths } from '../paths.js';
import { resolveCategoryFilePath } from '../../adapters/paths.js';
import type { AdapterConfig, CategoryConfig, HomerConfig } from '../types.js';
import type {
  ApplyResult,
  PullAction,
  PullConflictAction,
  PullDeleteAction,
  PullPlan,
  PullWriteAction,
} from './types.js';

export interface ApplyPullActionsOptions {
  /** 是否在覆盖 / 删除前备份已存在的本地文件（默认 `true`）。 */
  backup?: boolean;
}

/** 备份目录名里的命令分量（applyPullActions 的冻结签名没有 command 参数，故固定为 `pull`）。 */
const BACKUP_COMMAND = 'pull';

/** 动作 → adapter / category 配置；缺失 → undefined。 */
function configOf(
  config: HomerConfig,
  adapterId: string,
  category: string,
): { adapter: AdapterConfig; category: CategoryConfig } | undefined {
  const adapter = config.adapters[adapterId];
  if (adapter === undefined) return undefined;
  const categoryConfig = adapter.categories[category];
  if (categoryConfig === undefined) return undefined;
  return { adapter, category: categoryConfig };
}

/** 备份标签：store 相对路径形式。 */
function labelOf(adapterId: string, category: string, relPath: string): string {
  return `${adapterId}/${category}/${relPath}`;
}

/** 目录型 path（以 '/' 结尾）→ true（与 scan.ts / adapters/paths.ts 同义）。 */
function isDirPath(p: string): boolean {
  return p.endsWith('/');
}

/**
 * 该 relPath 所属 category 的**目录根**（清理空父目录的边界，本身不删）。
 *   - 目录型 path → `<root>/<dir>`；
 *   - 单文件型 path → `undefined`（文件直接躺在 root 下，无可清理的中间目录）。
 * 声明顺序里**最后一个**匹配者与 `resolveCategoryFilePath` 的裁定一致。
 */
function categoryDirBoundary(
  root: string,
  categoryConfig: CategoryConfig,
  relPath: string,
): string | undefined {
  let boundary: string | undefined;
  for (const declared of categoryConfig.paths) {
    if (isDirPath(declared)) {
      const dir = declared.replace(/\/+$/, '');
      if (dir === '') continue;
      boundary = path.join(root, dir);
      continue;
    }
    const base = declared.slice(declared.lastIndexOf('/') + 1);
    if (base === relPath) boundary = undefined; // 单文件型命中 → 无中间目录
  }
  return boundary;
}

/** 目录是否为空（不存在 / 不可读 → 视为非空，宁保守不删）。 */
function isDirEmpty(dir: string): boolean {
  try {
    return fs.readdirSync(dir).length === 0;
  } catch {
    return false;
  }
}

/**
 * 逐级删除空父目录，**在 `boundary` 处停手**（`boundary` 本身永不删）。
 * `boundary === undefined`（单文件型）→ 无事可做。
 */
function pruneEmptyParents(from: string, boundary: string | undefined): void {
  if (boundary === undefined) return;
  let current = from;
  while (current !== boundary && isUnderOrEqual(current, boundary)) {
    if (!isDirEmpty(current)) return;
    fs.rmdirSync(current);
    const parent = path.dirname(current);
    if (parent === current) return;
    current = parent;
  }
}

/** `child` 是否等于 `parent` 或位于其下。 */
function isUnderOrEqual(child: string, parent: string): boolean {
  if (child === parent) return true;
  const prefix = parent.endsWith(path.sep) ? parent : `${parent}${path.sep}`;
  return child.startsWith(prefix);
}

/**
 * 解析后的单个动作：把 `conflict`（不碰磁盘）与需要落盘的动作（write / delete）分开，
 * 并把 target 绝对路径与清理边界预先算好。**解析阶段零磁盘写入**，
 * 于是「config 失配 / relPath 逃逸」这类编程错误在**任何写操作之前**就整批抛掉。
 */
type ResolvedAction =
  | { kind: 'conflict'; action: PullConflictAction }
  | { kind: 'write'; action: PullWriteAction; target: string }
  | { kind: 'delete'; action: PullDeleteAction; target: string; boundary: string | undefined };

/** 解析单个动作（纯计算，不碰磁盘）。 */
function resolveAction(config: HomerConfig, action: PullAction): ResolvedAction {
  if (action.type === 'conflict') return { kind: 'conflict', action };

  const resolvedConfig = configOf(config, action.adapterId, action.category);
  if (resolvedConfig === undefined) {
    throw new CliError(
      `applyPullActions: ${action.adapterId}/${action.category} 不在 homer.json 中（plan 与 config 失配）`,
      '请重新运行 `homer pull` 生成与当前配置一致的计划。',
    );
  }

  // `'~'` 展开：scan 侧用 expandHome 展开过 config.root，这里必须同规则，否则映射到错误的盘位。
  const root = expandHome(resolvedConfig.adapter.root);
  const target = resolveCategoryFilePath(root, resolvedConfig.category, action.relPath);

  if (action.type === 'write') return { kind: 'write', action, target };
  return {
    kind: 'delete',
    action,
    target,
    boundary: categoryDirBoundary(root, resolvedConfig.category, action.relPath),
  };
}

/**
 * 应用 `plan.actions`（docs/m2-plan.md §2.5 冻结签名）。
 *
 * **三个阶段，顺序是语义的一部分**：
 *   1. **解析**（零磁盘写入）：逐动作解析 target / 清理边界，并校验 config 失配与 relPath 逃逸；
 *      任一非法 → 整批抛错，磁盘保持原样。
 *   2. **备份**（仅 `backup=true` 且有已存在的目标）：一次 `backupFiles` 调用覆盖全部
 *      将被覆盖 / 删除的文件。必须在阶段 3 **之前**完成，否则备份到的是新内容，
 *      「备份内容 == 覆盖前内容」这一不变式不成立。
 *   3. **应用**：按 plan 顺序 write / delete（conflict 只记录，保留本地）。
 *
 * 备份合并为一次调用（同一个 `<HHmmss>-pull` 目录），故 `result.backupDir` 唯一，
 * 用户能在一处找回本轮所有被覆盖 / 删除的文件。需要备份的目标为空
 * （全新增 / 全 conflicts / `backup=false`）→ 不创建 `backups/`，`backupDir` 保持 undefined。
 */
export function applyPullActions(
  paths: HomerPaths,
  config: HomerConfig,
  plan: PullPlan,
  opts?: ApplyPullActionsOptions,
): ApplyResult {
  const backup = opts?.backup !== false; // 默认 true

  const result: ApplyResult = { written: [], deleted: [], conflicts: [] };

  // ---- 阶段 1：解析（不写盘）----
  const resolved = plan.actions.map((action) => resolveAction(config, action));

  // ---- 阶段 2：备份（先于任何写 / 删）----
  const backupTargets: BackupTarget[] = [];
  if (backup) {
    for (const item of resolved) {
      if (item.kind === 'conflict') continue;
      if (!fs.existsSync(item.target)) continue; // 新增 / 本就不存在 → 无内容可备份
      const { adapterId, category, relPath } = item.action;
      backupTargets.push({ sourceAbs: item.target, label: labelOf(adapterId, category, relPath) });
    }
    if (backupTargets.length > 0) {
      // 备份必须先落盘（阶段 3 之前），否则备份到的是新内容。
      result.backupDir = backupFiles(paths, BACKUP_COMMAND, backupTargets).backupDir;
    }
  }

  // ---- 阶段 3：应用 ----
  for (const item of resolved) {
    const { adapterId, category, relPath } = item.action;

    if (item.kind === 'conflict') {
      result.conflicts.push({ adapterId, category, relPath });
      continue;
    }

    if (item.kind === 'write') {
      fs.mkdirSync(path.dirname(item.target), { recursive: true });
      fs.writeFileSync(item.target, item.action.content, 'utf8');
      result.written.push({ adapterId, category, relPath });
      continue;
    }

    // delete：源不存在时无需 rm，但仍计入 deleted（计划说它不该再存在）。
    if (fs.existsSync(item.target)) {
      fs.rmSync(item.target, { force: true });
      pruneEmptyParents(path.dirname(item.target), item.boundary);
    }
    result.deleted.push({ adapterId, category, relPath });
  }

  return result;
}
