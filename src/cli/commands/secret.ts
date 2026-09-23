/**
 * `homer secret` —— age 密钥投递子命令族（docs/m3-plan.md §2.6 / §2.1）。
 *
 * **P0 落地范围**：§2.6 的全部接口（类型 + `SECRET_USAGE` + 四个 `runSecret*` 的 stub）
 * + 子命令参数切分（`parseSecretSubcommand`）+ 渲染钩子（`renderSecret*Report`，使实现时
 * 无需改分发层）。**P2-W6 在本文件内实现四个子命令的真实流程**（类型/签名一字未动）。
 *
 * ## 为什么子命令解析放在本文件
 *
 * §2.1 冻结：`secret` 的子命令（keygen/push/pull/list）由本文件内部解析 `rest[0]`，
 * 未知子命令 → usageError。分发层（`index.ts`）只负责：切出 command 位 → 调本文件的
 * `parseSecretSubcommand` → 按子命令解析各自的 flag 表（§2.1 表格）→ 调对应 `runSecret*`。
 * `index.ts` 自身不硬编码子命令名，因此子命令集合的真相只有一份（`SECRET_SUBCOMMANDS`）。
 *
 * ## 退出码总表（§2.6 冻结）
 *
 *   keygen：成功 → 0 / identity 已存在 → 1
 *   push  ：pushed / no-secrets → 0；no-identity / no-recipients / missing-source /
 *           aborted / error → 1
 *   pull  ：applied / no-secrets → 0；no-identity / missing-vault / undecryptable /
 *           aborted / error → 1
 *   list  ：恒 0
 *
 * ## 管线边界（§1-D4 冻结）
 *
 * `secret push|pull` 与 store 管线（push/pull/merge）**零耦合**：
 *   - commit 只 `add secrets/`（`commitPaths(['secrets/'])`）——store/ 的脏工作区保持原样；
 *   - `secret pull` 从 `@{upstream}`（fetch 后）读密文，**不 ff 整仓**（避免 store 工作区被
 *     远端悄悄推进）；
 *   - vault 只写密文：密文自检在 `encryptSecretToFile` 内（§2.2），明文从不进 git。
 *
 * ## 安全约束（§2.2 / §4-2，冻结）
 *
 * 报告与 stdout **只含 recipient（公钥）**，绝不回显 `secretKey`、也不含任何明文密钥内容。
 */

import fs from 'node:fs';
import process from 'node:process';

import {
  createAgeCryptoPort,
  destinationOf,
  encryptSecretToFile,
  generateIdentity,
  identityFilePath,
  loadIdentity,
  listSecrets,
  secretFilePath,
  secretNames,
  secretRelativePath,
  writeIdentityFile,
  writeSecretDestinations,
} from '../../core/age/index.js';
import type { AgeCryptoPort } from '../../core/age/types.js';
import type { VaultEntryStatus } from '../../core/age/vault.js';
import { loadConfig } from '../../core/config.js';
import { CliError, cliErrorLines } from '../../core/errors.js';
import { commitPaths, gitExec, readVaultFileAtCommit } from '../../core/git/index.js';
import type { HomerPaths } from '../../core/paths.js';
import type { HomerConfig } from '../../core/types.js';
import { resolveHomerPaths } from '../render.js';
import { createDefaultPromptPort, type PromptPort } from '../ui.js';
import { resolveGitPort, type GitPort } from './git-port.js';

/**
 * 子命令集合（冻结顺序 = §2.1 表格顺序）。
 *
 * `VaultEntryStatus` 的权威定义在 `src/core/age/vault.ts`（P1-W1 已落地，形状与 §2.2 逐字
 * 相同）。此处**转出**而非本地声明：`SecretListReport` 的成员类型只有一个真相，且与
 * `listSecrets()` 的返回类型恒同构（不会因两处各自演进产生假兼容）。
 */
export const SECRET_SUBCOMMANDS = ['keygen', 'push', 'pull', 'list'] as const;
export type SecretSubcommand = (typeof SECRET_SUBCOMMANDS)[number];

export type { VaultEntryStatus } from '../../core/age/vault.js';

export interface SecretDeps {
  ui?: PromptPort;
  age?: AgeCryptoPort;
  git?: Partial<GitPort>;          // 复用 src/cli/commands/git-port.ts
}

