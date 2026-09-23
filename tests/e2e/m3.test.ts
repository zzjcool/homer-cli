/**
 * M3 · P4-W9 集成验收 e2e —— **MVP 验收场景主线**（docs/m3-plan.md §3-P4 / §5 M3 Done；DESIGN §3）。
 *
 * 覆盖计划 §3-P4 的七组证据（①-⑦），全程隔离、绝无外部副作用：
 *
 *   <tmp>/origin.git        ← `git init --bare` 假 origin（纯文件系统路径，无网络）
 *   <tmp>/A/{home,homer}    ← 机器 A：假 HOME（三工具目录）+ HOMER_HOME（配置中心本体，A 自己 clone）
 *   <tmp>/A/external/...    ← pi fixture 的 symlink 逃逸目标（⑤ allowEscape 组用）
 *   <tmp>/B/... <tmp>/C/... ← 机器 B / C：全新假 HOME（工具 root 空目录）+ HOMER_HOME（home 命令 clone）
 *
 * 每条命令都以**真实子进程**跑 CLI 入口（`node --import tsx src/cli/index.ts`）：
 * argv 解析、退出码、stdout `--json` 形状全部钉在进程边界上（同 tests/e2e/m1.test.ts / m2.test.ts）。
 *
 * ## ① 机器 A 装配（`beforeAll` 一次性建好共享世界）
 *
 *   fixture 三工具（pi/herdr/opencode）→ `homer init --json`（**三 adapter 全注册**，依赖
 *   P4-W9 在 `init.ts` 的 `KNOWN_ADAPTERS` 注册 herdr/opencode）→ `homer secret keygen`
 *   → homer.json 追加 `secrets.recipients` / `secrets.files` + pi `allowEscape`（⑤）
 *   → 明文落 A 假 HOME 的目标路径 → `homer secret push --yes` → `homer push --yes`
 *   → origin 齐备（`store/` + `secrets/` + `homer.json`，且 `git grep` 无明文 / 无私钥）。
 *
 * ## ② 机器 B 一键归位（核心场景）
 *
 *   全新假 HOME → `homer home <origin> --yes` → 三工具目录逐字节 == A 侧 store（`__REQUIRED__`
 *   占位符例外：store 有、工具目录**绝不**有 = M2 不变量）→ 随后按 DESIGN §2.6 的**换设备两步**
 *   （本机 `secret keygen` → 旧机登记该 recipient 并重加密 push → 本机 `secret pull`）把密钥归位
 *   （明文 == A 明文、0600）→ `doctor --json` 无 fail → `status --json` 零漂移 + `state == HEAD`。
 *
 *   为什么 home 之后还要两步：新设备的 identity 在 `home` 之前并不存在（`keys/` 是 gitignored 的
 *   机器本地状态，永不随仓库走），故新设备首次必然是「先出公钥 → 旧机登记 → 重加密 → 拉取」。
 *   这正是 DESIGN §2.6 的轮转流程（③ 组验证同一机制）；`home` 已把**配置**一次性归位。
 *
 * ## ③ 换设备 = 加 recipient + 重加密
 *
 *   机器 C：`home --yes` → `secret keygen` → A 追加 C 公钥到 `secrets.recipients` → A `secret push`
 *   （密文变化 + 新 commit）→ C `secret pull --yes` 成功；**同时** B（旧设备）与 A 自己仍能解 ——
 *   多 recipient 的「旧新设备都可解」铁证。
 *
 * ## ④ `__REQUIRED__` 残留进 doctor（M2 遗留关闭）
 *
 *   pi `models.json` 的 `excludeKeys: ['apiKeys']` 使 store 留下 `"apiKeys": "__REQUIRED__"`
 *   → doctor 的 `required` 检查 warn 且 details 精确到 `pi/models/models.json: apiKeys`。
 *
 * ## ⑤ symlink 逃逸 allowlist（M1 已知限制关闭）
 *
 *   pi fixture 的 `skills/agent-browser` 是指向 root 之外的 symlink：`init` 时跳过 + 记 ScanError
 *   （报告 errors 可见）；homer.json 加 `allowEscape: ['skills/agent-browser']` 后 `push`，
 *   store 出现其内容（含子目录），status 无告警、无漂移。
 *
 * ## ⑥ install.sh 冒烟
 *
 *   **引用** W4 的既有用例（tests/e2e/install.test.ts：`npm pack` → 隔离 prefix 安装 →
 *   `$PREFIX/bin/homer --help` exit 0），本组只钉「脚本存在 + POSIX 语法 + 注入位契约 +
 *   W4 用例仍在 + CLI 自身 `--help` 可跑」，不重复跑一次数分钟的全局安装（会让全量 e2e 时长翻倍）。
 *
 * ## ⑦ 真实环境只读冒烟（可选）
 *
 *   `HOMER_HOME=<tmp>` + **真实 HOME**：`homer init --json` 扫出三 adapter（只读真实
 *   `~/.pi/agent`、`~/.config/herdr`、`~/.config/opencode`；写入只落临时 HOMER_HOME）；
 *   `homer doctor --offline --json` 报 age 未配置 → ok。
 *
 * ## 测试间依赖（刻意）
 *
 * ①② 在同一 `beforeAll` 里顺序完成并把中间报告存进 `world`，各 `it` 断言其中一片证据；
 * ③ 依赖 ② 建立的世界（B 的 identity 已登记）并对 A 的 vault 做一次真实轮转。这是里程碑级
 * e2e 的叙事结构（与 tests/e2e/m2.test.ts 的共享 fixture 同款）。
 */

import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { resolveCategoryFilePath } from '../../src/adapters/paths.js';
import type { HomerConfig } from '../../src/core/types.js';

/* ------------------------------------------------------------------ */
/* 隔离 harness                                                        */
/* ------------------------------------------------------------------ */

const REPO_ROOT = process.cwd();
const created: string[] = [];

function mkTmp(prefix: string): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), `homer-m3-e2e-${prefix}-`));
  created.push(dir);
  return dir;
}

afterAll(() => {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) {
      try {
        fs.chmodSync(dir, 0o755);
      } catch {
        /* 目录已不存在 */
      }
      fs.rmSync(dir, { recursive: true, force: true });
    }
  }
});

/* ------------------------------------------------------------------ */
/* git / fs 工具                                                       */
/* ------------------------------------------------------------------ */

interface GitOutcome {
  ok: boolean;
  out: string;
  err: string;
}

