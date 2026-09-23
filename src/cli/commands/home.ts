/**
 * `homer home <repo-url>` —— 新机器一键归位（docs/m3-plan.md §2.7 / §1-D5 / §3-P3-W8）。
 *
 * **P3-W8 落地范围（本文件）**：§2.7 冻结接口（类型 + `HOME_USAGE` + `runHome` + 渲染钩子）
 * 的真实实现。类型/签名与 P0 逐字一致——唯一的形状收口是 `firstContact.conflicts`：
 * P0 曾本地声明 `HomeConflictSummary` 并留 TODO「W8 若形状与 M2 一致改用 `PullConflictAction`」，
 * §2.7 冻结签名写的也是 `PullConflictAction[]`，故 W8 起直接用 M2 的权威类型
 * （`HomeConflictSummary` 保留为等价别名，零破坏）。
 *
 * ## 十步流程（§2.7 冻结，逐条落地）
 *
 *   1. `resolveHomerPaths`；home 目录存在且非空 → CliError（提示手工处理或 `--home` 另指定）
 *   2. `deps.clone`（默认真 `git clone`，timeout 60s；失败 → CliError 含 stderr 摘要）
 *   3. `loadConfig`（clone 下来的 homer.json）；缺失/非法 → CliError「该仓库不是 homer 配置中心」
 *   4. `scanAdapter` 逐 enabled adapter（本机现状；root 缺失按 M-A 守卫回落 = remote）
 *   5. remote = `readSnapshotFromStore`（clone 后工作区 == HEAD）
 *   6. **首次对接三选一**：`--mode` → 用之；`--yes` 无 mode → `merge`；否则 `ui.select`；
 *      非 TTY 且无 mode/yes → CliError（提示 `--mode` 或 `--yes`）
 *   7. `planFirstContact` → 确认 → `applyPullActions(backup=true, command='home')`
 *   8. `saveState`：`lastSyncCommit = HEAD`、`lastSyncCommand = 'pull'`、`lastSyncAt` = now
 *      （remote 已被看见；merge 模式保留的本地文件成为 push 漂移，见 §1-D5）
 *   9. **密钥归位**：`secrets.files` 非空 → `loadIdentity`；缺失 → `secrets.skipped` + warning；
 *      有 identity → 从工作区 vault 逐项解密（全有或全无）→ 备份后写目标（0600）；
 *      不可解 / vault 缺失 → `secrets.errors` + warning，**不 fail 整个 home**
 *  10. `runDoctor`（透传 `deps.age`）→ 附于 `doctor`；`homed` exit 0（doctor 的 fail
 *      只呈现在报告里，不改变 home 退出码 —— doctor 独立运行时才 exit 1）
 *
 * ## 失败形态（三种，互不混淆）
 *
 *   - **流程无法开始**（目标目录非空 / clone 失败 / 仓库无 homer.json / 非 TTY 且无 mode+yes）
 *     → `throw CliError`：分发层打印 message(+hint) 并 exit 1，绝不产出半真半假的报告。
 *   - **用户拒绝确认** → `status='aborted'` 报告（零应用、零 state、零密钥写入）。
 *   - **应用阶段之后**（步骤 7-10）的用户级失败 → `status='error'` 报告：`--json` 消费者仍能
 *     看到 `cloned` / `adapterIds` / 已完成的 `applied`（进程退出码同样为 1）。
 *     非 `CliError` 的异常（编程错误）一律冒泡，不伪装成同步结果（与 push/secret 同款纪律）。
 *
 * ## 唯一确认门槛：配置归位 ∪ 密钥归位（为什么并集）
 *
 * §2.7 步骤 7 的 `confirm` 是本流程唯一的用户同意点（`--yes` 时完全不创建 port）。
 * 密钥归位同样是**写用户目录**（0600，且会覆写已存在的目标），与 `secret pull` 的
 * 「非 `--yes` 必须确认」是同一种破坏性操作。若门槛只挂在「非 skip 且有动作」上，
 * `--mode skip` + 配了密钥时会出现「无动作 → 无确认 → 静默覆写密钥目标」，
 * 与 secret 通道自己的安全语义冲突。故门槛条件取**并集**（有配置动作 **或** 有待归位密钥），
 * preview 同时列出两类计数；非 TTY 下 `confirm` 回落 `false` → `aborted`（不阻塞、不默认同意）。
 *
 * ## Non-goal（M3 明确不做）
 *
 * home **不装依赖**（DESIGN §2.3 的「装依赖」延后：涉及各工具包管理器与网络副作用，
 * 风险大于收益；MVP 验收场景只需配置 + 密钥归位）。记录为 M4+ 候选。
 * 也**不**做 store 三路合并 / 逐文件冲突裁决 UI：merge 模式的冲突整体保留本地（§1-D5），
 * 随后自然表现为 push 漂移，由 `homer status` 可见、`homer merge` / `homer push` 收敛。
 */

