/**
 * `secrets/` vault —— age 密文的落盘与读取（docs/m3-plan.md §2.2 / §1-D3 / §1-D4，P1-W1 落地）。
 *
 * 布局（§1-D3 冻结）：**一密钥一文件** `<secretsDir>/<name>.age`，二进制 age 密文（非 armor）；
 * `name` 限 `[A-Za-z0-9][A-Za-z0-9._-]*`（扁平、无子目录 → 结构性防逃逸）。
 * `secrets/` 是**入库**通道（与 store/ 平级的第二通道，§1-D4）。
 *
 * ## 密文自检（§1-D4 冻结：实现内断言密文不含明文片段）
 *
 * `encryptSecretToFile` 在写盘**之前**检查密文里不含明文的任何长度 >16 字节的片段
 * （取明文首/中/尾三处采样）。这让「加密层被换成一个假实现 / 被配置成 passphrase 空转 /
 * 上层误传明文」这类事故在写盘前就炸掉，而不是把明文 commit 进 git 再发现。
 * 阈值取 >16 字节而非全部子串：age 的 STREAM 密文与明文等长且只做流加密，短片段
 * （1-16 字节）本就有随机重合概率，断言全部子串会产生假阳性。
 */

import fs from 'node:fs';
import path from 'node:path';

import { CliError } from '../errors.js';
import { expandHome, type HomerPaths } from '../paths.js';
import type { HomerConfig } from '../types.js';
import { backupFiles } from '../backup/backup.js';
import { identityFilePath, loadIdentity } from './keys.js';
import { secretNameValid, type AgeCryptoPort } from './types.js';

/** 密文自检的明文采样长度（字节）。 */
const PLAINTEXT_SAMPLE_BYTES = 64;
/** 断言「密文不含明文片段」时片段的最小长度（>16 字节，见文件头）。 */
const MIN_LEAK_FRAGMENT_BYTES = 17;

/**
 * secret 名是否合法：`[A-Za-z0-9][A-Za-z0-9._-]*`（§2.2 冻结）。
 *
 * **权威实现在 `types.ts`**（P0 落地，`src/core/config.ts` 的校验依赖它）。
 * 此处 re-export 以对齐 §2.2 的字面归属（vault.ts 名下列出该函数）。
 */
export { secretNameValid };

/** secret 名的最大长度（`<name>.age` 必须是单个文件名 → ≤ 255 字节，留足余量）。 */
const MAX_SECRET_NAME_BYTES = 128;

/**
 * secret 名 → vault 文件绝对路径（`<secretsDir>/<name>.age`）。
 *
 * 非法 name → `CliError`（不 throw 裸 Error：调用方是 CLI 命令层，需统一渲染 + exit 1）。
 * 重复校验（config 已校验过 `secrets.files` 的键）是纵深防御：本函数也会被
 * `--name` 之类的直接入参路径调用。
 */
export function secretFilePath(paths: HomerPaths, name: string): string {
  if (!secretNameValid(name) || Buffer.byteLength(name, 'utf8') > MAX_SECRET_NAME_BYTES) {
    throw new CliError(
      `非法 secret 名: ${JSON.stringify(name)}`,
      '规则：字母或数字开头，后续只允许 A-Za-z0-9 . _ -（无 `/`，即不支持子目录）',
    );
  }
  return path.join(paths.secretsDir, `${name}.age`);
}

/** secret 名的相对路径（相对 home，供 git pathspec / 报告使用）。 */
export function secretRelativePath(name: string): string {
  if (!secretNameValid(name)) {
    throw new CliError(`非法 secret 名: ${JSON.stringify(name)}`);
  }
  return path.posix.join('secrets', `${name}.age`);
}

/**
 * 取明文采样片段（首 / 中 / 尾）。
 * 极端短明文（< 17 字节）不产生样本 —— 无可断言的片段，直接跳过（见文件头阈值说明）。
 */
function plaintextSamples(plaintext: Buffer): Buffer[] {
  if (plaintext.length < MIN_LEAK_FRAGMENT_BYTES) return [];

  const len = Math.min(plaintext.length, PLAINTEXT_SAMPLE_BYTES);
  const starts = [0, Math.floor((plaintext.length - len) / 2), plaintext.length - len];
  const samples: Buffer[] = [];
  for (const start of [...new Set(starts)]) {
    const sample = plaintext.subarray(start, start + len);
    if (sample.length >= MIN_LEAK_FRAGMENT_BYTES) samples.push(sample);
  }
  return samples;
}

