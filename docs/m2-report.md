# W10-integrator 报告（homer-cli M2 · P4）

**分支**：`pi-subagent/w10-integrator`（基于 master `d685c81`，702 tests 绿）
**commit**：`bd77e80` `feat(m2): W10 integrator — e2e acceptance + status remote injection + seam fixes`
**工作区**：独立 worktree `/tmp/homer-w10`（未动父 checkout）
**验收范围**：`docs/m2-plan.md` §3-P4（8 组 e2e + status/diff remote 注入 + 缝隙修復）+ §5（M2 Done 定义）

---

## 1. 做了什么

| 文件 | 变更 | 归属 |
|---|---|---|
| `tests/e2e/m2.test.ts` | 新增 13 例（8 组验收 + §5 Done 抽样） | §3-P4 指定 |
| `tests/cli/integrator-seams.test.ts` | 新增 8 例（缝隙定点回归） | 缝隙修復附带 |
| `src/cli/commands/status.ts` | additive：`StatusReport.warnings`（仅非空时出现） | §3-P4 指定 |
| `src/cli/render.ts` | `collectSnapshotSources` 注入 remote + store-脏告警 | §3-P4 指定 |
| `src/cli/commands/diff.ts` | 置顶渲染 `warnings` | §3-P4 指定文件（同属 render 渲染面） |
| `src/cli/commands/git-port.ts` | **新增**：命令层共享 git 端口 | 缝隙修復（§3-P4 授权） |
| `src/cli/commands/push.ts` / `pull.ts` / `merge.ts` | 改用共享端口 + 三处接缝裁定 | 缝隙修復 |
| `src/core/sync/base.ts` | 首次接入：upstream 无 `store/` 树 → `remote := base` | 缝隙修復 |
| `README.md` | M2 已完成状态 + 命令集 | §3-P4 指定 |

**未动**：`src/core/types.ts`、`src/cli/index.ts`、`src/cli/args.ts`、`src/cli/ui.ts`、任何冻结签名。
所有改动对 `Deps` 都是 additive 可选字段；命令的**冻结流程与退出码总表逐条未变**。

---

## 2. 8 组 e2e 验收结果

全程 `mkdtemp` + 假 `HOME`/`HOMER_HOME` + `git init --bare` 假 origin；每条命令以
**真实子进程**跑 `node --import tsx src/cli/index.ts`（argv / 退出码 / `--json` 形状钉在进程边界）。
绝不碰真实 `~/.homer` / `~/.pi` / 真实 remote。

| # | 组 | 结果 | 关键断言 |
|---|---|---|---|
| ① | 安全往返 | ✅ | `origin` **首次**拿到 commit（push 前断言 bare 无 HEAD）；store 齐全（4 个文件含 `.homer-complete`）；`state.lastSyncCommit == HEAD`；改 skill + 删 skill + 改 settings 键 → 单次 commit 含 `A`/`D`/`M` 三类；幂等重跑 = `no-drift` 不产生新 commit |
| ② | 密钥拒推 | ✅ | 植入 `sk-ant-*` → **exit 1**、`status=secrets-rejected`、报告 `path:line`（`pi/settings/settings.json:1`）、脱敏摘录不含完整 token、**origin 无新 commit**、store 未被写入；加 `secrets.ignorePaths` 豁免后同 token 可推（exit 0 + origin 前进） |
| ③ | pull 应用 | ✅ | B 预放旧版 → `pull --yes` exit 0；工具目录 == store **逐字节**；`backups/<YYYYMMDD>/<HHmmss>-pull/pi/...` 存在且内容 == **覆盖前**旧版；`state == HEAD`；再跑 = `no-drift` |
| ④ | remote-ahead | ✅ | A 推 → B 推被拒（**exit 1**、`status=remote-ahead`、origin 不变、store 未写）→ B `pull --yes` exit 0 应用 A 的 settings，**同时保留 B 自己的 beta 改动** → B 随后 push 成功 |
| ⑤ | 冲突 + merge | ✅ | 同一 mirror 文件双方改 → B `pull --yes` **exit 1** `conflicts-remain`（reason=`modify-vs-modify`、本地原样保留）→ B `merge --accept-remote` exit 0 → B 工具目录 == A 版本、B==upstream、`state == HEAD` → A `pull` 拿到一致状态（`no-drift`） |
| ⑥ | pull-delete 传播 | ✅ | A 删 `beta/` 推 → B `pull --yes` exit 0；`applied.deleted` 含 `beta/SKILL.md`；本地文件被删、空父目录清理而 `skills/` 根保留；备份内容 == 删除前内容 |
| ⑦ | status ↓ 激活 | ✅ | git 模式下 A push 后 B `status --json` 出现 **pull=1**（M1 已知限制 #1 关闭）、push=0、settings 分类 pull=1；文本输出含 `↓1`；B 直改 store → `warnings` 置顶 ⚠（文本首行 ⚠）且 exit 仍 0 |
| ⑧ | retention | ✅ | 构造 10 个日期目录（20200101..20200110）→ `pull` 写操作后剩 7 个：最旧 4 个被删、本次备份的「今天」保留；`backup.keep=3` 用例 → 剩 3 个 |