import { execFile } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';

import { scanAdapter } from '../../adapters/pi/index.js';
import {
  createAgeCryptoPort,
  identityFilePath,
  loadIdentity,
  secretFilePath,
  secretRelativePath,
} from '../../core/age/index.js';
import type { AgeCryptoPort } from '../../core/age/types.js';
import { backupFiles, type BackupTarget } from '../../core/backup/backup.js';
import { loadConfig } from '../../core/config.js';
import { CliError } from '../../core/errors.js';
import { expandHome, type HomerPaths } from '../../core/paths.js';
import { isRootUnreadable, scanWarningMessages } from '../../core/scan-guard.js';
import { saveState } from '../../core/state.js';
import { readSnapshotFromStore } from '../../core/store/store.js';
import { applyPullActions } from '../../core/sync/apply.js';
import { planFirstContact, type FirstContactMode, type FirstContactPlan } from '../../core/sync/first-sync.js';
import type { ApplyResult, PullAction, PullConflictAction } from '../../core/sync/types.js';
import type { AdapterSnapshot, HomerConfig } from '../../core/types.js';
import { resolveHomerPaths } from '../render.js';
import { createDefaultPromptPort, type PromptPort } from '../ui.js';
import { runDoctor, type DoctorReport } from './doctor.js';
import { resolveGitPort, type GitPort } from './git-port.js';

/* ------------------------------------------------------------------ */
/* §2.4 / §2.7 类型（W5 落地后改为 import；形状不变）                     */
/* ------------------------------------------------------------------ */

/** 首次对接模式（权威定义在 `src/core/sync/first-sync.ts`，§2.4）。此处转出保持既有 import 路径可用。 */
export type { FirstContactMode };

/**
 * P0 时期的临时冲突形状（§2.7 报告形状）。
 *
 * @deprecated W8 起 `HomeReport.firstContact.conflicts` 用 M2 的 `PullConflictAction`
 * （§2.7 冻结签名原文即如此；P0 的 TODO 亦要求收口）。保留本别名为等价类型，
 * 使既有 `import type { HomeConflictSummary }` 仍可编译。
 */
export type HomeConflictSummary = PullConflictAction;

// ---- §2.7 命令层接口 ----

export interface HomeOptions {
  homerHome?: string;              // 目标工作区（默认 ~/.homer）；目录必须不存在或为空
  repoUrl: string;                 // 位置参数（必填）
  json?: boolean;
  yes?: boolean;
  mode?: FirstContactMode;         // --mode；缺省 + --yes → 'merge'（最安全默认）
}

export interface HomeDeps {
  ui?: PromptPort;
  age?: AgeCryptoPort;
  git?: Partial<GitPort>;
  /** clone 注入位（测试用假 origin）。缺省 = execFile('git', ['clone', url, dest], timeout 60s)。 */
  clone?: (repoUrl: string, destDir: string) => Promise<void>;
}

export interface HomeReport {
  ok: boolean;
  status: 'homed' | 'aborted' | 'error';
  cloned: boolean;
  adapterIds: string[];
  firstContact: { mode: FirstContactMode; applied: ApplyResult; conflicts: PullConflictAction[] } | undefined;
  secrets: { pulled: string[]; skipped: string[]; errors: string[] };
  doctor: DoctorReport | undefined;
  warnings: string[];
  errors: string[];
}

