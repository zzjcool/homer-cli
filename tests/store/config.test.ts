import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { loadConfig, saveConfig, validateConfig } from '../../src/core/config.js';
import { getHomerPaths } from '../../src/core/paths.js';
import type { HomerConfig } from '../../src/core/types.js';

const created: string[] = [];

function tmpHome(): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-config-'));
  created.push(dir);
  return dir;
}

afterEach(() => {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) fs.rmSync(dir, { recursive: true, force: true });
  }
});

/** 一个最小合法配置，便于在用例里浅拷贝后制造单点错误。 */
function validConfig(): HomerConfig {
  return {
    version: 1,
    adapters: {
      pi: {
        root: '~/.pi/agent',
        categories: {
          settings: { paths: ['settings.json'], mode: 'merge' },
          skills: { paths: ['skills/'], mode: 'mirror' },
        },
      },
    },
  };
}

describe('validateConfig — 合法输入', () => {
  it('完整配置通过，返回同一结构', () => {
    const config = validConfig();
    const result = validateConfig(config);
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.config.version).toBe(1);
    expect(Object.keys(result.config.adapters)).toEqual(['pi']);
    expect(result.config.adapters['pi']?.categories['settings']?.mode).toBe('merge');
  });

  it('可选字段（enabled/exclude/excludeKeys/ignore）齐全时通过', () => {
    const result = validateConfig({
      version: 1,
      adapters: {
        pi: {
          root: '~/.pi/agent',
          enabled: true,
          ignore: ['auth.json', 'sessions/'],
          categories: {
            extensions: {
              paths: ['extensions/'],
              mode: 'mirror',
              enabled: false,
              exclude: ['*cache*'],
            },
            models: { paths: ['models.json'], mode: 'merge', excludeKeys: ['apiKeys'] },
          },
        },
      },
    });
    expect(result.ok).toBe(true);
  });

  it('adapters 为空对象也合法（尚未配置任何工具）', () => {
    expect(validateConfig({ version: 1, adapters: {} }).ok).toBe(true);
  });
});

describe('validateConfig — 非法输入（≥6 用例）', () => {
  it('缺 version', () => {
    const { version: _drop, ...rest } = validConfig();
    const result = validateConfig(rest);
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors.join('\n')).toMatch(/version 必须是 1/);
  });

  it('version 非 1（旧版本 / 字符串）', () => {
    const result = validateConfig({ ...validConfig(), version: 2 });
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors.some((e) => e.includes('version'))).toBe(true);
  });

  it('错 mode', () => {
    const config = validConfig();
    // SAFETY: 故意构造非法值以覆盖校验分支；结构其余部分合法。
    config.adapters['pi']!.categories['settings'] = { paths: ['settings.json'], mode: 'copy' as 'merge' };
    const result = validateConfig(config);
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors.join('\n')).toMatch(/adapters\.pi\.categories\.settings\.mode/);
  });

  it('空 paths 数组', () => {
    const config = validConfig();
    config.adapters['pi']!.categories['settings']!.paths = [];
    const result = validateConfig(config);
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors.join('\n')).toMatch(/\.paths 必须是非空字符串数组/);
  });

  it('缺 paths 字段', () => {
    const result = validateConfig({
      version: 1,
      adapters: { pi: { root: '~/.pi/agent', categories: { settings: { mode: 'merge' } } } },
    });
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors.join('\n')).toMatch(/\.paths 必须是非空字符串数组/);
  });

  it('root 非 string', () => {
    const result = validateConfig({
      version: 1,
      adapters: { pi: { root: 42, categories: {} } },
    });
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors.join('\n')).toMatch(/adapters\.pi\.root 必须是非空字符串/);
  });

  it('root 缺失', () => {
    const result = validateConfig({ version: 1, adapters: { pi: { categories: {} } } });
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors.join('\n')).toMatch(/\.root 必须是非空字符串/);
  });

  it('顶层不是对象（null / 数组 / 字符串）', () => {
    for (const bad of [null, [], 'nope', 7]) {
      const result = validateConfig(bad);
      expect(result.ok).toBe(false);
    }
  });

  it('adapters 非对象', () => {
    const result = validateConfig({ version: 1, adapters: [] });
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors.join('\n')).toMatch(/adapters 必须是对象/);
  });

  it('categories 非对象 / category 非对象', () => {
    const a = validateConfig({ version: 1, adapters: { pi: { root: '~/x', categories: 'no' } } });
    expect(a.ok).toBe(false);

    const b = validateConfig({
      version: 1,
      adapters: { pi: { root: '~/x', categories: { settings: 'settings.json' } } },
    });
    expect(b.ok).toBe(false);
  });

  it('可选字段类型错误（enabled / exclude / excludeKeys / ignore）', () => {
    const badEnabled = validateConfig({
      version: 1,
      adapters: { pi: { root: '~/x', enabled: 'yes', categories: {} } },
    });
    expect(badEnabled.ok).toBe(false);

    const badExclude = validateConfig({
      version: 1,
      adapters: {
        pi: { root: '~/x', categories: { skills: { paths: ['skills/'], mode: 'mirror', exclude: 'nope' } } },
      },
    });
    expect(badExclude.ok).toBe(false);

    const badExcludeKeys = validateConfig({
      version: 1,
      adapters: {
        pi: { root: '~/x', categories: { models: { paths: ['models.json'], mode: 'merge', excludeKeys: [1] } } },
      },
    });
    expect(badExcludeKeys.ok).toBe(false);

    const badIgnore = validateConfig({
      version: 1,
      adapters: { pi: { root: '~/x', ignore: ['', 'ok'], categories: {} } },
    });
    expect(badIgnore.ok).toBe(false);
  });

  it('累积多个错误，一次报全', () => {
    const result = validateConfig({ version: 9, adapters: { pi: { root: 1, categories: {} } } });
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors.length).toBeGreaterThanOrEqual(2);
  });
});