/** git（失败即抛：harness 里的 git 失败都是测试自身构造错误）。 */
function git(cwd: string, args: readonly string[]): string {
  return execFileSync('git', [...args], { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

function gitTry(cwd: string, args: readonly string[]): GitOutcome {
  try {
    return { ok: true, out: git(cwd, args), err: '' };
  } catch (error) {
    const failure = error as { stdout?: string | Buffer; stderr?: string | Buffer };
    return {
      ok: false,
      out: failure.stdout?.toString() ?? '',
      err: failure.stderr?.toString() ?? '',
    };
  }
}

function sha(cwd: string, ref = 'HEAD'): string {
  return git(cwd, ['rev-parse', ref]).trim();
}

function writeAbs(abs: string, content: string): void {
  fs.mkdirSync(path.dirname(abs), { recursive: true });
  fs.writeFileSync(abs, content, 'utf8');
}

function readAbs(abs: string): string {
  return fs.readFileSync(abs, 'utf8');
}

/** 递归列出目录下所有文件（相对该目录，posix 分隔符），排序。 */
function listFiles(dir: string, prefix = ''): string[] {
  if (!fs.existsSync(dir)) return [];
  return fs
    .readdirSync(dir, { withFileTypes: true })
    .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))
    .flatMap((entry) =>
      entry.isDirectory() ? listFiles(path.join(dir, entry.name), `${prefix}${entry.name}/`) : [`${prefix}${entry.name}`],
    );
}

function modeOf(abs: string): number {
  return fs.statSync(abs).mode & 0o777;
}

/* ------------------------------------------------------------------ */
/* 机器与三工具 fixture                                                 */
/* ------------------------------------------------------------------ */

interface Machine {
  name: string;
  /** 假 HOME：adapter root 的 `~` 展开基准，也是 secrets.files 目标的落点。 */
  fakeHome: string;
  /** HOMER_HOME：配置中心本体（A 自 clone / B、C 由 `homer home` clone）。 */
  home: string;
}

/** adapter root（`~` 前缀）在假 HOME 下的绝对路径。 */
function expandTilde(root: string, fakeHome: string): string {
  if (root === '~') return fakeHome;
  if (root.startsWith('~/')) return path.join(fakeHome, root.slice(2));
  return root;
}

function piRoot(m: Machine): string {
  return path.join(m.fakeHome, '.pi', 'agent');
}

function herdrRoot(m: Machine): string {
  return path.join(m.fakeHome, '.config', 'herdr');
}

function opencodeRoot(m: Machine): string {
  return path.join(m.fakeHome, '.config', 'opencode');
}

/* ---- 冻结的 fixture 内容（三方一致：A 侧工具目录 == store == B/C 侧工具目录） ---- */

const PI_SETTINGS = `${JSON.stringify({ theme: 'light', keep: 1 }, null, 2)}\n`;
/** 带 `excludeKeys: ['apiKeys']` 的 models.json → store 里该键值变 `__REQUIRED__`（④ 组）。 */
const PI_MODELS = `${JSON.stringify({ apiKeys: { openai: 'sk-a' }, models: { openai: { id: 'gpt' } } }, null, 2)}\n`;
const PI_ALPHA = '# alpha\n';
const PI_BETA = '# beta\n';
/** 逃逸 symlink 指向的外部内容（⑤ 组）。 */
const ESCAPE_SKILL = '# agent-browser (outside root)\n';
const ESCAPE_SUB = 'export const a = 1;\n';

const HERDR_CONFIG = '[ui]\ntheme = "dark"\n';
const OPENCODE_CONFIG = `${JSON.stringify(
  { $schema: 'https://opencode.ai/config.json', provider: { anthropic: { options: { apiKey: 'ccrb' } } } },
  null,
  2,
)}\n`;
const OPENCODE_PACKAGE = `${JSON.stringify(
  { name: 'opencode-plugins', dependencies: { '@opencode-ai/plugin': '1.0.0' } },
  null,
  2,
)}\n`;
const OPENCODE_LOCK = `${JSON.stringify({ name: 'opencode-plugins', lockfileVersion: 3 }, null, 2)}\n`;

/** 逃逸目标目录：与 pi root 同级但在其之外（`<tmp>/<name>/external/browser`）。 */
function escapeTarget(m: Machine): string {
  return path.join(path.dirname(m.fakeHome), 'external', 'browser');
}

/** 写三工具 fixture（含大量必须被 ignore 的垃圾）。 */
function buildToolFixtures(m: Machine): void {
  const pi = piRoot(m);
  writeAbs(path.join(pi, 'settings.json'), PI_SETTINGS);
  writeAbs(path.join(pi, 'models.json'), PI_MODELS);
  writeAbs(path.join(pi, 'skills', 'alpha', 'SKILL.md'), PI_ALPHA);
  writeAbs(path.join(pi, 'skills', 'beta', 'SKILL.md'), PI_BETA);
  // 垃圾：adapter 级 ignore / 「不在任何分类 path 内」都必须一条不进 store
  writeAbs(path.join(pi, 'auth.json'), '{"token":"x"}\n');
  writeAbs(path.join(pi, 'trust.json'), '{"trusted":[]}\n');
  writeAbs(path.join(pi, 'sessions', 's1.jsonl'), '{"turn":1}\n');
  writeAbs(path.join(pi, 'run-history.jsonl'), '{}\n');

  // ⑤ 逃逸 symlink：skills/agent-browser → root 之外的目录
  const target = escapeTarget(m);
  writeAbs(path.join(target, 'SKILL.md'), ESCAPE_SKILL);
  writeAbs(path.join(target, 'sub', 'a.ts'), ESCAPE_SUB);
  fs.symlinkSync(target, path.join(pi, 'skills', 'agent-browser'));

  const herdr = herdrRoot(m);
  writeAbs(path.join(herdr, 'config.toml'), HERDR_CONFIG);
  writeAbs(path.join(herdr, 'session.json'), '{"panes":[]}\n'); // 运行时状态 → ignore
  writeAbs(path.join(herdr, 'herdr.sock'), ''); // socket → ignore
  writeAbs(path.join(herdr, 'herdr.log'), 'debug\n'); // 日志 → ignore
  writeAbs(path.join(herdr, 'release-notes.json'), '{"version":"1.2.3"}\n'); // 机器相关 → ignore
  writeAbs(path.join(herdr, '.plugins.lock'), '{}\n'); // → ignore

  const opencode = opencodeRoot(m);
  writeAbs(path.join(opencode, 'opencode.json'), OPENCODE_CONFIG);
  writeAbs(path.join(opencode, 'package.json'), OPENCODE_PACKAGE);
  writeAbs(path.join(opencode, 'package-lock.json'), OPENCODE_LOCK);
  writeAbs(path.join(opencode, 'node_modules', '@opencode-ai', 'plugin', 'index.js'), 'module.exports={}\n');
  writeAbs(path.join(opencode, '.plugins.lock'), '{}\n');
  writeAbs(path.join(opencode, 'opencode.log'), 'log\n');
  writeAbs(path.join(opencode, '.gitignore'), 'node_modules\n');
}

/**
 * 建一台机器。
 * `fixtures: true` → 写三工具 fixture（机器 A）；
 * `fixtures: false` → 只建**空**工具 root 目录（新机器 B/C：目录必须存在，否则 home 的 M-A 守卫
 * 会把 local 视作 = remote → 零写入，「逐字节归位」就无从验证）。
 */
function makeMachine(root: string, name: string, opts: { fixtures: boolean }): Machine {
  const fakeHome = path.join(root, name, 'home');
  const home = path.resolve(path.join(root, name, 'homer'));
  fs.mkdirSync(fakeHome, { recursive: true });

  const m: Machine = { name, fakeHome, home };
  if (opts.fixtures) {
    buildToolFixtures(m);
  } else {
    fs.mkdirSync(piRoot(m), { recursive: true });
    fs.mkdirSync(herdrRoot(m), { recursive: true });
    fs.mkdirSync(opencodeRoot(m), { recursive: true });
  }
  return m;
}

/* ------------------------------------------------------------------ */
/* CLI 子进程                                                          */
/* ------------------------------------------------------------------ */

interface CliResult {
  code: number;
  out: string;
  err: string;
}

/**
 * 以真实子进程跑 CLI（HOME = 假 HOME，HOMER_HOME = 机器工作区）。
 * 子进程必须不带 `VITEST`：`src/cli/index.ts` 据此跳过入口副作用（否则不会真跑命令）。
 */
function homer(m: Machine, args: readonly string[], env: NodeJS.ProcessEnv = {}): CliResult {
  const childEnv: NodeJS.ProcessEnv = {
    ...process.env,
    HOME: m.fakeHome,
    HOMER_HOME: m.home,
    ...env,
  };
  delete childEnv['VITEST'];
  try {
    const out = execFileSync(process.execPath, ['--import', 'tsx', 'src/cli/index.ts', ...args], {
      cwd: REPO_ROOT,
      env: childEnv,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
      timeout: 120_000,
    });
    return { code: 0, out, err: '' };
  } catch (error) {
    const failure = error as { status?: number | null; stdout?: string | Buffer; stderr?: string | Buffer };
    return {
      code: typeof failure.status === 'number' ? failure.status : -1,
      out: failure.stdout?.toString() ?? '',
      err: failure.stderr?.toString() ?? '',
    };
  }
}

/** 跑一条命令并断言 exit 0 + 空 stderr，返回解析后的 `--json` 报告。 */
function homerJsonOk<T>(m: Machine, args: readonly string[], env: NodeJS.ProcessEnv = {}): T {
  const result = homer(m, args, env);
  expect(result.err, `stderr: ${result.err}`).toBe('');
  expect(result.code, `stdout: ${result.out}\nstderr: ${result.err}`).toBe(0);
  return JSON.parse(result.out) as T;
}

function readState(home: string): Record<string, unknown> {
  return JSON.parse(readAbs(path.join(home, 'state.json'))) as Record<string, unknown>;
}

/* ------------------------------------------------------------------ */
/* 断言工具                                                            */
/* ------------------------------------------------------------------ */

/** store 里的文件（相对 store/，posix），滤掉内部完整性标记。 */
function storeFiles(home: string): string[] {
  return listFiles(path.join(home, 'store'))
    .filter((rel) => !rel.endsWith('.homer-complete'))
    .sort();
}

/** category 配置的 excludeKeys（非空时该文件在 store / 工具目录之间必然不同）。 */
function categoryExcludeKeys(config: HomerConfig, adapterId: string, category: string): string[] {
  return config.adapters[adapterId]?.categories[category]?.excludeKeys ?? [];
}

interface StoreEntry {
  adapterId: string;
  category: string;
  relPath: string;
  storeContent: string;
  toolAbsPath: string;
}

/** store 文件 → （store 内容, 工具目录绝对路径），用扫描的逆映射（`resolveCategoryFilePath`）。 */
function storeEntries(m: Machine, config: HomerConfig): StoreEntry[] {
  return storeFiles(m.home).map((rel) => {
    const [adapterId, category, ...rest] = rel.split('/');
    const adapter = config.adapters[adapterId as string];
    if (adapter === undefined) throw new Error(`homer.json 缺 adapter: ${adapterId}`);
    const categoryCfg = adapter.categories[category as string];
    if (categoryCfg === undefined) throw new Error(`adapter ${adapterId} 缺 category ${category}`);
    const root = expandTilde(adapter.root, m.fakeHome);
    return {
      adapterId: adapterId as string,
      category: category as string,
      relPath: rest.join('/'),
      storeContent: readAbs(path.join(m.home, 'store', rel)),
      toolAbsPath: resolveCategoryFilePath(root, categoryCfg, rest.join('/')),
    };
  });
}

/** store 相对路径 → 「工具目录相对 root 的路径」标签（跨机断言用）。 */
function toolLabels(m: Machine, config: HomerConfig): string[] {
  return storeFiles(m.home)
    .map((rel) => {
      const [adapterId, category, ...rest] = rel.split('/');
      const root = expandTilde(config.adapters[adapterId as string]!.root, m.fakeHome);
      const target = resolveCategoryFilePath(
        root,
        config.adapters[adapterId as string]!.categories[category as string]!,
        rest.join('/'),
      );
      return `${adapterId}:${path.relative(root, target).split(path.sep).join('/')}`;
    })
    .sort();
}

/* ------------------------------------------------------------------ */
/* ① / ② 的共享世界                                                     */
/* ------------------------------------------------------------------ */

const SECRET_NAME = 'a-secret';
const SECRET_DEST = '~/.secrets/a.env';
/** 明文里的独特片段（用于 origin `git grep` 铁证）。 */
const SECRET_FRAGMENT = 'M3-E2E-PLAINTEXT-FRAGMENT-3f91c7';
const SECRET_PLAINTEXT = `API_TOKEN=${SECRET_FRAGMENT}\nsecond-line-body-long-enough\n`;

interface InitReportShape {
  homerHome: string;
  adapters: { id: string; categories: { name: string; fileCount: number }[] }[];
  errors: string[];
}

interface DoctorCheckShape {
  id: string;
  status: 'ok' | 'warn' | 'fail';
  message: string;
  details?: string[];
}

interface DoctorReportShape {
  checks: DoctorCheckShape[];
  ok: boolean;
}

interface StatusReportShape {
  adapters: { id: string; push: number; pull: number; conflicts: number; categories: unknown[] }[];
  errors: string[];
  warnings?: string[];
}

interface HomeReportShape {
  ok: boolean;
  status: string;
  cloned: boolean;
  adapterIds: string[];
  firstContact?: { mode: string; applied: { written: unknown[]; deleted: unknown[]; conflicts: unknown[] } };
  secrets: { pulled: string[]; skipped: string[]; errors: string[] };
  doctor?: DoctorReportShape;
  warnings: string[];
  errors: string[];
}

interface KeygenShape {
  ok: boolean;
  identityFile: string;
  recipient: string;
  created: boolean;
}

interface SecretPushShape {
  status: string;
  encrypted: string[];
  pushedToRemote: boolean;
  commit?: string;
}

interface SecretPullShape {
  status: string;
  pulled: string[];
}

interface World {
  tmp: string;
  origin: string;
  A: Machine;
  B: Machine;
  configA: HomerConfig;
  initReport: InitReportShape;
  keygenA: KeygenShape;
  secretPushA: SecretPushShape;
  pushA: SecretPushShape;
  homeReport: HomeReportShape;
  keygenB: KeygenShape;
  secretPushB: SecretPushShape;
  secretPullB: SecretPullShape;
  doctorA: DoctorReportShape;
  doctorB: DoctorReportShape;
  statusB: StatusReportShape;
}

let world: World;
/**
 * ② 的核心证据：`homer home` 刚结束时 B 侧三工具目录的文件清单（`<root>:<rel>`）。
 *
 * 为什么在 `beforeAll` 里快照：紧随其后的换设备两步会向 `~/.secrets/a.env` 写密钥；
 * 而「home 之后三工具目录逐字节 == store」必须在**其它命令未再写工具目录**的窗口内断言。
 */
let homePlacedTree: string[] = [];

/** ① 中途：改写 homer.json 并提交（真实用户会在 init 之后后置编辑配置）。 */
function patchConfigA(m: Machine, mutate: (config: HomerConfig) => void): void {
  const file = path.join(m.home, 'homer.json');
  const config = JSON.parse(readAbs(file)) as HomerConfig;
  mutate(config);
  fs.writeFileSync(file, `${JSON.stringify(config, null, 2)}\n`, 'utf8');
  git(m.home, ['add', 'homer.json']);
  git(m.home, ['commit', '-m', 'chore(m3-e2e): 更新 homer.json']);
}

/** ③ 换设备：把新设备的 recipient 追加进 A 的 `secrets.recipients` 并提交（密文随后由 secret push 重写）。 */
function appendRecipient(m: Machine, recipient: string): void {
  patchConfigA(m, (config) => {
    const recipients = config.secrets?.recipients ?? [];
    if (!recipients.includes(recipient)) recipients.push(recipient);
    config.secrets = { ...(config.secrets ?? {}), recipients };
  });
}

beforeAll(async () => {
  const tmp = mkTmp('world');
  const origin = path.join(tmp, 'origin.git');
  git(tmp, ['init', '--bare', '-b', 'main', origin]);

  /* ---------------- ① 机器 A 装配 ---------------- */

  const A = makeMachine(tmp, 'A', { fixtures: true });
  git(tmp, ['clone', origin, A.home]);
  git(A.home, ['config', 'user.email', 'homer-m3@example.invalid']);
  git(A.home, ['config', 'user.name', 'Homer M3 E2E']);

  const initReport = homerJsonOk<InitReportShape>(A, ['init', '--json']);
  const keygenA = homerJsonOk<KeygenShape>(A, ['secret', 'keygen', '--json']);

  // homer.json：密钥目标 + ⑤ 的 allowEscape（② 的换设备 recipient 在 B keygen 之后追加）
  patchConfigA(A, (config) => {
    config.secrets = { ...(config.secrets ?? {}), recipients: [keygenA.recipient], files: { [SECRET_NAME]: SECRET_DEST } };
    const pi = config.adapters['pi'];
    if (pi === undefined) throw new Error('homer.json 缺 pi adapter');
    pi.allowEscape = ['skills/agent-browser'];
  });

  // 明文落 A 假 HOME 的目标路径（`~/.secrets/a.env`），随后由 secret push 加密进 vault。
  writeAbs(path.join(A.fakeHome, '.secrets', 'a.env'), SECRET_PLAINTEXT);

  const secretPushA = homerJsonOk<SecretPushShape>(A, ['secret', 'push', '--yes', '--json']);
  const pushA = homerJsonOk<SecretPushShape>(A, ['push', '--yes', '--json']);
  const configA = JSON.parse(readAbs(path.join(A.home, 'homer.json'))) as HomerConfig;

  /* ---------------- ② 机器 B 一键归位 ---------------- */

  const B = makeMachine(tmp, 'B', { fixtures: false });
  const homeReport = homerJsonOk<HomeReportShape>(B, ['home', origin, '--yes', '--json']);

  // ②的核心证据：home 之后**配置**已全部归位（密钥通道见下，新设备需「换设备两步」）。
  // 必须在其它命令跑之前快照 —— 见文件头「为什么 home 之后还要两步」。
  homePlacedTree = [piRoot(B), herdrRoot(B), opencodeRoot(B)].flatMap((root) =>
    listFiles(root).map((rel) => `${root}:${rel}`),
  );

  // 换设备两步（DESIGN §2.6）：B 出公钥 → A 登记并重加密 push → B pull 归位。
  const keygenB = homerJsonOk<KeygenShape>(B, ['secret', 'keygen', '--json']);
  appendRecipient(A, keygenB.recipient);
  const secretPushB = homerJsonOk<SecretPushShape>(A, ['secret', 'push', '--yes', '--json']);
  const secretPullB = homerJsonOk<SecretPullShape>(B, ['secret', 'pull', '--yes', '--json']);

  const doctorA = homerJsonOk<DoctorReportShape>(A, ['doctor', '--json']);
  const doctorB = homerJsonOk<DoctorReportShape>(B, ['doctor', '--json']);
  const statusB = homerJsonOk<StatusReportShape>(B, ['status', '--json']);

  world = {
    tmp,
    origin,
    A,
    B,
    configA,
    initReport,
    keygenA,
    secretPushA,
    pushA,
    homeReport,
    keygenB,
    secretPushB,
    secretPullB,
    doctorA,
    doctorB,
    statusB,
  };
}, 900_000);
/* ================================================================== */
/* ① 机器 A 装配                                                       */
/* ================================================================== */

describe('M3 e2e ① 机器 A 装配：init 三 adapter → keygen → secret push → push → origin 齐备', () => {
  it('init --json 注册并扫描 pi + herdr + opencode 三个 adapter（⑤ 前：allowEscape 尚未配置）', () => {
    expect(world.initReport.homerHome).toBe(world.A.home);
    expect(world.initReport.adapters).toEqual([
      {
        id: 'pi',
        categories: [
          { name: 'settings', fileCount: 1 },
          { name: 'skills', fileCount: 2 }, // 逃逸链接被跳过 → 只有 alpha/beta
          { name: 'extensions', fileCount: 0 },
          { name: 'agents', fileCount: 0 },
          { name: 'models', fileCount: 1 },
          { name: 'prompts', fileCount: 0 },
          { name: 'themes', fileCount: 0 },
        ],
      },
      { id: 'herdr', categories: [{ name: 'config', fileCount: 1 }] },
      {
        id: 'opencode',
        categories: [
          { name: 'config', fileCount: 1 },
          { name: 'plugins', fileCount: 1 },
          { name: 'locks', fileCount: 1 },
        ],
      },
    ]);
  });

  it('init 报告显式提示 pi 的 symlink 逃逸被跳过（⑤ 的前半：M1 安全边界仍在）', () => {
    expect(world.initReport.errors.some((line) => line.includes('pi') && line.includes('逃逸'))).toBe(true);
  });

  it('homer.json：三 adapter 齐全 + 密钥段（recipients / files），root 保留 ~ 写法', () => {
    expect(Object.keys(world.configA.adapters)).toEqual(['pi', 'herdr', 'opencode']);
    expect(world.configA.adapters['pi']?.root).toBe('~/.pi/agent');
    expect(world.configA.adapters['herdr']?.root).toBe('~/.config/herdr');
    expect(world.configA.adapters['opencode']?.root).toBe('~/.config/opencode');
    expect(world.configA.secrets?.files).toEqual({ [SECRET_NAME]: SECRET_DEST });
    expect(world.configA.secrets?.recipients?.[0]).toBe(world.keygenA.recipient);
    expect(world.keygenA.recipient).toMatch(/^age1[02-9ac-hj-np-z]{58}$/);
  });

  it('secret keygen：私钥落盘 0600、只回显 recipient、keys/ 不入库（.gitignore）', () => {
    const identityFile = path.join(world.A.home, 'keys', 'age.txt');
    expect(modeOf(identityFile)).toBe(0o600);
    expect(readAbs(identityFile)).toContain('AGE-SECRET-KEY-');
    // 报告里只有 recipient（公钥），绝无私钥本体
    expect(JSON.stringify(world.keygenA)).not.toContain('AGE-SECRET-KEY');
    expect(world.keygenA.identityFile).toBe(identityFile);
    expect(world.keygenA.created).toBe(true);
    // keys/ 由 ensureGitRepo 幂等维护进 .gitignore
    expect(readAbs(path.join(world.A.home, '.gitignore'))).toContain('keys/');
  });

  it('secret push：vault 写 secrets/<name>.age，密文不含明文片段，推送远端', () => {
    expect(world.secretPushA.status).toBe('pushed');
    expect(world.secretPushA.encrypted).toEqual([SECRET_NAME]);
    expect(world.secretPushA.pushedToRemote).toBe(true);

    const vault = path.join(world.A.home, 'secrets', `${SECRET_NAME}.age`);
    expect(fs.existsSync(vault)).toBe(true);
    expect(readAbs(vault)).not.toContain(SECRET_FRAGMENT);
    // vault 是密文（非机密）：不执行、不外放可写（≤ 0644）；私钥才是 0600（见 keygen 组）。
    expect(modeOf(vault) & 0o111).toBe(0);
    expect(modeOf(vault)).toBeLessThanOrEqual(0o644);
  });

  it('push：三 adapter 快照入库（store 精确清单）', () => {
    expect(world.pushA.status).toBe('pushed');
    expect(world.pushA.pushedToRemote).toBe(true);

    expect(storeFiles(world.A.home)).toEqual([
      'herdr/config/config.toml',
      'opencode/config/opencode.json',
      'opencode/locks/package-lock.json',
      'opencode/plugins/package.json',
      'pi/models/models.json',
      'pi/settings/settings.json',
      // ⑤ 后半：allowEscape 命中 → 逃逸链接的内容（含子目录）进 store
      'pi/skills/agent-browser/SKILL.md',
      'pi/skills/agent-browser/sub/a.ts',
      'pi/skills/alpha/SKILL.md',
      'pi/skills/beta/SKILL.md',
    ]);

    expect(readAbs(path.join(world.A.home, 'store', 'herdr/config/config.toml'))).toBe(HERDR_CONFIG);
    expect(readAbs(path.join(world.A.home, 'store', 'opencode/config/opencode.json'))).toBe(OPENCODE_CONFIG);
    expect(readAbs(path.join(world.A.home, 'store', 'opencode/plugins/package.json'))).toBe(OPENCODE_PACKAGE);
    expect(readAbs(path.join(world.A.home, 'store', 'pi/skills/agent-browser/sub/a.ts'))).toBe(ESCAPE_SUB);

    // 垃圾一条都不进 store（herdr session/sock/log、opencode node_modules 等）
    const flat = storeFiles(world.A.home).join('\n');
    expect(flat).not.toMatch(/session\.json|\.sock|\.log|release-notes|node_modules|\.plugins\.lock|auth\.json/);
  });

  it('origin 齐备：store/ + secrets/ + homer.json；keys/ 永不入库', () => {
    const tree = git(world.origin, ['ls-tree', '-r', '--name-only', 'HEAD'])
      .split('\n')
      .filter((line) => line !== '');

    for (const expected of [
      'homer.json',
      `secrets/${SECRET_NAME}.age`,
      'store/herdr/config/config.toml',
      'store/opencode/config/opencode.json',
      'store/opencode/locks/package-lock.json',
      'store/opencode/plugins/package.json',
      'store/pi/models/models.json',
      'store/pi/settings/settings.json',
      'store/pi/skills/agent-browser/SKILL.md',
      'store/pi/skills/agent-browser/sub/a.ts',
      'store/pi/skills/alpha/SKILL.md',
      'store/pi/skills/beta/SKILL.md',
    ]) {
      expect(tree, `${expected} 应在 origin`).toContain(expected);
    }
    expect(tree.some((rel) => rel.startsWith('keys/'))).toBe(false);
    expect(sha(world.origin)).toBe(sha(world.A.home));
  });

  it('bare origin 铁证：git grep 无明文片段、无私钥字符串', () => {
    expect(gitTry(world.origin, ['grep', '-h', '-e', SECRET_FRAGMENT, 'HEAD']).out).not.toContain(SECRET_FRAGMENT);
    expect(gitTry(world.origin, ['grep', '-h', '-e', 'AGE-SECRET-KEY', 'HEAD']).out).not.toContain('AGE-SECRET-KEY');
  });

  it('A 侧 doctor：无 fail，age = ok（identity 就绪 + 密文可解）', () => {
    expect(world.doctorA.ok).toBe(true);
    expect(world.doctorA.checks.filter((check) => check.status === 'fail')).toEqual([]);
    expect(world.doctorA.checks.find((check) => check.id === 'age')?.status).toBe('ok');
  });

  it('A 侧 status：零漂移、零告警（allowEscape 生效后逃逸不再是告警）', () => {
    const status = homerJsonOk<StatusReportShape>(world.A, ['status', '--json']);
    expect(status.errors).toEqual([]);
    expect(status.adapters.map((adapter) => adapter.id).sort()).toEqual(['herdr', 'opencode', 'pi']);
    for (const adapter of status.adapters) {
      expect(adapter, JSON.stringify(adapter)).toMatchObject({ push: 0, pull: 0, conflicts: 0 });
    }
  });
});

/* ================================================================== */
/* ② 机器 B 一键归位（核心场景）                                        */
/* ================================================================== */

describe('M3 e2e ② 机器 B 一键归位：homer home --yes → 配置 + 密钥归位 + doctor + status', () => {
  it('home --yes：clone 成功、三 adapter、首次对接 = merge、零错误/零冲突', () => {
    expect(world.homeReport.status).toBe('homed');
    expect(world.homeReport.ok).toBe(true);
    expect(world.homeReport.cloned).toBe(true);
    expect(world.homeReport.adapterIds).toEqual(['pi', 'herdr', 'opencode']);
    expect(world.homeReport.firstContact?.mode).toBe('merge');
    expect(world.homeReport.errors).toEqual([]);
    expect(world.homeReport.firstContact?.applied.conflicts).toEqual([]);
    expect(world.homeReport.firstContact?.applied.deleted).toEqual([]);
    // B 侧工具目录原本为空 → 远端每个文件都被写下来
    expect(world.homeReport.firstContact?.applied.written.length).toBe(storeFiles(world.A.home).length);
  });

  it('home 报告如实说明「新设备尚无 identity」→ 密钥 skipped + 换设备两步提示（不 fail 整个 home）', () => {
    expect(world.homeReport.secrets.skipped).toEqual([SECRET_NAME]);
    expect(world.homeReport.secrets.pulled).toEqual([]);
    expect(world.homeReport.warnings.join('\n')).toContain('identity');
    expect(world.homeReport.warnings.join('\n')).toContain('recipient');
  });

  it('三工具目录逐字节 == A 侧 store（含 herdr/opencode；excludeKeys 文件见下一条）', () => {
    const entries = storeEntries(world.B, world.configA);
    expect(entries.length).toBeGreaterThanOrEqual(10);

    for (const entry of entries) {
      const key = `${entry.adapterId}/${entry.category}/${entry.relPath}`;
      if (categoryExcludeKeys(world.configA, entry.adapterId, entry.category).length > 0) continue;
      expect(fs.existsSync(entry.toolAbsPath), `${key} 应被归位`).toBe(true);
      expect(readAbs(entry.toolAbsPath), key).toBe(entry.storeContent);
    }

    // 工具目录里没有多余文件（store 是唯一来源；B 的三工具 root 起手为空）
    const extras = listFiles(piRoot(world.B))
      .map((rel) => `pi:${rel}`)
      .concat(listFiles(herdrRoot(world.B)).map((rel) => `herdr:${rel}`))
      .concat(listFiles(opencodeRoot(world.B)).map((rel) => `opencode:${rel}`))
      .sort();
    expect(extras).toEqual(toolLabels(world.B, world.configA));
  });

  it('home 刚结束时（其它命令未再写入前）：三工具目录的落点清单 == store 反算清单', () => {
    const expected = [
      ...toolLabels(world.B, world.configA).map((label) => {
        const [adapterId, ...rest] = label.split(':');
        const root = expandTilde(world.configA.adapters[adapterId as string]!.root, world.B.fakeHome);
        return `${root}:${rest.join(':')}`;
      }),
    ].sort();
    expect(homePlacedTree.slice().sort()).toEqual(expected);
  });

  it('excludeKeys 不变量：store 有 `__REQUIRED__`，工具目录绝无占位符（跨机保持）', () => {
    const storeModels = readAbs(path.join(world.B.home, 'store', 'pi/models/models.json'));
    const toolModels = readAbs(path.join(piRoot(world.B), 'models.json'));

    expect(storeModels).toContain('"apiKeys": "__REQUIRED__"');
    expect(toolModels).not.toContain('__REQUIRED__');
    expect(JSON.parse(toolModels)).toMatchObject({ models: { openai: { id: 'gpt' } } });
  });

  it('密钥归位（换设备两步后）：明文 == A 明文、0600、报告只含 secret 名', () => {
    expect(world.secretPushB.status).toBe('pushed');
    expect(world.secretPushB.pushedToRemote).toBe(true);
    expect(world.secretPullB.status).toBe('applied');
    expect(world.secretPullB.pulled).toEqual([SECRET_NAME]);

    const destination = path.join(world.B.fakeHome, '.secrets', 'a.env');
    expect(readAbs(destination)).toBe(SECRET_PLAINTEXT);
    expect(modeOf(destination)).toBe(0o600);
    // 报告里绝无明文
    expect(JSON.stringify(world.secretPullB)).not.toContain(SECRET_FRAGMENT);
  });

  it('B 侧 doctor --json：无 fail（age / machine / adapters 均 ok），required 只 warn', () => {
    expect(world.doctorB.ok).toBe(true);
    expect(world.doctorB.checks.filter((check) => check.status === 'fail')).toEqual([]);
    expect(world.doctorB.checks.find((check) => check.id === 'age')?.status).toBe('ok');
    expect(world.doctorB.checks.find((check) => check.id === 'machine')?.status).toBe('ok');
    expect(world.doctorB.checks.find((check) => check.id === 'adapters')?.status).toBe('ok');
  });

  it('B 侧 status：零漂移、零告警，state.lastSyncCommit == HEAD', () => {
    for (const adapter of world.statusB.adapters) {
      expect(adapter, JSON.stringify(adapter)).toMatchObject({ push: 0, pull: 0, conflicts: 0 });
    }
    expect(world.statusB.errors).toEqual([]);
    expect(world.statusB.warnings ?? []).toEqual([]);
    expect(readState(world.B.home)['lastSyncCommit']).toBe(sha(world.B.home));
    // 文本渲染也确认「无漂移」
    expect(homer(world.B, ['status']).out).toContain('无漂移');
  });
});

/* ================================================================== */
/* ③ 换设备 = 加 recipient + 重加密                                     */
/* ================================================================== */

describe('M3 e2e ③ 换设备：C keygen → A 加 recipient 重加密 push → C pull 成功（旧设备仍可解）', () => {
  it(
    '重加密后 C（新）/ B（旧）/ A（原）三方都能 secret pull，密文确实变化且只含密文',
    () => {
      const vault = path.join(world.A.home, 'secrets', `${SECRET_NAME}.age`);
      const vaultBefore = fs.readFileSync(vault);
      const headBefore = sha(world.A.home);

      // 新机器 C：一键归位（配置）+ 生成本机 identity
      const C = makeMachine(world.tmp, 'C', { fixtures: false });
      const homeC = homerJsonOk<HomeReportShape>(C, ['home', world.origin, '--yes', '--json']);
      expect(homeC.status).toBe('homed');
      const keygenC = homerJsonOk<KeygenShape>(C, ['secret', 'keygen', '--json']);

      // A：登记 C 的 recipient → 重加密 push（多 recipient）
      appendRecipient(world.A, keygenC.recipient);
      const repush = homerJsonOk<SecretPushShape>(world.A, ['secret', 'push', '--yes', '--json']);
      expect(repush.status).toBe('pushed');
      expect(repush.pushedToRemote).toBe(true);

      // 密文变了（重加密）+ 产生了新 commit + origin 跟上
      expect(fs.readFileSync(vault).equals(vaultBefore)).toBe(false);
      expect(sha(world.A.home)).not.toBe(headBefore);
      expect(sha(world.origin)).toBe(sha(world.A.home));

      // C（新设备）拉取成功：自己的 identity 是 recipient 之一
      const pullC = homerJsonOk<SecretPullShape>(C, ['secret', 'pull', '--yes', '--json']);
      expect(pullC.status).toBe('applied');
      expect(pullC.pulled).toEqual([SECRET_NAME]);
      const destinationC = path.join(C.fakeHome, '.secrets', 'a.env');
      expect(readAbs(destinationC)).toBe(SECRET_PLAINTEXT);
      expect(modeOf(destinationC)).toBe(0o600);

      // B（② 的旧设备）仍能解密（多 recipient 不破坏既有设备）
      const pullB = homerJsonOk<SecretPullShape>(world.B, ['secret', 'pull', '--yes', '--json']);
      expect(pullB.status).toBe('applied');
      expect(readAbs(path.join(world.B.fakeHome, '.secrets', 'a.env'))).toBe(SECRET_PLAINTEXT);

      // A 自己（最初设备）同样仍可解密
      expect(homerJsonOk<SecretPullShape>(world.A, ['secret', 'pull', '--yes', '--json']).status).toBe('applied');
      expect(readAbs(path.join(world.A.fakeHome, '.secrets', 'a.env'))).toBe(SECRET_PLAINTEXT);

      // secret list：一密钥一文件，vault present
      const list = homerJsonOk<{ secrets: { name: string; destination: string; vaultFile: string }[] }>(world.A, [
        'secret',
        'list',
        '--json',
      ]);
      expect(list.secrets).toEqual([{ name: SECRET_NAME, destination: SECRET_DEST, vaultFile: 'present' }]);

      // 轮转后 origin 依旧无私钥 / 无明文
      expect(gitTry(world.origin, ['grep', '-h', '-e', SECRET_FRAGMENT, 'HEAD']).out).not.toContain(SECRET_FRAGMENT);
      expect(gitTry(world.origin, ['grep', '-h', '-e', 'AGE-SECRET-KEY', 'HEAD']).out).not.toContain('AGE-SECRET-KEY');
    },
    300_000,
  );
});

/* ================================================================== */
/* ④ `__REQUIRED__` 残留进 doctor（M2 遗留关闭）                        */
/* ================================================================== */

describe('M3 e2e ④ excludeKeys 占位符残留：doctor 的 required 报 warn 且 details 精确', () => {
  it('push 侧置换真的发生：store 的 models.json 里 apiKeys = __REQUIRED__', () => {
    expect(world.pushA.status).toBe('pushed');
    expect(readAbs(path.join(world.A.home, 'store', 'pi/models/models.json'))).toContain('"apiKeys": "__REQUIRED__"');
  });

  it('A 侧 doctor：required = warn，details 精确到 `pi/models/models.json: apiKeys`，且不影响 ok / 退出码', () => {
    const required = world.doctorA.checks.find((check) => check.id === 'required');
    expect(required?.status).toBe('warn');
    expect(required?.message).toContain('__REQUIRED__');
    expect(required?.details).toEqual(['pi/models/models.json: apiKeys']);
    expect(world.doctorA.ok).toBe(true);
    expect(homer(world.A, ['doctor']).code).toBe(0);
  });

  it('B 侧 doctor 同样报 required warn（占位符随仓库跨机可见）', () => {
    expect(world.doctorB.checks.find((check) => check.id === 'required')?.status).toBe('warn');
    const result = homer(world.B, ['doctor', '--json']);
    expect(result.code).toBe(0);
    const parsed = JSON.parse(result.out) as DoctorReportShape;
    expect(parsed.checks.find((check) => check.id === 'required')?.details).toEqual(['pi/models/models.json: apiKeys']);
  });
});

/* ================================================================== */
/* ⑤ symlink 逃逸 allowlist（M1 已知限制关闭）                          */
/* ================================================================== */

describe('M3 e2e ⑤ symlink 逃逸 allowlist：命中后 root 外内容入 store 且无告警', () => {
  it('无 allowEscape 时 init 跳过 + 记 ScanError（M1 安全边界默认仍生效）', () => {
    expect(world.initReport.adapters[0]?.categories.find((c) => c.name === 'skills')?.fileCount).toBe(2);
    expect(world.initReport.errors.some((line) => line.includes('逃逸'))).toBe(true);
  });

  it('allowEscape 命中后：root 外内容（含子目录）进 store，且 push 报告零告警', () => {
    expect(readAbs(path.join(world.A.home, 'store', 'pi/skills/agent-browser/SKILL.md'))).toBe(ESCAPE_SKILL);
    expect(readAbs(path.join(world.A.home, 'store', 'pi/skills/agent-browser/sub/a.ts'))).toBe(ESCAPE_SUB);
    expect(world.pushA.status).toBe('pushed');
  });

  it('A 侧 status：allowEscape 生效 → 逃逸不再是告警（errors 空，零漂移）', () => {
    const status = homerJsonOk<StatusReportShape>(world.A, ['status', '--json']);
    expect(status.errors).toEqual([]);
    const skills = status.adapters.find((adapter) => adapter.id === 'pi');
    expect(skills).toMatchObject({ push: 0, pull: 0, conflicts: 0 });
  });

  it('allowlist 只放行命中的那一条：未命中路径仍按逃逸跳过（用真实扫描复核契约）', () => {
    // 临时机器：两个逃逸 symlink（allowed / blocked），allowEscape 只写其中一个
    const D = makeMachine(world.tmp, 'D-allowlist', { fixtures: false });
    const pi = piRoot(D);
    writeAbs(path.join(pi, 'settings.json'), PI_SETTINGS);
    fs.mkdirSync(path.join(pi, 'skills'), { recursive: true });
    for (const name of ['allowed', 'blocked']) {
      const outside = path.join(path.dirname(D.fakeHome), 'outside', name);
      writeAbs(path.join(outside, 'SKILL.md'), `# ${name}\n`);
      fs.symlinkSync(outside, path.join(pi, 'skills', name));
    }

    const home = path.join(D.home, 'allowlist-home');
    expect(homer(D, ['init', '--json', '--home', home]).code).toBe(0);

    // 把 allowEscape 收紧到只放行 skills/allowed，并用 CLI 复核真实扫描结果
    const configFile = path.join(home, 'homer.json');
    const config = JSON.parse(readAbs(configFile)) as HomerConfig;
    config.adapters['pi']!.allowEscape = ['skills/allowed'];
    fs.writeFileSync(configFile, `${JSON.stringify(config, null, 2)}\n`, 'utf8');

    const status = homer(D, ['status', '--home', home, '--json']);
    expect(status.code).toBe(0);
    const parsed = JSON.parse(status.out) as StatusReportShape;
    // blocked 仍被跳过 → 产生一条逃逸告警；allowed 已放行 → 不再告警
    expect(parsed.errors.some((line) => line.includes('blocked'))).toBe(true);
    expect(parsed.errors.some((line) => line.includes('allowed'))).toBe(false);
    // allowed 的内容进入判定（tool 目录 vs store：store 里还没有 → 计为 push 1）
    expect(parsed.adapters.find((adapter) => adapter.id === 'pi')?.push).toBe(1);
  });
});

/* ================================================================== */
/* ⑥ install.sh 冒烟（引用 W4 用例）                                    */
/* ================================================================== */

describe('M3 e2e ⑥ install.sh 冒烟：脚本契约 + W4 安装用例仍在（引用不重复执行）', () => {
  it('install.sh 存在、POSIX 语法通过（sh -n）、带可测性注入位', () => {
    const script = path.join(REPO_ROOT, 'install.sh');
    expect(fs.existsSync(script)).toBe(true);
    execFileSync('/bin/sh', ['-n', script], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });

    const src = readAbs(script);
    expect(src.startsWith('#!/bin/sh')).toBe(true);
    expect(src).toContain('HOMER_INSTALL_PACKAGE');
    expect(src).toContain('HOMER_INSTALL_PREFIX');
    // 下一步指引必须指向 MVP 主命令
    expect(src).toContain('homer home');
  });

  it('W4 的安装 e2e 用例仍在（npm pack → 隔离 prefix → homer --help 由该文件负责真实执行）', () => {
    const w4 = readAbs(path.join(REPO_ROOT, 'tests/e2e/install.test.ts'));
    // 三件关键证据：语法用例、tarball 安装用例、PATH 无 node 用例
    expect(w4).toContain('sh -n install.sh');
    expect(w4).toContain('npm pack');
    expect(w4).toContain('--help');
    expect(w4).toContain('NPM_CONFIG_PREFIX');
    // 隔离保证：绝不碰真实全局 prefix
    expect(w4).toContain('prefix');
  });

  it('CLI 自身可用（子进程 --help 退出 0 且列出 M3 三命令）', () => {
    const anyMachine = makeMachine(world.tmp, 'E-help', { fixtures: false });
    const result = homer(anyMachine, ['--help']);
    expect(result.code).toBe(0);
    for (const command of ['homer home', 'homer doctor', 'homer secret']) {
      expect(result.out).toContain(command);
    }
  });
});

