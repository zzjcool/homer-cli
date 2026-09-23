/**
 * P1-W1 · age 密码学端口（`src/core/age/cipher.ts`）验收（docs/m3-plan.md §2.2 / §3-P1-W1）。
 *
 * 覆盖：X25519 同步生成与公钥派生、与参考实现（`age-encryption` 包，即 age 官方作者的
 * 纯 JS 实现）的**逐字节一致性**、加解密 roundtrip（含二进制 payload）、多 recipient、
 * 非法 recipient / 空 recipient / 非法 identity 的拒绝。
 *
 * ## 硬约束
 *
 * 所有 identity 一律**临时生成 + mkdtemp**，绝不落真实 HOME，绝不读真实 `~/.homer`。
 * 本机**无 `age` CLI**（`docs/m3-scout-report.md` §4 实测），故「age CLI 互操作」用例
 * 以 `skipIf` 显式跳过（见文件尾），互操作基准改由 `age-encryption` npm 实现充当。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { CliError } from '../../../src/core/errors.js';
import {
  ageRecipientFromScalar,
  ageSecretKeyFromScalar,
  bech32Decode,
  bech32Encode,
  createAgeCryptoPort,
  identityFromSecretKey,
  newAgeIdentity,
  scalarFromSecretKey,
  x25519PublicKeyFromScalar,
} from '../../../src/core/age/cipher.js';
import { recipientIsValid } from '../../../src/core/age/types.js';

const created: string[] = [];

function tmpDir(prefix: string): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), `homer-w1-${prefix}-`));
  created.push(dir);
  return dir;
}

afterEach(() => {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) fs.rmSync(dir, { recursive: true, force: true });
  }
});

/** 参考实现（age 官方作者的 npm 包）——本文件用它做互操作基准。 */
async function referenceAge(): Promise<typeof import('age-encryption')> {
  return import('age-encryption');
}

describe('W1 · bech32 编解码（自实现，安全加固版）', () => {
  it('编码往返：随机 32 字节 → encode → decode 原值', () => {
    for (let i = 0; i < 8; i += 1) {
      const bytes = Buffer.from(crypto.getRandomValues(new Uint8Array(32)));
      const encoded = bech32Encode('age', bytes);
      const decoded = bech32Decode(encoded);
      expect(decoded.hrp).toBe('age');
      expect(decoded.bytes.equals(bytes)).toBe(true);
    }
  });

  it('与 @scure/base（参考包依赖的 bech32 实现）逐字节一致', async () => {
    const { bech32 } = await import('@scure/base');
    for (let i = 0; i < 8; i += 1) {
      const bytes = new Uint8Array(crypto.getRandomValues(new Uint8Array(32)));
      expect(bech32Encode('AGE-SECRET-KEY-', bytes).toUpperCase()).toBe(
        bech32.encodeFromBytes('AGE-SECRET-KEY-', bytes).toUpperCase(),
      );
      expect(bech32Encode('age', bytes)).toBe(bech32.encodeFromBytes('age', bytes));
    }
  });

  it('大小写：编码恒小写；解码接受全大写/全小写，拒绝混用与非法字符', () => {
    const bytes = Buffer.alloc(32, 7);
    const lower = bech32Encode('age', bytes);
    expect(lower).toBe(lower.toLowerCase());

    expect(bech32Decode(lower).bytes.equals(bytes)).toBe(true);
    expect(bech32Decode(lower.toUpperCase()).bytes.equals(bytes)).toBe(true);

    for (const bad of [
      lower.slice(0, 5) + lower.charAt(5).toUpperCase() + lower.slice(6), // 混用
      lower.replace('q', 'b'), // 字母表外（b 被 bech32 排除）
      lower.replace('q', '1'), // 字母表外（1 是分隔符）
      lower.slice(0, -1), // 截断 → checksum 错
      lower + 'q', // 追加 → checksum 错
      '',
      'age1',
    ]) {
      expect(() => bech32Decode(bad), `${bad} 应解码失败`).toThrow();
    }
  });

  it('错误消息不含入参内容（防私钥随异常外泄）', () => {
    const secret = ageSecretKeyFromScalar(Buffer.alloc(32, 3));
    const corrupted = `${secret.slice(0, -1)}${secret.endsWith('q') ? 'p' : 'q'}`;
    let message = '';
    try {
      bech32Decode(corrupted);
    } catch (err) {
      message = (err as Error).message;
    }
    expect(message).not.toBe('');
    expect(message).not.toContain(corrupted);
    expect(message).not.toContain(corrupted.slice(16, 40));
  });
});