外加 §5 Done 抽样用例：`push` / `pull` / `merge` 三条成功路径均满足
`state.lastSyncCommit == git rev-parse HEAD`。

---

## 3. 验证输出（原样）

### 全量 typecheck + test

```
$ npm run typecheck
> homer-cli@0.0.0 typecheck
> tsc --noEmit
（无输出 = 通过）

$ npm test
> vitest run

 RUN  v5.0.1 /tmp/homer-w10

 Test Files  35 passed (35)
      Tests  723 passed (723)
   Start at  04:48:10
   Duration  20.15s (tests 63%, transform 30%, import 7%, worker 1%)
```

基线 702 → **723**（+21 = e2e 13 + 缝隙回归 8）。全部 33 个既有测试文件原样通过
（`git reset` 后逐文件核对：push 21 / pull 46 / merge 20 / base 19 / … 均未改动断言）。

### e2e 单文件

```
$ npx vitest run tests/e2e/m2.test.ts
 Test Files  1 passed (1)
      Tests  13 passed (13)
   Duration  19.74s
```

### §5 机器可验收标准（独立子进程脚本，字面复刻计划 §5）

脚本 `/tmp/w10-s5-verify.sh`（临时目录 + 假 HOME + 假 origin，用后清理）：

```
== 1. init（A）
  OK   exit=0  homer init --json
== 2. push（拒密钥）
  OK   exit=1  push（密钥命中，应拒推）
  OK   status=secrets-rejected
  OK   报告含 path
== 3. push（成功）
  OK   exit=0  push（成功）
  OK   state.lastSyncCommit == git rev-parse HEAD (4cb49e51e08e636e176b6e8abe9cfbf43f6e5fad)
== 4. 第二 clone（B）
== 5. 制造冲突（双方改同一 mirror 文件）
  OK   exit=0  A push
== 6. B pull（冲突 → 保留本地）
  OK   exit=1  B pull（冲突，应 exit 1）
  OK   status=conflicts-remain
  OK   冲突时保留本地
== 7. B merge --accept-remote
  OK   exit=0  B merge --accept-remote
  OK   B 工具目录 == A 版本
  OK   B state.lastSyncCommit == git rev-parse HEAD
== 8. A pull 收敛
  OK   exit=0  A pull
  OK   A state.lastSyncCommit == git rev-parse HEAD
== 9. ~/.homer/.gitignore 含 state.json / backups/
  OK   .gitignore 含两行
== 10. git -C ~/.homer log --oneline（提交信息可读）
  8fc6538 homer push: 同步 1 个变更文件
  4cb49e5 homer push: 建立同步基线（store 首次入库）
  03e8fe7 chore: add homer.json

§5 机器可验收标准：全部通过
```

（§5 的「人工核验」三项也都覆盖：提交信息可读 ↑、`.gitignore` 含两行 ↑、
真实 `~/.pi/agent` 零写入——所有 CLI 子进程的 `HOME` 都指向 `mkdtemp` 目录。）