export const SECRET_USAGE = `用法: homer secret <keygen|push|pull|list> [options]

age 加密的密钥投递：明文从不进 store；密文走 \`secrets/<name>.age\` 入库，
本机私钥留在 \`<home>/keys/age.txt\`（0600，gitignored）。

子命令:
  keygen   生成本机 age X25519 identity（已存在则拒绝覆盖，exit 1）
  push     读取 \`secrets.files\` 的明文 → 用全部 recipients 加密 → 写 vault → commit
  pull     从远端读密文 → 用本机 identity 解密 → 写回目标路径（覆盖前备份）
  list     列出配置的密钥及其 vault 文件状态

选项:
  --home <dir>       homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --json             输出机器可读 JSON
  -h, --help         显示本帮助

    keygen: --home --json
    push  : --home --yes --no-push --json   （非 TTY 必须 --yes）
    pull  : --home --yes --json             （非 TTY 必须 --yes）
    list  : --home --json

退出码: keygen 成功 → 0（已存在 → 1）；push pushed/no-secrets → 0；pull applied/no-secrets → 0；list → 0；其余 → 1。

提示: 私钥永不回显；输出只含 recipient（公钥）。`;

/** 子命令切分结果；`subcommand === undefined` = 只给了 `homer secret`（无子命令）。 */
export type SecretSubcommandParse =
  | { ok: true; subcommand: SecretSubcommand; rest: string[] }
  | { ok: true; subcommand: undefined; rest: string[] }
  | { ok: false; unknown: string };

/**
 * 从 `rest` 中切出子命令与其余参数（§2.1 冻结：本文件内部解析 `rest[0]`）。
 *
 * `homer secret`（无子命令）→ `subcommand: undefined`：分发层打印用法并 exit 0
 * （用户意图是「看看怎么用」，不是错误）。未知子命令 → `ok: false`：分发层 usageError + exit 1。
 */
export function parseSecretSubcommand(argv: readonly string[]): SecretSubcommandParse {
  const [first, ...rest] = argv;
  if (first === undefined) return { ok: true, subcommand: undefined, rest: [] };
  if ((SECRET_SUBCOMMANDS as readonly string[]).includes(first)) {
    return { ok: true, subcommand: first as SecretSubcommand, rest };
  }
  return { ok: false, unknown: first };
}

/* ------------------------------------------------------------------ */
/* 共用工具                                                            */
/* ------------------------------------------------------------------ */

/** 非交互环境确认失败时的提示（§2.7 约定 2 的固定措辞，同 `push.ts`）。 */
const NON_INTERACTIVE_HINT = '非交互环境，请加 --yes';

/**
 * `secrets.files` 的 name → 目标路径口径在两个命令层已提升到 `core/age/vault.ts`
 * （`secretNames` / `destinationOf`）——对抗式 review minor 4 的收口点，此处不再持副本。
 */

/** 缺 homer.json 的统一提示（与 `push.ts` / `status.ts` 同款）。 */
function requireConfig(paths: HomerPaths): HomerConfig {
  const config = loadConfig(paths);
  if (config === undefined) {
    throw new CliError(
      `未找到 homer 配置: ${paths.configFile}`,
      '请先运行 `homer init` 生成 homer.json 与 store 快照。',
    );
  }
  return config;
}

/** `name → destination` 的确认清单文本。 */
function confirmPreview(config: HomerConfig, names: readonly string[], verb: string): string {
  const lines = names.map((name) => `  ${name} → ${destinationOf(config, name)}`);
  return `${verb} ${names.length} 个密钥：\n${lines.join('\n')}\n继续？`;
}

/** `git status --porcelain -- secrets/` 非空 = 有未提交的 vault 改动。 */
function secretsDirty(home: string): boolean {
  const status = gitExec(home, ['status', '--porcelain', '--', 'secrets/']);
  return status.ok && status.stdout.trim() !== '';
}

/* ------------------------------------------------------------------ */
/* keygen                                                              */
/* ------------------------------------------------------------------ */

export interface SecretKeygenOptions { homerHome?: string; json?: boolean; }
export interface SecretKeygenReport { ok: boolean; identityFile: string; recipient: string; created: boolean; }