/**
 * 加密明文并**原子写入** `<secretsDir>/<name>.age`。
 *
 * 流程（顺序有意义）：
 *   1. 校验 name（逃逸闸门）与 recipients 非空；
 *   2. `crypto.encrypt`（**全部** recipients，§2.2）；
 *   3. 密文自检：非空 / 不含明文采样片段（§1-D4）——失败**不写任何东西**；
 *   4. `mkdir -p secrets/`（0755：密文可入库，无需收紧）→ tmp + rename 原子写（0644）。
 *
 * 覆盖语义：同 name 重复写入 = 覆盖（§0 Non-goals「secrets 通道无 base 概念，M3 = 整文件覆盖」）。
 */
export async function encryptSecretToFile(
  crypto: AgeCryptoPort,
  paths: HomerPaths,
  name: string,
  plaintext: Buffer,
  recipients: readonly string[],
): Promise<void> {
  const file = secretFilePath(paths, name);

  if (recipients.length === 0) {
    throw new CliError(
      `拒绝加密 ${name}：没有可用的 age recipient`,
      '在 homer.json 的 secrets.recipients 里至少配置一个 `age1...` 公钥',
    );
  }

  const ciphertext = await crypto.encrypt(plaintext, recipients);

  if (ciphertext.length === 0) {
    throw new CliError(`age 加密 ${name} 失败：产出空密文`);
  }
  // 全文兜底（对抗式 review minor 1）：明文 ≤16 字节时采样为空、「不含片段」断言恒真，
  // 一个 passthrough 的假加密层就能把明文原样写进 vault。密文 == 明文（全文相等）
  // 在任何明文长度下都不能是合法加密结果，故这条断言无条件生效。
  if (ciphertext.equals(plaintext) && plaintext.length > 0) {
    throw new CliError(
      `age 加密 ${name} 失败：密文与明文逐字节相同，拒绝写入 vault`,
      '这是实现级事故（加密层未生效），明文绝不入库；请上报该问题，勿提交 secrets/',
    );
  }
  for (const sample of plaintextSamples(plaintext)) {
    if (ciphertext.includes(sample)) {
      throw new CliError(
        `age 加密 ${name} 失败：密文中出现明文片段，拒绝写入 vault`,
        '这是实现级事故（加密层未生效），明文绝不入库；请上报该问题，勿提交 secrets/',
      );
    }
  }

  fs.mkdirSync(paths.secretsDir, { recursive: true });
  const tmp = `${file}.tmp-${process.pid}`;
  try {
    fs.writeFileSync(tmp, ciphertext, { mode: 0o644 });
    fs.renameSync(tmp, file);
  } catch (err) {
    fs.rmSync(tmp, { force: true });
    throw new CliError(
      `写入 vault 文件失败: ${file}（${err instanceof Error ? err.message : '未知错误'}）`,
    );
  }
}

/**
 * 读取并解密 `<secretsDir>/<name>.age`（§2.2 冻结签名：只注入 `crypto`）。
 *
 * identity 由本函数从 `<keysDir>/age.txt` 自行加载（`loadIdentity`）——§2.2 的签名没有
 * identity 入参，而 §2.6 pull 流程与 §2.5 的 doctor `checkAge`（「逐个
 * `decryptSecretFromFile`」）都按这个签名调用，故「取本机 identity」是它的内在职责。
 *
 * 失败语义：
 *   - vault 文件缺失 → `CliError`（调用方据此产出 `missing-vault`，§2.6）；
 *   - 本机 identity 缺失 / 损坏 → `CliError`（调用方据此产出 `no-identity`）；
 *   - 解密失败 → `crypto.decrypt` 的 `CliError`（`cipher.ts` 已包装成不含密文细节的固定文案）。
 * 三种情况都在**写任何目标文件之前**抛出，满足 §2.6 的「全有或全无」。
 */
export async function decryptSecretFromFile(
  crypto: AgeCryptoPort,
  paths: HomerPaths,
  name: string,
): Promise<Buffer> {
  const file = secretFilePath(paths, name);

  let ciphertext: Buffer;
  try {
    ciphertext = fs.readFileSync(file);
  } catch {
    throw new CliError(
      `vault 文件不存在: ${file}`,
      '远端可能还没 push 这个密钥（secrets/ 未入库）；确认路径与 secret 名后重试',
    );
  }

  const identity = loadIdentity(paths);
  if (identity === undefined) {
    throw new CliError(
      `未找到可用的 age identity: ${identityFilePath(paths)}`,
      '先在本机运行 `homer secret keygen`；若要从旧机迁移，请在旧机把本机 recipient 加入 secrets.recipients 后重新 `homer secret push`',
    );
  }

  return crypto.decrypt(ciphertext, identity);
}

/** `listSecrets` / 目标路径解析都用到的单条配置项。 */
export interface VaultEntryStatus {
  name: string;
  destination: string;
  vaultFile: 'present' | 'missing';
}

