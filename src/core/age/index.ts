/**
 * `src/core/age/` 的公共门面（docs/m3-plan.md §2.2 / §3-P1-W1）。
 *
 * 上层（`src/cli/commands/{secret,doctor,home}.ts` 及后续 P2/P3 worker）从这里 import，
 * 不直接深入 `cipher.ts` / `keys.ts` / `vault.ts`，使内部文件划分可自由重构。
 *
 * ## 导出清单的归属约定（避免 `export *` 撞名）
 *
 * §2.2 把 `recipientIsValid` / `secretNameValid` 列为**两个文件共有**（types 是权威实现，
 * keys/vault 各自 re-export 一次以对齐计划字面归属）。因此本文件**逐项显式导出**，
 * 同名的只保留一条（来自 `types.js` 的权威定义），不用 `export *`（那会产生歧义警告）。
 *
 * `createAgeCryptoPort` 的真相：**权威实现在 `cipher.ts`**。`types.ts` 里的同名函数
 * 只是 P0 遗留的 stub（P0 阶段 `cipher.ts` 尚不存在时的占位），P1-W1 已改为从本门面
 * 转出 `cipher.ts` 的实现，保证「`types.createAgeCryptoPort` 与 `cipher.createAgeCryptoPort`
 * 是同一个函数」（否则 `m3-p0-scaffold.test.ts` 之类的调用方会拿到两个不同实现）。
 */

// ---- 类型（types.ts，P0 冻结）----
export type { AgeIdentity, AgeCryptoPort } from './types.js';

// ---- 纯校验（types.ts 权威实现，P0 冻结）----
export { recipientIsValid, secretNameValid } from './types.js';

// ---- 密码学端口（cipher.ts，W1）----
export { createAgeCryptoPort } from './types.js';

// ---- 私钥生成 / 落盘（keys.ts，W1）----
export {
  generateIdentity,
  identityFilePath,
  loadIdentity,
  parseIdentityFile,
  writeIdentityFile,
} from './keys.js';

// ---- vault（vault.ts，W1）----
export {
  decryptSecretFromFile,
  encryptSecretToFile,
  listSecrets,
  secretFilePath,
  secretRelativePath,
} from './vault.js';
export type { VaultEntryStatus } from './vault.js';