export const HOME_USAGE = `用法: homer home <repo-url> [options]

新机器一键归位：clone 配置仓库 → 应用各 adapter 分类配置 → 首次对接三选一
→ 用本机 age identity 解密密钥归位 → 体检。

选项:
  --home <dir>             目标工作区（默认 $HOMER_HOME 或 ~/.homer；须不存在或为空目录）
  --mode pull|merge|skip   首次对接模式（pull=远端覆盖 / merge=合并保留本地 / skip=暂不应用）
  --yes                    跳过交互确认；未给 --mode 时默认 merge（最安全）
  --json                   输出机器可读 JSON（HomeReport）
  -h, --help               显示本帮助

说明:
  不安装任何工具依赖（只归位配置与密钥）。
  非交互环境必须给出 --mode 或 --yes。
  merge 模式保留的本地文件随后表现为「待 push 漂移」（远端已被看见，本地 = 意图真相）。

退出码: homed → 0；aborted / error → 1（doctor 的 fail 不影响 home 退出码）。`;

/** `git clone` 的超时（§2.7 步骤 2 冻结：60s）。 */
export const CLONE_TIMEOUT_MS = 60_000;

/** 预览里最多逐条列出的动作数（超出以「… 其余 N 条省略」收尾，防大仓库刷屏）。 */
export const PREVIEW_MAX_ACTIONS = 20;

/* ------------------------------------------------------------------ */
/* 报告零件                                                            */
/* ------------------------------------------------------------------ */

/** 空应用结果（skip / aborted / 无动作时报告里仍是合法形状）。 */
function emptyApplyResult(): ApplyResult {
  return { written: [], deleted: [], conflicts: [] };
}

function emptySecrets(): HomeReport['secrets'] {
  return { pulled: [], skipped: [], errors: [] };
}

function isConflict(action: PullAction): action is PullConflictAction {
  return action.type === 'conflict';
}

/** `CliError` → 报告 `errors` 数组（message + 可选 hint）。 */
function errorLines(err: CliError): string[] {
  return err.hint === undefined ? [err.message] : [err.message, err.hint];
}

/* ------------------------------------------------------------------ */
/* 步骤 1：目标目录                                                     */
/* ------------------------------------------------------------------ */

/**
 * 目标目录必须「不存在」或「空」。
 *
 * 由 `git clone` 自己创建 `home`（父目录需存在，见 `defaultClone`）——若 `home` 里已有内容，
 * clone 会失败，且用户可能把 `--home` 指到自己的真实 `~/.pi` 之类目录上：宁可在**任何写操作之前**
 * 用一条明确的 CliError 拦下，也不要让 git 的半途失败留下混合目录。
 */
function assertTargetIsEmpty(home: string): void {
  let stat: fs.Stats;
  try {
    stat = fs.statSync(home);
  } catch {
    return; // 不存在 → clone 会创建
  }

  if (!stat.isDirectory()) {
    throw new CliError(
      `目标路径不是目录: ${home}`,
      '请用 `--home <dir>` 指定一个不存在或为空的目录作为 homer 工作区。',
    );
  }

  const entries = fs.readdirSync(home);
  if (entries.length > 0) {
    throw new CliError(
      `目标目录非空: ${home}（${entries.length} 项）`,
      '请先备份并移走该目录中的内容，或用 `--home <dir>` 指定另一个工作区（homer 不覆盖已有目录）。',
    );
  }
}

/* ------------------------------------------------------------------ */
/* 步骤 2：clone                                                       */
/* ------------------------------------------------------------------ */

/** stderr / Error.message → 一行摘要（首条非空行，超长截断；绝不含凭据改写）。 */
function cloneFailureSummary(err: unknown, stderr?: string): string {
  const raw = (stderr ?? '').trim() !== '' ? (stderr as string) : err instanceof Error ? err.message : String(err);
  const firstLine = raw.split('\n').map((line) => line.trim()).find((line) => line !== '') ?? '未知错误';
  return firstLine.length > 300 ? `${firstLine.slice(0, 300)}…` : firstLine;
}