/* ------------------------------------------------------------------ */
/* secrets.files 的口径（唯一实现点，对抗式 review minor 4）              */
/* ------------------------------------------------------------------ */

/**
 * `secrets.files` 的 name 清单（字典序，报告稳定）。
 *
 * **唯一实现点**：`home.ts` 与 `secret.ts` 曾各自持有一份逐字相同的私有实现
 * （对抗式 review minor 4：一处改文案 / 改排序另一处必漂移）。
 */
export function secretNames(config: HomerConfig): string[] {
  return Object.keys(config.secrets?.files ?? {}).sort();
}

/**
 * 配置里某 secret 的目标绝对路径（`~` 展开；config 已校验值以 `~` / `/` 开头）。
 *
 * 同 `secretNames`：从两个命令层的副本提升到此处，实现与错误文案只有一份。
 */
export function destinationOf(config: HomerConfig, name: string): string {
  const raw = config.secrets?.files?.[name];
  if (raw === undefined) {
    throw new CliError(`homer.json 的 secrets.files 不含 ${JSON.stringify(name)}`);
  }
  return path.resolve(expandHome(raw));
}

/* ------------------------------------------------------------------ */
/* 密钥目标写回（唯一实现点，对抗式 review minor 4）                       */
/* ------------------------------------------------------------------ */

/** 密钥备份的目录 / 文件权限（D2：密钥明文只存在于 0600 / 0700 下）。 */
const SECRET_BACKUP_MODES = { dir: 0o700, file: 0o600 } as const;

/** `writeSecretDestinations` 的结果：备份位置 + 实际写入的目标路径。 */
export interface WriteSecretDestinationsResult {
  /** 已存在目标被覆盖前的备份目录（本次无已存在目标 → undefined）。 */
  backupDir?: string;
  /** 实际写入的目标路径（与入参 `names` 同序）。 */
  written: string[];
}

/**
 * 把已解密的明文写回 `secrets.files` 的目标路径（对抗式 review minor 4 的共享实现点）。
 *
 * 步骤（顺序是语义的一部分）：
 *   1. 已存在的目标先备份（label = `secret/<name>`）——备份内容必须是**覆盖前**的旧密钥明文，
 *      故 `backupFiles` 的 `mode: { dir: 0o700, file: 0o600 }` 收紧张权限（对抗式 review M1：
 *      D2 冻结「密钥明文只在 0600」下，备份副本落在 0755 目录是直接违反）；
 *   2. 逐项 `mkdir -p` 父目录 + 写 0600 + 显式 `chmod`（`writeFileSync` 的 mode 只在创建时生效，
 *      覆盖已存在的文件不会改权限）。
 *
 * `command` 进备份目录名（`home` / `secret`），两个命令层共用本函数、各自传自己的名字。
 */
export function writeSecretDestinations(
  paths: HomerPaths,
  config: HomerConfig,
  nameToPlaintext: ReadonlyMap<string, Buffer>,
  command: string,
): WriteSecretDestinationsResult {
  const names = [...nameToPlaintext.keys()];

  let backupDir: string | undefined;
  const existing = names.filter((name) => fs.existsSync(destinationOf(config, name)));
  if (existing.length > 0) {
    backupDir = backupFiles(
      paths,
      command,
      existing.map((name) => ({ sourceAbs: destinationOf(config, name), label: `secret/${name}` })),
      { mode: SECRET_BACKUP_MODES },
    ).backupDir;
  }

  const written: string[] = [];
  for (const name of names) {
    const destination = destinationOf(config, name);
    fs.mkdirSync(path.dirname(destination), { recursive: true });
    fs.writeFileSync(destination, nameToPlaintext.get(name)!, { mode: 0o600 });
    fs.chmodSync(destination, 0o600);
    written.push(destination);
  }

  return backupDir === undefined ? { written } : { backupDir, written };
}

/**
 * 列出 `config.secrets.files` 的每一项及 vault 文件 present/missing（§2.2 冻结）。
 *
 * **纯读**：只看 `<secretsDir>/<name>.age` 是否存在（用 `statSync`，不解密、不读内容、
 * 不碰目标路径）——故不需要 identity，也不会因「本机不是 recipient」而失败。
 * name 按字典序排序（报告稳定可断言）。
 */
export function listSecrets(paths: HomerPaths, config: HomerConfig): VaultEntryStatus[] {
  const files = config.secrets?.files ?? {};
  const names = Object.keys(files).sort();

  return names.map((name) => ({
    name,
    destination: files[name]!,
    vaultFile: fs.existsSync(path.join(paths.secretsDir, `${name}.age`)) ? 'present' : 'missing',
  }));
}
