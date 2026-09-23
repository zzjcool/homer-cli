# homer-cli M3 实施计划（密钥层 + home + doctor + herdr/opencode adapter）

> 目标（DESIGN §6 M3）：age 集成、curl 安装脚本、herdr/opencode adapter —— **MVP 验收场景达成**（DESIGN §3：新机器 curl 安装 → `homer home <url>` → 配置 + 密钥全部归位）。
> 本文冻结全部新增 TS 接口；worker 只实现，不发明接口。M1/M2 冻结接口**零破坏性修改**，仅允许 §2.0 列出的 additive 变更。基线：master `10cc6ea`，750 tests 绿。

## 0. 目标与 Non-goals

**目标**：
1. **age 密钥层**：X25519 identity 生成/存放、多 recipient 加密、`secrets/` 目录 vault
2. **`homer secret keygen|push|pull|list`**：密钥安全投递（age 加密文件走 git；tailcat 是 M5 不做）
3. **`homer home <repo-url>`**：新机器一键归位（clone → 应用分类配置 → 首次对接三选一 → 密钥解密归位 → doctor）
4. **`homer doctor`**：八项体检 + M2 遗留的 `__REQUIRED__` 残留报告
5. **herdr / opencode adapter**（基于真实目录侦察）
6. **install.sh** curl 一键安装
7. **symlink 逃逸 allowlist**（M1 已知限制的产品决策：做）

**Non-goals（M3 明确不做）**：
- tailcat 快车道、`homer pair`（M5）
- **home 不装依赖**（DESIGN §2.3 的「装依赖」延后：依赖安装涉及各工具包管理器与网络副作用，风险大于收益；MVP 验收场景只需配置 + 密钥归位。记录为 M4+ 候选）
- `homer sync` 智能同步编排、pi 扩展形态（M4）
- adapter 插件机制、claude/codex/shell adapter（M5）
- **secrets/ 的三路合并/冲突**：secrets 通道无 base 概念，M3 = 整文件覆盖（后写胜）；记录为已知限制
- canonical JSON 序列化（维持 M2 决定）
- `homer unlock` / 并发锁（v2）
- 首次对接的**逐文件/逐键**冲突裁决 UI（三选一后，merge 模式的冲突文件整体保留本地，见 §1-D5）

## 1. 关键架构决策

| # | 决策 | 依据 |
|---|---|---|
| D1 | **age 包选型**：首选 npm `age-encryption`（纯 JS age 实现，X25519 recipient/identity 字符串 API）。**S0 侦察先行验证**（版本/API/ESM/Node≥20/维护状态 + /tmp 冒烟加解密）；全部代码只依赖 §2.2 冻结的 `AgeCryptoPort` 接口，包可整体替换。否决时的 fallback：`node:crypto` X25519 + age 格式规范自实现（工作量 ~2 天，仅兜底） | DESIGN §2.3/§4「age 加密考虑 age-encryption npm 实现」 |
| D2 | **私钥位置**：`<home>/keys/age.txt`（0600），`ensureGitRepo` 幂等维护 `.gitignore` 增加 `keys/` 行（与 state.json/backups/ 同模式）；私钥**永不**进 store/secrets/、任何报告、stdout/日志。公钥（recipient，`age1...`）存 `homer.json` → `secrets.recipients`（随仓库走） | §2.1（homer.json 入库）、§2.6（本地状态不入库） |
| D3 | **secrets/ 布局**：一密钥一文件，`<home>/secrets/<name>.age`（age 二进制密文，name 限 `[A-Za-z0-9][A-Za-z0-9._-]*`，扁平无子目录防逃逸）；name → 目标绝对路径映射存 `homer.json` → `secrets.files`。secrets/ **入库**（store/ 平级的第二通道） | DESIGN §2.1 布局、§2.3 独立子命令族 |
| D4 | **secret 管线边界**：`homer secret push|pull` 是独立子命令族，与 store 管线（push/pull/merge）**零耦合**——`commitAllStore` 只 add `store/`，secret 侧新增 `commitPaths(['secrets/'])`；store 密钥扫描闸门不扫描明文（明文从不进 store），但 vault 文件必须为密文（实现内断言密文不含明文片段）。`secret pull` 从 `@{upstream}`（fetch 后）读密文，不 ff 整仓（避免 store 工作区悄悄前进） | DESIGN §2.3（独立子命令）、§2.4 分层 |
| D5 | **首次对接语义**：base = **全空**（按 config 结构的空 Map）。`pull` 模式 = 远端全量覆盖（冲突项备份后写 remote 内容，**不删** local-only 文件——空 base 下“删除”无依据）；`merge` 模式 = 远端独有 → write，双方都有且不同 → 冲突整体保留本地；`skip` = 不应用。merge 保留的本地文件在 home 写入 `state.lastSyncCommit=HEAD` 后自然成为 **push 漂移**（remote 已被看见，本地 = 意图真相，与 M2 merge 完成语义同构，不触发 S4 问题） | DESIGN §2.5-4、§2.7「首次同步」、m2-report §4.2-S4 |
| D6 | **doctor 边界**：八项检查（§2.6），`fail` → exit 1，`warn` → exit 0；`--offline` 跳过远端可达性（回应 m2-report §5-3）。`__REQUIRED__` 残留 = warn 级（列出 store 中的文件+键），落地 DESIGN §2.7 | DESIGN §2.3、§2.7 |
| D7 | **symlink 逃逸 allowlist**：`AdapterConfig.allowEscape?: string[]`（相对 root 的 glob，`matchesIgnore` 语义）。默认缺省 = 维持 M1 安全边界（跳过 + ScanError）；命中 allowlist → 跟随（回环检测/悬空跳过行为不变）。显式 opt-in，不做全局放开 | M1 已知限制（agent-browser 逃逸链接）、M2 Non-goals 留 M3 |
| D8 | **herdr/opencode adapter 分类**基于本机真实目录侦察（见 §1.1），S0-scout 复核后冻结为默认配置 | DESIGN §2.2、任务要求 |