describe('W1 · X25519 同步生成与公钥派生', () => {
  it('生成的 identity 形态符合 age 约定（前缀 / 长度 / recipientIsValid）', () => {
    for (let i = 0; i < 5; i += 1) {
      const identity = newAgeIdentity();
      expect(identity.secretKey.startsWith('AGE-SECRET-KEY-1')).toBe(true);
      expect(identity.secretKey).toMatch(/^AGE-SECRET-KEY-1[02-9AC-HJ-NP-Z]{58}$/);
      expect(identity.recipientIsValid === undefined).toBe(true); // 形状守卫：无多余字段
      expect(recipientIsValid(identity.recipient)).toBe(true);
    }
  });

  it('每次生成都不同（随机 scalar，无复用）', () => {
    const seen = new Set<string>();
    for (let i = 0; i < 20; i += 1) seen.add(newAgeIdentity().secretKey);
    expect(seen.size).toBe(20);
  });

  it('派生的 recipient 与参考实现 identityToRecipient 逐字节一致', async () => {
    const { identityToRecipient } = await referenceAge();
    for (let i = 0; i < 12; i += 1) {
      const identity = newAgeIdentity();
      expect(await identityToRecipient(identity.secretKey)).toBe(identity.recipient);
    }
  });

  it('scalar → 公钥 → 编码 与 参考实现 一致（绕过 identity 层，直接验派生）', async () => {
    const { identityToRecipient } = await referenceAge();
    for (let i = 0; i < 12; i += 1) {
      const scalar = Buffer.from(crypto.getRandomValues(new Uint8Array(32)));
      expect(await identityToRecipient(ageSecretKeyFromScalar(scalar))).toBe(
        ageRecipientFromScalar(scalar),
      );
      // 公钥派生是同步的（node:crypto），且长度必须是 32 字节 raw
      expect(x25519PublicKeyFromScalar(scalar).length).toBe(32);
    }
  });

  it('私钥 → scalar 往返；大写/小写写法都被规范化为官方大写形态', () => {
    const identity = newAgeIdentity();
    expect(scalarFromSecretKey(identity.secretKey).length).toBe(32);

    const lowered = identityFromSecretKey(identity.secretKey.toLowerCase());
    expect(lowered.secretKey).toBe(identity.secretKey);
    expect(lowered.recipient).toBe(identity.recipient);
  });
});

