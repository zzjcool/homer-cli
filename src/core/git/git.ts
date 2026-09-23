/**
 * git 边界（docs/m2-plan.md §2.4）。
 *
 * 约定：
 * - 所有命令以 `home`（= `<HOMER_HOME>`，即 `~/.homer`）为 cwd 执行；
 * - `gitExec` **永不 throw**，失败/超时一律 `{ ok: false, ... }`，调用方自行判定；
 * - 需要「失败即用户级错误」的语义由调用方（CLI 命令层 / W6 base.ts）构造 CliError。
 *
 * 为什么永不 throw：git 的可用性是环境问题（无 git、无 remote、离线、超时、非仓库），
 * 不是编程错误。M2 的降级矩阵（§1 降级矩阵）要求这些情形可被分派为 warning / CliError /
 * 降级路径，而不是让异常穿透 core 层。
 */

import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';

export interface GitExecResult {
  ok: boolean;
  stdout: string;
  stderr: string;
}

/** `gitExec` 默认超时（冻结于 §2.4：15s）。 */
export const GIT_DEFAULT_TIMEOUT_MS = 15_000;

/** store 相对 home 的固定目录名（paths.ts 冻结布局：`storeDir = <home>/store`）。 */
const STORE_DIR_NAME = 'store';

/** store 的 git pathspec（相对仓库根，posix 分隔符）。 */
const STORE_PATHSPEC = `${STORE_DIR_NAME}/`;

/** 大快照下 stdout 可能远超默认 1MB（git show 整文件 / ls-tree 全量）。 */
const MAX_BUFFER = 64 * 1024 * 1024;

/**
 * `.gitignore` 必须包含的行（D5：state.json 与 backups/ 不入库；M3 §2.0-5：keys/ 亦不入库）。
 *
 * `keys/` = age 私钥目录（`<home>/keys/age.txt`，0600，docs/m3-plan.md D2）：
 * 私钥永不入库、永不同步；公钥（recipient）才随 `homer.json` 走。
 */
export const GITIGNORE_REQUIRED_LINES: readonly string[] = ['state.json', 'backups/', 'keys/'];

/** execFileSync 的 stdout/stderr 在异常分支里可能是 Buffer / null。 */
function asText(value: unknown): string {
  if (typeof value === 'string') return value;
  if (Buffer.isBuffer(value)) return value.toString('utf8');
  return '';
}

/**
 * 执行一条 git 命令。永不 throw。
 *
 * 超时（`opts.timeoutMs`，默认 15s）与 spawn 失败（ENOENT 等）都返回 `ok: false`；
 * 失败分支的 `stderr` 优先取真实 stderr，缺失时回落到 Error.message（例如 spawn 失败）。
 */
export function gitExec(
  home: string,
  args: readonly string[],
  opts?: { timeoutMs?: number },
): GitExecResult {
  const timeoutMs = opts?.timeoutMs !== undefined && opts.timeoutMs > 0
    ? opts.timeoutMs
    : GIT_DEFAULT_TIMEOUT_MS;

  try {
    const stdout = execFileSync('git', [...args], {
      cwd: home,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
      timeout: timeoutMs,
      maxBuffer: MAX_BUFFER,
      windowsHide: true,
    });
    return { ok: true, stdout: asText(stdout), stderr: '' };
  } catch (err) {
    const failure = err as { stdout?: unknown; stderr?: unknown; message?: unknown };
    const stderr = asText(failure.stderr);
    return {
      ok: false,
      stdout: asText(failure.stdout),
      stderr: stderr !== '' ? stderr : asText(failure.message),
    };
  }
}

/** realpath，失败则原样返回（目录刚被删 / 权限问题不改变判定结果）。 */
function realpathSafe(target: string): string {
  try {
    return fs.realpathSync(target);
  } catch {
    return target;
  }
}

/**
 * `home` 自身是否是一个 git 仓库的根。
 *
 * 用 `rev-parse --show-toplevel` 比对而非「存在 .git 文件」：避免把「位于其它仓库内部」
 * 的目录误判为自己的仓库（否则 `homer push` 会把 commit 落到外层仓库）。
 */
export function isGitRepo(home: string): boolean {
  const result = gitExec(home, ['rev-parse', '--show-toplevel']);
  if (!result.ok) return false;
  const top = result.stdout.trim();
  if (top === '') return false;
  return realpathSafe(top) === realpathSafe(home);
}