/**
 * 生成本机 identity 并落盘 `<keysDir>/age.txt`（0600，拒绝覆盖，原子写）。
 * 输出**只含 recipient**；私钥永不回显、不进错误消息（§2.2 / D2）。
 *
 * 已存在 identity → `writeIdentityFile` 抛 `CliError`（分发层渲染 + exit 1），**绝不覆盖**：
 * 覆盖等于永久销毁旧机可解的历史密文。
 *
 * 本函数只做「生成 + 落盘」（§2.6 冻结流程），不碰 git——`keys/` 的 gitignore 防护由
 * `ensureGitRepo`（`homer init` / `homer push` / `secret push` 路径）幂等维护，且私钥
 * 从不被列入任何 commit pathspec。
 */
export async function runSecretKeygen(opts: SecretKeygenOptions): Promise<SecretKeygenReport> {
  const paths = resolveHomerPaths(opts.homerHome);

  const identity = generateIdentity();
  writeIdentityFile(paths, identity);

  return { ok: true, identityFile: identityFilePath(paths), recipient: identity.recipient, created: true };
}

/* ------------------------------------------------------------------ */
/* push                                                                */
/* ------------------------------------------------------------------ */

export interface SecretPushOptions { homerHome?: string; json?: boolean; yes?: boolean; noPush?: boolean; }
export interface SecretPushReport {
  ok: boolean;
  status: 'pushed' | 'no-secrets' | 'no-identity' | 'no-recipients' | 'missing-source' | 'aborted' | 'error';
  encrypted: string[];             // secret 名清单
  commit?: string;
  pushedToRemote: boolean;
  warnings: string[];
  errors: string[];
}

/** 统一构造报告（字段齐全，避免各出口漏字段）。 */
function makePushReport(
  status: SecretPushReport['status'],
  patch: Partial<SecretPushReport> = {},
): SecretPushReport {
  return {
    ok: status === 'pushed' || status === 'no-secrets',
    status,
    encrypted: [],
    pushedToRemote: false,
    warnings: [],
    errors: [],
    ...patch,
  };
}

/**
 * 读明文 → 多 recipient 加密 → 写 vault → `commitPaths(['secrets/'])` →（可选）`git push`。
 *
 * 流程严格照 §2.6 的冻结顺序（「先读全部 → 逐项加密 → 确认 → commit → push」）：
 * `missing-source` 判定前**不写任何 vault**（全有或全无）；`--yes` 不创建 port；
 * commit 只覆盖 `secrets/`。
 *
 * 只把 `CliError` 收敛为 `status='error'` 报告（用户级问题 → exit 1 + 可解析 `--json`），
 * 其它异常照旧抛给分发层（编程错误不伪装成同步结果）。
 */
export async function runSecretPush(opts: SecretPushOptions, deps?: SecretDeps): Promise<SecretPushReport> {
  try {
    return await pushPipeline(opts, deps);
  } catch (err) {
    if (err instanceof CliError) return makePushReport('error', { errors: cliErrorLines(err) });
    throw err;
  }
}

