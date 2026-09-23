/**
 * P3-W8 · `src/cli/commands/home.ts` 验收（docs/m3-plan.md §2.7 / §1-D5 / §3-P3-W8）。
 *
 * ## 隔离（硬约束）
 *
 * 每个用例 `mkdtemp` 造临时根目录：**双假 HOME**（A = 旧机、B = 新机）+ `git init --bare`
 * 假 origin。adapter root 一律写成 `~/.pi/agent`（`expandHome` 走进程 HOME），因此每个断言
 * 都在「HOME 指向临时目录」的窗口内执行（`withFakeHome`），**绝不碰真实 `~/.pi` / `~/.homer`**。
 * identity 全部 `generateIdentity()` 临时生成。
 *
 * ## 证据线（对应 §3-P3-W8 验收原文）
 *
 * 1. **全流程**：A 侧 `init`（手工写 config + `writeSnapshotToStore`）→ commit → push →
 *    `secret push` → B `runHome({repoUrl, mode:'merge'})`（**真 git clone**，identity 经
 *    `deps.clone` 注入位预置）→ 工具目录逐文件 == store 内容、密文解密归位（内容 == A 明文、0600）、
 *    `state.lastSyncCommit == HEAD`、doctor 报告附于结果；
 * 2. **三模式**：pull（远端覆盖 + local-only 保留 + 备份存在）、merge（冲突保留本地 →
 *    随后 `status` 出现 ↑ 漂移 = D5 语义锁定）、skip（零应用但 config/state 就位）；
 * 3. **交互**：fake PromptPort 记录 `select` 调用；非 TTY 无 mode/yes → CliError；
 *    `--yes` 默认 merge（且完全不创建 / 不触碰 port）；
 * 4. **边界**：home 目录非空 → CliError（且不尝试 clone）；clone 失败 → CliError 含摘要；
 *    仓库无 homer.json → CliError；identity 缺失 → 配置归位 + `secrets.skipped` + warning、
 *    整体仍 `homed`（exit 0，doctor 的 age fail 不改变 home 退出码）；adapter root 缺失 →
 *    M-A 守卫（local := remote，零写入）。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { run, type CliIO } from '../../src/cli/index.js';
import {
  buildFirstContactPreview,
  HOME_USAGE,
  renderHomeReport,
  runHome,
  type HomeReport,
} from '../../src/cli/commands/home.js';
import { runStatus } from '../../src/cli/commands/status.js';
import { runSecretPush } from '../../src/cli/commands/secret.js';
import type { PromptPort } from '../../src/cli/ui.js';
import { createAgeCryptoPort } from '../../src/core/age/cipher.js';
import { generateIdentity, identityFilePath, loadIdentity, writeIdentityFile } from '../../src/core/age/keys.js';
import type { AgeIdentity } from '../../src/core/age/types.js';
import { gitExec, headCommit } from '../../src/core/git/index.js';
import { CliError } from '../../src/core/errors.js';
import { getHomerPaths, type HomerPaths } from '../../src/core/paths.js';
import { loadState } from '../../src/core/state.js';
import { writeSnapshotToStore } from '../../src/core/store/store.js';
import { resolveCategoryFilePath } from '../../src/adapters/paths.js';
import type { AdapterSnapshot, HomerConfig } from '../../src/core/types.js';

/* ------------------------------------------------------------------ */
/* 隔离工具                                                            */
/* ------------------------------------------------------------------ */

const created: string[] = [];

function mkTmp(prefix: string): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), `homer-home-${prefix}-`));
  created.push(dir);
  return dir;
}

const savedHome = process.env['HOME'];

afterEach(() => {
  if (savedHome === undefined) delete process.env['HOME'];
  else process.env['HOME'] = savedHome;
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) fs.rmSync(dir, { recursive: true, force: true });
  }
});

/** 在「HOME = fakeHome」的窗口内执行（`~` 展开依赖它）。 */
async function withFakeHome<T>(fakeHome: string, fn: () => T | Promise<T>): Promise<T> {
  const previous = process.env['HOME'];
  process.env['HOME'] = fakeHome;
  try {
    return await fn();
  } finally {
    if (previous === undefined) delete process.env['HOME'];
    else process.env['HOME'] = previous;
  }
}

function gitOk(cwd: string, args: readonly string[]): string {
  const result = gitExec(cwd, args);
  if (!result.ok) throw new Error(`git ${args.join(' ')} 失败 (cwd=${cwd}): ${result.stderr}`);
  return result.stdout;
}

function configureUser(repo: string): void {
  gitOk(repo, ['config', 'user.email', 'homer-home@example.invalid']);
  gitOk(repo, ['config', 'user.name', 'Homer Home']);
}

function writeAbs(abs: string, content: string): void {
  fs.mkdirSync(path.dirname(abs), { recursive: true });
  fs.writeFileSync(abs, content, 'utf8');
}

