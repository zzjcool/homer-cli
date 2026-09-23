/**
 * P2-W6 · `src/cli/commands/secret.ts` 验收（docs/m3-plan.md §2.6 / §1-D2/D3/D4 / §3-P2-W6）。
 *
 * ## 隔离（硬约束）
 *
 * 每个用例 `mkdtemp` 造临时根目录：假 HOME（`HOMER_HOME`）+ `git init --bare` 假 origin。
 * **绝不碰真实 `~/.homer` / `~/.ssh` / 真实 remote**；identity 一律 `generateIdentity()`
 * 临时生成（不读用户真实 `keys/age.txt`）。
 *
 * ## 证据线
 *
 * 1. **keygen**：文件 0600、输出/`--json` 只含 recipient、重复 keygen → exit 1 不覆盖；
 * 2. **push**：多 recipient 密文（origin 侧密文可被两个 identity 各自解开）、
 *    **bare origin `git grep <明文片段>` 为空**（密文入库铁证）、missing-source 全有或全无、
 *    no-identity / no-recipients exit 1、`--no-push`（origin 不动）、
 *    commit 只落 `secrets/` 而 `store/` 的脏改动一个字节都不进这次 commit（D4 零耦合）；
 * 3. **pull**：从 `@{upstream}` 读密文（origin 领先本地工作区场景 + **不 ff 整仓**）、
 *    目标 0600 + 备份内容 == 覆盖前、undecryptable（非 recipient identity）exit 1 且零写入、
 *    fetch 失败回落工作区；
 * 4. **list**：present/missing + `--json` 形状。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { run, type CliIO } from '../../src/cli/index.js';
import {
  renderSecretListReport,
  renderSecretPullReport,
  renderSecretPushReport,
  runSecretKeygen,
  runSecretList,
  runSecretPull,
  runSecretPush,
} from '../../src/cli/commands/secret.js';
import type { PromptPort } from '../../src/cli/ui.js';
import { createAgeCryptoPort } from '../../src/core/age/cipher.js';
import { generateIdentity, identityFilePath, writeIdentityFile } from '../../src/core/age/keys.js';
import type { AgeIdentity } from '../../src/core/age/types.js';
import { secretFilePath } from '../../src/core/age/vault.js';
import { CliError } from '../../src/core/errors.js';
import { gitExec, headCommit, readVaultFileAtCommit } from '../../src/core/git/index.js';
import { getHomerPaths, type HomerPaths } from '../../src/core/paths.js';
import type { SecretsConfig } from '../../src/core/types.js';

/* ------------------------------------------------------------------ */
/* 隔离工具                                                            */
/* ------------------------------------------------------------------ */

const created: string[] = [];

function mkTmp(prefix: string): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), `homer-w6-${prefix}-`));
  created.push(dir);
  return dir;
}

afterEach(() => {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) fs.rmSync(dir, { recursive: true, force: true });
  }
});

/** git 命令成功断言（失败即抛，便于用例里直读 stdout）。 */
function gitOk(cwd: string, args: readonly string[]): string {
  const result = gitExec(cwd, args);
  if (!result.ok) throw new Error(`git ${args.join(' ')} 失败 (cwd=${cwd}): ${result.stderr}`);
  return result.stdout;
}

interface Env {
  root: string;
  origin: string;
  home: string;
  paths: HomerPaths;
}

/** 假 origin（bare）+ 本地工作区（`git init -b main` + remote origin）。 */
function makeEnv(prefix: string): Env {
  const root = mkTmp(prefix);
  const origin = path.join(root, 'origin.git');
  gitOk(root, ['init', '--bare', '-b', 'main', origin]);

  const home = path.join(root, 'home');
  fs.mkdirSync(home, { recursive: true });
  gitOk(home, ['init', '-b', 'main']);
  gitOk(home, ['config', 'user.email', 'homer-w6@example.invalid']);
  gitOk(home, ['config', 'user.name', 'Homer W6']);
  gitOk(home, ['remote', 'add', 'origin', origin]);

  return { root, origin, home, paths: getHomerPaths({ HOMER_HOME: home }) };
}

function writeHomerConfig(home: string, secrets: SecretsConfig): void {
  const config = { version: 1, adapters: {}, secrets };
  fs.mkdirSync(home, { recursive: true });
  fs.writeFileSync(path.join(home, 'homer.json'), `${JSON.stringify(config, null, 2)}\n`, 'utf8');
}

/** 写一个明文文件（返回绝对路径）。 */
function writePlaintext(file: string, content: string): string {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, content, 'utf8');
  return file;
}

function mode(abs: string): number {
  return fs.statSync(abs).mode & 0o777;
}

function capture(): { io: CliIO; out: string[]; err: string[] } {
  const out: string[] = [];
  const err: string[] = [];
  return { io: { out: (line) => out.push(line), err: (line) => err.push(line) }, out, err };
}

/** 记录 confirm 调用的假 port（默认同意）。 */
function fakeUi(approve = true): PromptPort & { confirms: string[] } {
  const confirms: string[] = [];
  return {
    confirms,
    async confirm(message: string): Promise<boolean> {
      confirms.push(message);
      return approve;
    },
    async select(): Promise<never> {
      throw new Error('secret 不使用 select');
    },
  } as PromptPort & { confirms: string[] };
}

const crypto = createAgeCryptoPort();

