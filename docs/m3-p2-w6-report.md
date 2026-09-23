# M3 · P2-W6-secret 报告

> 分支 `m3-p2-w6-secret`，commit `4a1bc17`（parent = `b14b2b2`，即任务书给的 master 基线）。
> 计划依据：`docs/m3-plan.md` §2.3 / §1-D2 / §1-D3 / §1-D4 / §2.5 / §2.6 / §3-P2-W6。

## 1. 做了什么

### 1.1 `src/core/git/`（§2.3 additive，两个新函数）

| 函数 | 文件 | 语义 |
|---|---|---|
| `commitPaths(home, pathspecs, message)` | `git.ts` | `git status --porcelain -- <pathspecs>` 空 → `undefined`；否则 `add -A --` + `commit -m … --`（**只覆盖 pathspec**），返回新 HEAD SHA。非仓库 / 缺 git 身份 / 空 pathspec → `undefined`（不抛）。 |
| `readVaultFileAtCommit(home, relPath, commitish)` | `reader.ts` | `git show <commitish>:<relPath>` 的**二进制**读取，返回 `Buffer`；路径不存在 / commit 不可解析 / 非仓库 → `undefined`。 |

两处实现细节（计划未明说、但决定正确性，已写入代码注释）：

1. **变更检测必须先于 `git add`**：`git add -- secrets/` 在 `secrets/` 目录尚不存在时会以
   「pathspec did not match」`fatal` 失败，而「还没有任何 vault 文件」是合法状态。
   加 `git status --porcelain` 前置判定后，`commitPaths` 在空 vault 下安全返回 `undefined`。
2. **`readVaultFileAtCommit` 不走 `gitExec`**：`gitExec` 用 `encoding: 'utf8'`，而 `secrets/*.age`
   是 age 二进制密文，UTF-8 解码会把非法字节置换为 U+FFFD（不可逆）→ 解密必然失败。
   故这条读取单独用无 `encoding` 的 `execFileSync`（返回 Buffer），是该模块唯一不经过 `gitExec`
   的读路径。空 blob 返回**空 Buffer**（与 `undefined` 语义可区分）。
3. 归属：`readVaultFileAtCommit` 放在 `reader.ts`（「从 git 对象读内容」的读侧，与
   `readStoreSnapshotAtCommit` 同族），`commitPaths` 放在 `git.ts`（写侧）。`index.ts` barrel 两个都导出。

### 1.2 `src/cli/commands/secret.ts`（§2.6 四子命令实现，类型/签名一字未动）

- **keygen**：`generateIdentity()` → `writeIdentityFile()`（0600、原子写、**拒绝覆盖**）。
  报告 `{ ok, identityFile, recipient, created }` **只含 recipient**；私钥永不回显、不进错误消息。
  重复 keygen → `writeIdentityFile` 抛 `CliError` → 分发层 exit 1，原文件字节级不变。
  严格照冻结流程：本函数只做「生成 + 落盘」，不碰 git（`keys/` 的 gitignore 防护由
  `ensureGitRepo` 在 init/push 路径幂等维护，私钥从不被列入任何 commit pathspec）。
- **push**：`secrets.files` 空 → `no-secrets`(0) → identity 缺失 → `no-identity`(1) →
  recipients 空 → `no-recipients`(1) → **先读全部** destination 明文（任一缺失 →
  `missing-source`(1)，**全有或全无**，未写任何 vault 文件）→ 逐项 `encryptSecretToFile`（全部
  recipients）→ 非 `--yes` 时 `ui.confirm`（列 name→destination），拒绝 → `aborted`(1) →
  `commitPaths(home, ['secrets/'], …)` → 有 push target 且非 `--no-push` → `gitPush`
  （失败 → warning + `error`(1)）。
