/**
 * P1-W2 · `homer.json` 的 P0 additive 校验补测（docs/m2-plan.md §2.0-3 / §3-P1-W2）。
 * P1-W3 · 追加 `adapters.*.allowEscape` 校验用例（docs/m3-plan.md §2.0-3 / D7）。
 *
 * `backup.keep` / `secrets.ignorePaths` 的校验实现在 P0 已落地（src/core/config.ts），
 * W2 补上验收要求的用例：`backup.keep: 0` / `'x'`、`secrets.ignorePaths: [42]` 必须报错，
 * 合法值（含整段缺省）必须通过——这些字段直接决定备份保留策略与密钥豁免范围，误放行代价高。
 *
 * 全部临时目录走 `HOMER_HOME=$(mkdtemp -d)`，绝不碰真实 `~/.homer`。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { loadConfig, saveConfig, validateConfig } from '../../src/core/config.js';
import { getHomerPaths, type HomerPaths } from '../../src/core/paths.js';
import type { HomerConfig } from '../../src/core/types.js';

const created: string[] = [];

function tmpPaths(): HomerPaths {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-config-backup-'));
  created.push(dir);
  return getHomerPaths({ HOMER_HOME: dir });
}

afterEach(() => {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) fs.rmSync(dir, { recursive: true, force: true });
  }
});

/** 最小合法配置；用例浅拷贝后只改 `backup` / `secrets` 段。 */
function validConfig(): HomerConfig {
  return {
    version: 1,
    adapters: {
      pi: {
        root: '~/.pi/agent',
        categories: {
          settings: { paths: ['settings.json'], mode: 'merge' },
        },
      },
    },
  };
}

/** 在上面的合法配置上只改 `adapters.pi.allowEscape`（支持故意塞非法值）。 */
function withAllowEscape(allowEscape: unknown): unknown {
  const config = validConfig();
  (config.adapters['pi'] as unknown as Record<string, unknown>)['allowEscape'] = allowEscape;
  return config;
}

describe('validateConfig — backup 段（P0 additive，W2 补测）', () => {
  it('缺省（无 backup 段）→ 合法', () => {
    const result = validateConfig(validConfig());
    expect(result.ok).toBe(true);
  });

  it('backup: {} （keep 缺省）→ 合法', () => {
    const result = validateConfig({ ...validConfig(), backup: {} });
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.config.backup).toEqual({});
  });

  it('backup: { keep: 7 } / { keep: 1 } → 合法', () => {
    for (const keep of [1, 7, 30]) {
      const result = validateConfig({ ...validConfig(), backup: { keep } });
      expect(result.ok, `keep=${keep} 应合法`).toBe(true);
      if (!result.ok) continue;
      expect(result.config.backup?.keep).toBe(keep);
    }
  });

  it('backup.keep: 0 → 报错且错误信息含 backup.keep', () => {
    const result = validateConfig({ ...validConfig(), backup: { keep: 0 } });
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors.join('\n')).toMatch(/backup\.keep/);
  });

  it("backup.keep: 'x' → 报错", () => {
    const result = validateConfig({ ...validConfig(), backup: { keep: 'x' } });
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors.join('\n')).toMatch(/backup\.keep/);
  });

  it('backup.keep 负数 / 小数 / null → 全部报错', () => {
    for (const keep of [-1, 1.5, null]) {
      const result = validateConfig({ ...validConfig(), backup: { keep } });
      expect(result.ok, `keep=${JSON.stringify(keep)} 应报错`).toBe(false);
    }
  });

  it('backup 非对象（字符串 / 数组）→ 报错', () => {
    for (const backup of ['7', [7]]) {
      const result = validateConfig({ ...validConfig(), backup });
      expect(result.ok, `backup=${JSON.stringify(backup)} 应报错`).toBe(false);
      if (result.ok) continue;
      expect(result.errors.join('\n')).toMatch(/backup 必须是对象/);
    }
  });
});