### 1.1 真实目录侦察结论（planner 已实测，S0-scout 负责复核与补全）

**herdr `~/.config/herdr`**（实测）：`config.toml`（用户设置：onboarding/theme/ui —— 可同步）、`session.json`（**运行时状态**：workspace/pane 布局、机器本地 cwd 与 session 路径 —— 忽略）、`herdr.sock`/`herdr-client.sock`（Unix socket —— 忽略）、`*.log`（忽略）、`.plugins.lock`（忽略）、`release-notes.json`（版本提示，机器相关 —— 忽略）。

**opencode `~/.config/opencode`**（实测）：`opencode.json`（主配置，JSON；内含 `provider.*.options.apiKey` —— 短占位值不触发扫描，真实长 key 由 M2 密钥扫描闸门拦截 → merge 模式安全）、`package.json`（插件依赖，JSON）、`package-lock.json`/`bun.lock`（锁文件）、`node_modules/`（忽略）、`.plugins.lock`（忽略）、`.gitignore`（忽略）。

## 2. 冻结接口（全部签名；各 worker 逐字落地，不得改动）

### 2.0 M1/M2 冻结接口的 additive 变更（仅此 5 处，P0-M0 独家执行）

1. `src/core/types.ts`：
```ts
export interface SecretsConfig {
  ignorePaths?: string[];              // （M2 已有）
  /** age X25519 recipients（age1...），secret push 的加密目标。 */
  recipients?: string[];
  /** secret 名 -> 目标路径（必须 '~' 或 '/' 开头；'~' 用 expandHome 展开）。name 须过 secretNameValid。 */
  files?: Record<string, string>;
}
// AdapterConfig 增加可选字段（不改既有字段）：
//   allowEscape?: string[];   // 相对 root 的 glob（matchesIgnore 语义）；命中 = 允许该 symlink 逃逸 root 并跟随
```
2. `src/core/paths.ts`：`HomerPaths` 增 `secretsDir: string`（=`<home>/secrets`）与 `keysDir: string`（=`<home>/keys`）。
3. `src/core/config.ts`：`validateConfig` additive 校验 `secrets.recipients`（每项 `recipientIsValid`）、`secrets.files`（name 过 `secretNameValid`、值以 `~`/`/` 开头）、`adapters.*.allowEscape`（string[]），缺省一律合法。
4. `src/core/secrets/patterns.ts`：`SECRET_PATTERNS` **追加第 13 条** `AGE-SECRET-KEY-[a-z0-9]{20,}`（age 私钥误入 store 拒推；数组追加，不动既有 12 条）。
5. `src/core/git/git.ts`：`ensureGitRepo` 维护的 `.gitignore` 模板增加 `keys/` 行（幂等，已有行不重复追加）。

### 2.1 `src/cli/args.ts` / `src/cli/index.ts`（P0-M0 独家，此后任何 worker 禁改）

`COMMANDS` 增 `'home' | 'doctor' | 'secret'`；USAGE 增三命令条目；`index.ts` 增三个 dispatch（`secret` 的子命令 keygen/push/pull/list 由 `secret.ts` 内部解析 `rest[0]`，未知子命令 → usageError）。新命令 flag 表：

| 命令 | flag |
|---|---|
| `homer home <repo-url>` | `--home <dir>` `--mode pull\|merge\|skip` `--yes` `--json` `-h` |
| `homer doctor` | `--home <dir>` `--offline` `--json` `-h` |
| `homer secret keygen` | `--home <dir>` `--json` `-h` |
| `homer secret push` | `--home <dir>` `--yes` `--no-push` `--json` `-h` |
| `homer secret pull` | `--home <dir>` `--yes` `--json` `-h` |
| `homer secret list` | `--home <dir>` `--json` `-h` |

### 2.2 `src/core/age/`（P0 落类型，P1-W1 实现）