---

## 4. 缝隙修復（§3-P4 第 3 条）

### 4.1 统一 git stub 端口（任务明确要求）

W7-push / W9-merge 直接 `import` `core/git`，没有任何注入位；W8-pull 自建了 `PullGitPort` +
私有 `resolveGitPort`（三处形状不同 → 同一子集在不同命令里被替换成不同东西，测试断言的不是
同一条代码路径）。新增 **`src/cli/commands/git-port.ts`**：

- `GitPort`：12 个全可选成员（`isGitRepo` / `hasUpstream` / `upstreamRef` / `gitFetch` /
  `gitPush` / `headCommit` / `ensureGitRepo` / `commitStoreIfNeeded` / `isStoreClean` /
  `mergeFfUpstream` / `requireCleanStore` / `requireFastForwardable` + additive 的 `hasPushTarget`）；
- `resolveGitPort(port?)`：未覆盖项填真实实现；push / pull / merge 三者共用同一个解析函数；
- `PullGitPort` 保留为 `GitPort` 的**类型别名** → W8 既有 `import type { PullGitPort }` 与 46 例测试零改动；
- `PushDeps.git` / `MergeDeps.git` 为 additive 可选项，不传时行为与改动前逐字相同。

### 4.2 其他必须修的接缝（e2e 暴露的 4 处真问题）

集成 8 组 e2e 时，①②④ 组在**未改任何代码**的情况下无法跑通。逐条定位后发现是计划散文式
流程与实现之间的真实矛盾（非测试构造问题），按「不变式优先 + 最小 additive 改动」修：

| # | 现象 | 根因 | 裁定 |
|---|---|---|---|
| S1 | `init → push --yes` 返回 `no-drift`，origin 永远没有 commit，`state.json` 不生成 | §2.8「changedFiles 空 → no-drift exit 0」与 §3-P4①「init → push → origin 有 commit」/§5「state.lastSyncCommit == HEAD」直接冲突：`init` 只写 store **工作区**，三方（base=工作区 / local / remote）天然一致 ⇒ 零漂移 ⇒ 永无首个 commit。没有 HEAD 时 pull/merge 的 `requireFastForwardable` 也永久不可用 | 零漂移**且**仓库已有 HEAD**且** store 相对 HEAD 未提交 → 落首个 store commit（message =「建立同步基线」），随后走常规 push/state。无仓库 / 无 HEAD（local-only 工作区）**保持 no-drift** —— W7 已钉死该语义（`无 upstream → local-only: no-drift` 等 3 例原样通过） |
| S2 | 首次 push 被误拦成 `remote-ahead`（远端「每个文件都像是被删了」） | `collectSyncSources` 把「upstream commit 里没有 `store/` 树」当成「远端删光了所有文件」 | upstream commit 无 `store/` 树（`git ls-tree store/` 为空）→ `remote := base` + warning（§1-D3 fallback 语义的首次接入版）。base 为空时不发 warning（无观察差异，不骚扰） |
| S3 | `homer push` 在「刚 clone 空 origin」时误入 local-only，commit 推不上去 | `hasUpstream()` 依赖 `@{upstream}`，而 clone 空仓库后只有 `branch.main.remote=origin`、**没有远程跟踪引用**，`@{upstream}` 不可解析 | additive `hasPushTarget`：`@{upstream}` 可解析 **或** `branch.<name>.remote` 指向存在的 remote → 可推送。仅用于 push 的远端推送判定（pull/merge 的 upstream 检查语义不变） |
| S4 | `B pull`（`conflicts-remain`）之后再 `homer merge` 报 `no-conflicts`，§3-P4⑤ 不可达 | pull 在**有残留冲突**时仍把 `state.lastSyncCommit` 前移到 ff 后 HEAD ⇒ 下一轮 merge 的 base 变成远端，本地改动被当成单纯 push 漂移，冲突消失 | 语义收口：「`lastSyncCommit` = 上次**成功**同步后的 HEAD」。ff 前记 `preFfHead`；**有残留冲突 → base 停在 `preFfHead`**（由 merge 收尾前移）；无冲突 → 前移到 ff HEAD（§1-D6 原语义不变）。两条路径各有定点用例 |

