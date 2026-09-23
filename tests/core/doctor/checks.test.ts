/**
 * `src/core/doctor/checks.ts` 单测（docs/m3-plan.md §2.5 / §3-P2-W7）。
 *
 * 覆盖口径：**八项检查各自 ok / warn / fail 至少一例**（验收原文），外加
 *   - age 检查用**注入的假 `AgeCryptoPort`** 造「identity 与密文不匹配」的 fail（§4-4 解耦目的）；
 *   - `__REQUIRED__` 残留的 details **精确到文件 + 键**；
 *   - `--offline` 跳过远端检查、离线时 warn。
 *
 * 全部隔离：`mkdtemp` 临时 home + `git init --bare` 假 origin（`tests/core/git/helpers.ts`），
 * 绝不碰真实 `~/.homer` / `~/.gitconfig`（仓库级身份由 helper 写入）。
 */

import fs from 'node:fs';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import {
  checkAdapters,
  checkAge,
  checkConfig,
  checkMachine,
  checkRemote,
  checkRepoAndStore,
  checkRequiredPlaceholders,
} from '../../../src/core/doctor/checks.js';
import { generateIdentity, writeIdentityFile } from '../../../src/core/age/keys.js';
import { encryptSecretToFile } from '../../../src/core/age/vault.js';
import { createAgeCryptoPort } from '../../../src/core/age/types.js';
import { writeSnapshotToStore } from '../../../src/core/store/store.js';
import { saveState } from '../../../src/core/state.js';
import {
  cleanupTmp,
  cloneRepo,
  configureUser,
  gitOk,
  initBare,
  initRepo,
  mkTmp,
  pathsFor,
  writeFile,
  shaOf,
} from '../git/helpers.js';
import type { AgeCryptoPort } from '../../../src/core/age/types.js';
import type { HomerConfig } from '../../../src/core/types.js';

afterEach(cleanupTmp);

/** 最小合法配置；adapter root 默认指向一个**不存在**的路径（除非用例自己建）。 */
function configWith(overrides: Partial<HomerConfig> = {}): HomerConfig {
  return {
    version: 1,
    adapters: {
      pi: {
        root: '~/.pi/agent',
        categories: { settings: { paths: ['settings.json'], mode: 'merge' } },
      },
    },
    ...overrides,
  };
}

/** 写 homer.json（合法配置）。 */
function writeHomerJson(home: string, config: HomerConfig): void {
  fs.mkdirSync(home, { recursive: true });
  fs.writeFileSync(path.join(home, 'homer.json'), `${JSON.stringify(config, null, 2)}\n`, 'utf8');
}

/* ------------------------------------------------------------------ */
/* ① config                                                            */
/* ------------------------------------------------------------------ */

describe('checkConfig（① config）', () => {
  it('合法配置 → ok，消息含 adapter 清单', () => {
    const home = mkTmp('doctor-config');
    writeHomerJson(home, configWith());

    const check = checkConfig(pathsFor(home));
    expect(check.id).toBe('config');
    expect(check.status).toBe('ok');
    expect(check.message).toContain('pi');
  });

  it('homer.json 缺失 → fail（提示先 init）', () => {
    const home = mkTmp('doctor-config-missing');
    const check = checkConfig(pathsFor(home));
    expect(check.status).toBe('fail');
    expect(check.message).toContain('未找到');
    expect(check.details?.join('\n')).toContain('homer init');
  });

  it('homer.json 不是合法 JSON → fail', () => {
    const home = mkTmp('doctor-config-badjson');
    fs.writeFileSync(path.join(home, 'homer.json'), '{ not json', 'utf8');
    const check = checkConfig(pathsFor(home));
    expect(check.status).toBe('fail');
    expect(check.message).toContain('不是合法 JSON');
  });

  it('homer.json 校验不通过 → fail 且 details 逐条列出错误', () => {
    const home = mkTmp('doctor-config-invalid');
    fs.writeFileSync(
      path.join(home, 'homer.json'),
      JSON.stringify({ version: 2, adapters: {} }),
      'utf8',
    );
    const check = checkConfig(pathsFor(home));
    expect(check.status).toBe('fail');
    expect(check.message).toContain('配置无效');
    expect(check.details?.some((line) => line.includes('version'))).toBe(true);
  });
});

