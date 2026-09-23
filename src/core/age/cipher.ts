/**
 * age 底层密码学封装点（docs/m3-plan.md §2.2 / §1-D1，P1-W1 落地）。
 *
 * 本文件是 `age-encryption` npm 包的**唯一** import 点（连同 `node:crypto` 的
 * X25519 派生）。上层（keys / vault / secret 命令 / doctor / home）只依赖
 * `../types.js` 的 `AgeCryptoPort` / `AgeIdentity`，因此底层实现可整体替换
 * （§3-风险 1 的 fallback：node:crypto + age 格式自实现，只需换本文件）。
 *
 * ## 为什么这里还要手写 bech32 + node:crypto 派生
 *
 * §2.2 冻结签名 `generateIdentity(): AgeIdentity` 是**同步**的，而 `age-encryption`
 * 的 `generateIdentity()` / `identityToRecipient()` 都是 **Promise**
 * （`docs/m3-scout-report.md` §3.2/§3.4 实测：内部逻辑本身同步，但 API 异步）。
 * 同步语义对 keygen 很重要（`writeIdentityFile` 也是同步、拒绝覆盖 + 原子写的
 * 文件操作），故这里用 `node:crypto` 直接做 X25519：
 *
 *   - 私钥 = 32 字节随机 scalar（与 `age-encryption` 的 `generateX25519Identity`
 *     完全一致——它也是 `randomBytes(32)` 后直接 bech32 编码，不做 clamp，
 *     clamp 发生在每次 scalar mult 内部）；
 *   - 公钥 = `createPublicKey(createPrivateKey(PKCS#8(prefix||scalar)))` 的 SPKI 末 32 字节；
 *   - 两个字符串都用 BIP-173 bech32 编码（`AGE-SECRET-KEY-` 大写 / `age` 小写）。
 *
 * **与参考实现的逐字节一致性由测试锁定**（`tests/core/age/cipher.test.ts`）：
 * 我们派生的 recipient === `age-encryption` 的 `identityToRecipient(secretKey)`，
 * 且双方密文可互相解密——本机无 `age` CLI（S0 结论），故用官方 npm 实现充当互操作基准。
 *
 * 安全约束（§2.2 / §4-2，冻结）：私钥永不进 stdout / 日志 / 错误消息。因此本文件
 * 所有错误消息**一律不回显入参内容**（`@scure/base` 的 `Invalid checksum in <str>`
 * 这类会回显整串的报错在包内被我们提前拦下，见 `bech32Decode` 注释）。
 */

import { createPrivateKey, createPublicKey, randomBytes } from 'node:crypto';

import { CliError } from '../errors.js';
import { recipientIsValid, type AgeCryptoPort, type AgeIdentity } from './types.js';

/**
 * `age-encryption` 包的**延迟**加载（动态 import，只在真正加解密时触发）。
 *
 * 为什么不用顶层 `import { Encrypter, Decrypter } from 'age-encryption'`：
 * `types.ts` 是本文件的上游（我们 import 它的 `recipientIsValid`），而 `types.ts` 又被
 * `src/core/config.ts` import。「`createAgeCryptoPort` 从 types.ts 转出」意味着
 * `config.ts → types.ts → cipher.ts` 是一条**静态**依赖链；若此处用顶层 import，
 * 「读 homer.json」（`homer status`/`init`/`doctor` 的必做第一步）就会连带加载整包
 * 密码学实现（实测 @noble 系 ~90ms），直接违反 `types.ts` 文件头写下的不变量：
 * 「本模块零第三方依赖……若在此 import age-encryption 就会让读 homer.json 也拖入
 * 密码学实现」。改用调用期动态 import 后，静态图里只剩 `node:crypto`（内建，零成本），
 * 不变量得以保全，而重活被推迟到真正 `encrypt`/`decrypt` 那一刻（两者本就是 async）。
 *
 * 模块缓存使重复调用只付一次真实开销；`createAgeCryptoPort()` 因此可以保持
 * §2.2 冻结的**同步**签名（真返回 port 对象，不在构造期加载包）。
 */
