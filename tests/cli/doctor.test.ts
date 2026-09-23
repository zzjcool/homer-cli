/**
 * `homer doctor` 命令层测试（docs/m3-plan.md §2.5 / §3-P2-W7）。
 *
 * 覆盖口径（验收原文）：
 *   - 顺序执行八项检查、`DoctorReport` 形状（`--json` 可 parse）；
 *   - **exit 码**：有 fail → 1、仅 warn → 0；
 *   - `--offline` 跳过远端检查且离线时 warn（可达时 ok）；
 *   - `deps.age` 注入假 `AgeCryptoPort`（命令层透传到 age 检查）；
 *   - `renderDoctorReport` 的人类可读输出（标记 / details 缩进 / 合计行）。
 *
 * 全程隔离：`mkdtemp` 临时 home + `git init --bare` 假 origin（`tests/core/git/helpers.ts`）。
 */

import fs from 'node:fs';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { DOCTOR_USAGE, renderDoctorReport, runDoctor } from '../../src/cli/commands/doctor.js';
import { run } from '../../src/cli/index.js';
import { writeSnapshotToStore } from '../../src/core/store/store.js';
import { saveState } from '../../src/core/state.js';
import {
  cleanupTmp,
  gitOk,
  initBare,
  initRepo,
  mkTmp,
  pathsFor,
  writeFile,
  shaOf,
} from '../core/git/helpers.js';
import type { AgeCryptoPort } from '../../src/core/age/types.js';
import type { DoctorReport } from '../../src/core/doctor/checks.js';
import type { HomerConfig } from '../../src/core/types.js';

afterEach(cleanupTmp);

interface Capture { io: { out: (line: string) => void; err: (line: string) => void }; out: string[]; err: string[] }

function capture(): Capture {
  const out: string[] = [];
  const err: string[] = [];
  return { io: { out: (line) => out.push(line), err: (line) => err.push(line) }, out, err };
}

const EIGHT_IDS = [
  'config', 'repo', 'store-clean', 'remote', 'adapters', 'age', 'machine', 'required',
] as const;

/** 写 homer.json（adapter root 指向已创建的目录，使 adapters 检查 ok）。 */
function writeHealthyConfig(home: string, adapterRoot: string, extra: Partial<HomerConfig> = {}): void {
  const config: HomerConfig = {
    version: 1,
    adapters: {
      pi: {
        root: adapterRoot,
        categories: { settings: { paths: ['settings.json'], mode: 'merge' } },
      },
    },
    ...extra,
  };
  fs.mkdirSync(home, { recursive: true });
  fs.writeFileSync(path.join(home, 'homer.json'), `${JSON.stringify(config, null, 2)}\n`, 'utf8');
}

/**
 * 一台「健康机器」：git 仓库 + upstream（本地 bare origin）+ store 干净 + adapter root 存在
 * + state == HEAD + 无占位符残留 + 未配置密钥同步。
 */
function healthyHome(prefix: string): { home: string; adapterRoot: string } {
  const bare = initBare(`${prefix}-origin`);
  const home = initRepo(`${prefix}-home`);
  const adapterRoot = path.join(home, 'pi-root');
  fs.mkdirSync(adapterRoot, { recursive: true });
  writeHealthyConfig(home, adapterRoot);

  gitOk(home, ['add', '-A']);
  gitOk(home, ['commit', '-m', 'init']);
  gitOk(home, ['remote', 'add', 'origin', bare]);
  gitOk(home, ['push', '-u', 'origin', 'main']);
  saveState(pathsFor(home), { version: 1, lastSyncCommit: shaOf(home) });
  return { home, adapterRoot };
}

/* ------------------------------------------------------------------ */
/* 报告形状 / 顺序                                                      */
/* ------------------------------------------------------------------ */

describe('runDoctor — 报告形状与检查顺序（§2.5）', () => {
  it('无 config：八项齐全、config= fail、ok=false', async () => {
    const home = mkTmp('doctor-cli-noconfig');
    const report = await runDoctor({ homerHome: home, offline: true });

    expect(report.checks.map((check) => check.id)).toEqual([...EIGHT_IDS]);
    expect(report.checks[0]).toMatchObject({ id: 'config', status: 'fail' });
    expect(report.ok).toBe(false);
  });

  it('config 不可用时下游检查仍尽力而为（repo/machine 照跑，adapters/age/required 标「已跳过」）', async () => {
    const home = initRepo('doctor-cli-partial');
    const report = await runDoctor({ homerHome: home, offline: true });

    const byId = Object.fromEntries(report.checks.map((check) => [check.id, check]));
    expect(byId['repo']?.status).toBe('warn'); // 无 upstream
    expect(byId['machine']?.status).toBe('warn'); // 无 state
    expect(byId['adapters']?.message).toContain('已跳过');
    expect(byId['age']?.message).toContain('已跳过');
    expect(byId['required']?.message).toContain('已跳过');
    expect(report.ok).toBe(false); // config fail
  });

  it('健康机器（可达远端）：八项全 ok，ok=true', async () => {
    const { home } = healthyHome('doctor-cli-healthy');
    const report = await runDoctor({ homerHome: home, offline: false });

    expect(report.checks.map((check) => check.id)).toEqual([...EIGHT_IDS]);
    const notOk = report.checks.filter((check) => check.status !== 'ok');
    expect(notOk, JSON.stringify(notOk)).toEqual([]);
    expect(report.ok).toBe(true);
  });
});