/* ------------------------------------------------------------------ */
/* ② repo / ③ store-clean                                              */
/* ------------------------------------------------------------------ */

/** 一个带假 origin 与 upstream 的临时仓（返回 home 与 bare）。 */
function repoWithOrigin(prefix: string): { home: string; bare: string } {
  const bare = initBare(`${prefix}-origin`);
  const home = initRepo(`${prefix}-home`);
  writeFile(path.join(home, 'store', 'pi', '.keep'), '');
  gitOk(home, ['add', '-A']);
  gitOk(home, ['commit', '-m', 'init']);
  gitOk(home, ['remote', 'add', 'origin', bare]);
  gitOk(home, ['push', '-u', 'origin', 'main']);
  return { home, bare };
}

describe('checkRepoAndStore（② repo / ③ store-clean）', () => {
  it('非 git 仓库 → repo fail + store-clean warn（不可判定）', () => {
    const home = mkTmp('doctor-nonrepo');
    const [repo, store] = checkRepoAndStore(pathsFor(home));
    expect(repo?.id).toBe('repo');
    expect(repo?.status).toBe('fail');
    expect(store?.id).toBe('store-clean');
    expect(store?.status).toBe('warn');
    expect(store?.message).toContain('不可判定');
  });

  it('git 仓库无 upstream → repo warn', () => {
    const home = initRepo('doctor-noupstream');
    const [repo] = checkRepoAndStore(pathsFor(home));
    expect(repo?.status).toBe('warn');
    expect(repo?.message).toContain('upstream');
  });

  it('有 upstream 且 store 干净 → repo ok + store-clean ok', () => {
    const { home } = repoWithOrigin('doctor-clean');
    const [repo, store] = checkRepoAndStore(pathsFor(home));
    expect(repo?.status).toBe('ok');
    expect(repo?.message).toContain('origin/main');
    expect(store?.status).toBe('ok');
  });

  it('store 有未提交改动 → store-clean warn + details 列出条目', () => {
    const { home } = repoWithOrigin('doctor-dirty');
    writeFile(path.join(home, 'store', 'pi', 'settings', 'settings.json'), '{}\n');

    const [, store] = checkRepoAndStore(pathsFor(home));
    expect(store?.status).toBe('warn');
    expect(store?.message).toContain('未提交');
    expect(store?.details?.some((line) => line.includes('store/pi/settings/settings.json'))).toBe(true);
  });
});

/* ------------------------------------------------------------------ */
/* ④ remote                                                            */
/* ------------------------------------------------------------------ */

describe('checkRemote（④ remote）', () => {
  it('--offline → ok（已跳过），且不碰网络', () => {
    const home = mkTmp('doctor-offline');
    const check = checkRemote(pathsFor(home), { offline: true });
    expect(check.status).toBe('ok');
    expect(check.message).toContain('跳过');
  });

  it('--offline 在不可达远端上仍 ok（跳过优先于可达性）', () => {
    const { home } = repoWithOrigin('doctor-offline-repo');
    gitOk(home, ['remote', 'set-url', 'origin', path.join(home, 'no-such-origin.git')]);
    expect(checkRemote(pathsFor(home), { offline: true }).status).toBe('ok');
  });

  it('远端可达（本地 bare origin）→ ok', () => {
    const { home } = repoWithOrigin('doctor-reach');
    const check = checkRemote(pathsFor(home), { offline: false });
    expect(check.status).toBe('ok');
    expect(check.message).toContain('远端可达');
  });

  it('远端不可达 → warn（离线？）', () => {
    const { home } = repoWithOrigin('doctor-unreach');
    gitOk(home, ['remote', 'set-url', 'origin', path.join(home, 'missing-origin.git')]);

    const check = checkRemote(pathsFor(home), { offline: false });
    expect(check.status).toBe('warn');
    expect(check.message).toContain('远端不可达');
  });

  it('非 git 仓库 / 无 upstream → warn（跳过而非 fail）', () => {
    const plain = mkTmp('doctor-remote-plain');
    expect(checkRemote(pathsFor(plain), { offline: false }).status).toBe('warn');

    const noUpstream = initRepo('doctor-remote-noup');
    const check = checkRemote(pathsFor(noUpstream), { offline: false });
    expect(check.status).toBe('warn');
    expect(check.message).toContain('upstream');
  });
});