/** 缺省 clone：`execFile('git', ['clone', url, dest], timeout 60s)`（§2.7 步骤 2 逐字落地）。 */
function defaultClone(repoUrl: string, destDir: string): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    fs.mkdirSync(path.dirname(destDir), { recursive: true });
    execFile(
      'git',
      ['clone', repoUrl, destDir],
      { cwd: path.dirname(destDir), timeout: CLONE_TIMEOUT_MS, encoding: 'utf8', windowsHide: true },
      (err: Error | null, _stdout: string, stderr: string) => {
        if (err === null) {
          resolve();
          return;
        }
        const summary = cloneFailureSummary(err, typeof stderr === 'string' ? stderr : undefined);
        reject(
          new CliError(
            `git clone 失败（${summary}）: ${repoUrl}`,
            '请检查仓库地址是否正确、网络是否可用、是否需要访问凭据（私有仓库）。',
          ),
        );
      },
    );
  });
}

/**
 * clone（注入位优先）。失败一律收敛为**含摘要**的 `CliError`：
 * 无论是真 git 的 stderr，还是测试注入的 `Error` 消息，用户都必须在报告里看到原因。
 */
async function cloneRepo(repoUrl: string, destDir: string, deps?: HomeDeps): Promise<void> {
  const clone = deps?.clone ?? defaultClone;
  try {
    await clone(repoUrl, destDir);
  } catch (err) {
    if (err instanceof CliError) throw err;
    const stderr = (err as { stderr?: unknown }).stderr;
    const summary = cloneFailureSummary(err, typeof stderr === 'string' ? stderr : undefined);
    throw new CliError(
      `git clone 失败（${summary}）: ${repoUrl}`,
      '请检查仓库地址是否正确、网络是否可用、是否需要访问凭据（私有仓库）。',
    );
  }
}

/* ------------------------------------------------------------------ */
/* 步骤 3：config                                                      */
/* ------------------------------------------------------------------ */

/** clone 下来的仓库必须是 homer 配置中心（含合法 homer.json），否则明确的 CliError。 */
function requireHomerConfig(paths: HomerPaths): HomerConfig {
  let config: HomerConfig | undefined;
  try {
    config = loadConfig(paths);
  } catch (err) {
    const reason = err instanceof Error ? err.message : String(err);
    throw new CliError(
      `该仓库不是 homer 配置中心: ${paths.configFile} 不可用（${reason}）`,
      '请确认 <repo-url> 指向的是 homer 配置仓库（根目录含合法 homer.json）。',
    );
  }

  if (config === undefined) {
    throw new CliError(
      `该仓库不是 homer 配置中心: 未找到 ${paths.configFile}`,
      '请确认 <repo-url> 指向的是 homer 配置仓库（根目录含 homer.json），或先在旧机运行 `homer push` 把配置推上去。',
    );
  }

  return config;
}

/* ------------------------------------------------------------------ */
/* 步骤 4：本机现状扫描（M-A 守卫）                                       */
/* ------------------------------------------------------------------ */

/** enabled adapter 的 id（config 声明顺序）。 */
function enabledAdapterIds(config: HomerConfig): string[] {
  return Object.entries(config.adapters)
    .filter(([, adapter]) => adapter.enabled !== false)
    .map(([adapterId]) => adapterId);
}

/**
 * 逐 enabled adapter 扫描本机现状（§2.7 步骤 4）。
 *
 * root 不可读（不存在 / 不是目录）→ **local := remote**（M-A 守卫的唯一实现点：
 * `core/scan-guard.ts` 的 `isRootUnreadable`）：扫不到 ≠ 本地全空，否则 merge 模式下
 * 「双方都有且不同」会被误判成「remote 独有 → write」，把用户没读到的本地文件静默覆盖。
 * 原因经 `scanWarningMessages` 进报告 `warnings`（措辞与 status/merge 同一份）。
 */