/* ------------------------------------------------------------------ */
/* --offline / 远端 warn                                                */
/* ------------------------------------------------------------------ */

describe('runDoctor — 远端检查与 --offline（§1-D6）', () => {
  it('--offline：remote = ok（已跳过），整体仍 ok', async () => {
    const { home } = healthyHome('doctor-cli-offline');
    const report = await runDoctor({ homerHome: home, offline: true });

    const remote = report.checks.find((check) => check.id === 'remote');
    expect(remote?.status).toBe('ok');
    expect(remote?.message).toContain('跳过');
    expect(report.ok).toBe(true);
  });

  it('远端不可达（非 --offline）→ remote warn 但 ok=true（warn 不影响 ok）', async () => {
    const { home } = healthyHome('doctor-cli-unreach');
    gitOk(home, ['remote', 'set-url', 'origin', path.join(home, 'gone.git')]);

    const report = await runDoctor({ homerHome: home, offline: false });
    const remote = report.checks.find((check) => check.id === 'remote');
    expect(remote?.status).toBe('warn');
    expect(report.ok).toBe(true);
  });
});

/* ------------------------------------------------------------------ */
/* 注入 AgeCryptoPort                                                   */
/* ------------------------------------------------------------------ */

describe('runDoctor — age 检查注入（DoctorDeps.age）', () => {
  it('注入假 port：解密失败 → age fail → ok=false（命令层透传 deps）', async () => {
    const home = initRepo('doctor-cli-age-fail');
    const adapterRoot = path.join(home, 'pi-root');
    fs.mkdirSync(adapterRoot, { recursive: true });
    writeHealthyConfig(home, adapterRoot, {
      secrets: { recipients: ['age1' + 'q'.repeat(58)], files: { token: '~/token.txt' } },
    });
    fs.mkdirSync(path.join(home, 'keys'), { recursive: true });
    // 写一个真实 shape 的 identity 文件（loadIdentity 需要能解析）
    const { generateIdentity, writeIdentityFile } = await import('../../src/core/age/keys.js');
    writeIdentityFile(pathsFor(home), generateIdentity());
    writeFile(path.join(home, 'secrets', 'token.age'), 'ciphertext');

    const fake: AgeCryptoPort = {
      encrypt: async () => Buffer.from('x'),
      decrypt: async () => {
        throw new Error('age 解密失败：identity 与密文不匹配，或密文已损坏');
      },
    };

    const report = await runDoctor({ homerHome: home, offline: true }, { age: fake });
    const age = report.checks.find((check) => check.id === 'age');
    expect(age?.status).toBe('fail');
    expect(report.ok).toBe(false);
  });

  it('注入假 port 记录调用 → 未配置密钥同步时不触碰 port', async () => {
    const { home } = healthyHome('doctor-cli-age-unset');
    let calls = 0;
    const fake: AgeCryptoPort = {
      encrypt: async () => {
        calls += 1;
        return Buffer.from('x');
      },
      decrypt: async () => {
        calls += 1;
        return Buffer.from('x');
      },
    };

    const report = await runDoctor({ homerHome: home, offline: true }, { age: fake });
    expect(report.checks.find((check) => check.id === 'age')?.status).toBe('ok');
    expect(calls).toBe(0);
  });
});

/* ------------------------------------------------------------------ */
/* 分发层：退出码 / --json / 渲染                                        */
/* ------------------------------------------------------------------ */