describe('loadConfig / saveConfig', () => {
  it('文件不存在 → undefined', () => {
    const paths = getHomerPaths({ HOMER_HOME: tmpHome() });
    expect(loadConfig(paths)).toBeUndefined();
  });

  it('roundtrip：saveConfig → loadConfig 深比较相等', () => {
    const paths = getHomerPaths({ HOMER_HOME: tmpHome() });
    const config = validConfig();
    saveConfig(paths, config);

    expect(fs.existsSync(paths.configFile)).toBe(true);
    expect(loadConfig(paths)).toEqual(config);
  });

  it('写入格式为 2 空格缩进 + 末尾换行', () => {
    const paths = getHomerPaths({ HOMER_HOME: tmpHome() });
    saveConfig(paths, validConfig());
    const text = fs.readFileSync(paths.configFile, 'utf8');
    expect(text.endsWith('\n')).toBe(true);
    expect(text).toContain('\n  "version": 1,');
  });

  it('已存在的配置被覆盖（init --force 依赖此语义）', () => {
    const paths = getHomerPaths({ HOMER_HOME: tmpHome() });
    saveConfig(paths, validConfig());
    const next: HomerConfig = { version: 1, adapters: {} };
    saveConfig(paths, next);
    expect(loadConfig(paths)).toEqual(next);
  });

  it('文件存在但 JSON 非法 → 抛出且信息含路径', () => {
    const paths = getHomerPaths({ HOMER_HOME: tmpHome() });
    fs.mkdirSync(path.dirname(paths.configFile), { recursive: true });
    fs.writeFileSync(paths.configFile, '{ not json', 'utf8');
    expect(() => loadConfig(paths)).toThrow(/不是合法 JSON/);
  });

  it('文件存在但校验失败 → 抛出并列出错误', () => {
    const paths = getHomerPaths({ HOMER_HOME: tmpHome() });
    fs.mkdirSync(path.dirname(paths.configFile), { recursive: true });
    fs.writeFileSync(paths.configFile, JSON.stringify({ version: 3, adapters: {} }), 'utf8');
    expect(() => loadConfig(paths)).toThrow(/配置无效/);
  });

  it('拒绝写入非法配置（不会产生坏 homer.json）', () => {
    const paths = getHomerPaths({ HOMER_HOME: tmpHome() });
    const broken = { version: 1, adapters: { pi: { root: 1, categories: {} } } };
    // SAFETY: 故意传入非法结构以覆盖 saveConfig 的前置校验分支。
    expect(() => saveConfig(paths, broken as unknown as HomerConfig)).toThrow(/拒绝写入非法配置/);
    expect(fs.existsSync(paths.configFile)).toBe(false);
  });
});
