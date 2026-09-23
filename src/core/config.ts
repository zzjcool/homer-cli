import fs from 'node:fs';
import path from 'node:path';

import type { HomerConfig } from './types.js';
import { isPlainObject } from './entry-kind.js';
import type { HomerPaths } from './paths.js';
// 纯校验函数从 age/types.ts 取（docs/m3-plan.md §2.0-3）：那两个函数在 P0 就是纯校验实现，
// 不依赖 age-encryption，因此 config 校验不会拖入密码学依赖。
import { recipientIsValid, secretNameValid } from './age/types.js';

export type ConfigResult =
  | { ok: true; config: HomerConfig }
  | { ok: false; errors: string[] };

function checkStringArray(value: unknown, where: string, errors: string[]): void {
  if (!Array.isArray(value)) {
    errors.push(`${where} 必须是字符串数组`);
    return;
  }
  value.forEach((item, i) => {
    if (typeof item !== 'string' || item.length === 0) {
      errors.push(`${where}[${i}] 必须是非空字符串`);
    }
  });
}

function checkOptionalStringArray(value: unknown, where: string, errors: string[]): void {
  if (value === undefined) return;
  checkStringArray(value, where, errors);
}

function checkOptionalBoolean(value: unknown, where: string, errors: string[]): void {
  if (value === undefined) return;
  if (typeof value !== 'boolean') errors.push(`${where} 必须是布尔值`);
}

function validateCategory(raw: unknown, where: string, errors: string[]): void {
  if (!isPlainObject(raw)) {
    errors.push(`${where} 必须是对象`);
    return;
  }

  const paths = raw['paths'];
  if (!Array.isArray(paths) || paths.length === 0) {
    errors.push(`${where}.paths 必须是非空字符串数组`);
  } else {
    checkStringArray(paths, `${where}.paths`, errors);
  }

  const mode = raw['mode'];
  if (mode !== 'merge' && mode !== 'mirror') {
    errors.push(`${where}.mode 必须是 'merge' 或 'mirror'（当前: ${JSON.stringify(mode)}）`);
  }

  checkOptionalBoolean(raw['enabled'], `${where}.enabled`, errors);
  checkOptionalStringArray(raw['exclude'], `${where}.exclude`, errors);
  checkOptionalStringArray(raw['excludeKeys'], `${where}.excludeKeys`, errors);
}

function validateAdapter(raw: unknown, where: string, errors: string[]): void {
  if (!isPlainObject(raw)) {
    errors.push(`${where} 必须是对象`);
    return;
  }

  const root = raw['root'];
  if (typeof root !== 'string' || root.length === 0) {
    errors.push(`${where}.root 必须是非空字符串`);
  }

  const categories = raw['categories'];
  if (!isPlainObject(categories)) {
    errors.push(`${where}.categories 必须是对象`);
  } else {
    for (const [name, category] of Object.entries(categories)) {
      validateCategory(category, `${where}.categories.${name}`, errors);
    }
  }

  checkOptionalBoolean(raw['enabled'], `${where}.enabled`, errors);
  checkOptionalStringArray(raw['ignore'], `${where}.ignore`, errors);
  // additive（docs/m3-plan.md §2.0-1 / §2.0-3）：symlink 逃逸 allowlist，glob 数组，缺省合法。
  checkOptionalStringArray(raw['allowEscape'], `${where}.allowEscape`, errors);
}

/**
 * `backup` 段（docs/m2-plan.md §2.0-3）：可缺省；给出时必须是对象，
 * `keep` 若给出必须是正整数。
 */
function validateBackup(raw: unknown, errors: string[]): void {
  if (raw === undefined) return;
  if (!isPlainObject(raw)) {
    errors.push('backup 必须是对象');
    return;
  }
  const keep = raw['keep'];
  if (keep === undefined) return;
  if (typeof keep !== 'number' || !Number.isInteger(keep) || keep <= 0) {
    errors.push(`backup.keep 必须是正整数（当前: ${JSON.stringify(keep)}）`);
  }
}

/**
 * `secrets` 段（docs/m2-plan.md §2.0-3 + docs/m3-plan.md §2.0-3）：可缺省；给出时必须是对象。
 *
 * 校验项：
 *   - `ignorePaths`：字符串数组（M2）；
 *   - `recipients`（M3）：字符串数组，每项必须是合法 age recipient —— 非法值会让
 *     `secret push` 在加密阶段才报错，故在配置阶段拦下；
 *   - `files`（M3）：`{ <secret 名>: <目标路径> }`，name 须过 `secretNameValid`
 *     （决定 vault 文件名，扁平无子目录防逃逸），值必须以 `~` / `/` 开头
 *     （拒绝相对路径：目标路径的解释与 cwd 无关，只认绝对路径或 `~` 展开）。
 */
