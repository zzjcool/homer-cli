/**
 * P1-W1 · age 私钥落盘（`src/core/age/keys.ts`）验收（docs/m3-plan.md §2.2 / §1-D2 / §3-P1-W1）。
 *
 * 覆盖：keygen → write → load roundtrip（recipient 一致 + 文件 mode 0600）、**拒绝覆盖**、
 * `parseIdentityFile` 非法输入 → `CliError` 且消息无私钥本体、`loadIdentity` 的容错降级、
 * 原子写（tmp 不残留）、`identityFilePath` 布局。
 *
 * ## 硬约束
 *
 * 全部 identity 用 `generateIdentity()` 临时生成，路径用 `mkdtemp` + `getHomerPaths({HOMER_HOME})`
 * 注入——**绝不落真实 HOME、绝不读写真实 `~/.homer`**。文件权限断言在 Windows 上无意义，
 * 故整个 mode 用例以 `skipIf(process.platform === 'win32')` 保护（本项目目标平台为 POSIX）。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { CliError } from '../../../src/core/errors.js';
import { getHomerPaths, type HomerPaths } from '../../../src/core/paths.js';
import {
  generateIdentity,
  identityFilePath,
  loadIdentity,
  parseIdentityFile,
  recipientIsValid,
  writeIdentityFile,
} from '../../../src/core/age/keys.js';

const created: string[] = [];

/** 临时工作区：mkdtemp 根 + `getHomerPaths` 注入（不碰真实 HOME）。 */
function tmpPaths(prefix = 'keys'): HomerPaths {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), `homer-w1-${prefix}-`));
  created.push(dir);
  return getHomerPaths({ HOMER_HOME: dir });
}

function mode(file: string): number {
  return fs.statSync(file).mode & 0o777;
}

afterEach(() => {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) fs.rmSync(dir, { recursive: true, force: true });
  }
});

describe('W1 · identityFilePath（§2.2 / §1-D2：<keysDir>/age.txt）', () => {
  it('路径 = <home>/keys/age.txt，且是绝对路径', () => {
    const paths = tmpPaths();
    expect(identityFilePath(paths)).toBe(path.join(paths.home, 'keys', 'age.txt'));
    expect(identityFilePath(paths)).toBe(path.join(paths.keysDir, 'age.txt'));
    expect(path.isAbsolute(identityFilePath(paths))).toBe(true);
  });

  it('不创建目录、不做存在性检查（纯路径计算）', () => {
    const paths = tmpPaths();
    expect(fs.existsSync(paths.keysDir)).toBe(false);
    identityFilePath(paths);
    expect(fs.existsSync(paths.keysDir)).toBe(false);
  });
});

describe('W1 · keygen → write → load roundtrip（§3-P1-W1 验收 1）', () => {
  it('roundtrip：recipient 一致、secretKey 一致、文件 0600', () => {
    const paths = tmpPaths();
    const generated = generateIdentity();

    expect(recipientIsValid(generated.recipient)).toBe(true);
    expect(fs.existsSync(identityFilePath(paths))).toBe(false);

    writeIdentityFile(paths, generated);

    const file = identityFilePath(paths);
    expect(fs.existsSync(file)).toBe(true);
    expect(mode(file)).toBe(0o600);

    const loaded = loadIdentity(paths);
    expect(loaded).toBeDefined();
    expect(loaded!.recipient).toBe(generated.recipient);
    expect(loaded!.secretKey).toBe(generated.secretKey);
    expect(loaded).toEqual(generated);
  });

  it('文件内容 = 私钥一行 + 换行（兼容 age CLI key 文件格式）', () => {
    const paths = tmpPaths();
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);

    const raw = fs.readFileSync(identityFilePath(paths), 'utf8');
    expect(raw).toBe(`${identity.secretKey}\n`);
    expect(raw.split('\n').filter((l) => l.trim() !== '')).toHaveLength(1);
  });

  it.skipIf(process.platform === 'win32')('目录权限 0700 与文件 0600（收紧读权限）', () => {
    const paths = tmpPaths();
    writeIdentityFile(paths, generateIdentity());
    expect(mode(paths.keysDir)).toBe(0o700);
    expect(mode(identityFilePath(paths))).toBe(0o600);
  });

  it.skipIf(process.platform === 'win32')('umask 宽松时仍强制 0600（不被 umask 放宽）', () => {
    const paths = tmpPaths();
    const previous = process.umask(0o000);
    try {
      writeIdentityFile(paths, generateIdentity());
      expect(mode(identityFilePath(paths))).toBe(0o600);
    } finally {
      process.umask(previous);
    }
  });

  it('写入成功后无 tmp 残留（原子写收尾干净）', () => {
    const paths = tmpPaths();
    writeIdentityFile(paths, generateIdentity());
    expect(fs.readdirSync(paths.keysDir)).toEqual(['age.txt']);
  });

  it('多次 keygen 到不同 home 互不影响（各自独立 identity）', () => {
    const a = tmpPaths('keys-a');
    const b = tmpPaths('keys-b');
    writeIdentityFile(a, generateIdentity());
    writeIdentityFile(b, generateIdentity());
    expect(loadIdentity(a)!.secretKey).not.toBe(loadIdentity(b)!.secretKey);
  });
});