describe('validateConfig — secrets 段（P0 additive，W2 补测）', () => {
  it('缺省（无 secrets 段）→ 合法', () => {
    const result = validateConfig(validConfig());
    expect(result.ok).toBe(true);
  });

  it('secrets: {} （ignorePaths 缺省）→ 合法', () => {
    const result = validateConfig({ ...validConfig(), secrets: {} });
    expect(result.ok).toBe(true);
  });

  it('secrets.ignorePaths 为字符串数组 → 合法且原样返回', () => {
    const ignorePaths = ['pi/settings/settings.json', 'pi/skills/**'];
    const result = validateConfig({ ...validConfig(), secrets: { ignorePaths } });
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.config.secrets?.ignorePaths).toEqual(ignorePaths);
  });

  it('secrets.ignorePaths: [42] → 报错', () => {
    const result = validateConfig({ ...validConfig(), secrets: { ignorePaths: [42] } });
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors.join('\n')).toMatch(/secrets\.ignorePaths\[0\]/);
  });

  it('ignorePaths 非数组（字符串 / 对象）或含空串元素 → 报错', () => {
    for (const ignorePaths of ['pi/settings/settings.json', { 0: 'x' }, ['']]) {
      const result = validateConfig({ ...validConfig(), secrets: { ignorePaths } });
      expect(result.ok, `ignorePaths=${JSON.stringify(ignorePaths)} 应报错`).toBe(false);
    }
  });

  it('secrets 非对象 → 报错', () => {
    const result = validateConfig({ ...validConfig(), secrets: ['pi/settings/settings.json'] });
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors.join('\n')).toMatch(/secrets 必须是对象/);
  });
});

/* ------------------------------------------------------------------ */
/* adapters.*.allowEscape（P1-W3，§2.0-3 / D7）                          */
/* ------------------------------------------------------------------ */

