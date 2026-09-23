/**
 * `homer doctor` 的八项检查实现（docs/m3-plan.md §2.5 / §1-D6，P2-W7）。
 *
 * ## 分层
 *
 * 本文件是**纯逻辑 + 薄 fs/git**：每个 `checkXxx` 只读入参声明的路径 / 配置，返回一个
 * `DoctorCheck`，不做 IO 编排、不打印、不决定退出码（那是 `src/cli/commands/doctor.ts` 的活）。
 * `checkAge` 的密码学能力经 `AgeCryptoPort` **注入**（§2.5 冻结签名），使 age 检查与 W1 的
 * 真实实现解耦——单测可用假 port 造出「本机不是 recipient」这类无法稳定复现的场景（§4-4）。
 *
 * ## 八项检查（§2.5 / D6 冻结 id 集合，顺序由命令层决定）
 *
 * | id | 语义 | 状态 |
 * |---|---|---|
 * | `config` | `homer.json` 存在且合法 | 缺失 / 非法 → **fail** |
 * | `repo` | home 是 git 仓库根（含 upstream 事实） | 非仓库 → **fail**；无 upstream → warn |
 * | `store-clean` | `store/` 工作区干净 | 脏 → **warn** |
 * | `remote` | 远端可达（`git ls-remote --heads <url>`，10s 超时） | `--offline` → ok（跳过）；不可达 / 无 upstream → **warn** |
 * | `adapters` | 各 enabled adapter root 存在 | 缺失 → **warn**（工具未安装？）+ details |
 * | `age` | 密钥同步配置与身份 | 未配置 → ok；identity 缺失 / recipients 空 / 试解密失败 → **fail** |
 * | `machine` | state 与 HEAD 的关系 | 缺失 / 领先 → **warn** |
 * | `required` | store 中的 `__REQUIRED__` 占位符残留 | 有 → **warn**（details = 文件 + 键） |
 *
 * ## 状态与退出码的边界（§1-D6 冻结）
 *
 * `fail` → exit 1，`warn` → exit 0。因此「用户必须自己决定 / 信息性」的问题一律 **warn**
 * （store 脏、远端离线、adapter 未安装、state 落后、`__REQUIRED__` 残留），
 * 只有「homer 立刻不可用」的问题才 **fail**（配置缺失、非 git 仓库、密钥层自相矛盾）。
 *
 * ## 只读保证
 *
 * 本文件**绝不写盘**：唯一的写操作风险点（git ls-remote / git status）都是只读子命令，
 * 且 `gitExec` 永不 throw、失败降级为 `{ok:false}`（见 `core/git/git.ts` 的文件头约定）。
 */

import fs from 'node:fs';

import { validateConfig } from '../config.js';
import { isPlainObject } from '../entry-kind.js';
import { gitExec, headCommit, isGitRepo, isStoreClean, upstreamRef } from '../git/index.js';
import { loadState } from '../state.js';
import { readSnapshotFromStore } from '../store/store.js';
import { expandHome, type HomerPaths } from '../paths.js';
import { REQUIRED_PLACEHOLDER } from '../sync/index.js';
import { decryptSecretFromFile, identityFilePath, listSecrets, loadIdentity } from '../age/index.js';
import type { AdapterSnapshot, HomerConfig } from '../types.js';
import type { AgeCryptoPort } from '../age/index.js';

/** 单项检查状态（§2.5 冻结）。 */
export type CheckStatus = 'ok' | 'warn' | 'fail';

/** 八项检查的 id 集合（§2.5 冻结，顺序 = 命令层的报告顺序）。 */
export type DoctorCheckId =
  | 'config' | 'repo' | 'store-clean' | 'remote' | 'adapters' | 'age' | 'machine' | 'required';

/** 单项检查结果。`details` 是给人看的补充信息（多行缩进渲染，`--json` 里原样呈现）。 */
export interface DoctorCheck { id: DoctorCheckId; status: CheckStatus; message: string; details?: string[]; }

/** `ok` = 无 `fail`（`warn` 不影响 ok 与退出码）。 */
export interface DoctorReport { checks: DoctorCheck[]; ok: boolean; }

/** `git ls-remote` 的超时（§2.5 冻结：10s；比 `gitExec` 默认 15s 更保守——doctor 不该挂住）。 */
export const REMOTE_CHECK_TIMEOUT_MS = 10_000;

/** store 脏时 details 里最多列出的条目数（防大改动刷屏）。 */
const MAX_STORE_DETAILS = 20;

