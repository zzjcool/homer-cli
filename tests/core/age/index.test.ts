/**
 * P1-W1 · `src/core/age/index.ts` 门面验收（docs/m3-plan.md §2.2 / §3-P1-W1）。
 *
 * 门面是上层（P2-W6 secret 命令 / P2-W7 doctor / P3-W8 home）的唯一 import 路径，
 * 故这里锁定三件事：
 *   1. §2.2 列出的每个公开符号都能从门面取到（漏导出会在 typecheck 阶段才炸）；
 *   2. 同名符号（§2.2 在 types/keys/vault 三处都列了 `recipientIsValid`/`secretNameValid`）
 *      **只有一份实现**，不因 `export *` 撞名而产生两套口径；
 *   3. `createAgeCryptoPort` 从 `types.js` / `cipher.js` / `index.js` 三条路径拿到的是
 *      **同一个函数对象**（P0 的 `types.ts` 契约 + W1 的 `cipher.ts` 实现不漂移）。
 *
 * 本文件不落任何文件（门面测试不碰 fs），identity 临时生成。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { createRequire } from 'node:module';
import { describe, expect, it } from 'vitest';

import * as ageIndex from '../../../src/core/age/index.js';
import { recipientIsValid, secretNameValid } from '../../../src/core/age/types.js';
import { createAgeCryptoPort as fromCipher, newAgeIdentity } from '../../../src/core/age/cipher.js';
import { generateIdentity, parseIdentityFile } from '../../../src/core/age/keys.js';
import { secretFilePath, secretNameValid as vaultNameValid } from '../../../src/core/age/vault.js';

describe('W1 · age 门面导出完整性（§2.2）', () => {
  it('§2.2 冻结的全部公开符号都从 index 可取', () => {
    for (const name of [
      // types.ts（§2.2 第 1 段）
      'createAgeCryptoPort',
      'recipientIsValid',
      // keys.ts（§2.2 第 2 段）
      'generateIdentity',
      'identityFilePath',
      'writeIdentityFile',
      'loadIdentity',
      'parseIdentityFile',
      // vault.ts（§2.2 第 3 段）
      'secretNameValid',
      'secretFilePath',
      'encryptSecretToFile',
      'decryptSecretFromFile',
      'listSecrets',
    ]) {
      expect(ageIndex, `index.ts 应导出 ${name}`).toHaveProperty(name);
      expect((ageIndex as Record<string, unknown>)[name], `${name} 不应为 undefined`).toBeDefined();
    }
  });

  it('类型导出可用（编译期断言；运行时只验对应值存在）', () => {
    // 类型层面：这些 import 能通过 typecheck 即证明门面导出了对应类型。
    const _types: [
      ageIndex.AgeIdentity,
      ageIndex.AgeCryptoPort,
      ageIndex.VaultEntryStatus,
    ] = [newAgeIdentity(), fromCipher(), { name: 'x', destination: '~/x', vaultFile: 'missing' }];
    expect(_types).toHaveLength(3);
  });
});

describe('W1 · 同名导出的唯一实现（避免 export * 撞名漂移）', () => {
  it('recipientIsValid：index / types 是同一函数', () => {
    expect(ageIndex.recipientIsValid).toBe(recipientIsValid);
  });

  it('secretNameValid：index / types / vault 三处是同一函数', () => {
    expect(ageIndex.secretNameValid).toBe(secretNameValid);
    expect(vaultNameValid).toBe(secretNameValid);
  });

  it('createAgeCryptoPort：index / types / cipher 三处是同一函数对象', async () => {
    const fromTypes = (await import('../../../src/core/age/types.js')).createAgeCryptoPort;
    expect(fromTypes).toBe(fromCipher);
    expect(ageIndex.createAgeCryptoPort).toBe(fromCipher);
  });

  it('从门面取到的 port 真能加解密（不是 stub）', async () => {
    const identity = generateIdentity();
    const port = ageIndex.createAgeCryptoPort();
    const plaintext = Buffer.from('index-facade-roundtrip');

    const ciphertext = await port.encrypt(plaintext, [identity.recipient]);
    expect(await port.decrypt(ciphertext, identity)).toEqual(plaintext);
  });
});

describe('W1 · 门面与各模块行为一致（抽样交叉验证）', () => {
  it('门面函数就是模块函数（同一引用，无包装层）', () => {
    expect(ageIndex.generateIdentity).toBe(generateIdentity);
    expect(ageIndex.parseIdentityFile).toBe(parseIdentityFile);
    expect(ageIndex.secretFilePath).toBe(secretFilePath);
  });

  it('门面导出的 parseIdentityFile 与 keys 行为一致（拒绝非法、接受合法）', () => {
    const identity = generateIdentity();
    expect(ageIndex.parseIdentityFile(identity.secretKey)).toEqual(identity);
    expect(() => ageIndex.parseIdentityFile('nope')).toThrow();
  });
});

/**
 * 惰性加载不变量（`cipher.ts` 文件头有完整论证）。
 *
 * `types.ts` 的文件头写着「本模块零第三方依赖……若在此 import age-encryption 就会让
 * 读 homer.json 也拖入密码学实现」，而 W1 把 `createAgeCryptoPort` 改为从 `types.ts`
 * 转出 `cipher.ts` 的实现——这就产生一条 `config.ts → types.ts → cipher.ts` 的**静态**
 * 依赖链。若 cipher.ts 顶层 import 密码学包，该不变量会被静默破坏（读配置平白多付
 * ~90ms + 加载 @noble 全家桶），且**不会有任何测试报错**。
 *
 * 这里用 Node 的模块加载钩子直接断言「import config.js 时不解析 age-encryption」，
 * 把这条不变量变成可执行回归。
 */