```ts
// ---- types.ts（P0 落地，冻结）----
export interface AgeIdentity {
  secretKey: string;   // 'AGE-SECRET-KEY-1...'，仅存在于内存与 <keysDir>/age.txt（0600）
  recipient: string;   // 派生公钥 'age1...'
}
export interface AgeCryptoPort {
  encrypt(plaintext: Buffer, recipients: readonly string[]): Promise<Buffer>;
  decrypt(ciphertext: Buffer, identity: AgeIdentity): Promise<Buffer>;
}
export function createAgeCryptoPort(): AgeCryptoPort;   // 底层实现（age-encryption 包）唯一封装点

// ---- keys.ts（W1）----
export function generateIdentity(): AgeIdentity;                    // 纯生成，不落盘；secretKey 永不进错误消息
export function identityFilePath(paths: HomerPaths): string;        // <keysDir>/age.txt
export function writeIdentityFile(paths: HomerPaths, identity: AgeIdentity): void;
// 拒绝覆盖已存在文件（CliError）；0600；tmp+rename 原子写
export function loadIdentity(paths: HomerPaths): AgeIdentity | undefined; // 缺失/损坏 → undefined，不 throw
export function parseIdentityFile(content: string): AgeIdentity;   // 校验前缀；非法 → CliError（消息不含内容本体）
export function recipientIsValid(recipient: string): boolean;      // /^age1[02-9ac-hj-np-z]{58}$/

// ---- vault.ts（W1）----
export function secretNameValid(name: string): boolean;            // /^[A-Za-z0-9][A-Za-z0-9._-]*$/
export function secretFilePath(paths: HomerPaths, name: string): string; // <secretsDir>/<name>.age；非法 name → throw
export async function encryptSecretToFile(
  crypto: AgeCryptoPort, paths: HomerPaths, name: string,
  plaintext: Buffer, recipients: readonly string[],
): Promise<void>;   // 原子写；断言密文不含明文子串（>16 字节样本）
export async function decryptSecretFromFile(
  crypto: AgeCryptoPort, paths: HomerPaths, name: string,
): Promise<Buffer>;  // 文件缺失 → CliError
export interface VaultEntryStatus { name: string; destination: string; vaultFile: 'present' | 'missing'; }
export function listSecrets(paths: HomerPaths, config: HomerConfig): VaultEntryStatus[];
```

### 2.3 `src/core/git/`（P2-W6 additive）

```ts
export function commitPaths(home: string, pathspecs: readonly string[], message: string): string | undefined;
// git add -- <pathspecs...> + commit；无可提交变更 → undefined；不触碰未列入 pathspec 的路径
export function readVaultFileAtCommit(home: string, relPath: string, commitish: string): Buffer | undefined;
// git show <commitish>:<relPath>（binary）；路径不存在 → undefined
```

### 2.4 `src/core/sync/first-sync.ts`（P1-W5，纯函数，只依赖 types/engine/sync-types）

```ts
export type FirstContactMode = 'pull' | 'merge' | 'skip';
export function emptyBaseSnapshots(config: HomerConfig): AdapterSnapshot[];  // 按 config 结构的全空 Map
export interface FirstContactPlan { mode: FirstContactMode; actions: PullAction[]; }
export function planFirstContact(
  config: HomerConfig,
  local: AdapterSnapshot[],
  remote: AdapterSnapshot[],     // home 场景 = clone 后 store 工作区（== HEAD）
  mode: FirstContactMode,
): FirstContactPlan;
```

**语义（冻结）**：
- `pull`：remote 每个文件 → `write`（含双方都有且不同的项，`content` = remote 内容）；local-only 文件**不产生任何动作**（无 delete）；skip `__REQUIRED__` 检查不适用（secret 通道独立）。
- `merge`：`planPull(config, emptyBase, local, remote)` 原样（remote-only → write；both-exist differ → conflict 保留本地；merge JSON 键级并集冲突进 conflict action）。
- `skip`：`actions: []`。

### 2.5 `src/core/doctor/`（P2-W7）

```ts
// ---- checks.ts（纯函数 + 薄 fs/git，可注入）----
export type CheckStatus = 'ok' | 'warn' | 'fail';
export type DoctorCheckId =
  | 'config' | 'repo' | 'store-clean' | 'remote' | 'adapters' | 'age' | 'machine' | 'required';
export interface DoctorCheck { id: DoctorCheckId; status: CheckStatus; message: string; details?: string[]; }
export interface DoctorReport { checks: DoctorCheck[]; ok: boolean; }   // ok = 无 'fail'

export function checkConfig(paths: HomerPaths): DoctorCheck;                 // 缺失/非法 → fail
export function checkRepoAndStore(paths: HomerPaths): DoctorCheck[];         // ['repo','store-clean']；非 git 仓库 → repo fail；store 脏 → store-clean warn
export function checkRemote(paths: HomerPaths, opts: { offline: boolean }): DoctorCheck;
// offline=true → ok（'已跳过'）；否则 git ls-remote --heads <upstream url>（timeout 10s）失败 → warn（离线/不可达）
export function checkAdapters(config: HomerConfig): DoctorCheck;             // 逐 enabled adapter root 存在性；缺失 → warn（'工具未安装？'）+ details
export function checkAge(paths: HomerPaths, config: HomerConfig, crypto: AgeCryptoPort): Promise<DoctorCheck>;
// 未配置 secrets.files/recipients → ok（'未配置密钥同步'）；
// 配置了：identity 缺失 → fail；recipients 空 → fail；有 vault 文件且全部试解密（逐个 decryptSecretFromFile），
// 任一失败 → fail（提示'本机 identity 非 recipient，需在旧机加 recipient 后重新 secret push'）
export function checkMachine(paths: HomerPaths): DoctorCheck;                // state 缺失 → warn；lastSyncCommit != HEAD → warn（'本地有未同步 commit'）
export function checkRequiredPlaceholders(paths: HomerPaths, config: HomerConfig): DoctorCheck;
// 读 store 快照：JSON 顶层值 === '__REQUIRED__' 的键 → warn + details（'pi/settings/settings.json: apiKeys'）；无 → ok

// ---- src/cli/commands/doctor.ts ----
export interface DoctorOptions { homerHome?: string; json?: boolean; offline?: boolean; }
export interface DoctorDeps { age?: AgeCryptoPort; }
export const DOCTOR_USAGE: string;
export function runDoctor(opts: DoctorOptions, deps?: DoctorDeps): Promise<DoctorReport>;
// 顺序：config → repo/store-clean → remote → adapters → age → machine → required；
// config fail 时仍继续其余检查（尽力而为），ok=false。exit：无 fail → 0；有 fail → 1
```