function mode(abs: string): number {
  return fs.statSync(abs).mode & 0o777;
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

function capture(): { io: CliIO; out: string[]; err: string[] } {
  const out: string[] = [];
  const err: string[] = [];
  return { io: { out: (line) => out.push(line), err: (line) => err.push(line) }, out, err };
}

/** 执行并返回抛出的错误对象（未抛出 → 抛断言失败）。用于校验 `CliError` 的具体类型。 */
async function captureThrown(fn: () => unknown): Promise<unknown> {
  try {
    await fn();
  } catch (err) {
    return err;
  }
  throw new Error('预期抛出错误，但调用成功返回');
}

/** 记录调用的假 port：`select` 返回 `picked`，`confirm` 返回 `approve`。 */
interface RecordingPort extends PromptPort {
  selects: { message: string; options: readonly { value: string; label: string }[]; fallback: string }[];
  confirms: string[];
}

function fakeUi(picked: 'pull' | 'merge' | 'skip' = 'merge', approve = true): RecordingPort {
  const selects: RecordingPort['selects'] = [];
  const confirms: string[] = [];
  return {
    selects,
    confirms,
    async confirm(message: string): Promise<boolean> {
      confirms.push(message);
      return approve;
    },
    async select<T extends string>(
      message: string,
      options: readonly { value: T; label: string }[],
      fallback: T,
    ): Promise<T> {
      selects.push({ message, options, fallback });
      return picked as unknown as T;
    },
  };
}

/** 拒绝一切交互的 port（用于证明 `--yes` 路径完全不创建 / 不触碰 port）。 */
function explodingUi(): PromptPort {
  return {
    async confirm(): Promise<boolean> {
      throw new Error('port 不应被调用');
    },
    async select(): Promise<never> {
      throw new Error('port 不应被调用');
    },
  };
}

/* ------------------------------------------------------------------ */
/* fixture：A 侧配置中心                                                */
/* ------------------------------------------------------------------ */

const REMOTE_ALPHA = '# alpha remote\n';
const REMOTE_BETA = '# beta remote\n';
const REMOTE_SETTINGS = `${JSON.stringify({ theme: 'dark', keep: 1 }, null, 2)}\n`;

const LOCAL_ALPHA = '# alpha local\n';
const LOCAL_SETTINGS = `${JSON.stringify({ theme: 'light', keep: 1 }, null, 2)}\n`;

/** 明文里的独特片段（用于 bare origin `git grep` 铁证）。 */
const FRAGMENT = 'HOMER-HOME-PLAINTEXT-FRAGMENT-7c1d93';
const SECRET_PLAINTEXT = `API_TOKEN=${FRAGMENT}\nline-2: 不短于 17 字节的正文\n`;

const SECRET_NAME = 'a-secret';
const SECRET_DEST = '~/.secrets/a.env';

/** 冻结的测试 config（adapter root 用 `~` 前缀 → 依赖进程 HOME）。 */
function testConfig(secrets?: HomerConfig['secrets']): HomerConfig {
  const config: HomerConfig = {
    version: 1,
    adapters: {
      pi: {
        root: '~/.pi/agent',
        enabled: true,
        categories: {
          skills: { paths: ['skills/'], mode: 'mirror' },
          settings: { paths: ['settings.json'], mode: 'merge' },
        },
      },
    },
  };
  if (secrets !== undefined) config.secrets = secrets;
  return config;
}

function storeSnapshot(): AdapterSnapshot {
  return {
    adapterId: 'pi',
    categories: [
      {
        adapterId: 'pi',
        category: 'skills',
        mode: 'mirror',
        files: new Map([
          ['alpha/SKILL.md', { kind: 'file', content: REMOTE_ALPHA }],
          ['beta/SKILL.md', { kind: 'file', content: REMOTE_BETA }],
        ]),
      },
      {
        adapterId: 'pi',
        category: 'settings',
        mode: 'merge',
        files: new Map([['settings.json', { kind: 'json', content: REMOTE_SETTINGS }]]),
      },
    ],
  };
}

interface Fixture {
  root: string;
  origin: string;
  /** A 侧 homer 工作区（配置中心本体，不是 clone）。 */
  homeA: string;
  /** A 侧假 HOME（secrets.files 的 `~` 落点）。 */
  fakeHomeA: string;
  /** B 侧假 HOME（新机，工具目录 root 的 `~` 落点）。 */
  fakeHomeB: string;
  identityA: AgeIdentity;
  identityB: AgeIdentity;
  /** A 侧 HEAD（= B clone 后的 HEAD）。 */
  headA: string;
}

/**
 * 造假 origin：A 侧写 config + store 快照 → commit → `push -u` →（可选）`secret push`。
 *
 * `opts.homerJson=false` 用于「仓库不是 homer 配置中心」用例；`opts.secrets=false` 跳过密钥通道
 * （也就没有密钥归位可断言）。
 */
async function setupOrigin(
  opts: {
    homerJson?: boolean;
    secrets?: boolean;
    pushSecrets?: boolean;
    config?: HomerConfig;
    snapshot?: AdapterSnapshot;
  } = {},
): Promise<Fixture> {
  const root = mkTmp('origin');
  const origin = path.join(root, 'origin.git');
  gitOk(root, ['init', '--bare', '-b', 'main', origin]);

  const homeA = path.join(root, 'A', 'homer');
  const fakeHomeA = path.join(root, 'A', 'home');
  const fakeHomeB = path.join(root, 'B', 'home');
  fs.mkdirSync(fakeHomeA, { recursive: true });
  fs.mkdirSync(path.join(fakeHomeB, '.pi', 'agent'), { recursive: true });
  fs.mkdirSync(homeA, { recursive: true });

  gitOk(homeA, ['init', '-b', 'main']);
  configureUser(homeA);
  gitOk(homeA, ['remote', 'add', 'origin', origin]);

  const identityA = generateIdentity();
  const identityB = generateIdentity();
  const withSecrets = opts.secrets !== false;
  const withHomerJson = opts.homerJson !== false;

  const pathsA: HomerPaths = getHomerPaths({ HOMER_HOME: homeA });
  if (withHomerJson) {
    const secrets = withSecrets
      ? { recipients: [identityA.recipient, identityB.recipient], files: { [SECRET_NAME]: SECRET_DEST } }
      : undefined;
    const config =
      opts.config ?? testConfig(secrets);
    fs.writeFileSync(pathsA.configFile, `${JSON.stringify(config, null, 2)}\n`, 'utf8');
    writeSnapshotToStore(pathsA, opts.snapshot ?? storeSnapshot());
  } else {
    writeAbs(path.join(homeA, 'README.md'), '# not a homer config center\n');
  }

  gitOk(homeA, ['add', '-A']);
  gitOk(homeA, ['commit', '-m', 'fixture: config + store']);
  gitOk(homeA, ['push', '-u', 'origin', 'main']);

  if (withHomerJson && withSecrets && opts.pushSecrets !== false) {
    writeIdentityFile(pathsA, identityA);
    await withFakeHome(fakeHomeA, async () => {
      writeAbs(path.join(fakeHomeA, '.secrets', 'a.env'), SECRET_PLAINTEXT);
      const pushed = await runSecretPush({ homerHome: homeA, yes: true }, { age: crypto });
      expect(pushed.status, JSON.stringify(pushed.errors)).toBe('pushed');
    });
  }

  return {
    root,
    origin,
    homeA,
    fakeHomeA,
    fakeHomeB,
    identityA,
    identityB,
    headA: gitOk(homeA, ['rev-parse', 'HEAD']).trim(),
  };
}

const crypto = createAgeCryptoPort();

/** 真 clone（`execFile git clone` 的测试替身：同一 git 命令，路径可控）。 */
function realClone(repoUrl: string, destDir: string): void {
  fs.mkdirSync(path.dirname(destDir), { recursive: true });
  gitOk(path.dirname(destDir), ['clone', repoUrl, destDir]);
  configureUser(destDir);
}

/**
 * `deps.clone` 注入位：真 clone + 预置 B 侧 identity（模拟「本机已 keygen」）。
 * 目标目录在 clone 前必须为空（runHome 步骤 1 的守卫），故 identity 只能在 clone 之后落盘。
 */
function cloneWithIdentity(identity: AgeIdentity): (repoUrl: string, destDir: string) => Promise<void> {
  return async (repoUrl, destDir) => {
    realClone(repoUrl, destDir);
    writeIdentityFile(getHomerPaths({ HOMER_HOME: destDir }), identity);
  };
}

/** B 侧工具目录（fakeHome 下的 `.pi/agent`）。 */
function toolRootOf(fakeHome: string): string {
  return path.join(fakeHome, '.pi', 'agent');
}

/** 往 B 侧工具目录写文件（相对 adapter root）。 */
function seedTool(fakeHome: string, rel: string, content: string): void {
  writeAbs(path.join(toolRootOf(fakeHome), rel), content);
}

/**
 * 「工具目录 == store 内容」断言：逐 store 文件按 `resolveCategoryFilePath` 反算工具目录路径，
 * 比对内容；再确认工具目录没有多余文件（store 侧的 `.homer-complete` 标记除外）。
 */
function expectToolDirMatchesStore(paths: HomerPaths, fakeHome: string, config: HomerConfig): void {
  const storeRoot = path.join(paths.storeDir, 'pi');
  const stored = listFiles(storeRoot).filter((rel) => rel !== '.homer-complete');
  expect(stored.length).toBeGreaterThan(0);

  const expected: string[] = [];
  for (const rel of stored) {
    const [category, ...rest] = rel.split('/');
    const adapterConfig = config.adapters['pi']!;
    const categoryConfig = adapterConfig.categories[category!]!;
    const target = resolveCategoryFilePath(
      toolRootOf(fakeHome),
      categoryConfig,
      rest.join('/'),
    );
    expect(fs.readFileSync(target, 'utf8'), rel).toBe(fs.readFileSync(path.join(storeRoot, rel), 'utf8'));
    expected.push(path.relative(toolRootOf(fakeHome), target).split(path.sep).join('/'));
  }

  expect(listFiles(toolRootOf(fakeHome)).sort()).toEqual(expected.sort());
}

/** 备份目录里所有文件的（相对 backupsDir 的）路径。 */
function backupEntries(paths: HomerPaths): string[] {
  return listFiles(paths.backupsDir);
}

/* ================================================================== */
/* A. 全流程                                                           */
/* ================================================================== */

describe('W8 · home 全流程（A 造 origin → B 一键归位）', () => {
  it('merge 模式：工具目录 == store 内容、密文解密归位（0600）、state == HEAD、doctor 附于结果', async () => {
    const fx = await setupOrigin();
    const homeB = path.join(fx.root, 'B', 'homer');
    const pathsB = getHomerPaths({ HOMER_HOME: homeB });
    const config = testConfig({
      recipients: [fx.identityA.recipient, fx.identityB.recipient],
      files: { [SECRET_NAME]: SECRET_DEST },
    });

    const report = await withFakeHome(fx.fakeHomeB, () =>
      runHome(
        { homerHome: homeB, repoUrl: fx.origin, mode: 'merge', yes: true },
        { clone: cloneWithIdentity(fx.identityB), age: crypto },
      ),
    );

    // ---- 报告形状 ----
    expect(report.status).toBe('homed');
    expect(report.ok).toBe(true);
    expect(report.cloned).toBe(true);
    expect(report.adapterIds).toEqual(['pi']);
    expect(report.firstContact?.mode).toBe('merge');
    expect(report.errors).toEqual([]);
    expect(report.doctor).toBeDefined();

    // ---- 工具目录 == store 内容（逐字节）----
    expectToolDirMatchesStore(pathsB, fx.fakeHomeB, config);

    // ---- 密钥归位：内容 == A 明文、0600、报告只含 name ----
    const dest = path.join(fx.fakeHomeB, '.secrets', 'a.env');
    expect(fs.readFileSync(dest, 'utf8')).toBe(SECRET_PLAINTEXT);
    expect(mode(dest)).toBe(0o600);
    expect(report.secrets.pulled).toEqual([SECRET_NAME]);
    expect(report.secrets.skipped).toEqual([]);
    expect(report.secrets.errors).toEqual([]);
    expect(JSON.stringify(report)).not.toContain(FRAGMENT);

    // ---- state.lastSyncCommit == HEAD ----
    expect(headCommit(homeB)).toBe(fx.headA);
    expect(loadState(pathsB).lastSyncCommit).toBe(fx.headA);
    expect(loadState(pathsB).lastSyncCommand).toBe('pull');

    // ---- origin 侧只有密文（明文 / 私钥都没进 git）----
    expect(gitExec(fx.origin, ['grep', FRAGMENT, 'HEAD']).stdout).toBe('');
    expect(gitExec(fx.origin, ['grep', '-I', 'AGE-SECRET-KEY', 'HEAD']).stdout).toBe('');
    expect(gitOk(fx.origin, ['ls-tree', '-r', '--name-only', 'HEAD', '--', 'secrets/']).trim()).toBe(
      `secrets/${SECRET_NAME}.age`,
    );

    // ---- doctor 检查项齐全且 age 项通过（identity 就位 + 密文可解）----
    const doctor = report.doctor!;
    expect(doctor.checks.map((check) => check.id)).toEqual([
      'config', 'repo', 'store-clean', 'remote', 'adapters', 'age', 'machine', 'required',
    ]);
    expect(doctor.checks.find((check) => check.id === 'age')?.status).toBe('ok');
    expect(doctor.ok).toBe(true);
  });

  it('分发层退出码：homed → 0（`--json` 可 parse）', async () => {
    const fx = await setupOrigin();
    const homeB = path.join(fx.root, 'B', 'homer');
    const cap = capture();

    const code = await withFakeHome(fx.fakeHomeB, () =>
      run(['home', fx.origin, '--home', homeB, '--mode', 'merge', '--yes', '--json'], cap.io),
    );

    expect(code).toBe(0);
    const parsed = JSON.parse(cap.out.join('\n')) as HomeReport;
    expect(parsed.status).toBe('homed');
    expect(parsed.ok).toBe(true);
  });

  it('真 clone（不注入 deps.clone）也能跑通：工具目录归位 + state 就位', async () => {
    const fx = await setupOrigin({ secrets: false });
    const homeB = path.join(fx.root, 'B', 'homer');
    const pathsB = getHomerPaths({ HOMER_HOME: homeB });

    const report = await withFakeHome(fx.fakeHomeB, () =>
      runHome({ homerHome: homeB, repoUrl: fx.origin, mode: 'pull', yes: true }),
    );

    expect(report.status).toBe('homed');
    expectToolDirMatchesStore(pathsB, fx.fakeHomeB, testConfig());
    expect(loadState(pathsB).lastSyncCommit).toBe(headCommit(homeB));
  });

  it('密钥目标已存在 → 先备份（备份内容 == 覆盖前），再写 0600', async () => {
    const fx = await setupOrigin();
    const homeB = path.join(fx.root, 'B', 'homer');
    const pathsB = getHomerPaths({ HOMER_HOME: homeB });
    const dest = path.join(fx.fakeHomeB, '.secrets', 'a.env');
    writeAbs(dest, 'OLD-SECRET-BODY\n');

    const report = await withFakeHome(fx.fakeHomeB, () =>
      runHome(
        { homerHome: homeB, repoUrl: fx.origin, mode: 'skip', yes: true },
        { clone: cloneWithIdentity(fx.identityB), age: crypto },
      ),
    );

    expect(report.status).toBe('homed');
    expect(fs.readFileSync(dest, 'utf8')).toBe(SECRET_PLAINTEXT);
    expect(mode(dest)).toBe(0o600);

    const backedUp = backupEntries(pathsB).filter((rel) => rel.endsWith('secret/a-secret'));
    expect(backedUp.length).toBe(1);
    expect(fs.readFileSync(path.join(pathsB.backupsDir, backedUp[0]!), 'utf8')).toBe('OLD-SECRET-BODY\n');
  });
});

/* ================================================================== */
/* B. 三模式（D5 语义锁定）                                             */
/* ================================================================== */

describe('W8 · 三模式语义', () => {
  it('pull：远端覆盖 + local-only 保留（无 delete）+ 备份存在', async () => {
    const fx = await setupOrigin({ secrets: false });
    const homeB = path.join(fx.root, 'B', 'homer');
    const pathsB = getHomerPaths({ HOMER_HOME: homeB });

    seedTool(fx.fakeHomeB, 'skills/alpha/SKILL.md', LOCAL_ALPHA);
    seedTool(fx.fakeHomeB, 'skills/local-only.md', '# local only\n');
    seedTool(fx.fakeHomeB, 'settings.json', LOCAL_SETTINGS);

    const report = await withFakeHome(fx.fakeHomeB, () =>
      runHome({ homerHome: homeB, repoUrl: fx.origin, mode: 'pull', yes: true }),
    );

    expect(report.status).toBe('homed');
    expect(report.firstContact?.mode).toBe('pull');
    // 每个 remote 文件一个 write（含双方都有且不同的 alpha / settings）；零 delete。
    expect(report.firstContact?.applied.written.length).toBe(3);
    expect(report.firstContact?.applied.deleted).toEqual([]);
    expect(report.firstContact?.conflicts).toEqual([]);

    expect(fs.readFileSync(path.join(toolRootOf(fx.fakeHomeB), 'skills/alpha/SKILL.md'), 'utf8')).toBe(REMOTE_ALPHA);
    expect(fs.readFileSync(path.join(toolRootOf(fx.fakeHomeB), 'settings.json'), 'utf8')).toBe(REMOTE_SETTINGS);
    // D5：local-only 文件不产生任何动作（空 base 下「删除」无依据）→ 原样保留。
    expect(fs.readFileSync(path.join(toolRootOf(fx.fakeHomeB), 'skills/local-only.md'), 'utf8')).toBe('# local only\n');

    // 备份存在且内容 == 覆盖前（alpha + settings 都被覆盖过）。
    const backups = backupEntries(pathsB);
    expect(backups.some((rel) => rel.endsWith('pi/skills/alpha/SKILL.md'))).toBe(true);
    const alphaBackup = backups.find((rel) => rel.endsWith('pi/skills/alpha/SKILL.md'))!;
    expect(fs.readFileSync(path.join(pathsB.backupsDir, alphaBackup), 'utf8')).toBe(LOCAL_ALPHA);
  });

  it('merge：冲突保留本地（remote 独有仍写入）→ 随后 `status` 出现 ↑ 漂移（D5 锁定）', async () => {
    const fx = await setupOrigin({ secrets: false });
    const homeB = path.join(fx.root, 'B', 'homer');
    const pathsB = getHomerPaths({ HOMER_HOME: homeB });

    seedTool(fx.fakeHomeB, 'skills/alpha/SKILL.md', LOCAL_ALPHA);       // mirror：双方不同 → conflict
    seedTool(fx.fakeHomeB, 'skills/local-only.md', '# local only\n');   // mirror：local-only → 零动作
    seedTool(fx.fakeHomeB, 'settings.json', LOCAL_SETTINGS);            // merge：同键不同值 → conflict

    const report = await withFakeHome(fx.fakeHomeB, () =>
      runHome({ homerHome: homeB, repoUrl: fx.origin, mode: 'merge', yes: true }),
    );

    expect(report.status).toBe('homed');
    expect(report.firstContact?.mode).toBe('merge');

    // 冲突项整体保留本地（不被删除、不被覆盖）。
    const conflicts = report.firstContact!.conflicts;
    expect(conflicts.map((conflict) => conflict.relPath).sort()).toEqual(['alpha/SKILL.md', 'settings.json']);
    expect(fs.readFileSync(path.join(toolRootOf(fx.fakeHomeB), 'skills/alpha/SKILL.md'), 'utf8')).toBe(LOCAL_ALPHA);
    expect(fs.readFileSync(path.join(toolRootOf(fx.fakeHomeB), 'settings.json'), 'utf8')).toBe(LOCAL_SETTINGS);

    // remote 独有 → write；local-only 保留。
    expect(report.firstContact!.applied.written.map((item) => item.relPath)).toEqual(['beta/SKILL.md']);
    expect(fs.readFileSync(path.join(toolRootOf(fx.fakeHomeB), 'skills/beta/SKILL.md'), 'utf8')).toBe(REMOTE_BETA);
    expect(fs.readFileSync(path.join(toolRootOf(fx.fakeHomeB), 'skills/local-only.md'), 'utf8')).toBe('# local only\n');

    // D5：state = HEAD（远端已被看见），保留的本地内容自然成为 push 漂移。
    expect(loadState(pathsB).lastSyncCommit).toBe(headCommit(homeB));

    const status = await withFakeHome(fx.fakeHomeB, () => runStatus({ homerHome: homeB }));
    const push = status.adapters.reduce((sum, adapter) => sum + adapter.push, 0);
    const pull = status.adapters.reduce((sum, adapter) => sum + adapter.pull, 0);
    expect(push).toBeGreaterThan(0);
    expect(pull).toBe(0);
  });

  it('skip：零应用但 config / state 就位', async () => {
    const fx = await setupOrigin({ secrets: false });
    const homeB = path.join(fx.root, 'B', 'homer');
    const pathsB = getHomerPaths({ HOMER_HOME: homeB });

    seedTool(fx.fakeHomeB, 'skills/alpha/SKILL.md', LOCAL_ALPHA);

    const report = await withFakeHome(fx.fakeHomeB, () =>
      runHome({ homerHome: homeB, repoUrl: fx.origin, mode: 'skip', yes: true }),
    );

    expect(report.status).toBe('homed');
    expect(report.firstContact?.mode).toBe('skip');
    expect(report.firstContact?.applied).toEqual({ written: [], deleted: [], conflicts: [] });
    expect(report.firstContact?.conflicts).toEqual([]);

    // 零应用：既有文件原样、remote 独有文件没被写下来。
    expect(fs.readFileSync(path.join(toolRootOf(fx.fakeHomeB), 'skills/alpha/SKILL.md'), 'utf8')).toBe(LOCAL_ALPHA);
    expect(fs.existsSync(path.join(toolRootOf(fx.fakeHomeB), 'skills/beta/SKILL.md'))).toBe(false);
    expect(backupEntries(pathsB)).toEqual([]);

    // config / state 就位。
    expect(fs.existsSync(pathsB.configFile)).toBe(true);
    expect(loadState(pathsB).lastSyncCommit).toBe(headCommit(homeB));
  });
});

/* ================================================================== */
/* C. 交互                                                             */
/* ================================================================== */

describe('W8 · 首次对接交互', () => {
  it('fake PromptPort：select 被调用（Pull/Merge/Skip 三选一）+ confirm 收到预览', async () => {
    const fx = await setupOrigin();
    const homeB = path.join(fx.root, 'B', 'homer');
    const port = fakeUi('pull');

    const report = await withFakeHome(fx.fakeHomeB, () =>
      runHome(
        { homerHome: homeB, repoUrl: fx.origin },
        { clone: cloneWithIdentity(fx.identityB), age: crypto, ui: port },
      ),
    );

    expect(report.status).toBe('homed');
    expect(port.selects.length).toBe(1);
    expect(port.selects[0]!.options.map((option) => option.value)).toEqual(['pull', 'merge', 'skip']);
    expect(port.selects[0]!.fallback).toBe('merge');
    expect(report.firstContact?.mode).toBe('pull');

    // 确认门槛：actions>0 或有待归位密钥 → confirm 收到含计数与密钥清单的预览。
    expect(port.confirms.length).toBe(1);
    expect(port.confirms[0]).toContain('首次对接（pull）');
    expect(port.confirms[0]).toContain(`密钥归位: 1 个（${SECRET_NAME}）`);
  });

  it('select 选 skip → 零应用（交互结果被采纳）', async () => {
    const fx = await setupOrigin({ secrets: false });
    const homeB = path.join(fx.root, 'B', 'homer');
    const port = fakeUi('skip');

    const report = await withFakeHome(fx.fakeHomeB, () =>
      runHome({ homerHome: homeB, repoUrl: fx.origin }, { ui: port }),
    );

    expect(report.firstContact?.mode).toBe('skip');
    expect(report.firstContact?.applied.written).toEqual([]);
    expect(port.confirms.length).toBe(0); // skip 且无密钥 → 无动作可确认
  });

  it('用户拒绝 confirm → aborted（零写入 / 零 state）', async () => {
    const fx = await setupOrigin();
    const homeB = path.join(fx.root, 'B', 'homer');
    const pathsB = getHomerPaths({ HOMER_HOME: homeB });
    const port = fakeUi('pull', false);

    const report = await withFakeHome(fx.fakeHomeB, () =>
      runHome(
        { homerHome: homeB, repoUrl: fx.origin },
        { clone: cloneWithIdentity(fx.identityB), age: crypto, ui: port },
      ),
    );

    expect(report.status).toBe('aborted');
    expect(report.ok).toBe(false);
    expect(report.firstContact?.applied.written).toEqual([]);
    expect(fs.existsSync(path.join(toolRootOf(fx.fakeHomeB), 'settings.json'))).toBe(false);
    expect(fs.existsSync(pathsB.stateFile)).toBe(false);
    expect(fs.existsSync(path.join(fx.fakeHomeB, '.secrets', 'a.env'))).toBe(false);
    expect(report.errors.join('\n')).toContain('已取消');
  });

  it('非 TTY 且无 --mode / --yes → CliError（提示 --mode 或 --yes）', async () => {
    const fx = await setupOrigin({ secrets: false });
    const homeB = path.join(fx.root, 'B', 'homer');

    await expect(
      withFakeHome(fx.fakeHomeB, () => runHome({ homerHome: homeB, repoUrl: fx.origin })),
    ).rejects.toThrow(/非交互环境无法选择首次对接模式/);

    // clone 已发生（步骤 2 先于步骤 6）；步骤 6 在任何应用 / state 写入之前失败。
    const pathsB = getHomerPaths({ HOMER_HOME: homeB });
    expect(fs.existsSync(pathsB.stateFile)).toBe(false);

    // 分发层同一形态（用另一个空目录，避免被上面那次 clone 占用）。
    const cap = capture();
    const homeC = path.join(fx.root, 'C', 'homer');
    const code = await withFakeHome(fx.fakeHomeB, () =>
      run(['home', fx.origin, '--home', homeC], cap.io),
    );
    expect(code).toBe(1);
    expect(cap.err.join('\n')).toContain('非交互环境无法选择首次对接模式');
    expect(cap.err.join('\n')).toContain('--mode');
    expect(fs.existsSync(getHomerPaths({ HOMER_HOME: homeC }).stateFile)).toBe(false);
  });

  it('--yes 无 mode → 默认 merge，且完全不创建 / 不触碰 port', async () => {
    const fx = await setupOrigin({ secrets: false });
    const homeB = path.join(fx.root, 'B', 'homer');

    seedTool(fx.fakeHomeB, 'skills/alpha/SKILL.md', LOCAL_ALPHA);

    const report = await withFakeHome(fx.fakeHomeB, () =>
      runHome({ homerHome: homeB, repoUrl: fx.origin, yes: true }, { ui: explodingUi() }),
    );

    expect(report.status).toBe('homed');
    expect(report.firstContact?.mode).toBe('merge');
    // merge 语义确实生效（默认 merge 不是「什么都不做」）：remote-only 的 beta + settings 被写下来；
    // 双方都有且不同的 alpha 作为冲突保留本地。
    expect(report.firstContact!.applied.written.map((item) => item.relPath).sort()).toEqual([
      'beta/SKILL.md',
      'settings.json',
    ]);
    expect(report.firstContact!.conflicts.map((conflict) => conflict.relPath)).toEqual(['alpha/SKILL.md']);
    expect(fs.readFileSync(path.join(toolRootOf(fx.fakeHomeB), 'skills/alpha/SKILL.md'), 'utf8')).toBe(LOCAL_ALPHA);
  });
});

/* ================================================================== */
/* D. 边界                                                             */
/* ================================================================== */

describe('W8 · 边界与失败形态', () => {
  it('home 目录非空 → CliError，且不尝试 clone', async () => {
    const fx = await setupOrigin({ secrets: false });
    const homeB = path.join(fx.root, 'B', 'homer');
    fs.mkdirSync(homeB, { recursive: true });
    fs.writeFileSync(path.join(homeB, 'important.txt'), 'user data\n', 'utf8');

    let cloneCalls = 0;

    const thrown = await captureThrown(() =>
      withFakeHome(fx.fakeHomeB, () =>
        runHome(
          { homerHome: homeB, repoUrl: fx.origin, mode: 'merge', yes: true },
          {
            clone: async () => {
              cloneCalls += 1;
            },
          },
        ),
      ),
    );
    // 契约：是 CliError（分发层据此打印 message(+hint) 并 exit 1），不是裸 Error。
    expect(thrown).toBeInstanceOf(CliError);
    expect((thrown as Error).message).toContain('目标目录非空');

    expect(cloneCalls).toBe(0);
    expect(fs.readFileSync(path.join(homeB, 'important.txt'), 'utf8')).toBe('user data\n');
    expect(fs.readdirSync(homeB)).toEqual(['important.txt']);
  });

  it('clone 失败 → CliError 含 stderr / 错误摘要', async () => {
    const fx = await setupOrigin({ secrets: false });
    const homeB = path.join(fx.root, 'B', 'homer');

    await expect(
      withFakeHome(fx.fakeHomeB, () =>
        runHome(
          { homerHome: homeB, repoUrl: 'git@nonexistent.invalid:repo.git', mode: 'merge', yes: true },
          {
            clone: async () => {
              const err = new Error('Command failed: git clone');
              (err as Error & { stderr?: string }).stderr = 'fatal: repository not found (simulated)';
              throw err;
            },
          },
        ),
      ),
    ).rejects.toThrow(/clone 失败（fatal: repository not found \(simulated\)）/);

    // 分发层（真 clone，指向不存在的本地路径 → git 立即失败）：exit 1 + stderr 里能看到摘要。
    const cap = capture();
    const missingOrigin = path.join(fx.root, 'no-such-origin.git');
    const homeC = path.join(fx.root, 'C', 'homer');
    const code = await withFakeHome(fx.fakeHomeB, () =>
      run(['home', missingOrigin, '--home', homeC, '--mode', 'merge', '--yes'], cap.io),
    );
    expect(code).toBe(1);
    expect(cap.err.join('\n')).toContain('clone 失败');
  });

  it('仓库无 homer.json → CliError「不是 homer 配置中心」', async () => {
    const fx = await setupOrigin({ homerJson: false });
    const homeB = path.join(fx.root, 'B', 'homer');

    await expect(
      withFakeHome(fx.fakeHomeB, () =>
        runHome({ homerHome: homeB, repoUrl: fx.origin, mode: 'merge', yes: true }),
      ),
    ).rejects.toThrow(/不是 homer 配置中心/);

    const cap = capture();
    const homeC = path.join(fx.root, 'C', 'homer');
    const code = await withFakeHome(fx.fakeHomeB, () =>
      run(['home', fx.origin, '--home', homeC, '--mode', 'merge', '--yes'], cap.io),
    );
    expect(code).toBe(1);
    expect(cap.err.join('\n')).toContain('不是 homer 配置中心');
  });

  it('identity 缺失 → 配置归位 + secrets.skipped + warning，整体仍 homed（exit 0；doctor 的 age fail 不改变 home 退出码）', async () => {
    const fx = await setupOrigin();
    const homeB = path.join(fx.root, 'B', 'homer');
    const pathsB = getHomerPaths({ HOMER_HOME: homeB });

    const report = await withFakeHome(fx.fakeHomeB, () =>
      runHome(
        { homerHome: homeB, repoUrl: fx.origin, mode: 'merge', yes: true },
        { clone: realCloneWrapper(), age: crypto },
      ),
    );

    expect(report.status).toBe('homed');
    expect(report.ok).toBe(true);
    expect(report.secrets.pulled).toEqual([]);
    expect(report.secrets.skipped).toEqual([SECRET_NAME]);
    expect(report.secrets.errors).toEqual([]);
    expect(report.warnings.join('\n')).toContain('未找到本机 age identity');
    expect(report.warnings.join('\n')).toContain('homer secret keygen');

    // 配置照常归位 + state 就位。
    expectToolDirMatchesStore(pathsB, fx.fakeHomeB, testConfig());
    expect(loadState(pathsB).lastSyncCommit).toBe(headCommit(homeB));
    // 密钥目标没被写。
    expect(fs.existsSync(path.join(fx.fakeHomeB, '.secrets', 'a.env'))).toBe(false);

    // doctor 报 age fail，但 home 仍 exit 0。
    expect(report.doctor?.checks.find((check) => check.id === 'age')?.status).toBe('fail');
    expect(report.doctor?.ok).toBe(false);

    const cap = capture();
    const homeC = path.join(fx.root, 'C', 'homer');
    const code = await withFakeHome(fx.fakeHomeB, () =>
      run(['home', fx.origin, '--home', homeC, '--mode', 'merge', '--yes'], cap.io),
    );
    expect(code).toBe(0);
    expect(cap.err.join('\n')).toBe('');
  });

  it('vault 密文缺失（旧机未 secret push）→ secrets.errors 含缺失清单 + 零写入，仍 homed', async () => {
    const fx = await setupOrigin({ pushSecrets: false });
    const homeB = path.join(fx.root, 'B', 'homer');

    const report = await withFakeHome(fx.fakeHomeB, () =>
      runHome(
        { homerHome: homeB, repoUrl: fx.origin, mode: 'skip', yes: true },
        { clone: cloneWithIdentity(fx.identityB), age: crypto },
      ),
    );

    expect(report.status).toBe('homed');
    expect(report.secrets.pulled).toEqual([]);
    expect(report.secrets.skipped).toEqual([]);
    expect(report.secrets.errors.join('\n')).toContain(`secrets/${SECRET_NAME}.age`);
    expect(report.secrets.errors.join('\n')).toContain('vault 文件缺失');
    expect(report.warnings.join('\n')).toContain('homer secret push');
    expect(fs.existsSync(path.join(fx.fakeHomeB, '.secrets', 'a.env'))).toBe(false);
  });

  it('本机 identity 不是 recipient → secrets.errors + warning，零写入，仍 homed', async () => {
    const fx = await setupOrigin();
    const homeB = path.join(fx.root, 'B', 'homer');
    const stranger = generateIdentity(); // 从未出现在 A 的 recipients 里

    const report = await withFakeHome(fx.fakeHomeB, () =>
      runHome(
        { homerHome: homeB, repoUrl: fx.origin, mode: 'merge', yes: true },
        { clone: cloneWithIdentity(stranger), age: crypto },
      ),
    );

    expect(report.status).toBe('homed');
    expect(report.secrets.pulled).toEqual([]);
    expect(report.secrets.skipped).toEqual([]);
    expect(report.secrets.errors.join('\n')).toContain(SECRET_NAME);
    expect(fs.existsSync(path.join(fx.fakeHomeB, '.secrets', 'a.env'))).toBe(false);

    // 报告绝不回显私钥 / 明文。
    expect(JSON.stringify(report)).not.toContain('AGE-SECRET-KEY');
    expect(JSON.stringify(report)).not.toContain(FRAGMENT);
  });

  it('adapter root 缺失 → M-A 守卫（local := remote，零写入）+ 报告警告', async () => {
    const fx = await setupOrigin({ secrets: false });
    const homeB = path.join(fx.root, 'B', 'homer');
    // 删掉 B 的工具目录（模拟「工具未安装」）。
    fs.rmSync(toolRootOf(fx.fakeHomeB), { recursive: true, force: true });

    const report = await withFakeHome(fx.fakeHomeB, () =>
      runHome({ homerHome: homeB, repoUrl: fx.origin, mode: 'merge', yes: true }),
    );

    expect(report.status).toBe('homed');
    expect(report.firstContact?.applied.written).toEqual([]);
    expect(report.warnings.join('\n')).toContain('adapter root 不可读');
    expect(fs.existsSync(path.join(toolRootOf(fx.fakeHomeB), 'settings.json'))).toBe(false);
    // config / state 仍就位（clone 是成功的）。
    expect(loadState(getHomerPaths({ HOMER_HOME: homeB })).lastSyncCommit).toBe(headCommit(homeB));
  });

  it('excludeKeys：store 里的 __REQUIRED__ 占位符永不流入工具目录（pull 模式）+ doctor 报 required warn', async () => {
    // A 侧 store 里带着 push 时替换出的占位符（模拟旧机 excludeKeys 通道的产物）。
    const config: HomerConfig = {
      version: 1,
      adapters: {
        pi: {
          root: '~/.pi/agent',
          enabled: true,
          categories: {
            skills: { paths: ['skills/'], mode: 'mirror' },
            settings: { paths: ['settings.json'], mode: 'merge', excludeKeys: ['apiKeys'] },
          },
        },
      },
    };
    const snapshot: AdapterSnapshot = {
      adapterId: 'pi',
      categories: [
        { adapterId: 'pi', category: 'skills', mode: 'mirror', files: new Map([['alpha/SKILL.md', { kind: 'file', content: REMOTE_ALPHA }]]) },
        {
          adapterId: 'pi',
          category: 'settings',
          mode: 'merge',
          files: new Map([
            ['settings.json', { kind: 'json', content: `${JSON.stringify({ apiKeys: '__REQUIRED__', theme: 'dark' }, null, 2)}\n` }],
          ]),
        },
      ],
    };
    const fx = await setupOrigin({ secrets: false, config, snapshot });
    const homeB = path.join(fx.root, 'B', 'homer');
    seedTool(fx.fakeHomeB, 'settings.json', `${JSON.stringify({ apiKeys: 'sk-local', theme: 'light' }, null, 2)}\n`);

    const report = await withFakeHome(fx.fakeHomeB, () =>
      runHome({ homerHome: homeB, repoUrl: fx.origin, mode: 'pull', yes: true }),
    );

    expect(report.status).toBe('homed');
    const written = fs.readFileSync(path.join(toolRootOf(fx.fakeHomeB), 'settings.json'), 'utf8');
    expect(written).not.toContain('__REQUIRED__');
    expect(JSON.parse(written)).toEqual({ theme: 'dark' });

    // 占位符残留对 doctor 可见（M2 遗留关闭）：required 项 warn，且不影响 home 退出码。
    const required = report.doctor?.checks.find((check) => check.id === 'required');
    expect(required?.status).toBe('warn');
    expect(required?.details?.join('\n')).toContain('apiKeys');
  });

  it('secrets.files 为空 → 不做密钥归位（无需 identity）', async () => {
    const fx = await setupOrigin({ secrets: false });
    const homeB = path.join(fx.root, 'B', 'homer');

    const report = await withFakeHome(fx.fakeHomeB, () =>
      runHome({ homerHome: homeB, repoUrl: fx.origin, mode: 'merge', yes: true }),
    );

    expect(report.status).toBe('homed');
    expect(report.secrets).toEqual({ pulled: [], skipped: [], errors: [] });
    expect(report.warnings.join('\n')).not.toContain('identity');
  });
});

/** `realClone` 的 `deps.clone` 形态（async，签名对齐）。 */
function realCloneWrapper(): (repoUrl: string, destDir: string) => Promise<void> {
  return async (repoUrl, destDir) => {
    realClone(repoUrl, destDir);
  };
}

/* ================================================================== */
/* E. 渲染与预览（纯函数）                                              */
/* ================================================================== */

describe('W8 · 渲染与预览', () => {
  const previewConfig: HomerConfig = {
    version: 1,
    adapters: {},
    secrets: { files: { 'a-secret': '/tmp/a.env' } },
  };

  it('buildFirstContactPreview 列出计数 / 明细 / 密钥清单', () => {
    const plan = {
      mode: 'merge' as const,
      actions: [
        { type: 'write' as const, adapterId: 'pi', category: 'skills', relPath: 'a/SKILL.md', content: 'x' },
        {
          type: 'conflict' as const,
          adapterId: 'pi',
          category: 'settings',
          relPath: 'settings.json',
          reason: 'merge-keys' as const,
          keyPaths: ['theme'],
        },
      ],
    };

    const text = buildFirstContactPreview(previewConfig, plan, ['a-secret']);

    expect(text).toContain('首次对接（merge）: 写入 1  冲突 1  删除 0');
    expect(text).toContain('写入 pi/skills/a/SKILL.md');
    expect(text).toContain('冲突 pi/settings/settings.json [merge-keys]（冲突键: theme）');
    expect(text).toContain('密钥归位: 1 个（a-secret）');
    expect(text).toContain('/tmp/a.env');
  });

  it('renderHomeReport 渲染状态 / 首次对接 / 密钥 / 体检 / 提示行', () => {
    const report: HomeReport = {
      ok: true,
      status: 'homed',
      cloned: true,
      adapterIds: ['pi'],
      firstContact: {
        mode: 'merge',
        applied: { written: [{ adapterId: 'pi', category: 'skills', relPath: 'b/SKILL.md' }], deleted: [], conflicts: [] },
        conflicts: [{ type: 'conflict', adapterId: 'pi', category: 'skills', relPath: 'a/SKILL.md', reason: 'modify-vs-modify' }],
      },
      secrets: { pulled: ['a-secret'], skipped: [], errors: [] },
      doctor: { checks: [], ok: true },
      warnings: ['某告警'],
      errors: [],
    };

    const text = renderHomeReport(report);

    expect(text).toContain('homer home: homed');
    expect(text).toContain('首次对接: merge（写入 1 / 删除 0 / 冲突 1）');
    expect(text).toContain('密钥归位: 1 个（a-secret）');
    expect(text).toContain('体检: 通过');
    expect(text).toContain('⚠ 某告警');
    expect(text).toContain('homer status');
  });

  it('HOME_USAGE 是非空用法文本', () => {
    expect(HOME_USAGE.startsWith('用法: homer home <repo-url>')).toBe(true);
    expect(HOME_USAGE).toContain('--mode pull|merge|skip');
    expect(HOME_USAGE).toContain('--yes');
  });

  it('clone 后 identity 文件落在 keys/age.txt（测试替身与生产布局一致）', async () => {
    const fx = await setupOrigin({ secrets: false });
    const homeB = path.join(fx.root, 'B', 'homer');
    const pathsB = getHomerPaths({ HOMER_HOME: homeB });

    await withFakeHome(fx.fakeHomeB, () =>
      runHome(
        { homerHome: homeB, repoUrl: fx.origin, mode: 'skip', yes: true },
        { clone: cloneWithIdentity(fx.identityB) },
      ),
    );

    expect(loadIdentity(pathsB)?.recipient).toBe(fx.identityB.recipient);
    expect(fs.existsSync(identityFilePath(pathsB))).toBe(true);
  });
});
