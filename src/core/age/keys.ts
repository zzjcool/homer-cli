/**
 * age 私钥的生成与落盘（docs/m3-plan.md §2.2 / §1-D2，P1-W1 落地）。
 *
 * 布局（§1-D2，冻结）：`<keysDir>/age.txt`，即 `<home>/keys/age.txt`，
 * 权限 **0600**，幂等由 `ensureGitRepo` 的 `.gitignore` 的 `keys/` 行保证**不入库**。
 *
 * ## 安全不变量（本文件的全部设计围绕它）
 *
 * 1. **私钥永不进错误消息 / stdout / 日志**：本文件所有 `CliError` 的文案都是**固定文本**，
 *    绝不拼接文件内容或私钥。唯一的例外通道是把 `identity` 原样返回给调用方（内存内使用）。
 * 2. **绝不覆盖已有私钥**：`writeIdentityFile` 见到已存在文件一律 `CliError`——覆盖等于
 *    永久销毁旧机可解的历史密文（`secrets/` 里已推送的 `.age` 文件）。
 * 3. **原子写**：`<file>.tmp-<pid>` + `rename`（同目录 rename 在同一文件系统上原子），
 *    避免写到一半崩溃留下被 `loadIdentity` 读成「损坏 → undefined」的半个私钥。
 * 4. **读路径不 throw**：`loadIdentity` 是 keygen/push/pull/doctor/home 的共用读口，
 *    缺失或损坏都降级为 `undefined`，由调用方决定是「提示 keygen」还是「警告」——
 *    与 `src/core/state.ts` 的 `loadState` 同款容错策略。
 */

import fs from 'node:fs';
import path from 'node:path';

import { CliError } from '../errors.js';
import type { HomerPaths } from '../paths.js';
import { identityFromSecretKey, newAgeIdentity } from './cipher.js';
import type { AgeIdentity } from './types.js';

/**
 * §2.2 把 `recipientIsValid` 列在 keys.ts 名下，但它的**权威实现**在 `types.ts`
 * （P0 落地：`src/core/config.ts` 的配置校验需要它，若定义在此会把 config 拖进
 * 密码学依赖）。此处**原样 re-export**，使「从 keys.js 取该校验函数」的调用方
 * （§2.2 字面读法）也能工作，同时保持实现唯一一份、零重复。
 * `index.ts` 对同名导出只保留一条路径，避免 `export *` 歧义。
 */
export { recipientIsValid } from './types.js';

/** identity 文件名（§1-D2 冻结：`<home>/keys/age.txt`）。 */
const IDENTITY_FILE_NAME = 'age.txt';

/**
 * 生成一对新 age X25519 identity（**纯内存，同步，不落盘**）。
 *
 * §2.2 冻结签名为同步；实现见 `cipher.ts`（`age-encryption` 的 `generateIdentity()`
 * 是 async，但其内部逻辑同步，故这里用 `node:crypto` 直接做，见该文件头注释）。
 * 生成的 `secretKey` 只回给调用方，永不进错误消息 / 日志。
 */
export function generateIdentity(): AgeIdentity {
  return newAgeIdentity();
}

/** 本机 identity 文件路径（`<keysDir>/age.txt`）。不做存在性检查、不创建目录。 */
export function identityFilePath(paths: HomerPaths): string {
  return path.join(paths.keysDir, IDENTITY_FILE_NAME);
}

/**
 * 解析 identity 文件内容 → `AgeIdentity`。
 *
 * 接受 age 官方 key 文件的最小形态：**首个非空、非 `#` 注释行**即私钥（与 `age` CLI 的
 * `-i` 文件格式兼容，便于将来手工从 age CLI 迁移）。其余行只允许空行 / 注释；出现第二个
 * 私钥行 → `CliError`（`AgeIdentity` 是单密钥模型，静默丢弃一个私钥会让用户以为生效了）。
 *
 * 非法输入（空内容 / 前缀不符 / 长度不对 / checksum 错 / 混用大小写 / 多密钥）一律
 * `CliError`，且**消息绝不含内容本体**（内容通常就是私钥）。
 */