- **pull**：同款前置检查 → `gitFetch`（**失败 → warning + 回落读工作区 vault**）→ 成功且
  upstream 可解析 → 逐项 `readVaultFileAtCommit(home, 'secrets/<name>.age', upstreamRef)`
  （**不 ff 整仓**，store 工作区不被悄悄推进）→ 任一 vault 缺失 → `missing-vault`(1) →
  逐项解密（任一失败 → `undecryptable`(1)，**零写入**）→ 非 `--yes` confirm → 已存在目标先
  `backupFiles(paths, 'secret', …)`（label = `secret/<name>`）→ 写目标（`mkdir -p`，**0600**）→
  `applied`(0)。
- **list**：`listSecrets(paths, config)` 原样（纯读，不解密、无需 identity）。

**D4 零耦合**得到落实并测试锁定：commit 只 `add secrets/`（`store/` 的脏工作区原样留在工作区，
未跟踪文件不动）；pull 从 `@{upstream}` 读密文而不 ff。密文自检仍在 `vault.ts`（P1-W1），明文从不进 git。

## 2. 测试覆盖

新增/改动测试共 **41 例**（基线 1013 → 本分支 1054 全绿）：

| 文件 | 例数 | 覆盖 |
|---|---|---|
| `tests/cli/secret.test.ts`（新） | 30 | keygen 4 / push 14 / pull 8 / list 4 |
| `tests/core/git/git.test.ts`（增） | 6 | `commitPaths` 全分支 |
| `tests/core/git/reader.test.ts`（增） | 5 | `readVaultFileAtCommit` 全分支 |

逐条对齐 §3-P2-W6 验收：

- **keygen**：文件 0600 ✓、输出/`--json` 只含 recipient（断言无 `AGE-SECRET-KEY`，且报告字段集恰为 4 项）✓、
  重复 keygen exit 1 且文件字节级不覆盖 ✓。
- **push**：多 recipient 密文（从 **bare origin** 读 blob，用两把 identity 各自解密成功）✓、
  **`git grep <明文片段>` 在 bare origin 为空且正控制非空**（`git grep baseline` 命中）✓、
  另加 `git grep AGE-SECRET-KEY` 为空 ✓、missing-source 全有或全无（`secrets/` 目录未被创建）✓、
  no-identity / no-recipients exit 1（不产零 recipient 密文）✓、`--no-push`（origin SHA 不变、
  origin 无 `secrets/`）✓、**commit 只含 `secrets/` 而 store/ 脏状态与未跟踪文件原样**✓、
  确认拒绝 → aborted（无 commit）✓、`--yes` 不创建 port（注入 ui 零调用）✓、推送失败 → error(1) 且本地 commit 保留 ✓。
- **pull**：**构造 origin 领先场景**（本地 `reset --hard HEAD~1` 且删掉 `secrets/`）后
  `applied` 且内容 == 明文、权限 0600 ✓、**不 ff 整仓**（HEAD 未前移、工作区 `secrets/` 仍不存在、
  `store/` 零改动）✓、备份内容 == 覆盖前（`backups/**/secret/a-secret` == `OLD-A\n`，且覆盖前不存在的
  目标不进备份）✓、undecryptable（换成本机非 recipient 的 identity）→ exit 1 且**两个目标都未变**、
  无备份目录 ✓、fetch 失败 → warning + 回落工作区成功 ✓、missing-vault → exit 1 零写入 ✓、
  no-identity → exit 1（含两步迁移提示）✓、`secrets.files` 空 → `no-secrets`(0) ✓、确认拒绝 → aborted ✓。
- **list**：present/missing + `--json` 形状 `{ secrets: [{name,destination,vaultFile}] }` ✓、
  纯读（不创建 `secrets/`、不解密、不改文件）✓。

隔离：全部 `mkdtemp` 假 HOME + `git init --bare` 假 origin + `generateIdentity()` 临时 identity；
**绝不碰真实 `~/.homer` / `~/.ssh` / 真实 remote**。另用真实 CLI 分发层（`npm run dev`）跑了一遍
keygen → push → bare origin grep（空）→ list → 重复 keygen(1) → pull（写 0600 + 备份 `OLD-CONTENT`）冒烟。

## 3. 验证输出（原样）