/**
 * 幂等维护 `<home>/.gitignore`：只追加缺失的必需行，已有内容（含用户自己写的规则）原样保留。
 * 二次运行不产生任何写入（字节级不变）。
 */
function ensureGitignore(home: string): void {
  const file = path.join(home, '.gitignore');

  let existing = '';
  try {
    existing = fs.readFileSync(file, 'utf8');
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code !== 'ENOENT') throw err;
  }

  const present = new Set(existing.split('\n').map((line) => line.trim()));
  const missing = GITIGNORE_REQUIRED_LINES.filter((line) => !present.has(line));
  if (missing.length === 0) return;

  let next = existing;
  if (next !== '' && !next.endsWith('\n')) next += '\n';
  next += `${missing.join('\n')}\n`;
  fs.writeFileSync(file, next, 'utf8');
}

/**
 * 确保 `home` 是 git 仓库（D1：首次 `homer push` 自动 init，进入 local-only 模式），
 * 并幂等维护 `.gitignore`（state.json / backups/ 不入库）。
 *
 * 不设置 user.name/user.email——那属于用户环境；缺失时 commit 会失败并被
 * `commitAllStore` 映射为 `undefined`（命令层据此给出提示）。
 */
export function ensureGitRepo(home: string): void {
  fs.mkdirSync(home, { recursive: true });
  if (!isGitRepo(home)) {
    gitExec(home, ['init']);
  }
  ensureGitignore(home);
}

/** 当前分支是否配置了 upstream（D4）。 */
export function hasUpstream(home: string): boolean {
  return upstreamRef(home) !== undefined;
}

/** upstream 的短名，如 `origin/main`；无 upstream → undefined。 */
export function upstreamRef(home: string): string | undefined {
  const result = gitExec(home, ['rev-parse', '--abbrev-ref', '--symbolic-full-name', '@{upstream}']);
  if (!result.ok) return undefined;
  const ref = result.stdout.trim();
  return ref === '' ? undefined : ref;
}

/**
 * 当前分支**配置**里的 upstream 短名（additive，M3 对抗式 review M2）。
 *
 * 与 `upstreamRef` 的关键区别：本函数读的是 `branch.<name>.{remote,merge}` **配置**，
 * 即使对应的 remote-tracking ref 在本地不存在（刚 fetch 失败 / 被 gc 掉）也能返回名字；
 * 而 `upstreamRef` 走 `@{upstream}` 解析，ref 缺失就报错 → undefined。
 *
 * 用途：`secret pull` 在 `git fetch` 失败时需要判断「是否有 upstream 配置可比对」——
 * 后者不能依赖 ref 是否存在（那恰恰是待判定的东西）。非仓库 / 无分支 / 无配置 → undefined。
 */
export function configuredUpstream(home: string): string | undefined {
  const branch = gitExec(home, ['symbolic-ref', '--quiet', '--short', 'HEAD']);
  if (!branch.ok) return undefined;
  const name = branch.stdout.trim();
  if (name === '') return undefined;

  const result = gitExec(home, ['for-each-ref', '--format=%(upstream:short)', `refs/heads/${name}`]);
  if (!result.ok) return undefined;
  const ref = result.stdout.trim();
  return ref === '' ? undefined : ref;
}

/** `git fetch`（不指定 remote/refspec，按 upstream 语义由 git 自行决定）。 */
export function gitFetch(home: string): GitExecResult {
  return gitExec(home, ['fetch']);
}

/**
 * 本地 ref 是否可解析（`git rev-parse --verify --quiet <ref>`；additive，M3 对抗式 review M2）。
 *
 * 用途：`secret pull` 在 `git fetch` 失败时判断「本地 remote-tracking ref 是否可用」——
 * `upstreamRef()` 读的是 **配置**（`branch.<name>.merge`），即使 `refs/remotes/…` 不存在
 * 也能返回 `origin/main`；而能否真正 `git show` 出密文取决于该 ref 是否在本地存在。
 * 两者语义不同，故需要这个独立判定。
 * 不 throw（与 `gitExec` 同款：git 不可用 / 非仓库 / ref 不存在 → false）。
 */