export function parseIdentityFile(content: string): AgeIdentity {
  const lines = content.split(/\r?\n/);
  const keyLines: string[] = [];
  for (const line of lines) {
    const trimmed = line.trim();
    if (trimmed === '' || trimmed.startsWith('#')) continue;
    keyLines.push(trimmed);
  }

  if (keyLines.length === 0) {
    throw new CliError('age identity 文件为空：期望一行 `AGE-SECRET-KEY-1...`');
  }
  if (keyLines.length > 1) {
    // 只报行数，不回显任何一行内容。
    throw new CliError(
      `age identity 文件含 ${keyLines.length} 行私钥：homer 只支持单个 identity（请只保留一个）`,
    );
  }

  // identityFromSecretKey 内部做前缀 / 长度 / checksum 校验，且错误消息不含私钥本体。
  return identityFromSecretKey(keyLines[0]!);
}

/** identity 文件的文本形态：私钥一行 + 末尾换行（与 age CLI 的 key 文件一致）。 */
function serializeIdentity(identity: AgeIdentity): string {
  return `${identity.secretKey}\n`;
}

/**
 * 写入 `<keysDir>/age.txt`（**0600**，原子写，**拒绝覆盖**）。
 *
 * 顺序刻意如此：先检查存在性（并发窗口靠 `rename` 前的存在性判断 + 写前再查一次收窄，
 * 但本命令是单进程 CLI，不做额外锁——见 §0 Non-goals 的 `homer unlock`/并发锁 v2）。
 * `keysDir` 以 0700 创建（目录权限收紧，防同机其他用户列举）。
 */
export function writeIdentityFile(paths: HomerPaths, identity: AgeIdentity): void {
  const file = identityFilePath(paths);

  if (fs.existsSync(file)) {
    throw new CliError(
      `age identity 已存在，拒绝覆盖: ${file}`,
      '若要轮换密钥：先备份该文件并确认 secrets/ 里的密文已可被新 recipient 解开；homer 不提供覆盖写入',
    );
  }

  // 写入前再确认一次私钥合法（防调用方传入手搓对象把垃圾写进 keys/age.txt）。
  const canonical = identityFromSecretKey(identity.secretKey);

  fs.mkdirSync(paths.keysDir, { recursive: true, mode: 0o700 });

  const tmp = `${file}.tmp-${process.pid}`;
  try {
    // mode 传给 open(2)：受 umask 影响只会更严（0600 & ~umask ⊆ 0600），不会更宽。
    fs.writeFileSync(tmp, serializeIdentity(canonical), { encoding: 'utf8', mode: 0o600 });
    fs.chmodSync(tmp, 0o600); // 显式再设一次：对抗"用户 umask 异常"或既有 tmp 文件
    fs.renameSync(tmp, file);
  } catch (err) {
    fs.rmSync(tmp, { force: true }); // 失败不留下半个私钥
    throw err instanceof CliError
      ? err
      : new CliError(`写入 age identity 失败: ${file}（${err instanceof Error ? err.message : '未知错误'}）`);
  }
}

/**
 * 读取本机 identity：缺失 / 不可读 / 损坏 → `undefined`（**不 throw**）。
 *
 * 「损坏」包括：内容非法、多密钥、权限/IO 错误——读路径的职责是让调用方拿到一个
 * 可判定的「有没有」，而不是让整个 CLI 崩掉（对应 §2.6 push/pull 的 `no-identity` 状态、
 * doctor 的 age 检查、home 的 `secrets.skipped`）。
 */
export function loadIdentity(paths: HomerPaths): AgeIdentity | undefined {
  let content: string;
  try {
    content = fs.readFileSync(identityFilePath(paths), 'utf8');
  } catch {
    return undefined;
  }
  try {
    return parseIdentityFile(content);
  } catch {
    return undefined;
  }
}