### 2.6 `src/cli/commands/secret.ts`（P0 stub，P2-W6 实现）

```ts
export type SecretSubcommand = 'keygen' | 'push' | 'pull' | 'list';
export interface SecretDeps {
  ui?: PromptPort;
  age?: AgeCryptoPort;
  git?: Partial<GitPort>;          // 复用 src/cli/commands/git-port.ts
}
export const SECRET_USAGE: string;

// ---- keygen ----
export interface SecretKeygenOptions { homerHome?: string; json?: boolean; }
export interface SecretKeygenReport { ok: boolean; identityFile: string; recipient: string; created: boolean; }
// generateIdentity → writeIdentityFile（已存在 → CliError exit 1，提示路径；绝不覆盖）；
// 输出只含 recipient（私钥永不回显）。exit：成功 → 0
export async function runSecretKeygen(opts: SecretKeygenOptions): Promise<SecretKeygenReport>;

// ---- push ----
export interface SecretPushOptions { homerHome?: string; json?: boolean; yes?: boolean; noPush?: boolean; }
export interface SecretPushReport {
  ok: boolean;
  status: 'pushed' | 'no-secrets' | 'no-identity' | 'no-recipients' | 'missing-source' | 'aborted' | 'error';
  encrypted: string[];             // secret 名清单
  commit?: string;
  pushedToRemote: boolean;
  warnings: string[]; errors: string[];
}
export async function runSecretPush(opts: SecretPushOptions, deps?: SecretDeps): Promise<SecretPushReport>;

// ---- pull ----
export interface SecretPullOptions { homerHome?: string; json?: boolean; yes?: boolean; }
export interface SecretPullReport {
  ok: boolean;
  status: 'applied' | 'no-secrets' | 'no-identity' | 'missing-vault' | 'undecryptable' | 'aborted' | 'error';
  pulled: string[];
  backupDir?: string;
  warnings: string[]; errors: string[];
}
export async function runSecretPull(opts: SecretPullOptions, deps?: SecretDeps): Promise<SecretPullReport>;

// ---- list ----
export interface SecretListOptions { homerHome?: string; json?: boolean; }
export interface SecretListReport { secrets: VaultEntryStatus[]; }
export function runSecretList(opts: SecretListOptions): SecretListReport;
```

**push 流程（冻结）**：load config → `secrets.files` 空 → `no-secrets` exit 0 → `loadIdentity` 缺失 → `no-identity` exit 1（提示 `homer secret keygen`）→ `recipients` 空 → `no-recipients` exit 1 → **先读全部** destination 明文（任一缺失 → `missing-source` exit 1，**全有或全无**，未写任何 vault 文件）→ 逐项 `encryptSecretToFile`（全部 recipients）→ 非 `--yes` 时 ui.confirm（列出 name→destination 清单），拒绝 → `aborted` exit 1 → `commitPaths(home, ['secrets/'], 'homer secret push: 更新 N 个密钥')` → 有 push target 且非 `--no-push` → `gitPush`（失败 → warning + exit 1，同 push 语义）。**vault 写入不经过 store 扫描闸门（明文不进 store），但 git 对象中只有密文**（e2e 用 `git grep` 明文片段断言为空）。

**pull 流程（冻结)**：load config → files 空 → `no-secrets` exit 0 → identity 缺失 → `no-identity` exit 1（提示 keygen；若从旧机器迁移，提示在旧机把本机 recipient 加入后重新 push）→ `gitFetch`（失败 → warning，回落读**工作区** vault；成功且 upstream 可解析 → 逐项 `readVaultFileAtCommit(home, 'secrets/<name>.age', upstreamRef)`，无 upstream → 工作区）→ 配置的 secret 任一 vault 缺失 → `missing-vault` exit 1（details 列出）→ 逐项解密（任一失败 → `undecryptable` exit 1，**不写任何目标文件**）→ 非 `--yes` 时 confirm（name→destination）→ 已存在的目标先 `backupFiles`（label = `secret/<name>`）→ 写入目标（父目录 mkdir -p，**0600**）→ `applied` exit 0。

**退出码总表**：keygen：成功→0 / 已存在→1；push：pushed/no-secrets→0，其余→1；pull：applied/no-secrets→0，其余→1；list：0。

### 2.7 `src/cli/commands/home.ts`（P0 stub，P3-W8 实现）

```ts
export interface HomeOptions {
  homerHome?: string;              // 目标工作区（默认 ~/.homer）；目录必须不存在或为空
  repoUrl: string;                 // 位置参数（必填）
  json?: boolean;
  yes?: boolean;
  mode?: FirstContactMode;         // --mode；缺省 + --yes → 'merge'（最安全默认）
}
export interface HomeDeps {
  ui?: PromptPort;
  age?: AgeCryptoPort;
  git?: Partial<GitPort>;
  /** clone 注入位（测试用假 origin）。缺省 = execFile('git', ['clone', url, dest], timeout 60s)。 */
  clone?: (repoUrl: string, destDir: string) => Promise<void>;
}
export interface HomeReport {
  ok: boolean;
  status: 'homed' | 'aborted' | 'error';
  cloned: boolean;
  adapterIds: string[];
  firstContact: { mode: FirstContactMode; applied: ApplyResult; conflicts: PullConflictAction[] } | undefined;
  secrets: { pulled: string[]; skipped: string[]; errors: string[] };
  doctor: DoctorReport | undefined;
  warnings: string[]; errors: string[];
}
export const HOME_USAGE: string;
export async function runHome(opts: HomeOptions, deps?: HomeDeps): Promise<HomeReport>;
```