/* ------------------------------------------------------------------ */
/* ① config                                                            */
/* ------------------------------------------------------------------ */

/**
 * `homer.json` 存在且合法。
 *
 * 三种失败分开报（缺失 / 不是合法 JSON / 校验不过），因为用户的下一步动作完全不同：
 * 前者要 `homer init`，后两者要修文件。校验错误逐条进 `details`（`validateConfig` 已给结构化错误）。
 */
export function checkConfig(paths: HomerPaths): DoctorCheck {
  let text: string;
  try {
    text = fs.readFileSync(paths.configFile, 'utf8');
  } catch (err) {
    const code = (err as NodeJS.ErrnoException).code;
    return {
      id: 'config',
      status: 'fail',
      message:
        code === 'ENOENT'
          ? `未找到 ${paths.configFile}`
          : `无法读取 ${paths.configFile}: ${err instanceof Error ? err.message : String(err)}`,
      details: ['先运行 `homer init` 生成 homer.json 与 store 快照。'],
    };
  }

  let raw: unknown;
  try {
    raw = JSON.parse(text) as unknown;
  } catch (err) {
    return {
      id: 'config',
      status: 'fail',
      message: `${paths.configFile} 不是合法 JSON`,
      details: [err instanceof Error ? err.message : String(err)],
    };
  }

  const result = validateConfig(raw);
  if (!result.ok) {
    return {
      id: 'config',
      status: 'fail',
      message: `homer.json 配置无效（${result.errors.length} 处）`,
      details: result.errors,
    };
  }

  const adapterIds = Object.keys(result.config.adapters);
  return {
    id: 'config',
    status: 'ok',
    message: `homer.json 有效（${adapterIds.length} 个 adapter: ${adapterIds.join(', ')}）`,
  };
}

/* ------------------------------------------------------------------ */
/* ② repo / ③ store-clean                                              */
/* ------------------------------------------------------------------ */

/** ② repo：home 是否是 git 仓库根；是仓库但未配 upstream → warn（push 会走 local-only）。 */
function checkRepo(paths: HomerPaths): DoctorCheck {
  if (!isGitRepo(paths.home)) {
    return {
      id: 'repo',
      status: 'fail',
      message: `工作区不是 git 仓库: ${paths.home}`,
      details: ['请先运行 `homer push` 建立 git 历史与 remote。'],
    };
  }

  const ref = upstreamRef(paths.home);
  if (ref === undefined) {
    return {
      id: 'repo',
      status: 'warn',
      message: 'git 仓库已就绪，但未配置 upstream（同步将停留在本地）',
      details: [
        '配置远端：`git push -u <remote> <branch>`（或在 home 内 `git branch --set-upstream-to`）。',
      ],
    };
  }

  return { id: 'repo', status: 'ok', message: `git 仓库（upstream: ${ref}）` };
}

/** ③ store-clean：`store/` 工作区脏 → warn（未提交的改动会让三方判定失去基准）。 */
function checkStoreClean(paths: HomerPaths): DoctorCheck {
  if (!isGitRepo(paths.home)) {
    return {
      id: 'store-clean',
      status: 'warn',
      message: 'store 工作区状态不可判定（不是 git 仓库）',
    };
  }

  if (isStoreClean(paths.home)) {
    return { id: 'store-clean', status: 'ok', message: 'store 工作区干净' };
  }

  // `--untracked-files=all`：默认的 `-normal` 会把整个未跟踪目录折叠成一行
  //（`?? store/pi/settings/`），details 要列出「哪个文件」就必须展开。
  const status = gitExec(paths.home, ['status', '--porcelain', '--untracked-files=all', '--', 'store/']);
  const lines = status.ok
    ? status.stdout.split('\n').map((line) => line.trimEnd()).filter((line) => line !== '')
    : [];
  const details = lines.slice(0, MAX_STORE_DETAILS);
  if (lines.length > MAX_STORE_DETAILS) details.push(`… 其余 ${lines.length - MAX_STORE_DETAILS} 条省略`);
  if (lines.length === 0) details.push('git status 无法判定（git 不可用？）');
  details.push('先运行 `homer push` 提交 store 改动，否则 pull / merge 会拒绝执行。');

  return {
    id: 'store-clean',
    status: 'warn',
    message: 'store 工作区有未提交的改动',
    details,
  };
}

/**
 * ② + ③：`['repo', 'store-clean']`（§2.5 冻结签名）。
 *
 * 合成一个函数是因为两者的判定共享同一前提「是不是 git 仓库」：非仓库时 repo 报
 * fail，store 状态无从谈起（报 warn + 「不可判定」而不是误报「store 脏」）。
 */