```
$ git rev-parse --abbrev-ref HEAD && git rev-parse HEAD
m3-p2-w6-secret
4a1bc17e6c8af28254c9033aaebd4540b765c5b7

$ npm run typecheck
> homer-cli@0.0.0 typecheck
> tsc --noEmit
（无输出 = 通过）

$ npm test
 RUN  v5.0.1 /root/code/homer-cli

 Test Files  48 passed (48)
      Tests  1054 passed | 1 skipped (1055)
   Start at  07:41:13
   Duration  26.07s (tests 71%, transform 24%, import 4%)
```

定向用例：

```
$ npx vitest run tests/cli/secret.test.ts tests/core/git
 Test Files  3 passed (3)
      Tests  80 passed (80)
```

## 4. MR 链接

- 分支已推送：`origin/m3-p2-w6-secret` = `4a1bc17`（`git ls-remote` 已核对）。
- 本环境**无 `gh` / `glab` CLI 也无可用 token**，无法程序化建 PR；GitHub 在 push 时返回了建 PR 入口：
  **https://github.com/zzjcool/homer-cli/pull/new/m3-p2-w6-secret**
- 与 orchestrator 当前 master（`2f25368`）做 `git merge-tree --write-tree` 检查：**可干净合并**，
  且 W7 未触碰我的任何文件（零重叠）。

## 5. 未决问题 / 需要 orchestrator 知道的事

1. **⚠️ 共享 worktree 竞态（重要，非本任务代码问题）**：`/root/code/homer-cli` 这个 checkout 被多个
   agent 共用。我在 `m3-p2-w6-secret` 上开发期间，**有另一个进程在 07:13:53 把该 worktree 的 HEAD
   从我的分支切到了 `master`**（`git reflog` 可查），导致我第一次 `git commit` 落在了本地 `master`
   上（`7dce7bb`），且我第一次 push 推的是**过期的** `b14b2b2`。已做无损修复：
   - 把该 commit 以 `cherry-pick` 方式重放到冻结基线 `b14b2b2` 之上 → 干净的**单 commit**
     `4a1bc17`（只含我的 7 个文件），`git update-ref` 落到 `m3-p2-w6-secret` 并 `--force-with-lease` 推送；
   - 把本地 `master` **reset 回** orchestrator 的状态 `2f25368`（我的改动已完整保留在分支上）；
   - 临时 worktree / anchor ref 已清理，shared worktree HEAD 已恢复到 `master`、工作区干净。
   **建议**：后续 worker 一律在自己的 worktree 里开发（如 `homer-cli-w7` 那样），不要让多个 agent
   共用同一个 checkout 的 HEAD；orchestrator 若发现本地 `master` 有异常 commit，可对照上表。
2. **`secret push` 的 aborted 语义**：§2.6 冻结的顺序是「加密 → confirm → commit」，故用户在 confirm
   阶段拒绝时，`secrets/` 工作区**已含新密文但未 commit**（保持脏）。这是计划的既定顺序，无泄漏面
   （明文与私钥从不落盘），下次 push 会重新加密覆盖；仅提示：`aborted` 后工作区不是「完全没动」。
3. **`--no-push` 下 `pushedToRemote=false` 但 `status='pushed'`**：照 §2.6 退出码总表（pushed → 0）。
   与 `homer push` 的 `--no-push` 语义同款（只本地 commit）。
4. **`decryptSecretFromFile` 未被 pull 使用**：§2.2 的该函数内在加载 identity 且只吃工作区文件，
   而 pull 需要「从 `@{upstream}` 的密文」解密，故 pull 直接用注入的 `AgeCryptoPort.decrypt` +
   `loadIdentity`（identity 加载语义与 `decryptSecretFromFile` 一致：缺失 → `no-identity`）。
   该函数仍被 doctor（§2.5 `checkAge`）与 W8-home 使用，未改动。
5. 计划 §3-P2-W6 的文件范围写的是 `src/core/git/{git.ts,reader.ts,index.ts}`——已按此落地
   （`reader.ts` 承载 `readVaultFileAtCommit`）。