/** 明文里的独特片段（用于 `git grep` 铁证；足够长以触发 vault 的密文自检）。 */
const FRAGMENT = 'HOMER-W6-PLAINTEXT-FRAGMENT-9f3a2b7c';
const PLAINTEXT = `API_TOKEN=${FRAGMENT}\nline-2: 不短于 17 字节的正文\nline-3: end\n`;

/** 提交初始 store 基线并推送（`-u` 建立 upstream）。 */
function commitBaseline(env: Env, marker: string): string {
  writePlaintext(path.join(env.home, 'store', 'pi', 'settings', 'settings.json'), `{"marker":"${marker}"}\n`);
  gitOk(env.home, ['add', '-A', '--', 'store/']);
  gitOk(env.home, ['commit', '-m', 'store baseline', '--', 'store/']);
  gitOk(env.home, ['push', '-u', 'origin', 'main']);
  return headCommit(env.home)!;
}

/* ------------------------------------------------------------------ */
/* keygen                                                              */
/* ------------------------------------------------------------------ */

describe('W6 · secret keygen', () => {
  it('生成 keys/age.txt（0600），报告只含 recipient（无私钥）', async () => {
    const env = makeEnv('keygen');
    const report = await runSecretKeygen({ homerHome: env.home });

    expect(report.ok).toBe(true);
    expect(report.created).toBe(true);
    expect(report.identityFile).toBe(identityFilePath(env.paths));

    expect(fs.existsSync(report.identityFile)).toBe(true);
    expect(mode(report.identityFile)).toBe(0o600);

    expect(report.recipient).toMatch(/^age1[02-9ac-hj-np-z]{58}$/);
    // 报告的字段集合固定（只有公钥面）。
    expect(Object.keys(report).sort()).toEqual(['created', 'identityFile', 'ok', 'recipient']);
  });

  it('`--json` 输出可 parse 且只含 recipient（无 AGE-SECRET-KEY 泄漏）', async () => {
    const env = makeEnv('keygen-json');
    const cap = capture();
    const code = await run(['secret', 'keygen', '--home', env.home, '--json'], cap.io);

    expect(code).toBe(0);
    const raw = cap.out.join('\n');
    expect(raw).not.toContain('AGE-SECRET-KEY');

    const parsed = JSON.parse(raw) as Record<string, unknown>;
    expect(Object.keys(parsed).sort()).toEqual(['created', 'identityFile', 'ok', 'recipient']);
    expect(parsed['ok']).toBe(true);
    expect(parsed['created']).toBe(true);

    // 文件里的私钥确实存在，但报告从未回显它。
    const fileContent = fs.readFileSync(identityFilePath(env.paths), 'utf8').trim();
    expect(fileContent).toMatch(/^AGE-SECRET-KEY-1/);
    expect(raw).not.toContain(fileContent);
  });

  it('重复 keygen → CliError + exit 1，且原私钥文件字节级不变（绝不覆盖）', async () => {
    const env = makeEnv('keygen-dup');
    await runSecretKeygen({ homerHome: env.home });
    const file = identityFilePath(env.paths);
    const before = fs.readFileSync(file);

    await expect(runSecretKeygen({ homerHome: env.home })).rejects.toThrow(CliError);
    expect(fs.readFileSync(file).equals(before)).toBe(true);

    const cap = capture();
    expect(await run(['secret', 'keygen', '--home', env.home], cap.io)).toBe(1);
    expect(cap.err.join('\n')).toContain('拒绝覆盖');
    expect(cap.err.join('\n')).not.toContain('AGE-SECRET-KEY');
  });

  it('文本输出含路径与 recipient（不打印私钥）', async () => {
    const env = makeEnv('keygen-text');
    const cap = capture();
    expect(await run(['secret', 'keygen', '--home', env.home], cap.io)).toBe(0);
    const text = cap.out.join('\n');
    expect(text).toContain('已生成');
    expect(text).toContain('recipient:');
    expect(text).not.toContain('AGE-SECRET-KEY');
  });
});

/* ------------------------------------------------------------------ */
/* push                                                                */
/* ------------------------------------------------------------------ */

/** 常规 push 环境：identity（A）+ 可选第二 identity（B）+ 两个 secret。 */
function pushEnv(prefix: string, opts: { secondRecipient?: boolean } = {}): {
  env: Env;
  identityA: AgeIdentity;
  identityB: AgeIdentity;
  destA: string;
  destB: string;
} {
  const env = makeEnv(prefix);
  commitBaseline(env, 'baseline');

  const identityA = generateIdentity();
  writeIdentityFile(env.paths, identityA);
  const identityB = generateIdentity();

  const destA = path.join(env.root, 'targets', 'a.env');
  const destB = path.join(env.root, 'targets', 'b.env');
  writePlaintext(destA, PLAINTEXT);
  writePlaintext(destB, `${PLAINTEXT}extra: second-secret-body\n`);

  writeHomerConfig(env.home, {
    recipients: opts.secondRecipient === true ? [identityA.recipient, identityB.recipient] : [identityA.recipient],
    files: { 'a-secret': destA, 'b-secret': destB },
  });

  return { env, identityA, identityB, destA, destB };
}

