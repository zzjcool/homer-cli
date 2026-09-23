/**
 * age 密钥层类型与 P0 纯校验（docs/m3-plan.md §2.2）。
 *
 * **P0 落地范围（本文件）**：
 *   - 全部类型：`AgeIdentity` / `AgeCryptoPort`
 *   - 两个纯校验函数：`recipientIsValid` / `secretNameValid`
 *     （`validateConfig` 的 additive 校验需要它们，故必须在 P0 可 import；
 *     W1 的 `keys.ts` / `vault.ts` 从这里复用，不另写一套）
 *   - `createAgeCryptoPort` **stub**（`throw CliError('尚未实现')`），实现由 P1-W1 在
 *     `src/core/age/cipher.ts` 内落地（age-encryption 包的唯一封装点）。
 *
 * 本模块**零第三方依赖**（只 import 自家 errors）——它是 config 校验的依赖，若在此 import
 * `age-encryption` 就会让「读 homer.json」也拖入密码学实现。真正的加密实现必须留在
 * `cipher.ts`（W1），这样 S0-scout 若否决 `age-encryption` 也只需换那一个文件。
 *
 * 安全约束（§2.2 / §4-2，冻结）：
 *   - `AgeIdentity.secretKey` 只存在于内存与 `<keysDir>/age.txt`（0600）；
 *   - 私钥**永不**进 vault / store / 报告 / stdout / 日志 / 错误消息。
 */

import { CliError } from '../errors.js';

/**
 * 一对 age X25519 身份（私钥 + 派生公钥）。
 * 字段命名与 age 官方术语对齐（identity / recipient），避免与「公钥/私钥」混用产生歧义。
 */
export interface AgeIdentity {
  secretKey: string;   // 'AGE-SECRET-KEY-1...'，仅存在于内存与 <keysDir>/age.txt（0600）
  recipient: string;   // 派生公钥 'age1...'
}

/**
 * age 加解密的唯一端口（§2.2 冻结）。
 *
 * 所有上层代码（keys / vault / secret 命令 / doctor / home）只依赖本接口，
 * 因此底层包（`age-encryption`）可整体替换而不触及调用方。
 */
export interface AgeCryptoPort {
  encrypt(plaintext: Buffer, recipients: readonly string[]): Promise<Buffer>;
  decrypt(ciphertext: Buffer, identity: AgeIdentity): Promise<Buffer>;
}

/**
 * 创建底层实现（`age-encryption` 包）的唯一封装点。
 *
 * P0 stub：实现由 P1-W1 落地于 `cipher.ts`（本文件不再改动）。
 */
export function createAgeCryptoPort(): AgeCryptoPort {
  throw new CliError('尚未实现');
}

/**
 * recipient（X25519 公钥，age1 + 58 字符 bech32 主体）是否合法。
 *
 * 字符集为 bech32 规范集去掉 `1` / `b` / `i` / `o`（`02-9ac-hj-np-z`），长度固定 58
 * （32 字节公钥 + 6 字符校验和的 bech32 编码，去掉 HRP `age` 与其后的分隔符 `1`）。
 * `recipientIsValid` 是 `secrets.recipients` 的配置校验闸门（§2.0-3）：非法 recipient
 * 会让 `secret push` 在加密阶段才失败，故必须在 load config 阶段拦下。
 */
export function recipientIsValid(recipient: string): boolean {
  return /^age1[02-9ac-hj-np-z]{58}$/.test(recipient);
}

/**
 * secret 名是否合法：`[A-Za-z0-9][A-Za-z0-9._-]*`（§2.2 冻结）。
 *
 * 首字符必须是字母或数字，故 `../x` / `.hidden` / `-x` 一律非法；
 * 字符集不含 `/`，故**无法构造子目录**（配合 `secretFilePath` 的扁平布局防逃逸，§1-D3）。
 * 注意 `.` 与 `..` 本身被首字符规则排除，无需额外分支。
 */
export function secretNameValid(name: string): boolean {
  return /^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(name);
}