async function pushPipeline(opts: SecretPushOptions, deps?: SecretDeps): Promise<SecretPushReport> {
  const paths = resolveHomerPaths(opts.homerHome);
  const git = resolveGitPort(deps?.git);
  const config = requireConfig(paths);
  const warnings: string[] = [];

  const names = secretNames(config);
  if (names.length === 0) {
    return makePushReport('no-secrets', {
      warnings: ['homer.json 的 secrets.files 为空，没有需要投递的密钥'],
    });
  }

  const identity = loadIdentity(paths);
  if (identity === undefined) {
    return makePushReport('no-identity', {
      warnings,
      errors: [
        `未找到本机 age identity: ${identityFilePath(paths)}`,
        '请先运行 `homer secret keygen` 生成本机私钥，并把输出的 recipient 写进 homer.json 的 secrets.recipients。',
      ],
    });
  }

  const recipients = config.secrets?.recipients ?? [];
  if (recipients.length === 0) {
    return makePushReport('no-recipients', {
      warnings,
      errors: [
        'homer.json 的 secrets.recipients 为空：没有加密目标',
        '把各机器的 recipient（`age1...`，由 `homer secret keygen` 输出）写进 secrets.recipients 后重试。',
      ],
    });
  }

  // ---- 1. 先读全部明文（任一缺失 → 全有或全无，未写任何 vault 文件）---------
  const plaintexts = new Map<string, Buffer>();
  const missing: string[] = [];
  for (const name of names) {
    const destination = destinationOf(config, name);
    try {
      plaintexts.set(name, fs.readFileSync(destination));
    } catch {
      missing.push(`${name} → ${destination}`);
    }
  }
  if (missing.length > 0) {
    return makePushReport('missing-source', {
      warnings,
      errors: [
        `以下目标文件不可读（${missing.length}/${names.length}），已中止且未写入任何 vault 文件：`,
        ...missing,
      ],
    });
  }

  // ---- 2. 逐项加密写入 vault（全部 recipients；密文自检在 vault 层）---------
  const crypto = deps?.age ?? createAgeCryptoPort();
  const encrypted: string[] = [];
  for (const name of names) {
    await encryptSecretToFile(crypto, paths, name, plaintexts.get(name)!, recipients);
    encrypted.push(name);
  }

  // ---- 3. 交互确认（§2.6 冻结顺序：加密之后、commit 之前）------------------
  // 注意：拒绝（aborted）时 vault 工作区已含新密文但**未 commit**（`secrets/` 保持脏），
  // 明文/私钥从不落盘，故无泄漏面；下次 push 会重新加密覆盖。
  if (opts.yes !== true) {
    const port = deps?.ui ?? createDefaultPromptPort();
    const approved = await port.confirm(confirmPreview(config, encrypted, '将加密并提交'), false);
    if (!approved) {
      if (deps?.ui === undefined && process.stdout.isTTY !== true) warnings.push(NON_INTERACTIVE_HINT);
      return makePushReport('aborted', {
        encrypted,
        warnings,
        errors: ['已取消：未确认投递（vault 未 commit，远端未推送）'],
      });
    }
  }

  // ---- 4. commit 只覆盖 secrets/ ------------------------------------------
  git.ensureGitRepo(paths.home);
  if (!git.isGitRepo(paths.home)) {
    return makePushReport('error', {
      encrypted,
      warnings,
      errors: [
        `vault 已写入但 ${paths.home} 不是 git 仓库（git 不可用？），无法提交`,
        '请确认 git 已安装并可用，然后重试。',
      ],
    });
  }

  const commit = commitPaths(paths.home, ['secrets/'], `homer secret push: 更新 ${encrypted.length} 个密钥`);
  if (commit === undefined) {
    if (secretsDirty(paths.home)) {
      return makePushReport('error', {
        encrypted,
        warnings,
        errors: [
          `vault 已写入但 git commit 失败: ${paths.home}`,
          '请检查 git 是否可用与 user.name / user.email 配置（`git -C <home> config user.email`），然后重试。',
        ],
      });
    }
    warnings.push('vault 内容与 HEAD 一致，未产生新 commit');
  }

  const report = makePushReport('pushed', { encrypted, warnings });
  if (commit !== undefined) report.commit = commit;

  // ---- 5. 远端推送（有 push target 且非 --no-push）-------------------------
  if (opts.noPush === true) {
    warnings.push('--no-push: 只做本地 commit，未推送远端');
    return report;
  }

  if (!git.hasPushTarget(paths.home)) {
    warnings.push('未配置 git upstream，仅本地 commit（local-only 模式；如需推送请 `git push -u <remote> <branch>`）');
    return report;
  }

  const pushed = git.gitPush(paths.home);
  if (!pushed.ok) {
    const reason = pushed.stderr.trim() === '' ? '未知错误' : pushed.stderr.trim();
    warnings.push(`远端推送失败: ${reason}`);
    return makePushReport('error', {
      encrypted,
      commit,
      warnings,
      errors: [
        `本地 commit 已成功${commit === undefined ? '' : `（${commit.slice(0, 7)}）`}，远端推送失败: ${reason}`,
        '密钥已在本地提交，修复远端问题后重试 `homer secret push`。',
      ],
    });
  }

  report.pushedToRemote = true;
  return report;
}

/* ------------------------------------------------------------------ */
/* pull                                                                */
/* ------------------------------------------------------------------ */