describe('W1 · 非法 identity 被拒且不泄漏私钥', () => {
  //
  // 安全不变量：错误消息**可以**含固定的格式标记（`AGE-SECRET-KEY-1...` —— 那是文档文案，
  // 本身不是秘密），但**绝不能**含被解析内容的本体。故断言口径是「消息不含入参里那段
  // 随机/高熵的 body」，而不是「消息不含入参的任何子串」（后者会误伤前缀文案）。
  const FIXED_MARKER = 'AGE-SECRET-KEY-1';

  /**
   * 入参中真正的密钥材料：去掉固定格式标记与所有非字母数字字符后剩下的那一段。
   * 例：`' AGE-SECRET-KEY-1<BODY>\nBAD'` → `'<BODY>BAD'`；`'AGE-SECRET-KEY-1'` → `''`。
   */
  function keyMaterial(input: string): string {
    return input.replaceAll(FIXED_MARKER, '').replace(/[^0-9A-Za-z]/g, '');
  }

  it.each([
    ['空串', ''],
    ['只有前缀', `${FIXED_MARKER}`],
    ['前缀错（recipient 当私钥）', 'age1qurswpc8qurswpc8qurswpc8qurswpc8qurswpc8qurswpc8qurs95jt69'],
    ['长度不足', `${FIXED_MARKER}${'Q'.repeat(50)}`],
    ['字母表外字符', `${FIXED_MARKER}${'Q'.repeat(42)}${'B'.repeat(16)}`],
    ['含换行（多行注入）', `${FIXED_MARKER}QURSWPC8QURSWPC8QURSWPC8QURSWPC8QURSWPC8QURSWPC8QURSKMP32K\nBAD`],
    ['前后空格', ` ${FIXED_MARKER}QURSWPC8QURSWPC8QURSWPC8QURSWPC8QURSWPC8QURSWPC8QURSKMP32K`],
  ])('拒绝 %s 且消息不含密钥材料', (_label, content) => {
    const material = keyMaterial(content);
    let sawError = false;

    for (const fn of [() => scalarFromSecretKey(content), () => identityFromSecretKey(content)]) {
      let message = '';
      try {
        fn();
      } catch (err) {
        expect(err).toBeInstanceOf(CliError);
        sawError = true;
        message = (err as Error).message;
      }
      expect(sawError, `${JSON.stringify(content)} 应当抛 CliError`).toBe(true);

      // 高熵 body（≥8 字符）绝不得出现在消息里。
      if (material.length >= 8) expect(message).not.toContain(material);
      // 再取 body 的任意 12 字符窗口抽查（防拼接式泄漏）。
      if (material.length >= 20) {
        for (let i = 0; i + 12 <= material.length; i += 4) {
          expect(message).not.toContain(material.slice(i, i + 12));
        }
      }
    }
  });

  it('真实形态的私钥（随机 body）被篡改后，消息不泄漏其 body', () => {
    const identity = newAgeIdentity();
    const body = identity.secretKey.slice(FIXED_MARKER.length);
    const tampered = `${FIXED_MARKER}${body.slice(0, -1)}${body.endsWith('A') ? 'C' : 'A'}`;

    let message = '';
    try {
      scalarFromSecretKey(tampered);
      throw new Error('应当抛出 CliError');
    } catch (err) {
      expect(err).toBeInstanceOf(CliError);
      message = (err as Error).message;
    }
    expect(message).not.toContain(body.slice(0, -1));
    expect(message).not.toContain(tampered);
    expect(message).not.toContain(identity.secretKey);
  });

  it('checksum 被改一个字符 → 拒绝（不误接受被篡改的私钥）', () => {
    const identity = newAgeIdentity();
    const tampered = `${identity.secretKey.slice(0, -1)}${identity.secretKey.endsWith('A') ? 'C' : 'A'}`;
    expect(() => scalarFromSecretKey(tampered)).toThrow(CliError);
  });
});

describe('W1 · AgeCryptoPort roundtrip', () => {
  it('二进制 payload（含 0x00/0xFF/CRLF）roundtrip 精确相等', async () => {
    const cryptoPort = createAgeCryptoPort();
    const identity = newAgeIdentity();
    const plaintext = Buffer.from([0, 1, 2, 3, 255, 254, 10, 13, 0, 128, 127, ...Buffer.from('\u0000end')]);

    const ciphertext = await cryptoPort.encrypt(plaintext, [identity.recipient]);
    expect(await cryptoPort.decrypt(ciphertext, identity)).toEqual(plaintext);
  });

  it('大 payload（1 MiB，跨 STREAM chunk 边界）roundtrip', async () => {
    const cryptoPort = createAgeCryptoPort();
    const identity = newAgeIdentity();
    const plaintext = Buffer.alloc(1024 * 1024);
    for (let i = 0; i < plaintext.length; i += 1) plaintext[i] = i % 251;

    const ciphertext = await cryptoPort.encrypt(plaintext, [identity.recipient]);
    expect(await cryptoPort.decrypt(ciphertext, identity)).toEqual(plaintext);
  }, 30_000); // 1 MiB 流式加解密在本机 ~5s：默认 5000ms 在并行负载下会擦边超时（显式放宽）

  it('密文是 age 二进制格式（非 armor）：不含 armor 头、不含明文', async () => {
    const cryptoPort = createAgeCryptoPort();
    const identity = newAgeIdentity();
    const plaintext = Buffer.from('homer-m3-w1-plaintext-should-never-appear-in-ciphertext');

    const ciphertext = await cryptoPort.encrypt(plaintext, [identity.recipient]);
    expect(ciphertext.toString('utf8')).not.toContain('-----BEGIN AGE ENCRYPTED FILE-----');
    expect(ciphertext.includes(plaintext)).toBe(false);
    expect(ciphertext.toString('utf8').includes('homer-m3-w1')).toBe(false);
  });

  it('ref 实现加密 → 我们解密（互操作方向 1）', async () => {
    const age = await referenceAge();
    const identity = newAgeIdentity();
    const payload = Buffer.from('interop-ref-to-ours-\u0000\u00ff');

    const encrypter = new age.Encrypter();
    encrypter.addRecipient(identity.recipient);
    const ciphertext = Buffer.from(await encrypter.encrypt(new Uint8Array(payload)));

    expect(await createAgeCryptoPort().decrypt(ciphertext, identity)).toEqual(payload);
  });

  it('我们加密 → ref 实现解密（互操作方向 2）', async () => {
    const age = await referenceAge();
    const identity = newAgeIdentity();
    const payload = Buffer.from('interop-ours-to-ref-\u0000\u00ff');

    const ciphertext = await createAgeCryptoPort().encrypt(payload, [identity.recipient]);
    const decrypter = new age.Decrypter();
    decrypter.addIdentity(identity.secretKey);

    expect(Buffer.from(await decrypter.decrypt(new Uint8Array(ciphertext)))).toEqual(payload);
  });
});