export function checkRepoAndStore(paths: HomerPaths): DoctorCheck[] {
  return [checkRepo(paths), checkStoreClean(paths)];
}

/* ------------------------------------------------------------------ */
/* ④ remote                                                            */
/* ------------------------------------------------------------------ */

/** upstream 的 remote URL（`branch.<name>.remote` → `git remote get-url`）。 */
function upstreamUrl(home: string): string | undefined {
  const ref = upstreamRef(home);
  if (ref === undefined) return undefined;

  // 优先信任分支配置（remote 名可能含 '/'，直接切 ref 会切错）；失败再按 `origin/main` 切。
  const branch = gitExec(home, ['symbolic-ref', '--short', 'HEAD']);
  const branchName = branch.ok ? branch.stdout.trim() : '';
  let remoteName = '';
  if (branchName !== '') {
    const configured = gitExec(home, ['config', '--get', `branch.${branchName}.remote`]);
    remoteName = configured.ok ? configured.stdout.trim() : '';
  }
  if (remoteName === '') {
    const slash = ref.indexOf('/');
    if (slash <= 0) return undefined;
    remoteName = ref.slice(0, slash);
  }

  const url = gitExec(home, ['remote', 'get-url', remoteName]);
  const value = url.ok ? url.stdout.trim() : '';
  return value === '' ? undefined : value;
}

/**
 * ④ remote：远端可达性（`git ls-remote --heads <upstream url>`，10s 超时）。
 *
 * `--offline` → ok（'已跳过'）：这是 m2-report §5-3 的诉求——CI / 断网环境需要一个
 * 「不碰网络」的体检档位。其余不可达情形（无 upstream / ls-remote 失败）一律 warn：
 * 离线与认证失败在用户视角是同一件事（「现在拉不到远端」），不是 homer 坏了。
 */
export function checkRemote(paths: HomerPaths, opts: { offline: boolean }): DoctorCheck {
  if (opts.offline) {
    return { id: 'remote', status: 'ok', message: '远端可达性检查已跳过（--offline）' };
  }

  if (!isGitRepo(paths.home)) {
    return { id: 'remote', status: 'warn', message: '跳过远端检查：工作区不是 git 仓库' };
  }

  const url = upstreamUrl(paths.home);
  if (url === undefined) {
    return { id: 'remote', status: 'warn', message: '未配置 git upstream，跳过远端可达性检查' };
  }

  const result = gitExec(paths.home, ['ls-remote', '--heads', url], {
    timeoutMs: REMOTE_CHECK_TIMEOUT_MS,
  });
  if (!result.ok) {
    const firstLine = result.stderr.split('\n').map((line) => line.trim()).find((line) => line !== '');
    return {
      id: 'remote',
      status: 'warn',
      message: `远端不可达（离线？）: ${url}`,
      details: firstLine === undefined ? [] : [firstLine],
    };
  }

  const branches = result.stdout.split('\n').filter((line) => line.trim() !== '').length;
  return { id: 'remote', status: 'ok', message: `远端可达: ${url}（${branches} 个分支）` };
}

/* ------------------------------------------------------------------ */
/* ⑤ adapters                                                          */
/* ------------------------------------------------------------------ */

/**
 * ⑤ adapters：逐 **enabled** adapter 检查 root 是否存在。
 *
 * 缺失 = 「这台机器还没装这个工具」（§2.5 原文「工具未安装？」）→ warn + details 列全 id/root，
 * 因为 homer 的能力本就是「有什么工具就同步什么」，缺一个不影响其余。
 * `enabled: false` 的 adapter 刻意不同步，不参与检查（否则用户会为每个不用的工具吃一条告警）。
 */
export function checkAdapters(config: HomerConfig): DoctorCheck {
  const missing: string[] = [];
  let checked = 0;

  for (const [adapterId, adapter] of Object.entries(config.adapters)) {
    if (adapter.enabled === false) continue;
    checked += 1;
    const root = expandHome(adapter.root);
    if (!fs.existsSync(root)) {
      missing.push(`${adapterId}: ${adapter.root}（${root}）`);
      continue;
    }
    let isDir = false;
    try {
      isDir = fs.statSync(root).isDirectory();
    } catch {
      isDir = false;
    }
    if (!isDir) missing.push(`${adapterId}: ${adapter.root}（${root}）不是目录`);
  }

  if (checked === 0) return { id: 'adapters', status: 'ok', message: '没有启用的 adapter' };
  if (missing.length === 0) {
    return { id: 'adapters', status: 'ok', message: `${checked} 个 adapter root 均存在` };
  }
  return {
    id: 'adapters',
    status: 'warn',
    message: `${missing.length}/${checked} 个 adapter root 缺失（工具未安装？）`,
    details: missing,
  };
}