export function refExists(home: string, ref: string): boolean {
  if (ref === '') return false;
  return gitExec(home, ['rev-parse', '--verify', '--quiet', ref]).ok;
}

/** `git push`；无 upstream 时 git 会以 ok:false 失败，由调用方降级为 warning。 */
export function gitPush(home: string): GitExecResult {
  return gitExec(home, ['push']);
}

/** HEAD SHA；无 commit（unborn HEAD）或非仓库 → undefined。 */
export function headCommit(home: string): string | undefined {
  const result = gitExec(home, ['rev-parse', 'HEAD']);
  if (!result.ok) return undefined;
  const sha = result.stdout.trim();
  return sha === '' ? undefined : sha;
}

/**
 * `git add -A -- store/` + `git commit`（D2：commit 只覆盖 store/，homer.json 等其它
 * 路径的变更不掺进本次同步 commit）。
 *
 * store 无变更（或非仓库 / commit 失败，例如缺 user.email）→ undefined。
 */
export function commitAllStore(home: string, message: string): string | undefined {
  if (isStoreClean(home)) return undefined;

  const add = gitExec(home, ['add', '-A', '--', STORE_PATHSPEC]);
  if (!add.ok) return undefined;

  const commit = gitExec(home, ['commit', '-m', message, '--', STORE_PATHSPEC]);
  if (!commit.ok) return undefined;

  return headCommit(home);
}

/**
 * `git add -A -- <pathspecs...>` + `git commit -m <message> -- <pathspecs...>`
 * （docs/m3-plan.md §2.3 additive；§1-D4 secret 管线边界）。
 *
 * 与 `commitAllStore` 的区别只在 pathspec：secret 通道用 `['secrets/']`，**不与 store/ 耦合**
 * —— 未列入 pathspec 的路径即使有改动也不会进这次 commit（store/ 的脏工作区保持原样，
 * 反之亦然）。`git status --porcelain -- <pathspecs>` 空 = 无变更 → `undefined`（幂等重跑）。
 *
 * 返回新 HEAD SHA；无变更 / 非仓库 / commit 失败（如缺 user.email）→ `undefined`（不抛）。
 * 调用方（`secret push`）据此区分「无事可做」与「真的提交失败」。
 *
 * `pathspecs` 为空数组 → `undefined`（无意义调用；`git status --` 空 pathspec 会报错）。
 */
export function commitPaths(
  home: string,
  pathspecs: readonly string[],
  message: string,
): string | undefined {
  if (pathspecs.length === 0) return undefined;

  // 变更检测必须先于 `git add`：`git add -- secrets/` 在目录尚不存在时会以 pathspec
  // 不匹配失败（fatal），而「还没有任何 vault 文件」是合法状态而不是错误。
  const status = gitExec(home, ['status', '--porcelain', '--', ...pathspecs]);
  if (!status.ok || status.stdout.trim() === '') return undefined;

  const add = gitExec(home, ['add', '-A', '--', ...pathspecs]);
  if (!add.ok) return undefined;

  const commit = gitExec(home, ['commit', '-m', message, '--', ...pathspecs]);
  if (!commit.ok) return undefined;

  return headCommit(home);
}

/** `git merge --ff-only @{upstream}`（D6：ff 失败 = 分叉，调用方在应用工具目录前拦截）。 */
export function mergeFfUpstream(home: string): GitExecResult {
  return gitExec(home, ['merge', '--ff-only', '@{upstream}']);
}

/**
 * store 工作区是否干净（`git status --porcelain -- store/` 为空）。
 *
 * 关注范围仅 store/：state.json / backups/ 被 .gitignore 排除，root 下未跟踪文件不影响判定。
 * 非仓库 / git 失败 → false（不可判定时按「不干净」处理，宁保守不冒险）。
 */
export function isStoreClean(home: string): boolean {
  const result = gitExec(home, ['status', '--porcelain', '--', STORE_PATHSPEC]);
  if (!result.ok) return false;
  return result.stdout.trim() === '';
}

/** `ancestor` 是否为 `descendant` 的祖先（`merge-base --is-ancestor`，D6 的分叉前置检查）。 */
export function isAncestorOf(home: string, ancestor: string, descendant: string): boolean {
  const result = gitExec(home, ['merge-base', '--is-ancestor', ancestor, descendant]);
  return result.ok;
}
