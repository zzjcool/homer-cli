/**
 * 应用远端变更前的本地文件备份 + 备份保留策略（docs/m2-plan.md §2.3，P1-W2）。
 *
 * 冻结签名：
 *   BackupTarget / BackupResult
 *   backupFiles(paths, command, targets): BackupResult
 *   pruneBackups(paths, keep?): { removed: string[]; kept: string[] }
 *
 * 语义（计划原文）：
 *   - 目录布局：`<backupsDir>/<YYYYMMDD>/<HHmmss>-<command>/<label>`
 *     （`<label>` 形如 `pi/settings/settings.json`，保留 store 相对结构，便于人工核对与整体回填）。
 *   - 只备份**已存在**的源文件；源不存在的 target 记入 `skipped`（无需备份，调用方常直接跳过）。
 *   - `label` 必须是受控的相对路径（禁绝对路径 / 禁 `..` 段），否则 throw——备份目录由 label
 *     拼出绝对路径，label 一旦可逃逸就可能写到 `~` 或更远处（与 store.ts 的 assertSafeSegment 同因）。
 *     校验在**任何写操作之前**完成：非法 target 不会留下半套备份。
 *   - `pruneBackups`：按日期目录名（`YYYYMMDD`）降序保留前 `keep` 个，删除其余**整个日期目录**；
 *     `keep` 默认 7。返回的 `kept`/`removed` 是日期目录**名**（相对 `backupsDir`，非绝对路径）。
 *     backupsDir 不存在 → 空结果（幂等，不 throw）。非日期名的条目（例如人工遗留的临时目录）不参与保留策略。
 *
 * 备份不是同步内容的一部分（§1 决策 D5）：`backups/` 由 `~/.homer/.gitignore` 排除。
 */

import fs from 'node:fs';
import path from 'node:path';

import type { HomerPaths } from '../paths.js';

/** 一个待备份项：`sourceAbs` 为源文件绝对路径，`label` 为 store 相对路径形式的目标名。 */
export interface BackupTarget {
  sourceAbs: string;
  label: string; // 'pi/settings/settings.json'，禁 '..' / 绝对路径
}

/** `backedUp` / `skipped` 的元素均为对应 target 的 `label`。 */
export interface BackupResult {
  backupDir: string;
  backedUp: string[];
  skipped: string[]; // 源不存在（无需备份）
}

/** 日期目录名（`YYYYMMDD`），只有这种名字的目录参与保留策略。 */
const DATE_DIR_RE = /^\d{8}$/;

/**
 * 备份目录树的可选权限收紧（additive，对抗式 review M1）。
 *
 * `undefined` = 维持既有行为（系统默认权限，受 umask 影响）——普通配置备份（store 快照）
 * 无需收紧，其可读性还有调试价值。
 *
 * 密钥通道（`secret pull` / `home` 步骤 9）**必须**传 `{ dir: 0o700, file: 0o600 }`：
 * 被覆盖前的目标文件是**密钥明文**，D2 冻结「密钥明文只存在于 0600」——备份副本落在
 * 0755 目录 / 0644 文件里等于让同机其他用户读到上一版私钥。
 */
export interface BackupFileModes {
  /** 本函数创建的整个备份目录树（含 `backupsDir` 与日期 / 时间 / 标签父目录）的权限。 */
  dir?: number;
  /** 备份文件（含目录型源递归出的文件）的权限（copy 完成后显式 `chmod`）。 */
  file?: number;
}

/** `backupFiles` 的可选参数（additive；缺省 = 与冻结签名逐字相同的行为）。 */
export interface BackupFilesOptions {
  /** 权限收紧（见 `BackupFileModes`）；缺省不改变任何权限。 */
  mode?: BackupFileModes;
}

/** 默认保留最近 7 个日期目录（§2.0-1 `backup.keep` 缺省值）。 */
export const DEFAULT_BACKUP_KEEP = 7;

function two(n: number): string {
  return String(n).padStart(2, '0');
}

/** 本地时区的 `YYYYMMDD`（目录名与用户 `ls` 时看到的日期一致，故不用 UTC）。 */
function dateStamp(now: Date): string {
  return `${now.getFullYear()}${two(now.getMonth() + 1)}${two(now.getDate())}`;
}

/** 本地时区的 `HHmmss`。 */
function timeStamp(now: Date): string {
  return `${two(now.getHours())}${two(now.getMinutes())}${two(now.getSeconds())}`;
}

function assertSafeSegment(segment: string, what: string): void {
  if (segment.length === 0) throw new Error(`${what} 不能为空`);
  if (path.isAbsolute(segment) || segment === '..' || segment.startsWith(`..${path.sep}`)) {
    throw new Error(`${what} 非法（不得为绝对路径或包含 '..'）: ${segment}`);
  }
  if (segment.split(/[\\/]/).includes('..')) {
    throw new Error(`${what} 非法（不得包含 '..' 段）: ${segment}`);
  }
}