/* ------------------------------------------------------------------ */
/* ⑥ age                                                               */
/* ------------------------------------------------------------------ */

const RE_PUSH_HINT =
  '若本机是新设备：在旧机把本机 recipient 加入 secrets.recipients，重新 `homer secret push` 后再 `homer secret pull`。';

/**
 * ⑥ age：密钥同步配置与本机 identity（§2.5 冻结签名，crypto 注入）。
 *
 * 判定阶梯（顺序有意义——越靠前的问题越基础）：
 *   1. `secrets.files` 与 `secrets.recipients` 都为空 → **ok**（用户根本没用密钥通道）；
 *   2. identity 缺失 → **fail**（无法解密任何东西，且 pull 会 `no-identity`）；
 *   3. `recipients` 为空 → **fail**（push 加密无目标，`encryptSecretToFile` 会拒绝）；
 *   4. 有 vault 文件 → 逐个 `decryptSecretFromFile`（真解密，不是存在性检查）：
 *      任一失败 → **fail**（典型原因：本机 identity 不是 recipient）；
 *   5. vault 文件缺失 → **warn**（配置里有这个密钥但远端/本地都还没有密文 → 尚未 push）。
 *
 * 只有「配置了密钥同步」时才检查 identity：没配 `secrets` 的机器不该被要求 keygen
 * （§3-P4 ⑦ 的真实环境冒烟就依赖这条：`homer doctor` 报 age 未配置 → ok）。
 */
export async function checkAge(
  paths: HomerPaths,
  config: HomerConfig,
  crypto: AgeCryptoPort,
): Promise<DoctorCheck> {
  const files = config.secrets?.files ?? {};
  const recipients = config.secrets?.recipients ?? [];
  const names = Object.keys(files).sort();

  if (names.length === 0 && recipients.length === 0) {
    return { id: 'age', status: 'ok', message: '未配置密钥同步（secrets 段为空）' };
  }

  const identity = loadIdentity(paths);
  if (identity === undefined) {
    return {
      id: 'age',
      status: 'fail',
      message: `未找到可用的 age identity: ${identityFilePath(paths)}`,
      details: ['先运行 `homer secret keygen`。', RE_PUSH_HINT],
    };
  }

  if (recipients.length === 0) {
    return {
      id: 'age',
      status: 'fail',
      message: 'secrets.recipients 为空：secret push 没有加密目标',
      details: ['在 homer.json 的 secrets.recipients 里至少配置一个 `age1...` 公钥。'],
    };
  }

  if (names.length === 0) {
    return {
      id: 'age',
      status: 'ok',
      message: `identity 就绪（${identity.recipient}），secrets.files 未配置密钥`,
    };
  }

  const failed: string[] = [];
  const missing: string[] = [];
  const vaultStatus = new Map(listSecrets(paths, config).map((entry) => [entry.name, entry.vaultFile]));

  for (const name of names) {
    if (vaultStatus.get(name) !== 'present') {
      missing.push(`${name}（${name}.age 不存在）`);
      continue;
    }
    try {
      await decryptSecretFromFile(crypto, paths, name);
    } catch (err) {
      failed.push(`${name}: ${err instanceof Error ? err.message : String(err)}`);
    }
  }

  if (failed.length > 0) {
    return {
      id: 'age',
      status: 'fail',
      message: `${failed.length} 个密钥无法解密（本机 identity 不是 recipient？）`,
      details: [...failed, RE_PUSH_HINT],
    };
  }

  if (missing.length > 0) {
    return {
      id: 'age',
      status: 'warn',
      message: `${missing.length} 个 vault 文件缺失（尚未 secret push？）`,
      details: missing,
    };
  }

  return { id: 'age', status: 'ok', message: `${names.length} 个密钥均可解密` };
}

/* ------------------------------------------------------------------ */
/* ⑦ machine                                                           */
/* ------------------------------------------------------------------ */

/** HEAD / state 里的 SHA 缩到 7 位（报告的阅读性；比较用全值）。 */
function shortSha(sha: string): string {
  return sha.slice(0, 7);
}