describe('W6 · secret push', () => {
  it('多 recipient：origin 侧密文可被两个 identity 各自解开，且 commit 只含 secrets/', async () => {
    const { env, identityA, identityB } = pushEnv('push-multi', { secondRecipient: true });

    const report = await runSecretPush({ homerHome: env.home, yes: true }, { age: crypto });
    expect(report.status).toBe('pushed');
    expect(report.ok).toBe(true);
    expect(report.encrypted).toEqual(['a-secret', 'b-secret']);
    expect(report.commit).toBeDefined();
    expect(report.pushedToRemote).toBe(true);

    // commit 只含 secrets/（store/ 的基线 commit 不在本次）。
    const files = gitOk(env.home, ['show', '--name-only', '--format=', 'HEAD']).trim().split('\n').sort();
    expect(files).toEqual(['secrets/a-secret.age', 'secrets/b-secret.age']);

    // 从 **bare origin** 读密文（越过工作区，证明推的是密文本身）。
    for (const name of ['a-secret', 'b-secret']) {
      const blob = readVaultFileAtCommit(env.origin, `secrets/${name}.age`, 'HEAD');
      expect(blob).toBeInstanceOf(Buffer);
      expect(blob!.length).toBeGreaterThan(0);

      const expected = fs.readFileSync(path.join(env.root, 'targets', `${name === 'a-secret' ? 'a' : 'b'}.env`));
      expect((await crypto.decrypt(blob!, identityA)).equals(expected)).toBe(true);
      expect((await crypto.decrypt(blob!, identityB)).equals(expected)).toBe(true);
    }
  });

  it('密文入库铁证：bare origin `git grep <明文片段>` 为空（正控制非空）', async () => {
    const { env } = pushEnv('push-grep');
    const pushed = await runSecretPush({ homerHome: env.home, yes: true }, { age: crypto });
    expect(pushed.status).toBe('pushed');

    // 正控制：仓库里确实有可 grep 的内容（store 基线的 marker）→ grep 机制本身有效。
    const control = gitExec(env.origin, ['grep', 'baseline', 'HEAD']);
    expect(control.ok).toBe(true);
    expect(control.stdout).toContain('baseline');

    // 铁证：明文片段在 origin 的任何 blob 里都搜不到。
    const plaintext = gitExec(env.origin, ['grep', FRAGMENT, 'HEAD']);
    expect(plaintext.stdout).toBe('');
    expect(plaintext.ok).toBe(false);

    // 私钥同样从未入库（pattern 13 的后置证据：origin 里没有 AGE-SECRET-KEY）。
    const key = gitExec(env.origin, ['grep', '-I', 'AGE-SECRET-KEY', 'HEAD']);
    expect(key.stdout).toBe('');
    expect(key.ok).toBe(false);

    // 但同时密文文件确实在 origin（否则上面为空是「什么都没推」的假阳性）。
    expect(gitOk(env.origin, ['ls-tree', '-r', '--name-only', 'HEAD', '--', 'secrets/']).trim().split('\n')).toEqual([
      'secrets/a-secret.age',
      'secrets/b-secret.age',
    ]);
  });

  it('push 的 ensureGitRepo 幂等补上 `keys/` gitignore（私钥目录结构性不入库）', async () => {
    const { env } = pushEnv('push-gitignore');
    // 前置：pushEnv 只提交 store/，尚未建 .gitignore 的 keys/ 行 → 未被忽略。
    expect(fs.existsSync(path.join(env.home, '.gitignore'))).toBe(false);
    expect(gitExec(env.home, ['check-ignore', '-q', 'keys/age.txt']).ok).toBe(false);

    expect((await runSecretPush({ homerHome: env.home, yes: true, noPush: true }, { age: crypto })).status).toBe(
      'pushed',
    );

    // .gitignore 已含 keys/，且 `git check-ignore` 命中私钥路径。
    expect(fs.readFileSync(path.join(env.home, '.gitignore'), 'utf8')).toContain('keys/');
    expect(gitOk(env.home, ['check-ignore', '-v', 'keys/age.txt']).trim()).toContain('keys/');
    // 私钥文件既未跟踪也未出现在任何 commit 里。
    expect(gitOk(env.home, ['ls-files', '--', 'keys/']).trim()).toBe('');
    expect(gitOk(env.home, ['ls-tree', '-r', '--name-only', 'HEAD', '--', 'keys/']).trim()).toBe('');
  });

  it('missing-source：任一目标不可读 → 全有或全无（未写任何 vault）', async () => {
    const { env, destB } = pushEnv('push-missing');
    fs.rmSync(destB); // b-secret 的目标不存在

    const report = await runSecretPush({ homerHome: env.home, yes: true }, { age: crypto });
    expect(report.status).toBe('missing-source');
    expect(report.ok).toBe(false);
    expect(report.encrypted).toEqual([]);

    // a-secret 的目标可读，但**一个 vault 文件都没写**。
    expect(fs.existsSync(secretFilePath(env.paths, 'a-secret'))).toBe(false);
    expect(fs.existsSync(secretFilePath(env.paths, 'b-secret'))).toBe(false);
    expect(fs.existsSync(env.paths.secretsDir)).toBe(false);

    const cap = capture();
    writeHomerConfig(env.home, {
      recipients: [generateIdentity().recipient],
      files: { 'a-secret': secretFilePath(env.paths, 'a-secret'), 'b-secret': path.join(env.root, 'nope.env') },
    });
    expect(await run(['secret', 'push', '--home', env.home, '--yes'], cap.io)).toBe(1);
    expect(cap.out.join('\n')).toContain('missing-source');
  });

  it('no-identity → exit 1（提示 keygen），不写 vault', async () => {
    const env = makeEnv('push-noident');
    const dest = writePlaintext(path.join(env.root, 't.env'), PLAINTEXT);
    writeHomerConfig(env.home, { recipients: [generateIdentity().recipient], files: { s: dest } });

    const report = await runSecretPush({ homerHome: env.home, yes: true }, { age: crypto });
    expect(report.status).toBe('no-identity');
    expect(report.ok).toBe(false);
    expect(report.errors.join('\n')).toContain('homer secret keygen');
    expect(fs.existsSync(env.paths.secretsDir)).toBe(false);

    const cap = capture();
    expect(await run(['secret', 'push', '--home', env.home, '--yes'], cap.io)).toBe(1);
    expect(cap.out.join('\n')).toContain('no-identity');
  });

  it('no-recipients → exit 1（不产出零 recipient 密文）', async () => {
    const env = makeEnv('push-norecip');
    const dest = writePlaintext(path.join(env.root, 't.env'), PLAINTEXT);
    writeIdentityFile(env.paths, generateIdentity());
    writeHomerConfig(env.home, { recipients: [], files: { s: dest } });

    const report = await runSecretPush({ homerHome: env.home, yes: true }, { age: crypto });
    expect(report.status).toBe('no-recipients');
    expect(report.ok).toBe(false);
    expect(fs.existsSync(env.paths.secretsDir)).toBe(false);

    const cap = capture();
    expect(await run(['secret', 'push', '--home', env.home, '--yes'], cap.io)).toBe(1);
    expect(cap.out.join('\n')).toContain('no-recipients');
  });

  it('secrets.files 为空 → no-secrets，exit 0（无 keygen 也无需 identity）', async () => {
    const env = makeEnv('push-nosecrets');
    writeHomerConfig(env.home, { recipients: [], files: {} });
    const report = await runSecretPush({ homerHome: env.home, yes: true }, { age: crypto });
    expect(report.status).toBe('no-secrets');
    expect(report.ok).toBe(true);

    const cap = capture();
    expect(await run(['secret', 'push', '--home', env.home, '--yes', '--json'], cap.io)).toBe(0);
    expect(JSON.parse(cap.out.join('\n'))['status']).toBe('no-secrets');
  });

  it('--no-push：本地 commit 成立、origin 不动', async () => {
    const { env } = pushEnv('push-nopush');
    const originBefore = gitOk(env.origin, ['rev-parse', 'HEAD']).trim();

    const report = await runSecretPush({ homerHome: env.home, yes: true, noPush: true }, { age: crypto });
    expect(report.status).toBe('pushed');
    expect(report.pushedToRemote).toBe(false);
    expect(report.commit).toBe(headCommit(env.home));
    expect(report.warnings.join('\n')).toContain('--no-push');

    // origin 停在基线：密文未过去。
    expect(gitOk(env.origin, ['rev-parse', 'HEAD']).trim()).toBe(originBefore);
    expect(gitOk(env.origin, ['ls-tree', '-r', '--name-only', 'HEAD', '--', 'secrets/']).trim()).toBe('');
  });

  it('D4 零耦合：commit 只覆盖 secrets/，store/ 的脏改动原样留在工作区', async () => {
    const { env, destA } = pushEnv('push-scope');
    // 制造 store/ 漂移（未提交）+ 另一个未跟踪文件：两者都不得进本次 commit。
    writePlaintext(path.join(env.home, 'store', 'pi', 'settings', 'settings.json'), '{"marker":"DRIFTED"}\n');
    writePlaintext(path.join(env.home, 'notes.txt'), 'untracked\n');
    const headBefore = headCommit(env.home)!;
    const storeBefore = gitOk(env.home, ['status', '--porcelain', '--', 'store/']);

    const report = await runSecretPush({ homerHome: env.home, yes: true, noPush: true }, { age: crypto });
    expect(report.status).toBe('pushed');
    expect(report.commit).not.toBe(headBefore);

    // 新 commit 只含 secrets/。
    expect(gitOk(env.home, ['show', '--name-only', '--format=', 'HEAD']).trim().split('\n')).toEqual([
      'secrets/a-secret.age',
      'secrets/b-secret.age',
    ]);
    // store/ 的脏状态与 HEAD 之前完全一致；未跟踪文件仍在。
    expect(gitOk(env.home, ['status', '--porcelain', '--', 'store/'])).toBe(storeBefore);
    expect(gitOk(env.home, ['status', '--porcelain', '--', 'store/']).trim()).not.toBe('');
    expect(fs.existsSync(path.join(env.home, 'notes.txt'))).toBe(true);

    // 明文从未进 git（任何 commit）。
    expect(gitExec(env.home, ['grep', FRAGMENT, 'HEAD']).stdout).toBe('');
    expect(fs.readFileSync(destA, 'utf8')).toBe(PLAINTEXT);
  });

  it('非 --yes：confirm 预览列出 name→destination；拒绝 → aborted exit 1（未 commit）', async () => {
    const { env } = pushEnv('push-abort');
    const ui = fakeUi(false);
    const headBefore = headCommit(env.home)!;

    const report = await runSecretPush({ homerHome: env.home }, { age: crypto, ui });
    expect(report.status).toBe('aborted');
    expect(report.ok).toBe(false);
    expect(report.encrypted).toEqual(['a-secret', 'b-secret']);
    expect(ui.confirms).toHaveLength(1);
    expect(ui.confirms[0]).toContain('a-secret');
    expect(fs.readFileSync(path.join(env.root, 'targets', 'a.env'), 'utf8')).toBe(PLAINTEXT);
    // 未产生 commit。
    expect(headCommit(env.home)).toBe(headBefore);
  });

  it('--yes 不创建 port：注入的 ui 永不被调用', async () => {
    const { env } = pushEnv('push-yes-noport');
    const ui = fakeUi(false);
    const report = await runSecretPush({ homerHome: env.home, yes: true, noPush: true }, { age: crypto, ui });
    expect(report.status).toBe('pushed');
    expect(ui.confirms).toEqual([]);
  });

  it('推送失败 → error exit 1（本地 commit 保留，密文已在本地）', async () => {
    const { env } = pushEnv('push-remotefail');
    const report = await runSecretPush(
      { homerHome: env.home, yes: true },
      { age: crypto, git: { gitPush: () => ({ ok: false, stdout: '', stderr: 'Could not resolve host' }) } },
    );
    expect(report.status).toBe('error');
    expect(report.ok).toBe(false);
    expect(report.commit).toBe(headCommit(env.home));
    expect(report.warnings.join('\n')).toContain('Could not resolve host');
    expect(fs.existsSync(secretFilePath(env.paths, 'a-secret'))).toBe(true);
  });

  it('渲染：文本含状态 + 已加密计数 + commit', async () => {
    const { env } = pushEnv('push-render');
    const report = await runSecretPush({ homerHome: env.home, yes: true, noPush: true }, { age: crypto });
    const text = renderSecretPushReport(report);
    expect(text).toContain('homer secret push: pushed');
    expect(text).toContain('已加密: 2 个密钥');
    expect(text).toContain('a-secret');
    expect(text).not.toContain(FRAGMENT);
  });

  it('缺 homer.json → error（提示 init），exit 1', async () => {
    const env = makeEnv('push-noconfig');
    const report = await runSecretPush({ homerHome: env.home, yes: true }, { age: crypto });
    expect(report.status).toBe('error');
    expect(report.errors.join('\n')).toContain('homer init');

    const cap = capture();
    expect(await run(['secret', 'push', '--home', env.home, '--yes'], cap.io)).toBe(1);
  });
});

