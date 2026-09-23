/**
 * M2 里程碑级 e2e 验收（docs/m2-plan.md §3-P4 的 8 组 / §5 M2 Done 定义）。
 *
 * 全程隔离，绝不碰真实 `~/.pi` / `~/.homer` / 真实 remote：
 *   <tmp>/repo/origin.git     ← `git init --bare` 假 origin（纯文件系统路径，无网络）
 *   <tmp>/repo/<机器名>/home  ← 假 HOME（pi adapter root 靠 '~' 展开到这里）
 *   <tmp>/repo/<机器名>/homer ← HOMER_HOME（= 一个真实 git clone，`homer.json` 随仓库走）
 *
 * 每条命令都以**真实子进程**跑 CLI 入口（`node --import tsx src/cli/index.ts`），
 * 这样 argv 解析、退出码、stdout/stderr 与 `--json` 形状都被钉在进程边界上
 * （与 tests/e2e/m1.test.ts 的进程级用例同款做法）。
 *
 * 关键不变式（§5）：成功路径上 `state.json.lastSyncCommit === git rev-parse HEAD`。
 */

import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

/* ------------------------------------------------------------------ */
/* 隔离 harness                                                        */
/* ------------------------------------------------------------------ */

const created: string[] = [];

function mkTmp(prefix: string): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), `homer-m2-e2e-${prefix}-`));
  created.push(dir);
  return dir;
}

afterEach(() => {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) fs.rmSync(dir, { recursive: true, force: true });
  }
});

interface GitOutcome {
  ok: boolean;
  out: string;
}

