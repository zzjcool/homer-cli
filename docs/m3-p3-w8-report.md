# P3-W8 · home 报告（`homer home <repo-url>` 一键归位）

> 计划：`docs/m3-plan.md` §2.7（home 流程，冻结）+ §1-D5（首次对接语义）+ §3-P3-W8（验收）
> 分支：`m3-p3-w8-home`　基线：`94f1a0f`（master，1107 tests 绿）　隔离 worktree：`/root/code/homer-cli-w8`

## 1. 做了什么

### 1.1 `src/cli/commands/home.ts`（P0 stub → 真实实现，类型/签名未动）

§2.7 十步流程逐条落地：

| 步骤 | 落地 |
|---|---|
| 1 | `assertTargetIsEmpty`：不存在 / 空目录放行；**非空或非目录 → CliError**，且发生在任何 clone / 写盘之前 |
| 2 | `deps.clone` 注入位优先；缺省 `execFile('git', ['clone', url, dest], { timeout: 60_000 })`。**任何失败（含注入抛错）→ CliError 含 stderr / message 首行摘要**（截断 300 字符） |
| 3 | `loadConfig`；缺失 → CliError「该仓库不是 homer 配置中心」；JSON 非法 / 校验失败 → 同一措辞 + 原因 |
| 4 | `scanAdapter` 逐 enabled adapter；**root 不可读 → local := remote**（M-A 守卫，判定与措辞走 `core/scan-guard.ts` 唯一实现点），原因进 `warnings` |
| 5 | `remote = readSnapshotFromStore`（clone 后工作区 == HEAD） |
| 6 | `--mode` 优先 → `--yes` 无 mode 默认 `merge` → 否则 `ui.select`（Pull/Merge/Skip，fallback `merge`）；**非 TTY 且无 mode/yes → CliError** |
| 7 | `planFirstContact` → `confirm`（唯一同意点）→ `applyPullActions(paths, config, plan, { backup: true, command: 'home' })`（conflict 项由 apply 跳过 = 整体保留本地） |
| 8 | `saveState`：`lastSyncCommit = HEAD` / `lastSyncCommand = 'pull'` / `lastSyncAt`；HEAD 不可解析 → warning + 只写时间戳 |
| 9 | 密钥归位：identity 缺失 → 全量 `skipped` + 两步提示；vault 缺失 / 不可解 → `errors` + warning，**零写入**；全部可解 → 已存在目标先 `backupFiles(label='secret/<name>')` → 写目标 `mkdir -p` + **0600** |
| 10 | `runDoctor`（透传 `deps.age`）→ 报告 `doctor`；`homed` exit 0（doctor 的 fail 只呈现在报告里） |

**新导出的纯函数**：`buildFirstContactPreview(config, plan, secretNames)`（预览文本，含动作计数 + 前 20 条明细 + 密钥 name→目标清单）、`PREVIEW_MAX_ACTIONS`、`CLONE_TIMEOUT_MS`。

### 1.2 三处设计裁定（需要 orchestrator 知晓）

1. **确认门槛取「配置动作 ∪ 密钥归位」并集**（§2.7 步骤 7 原文只写「非 skip 且有动作」）。
   密钥归位同样是写用户目录、且会覆写已存在目标（与 `secret pull` 同类破坏性操作）。
   若门槛只挂在配置动作上，`--mode skip` + 配了密钥就会出现**「无动作 → 无确认 → 静默覆写密钥目标」**，
   与 secret 通道自己的 `--yes` 语义冲突。故并集，preview 同时列两类；非 TTY 下
   `confirm` 回落 `false` → `aborted`（不阻塞、不默认同意）。用例「密钥目标已存在 → 先备份」即走这条路径。
2. **失败形态三分**（`ok`/`status` 映射冻结不变）：流程无法开始 → `throw CliError`（分发层 exit 1）；
   用户拒绝 → `aborted` 报告（零应用 / 零 state / 零密钥）；**步骤 7-10 的用户级失败 → `error` 报告**
   （`--json` 仍可解析，`cloned`/`adapterIds`/已完成的 `applied` 可见）；非 `CliError` 异常一律冒泡。
3. **`firstContact.conflicts` 换用 M2 权威类型 `PullConflictAction`**（P0 的 `HomeConflictSummary` 保留为等价别名）。
   §2.7 冻结签名原文即 `PullConflictAction[]`，P0 在该处留了 TODO 要求收口；形状一致，零破坏。

### 1.3 `tests/cli/home.test.ts`（新，25 例）

假 HOME 双机（A = 旧机 / B = 新机）+ `git init --bare` 假 origin；adapter root 一律声明为 `~/.pi/agent`
（`expandHome` 走进程 HOME），断言全部在 `withFakeHome` 窗口内执行 —— **绝不碰真实 `~/.pi` / `~/.homer`**。
identity 一律 `generateIdentity()` 临时生成。覆盖：

