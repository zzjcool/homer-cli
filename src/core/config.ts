import fs from 'node:fs';
import path from 'node:path';

import type { HomerConfig } from './types.js';
import type { HomerPaths } from './paths.js';

export type ConfigResult =
  | { ok: true; config: HomerConfig }
  | { ok: false; errors: string[] };

function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

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