**home 流程（冻结）**：
1. `resolveHomerPaths`；home 目录存在且非空 → CliError（提示手工处理或 `--home` 另指定）。
2. `deps.clone`（默认真 git clone，timeout 60s，失败 → CliError 含 stderr 摘要）。
3. `loadConfig`（clone 下来的 homer.json）；缺失/非法 → CliError「该仓库不是 homer 配置中心」。
4. `scanAdapter` 逐 enabled adapter（本机现状，可为空/缺 root——沿 M-A 守卫）。
5. remote = `readSnapshotFromStore`（clone 后工作区 == HEAD）。
6. **首次对接三选一**：`--mode` 给定 → 用之；`--yes` 无 mode → `merge`；否则 `ui.select`（Pull=远端覆盖 / Merge=合并保留本地 / Skip=暂不应用），非 TTY 且无 mode/yes → CliError（提示 `--mode` 或 `--yes`）。
7. `planFirstContact` → 非 skip 且有动作 → 非 `--yes` 时 confirm（write/conflict 计数预览）→ `applyPullActions`（backup=true）。
8. `saveState`：`lastSyncCommit = HEAD`、`lastSyncCommand = 'pull'`、`lastSyncAt` = now（remote 已被看见；merge 模式保留的本地文件成为 push 漂移，见 §1-D5）。
9. **密钥归位**：`secrets.files` 非空 → `loadIdentity`；缺失 → `secrets.skipped` 全量 + warning（提示两步：本机 `homer secret keygen` → 旧机把新 recipient 加入 `secrets.recipients` 后 `homer secret push` → 本机 `homer secret pull`）。有 identity → 逐项从**工作区** vault 解密（流程/失败语义同 secret pull：全有或全无；undecryptable → `secrets.errors` + warning，**不 fail 整个 home**——配置已归位，密钥可后补）→ 备份已存在目标 → 写入 0600。
10. `runDoctor`（透传 deps.age）→ 报告附于 `doctor`；`homed` exit 0（doctor 的 fail 在报告中呈现，不改变 home 退出码；doctor 独立运行时才 exit 1）。

### 2.8 `src/adapters/herdr/` 与 `src/adapters/opencode/`（P1-W2；初案如下，**以 S0-scout 报告为准冻结**）

```ts
// src/adapters/herdr/defaults.ts
export const HERDR_ADAPTER_ID = 'herdr';
export const DEFAULT_HERDR_ADAPTER: AdapterConfig = {
  root: '~/.config/herdr', enabled: true,
  categories: { config: { paths: ['config.toml'], mode: 'mirror' } },
  ignore: ['session.json', '*.sock', '*.log', '.plugins.lock', 'release-notes.json'],
};
// src/adapters/herdr/index.ts：export { scanHerdr? } —— 直接 re-export 通用 scanAdapter（从 ../pi/index.js import，签名本就通用）

// src/adapters/opencode/defaults.ts
export const OPENCODE_ADAPTER_ID = 'opencode';
export const DEFAULT_OPENCODE_ADAPTER: AdapterConfig = {
  root: '~/.config/opencode', enabled: true,
  categories: {
    config:  { paths: ['opencode.json'], mode: 'merge' },
    plugins: { paths: ['package.json'], mode: 'merge' },
    locks:   { paths: ['package-lock.json', 'bun.lock'], mode: 'mirror' },
  },
  ignore: ['node_modules/', '.plugins.lock', '*.log', '.gitignore'],
};
```

**安全注记（冻结）**：`opencode.json` 的 `provider.*.options.apiKey` 为嵌套键，M1 冻结的 `excludeKeys` 只支持顶层——不做引擎扩展（breaking 风险）。真实长 key 由 M2 密钥扫描闸门（pattern 12）在 push 时拦截，短占位值（如 `"ccrb"`）不命中；README 说明「真实 key 请走 secret 通道 + `secrets.ignorePaths` 豁免或改环境变量引用」。

### 2.9 `install.sh`（P1-W4，仓库根目录）

```sh
#!/bin/sh
# homer-cli 一键安装（curl -fsSL .../install.sh | sh）
# 可测性注入位（env）：
#   HOMER_INSTALL_PACKAGE  默认 homer-cli@latest（测试传 npm pack 的 tarball 路径）
#   HOMER_INSTALL_PREFIX   默认继承 npm 全局 prefix（测试传 NPM_CONFIG_PREFIX）
set -eu
# 1. 检测 node >= 20（command -v node + 版本比较）；缺失 → 打印安装指引 + exit 1
# 2. 检测 npm；npm install -g "$HOMER_INSTALL_PACKAGE"
# 3. 冒烟：homer --help（PATH 查找，找不到时提示 prefix/bin 加入 PATH）
# 4. 打印下一步：homer home <你的配置仓库 url>
```

