/**
 * P1-W2 · `homer.json` 的 P0 additive 校验补测（docs/m2-plan.md §2.0-3 / §3-P1-W2）。
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

import { loadConfig, validateConfig } from '../../src/core/config.js';
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
