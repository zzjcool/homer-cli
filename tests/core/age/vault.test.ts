/**
 * P1-W1 · `secrets/` vault（`src/core/age/vault.ts`）验收（docs/m3-plan.md §2.2 / §1-D3 / §1-D4 /
 * §3-P1-W1）。
 *
 * 覆盖：一密钥一 `.age` 文件、原子写（tmp 不残留）、**密文不含明文子串断言**、name 逃逸闸门、
 * 多 recipient 端到端、`decryptSecretFromFile` 的缺失/不可解语义、`listSecrets` present/missing。
 *
 * ## 硬约束
 *
 * 全部 identity 用 `generateIdentity()` 临时生成；路径全部 `mkdtemp`（**绝不落真实 HOME**）。
 * 用例还用「注入的假 crypto」锁定 vault 层自身的断言逻辑（与真实 age 实现解耦），
 * 这样即使将来换掉 `cipher.ts` 的底层包，密文自检与逃逸闸门仍被测试保护。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { CliError } from '../../../src/core/errors.js';
import { getHomerPaths, type HomerPaths } from '../../../src/core/paths.js';
import type { HomerConfig } from '../../../src/core/types.js';
import { createAgeCryptoPort } from '../../../src/core/age/cipher.js';
import { generateIdentity, writeIdentityFile } from '../../../src/core/age/keys.js';
import {
  decryptSecretFromFile,
  encryptSecretToFile,
  listSecrets,
  secretFilePath,
  secretNameValid,
  secretRelativePath,
  type VaultEntryStatus,
} from '../../../src/core/age/vault.js';
import type { AgeCryptoPort } from '../../../src/core/age/types.js';

const created: string[] = [];

function tmpPaths(prefix = 'vault'): HomerPaths {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), `homer-w1-${prefix}-`));
  created.push(dir);
  return getHomerPaths({ HOMER_HOME: dir });
}

function config(files?: Record<string, string>, recipients?: string[]): HomerConfig {
  const secrets: HomerConfig['secrets'] = {};
  if (files !== undefined) secrets.files = files;
  if (recipients !== undefined) secrets.recipients = recipients;
  return { version: 1, adapters: {}, secrets };
}

afterEach(() => {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) fs.rmSync(dir, { recursive: true, force: true });
  }
});

describe('W1 · secretFilePath / secretRelativePath（§1-D3 扁平布局防逃逸）', () => {
  it('路径 = <secretsDir>/<name>.age（绝对），相对路径 = secrets/<name>.age', () => {
    const paths = tmpPaths();
    expect(secretFilePath(paths, 'openai')).toBe(path.join(paths.home, 'secrets', 'openai.age'));
    expect(secretRelativePath('openai')).toBe('secrets/openai.age');
  });

  it.each([
    '../escape',
    'sub/dir',
    'a/../../b',
    '.hidden',
    '-leading-dash',
    '/absolute',
    '',
    'a b',
    'a\nb',
    'a\u0000b',
    'a$b',
    'a;b',
  ])('非法 name %j → CliError（可被 CLI 渲染 + exit 1）', (name) => {
    const paths = tmpPaths();
    expect(() => secretFilePath(paths, name)).toThrow(CliError);
    expect(() => secretRelativePath(name)).toThrow(CliError);
  });

  it('`secretNameValid` 全为假：路径函数与校验函数口径一致', () => {
    const paths = tmpPaths();
    for (const name of ['../x', 'a/b', '', '.x', '-x']) {
      expect(secretNameValid(name)).toBe(false);
      expect(() => secretFilePath(paths, name)).toThrow(CliError);
    }
  });

  it('超长 name（>128 字节）→ CliError（防超文件名上限）', () => {
    const paths = tmpPaths();
    expect(() => secretFilePath(paths, 'a'.repeat(129))).toThrow(CliError);
    expect(secretNameValid('a'.repeat(129)) && true).toBe(true); // 正则本身无长度限制
  });

  it('合法 name（含 . _ -、数字开头）正常映射，且路径必在 secretsDir 内', () => {
    const paths = tmpPaths();
    for (const name of ['a', 'A1', '1abc', 'a.b_c-d', 'x'.repeat(128)]) {
      expect(secretNameValid(name), name).toBe(true);
      expect(secretFilePath(paths, name).startsWith(`${paths.secretsDir}${path.sep}`)).toBe(true);
    }
  });
});

describe('W1 · encryptSecretToFile / decryptSecretFromFile roundtrip（§2.2）', () => {
  it('单 recipient：写入 .age → 读回明文一致；文件是 age 二进制密文', async () => {
    const paths = tmpPaths();
    const crypto = createAgeCryptoPort();
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);

    const plaintext = Buffer.from('openai-sk-super-secret-value\n');
    await encryptSecretToFile(crypto, paths, 'openai', plaintext, [identity.recipient]);

    const file = secretFilePath(paths, 'openai');
    expect(fs.existsSync(file)).toBe(true);
    expect(fs.statSync(file).isFile()).toBe(true);

    const onDisk = fs.readFileSync(file);
    expect(onDisk.includes(plaintext)).toBe(false);
    expect(onDisk.toString('utf8')).not.toContain('openai-sk-super-secret-value');

    expect(await decryptSecretFromFile(crypto, paths, 'openai')).toEqual(plaintext);
  });

  it('多 recipient（2 个 identity）：密文各自可解（§3-P1-W1 验收 4 的 vault 侧）', async () => {
    const paths = tmpPaths();
    const crypto = createAgeCryptoPort();
    const a = generateIdentity();
    writeIdentityFile(paths, a);
    const b = generateIdentity();
    const plaintext = Buffer.from('shared-vault-secret-#1');

    await encryptSecretToFile(crypto, paths, 'shared', plaintext, [a.recipient, b.recipient]);

    // a 是本机 identity（vault 从 keys/age.txt 读）→ 可解
    expect(await decryptSecretFromFile(crypto, paths, 'shared')).toEqual(plaintext);

    // b 换到「本机 identity」位置后同样可解（模拟换设备）
    const pathsB = tmpPaths('vault-b');
    writeIdentityFile(pathsB, b);
    fs.mkdirSync(pathsB.secretsDir, { recursive: true });
    fs.copyFileSync(secretFilePath(paths, 'shared'), secretFilePath(pathsB, 'shared'));
    expect(await decryptSecretFromFile(crypto, pathsB, 'shared')).toEqual(plaintext);
  });

  it('二进制明文（含 0x00 / 0xFF）roundtrip 精确相等', async () => {
    const paths = tmpPaths();
    const crypto = createAgeCryptoPort();
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);
    const plaintext = Buffer.from([0, 1, 255, 254, 0, 10, 13, 0, 42, 0, 99, 0, 7, 0, 0, 0, 3, 8]);

    await encryptSecretToFile(crypto, paths, 'bin', plaintext, [identity.recipient]);
    expect(await decryptSecretFromFile(crypto, paths, 'bin')).toEqual(plaintext);
  });

  it('大明文（256 KiB）roundtrip', async () => {
    const paths = tmpPaths();
    const crypto = createAgeCryptoPort();
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);
    const plaintext = Buffer.alloc(256 * 1024, 0x41);

    await encryptSecretToFile(crypto, paths, 'big', plaintext, [identity.recipient]);
    expect(await decryptSecretFromFile(crypto, paths, 'big')).toEqual(plaintext);
  });

  it('同 name 重复写入 = 覆盖（§0 Non-goals：secrets 无 base 概念，整文件覆盖）', async () => {
    const paths = tmpPaths();
    const crypto = createAgeCryptoPort();
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);

    await encryptSecretToFile(crypto, paths, 'k', Buffer.from('first'), [identity.recipient]);
    const first = fs.readFileSync(secretFilePath(paths, 'k'));
    await encryptSecretToFile(crypto, paths, 'k', Buffer.from('second'), [identity.recipient]);
    const second = fs.readFileSync(secretFilePath(paths, 'k'));

    expect(second.equals(first)).toBe(false);
    expect(second.includes(first)).toBe(false);
    expect(await decryptSecretFromFile(crypto, paths, 'k')).toEqual(Buffer.from('second'));
    expect(fs.readdirSync(paths.secretsDir)).toEqual(['k.age']);
  });

  it('多个 secret 一密钥一文件（互不干扰）', async () => {
    const paths = tmpPaths();
    const crypto = createAgeCryptoPort();
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);

    for (const [name, value] of [
      ['a', 'value-a'],
      ['b', 'value-b'],
      ['c', 'value-c'],
    ] as const) {
      await encryptSecretToFile(crypto, paths, name, Buffer.from(value), [identity.recipient]);
    }

    expect(fs.readdirSync(paths.secretsDir).sort()).toEqual(['a.age', 'b.age', 'c.age']);
    for (const [name, value] of [
      ['a', 'value-a'],
      ['b', 'value-b'],
      ['c', 'value-c'],
    ] as const) {
      expect(await decryptSecretFromFile(crypto, paths, name)).toEqual(Buffer.from(value));
    }
  });
});

describe('W1 · 原子写 + 密文自检（§1-D4 / §3-P1-W1 验收 5）', () => {
  it('写入后无 tmp 残留，目录内只有 .age 文件', async () => {
    const paths = tmpPaths();
    const crypto = createAgeCryptoPort();
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);

    await encryptSecretToFile(crypto, paths, 'k', Buffer.from('v'), [identity.recipient]);
    expect(fs.readdirSync(paths.secretsDir)).toEqual(['k.age']);
  });

  it('密文自检：夹具 crypto 返回含明文的「密文」→ CliError 且不写盘（明文绝不入库）', async () => {
    const paths = tmpPaths();
    const plaintext = Buffer.from('this-plaintext-must-never-reach-the-vault-file');

    // 假 crypto：原样返回明文（模拟「加密层失效」这一实现级事故）
    const leaky: AgeCryptoPort = {
      encrypt: (p) => Promise.resolve(Buffer.from(p)),
      decrypt: (c) => Promise.resolve(Buffer.from(c)),
    };

    await expect(
      encryptSecretToFile(leaky, paths, 'leak', plaintext, ['age1' + 'q'.repeat(58)]),
    ).rejects.toThrow(CliError);

    expect(fs.existsSync(paths.secretsDir)).toBe(false);
    expect(fs.existsSync(secretFilePath(paths, 'leak'))).toBe(false);
  });

  it('密文自检覆盖中段/尾段（不只看开头）', async () => {
    const paths = tmpPaths();
    // 长明文：片段采样取首/中/尾；这里在密文里只嵌入「尾段」以验证采样覆盖面
    const plaintext = Buffer.alloc(400, 0x2d);
    const injected = Buffer.from('TAIL-SEGMENT-MARKER-0123456789');
    injected.copy(plaintext, plaintext.length - injected.length);

    const sneaky: AgeCryptoPort = {
      encrypt: (p) => {
        const buf = Buffer.from(p);
        // 尾部片段泄漏，但首段被替换成随机噪声 → 只验首段的自检会漏掉
        buf.fill(0x7a, 0, 100);
        return Promise.resolve(buf);
      },
      decrypt: (c) => Promise.resolve(Buffer.from(c)),
    };

    await expect(
      encryptSecretToFile(sneaky, paths, 'tail', plaintext, ['age1' + 'q'.repeat(58)]),
    ).rejects.toThrow(CliError);
    expect(fs.existsSync(secretFilePath(paths, 'tail'))).toBe(false);
  });

  it('空密文（夹具返回 0 字节）→ CliError 且不写盘', async () => {
    const paths = tmpPaths();
    const empty: AgeCryptoPort = {
      encrypt: () => Promise.resolve(Buffer.alloc(0)),
      decrypt: () => Promise.resolve(Buffer.alloc(0)),
    };
    await expect(
      encryptSecretToFile(empty, paths, 'k', Buffer.from('v'), ['age1' + 'q'.repeat(58)]),
    ).rejects.toThrow(CliError);
    expect(fs.existsSync(secretFilePath(paths, 'k'))).toBe(false);
  });

  it('极短明文（< 17 字节）不触发自检误报（阈值语义）', async () => {
    const paths = tmpPaths();
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);
    // 8 字节明文：片段采样为空 → 正常写入（密文 ≠ 明文，因为真加密生效）
    await encryptSecretToFile(createAgeCryptoPort(), paths, 'tiny', Buffer.from('12345678'), [
      identity.recipient,
    ]);
    expect(await decryptSecretFromFile(createAgeCryptoPort(), paths, 'tiny')).toEqual(
      Buffer.from('12345678'),
    );
  });

  it('密文自检全文兜底：≤16 字节明文 + passthrough 假加密层 → CliError 且不写盘（对抗式 review minor 1）', async () => {
    const paths = tmpPaths();
    // 10 字节明文：片段采样为空（旧实现的断言因此完全失效）。
    const plaintext = Buffer.from('SHORT-KEY!');

    const passthrough: AgeCryptoPort = {
      encrypt: (p) => Promise.resolve(Buffer.from(p)),
      decrypt: (c) => Promise.resolve(Buffer.from(c)),
    };

    await expect(
      encryptSecretToFile(passthrough, paths, 'short', plaintext, ['age1' + 'q'.repeat(58)]),
    ).rejects.toThrow(CliError);
    await expect(
      encryptSecretToFile(passthrough, paths, 'short', plaintext, ['age1' + 'q'.repeat(58)]),
    ).rejects.toThrow(/密文与明文逐字节相同/);

    expect(fs.existsSync(paths.secretsDir)).toBe(false);
    expect(fs.existsSync(secretFilePath(paths, 'short'))).toBe(false);
  });

  it('全文兜底不误报：≤16 字节明文经真加密正常写入', async () => {
    const paths = tmpPaths();
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);
    const plaintext = Buffer.from('SHORT-KEY!');

    await encryptSecretToFile(createAgeCryptoPort(), paths, 'short-ok', plaintext, [identity.recipient]);
    expect(fs.readFileSync(secretFilePath(paths, 'short-ok')).equals(plaintext)).toBe(false);
    expect(await decryptSecretFromFile(createAgeCryptoPort(), paths, 'short-ok')).toEqual(plaintext);
  });

  it('空明文 + passthrough：不因“空 == 空”触发兜底断言（长度 > 0 才比较）', async () => {
    const paths = tmpPaths();
    const passthrough: AgeCryptoPort = {
      encrypt: () => Promise.resolve(Buffer.alloc(0)),
      decrypt: (c) => Promise.resolve(Buffer.from(c)),
    };
    // 空密文由「产出空密文」那条断言拦截（先于全文比较），错误文案应是前者。
    await expect(
      encryptSecretToFile(passthrough, paths, 'empty', Buffer.alloc(0), ['age1' + 'q'.repeat(58)]),
    ).rejects.toThrow(/产出空密文/);
  });

  it('真实现下密文永远不含明文（多种明文形态 × 多 recipient）', async () => {
    const paths = tmpPaths();
    const crypto = createAgeCryptoPort();
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);

    const cases: string[] = [
      'sk-abcdefghijklmnopqrstuvwxyz0123456789',
      'ghp_' + 'A'.repeat(40),
      'line1\nline2\nline3\n'.repeat(20),
      'x'.repeat(200),
    ];
    for (const [i, value] of cases.entries()) {
      const name = `plain-${i}`;
      const plaintext = Buffer.from(value);
      await encryptSecretToFile(crypto, paths, name, plaintext, [identity.recipient]);
      const onDisk = fs.readFileSync(secretFilePath(paths, name));
      expect(onDisk.includes(plaintext), `name=${name} 密文不得含明文`).toBe(false);
      // 也断言没有任何 32 字节明文窗口出现（更强的「不是明文」保证）
      if (plaintext.length >= 32) {
        for (let offset = 0; offset + 32 <= plaintext.length; offset += 16) {
          expect(onDisk.includes(plaintext.subarray(offset, offset + 32))).toBe(false);
        }
      }
    }
  });

  it('加密失败（真实现 + 非法 recipient）→ 不写盘、不留 tmp', async () => {
    const paths = tmpPaths();
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);

    await expect(
      encryptSecretToFile(createAgeCryptoPort(), paths, 'bad', Buffer.from('v'), [
        identity.secretKey, // 私钥当 recipient（最常见的误配）
      ]),
    ).rejects.toThrow(CliError);

    expect(fs.existsSync(paths.secretsDir)).toBe(false);
  });

  it('recipients 为空 → CliError 且不写盘（不产出零 recipient 密文）', async () => {
    const paths = tmpPaths();
    await expect(
      encryptSecretToFile(createAgeCryptoPort(), paths, 'k', Buffer.from('v'), []),
    ).rejects.toThrow(CliError);
    expect(fs.existsSync(paths.secretsDir)).toBe(false);
  });

  it('非法 name → CliError（逃逸闸门在写盘之前生效）', async () => {
    const paths = tmpPaths();
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);
    for (const name of ['../escape', 'sub/dir', '']) {
      await expect(
        encryptSecretToFile(createAgeCryptoPort(), paths, name, Buffer.from('v'), [identity.recipient]),
      ).rejects.toThrow(CliError);
    }
    expect(fs.existsSync(paths.secretsDir)).toBe(false);
  });
});

describe('W1 · decryptSecretFromFile 失败语义（§2.6 的 missing-vault / no-identity / undecryptable）', () => {
  it('vault 文件缺失 → CliError（消息含路径，便于 CLI 渲染）', async () => {
    const paths = tmpPaths();
    let message = '';
    try {
      await decryptSecretFromFile(createAgeCryptoPort(), paths, 'nope');
    } catch (err) {
      expect(err).toBeInstanceOf(CliError);
      message = (err as Error).message;
    }
    expect(message).toContain(secretFilePath(paths, 'nope'));
  });

  it('vault 存在但本机无 identity → CliError（提示 keygen，不泄漏）', async () => {
    const paths = tmpPaths();
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);
    await encryptSecretToFile(createAgeCryptoPort(), paths, 'k', Buffer.from('v'), [identity.recipient]);

    fs.rmSync(paths.keysDir, { recursive: true, force: true }); // 模拟换设备/未 keygen
    let message = '';
    try {
      await decryptSecretFromFile(createAgeCryptoPort(), paths, 'k');
    } catch (err) {
      expect(err).toBeInstanceOf(CliError);
      message = (err as Error).message;
    }
    expect(message).toContain('identity');
    expect(message).not.toContain(identity.secretKey);
  });

  it('本机 identity 不是 recipient → CliError，且消息不含密文内容', async () => {
    const paths = tmpPaths();
    const owner = generateIdentity();
    const outsider = generateIdentity();
    writeIdentityFile(paths, owner);
    const plaintext = Buffer.from('owner-only-secret');
    await encryptSecretToFile(createAgeCryptoPort(), paths, 'k', plaintext, [owner.recipient]);

    // 换成 outsider 的 identity
    fs.rmSync(paths.keysDir, { recursive: true, force: true });
    writeIdentityFile(paths, outsider);

    let message = '';
    try {
      await decryptSecretFromFile(createAgeCryptoPort(), paths, 'k');
      throw new Error('应当抛出 CliError');
    } catch (err) {
      expect(err).toBeInstanceOf(CliError);
      message = (err as Error).message;
    }
    expect(message).not.toContain('owner-only-secret');
    expect(message).not.toContain(outsider.secretKey);
  });

  it('密文损坏 → CliError（不解出错误明文）', async () => {
    const paths = tmpPaths();
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);
    await encryptSecretToFile(createAgeCryptoPort(), paths, 'k', Buffer.from('v'), [identity.recipient]);

    const file = secretFilePath(paths, 'k');
    const tampered = Buffer.from(fs.readFileSync(file));
    tampered[tampered.length - 1] = (tampered[tampered.length - 1]! ^ 0xff) & 0xff;
    fs.writeFileSync(file, tampered);

    await expect(decryptSecretFromFile(createAgeCryptoPort(), paths, 'k')).rejects.toThrow(CliError);
  });

  it('空 vault 文件 → CliError', async () => {
    const paths = tmpPaths();
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);
    fs.mkdirSync(paths.secretsDir, { recursive: true });
    fs.writeFileSync(secretFilePath(paths, 'k'), Buffer.alloc(0));

    await expect(decryptSecretFromFile(createAgeCryptoPort(), paths, 'k')).rejects.toThrow(CliError);
  });
});

describe('W1 · listSecrets（§2.2 / §3-P1-W1 验收 7）', () => {
  it('present / missing 并存；纯读（不解密、无需 identity）', async () => {
    const paths = tmpPaths();
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);
    await encryptSecretToFile(createAgeCryptoPort(), paths, 'exists', Buffer.from('v'), [
      identity.recipient,
    ]);

    const files = {
      exists: '~/.config/openai/key',
      absent: '/etc/homer/gh.key',
      another: '~/.ssh/id_ed25519',
    };
    const statuses = listSecrets(paths, config(files));

    expect(statuses).toEqual<VaultEntryStatus[]>([
      { name: 'absent', destination: '/etc/homer/gh.key', vaultFile: 'missing' },
      { name: 'another', destination: '~/.ssh/id_ed25519', vaultFile: 'missing' },
      { name: 'exists', destination: '~/.config/openai/key', vaultFile: 'present' },
    ]);
  });

  it('无需 identity 即可运行（换设备 / 未 keygen 也能 list）', () => {
    const paths = tmpPaths();
    expect(listSecrets(paths, config({ a: '~/a' }))).toEqual([
      { name: 'a', destination: '~/a', vaultFile: 'missing' },
    ]);
    expect(fs.existsSync(paths.keysDir)).toBe(false);
  });

  it('secrets 段 / files 段缺省 → 空数组（不 throw）', () => {
    const paths = tmpPaths();
    expect(listSecrets(paths, { version: 1, adapters: {} })).toEqual([]);
    expect(listSecrets(paths, config())).toEqual([]);
  });

  it('按 name 字典序排序（报告稳定可断言）', async () => {
    const paths = tmpPaths();
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);
    for (const name of ['zeta', 'alpha', 'Beta', 'beta', '10', '2']) {
      await encryptSecretToFile(createAgeCryptoPort(), paths, name, Buffer.from('v'), [
        identity.recipient,
      ]);
    }

    const files: Record<string, string> = {};
    for (const name of ['zeta', 'alpha', 'Beta', 'beta', '10', '2']) files[name] = `~/x/${name}`;

    const names = listSecrets(paths, config(files)).map((s) => s.name);
    expect(names).toEqual(['10', '2', 'Beta', 'alpha', 'beta', 'zeta']);
    expect(listSecrets(paths, config(files)).every((s) => s.vaultFile === 'present')).toBe(true);
  });

  it('.age 目录被当成文件放置 → 视为 present（只看存在性，不做类型检查）', () => {
    const paths = tmpPaths();
    fs.mkdirSync(path.join(paths.secretsDir, 'weird.age'), { recursive: true });
    expect(listSecrets(paths, config({ weird: '~/weird' }))).toEqual([
      { name: 'weird', destination: '~/weird', vaultFile: 'present' },
    ]);
  });

  it('只读：不创建 secrets/ 目录、不修改任何文件', () => {
    const paths = tmpPaths();
    listSecrets(paths, config({ a: '~/a' }));
    expect(fs.existsSync(paths.home) === false || !fs.existsSync(paths.secretsDir)).toBe(true);
  });
});