**这 4 处都需要 orchestrator 知晓**：它们是计划正文（§1-D1/§1-D3/§2.8/§5）之间的张力，
不改接口与退出码总表，但改变了「首次 push 在零漂移时也会产生 commit」这一可观察行为。
建议在计划 §2.8 push 流程补一行「**首次同步基线**：无 drift 且 store 未入库时仍 commit」，
在 §1-D6 补一行「带残留冲突的 pull 不前移 base」。

### 4.3 status / diff 的 remote 注入（任务第 2 条）

`collectSnapshotSources(paths, config)` additive：

- `paths.home` 是 git 仓库根且 `@{upstream}` 可解析 → **`git fetch` 后**
  `remote = readStoreSnapshotAtCommit(upstream)`（经同一 `stripExcludeKeys` 口径）。
  ⑦ 组的 ↓=1 由此达成；**关闭 M1 已知限制 #1**（「`homer status` 的 ↓ 恒为 0」）。
- 非仓库 / 无 upstream / ref 不可解析 → `remote` 缺省 → 调用方回落 = base（**M1 行为不变**，静默）。
- fetch 失败（离线）→ ⚠ 告警 + 回落 = base，exit 码不变（status 仍可用，不因网络挂掉）。
- **store 工作区脏** → 置顶 ⚠（`StatusReport.warnings`）。`warnings` **仅在非空时出现**，
  保持 M1 冻结的 `StatusReport` 形状在常规路径上逐字不变（既有 `--json` 消费者 / 快照测试
  不受影响，实测 702 例全绿）。

---

## 5. 已知限制（M2 边界）

1. **`pull` 的 `conflicts-remain` 不算「成功同步」**：`state.lastSyncCommit` 停在本轮 pull 之前的 HEAD
   （见 S4）。副作用：`homer status` 在残留冲突期间仍会报远端未拉（↓>0），直至 `homer merge` 收尾。
   这是刻意的——merge 需要这个 base 才能看见冲突。
2. **`homer push` 首次会写一个「基线 commit」**（S1）。零漂移时也会产生 commit
   （message「建立同步基线」）。已被 §3-P4① 与 §5 要求，但与 §2.8 字面「changedFiles 空 → no-drift」
   有张力，已在 §4.2 上报。
3. **`status` / `diff` 现在会 `git fetch`**（远端变更可见的前提）。离线时降级为 ⚠ + 回落 base，
   不再是「完全不碰网络」。若 M3 引入 `doctor`，可考虑加 `--offline`。
4. **无 upstream 的 local-only 仓库**：`push` 仍 no-op 返回（W7 已钉语义），store 不会入库。
   这类仓库要走 git 模式需先 `git push -u`。已在 warning 里提示。
5. **canonical JSON 序列化仍未做**（计划 Non-goals）：`planPull` 的 `mergeJson` 输出用
   `JSON.stringify(merged, null, 2)` 确定性格式，但 base/local/remote 的**内容相等**仍是字符串全等；
   git remote 注入后 base-absent 分支生产可达，语义等价但键序不同的 JSON 可能产生保守冲突
   （进 merge 交互，不静默丢数据）。e2e 未观测到该情形。
6. **未做 `homer status` 的 `--fetch/--offline` 开关**：每次 status 一次 fetch，
   离线 / 大仓库的网络代价未评估（计划未要求）。
7. **真实环境冒烟为隔离环境**：全部验收在 `mkdtemp` + 假 HOME 下完成；
   真实 `~/.pi/agent` 仅被**只读扫描**（`init` / `status` / `push` 的 local 采集），无任何写入。
   「store 与真实目录长期一致性」仍未验证（需人机交互确认，属 M3 `doctor`）。

---

## 6. W9 上报的三个未决问题的裁决建议

W9 报告 §5 列了 5 条，任务点名三条，逐条给裁决建议（附本次 e2e 的实证）：

### 6.1 `clean` 动作是否应在无冲突时也应用（W9 §5-1）