async function loadAge(): Promise<typeof import('age-encryption')> {
  return import('age-encryption');
}

/** age 私钥的 bech32 HRP（大写输出，与 age 官方一致）。 */
const SECRET_KEY_HRP = 'AGE-SECRET-KEY-';
/** age recipient（X25519 公钥）的 bech32 HRP（小写输出）。 */
const RECIPIENT_HRP = 'age';

/**
 * X25519 私钥的 PKCS#8 DER 前缀（`302e020100300506032b656e04220420`）：
 * 固定 16 字节头 + 32 字节 raw scalar。`node:crypto` 不支持 raw 导入 X25519 私钥，
 * 故拼前缀走 PKCS#8（与 `age-encryption` 内部 `importX25519Key` 同款做法）。
 */
const X25519_PKCS8_PREFIX = Buffer.from('302e020100300506032b656e04220420', 'hex');

// ---------------------------------------------------------------------------
// bech32（BIP-173 原版，checksum 常量 1；age 用 bech32 而非 bech32m）
// ---------------------------------------------------------------------------

/** BIP-173 字母表（不含 `1` `b` `i` `o`）。 */
const BECH32_CHARSET = 'qpzry9x8gf2tvdw0s3jn54khce6mua7l';

/** BIP-173 polymod 生成元。 */
const BECH32_GENERATOR = [0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3] as const;

/** 一轮 polymod 状态转移（先取高位、再异或生成元）。 */
function bech32PolymodStep(pre: number): number {
  const top = pre >>> 25;
  let chk = (pre & 0x1ffffff) << 5;
  for (let i = 0; i < BECH32_GENERATOR.length; i += 1) {
    if (((top >>> i) & 1) === 1) chk ^= BECH32_GENERATOR[i]!;
  }
  return chk;
}

/** 计算 6 个 checksum 字符（BIP-173，最终异或常量 1）。 */
function bech32Checksum(hrp: string, words: readonly number[]): number[] {
  const lower = hrp.toLowerCase();
  let chk = 1;
  for (const ch of lower) chk = bech32PolymodStep(chk) ^ (ch.charCodeAt(0) >> 5);
  chk = bech32PolymodStep(chk);
  for (const ch of lower) chk = bech32PolymodStep(chk) ^ (ch.charCodeAt(0) & 31);
  for (const word of words) chk = bech32PolymodStep(chk) ^ word;
  for (let i = 0; i < 6; i += 1) chk = bech32PolymodStep(chk);
  chk ^= 1;
  return [0, 1, 2, 3, 4, 5].map((i) => (chk >>> (5 * (5 - i))) & 31);
}

/** 位宽重组（BIP-173 convertbits）。 */
function convertBits(data: readonly number[], fromBits: number, toBits: number, pad: boolean): number[] {
  let acc = 0;
  let bits = 0;
  const maxValue = (1 << toBits) - 1;
  const out: number[] = [];
  for (const value of data) {
    acc = (acc << fromBits) | value;
    bits += fromBits;
    while (bits >= toBits) {
      bits -= toBits;
      out.push((acc >> bits) & maxValue);
    }
  }
  if (pad && bits > 0) out.push((acc << (toBits - bits)) & maxValue);
  return out;
}

/**
 * bech32 编码（**恒小写输出**，需要大写时由调用方 `.toUpperCase()`，与 age 官方一致：
 * 官方 `generateX25519Identity` 也是先小写编码再整体大写）。
 */
export function bech32Encode(hrp: string, bytes: Uint8Array): string {
  const words = convertBits([...bytes], 8, 5, true);
  const chars = [...words, ...bech32Checksum(hrp, words)].map((digit) => BECH32_CHARSET[digit]!);
  return `${hrp.toLowerCase()}1${chars.join('')}`;
}