export interface SecretPullOptions { homerHome?: string; json?: boolean; yes?: boolean; }
export interface SecretPullReport {
  ok: boolean;
  status: 'applied' | 'no-secrets' | 'no-identity' | 'missing-vault' | 'undecryptable' | 'aborted' | 'error';
  pulled: string[];
  backupDir?: string;
  warnings: string[];
  errors: string[];
}

function makePullReport(
  status: SecretPullReport['status'],
  patch: Partial<SecretPullReport> = {},
): SecretPullReport {
  return {
    ok: status === 'applied' || status === 'no-secrets',
    status,
    pulled: [],
    warnings: [],
    errors: [],
    ...patch,
  };
}

/**
 * 从远端读密文 → 解密 → 备份后写回目标（0600）。
 *
 * 冻结语义（§2.6）+ 对抗式 review 修复（M1/M2/M3）：
 *   - **取数双源对称**（M3，与 doctor `checkAge` 同口径）：逐项「upstream 可读优先 upstream，
 *     缺失 / 不可读回落工作区」。fetch 成功但 `@{upstream}` 上没有该文件时不再误报
 *     `missing-vault`（工作区有就归位）；fetch 失败时工作区与 upstream ref 任一可读即可。
 *   - **fetch 失败不得静默回滚**（M2）：存在 upstream 配置时，若本地 remote-tracking ref
 *     可用且工作区密文与之**不一致** → `status='error'` exit 1（不写任何目标）；
 *     无本地 tracking ref（无法比对）→ 回落仍执行，但 warning 明示「将回滚到本地旧版密文」，
 *     且 `--yes` 下直接 error（密钥回滚是高危操作）。
 *   - 任一 vault 缺失 → `missing-vault`（不写任何目标）；
 *   - 任一解密失败 → `undecryptable`（**不写任何目标**，全有或全无）；
 *   - 非 `--yes` → confirm；已存在的目标先备份（目录 0700 / 文件 0600，见 M1）再写。
 */
export async function runSecretPull(opts: SecretPullOptions, deps?: SecretDeps): Promise<SecretPullReport> {
  try {
    return await pullPipeline(opts, deps);
  } catch (err) {
    if (err instanceof CliError) return makePullReport('error', { errors: cliErrorLines(err) });
    throw err;
  }
}

/** 从工作区读 vault 密文（`readVaultFileAtCommit` 的回落口）。 */
function readWorkspaceVault(paths: HomerPaths, name: string): Buffer | undefined {
  try {
    return fs.readFileSync(secretFilePath(paths, name));
  } catch {
    return undefined;
  }
}