describe('homer doctor 分发层（exit 码 / --json / 渲染）', () => {
  it('仅 warn → exit 0', async () => {
    // adapter root 缺失 = warn；其余（config/repo/store-clean/remote/age/machine/required）自身可控。
    const { home } = healthyHome('doctor-cli-warnonly');
    writeHealthyConfig(home, path.join(home, 'missing-root'));

    const cap = capture();
    const code = await run(['doctor', '--home', home, '--offline'], cap.io);
    expect(code).toBe(0);
    const text = cap.out.join('\n');
    expect(text).toContain('⚠ adapters');
    expect(text).toContain('工具未安装');
  });

  it('有 fail → exit 1（config 缺失）', async () => {
    const home = mkTmp('doctor-cli-exit1');
    const cap = capture();
    const code = await run(['doctor', '--home', home, '--offline'], cap.io);
    expect(code).toBe(1);
    expect(cap.out.join('\n')).toContain('✗ config');
  });

  it('--json 输出可 parse 且形状为 DoctorReport', async () => {
    const { home } = healthyHome('doctor-cli-json');
    const cap = capture();
    const code = await run(['doctor', '--home', home, '--json', '--offline'], cap.io);

    expect(code).toBe(0);
    const report = JSON.parse(cap.out.join('\n')) as DoctorReport;
    expect(typeof report.ok).toBe('boolean');
    expect(report.checks.map((check) => check.id)).toEqual([...EIGHT_IDS]);
    for (const check of report.checks) {
      expect(['ok', 'warn', 'fail']).toContain(check.status);
      expect(typeof check.message).toBe('string');
    }
  });

  it('--json 在 fail 场景同样可 parse（exit 1）', async () => {
    const home = mkTmp('doctor-cli-json-fail');
    const cap = capture();
    expect(await run(['doctor', '--home', home, '--json'], cap.io)).toBe(1);

    const report = JSON.parse(cap.out.join('\n')) as DoctorReport;
    expect(report.ok).toBe(false);
  });

  it('__REQUIRED__ 残留：详情精确到文件 + 键，且仅为 warn（exit 0）', async () => {
    const { home } = healthyHome('doctor-cli-required');
    writeSnapshotToStore(pathsFor(home), {
      adapterId: 'pi',
      categories: [
        {
          adapterId: 'pi',
          category: 'settings',
          mode: 'merge',
          files: new Map([
            ['settings.json', { kind: 'json' as const, content: '{"apiKeys":"__REQUIRED__"}\n' }],
          ]),
        },
      ],
    });
    // store 变更后提交，保持 store-clean 为 ok（占位符残留本身与脏工作区无关）
    gitOk(home, ['add', '-A']);
    gitOk(home, ['commit', '-m', 'placeholders']);
    saveState(pathsFor(home), { version: 1, lastSyncCommit: shaOf(home) });

    const cap = capture();
    expect(await run(['doctor', '--home', home, '--offline', '--json'], cap.io)).toBe(0);

    const report = JSON.parse(cap.out.join('\n')) as DoctorReport;
    const required = report.checks.find((check) => check.id === 'required');
    expect(required?.status).toBe('warn');
    expect(required?.details).toEqual(['pi/settings/settings.json: apiKeys']);
  });

  it('用法/帮助文本不变（--help → DOCTOR_USAGE，exit 0）', async () => {
    const cap = capture();
    expect(await run(['doctor', '--help'], cap.io)).toBe(0);
    expect(cap.out.join('\n')).toBe(DOCTOR_USAGE);
    expect(DOCTOR_USAGE.startsWith('用法:')).toBe(true);
  });

  it('未知选项仍被 strict 拦下（exit 1）', async () => {
    const cap = capture();
    expect(await run(['doctor', '--frobnicate'], cap.io)).toBe(1);
  });
});

/* ------------------------------------------------------------------ */
/* 渲染                                                                */
/* ------------------------------------------------------------------ */

describe('renderDoctorReport', () => {
  it('逐项标记 + details 缩进 + 合计行；fail 时附修复提示', () => {
    const report: DoctorReport = {
      ok: false,
      checks: [
        { id: 'config', status: 'fail', message: '未找到 homer.json', details: ['先运行 `homer init`'] },
        { id: 'repo', status: 'ok', message: 'git 仓库' },
        { id: 'store-clean', status: 'warn', message: 'store 脏' },
      ],
    };

    const text = renderDoctorReport(report);
    expect(text.split('\n')[0]).toBe('homer doctor: 存在问题');
    expect(text).toContain('  ✗ config  未找到 homer.json');
    expect(text).toContain('      - 先运行 `homer init`');
    expect(text).toContain('  ✓ repo  git 仓库');
    expect(text).toContain('  ⚠ store-clean  store 脏');
    expect(text).toContain('合计: ok 1  warn 1  fail 1');
    expect(text).toContain('有 fail 项');
  });

  it('全 ok 时不出现修复提示', () => {
    const report: DoctorReport = {
      ok: true,
      checks: [{ id: 'config', status: 'ok', message: 'ok' }],
    };
    const text = renderDoctorReport(report);
    expect(text.split('\n')[0]).toBe('homer doctor: 通过');
    expect(text).not.toContain('有 fail 项');
  });
});