function scanLocalSnapshots(
  config: HomerConfig,
  remote: readonly AdapterSnapshot[],
  warnings: string[],
): AdapterSnapshot[] {
  const local: AdapterSnapshot[] = [];

  for (const [adapterId, adapterConfig] of Object.entries(config.adapters)) {
    if (adapterConfig.enabled === false) continue;

    const outcome = scanAdapter(adapterId, adapterConfig);
    warnings.push(...scanWarningMessages(adapterId, outcome));

    if (isRootUnreadable(outcome)) {
      const fallback = remote.find((snapshot) => snapshot.adapterId === adapterId);
      if (fallback !== undefined) {
        local.push(fallback);
        continue;
      }
    }
    local.push(outcome.snapshot);
  }

  return local;
}

/* ------------------------------------------------------------------ */
/* 步骤 6：首次对接模式                                                  */
/* ------------------------------------------------------------------ */

/** `ui.select` 的选项（Pull / Merge / Skip，§2.7 步骤 6 的措辞）。 */
const MODE_OPTIONS: readonly { value: FirstContactMode; label: string }[] = [
  { value: 'pull', label: 'Pull：远端覆盖本机（本机独有文件保留，冲突项先备份）' },
  { value: 'merge', label: 'Merge：合并，冲突保留本地（推荐）' },
  { value: 'skip', label: 'Skip：暂不应用，只 clone 配置仓库' },
];

/**
 * 三选一（§2.7 步骤 6 冻结）。
 *
 * `--mode` 优先；`--yes` 无 mode → `merge`（最安全默认：远端独有写下来，冲突保留本地，
 * 永不删本地文件）；否则交互 `select`。**非 TTY 且无 mode/yes → CliError**：
 * 非交互下不能替用户挑一种会写工具目录的策略。
 */
async function resolveFirstContactMode(opts: HomeOptions, deps?: HomeDeps): Promise<FirstContactMode> {
  if (opts.mode !== undefined) return opts.mode;
  if (opts.yes === true) return 'merge';

  const injected = deps?.ui;
  if (injected === undefined && process.stdout.isTTY !== true) {
    throw new CliError(
      '非交互环境无法选择首次对接模式',
      '请显式给出 `--mode pull|merge|skip`，或加 `--yes`（未给 --mode 时默认 merge —— 最安全的合并策略）。',
    );
  }

  const port = injected ?? createDefaultPromptPort();
  return await port.select('选择首次对接方式（远端配置 vs 本机现状）', MODE_OPTIONS, 'merge');
}

/* ------------------------------------------------------------------ */
/* 步骤 7：预览 + 确认                                                   */
/* ------------------------------------------------------------------ */

function actionRef(action: PullAction): string {
  return `${action.adapterId}/${action.category}/${action.relPath}`;
}

/** 确认预览（纯函数：只读入参）。列出动作计数 + 前若干条明细 + 待归位密钥清单。 */
export function buildFirstContactPreview(
  config: HomerConfig,
  plan: FirstContactPlan,
  secretNames: readonly string[],
): string {
  const writes = plan.actions.filter((action) => action.type === 'write');
  const conflicts = plan.actions.filter(isConflict);
  const deletes = plan.actions.filter((action) => action.type === 'delete');

  const lines: string[] = [
    `首次对接（${plan.mode}）: 写入 ${writes.length}  冲突 ${conflicts.length}  删除 ${deletes.length}`,
  ];

  for (const action of plan.actions.slice(0, PREVIEW_MAX_ACTIONS)) {
    if (action.type === 'write') lines.push(`  写入 ${actionRef(action)}`);
    else if (action.type === 'delete') lines.push(`  删除 ${actionRef(action)}`);
    else {
      const keys = action.keyPaths === undefined || action.keyPaths.length === 0 ? '' : `（冲突键: ${action.keyPaths.join(', ')}）`;
      lines.push(`  冲突 ${actionRef(action)} [${action.reason}]${keys}`);
    }
  }
  if (plan.actions.length > PREVIEW_MAX_ACTIONS) {
    lines.push(`  … 其余 ${plan.actions.length - PREVIEW_MAX_ACTIONS} 条省略`);
  }

  if (secretNames.length > 0) {
    lines.push(`密钥归位: ${secretNames.length} 个（${secretNames.join(', ')}）`);
    for (const name of secretNames) lines.push(`  ${name} → ${destinationOf(config, name)}`);
  }

  lines.push('以上将应用到本机工具目录（受影响文件先备份）与密钥目标路径。是否继续？');
  return lines.join('\n');
}