async function pullPipeline(opts: SecretPullOptions, deps?: SecretDeps): Promise<SecretPullReport> {
  const paths = resolveHomerPaths(opts.homerHome);
  const git = resolveGitPort(deps?.git);
  const config = requireConfig(paths);
  const warnings: string[] = [];

  const names = secretNames(config);
  if (names.length === 0) {
    return makePullReport('no-secrets', {
      warnings: ['homer.json 的 secrets.files 为空，没有需要归位的密钥'],
    });
  }

  const identity = loadIdentity(paths);
  if (identity === undefined) {
    return makePullReport('no-identity', {
      warnings,
      errors: [
        `未找到本机 age identity: ${identityFilePath(paths)}`,
        '先在本机运行 `homer secret keygen`；若要从旧机迁移：在旧机把本机 recipient 加入 homer.json 的 secrets.recipients 后重新 `homer secret push`，再回到本机 `homer secret pull`。',
      ],
    });
  }

  // ---- 1. fetch（失败降级为 warning + 双源回落）---------------------------
  const fetched = git.gitFetch(paths.home);
  // `configuredUpstream` 读的是**配置**（fetch 失败时 `@{upstream}` 解析不可靠）；
  // `upstreamRef` 读的是可解析的 ref（正常路径的真相；只读本地 ref，不发网络）。
  const configured = git.configuredUpstream(paths.home);
  const upstream = git.upstreamRef(paths.home);
  const upstreamName = upstream ?? configured;
  // 本地 remote-tracking ref 是否真的可读（`configured` 只说明配置里有）。
  const upstreamReadable = upstream !== undefined && git.refExists(paths.home, upstream);
  // 「fetch 失败但无法比对工作区与远端」= 高风险回滚场景（M2）。
  let unverifiableRollback = false;

  if (!fetched.ok) {
    const reason = fetched.stderr.trim() === '' ? '未知错误' : fetched.stderr.trim();
    if (upstreamName === undefined) {
      warnings.push(`git fetch 失败（${reason}）：当前分支无 upstream，回落读取工作区 vault`);
    } else if (upstreamReadable) {
      warnings.push(`git fetch 失败（${reason}）：回落读取工作区 vault（并与本地 ${upstream} 比对）`);
    } else {
      unverifiableRollback = true;
      warnings.push(
        `git fetch 失败（${reason}）且本地无 ${upstreamName} 可读引用，无法确认工作区密文是否为最新：将回滚到本地旧版密文`,
      );
    }
  } else if (upstream === undefined) {
    warnings.push('当前分支无 upstream：读取工作区 vault');
  }

  // ---- 2. 读全部密文（双源对称：upstream 优先，缺失 / 不可读回落工作区）----
  // 全有或全无：先于任何写操作。
  const ciphertexts = new Map<string, Buffer>();
  const missing: string[] = [];
  const diverged: string[] = [];

  for (const name of names) {
    const rel = secretRelativePath(name);
    const upstreamBytes = upstreamReadable
      ? readVaultFileAtCommit(paths.home, rel, upstream!)
      : undefined;
    const workspaceBytes = readWorkspaceVault(paths, name);

    // M2：fetch 失败且两端密文都可读、但不一致 → 拒绝写旧值（见下文早退）。
    if (!fetched.ok && upstreamBytes !== undefined && workspaceBytes !== undefined) {
      if (!upstreamBytes.equals(workspaceBytes)) diverged.push(`${rel}（工作区 ≠ ${upstream}）`);
    }

    const bytes = upstreamBytes ?? workspaceBytes;
    if (bytes === undefined) {
      missing.push(
        `${rel}${upstreamReadable ? `（工作区与 ${upstream}）` : '（工作区）'}`,
      );
      continue;
    }
    ciphertexts.set(name, bytes);
  }

  if (diverged.length > 0) {
    return makePullReport('error', {
      warnings,
      errors: [
        `git fetch 失败且工作区密文与本地 ${upstream} 不一致（${diverged.length}/${names.length}）：拒绝写旧值`,
        ...diverged,
        '无法确认远端最新密文（工作区可能是旧版本）：请恢复网络后重跑 `homer secret pull`；确需使用本地版本请先 `homer secret push`（或用 `git pull` 手工对齐）。',
      ],
    });
  }

  if (missing.length > 0) {
    return makePullReport('missing-vault', {
      warnings,
      errors: [
        `以下 vault 文件缺失（${missing.length}/${names.length}），已中止且未写入任何目标文件：`,
        ...missing,
        '确认旧机已 `homer secret push`（密文进 git）后重试 `homer secret pull`。',
      ],
    });
  }

  // ---- 3. 逐项解密（任一失败 → 零写入）------------------------------------
  const crypto = deps?.age ?? createAgeCryptoPort();
  const plaintexts = new Map<string, Buffer>();
  const failures: string[] = [];
  for (const name of names) {
    try {
      plaintexts.set(name, await crypto.decrypt(ciphertexts.get(name)!, identity));
    } catch (err) {
      // crypto.decrypt 的 CliError 文案不含密文细节（cipher.ts 保证），可直接进报告。
      failures.push(`${name}: ${err instanceof Error ? err.message : '解密失败'}`);
    }
  }
  if (failures.length > 0) {
    return makePullReport('undecryptable', {
      warnings,
      errors: [
        `以下密钥无法用本机 identity 解密（${failures.length}/${names.length}），已中止且未写入任何目标文件：`,
        ...failures,
        '本机 recipient 可能不在旧机的 secrets.recipients 里：请在旧机追加本机 recipient 后重新 `homer secret push`。',
      ],
    });
  }

  // ---- 3b. 无法比对时的回滚闸门（M2，密钥回滚 = 高危）----------------------
  if (unverifiableRollback && opts.yes === true) {
    return makePullReport('error', {
      warnings,
      errors: [
        'git fetch 失败且无法比对本地与远端密文（无可用 remote-tracking 引用）：`--yes` 下拒绝回滚到旧版密文',
        '请恢复网络后重跑 `homer secret pull`；若确认本地就是最新版，请先 `homer secret push` 把本地密文推到远端。',
      ],
    });
  }

  // ---- 4. 交互确认（写任何目标之前）---------------------------------------
  if (opts.yes !== true) {
    const port = deps?.ui ?? createDefaultPromptPort();
    const rollback = unverifiableRollback
      ? '\n⚠ 警告：fetch 失败且无法比对远端，本次将回滚到**本地旧版**密文（旧值可能已废弃）。'
      : '';
    const approved = await port.confirm(`${confirmPreview(config, names, '将归位')}${rollback}`, false);
    if (!approved) {
      if (deps?.ui === undefined && process.stdout.isTTY !== true) warnings.push(NON_INTERACTIVE_HINT);
      return makePullReport('aborted', {
        warnings,
        errors: ['已取消：未确认归位（未写入任何目标文件）'],
      });
    }
  }

  // ---- 5-6. 备份已存在的目标 + 写目标 0600（M1：备份目录树 0700/0600）------
  // 共享实现点：`core/age/vault.ts` 的 `writeSecretDestinations`（与 `home` 步骤 9 同一份）。
  const placed = writeSecretDestinations(paths, config, plaintexts, 'secret');

  return makePullReport('applied', {
    pulled: [...names],
    backupDir: placed.backupDir,
    warnings,
  });
}

