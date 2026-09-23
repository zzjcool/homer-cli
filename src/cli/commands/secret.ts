/**
 * `homer secret` —— age 密钥投递子命令族（docs/m3-plan.md §2.6 / §2.1）。
 *
 * **P0 落地范围（本文件）**：§2.6 的全部接口（类型 + `SECRET_USAGE` + 四个 `runSecret*`
 * 的 **stub `throw CliError('尚未实现')`**）+ 子命令参数切分（`parseSecretSubcommand`）
 * + 渲染钩子（`renderSecret*Report`，使 W6 实现时无需改分发层）。
 * 真实流程由 **P2-W6** 在本文件内实现。
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
 * ## 安全约束（§2.2 / §4-2，冻结）
 *
 * 报告与 stdout **只含 recipient（公钥）**，绝不回显 `secretKey`、也不含任何明文密钥内容。
 */

import { CliError } from '../../core/errors.js';
import type { AgeCryptoPort } from '../../core/age/types.js';
import type { PromptPort } from '../ui.js';
import type { GitPort } from './git-port.js';

/**
 * 子命令集合（冻结顺序 = §2.1 表格顺序）。
 *
 * ⚠️ **P0 的 VaultEntryStatus 临时声明**：`src/core/age/vault.ts` 由 P1-W1 落地，
 * 其导出的 `VaultEntryStatus`（§2.2 冻结形状）是 `SecretListReport` 的成员类型。
 * P0 阶段该文件不存在，故这里按 §2.2 **逐字**声明一份同名类型。
 *
 * TODO(P1-W1)：`src/core/age/vault.ts` 落地后，删除下面的本地声明，改为
 *   `import type { VaultEntryStatus } from '../../core/age/vault.js';`
 * （1 行改动；形状逐字相同，故不是接口变更）。
 */
export const SECRET_SUBCOMMANDS = ['keygen', 'push', 'pull', 'list'] as const;
export type SecretSubcommand = (typeof SECRET_SUBCOMMANDS)[number];

/** `vault.ts` 的 `VaultEntryStatus`（§2.2 冻结形状的 P0 逐字副本，见文件头 TODO）。 */
export interface VaultEntryStatus {
  name: string;
  destination: string;
  vaultFile: 'present' | 'missing';
}

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

// ---- keygen ----

export interface SecretKeygenOptions { homerHome?: string; json?: boolean; }
export interface SecretKeygenReport { ok: boolean; identityFile: string; recipient: string; created: boolean; }

/**
 * 生成本机 identity 并落盘 `<keysDir>/age.txt`（0600，拒绝覆盖，原子写）。
 * 输出**只含 recipient**；私钥永不回显、不进错误消息（§2.2 / D2）。
 */
export async function runSecretKeygen(opts: SecretKeygenOptions): Promise<SecretKeygenReport> {
  // P0 stub：参数按冻结签名保留（W6 实现时逐项消费），此处显式标记为暂未使用。
  void opts;
  throw new CliError('尚未实现');
}

// ---- push ----

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

/** 读明文 → 多 recipient 加密 → 写 vault → `commitPaths(['secrets/'])` →（可选）`git push`。 */
export async function runSecretPush(opts: SecretPushOptions, deps?: SecretDeps): Promise<SecretPushReport> {
  void opts;
  void deps;
  throw new CliError('尚未实现');
}

// ---- pull ----

export interface SecretPullOptions { homerHome?: string; json?: boolean; yes?: boolean; }
export interface SecretPullReport {
  ok: boolean;
  status: 'applied' | 'no-secrets' | 'no-identity' | 'missing-vault' | 'undecryptable' | 'aborted' | 'error';
  pulled: string[];
  backupDir?: string;
  warnings: string[];
  errors: string[];
}

/** 从 `@{upstream}`（fetch 后）读密文 → 解密 → 备份后写回目标（0600）。 */
export async function runSecretPull(opts: SecretPullOptions, deps?: SecretDeps): Promise<SecretPullReport> {
  void opts;
  void deps;
  throw new CliError('尚未实现');
}

// ---- list ----

export interface SecretListOptions { homerHome?: string; json?: boolean; }
export interface SecretListReport { secrets: VaultEntryStatus[]; }

/** 列出 `secrets.files` 的每一项及 vault 文件 present/missing（纯读，不解密）。 */
export function runSecretList(opts: SecretListOptions): SecretListReport {
  void opts;
  throw new CliError('尚未实现');
}

// ---- 渲染钩子（P0 预置，使 W6 实现时无需改 index.ts）----

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