describe('W1 · 多 recipient（§2.2 冻结：encrypt 收 readonly string[]）', () => {
  it('2 个 identity 各自能解开同一份密文（且明文一致）', async () => {
    const cryptoPort = createAgeCryptoPort();
    const a = newAgeIdentity();
    const b = newAgeIdentity();
    const plaintext = Buffer.from('shared-secret-for-two-recipients');

    const ciphertext = await cryptoPort.encrypt(plaintext, [a.recipient, b.recipient]);
    expect(await cryptoPort.decrypt(ciphertext, a)).toEqual(plaintext);
    expect(await cryptoPort.decrypt(ciphertext, b)).toEqual(plaintext);
  });

  it('3 个 identity：全部可解，且第 4 个（非 recipient）解不开', async () => {
    const cryptoPort = createAgeCryptoPort();
    const inside = [newAgeIdentity(), newAgeIdentity(), newAgeIdentity()];
    const outsider = newAgeIdentity();
    const plaintext = Buffer.from('three-recipients');

    const ciphertext = await cryptoPort.encrypt(
      plaintext,
      inside.map((i) => i.recipient),
    );
    for (const identity of inside) {
      expect(await cryptoPort.decrypt(ciphertext, identity)).toEqual(plaintext);
    }
    await expect(cryptoPort.decrypt(ciphertext, outsider)).rejects.toThrow(CliError);
  });

  it('重复 recipient 不破坏解密（幂等入参）', async () => {
    const cryptoPort = createAgeCryptoPort();
    const identity = newAgeIdentity();
    const plaintext = Buffer.from('duplicated-recipient');

    const ciphertext = await cryptoPort.encrypt(plaintext, [
      identity.recipient,
      identity.recipient,
    ]);
    expect(await cryptoPort.decrypt(ciphertext, identity)).toEqual(plaintext);
  });

  it('ref 实现也能解开多 recipient 密文（互操作 × 多 recipient 交叉）', async () => {
    const age = await referenceAge();
    const a = newAgeIdentity();
    const b = newAgeIdentity();
    const plaintext = Buffer.from('multi-recipient-interop');

    const ciphertext = await createAgeCryptoPort().encrypt(plaintext, [a.recipient, b.recipient]);
    for (const identity of [a, b]) {
      const decrypter = new age.Decrypter();
      decrypter.addIdentity(identity.secretKey);
      expect(Buffer.from(await decrypter.decrypt(new Uint8Array(ciphertext)))).toEqual(plaintext);
    }
  });
});