/** git（失败即抛：harness 里的 git 失败都是测试自身构造错误）。 */
function git(cwd: string, args: readonly string[]): string {
  return execFileSync('git', [...args], { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

function gitTry(cwd: string, args: readonly string[]): GitOutcome {
  try {
    return { ok: true, out: git(cwd, args) };
  } catch {
    return { ok: false, out: '' };
  }
}

/** git 输出（去首尾空白，多行保留）。 */
function sha(cwd: string, ref = 'HEAD'): string {
  return git(cwd, ['rev-parse', ref]).trim();
}

interface Machine {
  name: string;
  /** 假 HOME：pi adapter root 展开到 `<fakeHome>/.pi/agent`。 */
  fakeHome: string;
  /** HOMER_HOME：真实 git clone。 */
  home: string;
  agentRoot: string;
}

interface Repo {
  tmp: string;
  origin: string;
  A: Machine;
  B: Machine;
}

function makeMachine(tmp: string, name: string): Machine {
  const fakeHome = path.join(tmp, name, 'home');
  const home = path.join(tmp, name, 'homer');
  const agentRoot = path.join(fakeHome, '.pi', 'agent');
  fs.mkdirSync(agentRoot, { recursive: true });
  return { name, fakeHome, home: path.resolve(home), agentRoot };
}

function writeAbs(abs: string, content: string): void {
  fs.mkdirSync(path.dirname(abs), { recursive: true });
  fs.writeFileSync(abs, content, 'utf8');
}

/** 写工具目录文件（相对 adapter root）。 */
function writeAgent(m: Machine, rel: string, content: string): void {
  writeAbs(path.join(m.agentRoot, rel), content);
}

function readAgent(m: Machine, rel: string): string {
  return fs.readFileSync(path.join(m.agentRoot, rel), 'utf8');
}

/** 递归列出目录下所有文件（相对该目录，posix 分隔符），排序。 */
function listFiles(dir: string, prefix = ''): string[] {
  if (!fs.existsSync(dir)) return [];
  return fs
    .readdirSync(dir, { withFileTypes: true })
    .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))
    .flatMap((entry) =>
      entry.isDirectory()
        ? listFiles(path.join(dir, entry.name), `${prefix}${entry.name}/`)
        : [`${prefix}${entry.name}`],
    );
}

/* ---- 基线工具目录内容（三方一致：base == local == remote） ---- */

const SETTINGS_V1 = `${JSON.stringify({ theme: 'light', keep: 1 }, null, 2)}\n`;
const ALPHA_V1 = '# alpha v1\n';
const BETA_V1 = '# beta v1\n';

function seedAgentRoot(m: Machine): void {
  writeAgent(m, 'settings.json', SETTINGS_V1);
  writeAgent(m, 'skills/alpha/SKILL.md', ALPHA_V1);
  writeAgent(m, 'skills/beta/SKILL.md', BETA_V1);
}

/* ---- CLI 子进程 ---- */

interface CliResult {
  code: number;
  out: string;
  err: string;
}

function homer(m: Machine, args: readonly string[]): CliResult {
  const env: NodeJS.ProcessEnv = { ...process.env, HOME: m.fakeHome, HOMER_HOME: m.home };
  // 子进程必须不带 VITEST：src/cli/index.ts 据此跳过入口副作用（否则不会真正跑命令）。
  delete env['VITEST'];
  try {
    const out = execFileSync(process.execPath, ['--import', 'tsx', 'src/cli/index.ts', ...args], {
      cwd: process.cwd(),
      env,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    return { code: 0, out, err: '' };
  } catch (err) {
    const failure = err as { status?: number; stdout?: string; stderr?: string };
    return { code: failure.status ?? -1, out: failure.stdout ?? '', err: failure.stderr ?? '' };
  }
}

/** 跑一条命令并断言 exit 0，返回解析后的 `--json` 报告。 */
function homerJsonOk<T = Record<string, unknown>>(m: Machine, args: readonly string[]): T {
  const result = homer(m, args);
  expect(result.err, `stderr: ${result.err}`).toBe('');
  expect(result.code, `stdout: ${result.out}\nstderr: ${result.err}`).toBe(0);
  return JSON.parse(result.out) as T;
}

function readState(home: string): Record<string, unknown> {
  return JSON.parse(fs.readFileSync(path.join(home, 'state.json'), 'utf8')) as Record<string, unknown>;
}

/* ---- 仓库 bootstrap ---- */

interface MachineConfigOverride {
  /** 在 homer.json 中追加的字段（如 secrets.ignorePaths）。 */
  extraConfig?: Record<string, unknown>;
}

/**
 * 建立 `<tmp>/repo/origin.git` 假 origin，并把 A 机器置于「已 clone + init + 首次 push 成功」状态。
 *
 * 顺序（对应真实新机器接入流程，逐字对应 §3-P4 ①「clone A → init → push --yes → origin 有 commit」）：
 *   A: `git clone <空 bare origin>` → 写工具目录基线 → `homer init`（生成 homer.json + store）
 *      → `git commit homer.json`（把配置中心本体入仓，**故意不手动 push**）
 *      → `homer push --yes`：这一条命令同时完成
 *          · store 首次入库的同步基线 commit（§5 的 state.lastSyncCommit == HEAD 判据）
 *          · 把「homer.json commit + store commit」一起推到 origin（origin 首次拿到 commit）
 *   B: `git clone origin`（homer.json 随仓库走）→ 写同样的工具目录基线
 *
 * 这样构造的好处：**不依赖 `git push -u` 手工铺路**。A 在 clone 空 origin 后只有
 * `branch.main.remote=origin` 而**没有远程跟踪引用**（`@{upstream}` 不可解析），
 * 正是 `homer push` 首次运行的真实形态——它必须能把 commit 推上去（见 git-port 的 hasPushTarget）。
 *
 * `seedB=false` 时不建 B（只需 A 的分组用）。
 */
function setupRepo(opts: { seedB?: boolean; machineConfig?: MachineConfigOverride } = {}): Repo {
  const tmp = mkTmp('repo');
  const origin = path.join(tmp, 'origin.git');
  git(tmp, ['init', '--bare', '-b', 'main', origin]);

  const A = makeMachine(tmp, 'A');
  seedAgentRoot(A);
  git(path.dirname(A.home), ['clone', origin, A.home]);
  git(A.home, ['config', 'user.email', 'homer-e2e@example.invalid']);
  git(A.home, ['config', 'user.name', 'Homer E2E']);
  homerJsonOk(A, ['init', '--json']);

  if (opts.machineConfig?.extraConfig !== undefined) {
    const configFile = path.join(A.home, 'homer.json');
    const parsed = JSON.parse(fs.readFileSync(configFile, 'utf8')) as Record<string, unknown>;
    fs.writeFileSync(configFile, `${JSON.stringify({ ...parsed, ...opts.machineConfig.extraConfig }, null, 2)}\n`, 'utf8');
  }

  git(A.home, ['add', 'homer.json']);
  git(A.home, ['commit', '-m', 'chore: add homer.json']);
  // 此推之前 origin 为空（无任何 commit）——首次 `homer push` 必须自行把历史送上去。
  // 用 `--verify`：unborn HEAD 下裸 `rev-parse HEAD` 会回显 ref 名并非零退出。
  expect(gitTry(origin, ['rev-parse', '--verify', 'HEAD']).ok).toBe(false);
  homerJsonOk(A, ['push', '--yes', '--json']);

  const B = makeMachine(tmp, 'B');
  git(path.dirname(B.home), ['clone', origin, B.home]);
  git(B.home, ['config', 'user.email', 'homer-e2e@example.invalid']);
  git(B.home, ['config', 'user.name', 'Homer E2E']);
  if (opts.seedB !== false) seedAgentRoot(B);

  return { tmp, origin, A, B };
}

/* ------------------------------------------------------------------ */
/* ① 安全往返                                                          */
/* ------------------------------------------------------------------ */

describe('e2e M2 ① 安全往返：clone → init → push --yes → 改增删 → push --yes', () => {
  it('首次 push：origin 首次拿到 commit、store 齐全、state.lastSyncCommit == HEAD', () => {
    const { origin, A } = setupRepo();

    // 这一条 push 是在「origin 空仓库、无远程跟踪引用」的条件下成功的
    // （setupRepo 已断言 push 前 origin 无 HEAD）。
    expect(git(origin, ['rev-parse', 'HEAD']).trim()).toBe(sha(A.home));
    expect(git(origin, ['rev-list', '--count', 'HEAD']).trim()).toBe('2'); // homer.json + store 基线
    // origin 与 A 的历史一致（推送真的发生了）。
    expect(git(A.home, ['rev-parse', '@{upstream}']).trim()).toBe(sha(origin));

    // store 齐全：初始快照逐字进 store（含完整性标记）。
    const store = path.join(A.home, 'store');
    expect(listFiles(store)).toEqual([
      'pi/.homer-complete',
      'pi/settings/settings.json',
      'pi/skills/alpha/SKILL.md',
      'pi/skills/beta/SKILL.md',
    ]);
    expect(fs.readFileSync(path.join(store, 'pi/settings/settings.json'), 'utf8')).toBe(SETTINGS_V1);

    // §5 Done 判据：state.lastSyncCommit == git rev-parse HEAD
    expect(readState(A.home)['lastSyncCommit']).toBe(sha(A.home));
    expect(readState(A.home)['lastSyncCommand']).toBe('push');
  });

  it('改 A 本地 skill + 删另一 skill + 改 settings 键 → push --yes → commit 含对应增删改', () => {
    const { origin, A } = setupRepo();
    const before = sha(origin);

    // 改 settings 键（merge）
    writeAgent(A, 'settings.json', `${JSON.stringify({ theme: 'dark', keep: 1 }, null, 2)}\n`);
    // 新增一个 skill + 删掉另一个（mirror）
    writeAgent(A, 'skills/delta/SKILL.md', '# delta\n');
    fs.rmSync(path.join(A.agentRoot, 'skills', 'beta'), { recursive: true, force: true });

    const report = homerJsonOk<{ status: string; changedFiles: unknown[]; pushedToRemote: boolean }>(A, [
      'push',
      '--yes',
      '--json',
    ]);
    expect(report.status).toBe('pushed');
    expect(report.pushedToRemote).toBe(true);

    // 一次 commit 同时含「改 / 删 / 增」三类变更（name-status：M / D / A）。
    const status = git(A.home, ['diff-tree', '--no-commit-id', '--name-status', '-r', 'HEAD'])
      .trim()
      .split('\n')
      .sort();
    expect(status).toEqual(
      [
        'A\tstore/pi/skills/delta/SKILL.md',
        'D\tstore/pi/skills/beta/SKILL.md',
        'M\tstore/pi/settings/settings.json',
      ].sort(),
    );

    // origin 前进、store 内容正确、state 与 HEAD 相等（§5）。
    expect(sha(origin)).toBe(sha(A.home));
    expect(sha(origin)).not.toBe(before);
    expect(fs.readFileSync(path.join(A.home, 'store/pi/settings/settings.json'), 'utf8')).toContain('"theme": "dark"');
    expect(fs.existsSync(path.join(A.home, 'store/pi/skills/beta/SKILL.md'))).toBe(false);
    expect(readState(A.home)['lastSyncCommit']).toBe(sha(A.home));
  });

  it('幂等重跑 push --yes → no-drift exit 0，不产生新 commit', () => {
    const { origin, A } = setupRepo();
    const head = sha(origin);

    const report = homerJsonOk<{ status: string; commit?: string }>(A, ['push', '--yes', '--json']);

    expect(report.status).toBe('no-drift');
    expect(report.commit).toBeUndefined();
    expect(sha(origin)).toBe(head);
  });
});

/* ------------------------------------------------------------------ */
/* ② 密钥拒推 + ignorePaths 豁免                                        */
/* ------------------------------------------------------------------ */

/**
 * 假 Anthropic token：**分段拼装**，源码里不出现完整字面量
 * （即便将来测试仓库接上 GitHub，也不会被 push protection 拦下）。
 * 末段长度足够让 `sk-ant-[A-Za-z0-9_-]{20,}` 命中。
 */
const FAKE_ANTHROPIC_TOKEN = ['sk', '-ant-', 'api03', '-', 'e2e', '-', 'notareal', 'token', '-', '0123456789'].join('');

describe('e2e M2 ② 密钥拒推：命中 → exit 1 / 报告 path:line / origin 无新 commit；豁免后可推', () => {
  it('植入 sk-ant-* → push exit 1、报告含 path:line、origin 无新 commit、store 未被写入', () => {
    const { origin, A } = setupRepo();
    const originHead = sha(origin);

    // 单行 JSON：命中行号恒为 1；密钥写在 apiKey 值里（settings 是 merge 分类，值会进 store）。
    writeAbs(
      path.join(A.agentRoot, 'settings.json'),
      `${JSON.stringify({ theme: 'light', keep: 1, apiKey: FAKE_ANTHROPIC_TOKEN })}\n`,
    );

    const result = homer(A, ['push', '--yes', '--json']);
    expect(result.code).toBe(1);

    const report = JSON.parse(result.out) as {
      status: string;
      secrets: { path: string; line: number; patternId: string; excerpt: string }[];
    };
    expect(report.status).toBe('secrets-rejected');
    expect(report.secrets).toHaveLength(1);
    expect(report.secrets[0]?.path).toBe('pi/settings/settings.json');
    expect(report.secrets[0]?.line).toBe(1);
    // 脱敏：摘录不得包含完整 token。
    expect(report.secrets[0]?.excerpt).toContain('*');
    expect(report.secrets[0]?.excerpt).not.toContain(FAKE_ANTHROPIC_TOKEN);

    // 文本报告同样含 `path:line`（人工可核读）。
    const text = homer(A, ['push', '--yes']);
    expect(text.code).toBe(1);
    expect(text.out).toContain('pi/settings/settings.json:1');

    // origin 无新 commit；store 未被写入（仍是基线内容）。
    expect(sha(origin)).toBe(originHead);
    expect(fs.readFileSync(path.join(A.home, 'store/pi/settings/settings.json'), 'utf8')).toBe(SETTINGS_V1);
    expect(readState(A.home)['lastSyncCommit']).toBe(originHead);
  });

  it('加 secrets.ignorePaths 豁免命中路径后可推（同一 token）', () => {
    const { origin, A } = setupRepo();
    writeAgent(A, 'settings.json', `${JSON.stringify({ theme: 'light', keep: 1, apiKey: FAKE_ANTHROPIC_TOKEN })}\n`);

    // 先确认当前确实被拒。
    expect(homer(A, ['push', '--yes', '--json']).code).toBe(1);

    // 在 homer.json 里精确豁免该 store 相对路径（homer.json 的改动不入同步 commit）。
    const configFile = path.join(A.home, 'homer.json');
    const config = JSON.parse(fs.readFileSync(configFile, 'utf8')) as Record<string, unknown>;
    writeAbs(configFile, `${JSON.stringify({ ...config, secrets: { ignorePaths: ['pi/settings/settings.json'] } }, null, 2)}\n`);

    const report = homerJsonOk<{ status: string; pushedToRemote: boolean }>(A, ['push', '--yes', '--json']);
    expect(report.status).toBe('pushed');
    expect(report.pushedToRemote).toBe(true);
    // 豁免后该内容确实进了 store 与 origin。
    expect(fs.readFileSync(path.join(A.home, 'store/pi/settings/settings.json'), 'utf8')).toContain('apiKey');
    expect(sha(origin)).toBe(sha(A.home));
  });
});

/* ------------------------------------------------------------------ */
/* ②b 对抗式 review C1：merge 不得绕过密钥闸门（真实的进程级证据）      */
/* ------------------------------------------------------------------ */

describe('e2e M2 ②b merge 复用密钥闸门：push 被拦的密钥文件不得经 merge 明文入库', () => {
  it('B 本地植入 sk-ant-* 并制造冲突 → push exit 1；merge --accept-local 也 exit 1，origin 无明文', () => {
    const { origin, A, B } = setupRepo();

    // A 先改 settings 推送，让 B pull 时产生冲突。
    writeAgent(A, 'settings.json', `${JSON.stringify({ theme: 'dark', keep: 1 }, null, 2)}\n`);
    expect(homerJsonOk<{ status: string }>(A, ['push', '--yes', '--json']).status).toBe('pushed');

    // B 把密钥写进 settings（merge 分类，值会进 store）并改为自己的意图 → 保留本地。
    writeAgent(
      B,
      'settings.json',
      `${JSON.stringify({ theme: 'local', keep: 1, apiKey: FAKE_ANTHROPIC_TOKEN })}\n`,
    );

    // push 先被拦（闸门本身有效）。
    const pushResult = homer(B, ['push', '--yes', '--json']);
    expect(pushResult.code).toBe(1);
    expect((JSON.parse(pushResult.out) as { status: string }).status).toBe('secrets-rejected');

    // merge --accept-local：修复前会绕过扫描，把密钥明文写进 store 并推到 origin。
    const mergeResult = homer(B, ['merge', '--accept-local', '--json']);
    expect(mergeResult.code).toBe(1);
    const mergeReport = JSON.parse(mergeResult.out) as {
      status: string;
      commit?: string;
      errors: string[];
    };
    expect(mergeReport.status).toBe('error');
    expect(mergeReport.commit).toBeUndefined();
    expect(mergeReport.errors.join('\n')).toContain('疑似密钥');

    // store 里绝无明文（settings 仍是 push 被拦时的旧值）。
    const storeSettings = fs.readFileSync(path.join(B.home, 'store/pi/settings/settings.json'), 'utf8');
    expect(storeSettings).not.toContain(FAKE_ANTHROPIC_TOKEN);
    // store 工作区干净（未写入密钥字节）。
    expect(gitTry(B.home, ['status', '--porcelain', '--', 'store/']).out.trim()).toBe('');

    // origin 里绝无该明文（真实 bare 仓库的 git grep）。
    const originGrep = gitTry(origin, ['grep', '-h', '-e', FAKE_ANTHROPIC_TOKEN, 'HEAD']);
    expect(originGrep.out).not.toContain(FAKE_ANTHROPIC_TOKEN);
  });
});

/* ------------------------------------------------------------------ */
/* ③ pull 应用                                                         */
/* ------------------------------------------------------------------ */

describe('e2e M2 ③ pull 应用：B 预放旧版 → pull --yes → 工具目录 == store + 备份为旧版', () => {
  it('写入远端内容，并把覆盖前的旧版备份到 backups/<date>/', () => {
    const { A, B } = setupRepo();

    // A 改 settings + alpha 并推送。
    writeAgent(A, 'settings.json', `${JSON.stringify({ theme: 'dark', keep: 1 }, null, 2)}\n`);
    writeAgent(A, 'skills/alpha/SKILL.md', '# alpha v2\n');
    const pushed = homerJsonOk<{ status: string }>(A, ['push', '--yes', '--json']);
    expect(pushed.status).toBe('pushed');

    // B 的工具目录保持旧版（B 自己 clone 后手写的基线就是旧版）。
    expect(readAgent(B, 'skills/alpha/SKILL.md')).toBe(ALPHA_V1);

    const report = homerJsonOk<{
      status: string;
      applied: { written: { relPath: string }[]; backupDir?: string };
    }>(B, ['pull', '--yes', '--json']);

    expect(report.status).toBe('applied');
    // 工具目录 == store 内容（逐字节）。
    const store = path.join(B.home, 'store/pi');
    expect(readAgent(B, 'settings.json')).toBe(fs.readFileSync(path.join(store, 'settings/settings.json'), 'utf8'));
    expect(readAgent(B, 'skills/alpha/SKILL.md')).toBe(fs.readFileSync(path.join(store, 'skills/alpha/SKILL.md'), 'utf8'));
    expect(readAgent(B, 'skills/alpha/SKILL.md')).toBe('# alpha v2\n');

    // 备份目录存在，且保存的是**覆盖前**的旧版内容。
    const backupDir = report.applied.backupDir;
    expect(backupDir).toBeDefined();
    expect(path.relative(path.join(B.home, 'backups'), backupDir as string)).toMatch(/^\d{8}\/\d{6}-pull$/);
    expect(fs.readFileSync(path.join(backupDir as string, 'pi/settings/settings.json'), 'utf8')).toBe(SETTINGS_V1);
    expect(fs.readFileSync(path.join(backupDir as string, 'pi/skills/alpha/SKILL.md'), 'utf8')).toBe(ALPHA_V1);

    // ff 后 base 前进（§5：state == HEAD）。
    expect(readState(B.home)['lastSyncCommit']).toBe(sha(B.home));
    expect(readState(B.home)['lastSyncCommand']).toBe('pull');

    // 再跑一次 → no-drift（已收敛）。
    expect(homerJsonOk<{ status: string }>(B, ['pull', '--yes', '--json']).status).toBe('no-drift');
  });
});

/* ------------------------------------------------------------------ */
/* ④ remote-ahead                                                      */
/* ------------------------------------------------------------------ */

describe('e2e M2 ④ remote-ahead：A 推 → B 推被拒 → B pull --yes 合并双方', () => {
  it('push 被 remote-ahead 拦截（exit 1、origin 不变），pull 后两侧改动共存', () => {
    const { origin, A, B } = setupRepo();

    // A 改 settings（merge）并推送。
    writeAgent(A, 'settings.json', `${JSON.stringify({ theme: 'dark', keep: 1 }, null, 2)}\n`);
    expect(homerJsonOk<{ status: string }>(A, ['push', '--yes', '--json']).status).toBe('pushed');
    const afterA = sha(origin);

    // B 改另一个文件（beta，mirror）后 push → 远端有本地未见的变更 → 拒推。
    writeAgent(B, 'skills/beta/SKILL.md', '# beta v2 by B\n');
    const rejected = homer(B, ['push', '--yes', '--json']);
    expect(rejected.code).toBe(1);
    const rejectedReport = JSON.parse(rejected.out) as { status: string; pushedToRemote: boolean };
    expect(rejectedReport.status).toBe('remote-ahead');
    expect(rejectedReport.pushedToRemote).toBe(false);
    // origin 未前进，store 未被写入。
    expect(sha(origin)).toBe(afterA);
    expect(readAgent(B, 'settings.json')).toBe(SETTINGS_V1);

    // B pull --yes → 双方改动共存（A 的 settings + B 自己的 beta）。
    const pulled = homerJsonOk<{ status: string; applied: { written: { relPath: string }[] } }>(B, ['pull', '--yes', '--json']);
    expect(pulled.status).toBe('applied');
    expect(pulled.applied.written.map((item) => item.relPath)).toEqual(['settings.json']);
    expect(readAgent(B, 'settings.json')).toBe(`${JSON.stringify({ theme: 'dark', keep: 1 }, null, 2)}\n`);
    expect(readAgent(B, 'skills/beta/SKILL.md')).toBe('# beta v2 by B\n');

    // B 现在可以把自己的改动推上去。
    expect(homerJsonOk<{ status: string }>(B, ['push', '--yes', '--json']).status).toBe('pushed');
    expect(sha(origin)).toBe(sha(B.home));
    expect(readState(B.home)['lastSyncCommit']).toBe(sha(B.home));
  });
});

/* ------------------------------------------------------------------ */
/* ⑤ 冲突 + merge                                                      */
/* ------------------------------------------------------------------ */

describe('e2e M2 ⑤ 冲突 + merge：同一 mirror 文件双方改 → conflicts-remain → merge --accept-remote', () => {
  it('B pull → conflicts-remain exit 1；B merge --accept-remote → B == A 版本；A pull 拿到一致状态', () => {
    const { A, B } = setupRepo();

    // A / B 改同一个 mirror 文件（skills/alpha/SKILL.md）。
    writeAgent(A, 'skills/alpha/SKILL.md', '# alpha from A\n');
    expect(homerJsonOk<{ status: string }>(A, ['push', '--yes', '--json']).status).toBe('pushed');
    writeAgent(B, 'skills/alpha/SKILL.md', '# alpha from B\n');

    // B pull --yes：冲突默认策略 = 保留本地 → conflicts-remain exit 1。
    const pulled = homer(B, ['pull', '--yes', '--json']);
    expect(pulled.code).toBe(1);
    const pullReport = JSON.parse(pulled.out) as {
      status: string;
      conflicts: { relPath: string; reason: string }[];
      errors: string[];
    };
    expect(pullReport.status).toBe('conflicts-remain');
    expect(pullReport.conflicts).toEqual([
      expect.objectContaining({ relPath: 'alpha/SKILL.md', reason: 'modify-vs-modify' }),
    ]);
    expect(pullReport.errors.join('\n')).toContain('homer merge');
    // 本地版本原样保留。
    expect(readAgent(B, 'skills/alpha/SKILL.md')).toBe('# alpha from B\n');

    // B merge --accept-remote → B 工具目录 == A 版本；merge 把裁决推回 store/origin。
    const merged = homerJsonOk<{ status: string; resolutions: { relPath: string; choice: string }[] }>(B, [
      'merge',
      '--accept-remote',
      '--json',
    ]);
    expect(merged.status).toBe('resolved');
    expect(merged.resolutions[0]).toMatchObject({ relPath: 'alpha/SKILL.md', choice: 'remote' });
    expect(readAgent(B, 'skills/alpha/SKILL.md')).toBe('# alpha from A\n');
    expect(readState(B.home)['lastSyncCommit']).toBe(sha(B.home));
    expect(sha(B.home)).toBe(sha(B.home, '@{upstream}'));

    // A pull → 拿到一致状态（无漂移、内容相同）。
    const aPull = homerJsonOk<{ status: string }>(A, ['pull', '--yes', '--json']);
    expect(aPull.status).toBe('no-drift');
    expect(readAgent(A, 'skills/alpha/SKILL.md')).toBe('# alpha from A\n');
    expect(fs.readFileSync(path.join(A.home, 'store/pi/skills/alpha/SKILL.md'), 'utf8')).toBe('# alpha from A\n');
  });
});

/* ------------------------------------------------------------------ */
/* ⑥ pull-delete 传播                                                  */
/* ------------------------------------------------------------------ */

describe('e2e M2 ⑥ pull-delete 传播：A 删文件 push → B pull --yes → B 本地被删且备份存在', () => {
  it('远端删除传播到工具目录，删除前内容进备份', () => {
    const { A, B } = setupRepo();
    expect(fs.existsSync(path.join(B.agentRoot, 'skills/beta/SKILL.md'))).toBe(true);

    fs.rmSync(path.join(A.agentRoot, 'skills', 'beta'), { recursive: true, force: true });
    expect(homerJsonOk<{ status: string }>(A, ['push', '--yes', '--json']).status).toBe('pushed');

    const report = homerJsonOk<{
      status: string;
      applied: { deleted: { relPath: string }[]; backupDir?: string };
    }>(B, ['pull', '--yes', '--json']);

    expect(report.status).toBe('applied');
    expect(report.applied.deleted).toEqual([expect.objectContaining({ relPath: 'beta/SKILL.md' })]);
    expect(fs.existsSync(path.join(B.agentRoot, 'skills/beta/SKILL.md'))).toBe(false);
    // 删除的空父目录被清理，但 category 根保留。
    expect(fs.existsSync(path.join(B.agentRoot, 'skills/beta'))).toBe(false);
    expect(fs.existsSync(path.join(B.agentRoot, 'skills'))).toBe(true);

    // 备份存在且内容 == 删除前内容。
    const backupDir = report.applied.backupDir;
    expect(backupDir).toBeDefined();
    expect(fs.readFileSync(path.join(backupDir as string, 'pi/skills/beta/SKILL.md'), 'utf8')).toBe(BETA_V1);
  });
});

/* ------------------------------------------------------------------ */
/* ⑦ status ↓ 计数激活                                                 */
/* ------------------------------------------------------------------ */

describe('e2e M2 ⑦ status ↓ 计数激活：git 模式下 ↓ 反映远端未拉变更（关闭 M1 已知限制 #1）', () => {
  it('A push 后 B status --json 出现非零 pull；B 直改 store 时置顶 ⚠ 告警', () => {
    const { A, B } = setupRepo();

    // 基线：B 与远端一致 → 无漂移。
    const baseline = homerJsonOk<{ adapters: { push: number; pull: number }[] }>(B, ['status', '--json']);
    expect(baseline.adapters[0]).toMatchObject({ push: 0, pull: 0 });

    // A 改 settings 并推送 —— 远端前进，B 尚未 pull。
    writeAgent(A, 'settings.json', `${JSON.stringify({ theme: 'dark', keep: 1 }, null, 2)}\n`);
    expect(homerJsonOk<{ status: string }>(A, ['push', '--yes', '--json']).status).toBe('pushed');

    // ↓ 激活：M1 时代这里恒为 0。
    const afterPush = homerJsonOk<{
      adapters: { push: number; pull: number; categories: { name: string; pull: number }[] }[];
    }>(B, ['status', '--json']);
    expect(afterPush.adapters[0]?.pull).toBeGreaterThan(0);
    expect(afterPush.adapters[0]?.pull).toBe(1);
    expect(afterPush.adapters[0]?.push).toBe(0);
    expect(afterPush.adapters[0]?.categories.find((c) => c.name === 'settings')?.pull).toBe(1);

    // 文本输出同样带 ↓（人类可读）。
    const text = homer(B, ['status']);
    expect(text.code).toBe(0);
    expect(text.out).toMatch(/↓[1-9]/);

    // 直改 store（未提交）→ ⚠ 置顶告警；计数仍是信息、exit 0。
    const storeSettings = path.join(B.home, 'store/pi/settings/settings.json');
    fs.writeFileSync(storeSettings, SETTINGS_V1.replace('light', 'violet'), 'utf8');

    const dirty = homerJsonOk<{ warnings?: string[] }>(B, ['status', '--json']);
    expect(dirty.warnings?.join('\n')).toContain('store 工作区有未提交的改动');

    const dirtyText = homer(B, ['status']);
    expect(dirtyText.code).toBe(0);
    expect(dirtyText.out.split('\n')[0]).toContain('⚠ store 工作区有未提交的改动');
  });
});

/* ------------------------------------------------------------------ */
/* ⑧ retention                                                         */
/* ------------------------------------------------------------------ */

describe('e2e M2 ⑧ retention：构造 10 个日期目录 → 任一写操作后 prune 至 7', () => {
  it('pull 应用后备份目录按日期保留最近 7 个（默认 backup.keep=7）', () => {
    const { A, B } = setupRepo();

    // 构造 10 个历史日期目录（20200101..20200110，远早于今天，不与本次备份的日期目录冲突）。
    const backupsDir = path.join(B.home, 'backups');
    const historical = Array.from({ length: 10 }, (_, i) => `202001${String(i + 1).padStart(2, '0')}`);
    for (const day of historical) {
      writeAbs(path.join(backupsDir, day, '000000-pull', 'pi/skills/old.md'), `keep ${day}\n`);
    }
    expect(fs.readdirSync(backupsDir).sort()).toEqual(historical);

    // 触发一次真实写操作：A 改 alpha 推送 → B pull --yes（覆盖旧内容 → 备份 + prune）。
    writeAgent(A, 'skills/alpha/SKILL.md', '# alpha v9\n');
    expect(homerJsonOk<{ status: string }>(A, ['push', '--yes', '--json']).status).toBe('pushed');
    const pulled = homerJsonOk<{ status: string }>(B, ['pull', '--yes', '--json']);
    expect(pulled.status).toBe('applied');

    // 保留最近 7 个日期目录：本次备份的「今天」+ 最旧的 4 个历史目录被删。
    const remaining = fs.readdirSync(backupsDir).sort();
    expect(remaining).toHaveLength(7);
    expect(remaining.slice(0, 6)).toEqual(['20200105', '20200106', '20200107', '20200108', '20200109', '20200110']);
    expect(remaining[6]).toMatch(/^\d{8}$/);
    expect(remaining[6]).not.toBe('20200110');
    for (const removed of ['20200101', '20200102', '20200103', '20200104']) {
      expect(fs.existsSync(path.join(backupsDir, removed)), `${removed} 应被 prune`).toBe(false);
    }
  });

  it('backup.keep 可配置（keep=3 → 保留最近 3 个日期目录）', () => {
    const { A, B } = setupRepo();
    // 把 B 的 homer.json 改成 keep=3（homer.json 改动不入同步 commit，不影响 pull 前置检查）。
    const configFile = path.join(B.home, 'homer.json');
    const config = JSON.parse(fs.readFileSync(configFile, 'utf8')) as Record<string, unknown>;
    writeAbs(configFile, `${JSON.stringify({ ...config, backup: { keep: 3 } }, null, 2)}\n`);

    const backupsDir = path.join(B.home, 'backups');
    for (const day of ['20200101', '20200102', '20200103', '20200104', '20200105']) {
      writeAbs(path.join(backupsDir, day, '000000-pull', 'pi/skills/old.md'), `keep ${day}\n`);
    }

    writeAgent(A, 'skills/alpha/SKILL.md', '# alpha v3\n');
    expect(homerJsonOk<{ status: string }>(A, ['push', '--yes', '--json']).status).toBe('pushed');
    expect(homerJsonOk<{ status: string }>(B, ['pull', '--yes', '--json']).status).toBe('applied');

    const remaining = fs.readdirSync(backupsDir).sort();
    expect(remaining).toHaveLength(3);
    expect(remaining.slice(0, 2)).toEqual(['20200104', '20200105']);
    expect(fs.existsSync(path.join(backupsDir, '20200101'))).toBe(false);
  });

  // 对抗式 review minor 4：组⑧ 原本只验证 pull 路径的 prune；merge 也做备份，但有很长一段时间
  // 漏掉了 prune（M1）。这里补 merge 路径的 prune 触发断言，使「任一写操作」名副其实。
  it('merge 路径也触发 prune（备份后保留最近 7 个日期目录）', () => {
    const { A, B } = setupRepo();

    // A 改 alpha 推送 → B pull 后本地已是 A 版本；再让 A / B 双方改同一文件制造冲突。
    writeAgent(A, 'skills/alpha/SKILL.md', '# alpha from A\n');
    expect(homerJsonOk<{ status: string }>(A, ['push', '--yes', '--json']).status).toBe('pushed');
    // B 先 pull 拿到 A 的版本（不产生备份目录，因为 B 未改 alpha 的旧版）
    expect(homerJsonOk(B, ['pull', '--yes', '--json']).status).toBe('applied');

    // 构造 10 个历史日期目录（在 pull 之后构造，避免被 pull 的 prune 提前删掉）。
    const backupsDir = path.join(B.home, 'backups');
    // 清掉前面 pull 留下的今日备份目录，使 prune 的保留窗口只由本测试构造。
    if (fs.existsSync(backupsDir)) fs.rmSync(backupsDir, { recursive: true, force: true });
    const historical = Array.from({ length: 10 }, (_, i) => `202001${String(i + 1).padStart(2, '0')}`);
    for (const day of historical) {
      writeAbs(path.join(backupsDir, day, '000000-pull', 'pi/skills/old.md'), `keep ${day}\n`);
    }
    expect(fs.readdirSync(backupsDir).sort()).toEqual(historical);

    // A 与 B 同时改同一 mirror 文件 → B pull 冲突；B merge --accept-remote 走备份 → prune。
    writeAgent(A, 'skills/alpha/SKILL.md', '# alpha A2\n');
    expect(homerJsonOk<{ status: string }>(A, ['push', '--yes', '--json']).status).toBe('pushed');
    writeAgent(B, 'skills/alpha/SKILL.md', '# alpha B2\n');
    expect(homer(B, ['pull', '--yes']).code).toBe(1);

    const merged = homerJsonOk<{ status: string; applied: { backupDir?: string } }>(B, [
      'merge', '--accept-remote', '--json',
    ]);
    expect(merged.status).toBe('resolved');
    expect(merged.applied.backupDir).toBeDefined();
    expect(path.basename(merged.applied.backupDir as string)).toMatch(/^\d{6}-merge$/);
    expect(readAgent(B, 'skills/alpha/SKILL.md')).toBe('# alpha A2\n');

    // prune 到 7：最旧 4 个历史日期目录被删，本次备份的「今天」保留。
    const remaining = fs.readdirSync(backupsDir).sort();
    expect(remaining).toHaveLength(7);
    expect(remaining.slice(0, 6)).toEqual([
      '20200105', '20200106', '20200107', '20200108', '20200109', '20200110',
    ]);
    expect(remaining[6]).toMatch(/^\d{8}$/);
    for (const removed of ['20200101', '20200102', '20200103', '20200104']) {
      expect(fs.existsSync(path.join(backupsDir, removed)), `${removed} 应被 merge 的 prune 删掉`).toBe(false);
    }
  });
});

/* ------------------------------------------------------------------ */
/* §5 Done：state.lastSyncCommit == git rev-parse HEAD（全链路抽样）    */
/* ------------------------------------------------------------------ */

describe('e2e M2 §5 Done：成功路径上 state.lastSyncCommit == git rev-parse HEAD', () => {
  it('push / pull / merge 三条成功路径都满足', () => {
    const { A, B, origin } = setupRepo();

    // push
    writeAgent(A, 'settings.json', `${JSON.stringify({ theme: 'dark' }, null, 2)}\n`);
    expect(homerJsonOk(A, ['push', '--yes', '--json'])).toBeDefined();
    expect(readState(A.home)['lastSyncCommit']).toBe(sha(A.home));

    // pull
    expect(homerJsonOk(B, ['pull', '--yes', '--json'])).toBeDefined();
    expect(readState(B.home)['lastSyncCommit']).toBe(sha(B.home));
    expect(sha(B.home)).toBe(sha(origin));

    // merge（同一 mirror 文件冲突后 --accept-remote）
    writeAgent(A, 'skills/alpha/SKILL.md', '# alpha A\n');
    expect(homerJsonOk(A, ['push', '--yes', '--json'])).toBeDefined();
    writeAgent(B, 'skills/alpha/SKILL.md', '# alpha B\n');
    expect(homer(B, ['pull', '--yes']).code).toBe(1);
    expect(homerJsonOk(B, ['merge', '--accept-remote', '--json'])).toBeDefined();
    expect(readState(B.home)['lastSyncCommit']).toBe(sha(B.home));

    void gitTry(A.home, ['rev-parse', 'HEAD']);
  });
});