describe('W1 · 拒绝覆盖（§3-P1-W1 验收 2）', () => {
  it('已有 identity 文件时 writeIdentityFile → CliError（不覆盖）', () => {
    const paths = tmpPaths();
    const first = generateIdentity();
    writeIdentityFile(paths, first);

    const before = fs.readFileSync(identityFilePath(paths), 'utf8');
    let message = '';
    let hint = '';
    try {
      writeIdentityFile(paths, generateIdentity());
    } catch (err) {
      expect(err).toBeInstanceOf(CliError);
      message = (err as Error).message;
      hint = (err as CliError).hint ?? '';
    }

    expect(message).toContain('拒绝覆盖');
    expect(message).toContain(identityFilePath(paths));
    expect(hint).not.toBe('');

    // 原文件逐字节未变（覆盖 = 永久销毁旧机可解的历史密文，必须零风险）
    expect(fs.readFileSync(identityFilePath(paths), 'utf8')).toBe(before);
    expect(loadIdentity(paths)).toEqual(first);
  });

  it('重复拒绝（第二次仍报错，幂等无副作用）', () => {
    const paths = tmpPaths();
    writeIdentityFile(paths, generateIdentity());
    for (let i = 0; i < 3; i += 1) {
      expect(() => writeIdentityFile(paths, generateIdentity())).toThrow(CliError);
    }
    expect(fs.readdirSync(paths.keysDir)).toEqual(['age.txt']);
  });

  it('even 空文件占位也拒绝覆盖（存在性检查不读内容）', () => {
    const paths = tmpPaths();
    fs.mkdirSync(paths.keysDir, { recursive: true });
    fs.writeFileSync(identityFilePath(paths), '', { mode: 0o600 });
    expect(() => writeIdentityFile(paths, generateIdentity())).toThrow(CliError);
  });
});

describe('W1 · parseIdentityFile（§2.2 / §3-P1-W1 验收 3）', () => {
  it('接受官方形态（私钥一行）并派生出正确 recipient', () => {
    const identity = generateIdentity();
    expect(parseIdentityFile(identity.secretKey)).toEqual(identity);
    expect(parseIdentityFile(`${identity.secretKey}\n`)).toEqual(identity);
  });

  it('接受注释行 / 空行 / CRLF（兼容 age CLI 的 key 文件）', () => {
    const identity = generateIdentity();
    const content = `# created by homer\r\n\r\n${identity.secretKey}\r\n`;
    expect(parseIdentityFile(content)).toEqual(identity);
  });

  it('小写写法被规范化为官方大写形态（同一 scalar → 同一 recipient）', () => {
    const identity = generateIdentity();
    const parsed = parseIdentityFile(identity.secretKey.toLowerCase());
    expect(parsed.secretKey).toBe(identity.secretKey);
    expect(parsed.recipient).toBe(identity.recipient);
  });

  it.each([
    ['空内容', ''],
    ['只有空白 / 注释', '   \n# comment\n\n'],
    ['前缀不符（recipient）', 'age1qurswpc8qurswpc8qurswpc8qurswpc8qurswpc8qurswpc8qurs95jt69'],
    ['前缀不符（ssh 私钥）', '-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n'],
    ['长度不足', 'AGE-SECRET-KEY-1' + 'Q'.repeat(40)],
    ['含非法字符', `AGE-SECRET-KEY-1${'Q'.repeat(42)}${'!'.repeat(16)}`],
    ['JSON 包装（常见误配）', '{"secretKey":"AGE-SECRET-KEY-1AAA"}'],
  ])('非法输入（%s）→ CliError 且消息不含内容本体', (_label, content) => {
    let message = '';
    try {
      parseIdentityFile(content);
      throw new Error('应当抛出 CliError');
    } catch (err) {
      expect(err).toBeInstanceOf(CliError);
      message = (err as Error).message;
    }

    // 消息不含任何一行内容（内容就是私钥本体；固定格式标记文案不算泄漏，见 cipher.test.ts）
    for (const line of content.split('\n')) {
      const body = line.trim().replaceAll('AGE-SECRET-KEY-1', '').replace(/[^0-9A-Za-z]/g, '');
      if (body.length >= 12) expect(message).not.toContain(body);
    }
  });

  it('多行私钥 → CliError（不静默丢弃其中一个，消息只报行数）', () => {
    const a = generateIdentity();
    const b = generateIdentity();
    let message = '';
    try {
      parseIdentityFile(`${a.secretKey}\n${b.secretKey}\n`);
    } catch (err) {
      expect(err).toBeInstanceOf(CliError);
      message = (err as Error).message;
    }
    // 只报行数，绝不回显任何一行
    expect(message).toContain('2');
    expect(message).not.toContain(a.secretKey);
    expect(message).not.toContain(b.secretKey);
  });

  it('Checksum 错（私钥被改一个字符）→ CliError 且不回显被改后的串', () => {
    const identity = generateIdentity();
    const tampered = `${identity.secretKey.slice(0, -1)}${identity.secretKey.endsWith('A') ? 'C' : 'A'}`;
    let message = '';
    try {
      parseIdentityFile(tampered);
    } catch (err) {
      expect(err).toBeInstanceOf(CliError);
      message = (err as Error).message;
    }
    expect(message).not.toContain(tampered);
    expect(message).not.toContain(identity.secretKey);
  });
});