/* ------------------------------------------------------------------ */
/* ⑤ adapters                                                          */
/* ------------------------------------------------------------------ */

describe('checkAdapters（⑤ adapters）', () => {
  it('全部 root 存在 → ok', () => {
    const home = mkTmp('doctor-adapters-ok');
    const root = path.join(home, 'pi-agent');
    fs.mkdirSync(root, { recursive: true });

    const check = checkAdapters(configWith({
      adapters: { pi: { root, categories: {} } },
    }));
    expect(check.status).toBe('ok');
    expect(check.message).toContain('均存在');
  });

  it('root 缺失 → warn（工具未安装？）+ details 列 id 与路径', () => {
    const home = mkTmp('doctor-adapters-missing');
    const check = checkAdapters(configWith({
      adapters: { pi: { root: path.join(home, 'nope'), categories: {} } },
    }));
    expect(check.status).toBe('warn');
    expect(check.message).toContain('工具未安装');
    expect(check.details?.join('\n')).toContain('pi:');
  });

  it('root 存在但不是目录 → warn', () => {
    const home = mkTmp('doctor-adapters-file');
    const file = path.join(home, 'afile');
    fs.writeFileSync(file, 'x', 'utf8');
    const check = checkAdapters(configWith({ adapters: { pi: { root: file, categories: {} } } }));
    expect(check.status).toBe('warn');
    expect(check.details?.join('\n')).toContain('不是目录');
  });

  it('enabled=false 的 adapter 不参与检查；全禁用 → ok', () => {
    const home = mkTmp('doctor-adapters-disabled');
    const check = checkAdapters(configWith({
      adapters: { pi: { root: path.join(home, 'nope'), enabled: false, categories: {} } },
    }));
    expect(check.status).toBe('ok');
    expect(check.message).toContain('没有启用的 adapter');
  });
});

/* ------------------------------------------------------------------ */
/* ⑥ age                                                               */
/* ------------------------------------------------------------------ */

/** 只保留能装进 config 的 hooks。 */
function ageConfig(overrides: { recipients?: string[]; files?: Record<string, string> }): HomerConfig {
  return configWith({ secrets: overrides });
}