/* ------------------------------------------------------------------ */
/* list                                                                */
/* ------------------------------------------------------------------ */

export interface SecretListOptions { homerHome?: string; json?: boolean; }
export interface SecretListReport { secrets: VaultEntryStatus[]; }

/** 列出 `secrets.files` 的每一项及 vault 文件 present/missing（纯读，不解密）。 */
export function runSecretList(opts: SecretListOptions): SecretListReport {
  const paths = resolveHomerPaths(opts.homerHome);
  const config = requireConfig(paths);
  return { secrets: listSecrets(paths, config) };
}

/* ------------------------------------------------------------------ */
/* 渲染钩子（P0 预置，使 W6 实现时无需改 index.ts）                     */
/* ------------------------------------------------------------------ */

export function renderSecretKeygenReport(report: SecretKeygenReport): string {
  const lines = [
    `homer secret keygen: ${report.created ? '已生成' : '已存在'}`,
    `  identity 文件: ${report.identityFile}`,
    `  recipient: ${report.recipient}`,
  ];
  lines.push('  提示: 把上面这行 recipient 写入 homer.json 的 secrets.recipients（在旧机上），随后 `homer secret push`。');
  return lines.join('\n');
}

export function renderSecretPushReport(report: SecretPushReport): string {
  const lines = [`homer secret push: ${report.status}`];
  lines.push(`  已加密: ${report.encrypted.length} 个密钥`);
  for (const name of report.encrypted) lines.push(`    ${name}`);
  if (report.commit !== undefined) lines.push(`  本地提交: ${report.commit}`);
  lines.push(`  远端推送: ${report.pushedToRemote ? '已推送' : '未推送'}`);
  for (const warning of report.warnings) lines.push(`  ⚠ ${warning}`);
  for (const error of report.errors) lines.push(`  ✗ ${error}`);
  return lines.join('\n');
}

export function renderSecretPullReport(report: SecretPullReport): string {
  const lines = [`homer secret pull: ${report.status}`];
  lines.push(`  已归位: ${report.pulled.length} 个密钥`);
  for (const name of report.pulled) lines.push(`    ${name}`);
  if (report.backupDir !== undefined) lines.push(`  备份目录: ${report.backupDir}`);
  for (const warning of report.warnings) lines.push(`  ⚠ ${warning}`);
  for (const error of report.errors) lines.push(`  ✗ ${error}`);
  return lines.join('\n');
}

export function renderSecretListReport(report: SecretListReport): string {
  if (report.secrets.length === 0) return 'homer secret list: 未配置密钥（homer.json 的 secrets.files 为空）';
  const lines = ['homer secret list:'];
  for (const entry of report.secrets) {
    lines.push(`  ${entry.name}  →  ${entry.destination}  [vault ${entry.vaultFile}]`);
  }
  return lines.join('\n');
}