/* ------------------------------------------------------------------ */
/* 步骤 9：密钥归位                                                      */
/* ------------------------------------------------------------------ */

/** `secrets.files` 的 name 清单（字典序，报告稳定）。 */
function secretNamesOf(config: HomerConfig): string[] {
  return Object.keys(config.secrets?.files ?? {}).sort();
}

/** 配置里某 secret 的目标绝对路径（`~` 展开；config 已校验值以 `~` / `/` 开头）。 */
function destinationOf(config: HomerConfig, name: string): string {
  const raw = config.secrets?.files?.[name];
  if (raw === undefined) {
    throw new CliError(`homer.json 的 secrets.files 不含 ${JSON.stringify(name)}`);
  }
  return path.resolve(expandHome(raw));
}

/** 缺 identity 时的两步补齐提示（§2.7 步骤 9 冻结措辞）。 */
const SECRET_KEYGEN_HINT =
  '两步补齐：① 本机 `homer secret keygen`；② 在旧机把输出的 recipient 加入 homer.json 的 secrets.recipients 后 `homer secret push`；③ 回本机 `homer secret pull`。';

/**
 * 密钥归位（§2.7 步骤 9 冻结语义，与 `secret pull` 同款"全有或全无"）：
 *
 *   - identity 缺失 → 全部进 `skipped` + warning（**不 fail** home：配置已归位，密钥可后补）；
 *   - 任一 vault 缺失 / 任一解密失败 → 全部进 `errors`（**零写入**）+ warning（同样不 fail home）；
 *   - 全部可解 → 已存在的目标先备份（label = `secret/<name>`）→ 写目标（父目录 mkdir -p，0600）。
 *
 * 密文只从 **clone 后的工作区** `secrets/` 读（home 不做 fetch：工作区 == HEAD == 刚 clone 的远端）。
 * 报告的 `pulled` / `errors` 只含 secret **名**，绝不含明文或私钥。
 */