- **全流程**：A 侧 init（config + store 快照）→ commit → push → `secret push` → B `runHome({mode:'merge'})`：
  工具目录**逐文件按 `resolveCategoryFilePath` 反算后与 store 逐字节比对**（并断言工具目录无多余文件）、
  密文解密归位（内容 == A 明文、0600）、`state.lastSyncCommit == HEAD`、doctor 八项齐全且 age 项 ok；
  另加 bare origin `git grep` 铁证（明文片段 / `AGE-SECRET-KEY` 均为空，正控制非空）；
- **真 clone 路径**（不注入 `deps.clone`）与分发层 `--json` 退出码 0；
- **三模式**：pull（3 write / 零 delete / local-only 保留 / 备份内容 == 覆盖前）、
  merge（冲突保留本地 + remote 独有写入 → 随后 `runStatus` 出现 ↑>0 且 ↓==0 = **D5 语义锁定**）、
  skip（零应用 + `backups/` 为空 + config/state 就位）；
- **交互**：fake port 记录 `select`（选项 = pull/merge/skip、fallback = merge）+ `confirm` 收到预览；
  拒绝 → `aborted` 零写入零 state；非 TTY 无 mode/yes → CliError + 分发层 exit 1；
  `--yes` 无 mode → 默认 merge 且用「调用即抛」的 port 证明**完全没创建 / 没触碰 port**；
- **边界**：home 目录非空 → CliError 且 `deps.clone` 调用计数 == 0；clone 失败（注入 stderr）与真 clone
  失败（真 git）→ CliError 含摘要、exit 1；仓库无 homer.json → CliError；
  identity 缺失 → 配置归位 + `skipped` + warning + 仍 `homed` exit 0（doctor age=fail 但不改 home 退出码）；
  vault 缺失 → `errors` 含缺失清单 + 零写入；identity 非 recipient → `errors` + 零写入（且报告不含私钥/明文）；
  adapter root 缺失 → M-A 守卫（零写入 + warning）；`excludeKeys` 占位符**永不流入工具目录** +
  doctor `required` warn；渲染 / 预览纯函数。

### 1.4 `tests/core/m3-p0-scaffold.test.ts`（1 例断言更新）

原用例断言 stub `CliError('尚未实现')`。W8 落地后同一组 flag 会进入真实流程（该 repo-url clone 失败 → exit 1），
故断言改为「exit 1 且**不是** usage 错误（不打印 `用法: homer home`）」——仍钉住原意图「flag 表全部被接受」。
与 P1-W1 替换 `createAgeCryptoPort` stub 时同一处理方式。

## 2. 测试覆盖（验收逐条对照）

| §3-P3-W8 验收项 | 用例 |
|---|---|
| 全流程：工具目录 == store、密文归位（== A 明文 / 0600）、state == HEAD、doctor 附于结果 | 「merge 模式：工具目录 == store 内容…」 |
| pull：远端覆盖 + local-only 保留 + 备份存在 | 「pull：远端覆盖 + local-only 保留（无 delete）+ 备份存在」 |
| merge：冲突保留本地 → 随后 status 出现 ↑ 漂移 | 「merge：冲突保留本地…（D5 锁定）」 |
| skip：零应用但 config/state 就位 | 「skip：零应用但 config / state 就位」 |
| 交互：fake PromptPort 记录 select | 「fake PromptPort：select 被调用…」+「select 选 skip…」 |
| 非 TTY 无 mode/yes → CliError | 「非 TTY 且无 --mode / --yes → CliError」 |
| `--yes` 默认 merge | 「--yes 无 mode → 默认 merge…」 |
| home 目录非空 → CliError | 「home 目录非空 → CliError，且不尝试 clone」 |
| clone 失败 → CliError 含摘要 | 「clone 失败 → CliError 含 stderr / 错误摘要」 |
| 仓库无 homer.json → CliError | 「仓库无 homer.json → CliError…」 |
| identity 缺失 → 配置归位 + skipped + warning + 仍 homed exit 0 | 「identity 缺失 → …整体仍 homed」 |

**变异验证**（逐条改坏实现确认对应用例转红，改回后全绿）：`--yes` 默认值 → skip（1 红）、
state 不写 `lastSyncCommit`（6 红）、home 非空守卫移除（1 红）、`chmod 0600` 移除（1 红）、
identity 缺失改为抛错（2 红）、M-A 守卫移除（1 红）、clone 摘要丢失（1 红）、非 TTY 检查移除（1 红）、
skip 走 merge 分支（4 红）、无 homer.json 不拦（1 红）、密钥「全有或全无」破除（1 红）、
secret 备份跳过（1 红）、`confirm` 拒绝仍继续（1 红）。**13/13 变异全部被捕获。**

## 3. 验证输出

### 3.1 全量测试 + typecheck