describe('checkAge（⑥ age，注入 AgeCryptoPort）', () => {
  it('未配置 secrets（files+recipients 均空）→ ok，不要求 identity', async () => {
    const home = mkTmp('doctor-age-unset');
    const check = await checkAge(pathsFor(home), configWith(), createAgeCryptoPort());
    expect(check.status).toBe('ok');
    expect(check.message).toContain('未配置密钥同步');
  });

  it('配置了密钥但 identity 缺失 → fail（提示 keygen + 迁移两步）', async () => {
    const home = mkTmp('doctor-age-noidentity');
    const check = await checkAge(
      pathsFor(home),
      ageConfig({ files: { token: '~/token.txt' } }),
      createAgeCryptoPort(),
    );
    expect(check.status).toBe('fail');
    expect(check.message).toContain('age identity');
    expect(check.details?.join('\n')).toContain('homer secret keygen');
  });

  it('identity 就绪但 recipients 为空 → fail', async () => {
    const home = mkTmp('doctor-age-norecipients');
    const paths = pathsFor(home);
    writeIdentityFile(paths, generateIdentity());

    const check = await checkAge(paths, ageConfig({ files: { token: '~/token.txt' } }), createAgeCryptoPort());
    expect(check.status).toBe('fail');
    expect(check.message).toContain('recipients');
  });

  it('identity 就绪 + recipients 就绪 + 无 secrets.files → ok', async () => {
    const home = mkTmp('doctor-age-nofiles');
    const paths = pathsFor(home);
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);

    const check = await checkAge(paths, ageConfig({ recipients: [identity.recipient] }), createAgeCryptoPort());
    expect(check.status).toBe('ok');
    expect(check.message).toContain('identity 就绪');
  });

  it('vault 密文可用本机 identity 解密 → ok（真实 age port）', async () => {
    const home = mkTmp('doctor-age-ok');
    const paths = pathsFor(home);
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);
    const crypto = createAgeCryptoPort();
    await encryptSecretToFile(crypto, paths, 'token', Buffer.from('s3cr3t-value\n'), [identity.recipient]);

    const check = await checkAge(
      paths,
      ageConfig({ recipients: [identity.recipient], files: { token: '~/token.txt' } }),
      crypto,
    );
    expect(check.status).toBe('ok');
    expect(check.message).toContain('均可解密');
  });

  it('vault 文件缺失 → warn（尚未 secret push？）', async () => {
    const home = mkTmp('doctor-age-missingvault');
    const paths = pathsFor(home);
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);

    const check = await checkAge(
      paths,
      ageConfig({ recipients: [identity.recipient], files: { token: '~/token.txt' } }),
      createAgeCryptoPort(),
    );
    expect(check.status).toBe('warn');
    expect(check.message).toContain('vault 文件缺失');
    expect(check.details?.join('\n')).toContain('token');
  });

  it('注入假 port：解密抛错 → fail（本机 identity 不是 recipient）', async () => {
    const home = mkTmp('doctor-age-fakedecrypt');
    const paths = pathsFor(home);
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);
    writeFile(path.join(paths.secretsDir, 'token.age'), 'not-really-ciphertext');

    const fake: AgeCryptoPort = {
      encrypt: async () => Buffer.from('x'),
      decrypt: async () => {
        throw new Error('age 解密失败：identity 与密文不匹配，或密文已损坏');
      },
    };

    const check = await checkAge(
      paths,
      ageConfig({ recipients: [identity.recipient], files: { token: '~/token.txt' } }),
      fake,
    );
    expect(check.status).toBe('fail');
    expect(check.message).toContain('无法解密');
    expect(check.details?.join('\n')).toContain('token');
    expect(check.details?.join('\n')).toContain('重新 `homer secret push`');
  });

  it('注入假 port：解密成功 → ok（与真实 age 实现解耦的证明）', async () => {
    const home = mkTmp('doctor-age-fakeok');
    const paths = pathsFor(home);
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);
    writeFile(path.join(paths.secretsDir, 'token.age'), 'ciphertext-bytes');

    let calls = 0;
    const fake: AgeCryptoPort = {
      encrypt: async () => Buffer.from('x'),
      decrypt: async () => {
        calls += 1;
        return Buffer.from('plain\n');
      },
    };

    const check = await checkAge(
      paths,
      ageConfig({ recipients: [identity.recipient], files: { token: '~/token.txt' } }),
      fake,
    );
    expect(check.status).toBe('ok');
    expect(calls).toBe(1);
  });

  /* ---------------- P4-W9 缝隙修复：vault 取数口径 ---------------- */

  it('工作区 vault 缺失但 @{upstream} 有密文 → ok（secret pull 不 ff 工作区，不能误报 fail）', async () => {
    const bare = initBare('doctor-age-upstream-origin');
    const home = cloneRepo(bare, 'doctor-age-upstream');
    const paths = pathsFor(home);
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);

    // 先把 identity 的 recipient 写进 homer.json（clone 下来的仓库根即 home），
    // 再用真实 port 加密出密文 → commit + push 到 origin（工作区一个字节都不留 vault）。
    writeHomerJson(home, ageConfig({ recipients: [identity.recipient], files: { token: '~/token.txt' } }));
    const crypto = createAgeCryptoPort();
    await encryptSecretToFile(crypto, paths, 'token', Buffer.from('s3cr3t-value\n'), [identity.recipient]);
    gitOk(home, ['add', '-A']);
    gitOk(home, ['commit', '-m', 'vault']);
    gitOk(home, ['push', '-u', 'origin', 'main']);

    // 模拟「secret pull 刚成功但工作区未前移」：vault 只存在于 origin/main。
    fs.rmSync(paths.secretsDir, { recursive: true, force: true });
    expect(fs.existsSync(path.join(paths.secretsDir, 'token.age'))).toBe(false);

    const check = await checkAge(paths, ageConfig({ recipients: [identity.recipient], files: { token: '~/token.txt' } }), crypto);
    expect(check.status, JSON.stringify(check)).toBe('ok');
    expect(check.message).toContain('均可解密');
  });

  it('工作区与 @{upstream} 都解不开 → fail（仍能准确报错）', async () => {
    const bare = initBare('doctor-age-bothfail-origin');
    const home = cloneRepo(bare, 'doctor-age-bothfail');
    const paths = pathsFor(home);

    // 用 identityA 加密，但本机装的是 identityB（从未被列为 recipient）。
    const identityA = generateIdentity();
    const identityB = generateIdentity();
    writeIdentityFile(paths, identityB);
    writeHomerJson(home, ageConfig({ recipients: [identityA.recipient], files: { token: '~/token.txt' } }));

    const crypto = createAgeCryptoPort();
    await encryptSecretToFile(crypto, paths, 'token', Buffer.from('s3cr3t-value\n'), [identityA.recipient]);
    gitOk(home, ['add', '-A']);
    gitOk(home, ['commit', '-m', 'vault']);
    gitOk(home, ['push', '-u', 'origin', 'main']);

    const check = await checkAge(paths, ageConfig({ recipients: [identityA.recipient], files: { token: '~/token.txt' } }), crypto);
    expect(check.status).toBe('fail');
    expect(check.message).toContain('无法解密');
    expect(check.details?.join('\n')).toContain('token');
  });

  it('工作区 vault 存在且可解（无 upstream）→ ok，且不产生 upstream 回落文案', async () => {
    const home = mkTmp('doctor-age-workspaceonly');
    const paths = pathsFor(home);
    const identity = generateIdentity();
    writeIdentityFile(paths, identity);
    const crypto = createAgeCryptoPort();
    await encryptSecretToFile(crypto, paths, 'token', Buffer.from('s3cr3t-value\n'), [identity.recipient]);

    const check = await checkAge(
      paths,
      ageConfig({ recipients: [identity.recipient], files: { token: '~/token.txt' } }),
      crypto,
    );
    expect(check.status, JSON.stringify(check)).toBe('ok');
    expect(check.message).toContain('均可解密');
  });
});

