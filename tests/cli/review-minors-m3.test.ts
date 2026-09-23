/**
 * M3 对抗式 review · minor 1-5 的收敛回归。
 *
 * 与 `tests/cli/review-minors.test.ts`（M2 版）同款纪律：这些 minor 的共同点是
 * **同一个事实曾在多处各写一份**，于是任何一处漂移都会让「同一件事」在不同命令里
 * 给出不同结论。本文件钉住「只有一个实现点」与前缀闸门：
 *
 *   1. vault 密文自检的**全文兜底**（≤16B 明文不再让断言整体失效）
 *   2. `home` clone 后的 TOCTOU 复验（见 tests/cli/home.test.ts 的 M3 review 套件）
 *   3. `home.ts` 文件头的 `--yes` 确认豁免段（文档与实现同源）
 *   4. `secretNames` / `destinationOf` / `cliErrorLines` / 密钥目标写回块提升到 core 层
 *   5. `allowEscape` 裸通配护栏（见 tests/core/config.test.ts 的 M3 review 用例）
 */

import { readFileSync } from 'node:fs';
import path from 'node:path';

import { describe, expect, it } from 'vitest';

import { cliErrorLines, CliError } from '../../src/core/errors.js';
import { destinationOf, secretNames, writeSecretDestinations } from '../../src/core/age/vault.js';
import { secretNames as vaultSecretNames, destinationOf as vaultDestinationOf } from '../../src/core/age/index.js';
import type { HomerConfig } from '../../src/core/types.js';

const read = (rel: string): string => readFileSync(path.join(process.cwd(), rel), 'utf8');

/* ------------------------------------------------------------------ */
/* minor 4：secrets.files 口径与密钥写回块的唯一实现点                  */
/* ------------------------------------------------------------------ */

describe('minor 4 · 命令层不再持 secrets.files 口径副本', () => {
  it('两个命令层都 import core/age 的 secretNames / destinationOf，无就地私有实现', () => {
    for (const rel of ['src/cli/commands/home.ts', 'src/cli/commands/secret.ts']) {
      const source = read(rel);
      // 旧副本的机械证据：私有函数声明 + 就地展开 + 就地排序。
      expect(source, `${rel} 不得再声明私有 secretNames(Of)`).not.toMatch(
        /function secretNames(Of)?\(config: HomerConfig\)/,
      );
      expect(source, `${rel} 不得再声明私有 destinationOf`).not.toMatch(
        /function destinationOf\(config: HomerConfig, name: string\)/,
      );
      expect(source, `${rel} 不得再就地排序 secrets.files 的键`).not.toContain(
        'Object.keys(config.secrets?.files ?? {}).sort()',
      );
      expect(source, `${rel} 不得再就地拼 secrets.files 缺失文案`).not.toContain(
        'homer.json 的 secrets.files 不含',
      );
      // 必须从 core 层取（唯一实现点）。
      expect(source, `${rel} 应从 core/age 取 secretNames / destinationOf`).toContain('secretNames');
    }
  });

  it('两个命令层都 import cliErrorLines，无就地 errorLines 副本', () => {
    for (const rel of ['src/cli/commands/home.ts', 'src/cli/commands/secret.ts']) {
      const source = read(rel);
      expect(source, `${rel} 不得再声明私有 errorLines`).not.toMatch(/function errorLines\(/);
      expect(source, `${rel} 应使用 cliErrorLines`).toContain('cliErrorLines');
    }
  });

  it('secretNames / destinationOf 语义快照（字典序 + `~` 展开 + 缺失即抛 CliError）', () => {
    const config = {
      version: 1,
      adapters: {},
      secrets: { files: { zeta: '/etc/z', alpha: '~/a.env' } },
    } as unknown as HomerConfig;

    expect(secretNames(config)).toEqual(['alpha', 'zeta']);
    expect(destinationOf(config, 'zeta')).toBe('/etc/z');
    expect(destinationOf(config, 'alpha').endsWith('/a.env')).toBe(true);
    expect(() => destinationOf(config, 'missing')).toThrow(CliError);
    expect(() => destinationOf(config, 'missing')).toThrow(/secrets\.files 不含/);
  });

  it('index.ts 门面与 vault.ts 直取是同一函数（无第二份实现）', () => {
    expect(vaultSecretNames).toBe(secretNames);
    expect(vaultDestinationOf).toBe(destinationOf);
    expect(typeof writeSecretDestinations).toBe('function');
  });

  it('cliErrorLines：有 hint → 两行；无 hint → 一行', () => {
    expect(cliErrorLines(new CliError('boom'))).toEqual(['boom']);
    expect(cliErrorLines(new CliError('boom', 'hint-here'))).toEqual(['boom', 'hint-here']);
  });
});

/* ------------------------------------------------------------------ */
/* minor 3：home.ts 文件头的 `--yes` 确认豁免段                         */
/* ------------------------------------------------------------------ */

describe('minor 3 · home 的文件头确认门槛段明示 `--yes` 豁免', () => {
  it('文件头含「--yes 时一切确认豁免」且与 secret pull 语义对齐', () => {
    const source = read('src/cli/commands/home.ts');
    expect(source).toContain('`--yes` 时一切确认豁免');
    expect(source).toContain('与 `secret pull --yes` 同语义');
  });

  it('实现与文档一致：`--yes` 时**不创建 port**（完全不确认）', () => {
    const source = read('src/cli/commands/home.ts');
    // 门槛条件必须整体挂在 `opts.yes !== true` 下。
    expect(source).toMatch(/if \(opts\.yes !== true && \(plan\.actions\.length > 0/);
  });
});

/* ------------------------------------------------------------------ */
/* 参数解析：`--` 分隔符是 C1 修复的 CLI 侧一半                          */
/* ------------------------------------------------------------------ */

describe('C1 · home 的 repoUrl 位置参数解析', () => {
  it('CLI 解析层：`-` 开头的 URL 必须放在 `--` 之后（否则 parseArgs 报错）', async () => {
    const { parseArgs } = await import('node:util');
    expect(() =>
      parseArgs({
        args: ['--upload-pack=evil'],
        options: { home: { type: 'string' }, help: { type: 'boolean', short: 'h' } },
        allowPositionals: true,
        strict: true,
      }),
    ).toThrow(/Unknown option/);
  });

  it('home.ts 入口有 startsWith("-") 守卫（纵深防御第二层）', () => {
    const source = read('src/cli/commands/home.ts');
    expect(source).toContain("repoUrl.startsWith('-')");
    expect(source).toContain('非法 repo URL');
  });
});