async function placeSecretsInto(
  paths: HomerPaths,
  config: HomerConfig,
  crypto: AgeCryptoPort,
  secrets: HomeReport['secrets'],
  warnings: string[],
): Promise<void> {
  const names = secretNamesOf(config);
  if (names.length === 0) return;

  const identity = loadIdentity(paths);
  if (identity === undefined) {
    secrets.skipped.push(...names);
    warnings.push(`未找到本机 age identity，${names.length} 个密钥未归位: ${identityFilePath(paths)}`);
    warnings.push(SECRET_KEYGEN_HINT);
    return;
  }

  // 1. 读全部密文（任一缺失 → 全有或全无）
  const ciphertexts = new Map<string, Buffer>();
  const missing: string[] = [];
  for (const name of names) {
    try {
      ciphertexts.set(name, fs.readFileSync(secretFilePath(paths, name)));
    } catch {
      missing.push(secretRelativePath(name));
    }
  }
  if (missing.length > 0) {
    secrets.errors.push(
      `以下 vault 文件缺失（${missing.length}/${names.length}），密钥未归位且未写入任何目标文件: ${missing.join(', ')}`,
    );
    warnings.push('仓库里缺少密钥密文：请在旧机确认已 `homer secret push` 后回到本机 `homer secret pull`。');
    return;
  }

  // 2. 逐项解密（任一失败 → 零写入）
  const plaintexts = new Map<string, Buffer>();
  const failures: string[] = [];
  for (const name of names) {
    try {
      plaintexts.set(name, await crypto.decrypt(ciphertexts.get(name)!, identity));
    } catch (err) {
      // crypto.decrypt 的 CliError 文案不含密文/私钥细节（cipher.ts 保证），可直接进报告。
      failures.push(`${name}: ${err instanceof Error ? err.message : '解密失败'}`);
    }
  }
  if (failures.length > 0) {
    secrets.errors.push(
      `以下密钥无法用本机 identity 解密（${failures.length}/${names.length}），密钥未归位且未写入任何目标文件:`,
      ...failures,
    );
    warnings.push('本机 recipient 可能不在旧机的 secrets.recipients 里：请在旧机追加本机 recipient 后重新 `homer secret push`，再 `homer secret pull`。');
    return;
  }

  // 3. 备份已存在的目标（先于写盘，保证「备份内容 == 覆盖前」）
  const existing = names.filter((name) => fs.existsSync(destinationOf(config, name)));
  if (existing.length > 0) {
    const targets: BackupTarget[] = existing.map((name) => ({
      sourceAbs: destinationOf(config, name),
      label: `secret/${name}`,
    }));
    backupFiles(paths, 'home', targets);
  }

  // 4. 写目标（父目录 mkdir -p，0600）
  for (const name of names) {
    const destination = destinationOf(config, name);
    fs.mkdirSync(path.dirname(destination), { recursive: true });
    fs.writeFileSync(destination, plaintexts.get(name)!, { mode: 0o600 });
    // writeFileSync 的 mode 只在创建时生效（覆盖已存在文件不会改权限）→ 显式 chmod。
    fs.chmodSync(destination, 0o600);
    secrets.pulled.push(name);
  }
}

/* ------------------------------------------------------------------ */
/* runHome                                                             */
/* ------------------------------------------------------------------ */

/**
 * 执行一键归位（§2.7 十步流程）。
 *
 * 依赖注入（全部可选，additive）：`deps.clone` 假 origin、`deps.ui` PromptPort、
 * `deps.age` 密码学端口（透传给 doctor 的 age 检查与密钥归位）、`deps.git` git 端口
 * （目前只用 `headCommit` 读步骤 8 的 HEAD）。
 */