/* ------------------------------------------------------------------ */
/* ⑦ machine                                                           */
/* ------------------------------------------------------------------ */

describe('checkMachine（⑦ machine）', () => {
  it('state 缺失（lastSyncCommit 缺失）→ warn', () => {
    const home = initRepo('doctor-machine-nostate');
    const check = checkMachine(pathsFor(home));
    expect(check.status).toBe('warn');
    expect(check.message).toContain('lastSyncCommit');
  });

  it('state 落后于 HEAD（本地有未同步 commit）→ warn 且列出两个 SHA', () => {
    const home = initRepo('doctor-machine-behind');
    writeFile(path.join(home, 'a.txt'), 'a\n');
    gitOk(home, ['add', '-A']);
    gitOk(home, ['commit', '-m', 'first']);
    const first = shaOf(home);

    saveState(pathsFor(home), { version: 1, lastSyncCommit: first });
    writeFile(path.join(home, 'a.txt'), 'a2\n');
    gitOk(home, ['add', '-A']);
    gitOk(home, ['commit', '-m', 'second']);

    const check = checkMachine(pathsFor(home));
    expect(check.status).toBe('warn');
    expect(check.message).toContain('未同步');
    expect(check.details?.join('\n')).toContain(first.slice(0, 7));
  });

  it('state.lastSyncCommit == HEAD → ok', () => {
    const home = initRepo('doctor-machine-ok');
    writeFile(path.join(home, 'a.txt'), 'a\n');
    gitOk(home, ['add', '-A']);
    gitOk(home, ['commit', '-m', 'first']);
    saveState(pathsFor(home), { version: 1, lastSyncCommit: shaOf(home) });

    const check = checkMachine(pathsFor(home));
    expect(check.status).toBe('ok');
    expect(check.message).toContain('一致');
  });

  it('state 就绪但仓库无 commit → warn（关系不可判定）', () => {
    const home = mkTmp('doctor-machine-unborn');
    gitOk(home, ['init', '-b', 'main']);
    configureUser(home);
    saveState(pathsFor(home), { version: 1, lastSyncCommit: 'a'.repeat(40) });

    const check = checkMachine(pathsFor(home));
    expect(check.status).toBe('warn');
    expect(check.message).toContain('无法解析 HEAD');
  });
});

