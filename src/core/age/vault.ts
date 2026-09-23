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
import type { HomerPaths } from '../paths.js';
import type { HomerConfig } from '../types.js';
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