## 3. 波次拆分 / 文件归属 / 依赖 / 验收

```
P0（并行 2）──┬─ S0-scout     （只读侦察：herdr/opencode 目录 + age-encryption 包调研 → docs/m3-scout-report.md）
              └─ M0-scaffold （repo 变更：§2.0 additive + §2.1 dispatch + 3 命令 stub + 依赖安装）
P1（并行 5，依赖 M0；W2 另依赖 S0 报告）──┬─ W1-age        (src/core/age/)
                                          ├─ W2-adapters   (src/adapters/herdr/ opencode/)
                                          ├─ W3-allowlist  (src/adapters/pi/scan.ts + src/core/config.ts)
                                          ├─ W4-install    (install.sh)
                                          └─ W5-firstsync  (src/core/sync/first-sync.ts)
P2（并行 2，依赖 P1）──┬─ W6-secret   (src/cli/commands/secret.ts + src/core/git additive)
                       └─ W7-doctor   (src/core/doctor/ + src/cli/commands/doctor.ts)
P3（串行，依赖 P2）──── W8-home      (src/cli/commands/home.ts)
P4（串行收尾）────────── W9-integrator（init 注册新 adapter + tests/e2e/m3.test.ts + README + 报告 + 对抗 review）
```

### P0 · S0-scout（与 M0 并行；只读，不改 repo）

- **任务**：① 复核 §1.1 侦察结论（`ls -la ~/.config/herdr ~/.config/opencode`、查两工具文档确认是否有其他可同步目录如 `~/.herdr/`、herdr 主题/agent 定义位置），修正/确认 §2.8 初案——**W2 的分类以本报告为冻结依据**；② 调研 `age-encryption` npm 包：`npm view age-encryption`（版本/维护）、/tmp 下 ESM smoke（X25519 recipient 字符串加密 → identity 字符串解密 roundtrip、从 secretKey 派生 recipient 的 API 是否存在）、Node ≥20 兼容；结论写「可用 / 需 fallback」。③ 顺带确认本机是否有 `age` CLI（供 W1 互操作测试）。
- **产出**：`docs/m3-scout-report.md`。
- **验收**：报告含两工具的文件清单 + 分类裁定表 + age 包 API 验证代码片段与结论。

### P0 · M0-scaffold（串行 repo 变更）

- **文件**：`package.json`（+`age-encryption` 运行时依赖，S0 否决时由 orchestrator 修正）、`src/core/types.ts`（§2.0-1）、`src/core/paths.ts`（§2.0-2）、`src/core/config.ts`（§2.0-3 校验 + 引用 §2.2 的 `secretNameValid`/`recipientIsValid` —— 两函数在 `src/core/age/types.ts` 由 P0 以**纯校验实现**落地，W1 落地其余）、`src/core/age/types.ts`（P0 落地全部类型 + `secretNameValid`/`recipientIsValid`/`createAgeCryptoPort` stub throw）、`src/core/secrets/patterns.ts`（+第 13 条 + 测试）、`src/core/git/git.ts`（.gitignore +`keys/`）、`src/cli/args.ts` + `src/cli/index.ts`（§2.1，此后 P1-P4 禁改）、`src/cli/commands/{home,doctor,secret}.ts`（§2.5-2.7 接口逐字落地 + stub throw CliError）。
- **验收**：`npm install && npm run typecheck && npm test` 全绿（750 + patterns 新例）；`node bin/homer.js secret` 打印 usage；`--help` 含三新命令；`git diff src/core/types.ts` 仅含 additive 字段。**commit 检查点。**

### P1 · 五个并行 worker（目录互斥，均只依赖 P0 + 既有代码）

**W1-age**：文件 `src/core/age/{cipher.ts,keys.ts,vault.ts,index.ts}` + `tests/core/age/*.test.ts`。
验收：keygen→write→load roundtrip（recipient 一致、文件 mode 0600）；重复写入拒绝覆盖；`parseIdentityFile` 非法输入 → CliError 且消息无私钥本体；encrypt/decrypt **多 recipient**（2 个 identity 各自可解）roundtrip；非法 recipient → throw；vault 原子写 + 密文不含明文子串断言；`listSecrets` present/missing。**硬约束：测试 identity 一律 `generateIdentity()` 临时生成 + mkdtemp，绝不落真实 HOME；若本机有 `age` CLI（S0 结论），加互操作用例（age CLI 加密 → homer 解密），无则跳过。**

**W2-adapters**（依赖 S0 报告）：文件 `src/adapters/{herdr,opencode}/{defaults.ts,index.ts}` + `tests/adapters/{herdr,opencode}/*.test.ts`（fixture 用 mkdtemp 假 HOME，**禁碰真实 ~/.config**）。
验收：fixture 按冻结分类构造（herdr：config.toml + session.json/sock/log 垃圾；opencode：三分类 + node_modules 垃圾）→ 快照精确匹配（垃圾 0 条、JSON 分类 kind 正确）；root 缺失 → 空 snapshot + errors 不 throw；默认配置对象与 §2.8/S0 冻结版逐字一致（快照测试）。

**W3-allowlist**：文件 `src/adapters/pi/scan.ts`（escape 分支）+ `src/core/config.ts`（additive 校验，注意与 M0 的 §2.0-3 已落地部分合并去重）+ `tests/adapters/pi/scan-allowescape.test.ts` + `tests/core/config.test.ts` 增例。
验收：逃逸 symlink 无 allowlist → 跳过 + ScanError（既有 12 例回归全绿）；`allowEscape: ['skills/agent-browser']` → 跟随且内容入快照；allowlist 未命中路径 → 仍跳过；回环截断/悬空跳过行为不变；validateConfig 对 allowEscape 非法值报错。