```
$ npm run typecheck && npm test

> homer-cli@0.0.0 typecheck
> tsc --noEmit

> homer-cli@0.0.0 test
> vitest run

 RUN  v5.0.1 /root/code/homer-cli-w8

 Test Files  52 passed (52)
      Tests  1132 passed | 1 skipped (1133)
   Start at  08:08:44
   Duration  26.43s
```

（基线 1107 passed + 1 skipped；W8 净增 25 例。）

### 3.2 本文件专项

```
$ npx vitest run tests/cli/home.test.ts --reporter=verbose
 ✓ tests/cli/home.test.ts > W8 · 三模式语义 > merge：冲突保留本地（remote 独有仍写入）→ 随后 `status` 出现 ↑ 漂移（D5 锁定）
 ✓ tests/cli/home.test.ts > W8 · 首次对接交互 > --yes 无 mode → 默认 merge，且完全不创建 / 不触碰 port
 ✓ tests/cli/home.test.ts > W8 · 边界与失败形态 > identity 缺失 → 配置归位 + secrets.skipped + warning，整体仍 homed（exit 0；doctor 的 age fail 不改变 home 退出码）
 ✓ tests/cli/home.test.ts > W8 · 边界与失败形态 > excludeKeys：store 里的 __REQUIRED__ 占位符永不流入工具目录（pull 模式）+ doctor 报 required warn

 Test Files  1 passed (1)
      Tests  25 passed (25)
```

### 3.3 真实 CLI 冒烟（`node --import tsx src/cli/index.ts`，双假 HOME + 假 origin）

场景 1（全新机器 + 密钥）：

```
$ HOME=<B> HOMER_HOME=<B>/homer node --import tsx src/cli/index.ts home <origin> --mode merge --yes
homer home: homed
  已 clone: 是
  adapter: pi
  首次对接: merge（写入 2 / 删除 0 / 冲突 0）
  密钥归位: 1 个（tok）
  体检: 通过（详见 `homer doctor`）
exit=0
$ cat <B>/home/.token
TOKEN=smoke-secret-value-123456
$ ls -l <B>/home/.token   # -rw-------  (0600)
$ HOME=<B> HOMER_HOME=<B>/homer … status
pi  ↑0 ↓0
无漂移
```

场景 2（D5：冲突保留本地 → push 漂移）：

```
$ … home <origin> --mode merge --yes --json
status homed written [] conflicts [ 'settings.json', 'alpha/SKILL.md' ]
--- 工具目录（冲突保留本地、local-only 保留）:
# alpha local
{"theme":"light"}
# local only
--- status（应见 ↑ 漂移、↓ 0）:
pi  ↑3 ↓0
--- state:
{"version":1,"lastSyncCommit":"a75c3a6…","lastSyncCommand":"pull"}   # == git rev-parse HEAD
--- doctor --offline:
homer doctor: 通过   合计: ok 8  warn 0  fail 0
```

## 4. 未决问题 / 上报 orchestrator

1. **确认门槛并集裁定**（§1.2-1）：偏离 §2.7 步骤 7 的字面「非 skip 且有动作」。已按安全性取并集并在文件头详述理由；
   若 orchestrator 认为必须逐字，改成 `plan.actions.length > 0` 即可（但会引入 skip+密钥的静默覆写面）。
2. **`tests/core/m3-p0-scaffold.test.ts` 1 例断言更新**：P0 scaffold 用例原本断言 stub；
   不改则 P0 侧必红（与 W1 替换 age stub 时同样的接缝）。已按「只钉 flag 表被接受」的最小改法更新。
3. **`--yes` 时密钥归位不做二次确认**：§2.7 步骤 7 的 `confirm` 是唯一同意点（`secret push|pull` 同款纪律）。已按此实现。
4. **home 不做 `git fetch`**：clone 后工作区 == HEAD，密钥从工作区 vault 读（§2.7 步骤 9 原文）。
   若旧机在 clone 之后又 `secret push`，需用户自行 `homer secret pull`（warning 已给出该提示）。
5. **`docs/m3-p3-w8-report.md`（本文件）** 是唯一超出任务书点名字范围的产出（沿用 W1/W2/W6/W7 各自的报告惯例）；
   如需严格限定文件集，删除本文件即可，代码与测试零依赖它。

## 5. 交付信息

- 分支：`m3-p3-w8-home`（基线 `94f1a0f`，已 push 到 `origin`）
- 实现 commit：`4358216`（`src/cli/commands/home.ts` + `tests/cli/home.test.ts` + `tests/core/m3-p0-scaffold.test.ts` 断言更新）
- PR 链接（远端提示，仓库无 glab/gh CLI）：<https://github.com/zzjcool/homer-cli/pull/new/m3-p3-w8-home>
- 隔离：独立 worktree `/root/code/homer-cli-w8`，未触碰 master checkout / 其它 wave 的 worktree。