describe('W1 · 惰性加载不变量（config 路径不得拖入 age-encryption）', () => {
  // 仓库根（tests/core/age/index.test.ts → 上溯三层）与两个被测模块的绝对 file URL。
  const here = path.dirname(fileURLToPath(import.meta.url));
  const repoRoot = path.resolve(here, '..', '..', '..');
  const configModuleUrl = pathToFileURL(path.join(repoRoot, 'src', 'core', 'config.ts')).href;
  const cipherModuleUrl = pathToFileURL(path.join(repoRoot, 'src', 'core', 'age', 'cipher.ts')).href;
  /** age-encryption 包入口的绝对 URL（探针在 tmp 目录，裸包名解析不到）。 */
  const ageModuleUrl = pathToFileURL(
    createRequire(import.meta.url).resolve('age-encryption'),
  ).href;
  /** tsx 的 loader 入口（与本仓库 `npm run dev` 用的同一套 TS 加载能力）。 */
  const tsxImport = pathToFileURL(createRequire(import.meta.url).resolve('tsx')).href;

  /**
   * 跑一个子进程：装加载钩子记录每个 specifier 的**解析结果 URL**，然后执行 `body`（TS 源码）。
   *
   * 记录解析后 URL（而非原始 specifier）的原因：探针文件在 `mkdtemp` 出目录里，
   * 裸包名 `age-encryption` 在那里无法解析；用绝对 URL import 则 specifier 不再是包名。
   * 解析结果 URL 两处都能对准（含 `node_modules/age-encryption/`），信号同样决定。
   * 钩子输出写文件，避免与 loader 警告混流。
   */
  async function runProbe(body: string): Promise<string[]> {
    const { spawnSync } = await import('node:child_process');

    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-w1-lazy-'));
    const hookFile = path.join(dir, 'hook.mjs');
    const probeFile = path.join(dir, 'probe.mjs');

    fs.writeFileSync(
      hookFile,
      [
        'import fs from "node:fs";',
        'const out = new URL("./resolved.log", import.meta.url);',
        'fs.writeFileSync(out, "");',
        'export async function resolve(spec, ctx, next) {',
        '  const r = await next(spec, ctx);',
        '  fs.appendFileSync(out, (r.url ?? spec) + "\\n");',
        '  return r;',
        '}',
      ].join('\n'),
    );
    fs.writeFileSync(probeFile, body);

    try {
      const result = spawnSync(
        process.execPath,
        ['--experimental-loader', hookFile, '--import', tsxImport, probeFile],
        { encoding: 'utf8', cwd: repoRoot },
      );
      expect(result.status, `probe 失败:\n${result.stderr}`).toBe(0);
      return fs
        .readFileSync(path.join(dir, 'resolved.log'), 'utf8')
        .split('\n')
        .filter((s) => s !== '');
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  }

  /** 解析结果里是否含 age-encryption 包（含其内部依赖 @noble/@scure）。 */
  function ageCryptoEntries(resolved: readonly string[]): string[] {
    return resolved.filter(
      (url) =>
        url.includes('node_modules/age-encryption/') ||
        url.includes('node_modules/@noble/') ||
        url.includes('node_modules/@scure/'),
    );
  }

  it('钩子本身有效：探针显式 import 密码学包时能记录到它（防假绿）', async () => {
    // 若钩子失效（输出路径写错 / loader 未生效），下面的核心断言会“因看不到
    // age-encryption 而假绿”。先用一个必然加载它的探针证明钩子确实在工作。
    const resolved = await runProbe(
      [
        `await import(${JSON.stringify(configModuleUrl)});`,
        `await import(${JSON.stringify(ageModuleUrl)});`,
      ].join('\n'),
    );
    expect(resolved, '钩子未记录到任何模块').not.toHaveLength(0);
    expect(resolved, '钩子应记录到 age-encryption').toContainEqual(
      expect.stringMatching(/node_modules[/\\]age-encryption[/\\]/),
    );
  });

  it('core 配置路径（config → age/types）不静态拖入密码学实现', async () => {
    // 这是不变量本身。注意只 import config.ts：它在静态图上经 `age/types.ts`
    // 到 `age/cipher.ts`（W1 把 createAgeCryptoPort 从 types.ts 转出）。
    // 若 cipher.ts 顶层 import 密码学包，这里就会看到 age-encryption → 断言失败。
    const resolved = await runProbe(
      [
        `const cfg = await import(${JSON.stringify(configModuleUrl)});`,
        'if (!cfg.validateConfig({ version: 1, adapters: {} }).ok) process.exit(3);',
      ].join('\n'),
    );

    expect(resolved, '应解析到 core/config.ts').toContainEqual(
      expect.stringMatching(/core[/\\]config\.ts$/),
    );
    // age/types.ts（含 createAgeCryptoPort 转出）确实在这条链上——否则本用例没测到点子上
    expect(resolved, '应解析到 age/types.ts').toContainEqual(
      expect.stringMatching(/age[/\\]types\.ts$/),
    );
    expect(
      ageCryptoEntries(resolved),
      '读配置不得拖入 age-encryption/@noble/@scure（否则每个 homer status 都要加载整个密码学实现）',
    ).toEqual([]);
  });

  it('真正加解密时才加载密码学包（惰性而非缺失）', async () => {
    const resolved = await runProbe(
      [
        `const { createAgeCryptoPort, newAgeIdentity } = await import(${JSON.stringify(cipherModuleUrl)});`,
        'const id = newAgeIdentity();',
        'const ct = await createAgeCryptoPort().encrypt(Buffer.from("x"), [id.recipient]);',
        'await createAgeCryptoPort().decrypt(ct, id);',
      ].join('\n'),
    );
    expect(ageCryptoEntries(resolved), '加解密后应加载 age-encryption').not.toEqual([]);
  });
});