describe('W1 · loadIdentity 容错（缺失 / 损坏 → undefined，不 throw）', () => {
  it('文件缺失 → undefined（不创建目录、不抛）', () => {
    const paths = tmpPaths();
    expect(loadIdentity(paths)).toBeUndefined();
    expect(fs.existsSync(paths.keysDir)).toBe(false);
  });

  it('keysDir 存在但文件缺失 → undefined', () => {
    const paths = tmpPaths();
    fs.mkdirSync(paths.keysDir, { recursive: true });
    expect(loadIdentity(paths)).toBeUndefined();
  });

  it.each([
    ['空文件', ''],
    ['乱码', 'not a key at all\n'],
    ['JSON 包装', '{"secretKey":"x"}'],
    ['多密钥', 'x'],
  ])('损坏内容（%s）→ undefined 而不 throw', (_label, content) => {
    const paths = tmpPaths();
    fs.mkdirSync(paths.keysDir, { recursive: true });
    fs.writeFileSync(identityFilePath(paths), content, { mode: 0o600 });
    expect(loadIdentity(paths)).toBeUndefined();
  });

  it('目录被当成文件放置（EISDIR）→ undefined', () => {
    const paths = tmpPaths();
    fs.mkdirSync(identityFilePath(paths), { recursive: true });
    expect(loadIdentity(paths)).toBeUndefined();
  });

  it('写坏文件的正确修法是重新 keygen（loadIdentity 报 undefined → 上层提示）', () => {
    const paths = tmpPaths();
    fs.mkdirSync(paths.keysDir, { recursive: true });
    fs.writeFileSync(identityFilePath(paths), 'garbage', { mode: 0o600 });
    expect(loadIdentity(paths)).toBeUndefined();
    // 拒绝覆盖仍在（修坏文件必须显式删，防止误以为 keygen 会静默修复）
    expect(() => writeIdentityFile(paths, generateIdentity())).toThrow(CliError);
  });
});

describe('W1 · generateIdentity 契约（§2.2 冻结同步签名）', () => {
  it('是同步函数（返回对象而非 Promise）', () => {
    const result = generateIdentity();
    expect(result).not.toBeInstanceOf(Promise);
    expect(typeof result.secretKey).toBe('string');
    expect(typeof result.recipient).toBe('string');
  });

  it('不落盘、不产生任何文件（纯内存生成）', () => {
    const paths = tmpPaths();
    for (let i = 0; i < 5; i += 1) generateIdentity();
    expect(fs.existsSync(paths.home) && fs.readdirSync(paths.home)).toEqual([]);
  });

  it('连续生成互不相同', () => {
    const keys = new Set<string>();
    for (let i = 0; i < 25; i += 1) keys.add(generateIdentity().secretKey);
    expect(keys.size).toBe(25);
  });
});