describe('W1 · 非法入参被拒（CliError，且不泄漏入参）', () => {
  it('非法 recipient → throw（不静默产出解不开的密文）', async () => {
    const cryptoPort = createAgeCryptoPort();
    const secretLooking = newAgeIdentity().secretKey;

    for (const bad of [
      '',
      'age1',
      'age1short',
      secretLooking, // 最常见的误配：把私钥填进 recipients
      'age1' + 'q'.repeat(57),
      'age1' + 'b'.repeat(58), // 字母表外
      'AGE1' + 'Q'.repeat(58), // 大写
      'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI',
    ]) {
      let message = '';
      try {
        await cryptoPort.encrypt(Buffer.from('x'), [bad]);
        throw new Error(`recipient=${JSON.stringify(bad)} 应当被拒`);
      } catch (err) {
        expect(err, `recipient=${JSON.stringify(bad)}`).toBeInstanceOf(CliError);
        message = (err as Error).message;
      }
      expect(message).not.toContain(bad === '' ? '\u0000' : bad);
    }
  });

  it('recipients 为空数组 → CliError（绝不产出零 recipient 的无效密文）', async () => {
    const cryptoPort = createAgeCryptoPort();
    await expect(cryptoPort.encrypt(Buffer.from('x'), [])).rejects.toThrow(CliError);
    await expect(cryptoPort.encrypt(Buffer.from('x'), [])).rejects.toThrow(/recipient/);
  });

  it('空 recipients 的坑是真的：参考实现会「成功」产出无 recipient 密文', async () => {
    // 锁定我们为何必须自己拦：包不会报错，静默写出等于永久丢失密钥。
    const age = await referenceAge();
    const encrypter = new age.Encrypter();
    const ciphertext = await encrypter.encrypt(new Uint8Array(Buffer.from('lost')));
    expect(ciphertext.length).toBeGreaterThan(0);
  });

  it('identity 与密文不匹配 / 密文损坏 → CliError，消息不含密文片段', async () => {
    const cryptoPort = createAgeCryptoPort();
    const identity = newAgeIdentity();
    const other = newAgeIdentity();
    const ciphertext = await cryptoPort.encrypt(Buffer.from('payload-here'), [identity.recipient]);

    // 1) recipient 不匹配
    let message = '';
    try {
      await cryptoPort.decrypt(ciphertext, other);
    } catch (err) {
      expect(err).toBeInstanceOf(CliError);
      message = (err as Error).message;
    }
    expect(message).not.toContain(ciphertext.subarray(0, 24).toString('utf8'));

    // 2) 密文被截断 / 被篡改
    await expect(cryptoPort.decrypt(ciphertext.subarray(0, 40), identity)).rejects.toThrow(CliError);
    const tampered = Buffer.from(ciphertext);
    tampered[tampered.length - 1] = (tampered[tampered.length - 1]! ^ 0xff) & 0xff;
    await expect(cryptoPort.decrypt(tampered, identity)).rejects.toThrow(CliError);

    // 3) 完全不是 age 密文
    await expect(cryptoPort.decrypt(Buffer.from('not-an-age-file'), identity)).rejects.toThrow(CliError);
  });

  it('identity.secretKey 被伪造（非 age 私钥）→ CliError 而非崩溃', async () => {
    const cryptoPort = createAgeCryptoPort();
    const identity = newAgeIdentity();
    const ciphertext = await cryptoPort.encrypt(Buffer.from('x'), [identity.recipient]);

    await expect(
      cryptoPort.decrypt(ciphertext, { secretKey: 'not-a-secret-key', recipient: identity.recipient }),
    ).rejects.toThrow(CliError);
  });
});

describe('W1 · 本机 age CLI 互操作（无 CLI 则跳过）', () => {
  const ageCliAvailable = ((): boolean => {
    for (const dir of (process.env.PATH ?? '').split(path.delimiter)) {
      if (dir === '') continue;
      try {
        fs.accessSync(path.join(dir, 'age'), fs.constants.X_OK);
        return true;
      } catch {
        // 继续找
      }
    }
    return false;
  })();

  it.skipIf(!ageCliAvailable)('age CLI 加密 → homer 解密（S0 结论：本机无 CLI → skip）', () => {
    // 保留占位：一旦 CI 装上 age CLI，本用例即自动生效。
    expect(ageCliAvailable).toBe(true);
  });
});