/* ------------------------------------------------------------------ */
/* ⑧ required                                                          */
/* ------------------------------------------------------------------ */

describe('checkRequiredPlaceholders（⑧ required）', () => {
  it('无占位符残留 → ok', () => {
    const home = mkTmp('doctor-required-ok');
    const paths = pathsFor(home);
    writeSnapshotToStore(paths, {
      adapterId: 'pi',
      categories: [
        {
          adapterId: 'pi',
          category: 'settings',
          mode: 'merge',
          files: new Map([['settings.json', { kind: 'json' as const, content: '{"theme":"dark"}\n' }]]),
        },
      ],
    });

    const check = checkRequiredPlaceholders(paths, configWith());
    expect(check.status).toBe('ok');
    expect(check.message).toContain('未发现');
  });

  it('残留占位符 → warn，details 精确到 `文件: 键`', () => {
    const home = mkTmp('doctor-required-warn');
    const paths = pathsFor(home);
    writeSnapshotToStore(paths, {
      adapterId: 'pi',
      categories: [
        {
          adapterId: 'pi',
          category: 'settings',
          mode: 'merge',
          files: new Map([
            [
              'settings.json',
              { kind: 'json' as const, content: '{"theme":"dark","apiKeys":"__REQUIRED__","token":"__REQUIRED__"}\n' },
            ],
          ]),
        },
      ],
    });

    const check = checkRequiredPlaceholders(paths, configWith());
    expect(check.status).toBe('warn');
    expect(check.message).toContain('__REQUIRED__');
    expect(check.details).toEqual(['pi/settings/settings.json: apiKeys, token']);
  });

  it('嵌套键 / 非占位符值 / 非 JSON 文件不误报', () => {
    const home = mkTmp('doctor-required-nested');
    const paths = pathsFor(home);
    writeSnapshotToStore(paths, {
      adapterId: 'pi',
      categories: [
        {
          adapterId: 'pi',
          category: 'settings',
          mode: 'merge',
          files: new Map([
            ['settings.json', { kind: 'json' as const, content: '{"nested":{"apiKeys":"__REQUIRED__"},"other":"__REQUIRED__x"}\n' }],
            ['notes.md', { kind: 'file' as const, content: '__REQUIRED__\n' }],
          ]),
        },
      ],
    });

    const check = checkRequiredPlaceholders(paths, configWith());
    expect(check.status).toBe('ok');
  });

  it('store 目录存在但缺 .homer-complete（半残）→ warn 而非抛错', () => {
    const home = mkTmp('doctor-required-broken');
    const paths = pathsFor(home);
    fs.mkdirSync(path.join(paths.storeDir, 'pi', 'settings'), { recursive: true });
    fs.writeFileSync(path.join(paths.storeDir, 'pi', 'settings', 'settings.json'), '{}\n', 'utf8');

    const check = checkRequiredPlaceholders(paths, configWith());
    expect(check.status).toBe('warn');
    expect(check.message).toContain('store');
  });

  it('多个 adapter / 多个文件各成一条 detail，键按字典序', () => {
    const home = mkTmp('doctor-required-multi');
    const paths = pathsFor(home);
    for (const adapterId of ['herdr', 'pi']) {
      writeSnapshotToStore(paths, {
        adapterId,
        categories: [
          {
            adapterId,
            category: 'config',
            mode: 'merge',
            files: new Map([
              ['a.json', { kind: 'json' as const, content: '{"z":"__REQUIRED__","a":"__REQUIRED__"}\n' }],
            ]),
          },
        ],
      });
    }

    const config = configWith({
      adapters: {
        herdr: { root: '~/.config/herdr', categories: { config: { paths: ['a.json'], mode: 'merge' } } },
        pi: { root: '~/.pi/agent', categories: { config: { paths: ['a.json'], mode: 'merge' } } },
      },
    });

    const check = checkRequiredPlaceholders(paths, config);
    expect(check.status).toBe('warn');
    expect(check.details).toEqual([
      'herdr/config/a.json: a, z',
      'pi/config/a.json: a, z',
    ]);
  });
});