/**
 * bech32 解码 → HRP + 字节。
 *
 * 与 `@scure/base` 的差异（**安全相关**）：任何失败都抛**不含入参内容**的错误
 * （包的 `Invalid checksum in ${str}` 会把整串私钥写进错误消息，而我们可能正在
 * 解析 identity 文件内容）。大小写规则同 bech32 规范：全大写 / 全小写均可，
 * **混用非法**。5→8 位还原时不接受非零填充位。
 */
export function bech32Decode(text: string): { hrp: string; bytes: Buffer } {
  const fail = (): never => {
    throw new ElidedError('bech32 解码失败');
  };

  if (text.length < 8 || text.length > 1023) fail();

  const lower = text.toLowerCase();
  const isLower = text === lower;
  const isUpper = text === text.toUpperCase();
  if (!isLower && !isUpper) fail(); // 混用大小写（bech32 规范禁止）

  const sepIndex = lower.lastIndexOf('1');
  if (sepIndex < 1 || sepIndex + 7 > lower.length) fail();

  const hrp = lower.slice(0, sepIndex);
  const dataPart = lower.slice(sepIndex + 1);

  const words: number[] = [];
  for (const ch of dataPart) {
    const digit = BECH32_CHARSET.indexOf(ch);
    if (digit === -1) fail();
    words.push(digit);
  }

  const payload = words.slice(0, -6);
  const expected = bech32Checksum(hrp, payload);
  if (!expected.every((digit, i) => digit === words[words.length - 6 + i])) fail();

  const bytes = convertBits(payload, 5, 8, false);
  if (bytes.length === 0) fail();
  return { hrp, bytes: Buffer.from(bytes) };
}

/** 内部错误载体：`bech32Decode` 用它区分「格式错」与其它异常，消息永不含入参。 */
class ElidedError extends Error {}

// ---------------------------------------------------------------------------
// X25519 密钥物化（bech32 往返 + 同步公钥派生）
// ---------------------------------------------------------------------------

/** 32 字节 scalar → `AGE-SECRET-KEY-1...`（大写）。 */
export function ageSecretKeyFromScalar(scalar: Uint8Array): string {
  return bech32Encode(SECRET_KEY_HRP, scalar).toUpperCase();
}

/** 32 字节 scalar → 32 字节 X25519 公钥（同步，`node:crypto` SPKI 末 32 字节）。 */
export function x25519PublicKeyFromScalar(scalar: Uint8Array): Buffer {
  const priv = createPrivateKey({
    key: Buffer.concat([X25519_PKCS8_PREFIX, Buffer.from(scalar)]),
    format: 'der',
    type: 'pkcs8',
  });
  const spki = createPublicKey(priv).export({ format: 'der', type: 'spki' });
  return Buffer.from(spki.subarray(spki.length - 32));
}

/** 32 字节 scalar → `age1...` recipient（仅当入参确实是 32 字节时合法）。 */
export function ageRecipientFromScalar(scalar: Uint8Array): string {
  return bech32Encode(RECIPIENT_HRP, x25519PublicKeyFromScalar(scalar));
}

/**
 * `AGE-SECRET-KEY-1...` → 32 字节 scalar。非法（HRP 不符 / 长度不是 32 字节 /
 * checksum 错 / 混用大小写）→ `CliError`，**消息不含私钥本体**。
 */
export function scalarFromSecretKey(secretKey: string): Buffer {
  const message = 'age identity 非法：期望一行 `AGE-SECRET-KEY-1...`（32 字节 X25519 私钥）';

  if (typeof secretKey !== 'string' || secretKey.trim() !== secretKey || secretKey === '') {
    throw new CliError(message);
  }

  let decoded;
  try {
    decoded = bech32Decode(secretKey);
  } catch {
    throw new CliError(message);
  }
  if (decoded.hrp.toUpperCase() !== SECRET_KEY_HRP || decoded.bytes.length !== 32) {
    throw new CliError(message);
  }
  return decoded.bytes;
}

/**
 * 从私钥字符串物化 `AgeIdentity`（派生 recipient）。
 * 非法私钥 → `CliError`（消息不含私钥本体）。`parseIdentityFile` / `loadIdentity` 的底座。
 */