**W4-install**：文件 `install.sh` + `tests/e2e/install.test.ts`。
验收：`sh -n install.sh` 语法通过；`npm run build && npm pack` → tarball → `HOMER_INSTALL_PACKAGE=<tarball> NPM_CONFIG_PREFIX=$(mktemp -d) sh install.sh` → `$PREFIX/bin/homer --help` exit 0；PATH 无 node（注入假 PATH）→ exit 1 + 安装指引文本；幂等（二跑不炸）。

**W5-firstsync**：文件 `src/core/sync/first-sync.ts` + `src/core/sync/index.ts` barrel 增导出 + `tests/core/sync/first-sync.test.ts`。纯函数。
验收：三模式语义矩阵（§2.4）逐条：pull = remote 全量 write + 无 delete + local-only 无动作；merge = remote-only write + both-exist-differ → conflict（mirror 与 merge-JSON 两类）+ 键级并集冲突 keyPaths 正确；skip = 空；`emptyBaseSnapshots` 结构与 config 对齐（复用 M1 裁定的「空 Map」语义）。

### P2 · 两个并行 worker（依赖 P1 全部）

**W6-secret**：文件 `src/cli/commands/secret.ts` + `src/core/git/{git.ts,reader.ts,index.ts}`（§2.3 additive）+ `tests/cli/secret.test.ts` + `tests/core/git/` 增例。
验收（全部注入 `deps.age`（真 port + 临时 identity）+ 假 origin 临时仓库，绝不碰真实 HOME）：
- keygen：文件 0600、输出/`--json` 只含 recipient、重复 keygen exit 1 不覆盖；
- push：多 recipient 密文、`git grep <明文片段>` 在 bare origin 为空（密文入库铁证）、missing-source 全有或全无（未写任何 vault）、no-identity/no-recipients exit 1、`--no-push`、commit 落 `secrets/` 而 store/ 零变更；
- pull：从 upstream 读密文（构造 origin 领先于本地工作区的场景）、解密写目标 0600 + 备份内容 == 覆盖前、undecryptable（用非 recipient 的 identity）exit 1 且零写入、fetch 失败回落工作区；
- list + `--json` 形状。

**W7-doctor**：文件 `src/core/doctor/{checks.ts,index.ts}` + `src/cli/commands/doctor.ts` + `tests/core/doctor/*.test.ts` + `tests/cli/doctor.test.ts`。
验收：八项检查各自 ok/warn/fail 至少一例（config 缺失/损坏；非 git 仓库；store 脏；offline 跳过 + 离线 warn；adapter root 缺失 warn；age 未配置 ok / identity 缺失 fail / 试解密失败 fail；state 缺失或领先 warn；`__REQUIRED__` 残留 warn 且 details 精确）；`--json` 可 parse；exit 码（有 fail → 1，仅 warn → 0）；age 检查注入 fake `AgeCryptoPort` 可测。

### P3 · W8-home（串行，依赖 P2）

- **文件**：`src/cli/commands/home.ts` + `tests/cli/home.test.ts`。
- **验收**（假 HOME + `git init --bare` 假 origin，`deps.clone` 注入或真 clone）：
  - 全流程：A 侧构造 origin（init→push→secret push）→ B `runHome({repoUrl, mode:'merge'})` → 工具目录 == store 内容、密文解密归位（内容 == A 明文、0600）、`state.lastSyncCommit == HEAD`、doctor 报告附于结果；
  - 三模式：pull（远端覆盖 + local-only 保留 + 备份存在）、merge（冲突保留本地 → 随后 `status` 出现 ↑ 漂移 —— D5 语义锁定）、skip（零应用但 config/state 就位）；
  - 交互：fake PromptPort 记录 select 调用；非 TTY 无 mode/yes → CliError；`--yes` 默认 merge；
  - 边界：home 目录非空 → CliError；clone 失败 → CliError 含摘要；仓库无 homer.json → CliError；identity 缺失 → 配置归位 + secrets.skipped + warning、整体仍 `homed` exit 0。

### P4 · W9-integrator（串行收尾）

- **文件**：`src/cli/commands/init.ts`（**仅** `KNOWN_ADAPTERS` 注册 herdr/opencode 两行 + init 测试增例）、`tests/e2e/m3.test.ts`、`README.md`、`docs/m3-report.md`、缝隙修复（接口问题上报不私改）。
- **MVP 验收场景 e2e（DESIGN §3 主线，全程 mkdtemp 双假 HOME + 假 origin + 子进程跑 CLI）**：
  1. **机器 A 装配**：fixture 三工具（pi/herdr/opencode）→ `homer init --json`（三 adapter 全注册）→ `homer secret keygen` → homer.json 写入 `secrets.recipients` + `secrets.files`（目标 = 假 HOME 下某路径）→ 明文落目标 → `homer secret push --yes` → `homer push --yes` → origin 齐备（store/ + secrets/ + homer.json）。
  2. **机器 B 一键归位（核心场景）**：全新假 HOME → `homer home <origin-url> --yes` → 断言：三工具目录逐字节 == A 侧 store、密钥明文 == A 侧明文且 0600、`homer doctor --json` 无 fail、`homer status` 无漂移。
  3. **换设备 = 加 recipient + 重加密**（DESIGN §2.6）：B `secret keygen` → A 把 B 公钥追加 recipients → A `secret push` → 密文 commit 变更 → B `secret pull --yes` 成功（B 自己的 identity 解自己的 + A 的均成功）。
  4. **__REQUIRED__ 残留进 doctor**：A 侧 models.json excludeKeys 走 push 后 store 含占位符 → `homer doctor` 出现 required warn（M2 遗留关闭）。
  5. **symlink 逃逸 allowlist**：pi fixture 加逃逸链接 + `allowEscape` → init/status 快照包含其内容（M1 已知限制关闭）。
  6. **install.sh 冒烟**：引用 W4 用例（npm pack → 安装 → homer --help）。
  7. **真实环境只读冒烟（可选手动）**：`HOMER_HOME=/tmp/... homer init --json` 应扫出 pi+herdr+opencode 三 adapter，`homer doctor` 报告 age 未配置 ok。