function validateSecrets(raw: unknown, errors: string[]): void {
  if (raw === undefined) return;
  if (!isPlainObject(raw)) {
    errors.push('secrets 必须是对象');
    return;
  }
  checkOptionalStringArray(raw['ignorePaths'], 'secrets.ignorePaths', errors);

  const recipients = raw['recipients'];
  if (recipients !== undefined) {
    checkStringArray(recipients, 'secrets.recipients', errors);
    if (Array.isArray(recipients)) {
      recipients.forEach((item, i) => {
        if (typeof item !== 'string' || item.length === 0) return; // 上面已报，不重复
        if (!recipientIsValid(item)) {
          errors.push(`secrets.recipients[${i}] 不是合法的 age recipient（应为 age1 + 58 字符）`);
        }
      });
    }
  }

  const files = raw['files'];
  if (files !== undefined) {
    if (!isPlainObject(files)) {
      errors.push('secrets.files 必须是对象（secret 名 -> 目标路径）');
    } else {
      for (const [name, destination] of Object.entries(files)) {
        if (!secretNameValid(name)) {
          errors.push(`secrets.files 的键 "${name}" 不是合法的 secret 名（应形如 [A-Za-z0-9][A-Za-z0-9._-]*）`);
        }
        if (typeof destination !== 'string' || destination.length === 0) {
          errors.push(`secrets.files.${name} 必须是非空字符串`);
        } else if (!destination.startsWith('~') && !destination.startsWith('/')) {
          errors.push(`secrets.files.${name} 必须以 '~' 或 '/' 开头（当前: ${JSON.stringify(destination)}）`);
        }
      }
    }
  }
}

/**
 * 手写校验 `homer.json` 的原始解析值（零依赖）。
 * 只返回错误信息，不抛异常；`errors` 为空即合法。
 */
export function validateConfig(raw: unknown): ConfigResult {
  const errors: string[] = [];

  if (!isPlainObject(raw)) {
    return { ok: false, errors: ['homer.json 顶层必须是对象'] };
  }

  if (raw['version'] !== 1) {
    errors.push(`version 必须是 1（当前: ${JSON.stringify(raw['version'])}）`);
  }

  const adapters = raw['adapters'];
  if (!isPlainObject(adapters)) {
    errors.push('adapters 必须是对象');
  } else {
    for (const [adapterId, adapter] of Object.entries(adapters)) {
      validateAdapter(adapter, `adapters.${adapterId}`, errors);
    }
  }

  // additive（§2.0-3）：缺省合法。
  validateBackup(raw['backup'], errors);
  validateSecrets(raw['secrets'], errors);

  if (errors.length > 0) return { ok: false, errors };
  // SAFETY: 上面已逐字段校验 version/adapters/categories/root/mode/paths 等全部形状，
  // 且 validateConfig 全程只读不增删键，故 raw 结构此时等价于 HomerConfig。
  return { ok: true, config: raw as unknown as HomerConfig };
}

/**
 * 读取并校验 `homer.json`。
 * 文件不存在 → `undefined`（调用方可提示先 `homer init`）；
 * 文件存在但 JSON 非法 / 校验失败 → 抛 `Error`（附带全部校验错误）。
 */
export function loadConfig(paths: HomerPaths): HomerConfig | undefined {
  let text: string;
  try {
    text = fs.readFileSync(paths.configFile, 'utf8');
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return undefined;
    throw err;
  }

  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch (err) {
    throw new Error(`${paths.configFile} 不是合法 JSON: ${(err as Error).message}`);
  }

  const result = validateConfig(raw);
  if (!result.ok) {
    throw new Error(`${paths.configFile} 配置无效:\n${result.errors.map((e) => `  - ${e}`).join('\n')}`);
  }
  return result.config;
}

/**
 * 写入 `homer.json`（2 空格缩进 + 末尾换行，与手写格式一致）。
 * 先写临时文件再 rename，避免写到一半崩溃留下坏配置。
 */
export function saveConfig(paths: HomerPaths, config: HomerConfig): void {
  const result = validateConfig(config);
  if (!result.ok) {
    throw new Error(`拒绝写入非法配置:\n${result.errors.map((e) => `  - ${e}`).join('\n')}`);
  }

  fs.mkdirSync(path.dirname(paths.configFile), { recursive: true });
  const tmp = `${paths.configFile}.tmp-${process.pid}`;
  fs.writeFileSync(tmp, `${JSON.stringify(config, null, 2)}\n`, 'utf8');
  fs.renameSync(tmp, paths.configFile);
}