/**
 * ⑦ machine：`state.json` 与 HEAD 的关系（§2.5）。
 *
 * state 是**加速信息**（`loadState` 缺失 / 损坏都降级为 `{version:1}`，不 throw），因此：
 *   - `lastSyncCommit` 缺失 → warn（首次同步前 / state 被删）；不需要 fail——「无 base」有降级路径；
 *   - `lastSyncCommit !== HEAD` → warn（'本地有未同步 commit'）：这正是 M2 §1-D5 的 push 漂移
 *     语义，是**正常状态**而不是错误（刚 `homer push` 完 / 刚手改 store），绝不能 fail。
 */
export function checkMachine(paths: HomerPaths): DoctorCheck {
  const state = loadState(paths);
  const head = headCommit(paths.home);

  if (state.lastSyncCommit === undefined) {
    return {
      id: 'machine',
      status: 'warn',
      message: 'state.json 未记录同步状态（lastSyncCommit 缺失）',
      details: [
        `state 文件: ${paths.stateFile}`,
        '首次同步（`homer push` / `homer pull`）会自动写入；仅影响三方判定的基准。',
      ],
    };
  }

  if (head === undefined) {
    return {
      id: 'machine',
      status: 'warn',
      message: '无法解析 HEAD（仓库没有 commit？），state 与 HEAD 的关系不可判定',
      details: [`state.lastSyncCommit: ${shortSha(state.lastSyncCommit)}`],
    };
  }

  if (state.lastSyncCommit !== head) {
    return {
      id: 'machine',
      status: 'warn',
      message: '本地有未同步 commit（state.lastSyncCommit 落后于 HEAD）',
      details: [`state: ${shortSha(state.lastSyncCommit)}`, `HEAD:  ${shortSha(head)}`],
    };
  }

  return { id: 'machine', status: 'ok', message: `state 与 HEAD 一致（${shortSha(head)}）` };
}

/* ------------------------------------------------------------------ */
/* ⑧ required                                                          */
/* ------------------------------------------------------------------ */

/**
 * ⑧ required：store 中 `__REQUIRED__` 占位符残留（M2 遗留的可见化，§1-D6）。
 *
 * push 时 `excludeKeys` 列出的顶层键值会被换成 `__REQUIRED__` 占位符（密钥永不进 git），
 * 目标机拿到 store 后必须**自己补全**这些键。残留说明「某台机器的必填项还没填」——
 * 这是**用户待办**，不是故障 → warn，details 精确到「文件 + 键」：
 *   `pi/settings/settings.json: apiKeys, token`
 *
 * 读的是 store **工作区**快照（与 `homer status` 同源），因此 `store/` 尚未 init 的
 * adapter 会被 `readSnapshotFromStore` 按「空 Map」语义消化（不会误报）。store 半残
 * （缺 `.homer-complete`）会抛 `CliError`——doctor 作为诊断工具不能因此崩掉，降级为 warn。
 */
export function checkRequiredPlaceholders(paths: HomerPaths, config: HomerConfig): DoctorCheck {
  let snapshots: AdapterSnapshot[];
  try {
    snapshots = readSnapshotFromStore(paths, config);
  } catch (err) {
    return {
      id: 'required',
      status: 'warn',
      message: `store 快照不可读，跳过占位符检查: ${err instanceof Error ? err.message : String(err)}`,
    };
  }

  const details: string[] = [];
  for (const snapshot of snapshots) {
    for (const category of snapshot.categories) {
      const relPaths = [...category.files.keys()].sort();
      for (const relPath of relPaths) {
        const entry = category.files.get(relPath);
        if (entry === undefined || entry.kind !== 'json') continue;

        let value: unknown;
        try {
          value = JSON.parse(entry.content) as unknown;
        } catch {
          continue; // kind 判定来自 mode，坏 JSON 在此直接跳过（不是占位符问题的范围）。
        }
        if (!isPlainObject(value)) continue;

        const keys = Object.keys(value)
          .filter((key) => value[key] === REQUIRED_PLACEHOLDER)
          .sort();
        if (keys.length === 0) continue;

        details.push(`${snapshot.adapterId}/${category.category}/${relPath}: ${keys.join(', ')}`);
      }
    }
  }

  if (details.length === 0) {
    return { id: 'required', status: 'ok', message: '未发现 __REQUIRED__ 占位符残留' };
  }

  return {
    id: 'required',
    status: 'warn',
    message: `${details.length} 个文件残留 __REQUIRED__ 占位符（本机需补全这些必填项）`,
    details,
  };
}