- 里程碑 Done 后按 AGENTS.md 起三路 fresh-context 对抗 review（正确性/测试覆盖/简洁性），修复后写 `docs/m3-report.md`。

## 4. 风险与回滚点

1. **age-encryption 包不符预期**（最易错）：API 不支持从 secretKey 派生 recipient、或质量差。对策：`AgeCryptoPort` 单点隔离（§2.2）+ S0 前置调研 + 文档化 fallback（node:crypto X25519 + age 格式自实现，仅换 `cipher.ts` 一个文件）。回滚点：S0 报告 → orchestrator 修订 W1 任务书。
2. **私钥泄漏面**：keys/ 误入 git、私钥进日志/报告、测试残留。对策：`.gitignore` keys/（M0）+ push 闸门第 13 条 pattern（AGE-SECRET-KEY）+ 接口层「secretKey 永不进错误消息/报告」+ 全部测试临时生成 identity + e2e 断言 bare origin `git grep` 无明文无私钥。
3. **first-contact 与 M2 base 语义缠结**（S4 同类风险）：home 写 state=HEAD 后 merge 模式保留的本地文件变成 push 漂移——这是**特性**（D5），但若 worker 误把冲突文件删掉或误报 delete 就是灾难。对策：`planFirstContact` 纯函数语义矩阵冻结（§2.4）+ W5 独立测试锁定 + W8 e2e「pull 模式不删 local-only」专项。
4. **doctor/home 与并行 worker 的接缝**：home 依赖 secret+doctor 的实现而非仅类型 → 放 P3 串行；doctor 的 age 检查经 `AgeCryptoPort` 注入，与 W1 解耦。

**回滚点**：M0 commit（additive 类型 + dispatch + stub）；P1 各模块独立 commit；P2 两命令独立 commit；W8/W9 各一。任一 worker 失败 2 次重试后回滚该模块 commit、降级为 orchestrator 串行修复，不阻塞波次内其他 worker。`types.ts`/`index.ts`/`args.ts` 超出 §2.0/§2.1 的变更一律上报 orchestrator 统一修订。

## 5. 机器可验收标准（M3 Done 定义）

```bash
npm run typecheck && npm test          # 全量绿（750 基线 + M3 全部新增，e2e 含 §3-P4 MVP 七组）
sh -n install.sh                       # 语法通过
npm run build && npm pack                                              # tarball 产出
HOMER_INSTALL_PACKAGE=$(ls homer-cli-*.tgz) NPM_CONFIG_PREFIX=$(mktemp -d) sh install.sh   # 安装成功
# MVP 主线（tests/e2e/m3.test.ts 自动化）：
#   机器 A：init(pi+herdr+opencode) → secret keygen → secret push → push
#   机器 B：homer home <origin> --yes → 三工具配置逐字节归位 + 密钥明文归位(0600) + doctor 无 fail + status 零漂移
#   换设备：B keygen → A 加 recipient 重加密 push → B secret pull 成功
#   bare origin: git grep 明文片段 = 空、无私钥字符串
```

外加人工核验：`homer doctor`（真实环境）输出可读、`~/.homer/keys/age.txt` 权限 600、真实 `~/.config/herdr`/`~/.config/opencode` 除显式 apply 外零写入。

---

以上为计划全文（约 11 个模块：S0/M0 + P1×5 + P2×2 + W8 + W9）。

**完成报告**：我的工具集为只读（read/grep/find/ls），无法直接写 `docs/m3-plan.md`，已将完整计划全文置于上方报告主体，请主 agent 原样落盘到 `/root/code/homer-cli/docs/m3-plan.md`。M3 计划共 **11 个模块，其中 9 个可在 4 个并行波次中并行执行**（P0 两路：S0-scout + M0-scaffold；P1 五路：age/adapters/allowlist/install/firstsync；P2 两路：secret/doctor），关键决策：**age 包选型走 `AgeCryptoPort` 单点封装 + S0 前置调研 age-encryption（fallback node:crypto 自实现）；secret 管线为独立子命令族（secrets/ 一密钥一 .age 文件、私钥在 gitignored 的 `<home>/keys/`、与 store 管线经 `commitPaths(['secrets/'])` 完全解耦）；home 范围 = clone+配置归位+密钥解密+doctor，不装依赖，首次对接空 base 三选一（merge 保留本地 → push 漂移语义）。**