describe('validateConfig — adapters.*.allowEscape（P1-W3）', () => {
  it('缺省 → 合法（维持 M1 安全边界，不改既有字段要求）', () => {
    const result = validateConfig(validConfig());
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.config.adapters['pi']?.allowEscape).toBeUndefined();
  });

  it('字符串数组 → 合法且原样返回（glob 语义由 scan 侧解释，校验只看形状）', () => {
    const allowEscape = ['skills/agent-browser', 'extensions/*', 'settings.json'];
    const result = validateConfig(withAllowEscape(allowEscape));
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.config.adapters['pi']?.allowEscape).toEqual(allowEscape);
  });

  it('空数组 → 合法（等价缺省：什么都不放行）', () => {
    const result = validateConfig(withAllowEscape([]));
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.config.adapters['pi']?.allowEscape).toEqual([]);
  });

  it('裸通配模式（* / ** / */ / **/，含前导 ./ 与 / 变体）→ 报错（对抗式 review minor 5）', () => {
    for (const bare of ['*', '**', '*/', '**/', './*', '/*', './/*', '/**/']) {
      const result = validateConfig(withAllowEscape([bare]));
      expect(result.ok, JSON.stringify(bare)).toBe(false);
      if (result.ok) continue;
      const text = result.errors.join('\n');
      expect(text, JSON.stringify(bare)).toContain('adapters.pi.allowEscape');
      expect(text, JSON.stringify(bare)).toContain('裸通配模式');
    }
  });

  it('带具体路径的 glob 不误拦（含 `extensions/*` 这类“前缀 + 星”形态）', () => {
    for (const ok of [
      ['skills/agent-browser'],
      ['extensions/*'],
      ['skills/*/inner'],
      ['a*/'],
      ['skills/agent-browser/'],
    ]) {
      const result = validateConfig(withAllowEscape(ok));
      expect(result.ok, JSON.stringify(ok)).toBe(true);
      if (result.ok) expect(result.config.adapters['pi']?.allowEscape).toEqual(ok);
    }
  });

  it('裸通配也拦其它 adapter，且 saveConfig 拒绝落盘', () => {
    const config = validConfig() as unknown as Record<string, unknown>;
    const adapters = config['adapters'] as Record<string, Record<string, unknown>>;
    adapters['herdr'] = { root: '~/.config/herdr', allowEscape: ['**/'], categories: {} };
    const result = validateConfig(config);
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.errors.join('\n')).toContain('adapters.herdr.allowEscape');

    const paths = tmpPaths();
    fs.mkdirSync(paths.home, { recursive: true });
    expect(() => saveConfig(paths, withAllowEscape(['*']) as HomerConfig)).toThrow(/裸通配模式/);
    expect(fs.existsSync(paths.configFile)).toBe(false);
  });

  it('非 string[]（字符串 / 对象 / 数字 / null）→ 报错且信息含 allowEscape', () => {
    for (const value of ['skills/agent-browser', { 0: 'x' }, 42, null]) {
      const result = validateConfig(withAllowEscape(value));
      expect(result.ok, JSON.stringify(value)).toBe(false);
      if (result.ok) continue;
      expect(result.errors.join('\n'), JSON.stringify(value)).toMatch(/allowEscape/);
    }
  });

  it('数组元素非字符串 / 空串 → 报错（错误信息定位到 adapters.pi.allowEscape）', () => {
    for (const value of [[42], ['ok', 7], ['']]) {
      const result = validateConfig(withAllowEscape(value));
      expect(result.ok, JSON.stringify(value)).toBe(false);
      if (result.ok) continue;
      expect(result.errors.join('\n')).toMatch(/adapters\.pi\.allowEscape/);
    }
  });

  it('其它 adapter 的非法 allowEscape 也报错（定位到具体 adapterId）', () => {
    const config = validConfig();
    config.adapters['herdr'] = {
      root: '~/.config/herdr',
      categories: { settings: { paths: ['config.toml'], mode: 'merge' } },
    };
    (config.adapters['herdr'] as unknown as Record<string, unknown>)['allowEscape'] = 'config.toml';

    const result = validateConfig(config);
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors.join('\n')).toMatch(/adapters\.herdr\.allowEscape/);
  });

  it('loadConfig 读回：allowEscape 完整保留；非法值 loadConfig throw', () => {
    const paths = tmpPaths();
    fs.mkdirSync(paths.home, { recursive: true });
    fs.writeFileSync(
      paths.configFile,
      `${JSON.stringify(withAllowEscape(['skills/agent-browser']), null, 2)}\n`,
      'utf8',
    );
    expect(loadConfig(paths)?.adapters['pi']?.allowEscape).toEqual(['skills/agent-browser']);

    fs.writeFileSync(
      paths.configFile,
      `${JSON.stringify(withAllowEscape('skills/agent-browser'), null, 2)}\n`,
      'utf8',
    );
    expect(() => loadConfig(paths)).toThrow(/allowEscape/);
  });

  it('saveConfig 拒绝非法 allowEscape（不落盘）', () => {
    const paths = tmpPaths();
    fs.mkdirSync(paths.home, { recursive: true });
    expect(() => saveConfig(paths, withAllowEscape([42]) as HomerConfig)).toThrow(/allowEscape/);
    expect(fs.existsSync(paths.configFile)).toBe(false);
  });
});

describe('loadConfig — backup / secrets 段端到端（临时 HOMER_HOME）', () => {
  it('写盘后读回：backup.keep 与 secrets.ignorePaths 完整保留', () => {
    const paths = tmpPaths();
    fs.mkdirSync(paths.home, { recursive: true });
    const config = {
      ...validConfig(),
      backup: { keep: 3 },
      secrets: { ignorePaths: ['pi/settings/settings.json'] },
    };
    fs.writeFileSync(paths.configFile, `${JSON.stringify(config, null, 2)}\n`, 'utf8');

    const loaded = loadConfig(paths);
    expect(loaded?.backup?.keep).toBe(3);
    expect(loaded?.secrets?.ignorePaths).toEqual(['pi/settings/settings.json']);
  });

  it('磁盘上是 backup.keep: 0 的非法配置 → loadConfig throw 且信息含 backup.keep', () => {
    const paths = tmpPaths();
    fs.mkdirSync(paths.home, { recursive: true });
    fs.writeFileSync(
      paths.configFile,
      `${JSON.stringify({ ...validConfig(), backup: { keep: 0 } }, null, 2)}\n`,
      'utf8',
    );

    expect(() => loadConfig(paths)).toThrow(/backup\.keep/);
  });
});