export function identityFromSecretKey(secretKey: string): AgeIdentity {
  const scalar = scalarFromSecretKey(secretKey);
  // 统一输出官方形态（大写），故同一个 scalar 的小写写法也会被规范化为大写。
  const canonical = ageSecretKeyFromScalar(scalar);
  return { secretKey: canonical, recipient: ageRecipientFromScalar(scalar) };
}

/** 生成一对新 X25519 identity（纯内存，同步，不落盘）。 */
export function newAgeIdentity(): AgeIdentity {
  const scalar = randomBytes(32);
  return { secretKey: ageSecretKeyFromScalar(scalar), recipient: ageRecipientFromScalar(scalar) };
}

// ---------------------------------------------------------------------------
// AgeCryptoPort（§2.2 冻结接口的唯一实现）
// ---------------------------------------------------------------------------

/** 非法 recipient 的统一报错文案：**不回显入参**（入参可能是误配的私钥）。 */
function invalidRecipientError(recipient: string): CliError {
  const hint = recipient.toUpperCase().startsWith('AGE-SECRET-KEY')
    ? '（该值看起来是 age 私钥：secrets.recipients 只接受 `age1...` 公钥）'
    : '（期望 `age1` 开头的 62 字符 bech32 公钥）';
  return new CliError('非法 age recipient', hint);
}

/**
 * 创建 `AgeCryptoPort`（`age-encryption` 包封装）。
 *
 * 加密：多 recipient（`Encrypter.addRecipient` 逐次调用）→ age 二进制密文（**非 armor**，
 * §1-D3 冻结）。解密：单 identity（`Decrypter.addIdentity`）。
 *
 * 入参防御（上层已由 config 校验拦下，这里是纵深防御 + 给命令层干净的 CliError）：
 *   - `recipients` 为空 → `CliError`：**必须拦住**——`age-encryption` 在零 recipient 时
 *     会「成功」产出一个无 recipient stanza 的密文（实测 103 字节），静默写出等于把
 *     密钥变成永远解不开的垃圾；
 *   - 任一 recipient 不合法 → `CliError`（不回显入参）；
 *   - `identity.secretKey` 不合法 → `CliError`（不回显私钥）。
 *
 * 解密失败（recipient 不匹配 / 密文损坏）→ `CliError`，消息为固定文案：不回显包的错误
 * 细节（包在部分路径上会把密文串进消息），命令层只关心「解不开」这件事（§2.6 的
 * `undecryptable` / doctor 的 age 检查）。
 */
export function createAgeCryptoPort(): AgeCryptoPort {
  return {
    async encrypt(plaintext: Buffer, recipients: readonly string[]): Promise<Buffer> {
      if (recipients.length === 0) {
        throw new CliError(
          '没有可用的 age recipient，拒绝加密',
          '在 homer.json 的 secrets.recipients 里至少配置一个 `age1...` 公钥（旧机 `homer secret keygen` 可生成）',
        );
      }

      const { Encrypter } = await loadAge();
      const encrypter = new Encrypter();
      for (const recipient of recipients) {
        if (!recipientIsValid(recipient)) throw invalidRecipientError(recipient);
        encrypter.addRecipient(recipient);
      }

      try {
        return Buffer.from(await encrypter.encrypt(new Uint8Array(plaintext)));
      } catch (err) {
        throw new CliError(`age 加密失败: ${err instanceof Error ? err.name : '未知错误'}`);
      }
    },

    async decrypt(ciphertext: Buffer, identity: AgeIdentity): Promise<Buffer> {
      const { Decrypter } = await loadAge();
      const decrypter = new Decrypter();
      try {
        decrypter.addIdentity(identity.secretKey);
      } catch {
        throw new CliError('age identity 非法，无法解密');
      }

      try {
        return Buffer.from(await decrypter.decrypt(new Uint8Array(ciphertext)));
      } catch {
        throw new CliError('age 解密失败：identity 与密文不匹配，或密文已损坏');
      }
    },
  };
}