/* ================================================================== */
/* ⑦ 真实环境只读冒烟（可选）                                           */
/* ================================================================== */

describe('M3 e2e ⑦ 真实环境只读冒烟：真实 HOME + 临时 HOMER_HOME（只读，不写工具目录）', () => {
  /**
   * 跑 CLI 子进程并返回 stdout（env 已剔除 VITEST，使入口副作用生效）。
   *
   * `homedir()` 只读 `process.env.HOME`，故可在**当前进程**里把 HOME 指向临时目录来构造
   * 「无真实工具安装」的隔离场景（不依赖子进程 env 传递）。
   *
   * `tolerateNonZeroExit`：doctor 在「非 git 仓库」等会 fail 的项上会以退出码 1 结束
   * （判定是有意的，`--json` 报告仍完整）——此处要的是报告而非退出码。
   */
  function runCli(args: readonly string[], env: NodeJS.ProcessEnv = {}, tolerateNonZeroExit = false): string {
    const childEnv: NodeJS.ProcessEnv = { ...process.env, ...env };
    delete childEnv['VITEST'];
    try {
      return execFileSync(process.execPath, ['--import', 'tsx', 'src/cli/index.ts', ...args], {
        cwd: REPO_ROOT,
        env: childEnv,
        encoding: 'utf8',
        stdio: ['ignore', 'pipe', 'pipe'],
      });
    } catch (error) {
      const failure = error as { status?: number | null; stdout?: string | Buffer };
      if (tolerateNonZeroExit && typeof failure.status === 'number' && failure.status === 1) {
        return failure.stdout?.toString() ?? '';
      }
      throw error;
    }
  }

  it('init --json 扫出 pi + herdr + opencode 三 adapter（真实工具安装即真扫）', () => {
    const realHome = os.homedir();
    const present = [
      ['pi', path.join(realHome, '.pi', 'agent')],
      ['herdr', path.join(realHome, '.config', 'herdr')],
      ['opencode', path.join(realHome, '.config', 'opencode')],
    ].filter(([, dir]) => fs.existsSync(dir));

    if (present.length === 0) {
      // 无真实工具安装（CI / 容器）→ 降级为「隔离假 HOME 下三 adapter 全注册 + 形状正确」
      const tmp = mkTmp('real-empty');
      const fakeHome = path.join(tmp, 'home');
      fs.mkdirSync(fakeHome, { recursive: true });
      const report = JSON.parse(
        runCli(['init', '--json'], { HOME: fakeHome, HOMER_HOME: path.join(tmp, 'homer') }),
      ) as InitReportShape;
      expect(report.adapters.map((adapter) => adapter.id)).toEqual(['pi', 'herdr', 'opencode']);
      return;
    }

    // 真实 HOME 只读扫描：全部写入只落临时 HOMER_HOME
    const tmp = mkTmp('real');
    const homerHome = path.join(tmp, 'homer');
    const report = JSON.parse(runCli(['init', '--json'], { HOMER_HOME: homerHome })) as InitReportShape;
    expect(report.homerHome).toBe(homerHome);
    expect(report.adapters.map((adapter) => adapter.id)).toEqual(['pi', 'herdr', 'opencode']);

    // 真实存在的工具 root 必须真被扫到（分类非空 = 进走了该 adapter 的声明分类）
    for (const [id] of present) {
      const entry = report.adapters.find((adapter) => adapter.id === id);
      expect(entry, `${id} 应在报告里`).toBeDefined();
      expect(entry?.categories.length, `${id} 应有分类`).toBeGreaterThan(0);
    }
    // 未安装的 adapter root 必须报 root 不可读（不静默当成「本地全空」）
    for (const [id, dir] of [
      ['pi', path.join(realHome, '.pi', 'agent')],
      ['herdr', path.join(realHome, '.config', 'herdr')],
      ['opencode', path.join(realHome, '.config', 'opencode')],
    ] as [string, string][]) {
      if (fs.existsSync(dir)) continue;
      expect(report.errors.some((line) => line.includes(id))).toBe(true);
    }

    // 真实工具目录零写入：临时 HOMER_HOME 之外什么都没创建
    expect(fs.existsSync(path.join(realHome, '.homer'))).toBe(false);
  });

  it('doctor --offline：临时 HOMER_HOME（真实 HOME）→ age 未配置 = ok，remote 检查跳过', () => {
    const tmp = mkTmp('real-doctor');
    const homerHome = path.join(tmp, 'homer');
    // 先 init（写入临时 HOMER_HOME，真实工具目录只读）→ doctor 能看到合法 config 与三 adapter
    runCli(['init', '--json'], { HOMER_HOME: homerHome });

    const doctor = JSON.parse(
      runCli(['doctor', '--offline', '--json'], { HOMER_HOME: homerHome }, true),
    ) as DoctorReportShape;

    // 八项齐全且顺序固定
    expect(doctor.checks.map((check) => check.id)).toEqual([
      'config', 'repo', 'store-clean', 'remote', 'adapters', 'age', 'machine', 'required',
    ]);
    // ⑦ 的核心断言：未配置密钥同步 → age = ok（不要求 identity）
    expect(doctor.checks.find((check) => check.id === 'age')).toMatchObject({ status: 'ok' });
    expect(doctor.checks.find((check) => check.id === 'age')?.message).toContain('未配置密钥同步');
    // --offline → remote 跳过（W7 冻结措辞）
    expect(doctor.checks.find((check) => check.id === 'remote')?.status).toBe('ok');
    expect(doctor.checks.find((check) => check.id === 'remote')?.message).toContain('已跳过（--offline）');
    // 真实环境里 config / adapters 可用（init 刚写过）
    expect(doctor.checks.find((check) => check.id === 'config')?.status).toBe('ok');
    // 唯一可能的 fail 是「临时 HOMER_HOME 不是 git 仓库」（doctor 只读、不建仓库）
    const fails = doctor.checks.filter((check) => check.status === 'fail').map((check) => check.id);
    expect(fails.every((id) => id === 'repo')).toBe(true);
  });
});