/* ------------------------------------------------------------------ */
/* pull                                                                */
/* ------------------------------------------------------------------ */

describe('W6 · secret pull', () => {
  it('从 @{upstream} 读密文（origin 领先工作区），写目标 0600 且不 ff 整仓', async () => {
    const { env, identityA, destA, destB } = pushEnv('pull-upstream');
    // A 侧推送密文。
    const pushed = await runSecretPush({ homerHome: env.home, yes: true }, { age: crypto });
    expect(pushed.status).toBe('pushed');

    // 模拟「B 机器」：工作区回退到没有 vault 的 commit，但 origin/main 领先。
    const storeHead = gitOk(env.home, ['rev-parse', 'HEAD~1']).trim();
    gitOk(env.home, ['reset', '--hard', 'HEAD~1']);
    fs.rmSync(env.paths.secretsDir, { recursive: true, force: true });
    expect(fs.existsSync(secretFilePath(env.paths, 'a-secret'))).toBe(false);
    expect(headCommit(env.home)).toBe(storeHead);

    // 目标先写成旧内容（验证备份）。
    writePlaintext(destA, 'OLD-A\n');
    fs.rmSync(destB);

    const report = await runSecretPull({ homerHome: env.home, yes: true }, { age: crypto });
    expect(report.status).toBe('applied');
    expect(report.pulled).toEqual(['a-secret', 'b-secret']);

    // 归位内容 == 明文、权限 0600。
    expect(fs.readFileSync(destA, 'utf8')).toBe(PLAINTEXT);
    expect(mode(destA)).toBe(0o600);
    expect(mode(destB)).toBe(0o600);

    // 备份内容 == 覆盖前内容（label = secret/<name>）。
    expect(report.backupDir).toBeDefined();
    expect(fs.readFileSync(path.join(report.backupDir!, 'secret', 'a-secret'), 'utf8')).toBe('OLD-A\n');
    // 覆盖前不存在的目标不进备份。
    expect(fs.existsSync(path.join(report.backupDir!, 'secret', 'b-secret'))).toBe(false);

    // D4：**不 ff 整仓** —— 本地工作区的密文仍不存在，HEAD 未前移。
    expect(headCommit(env.home)).toBe(storeHead);
    expect(fs.existsSync(secretFilePath(env.paths, 'a-secret'))).toBe(false);
    // store/ 零改动。
    expect(gitOk(env.home, ['status', '--porcelain', '--', 'store/']).trim()).toBe('');

    // 身份仍是 A（未 keygen 覆盖）。
    expect(identityA.recipient).toMatch(/^age1/);
  });

  it('undecryptable（非 recipient 的 identity）→ exit 1 且零写入', async () => {
    const { env, destA, destB } = pushEnv('pull-undecryptable');
    expect((await runSecretPush({ homerHome: env.home, yes: true }, { age: crypto })).status).toBe('pushed');

    // 换成本机一把**从未被列为 recipient** 的新 identity。
    fs.rmSync(identityFilePath(env.paths));
    writeIdentityFile(env.paths, generateIdentity());

    const destABefore = fs.readFileSync(destA, 'utf8');
    const destBBefore = fs.readFileSync(destB, 'utf8');

    const report = await runSecretPull({ homerHome: env.home, yes: true }, { age: crypto });
    expect(report.status).toBe('undecryptable');
    expect(report.ok).toBe(false);
    expect(report.pulled).toEqual([]);
    expect(report.errors.join('\n')).toContain('secrets.recipients');

    // 零写入：两个目标都没被碰。
    expect(fs.readFileSync(destA, 'utf8')).toBe(destABefore);
    expect(fs.readFileSync(destB, 'utf8')).toBe(destBBefore);
    expect(report.backupDir).toBeUndefined();

    const cap = capture();
    expect(await run(['secret', 'pull', '--home', env.home, '--yes'], cap.io)).toBe(1);
    expect(cap.out.join('\n')).toContain('undecryptable');
  });

  it('fetch 失败 → warning + 回落读工作区 vault', async () => {
    const { env, destA } = pushEnv('pull-fetchfail');
    // 只做本地 commit（vault 在工作区里）。
    expect((await runSecretPush({ homerHome: env.home, yes: true, noPush: true }, { age: crypto })).status).toBe('pushed');
    writePlaintext(destA, 'STALE\n');

    const report = await runSecretPull(
      { homerHome: env.home, yes: true },
      { age: crypto, git: { gitFetch: () => ({ ok: false, stdout: '', stderr: 'Could not resolve host' }) } },
    );
    expect(report.status).toBe('applied');
    expect(report.warnings.join('\n')).toContain('git fetch 失败');
    expect(report.warnings.join('\n')).toContain('回落读取工作区 vault');
    expect(fs.readFileSync(destA, 'utf8')).toBe(PLAINTEXT);
    expect(fs.readFileSync(path.join(report.backupDir!, 'secret', 'a-secret'), 'utf8')).toBe('STALE\n');
  });

  it('missing-vault（upstream 里没有该密文）→ exit 1 且零写入', async () => {
    const env = makeEnv('pull-missingvault');
    commitBaseline(env, 'baseline');
    writeIdentityFile(env.paths, generateIdentity());
    const dest = writePlaintext(path.join(env.root, 't.env'), 'ORIGINAL\n');
    writeHomerConfig(env.home, { recipients: [generateIdentity().recipient], files: { ghost: dest } });

    const report = await runSecretPull({ homerHome: env.home, yes: true }, { age: crypto });
    expect(report.status).toBe('missing-vault');
    expect(report.errors.join('\n')).toContain('secrets/ghost.age');
    expect(fs.readFileSync(dest, 'utf8')).toBe('ORIGINAL\n');

    const cap = capture();
    expect(await run(['secret', 'pull', '--home', env.home, '--yes'], cap.io)).toBe(1);
  });

  it('no-identity → exit 1（提示 keygen + 旧机加 recipient 的两步迁移）', async () => {
    const env = makeEnv('pull-noident');
    commitBaseline(env, 'baseline');
    const dest = writePlaintext(path.join(env.root, 't.env'), 'ORIGINAL\n');
    writeHomerConfig(env.home, { recipients: [generateIdentity().recipient], files: { s: dest } });

    const report = await runSecretPull({ homerHome: env.home, yes: true }, { age: crypto });
    expect(report.status).toBe('no-identity');
    expect(report.errors.join('\n')).toContain('homer secret keygen');
    expect(report.errors.join('\n')).toContain('重新 `homer secret push`');
    expect(fs.readFileSync(dest, 'utf8')).toBe('ORIGINAL\n');

    const cap = capture();
    expect(await run(['secret', 'pull', '--home', env.home, '--yes'], cap.io)).toBe(1);
  });

  it('secrets.files 为空 → no-secrets exit 0', async () => {
    const env = makeEnv('pull-nosecrets');
    writeHomerConfig(env.home, { recipients: [], files: {} });
    const report = await runSecretPull({ homerHome: env.home, yes: true }, { age: crypto });
    expect(report.status).toBe('no-secrets');
    expect(report.ok).toBe(true);

    const cap = capture();
    expect(await run(['secret', 'pull', '--home', env.home, '--yes', '--json'], cap.io)).toBe(0);
    expect(JSON.parse(cap.out.join('\n'))['status']).toBe('no-secrets');
  });

  it('非 --yes：拒绝 → aborted exit 1，目标不动（密文取自 upstream）', async () => {
    const { env, destA } = pushEnv('pull-abort');
    expect((await runSecretPush({ homerHome: env.home, yes: true }, { age: crypto })).status).toBe('pushed');
    writePlaintext(destA, 'KEEP-ME\n');

    const ui = fakeUi(false);
    const report = await runSecretPull({ homerHome: env.home }, { age: crypto, ui });
    expect(report.status).toBe('aborted');
    expect(ui.confirms).toHaveLength(1);
    expect(ui.confirms[0]).toContain('a-secret');
    expect(fs.readFileSync(destA, 'utf8')).toBe('KEEP-ME\n');
  });

  it('备份落在 0700 目录树、密钥备份文件 0600（对抗式 review M1：D2「密钥明文只在 0600 下」）', async () => {
    const { env, destA } = pushEnv('pull-backupper');
    expect((await runSecretPush({ homerHome: env.home, yes: true }, { age: crypto })).status).toBe('pushed');
    writePlaintext(destA, 'OLD-KEY-BODY\n');
    fs.chmodSync(destA, 0o644); // 故意放宽：收紧必须来自 homer

    const report = await runSecretPull({ homerHome: env.home, yes: true }, { age: crypto });
    expect(report.status).toBe('applied');
    expect(report.backupDir).toBeDefined();

    const backupFile = path.join(report.backupDir!, 'secret', 'a-secret');
    expect(fs.readFileSync(backupFile, 'utf8')).toBe('OLD-KEY-BODY\n');
    expect(mode(backupFile)).toBe(0o600);
    expect(mode(env.paths.backupsDir)).toBe(0o700);
    expect(mode(path.dirname(report.backupDir!))).toBe(0o700);
    expect(mode(report.backupDir!)).toBe(0o700);
  });

  it('M2 · fetch 失败 + 工作区密文 ≠ 本地 remote-tracking ref → status=error exit 1，零写入', async () => {
    const { env, destA } = pushEnv('pull-rollback');
    expect((await runSecretPush({ homerHome: env.home, yes: true }, { age: crypto })).status).toBe('pushed');
    // 关键场景（review PoC）：upstream V2 已推送，但工作区被 reset 回 V1（无 vault 的旧 commit）。
    gitOk(env.home, ['reset', '--hard', 'HEAD~1']);
    // 工作区里放一份“旧版密文”（模拟本机残留的陈旧 vault）。
    const staleVault = await crypto.encrypt(Buffer.from('SECRET-V1-STALE\n'), [generateIdentity().recipient]);
    fs.mkdirSync(env.paths.secretsDir, { recursive: true });
    fs.writeFileSync(secretFilePath(env.paths, 'a-secret'), staleVault);
    expect(gitOk(env.home, ['rev-parse', 'origin/main']).trim()).toBeDefined();

    const destBefore = fs.readFileSync(destA);
    const report = await runSecretPull(
      { homerHome: env.home, yes: true },
      { age: crypto, git: { gitFetch: () => ({ ok: false, stdout: '', stderr: 'Could not resolve host' }) } },
    );

    expect(report.status).toBe('error');
    expect(report.ok).toBe(false);
    expect(report.pulled).toEqual([]);
    expect(report.errors.join('\n')).toContain('拒绝写旧值');
    expect(report.errors.join('\n')).toContain('origin/main');
    expect(report.warnings.join('\n')).toContain('git fetch 失败');
    // 零写入：目标未被回滚到 V1。
    expect(fs.readFileSync(destA).equals(destBefore)).toBe(true);
    expect(report.backupDir).toBeUndefined();

    // 退出码映射（§2.6）：`error` → 1。走注入端口（真 fetch 在本机可成功，不会触发本分支）。
    const code = await runSecretPull({ homerHome: env.home, yes: true }, {
      age: crypto,
      git: { gitFetch: () => ({ ok: false, stdout: '', stderr: 'Could not resolve host' }) },
    });
    expect(code.status).toBe('error');
  });

  it('M2 · fetch 失败 + 工作区密文 == 本地 ref（无分叉）→ 正常 applied（不误报）', async () => {
    const { env, destA } = pushEnv('pull-nodiverge');
    expect((await runSecretPush({ homerHome: env.home, yes: true }, { age: crypto })).status).toBe('pushed');
    writePlaintext(destA, 'STALE\n');

    const report = await runSecretPull(
      { homerHome: env.home, yes: true },
      { age: crypto, git: { gitFetch: () => ({ ok: false, stdout: '', stderr: 'Could not resolve host' }) } },
    );
    // 工作区 == HEAD == origin/main（刚 push）→ 无回滚风险，照常 applied。
    expect(report.status).toBe('applied');
    expect(fs.readFileSync(destA, 'utf8')).toBe(PLAINTEXT);
  });

  it('M2 · fetch 失败且无本地 tracking ref → 回落但 warning 明示回滚，--yes 下直接 error', async () => {
    const { env, destA } = pushEnv('pull-unverifiable');
    expect((await runSecretPush({ homerHome: env.home, yes: true, noPush: true }, { age: crypto })).status).toBe('pushed');
    writePlaintext(destA, 'STALE\n');
    const destBefore = fs.readFileSync(destA);

    // 本地无 origin/main ref：读不到 → 无法比对（传 `refExists: () => false` 精确模拟，
    // 不依赖 `git update-ref -d` 的副作用）。
    const deps = {
      age: crypto,
      git: {
        gitFetch: () => ({ ok: false, stdout: '', stderr: 'Could not resolve host' }),
        refExists: () => false,
      },
    };

    // `--yes`：回滚是高危操作 → 直接 error，零写入。
    const yesReport = await runSecretPull({ homerHome: env.home, yes: true }, deps);
    expect(yesReport.status).toBe('error');
    expect(yesReport.warnings.join('\n')).toContain('将回滚到本地旧版密文');
    expect(yesReport.errors.join('\n')).toContain('--yes');
    expect(fs.readFileSync(destA, 'utf8')).toBe('STALE\n');

    // 非 `--yes`：确认预览里带回滚警告；用户拒绝 → aborted（仍零写入）。
    const ui = fakeUi(false);
    const aborted = await runSecretPull({ homerHome: env.home }, { ...deps, ui });
    expect(aborted.status).toBe('aborted');
    expect(ui.confirms[0]).toContain('回滚到');
    expect(fs.readFileSync(destA).equals(destBefore)).toBe(true);

    // 非 `--yes` + 用户同意 → 回落生效（明示过的回滚）。
    const accepted = await runSecretPull({ homerHome: env.home }, { ...deps, ui: fakeUi(true) });
    expect(accepted.status).toBe('applied');
    expect(fs.readFileSync(destA, 'utf8')).toBe(PLAINTEXT);
  });

  it('M3 · 取数双源对称：upstream ref 有 commit 但**无 vault**，工作区有 → applied（不再误报 missing-vault）', async () => {
    const { env, destA } = pushEnv('pull-dualsource');
    // 只做本地 commit（密文只在工作区，从未入库）。
    expect((await runSecretPush({ homerHome: env.home, yes: true, noPush: true }, { age: crypto })).status).toBe('pushed');
    expect(fs.existsSync(secretFilePath(env.paths, 'a-secret'))).toBe(true);
    writePlaintext(destA, 'STALE\n');

    // fetch 成功 + upstream 可解析，但 upstream 的 commit 里没有 `secrets/`（工作区与 upstream 相同内容）。
    const report = await runSecretPull({ homerHome: env.home, yes: true }, { age: crypto });

    expect(report.status).toBe('applied');
    expect(report.pulled).toEqual(['a-secret', 'b-secret']);
    expect(fs.readFileSync(destA, 'utf8')).toBe(PLAINTEXT);
  });

  it('M3 · 双源都不存在 → 仍 missing-vault（不把“回落到工作区”当成“总能找到”）', async () => {
    const env = makeEnv('pull-dualmissing');
    commitBaseline(env, 'baseline');
    writeIdentityFile(env.paths, generateIdentity());
    const dest = writePlaintext(path.join(env.root, 't.env'), 'ORIGINAL\n');
    writeHomerConfig(env.home, { recipients: [generateIdentity().recipient], files: { ghost: dest } });

    const report = await runSecretPull({ homerHome: env.home, yes: true }, { age: crypto });
    expect(report.status).toBe('missing-vault');
    expect(report.errors.join('\n')).toContain('secrets/ghost.age');
    expect(report.errors.join('\n')).toContain('工作区与 origin/main');
    expect(fs.readFileSync(dest, 'utf8')).toBe('ORIGINAL\n');
  });

  it('渲染：文本含状态 + 计数 + 备份目录', async () => {
    const { env, destA } = pushEnv('pull-render');
    expect((await runSecretPush({ homerHome: env.home, yes: true }, { age: crypto })).status).toBe('pushed');
    writePlaintext(destA, 'OLD\n');
    const report = await runSecretPull({ homerHome: env.home, yes: true }, { age: crypto });
    expect(report.status).toBe('applied');
    const text = renderSecretPullReport(report);
    expect(text).toContain('homer secret pull: applied');
    expect(text).toContain('已归位: 2 个密钥');
    expect(text).toContain('备份目录:');
    expect(text).not.toContain(FRAGMENT);
  });
});