/** `chmod`，源在竞态下已被删（ENOENT）→ 跳过；其它错误照旧抛出（权限收紧失败必须让调用方知道）。 */
function chmodIfPresent(abs: string, mode: number): void {
  try {
    fs.chmodSync(abs, mode);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return;
    throw err;
  }
}

/**
 * 对 `root` 及其下整棵树施加权限收紧。
 *
 * 必须显式 `chmod` 而不是只靠 `mkdirSync(..., { mode })`：`mkdir` 的 mode 会被 umask 削
 * （只可能更严，故不会放宽），但 `fs.cpSync` 对目录型源会按源权限创建、对已存在的目录不改权限；
 * 且 `backupsDir` / 日期目录可能是**更早的、默认权限的调用**留下的。显式 chmod 才是不变式。
 */
function applyModes(root: string, mode: BackupFileModes): void {
  if (mode.dir !== undefined) chmodIfPresent(root, mode.dir);

  const stack: string[] = [root];
  while (stack.length > 0) {
    const dir = stack.pop()!;
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const abs = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        if (mode.dir !== undefined) chmodIfPresent(abs, mode.dir);
        stack.push(abs);
        continue;
      }
      if (mode.file !== undefined) chmodIfPresent(abs, mode.file);
    }
  }
}

/**
 * 备份 `targets` 中已存在的源文件。
 * `command` 是产生备份的命令名（push/pull/merge），进目录名 `<HHmmss>-<command>`。
 * 同秒内重复调用会落到同一目录（互相覆盖）——计划未要求唯一化，调用方按命令粒度使用即可。
 *
 * `opts.mode`（additive，对抗式 review M1）收紧备份目录树权限：密钥通道传
 * `{ dir: 0o700, file: 0o600 }`，普通配置备份不传（行为与改动前逐字相同）。
 */
export function backupFiles(
  paths: HomerPaths,
  command: string,
  targets: readonly BackupTarget[],
  opts?: BackupFilesOptions,
): BackupResult {
  assertSafeSegment(command, 'command');
  for (const target of targets) {
    assertSafeSegment(target.label, 'label');
  }

  const now = new Date();
  const backupDir = path.join(paths.backupsDir, dateStamp(now), `${timeStamp(now)}-${command}`);

  // 目录总是创建：`BackupResult.backupDir` 因此恒为「可展示给用户的真实路径」，
  // 即使本次无文件可备份（全 skipped）也能据此汇报「备份位置」。
  fs.mkdirSync(backupDir, { recursive: true });

  const backedUp: string[] = [];
  const skipped: string[] = [];

  for (const target of targets) {
    if (!fs.existsSync(target.sourceAbs)) {
      skipped.push(target.label);
      continue;
    }
    const dest = path.join(backupDir, target.label);
    fs.mkdirSync(path.dirname(dest), { recursive: true });
    // recursive 同时覆盖目录型源（分类目录整体备份）与单文件源。
    fs.cpSync(target.sourceAbs, dest, { recursive: true });
    backedUp.push(target.label);
  }

  const mode = opts?.mode;
  if (mode !== undefined) {
    // 目录链：backupsDir → 日期目录 → 时间目录（含标签父目录与文件）
    if (mode.dir !== undefined) {
      chmodIfPresent(paths.backupsDir, mode.dir);
      chmodIfPresent(path.dirname(backupDir), mode.dir);
    }
    applyModes(backupDir, mode);
  }

  return { backupDir, backedUp, skipped };
}

/**
 * 保留最近 `keep` 个日期目录，删除更旧的整个目录。
 * 返回的数组元素是日期目录名（`YYYYMMDD`，相对 `backupsDir`）。
 * `keep=0` → 删除全部日期目录。非法 `keep`（负数 / 非整数）→ throw（编程错误）。
 */
export function pruneBackups(
  paths: HomerPaths,
  keep: number = DEFAULT_BACKUP_KEEP,
): { removed: string[]; kept: string[] } {
  if (!Number.isInteger(keep) || keep < 0) {
    throw new Error(`pruneBackups: keep 必须是非负整数（当前: ${JSON.stringify(keep)}）`);
  }

  let entries: fs.Dirent[];
  try {
    entries = fs.readdirSync(paths.backupsDir, { withFileTypes: true });
  } catch {
    // backupsDir 不存在（从未备份过）：无事可做。
    return { removed: [], kept: [] };
  }

  const names = entries
    .filter((entry) => entry.isDirectory() && DATE_DIR_RE.test(entry.name))
    .map((entry) => entry.name)
    .sort((a, b) => (a < b ? 1 : a > b ? -1 : 0)); // 名降序 = 日期新→旧

  const kept = names.slice(0, keep);
  const removed = names.slice(keep);
  for (const name of removed) {
    fs.rmSync(path.join(paths.backupsDir, name), { recursive: true, force: true });
  }

  return { removed, kept };
}