export async function runHome(opts: HomeOptions, deps?: HomeDeps): Promise<HomeReport> {
  const paths = resolveHomerPaths(opts.homerHome);
  const git = resolveGitPort(deps?.git);
  const warnings: string[] = [];

  // ---- 1. 目标目录（任何写操作之前）---------------------------------------
  assertTargetIsEmpty(paths.home);

  // ---- 2. clone -----------------------------------------------------------
  await cloneRepo(opts.repoUrl, paths.home, deps);

  // ---- 3. config ----------------------------------------------------------
  const config = requireHomerConfig(paths);
  const adapterIds = enabledAdapterIds(config);

  // ---- 4. 本机现状 + 5. remote（= clone 后的 store 工作区 == HEAD）---------
  const remote = readSnapshotFromStore(paths, config);
  const local = scanLocalSnapshots(config, remote, warnings);

  // ---- 6. 三选一 ----------------------------------------------------------
  const mode = await resolveFirstContactMode(opts, deps);
  const plan = planFirstContact(config, local, remote, mode);

  // ---- 7. 预览 + 确认（唯一同意门槛；--yes 时完全不创建 port）-------------
  const secretNames = secretNamesOf(config);
  if (opts.yes !== true && (plan.actions.length > 0 || secretNames.length > 0)) {
    const injected = deps?.ui;
    const port = injected ?? createDefaultPromptPort();
    const approved = await port.confirm(buildFirstContactPreview(config, plan, secretNames), false);
    if (!approved) {
      // 区分「非交互降级自动拒绝」与「TTY 下用户真的说了不」。
      if (injected === undefined && process.stdout.isTTY !== true) {
        warnings.push('非交互环境无法确认，已中止；如需自动归位请加 `--yes`');
      }
      return {
        ok: false,
        status: 'aborted',
        cloned: true,
        adapterIds,
        firstContact: { mode, applied: emptyApplyResult(), conflicts: [] },
        secrets: emptySecrets(),
        doctor: undefined,
        warnings,
        errors: ['已取消：未确认归位（未写入任何工具目录 / 密钥目标，未更新 state）'],
      };
    }
  }

  // ---- 7-10. 应用 → state → 密钥 → doctor（此段失败 → status='error' 报告）----
  const agePort = deps?.age;
  let applied = emptyApplyResult();
  let secrets = emptySecrets();
  let doctor: DoctorReport | undefined;

  try {
    // ---- 7. 应用（conflict 项由 applyPullActions 跳过 = 保留本地，D5）------
    if (plan.actions.length > 0) {
      applied = applyPullActions(paths, config, plan, { backup: true, command: 'home' });
    }

    // ---- 8. state：remote 已被看见（merge 保留的本地文件成为 push 漂移）-----
    const head = git.headCommit(paths.home);
    const now = new Date().toISOString();
    if (head === undefined) {
      warnings.push(`无法解析 HEAD（仓库没有 commit？），state.lastSyncCommit 未写入: ${paths.home}`);
      saveState(paths, { version: 1, lastSyncAt: now, lastSyncCommand: 'pull' });
    } else {
      saveState(paths, { version: 1, lastSyncCommit: head, lastSyncAt: now, lastSyncCommand: 'pull' });
    }

    // ---- 9. 密钥归位（缺失/不可解 → 只 warning，不 fail home）-------------
    await placeSecretsInto(paths, config, agePort ?? createAgeCryptoPort(), secrets, warnings);

    // ---- 10. doctor（fail 只呈现在报告里，不改变 home 退出码）--------------
    doctor = await runDoctor({ homerHome: paths.home }, agePort === undefined ? undefined : { age: agePort });
  } catch (err) {
    if (err instanceof CliError) {
      return {
        ok: false,
        status: 'error',
        cloned: true,
        adapterIds,
        firstContact: { mode, applied, conflicts: [] },
        secrets,
        doctor,
        warnings,
        errors: errorLines(err),
      };
    }
    throw err;
  }

  return {
    ok: true,
    status: 'homed',
    cloned: true,
    adapterIds,
    firstContact: { mode, applied, conflicts: plan.actions.filter(isConflict) },
    secrets,
    doctor,
    warnings,
    errors: [],
  };
}

/** 人类可读渲染（P0 预置钩子，W8 细化措辞 —— 分发层无需改动）。 */
export function renderHomeReport(report: HomeReport): string {
  const lines: string[] = [`homer home: ${report.status}`];
  lines.push(`  已 clone: ${report.cloned ? '是' : '否'}`);
  if (report.adapterIds.length > 0) lines.push(`  adapter: ${report.adapterIds.join(', ')}`);
  if (report.firstContact !== undefined) {
    const applied = report.firstContact.applied;
    lines.push(
      `  首次对接: ${report.firstContact.mode}` +
        `（写入 ${applied.written.length} / 删除 ${applied.deleted.length} / 冲突 ${report.firstContact.conflicts.length}）`,
    );
  }
  if (report.secrets.pulled.length > 0) lines.push(`  密钥归位: ${report.secrets.pulled.length} 个（${report.secrets.pulled.join(', ')}）`);
  if (report.secrets.skipped.length > 0) lines.push(`  密钥跳过: ${report.secrets.skipped.length} 个（${report.secrets.skipped.join(', ')}）`);
  if (report.doctor !== undefined) lines.push(`  体检: ${report.doctor.ok ? '通过' : '存在问题'}（详见 \`homer doctor\`）`);
  for (const warning of report.warnings) lines.push(`  ⚠ ${warning}`);
  for (const error of report.errors) lines.push(`  ✗ ${error}`);
  if (report.status === 'homed' && report.firstContact?.conflicts.length) {
    lines.push('  提示: 冲突项已保留本地（远端已被看见），可用 `homer status` 查看待 push 漂移。');
  }
  return lines.join('\n');
}