/* ------------------------------------------------------------------ */
/* list                                                                */
/* ------------------------------------------------------------------ */

describe('W6 · secret list', () => {
  it('列出 name → destination 与 vault present/missing（纯读，无需 identity）', async () => {
    const env = makeEnv('list');
    const destA = path.join(env.root, 'a.env');
    const destB = path.join(env.root, 'b.env');
    writeHomerConfig(env.home, {
      files: { 'has-vault': destA, 'no-vault': destB },
    });
    // 只造一个 vault 文件（不解密、无 identity 也应能 list）。
    fs.mkdirSync(env.paths.secretsDir, { recursive: true });
    fs.writeFileSync(secretFilePath(env.paths, 'has-vault'), Buffer.from('not-really-ciphertext'));

    const report = runSecretList({ homerHome: env.home });
    expect(report.secrets).toEqual([
      { name: 'has-vault', destination: destA, vaultFile: 'present' },
      { name: 'no-vault', destination: destB, vaultFile: 'missing' },
    ]);

    const text = renderSecretListReport(report);
    expect(text).toContain('has-vault');
    expect(text).toContain('[vault present]');
    expect(text).toContain('[vault missing]');
  });

  it('`--json` 形状：{ secrets: [{name,destination,vaultFile}] }，exit 0', async () => {
    const env = makeEnv('list-json');
    const dest = path.join(env.root, 'x.env');
    writeHomerConfig(env.home, { files: { x: dest } });

    const cap = capture();
    expect(await run(['secret', 'list', '--home', env.home, '--json'], cap.io)).toBe(0);
    const parsed = JSON.parse(cap.out.join('\n')) as { secrets: unknown[] };
    expect(Object.keys(parsed)).toEqual(['secrets']);
    expect(parsed.secrets).toEqual([{ name: 'x', destination: dest, vaultFile: 'missing' }]);
  });

  it('secrets.files 缺省 → 空数组 + 明确文案（exit 0）', async () => {
    const env = makeEnv('list-empty');
    writeHomerConfig(env.home, {});
    const report = runSecretList({ homerHome: env.home });
    expect(report.secrets).toEqual([]);
    expect(renderSecretListReport(report)).toContain('未配置密钥');

    const cap = capture();
    expect(await run(['secret', 'list', '--home', env.home], cap.io)).toBe(0);
    expect(cap.out.join('\n')).toContain('未配置密钥');
  });

  it('list 纯读：不创建 secrets/ 目录、不解密、不改任何文件', async () => {
    const env = makeEnv('list-pure');
    const dest = writePlaintext(path.join(env.root, 'p.env'), PLAINTEXT);
    writeHomerConfig(env.home, { files: { p: dest } });
    const destBefore = fs.readFileSync(dest);

    runSecretList({ homerHome: env.home });
    expect(fs.existsSync(env.paths.secretsDir)).toBe(false);
    expect(fs.readFileSync(dest).equals(destBefore)).toBe(true);
  });
});