**建议：维持现状（有冲突时才应用）。** 理由：

- §2.8 冻结流程字面写「`planPull` 取 conflict actions → 空 → `no-conflicts` exit 0」；DESIGN §2.3
  明确「`homer sync` = 智能同步」「`merge` 处理冲突、`pull` 应用远端」；
- 本次 ⑤ 组实测量到：**有冲突时** clean 动作确实被一并应用且必要（否则后续 push 管线会把远端
  新增/删除冒充成本地状态回写——W9 的裁定 1 正确）；**无冲突时**保持零写入，用户显式走 `homer pull`
  即可（③⑦ 组证明 pull 是干净的常规入口）；
- 「无冲突时 merge 等价 pull --yes」是**新命令语义**，属产品决策，应由 M3 的 `homer sync` 承载，
  不在 M2 偷跑。

### 6.2 `applied` 在 `status='error'` 时返回空（W9 §5-2）

**建议：M2 维持现状，记录为 M3 的 additive 接口候选。**

- `ApplyResult` 由 W6 的 `applyPullActions` 产出（`src/core/sync/types.ts` 冻结）；「支持部分结果回传」
  是 additive 接口变更，本波次无权私改（§3-P4 明确：接口问题上报不私改）；
- W9 已用 `warnings` 如实告警「store 已快进到 upstream 且 base 已前进，但工具目录写入未完成」，
  信息没有丢；本次 e2e 也验证了正常路径（①②③④⑤⑥）不触发该分支；
- 若要做，建议 M3 给 `ApplyResult` 加 `failed?: { adapterId; category; relPath; error }[]`
  （additive），让 error 分支回传部分结果。**上报 orchestrator 决定是否纳入 M3 计划。**

### 6.3 `git push` 失败是否应改 exit 1（W9 §5-3）

**建议：merge 维持 `resolved` / exit 0 + warning，push 维持 exit 1。** 理由：

- §2.8 **push 流程**明写「`git push` 失败 → warning + exit 1」，这是 push 的冻结语义，本次未动；
- §2.8 **merge 流程**只说「走 push 管线」，未规定失败码。merge 的核心承诺是「本地意图一致」
  （§1 关键不变式：`local == 意图真相`），在 push 失败时该承诺**已达成**（本地 commit 已成立、
  state 已前移），只剩远端待重试。此时 exit 1 会让脚本误以为 merge 失败并重跑，而重跑是幂等的
  no-conflicts —— 反而掩盖真因；
- 折中：把「远端待重试」写进 warning（W9 已做），并在 M3 的 `homer status` / `doctor` 里暴露
  「本地领先远端」计数（本次 S3/§4.3 的 `hasPushTarget` 已为此打底）。

**另外两条（W9 §5-4 TTY 判定、§5-5 `keyPaths` 进 `--json`）**：本次复核认为现状正确，无需改：
前者避免在非交互环境留下 clack 提示框副作用（本次 ②③④⑤⑥ 的 e2e 全部实测非交互路径符合预期）；
后者只输出**配置键名**不含键值，② 组实测 `--json` 不含完整 token。

---

## 7. 未决问题

1. **无法自动创建 MR/PR**：环境无 `gh` / `glab` / `hub`，只有 `git`（SSH remote 可用）。
   分支已 push，GitHub 回显创建入口：
   **https://github.com/zzjcool/homer-cli/pull/new/pi-subagent/w10-integrator**（建议 base = `master`）。
   本地未 merge、未 push 到 master。
2. **§4.2 的 4 处接缝裁定需 orchestrator 追认**（S1/S4 改变了可观察行为；S2/S3 是缺陷修复）。
   若不追认 S1，则 §3-P4① 与 §5 的 Done 判据不可达（e2e 会红）。
3. **`status` / `diff` 的隐式 `git fetch`**（§5-3）：是否需要 `--offline` 开关属 M3 产品决策。
4. 计划 §3-P4 未列 `tests/cli/integrator-seams.test.ts`（属「缝隙修復」附带产物）；
   若不希望新增测试文件，可并入 `tests/cli/status.test.ts`，请示下。
