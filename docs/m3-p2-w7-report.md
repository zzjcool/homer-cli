# M3 P2-W7 报告：`homer doctor` 八项体检

> 计划依据：`docs/m3-plan.md` §2.5（doctor 接口与八项检查）/ §1-D6（边界）/ §3-P2-W7（验收）
> 基线：`master` = `b14b2b2`（1013 tests 绿 + 1 skip）
> 本分支：`m3-p2-w7-doctor`，实现 commit `35e2c1e`

## 1. 做了什么

| 计划条目 | 落地 | 说明 |
|---|---|---|
| §2.5 `src/core/doctor/checks.ts` | ✅ 新增 | 八项检查逐项落地；纯逻辑 + 薄 fs/git；`checkAge` 的密码学能力经 **`AgeCryptoPort` 注入**（冻结签名逐字一致） |
| §2.5 `src/core/doctor/index.ts` | ✅ 新增 | 门面：显式逐项导出（不用 `export *`，与 `core/age/index.ts` 同约定），避免同名歧义 |
| §2.5 `src/cli/commands/doctor.ts` | ✅ P0 stub 重写 | `runDoctor` 编排（八项 + `ok = 无 fail`）+ `renderDoctorReport` 细化；`DoctorOptions`/`DoctorDeps`/`DOCTOR_USAGE`/签名零改动；P0 本地类型副本删除，改为 `core/doctor/checks.js` import + re-export（形状逐字相同） |
| §3-P2-W7 测试 | ✅ 3 个新文件 | `tests/core/doctor/checks.test.ts`（34 例）、`tests/core/doctor/index.test.ts`（3 例）、`tests/cli/doctor.test.ts`（16 例） |

### 1.1 八项检查的状态矩阵（§1-D6 冻结：fail → exit 1，warn → exit 0）

| # | id | ok | warn | fail |
|---|---|---|---|---|
| ① | `config` | 合法 homer.json | — | 缺失 / 非法 JSON / 校验不过（details 逐条列错误） |
| ② | `repo` | git 仓库根 | 仓库但无 upstream | 非 git 仓库 |
| ③ | `store-clean` | `git status --porcelain store/` 空 | 脏（details 列条目，≤20 条）；非仓库时「不可判定」 | — |
| ④ | `remote` | 可达（`git ls-remote --heads <url>`，10s 超时）；`--offline` → 「已跳过」 | 不可达（离线？）/ 无 upstream / 非仓库 | — |
| ⑤ | `adapters` | 全部 enabled root 存在 | 缺失或不是目录（details 逐 id 列 root，提示「工具未安装？」） | — |
| ⑥ | `age` | 未配置 secrets（files+recipients 均空）/ identity+recipients 就绪且全部可解密 | 配置了密钥但 vault 文件缺失（尚未 push？） | identity 缺失 / recipients 空 / 任一试解密失败 |
| ⑦ | `machine` | `state.lastSyncCommit === HEAD` | state 缺失；落后于 HEAD（未同步 commit）；HEAD 不可解析 | — |
| ⑧ | `required` | 无 `__REQUIRED__` 残留 | 有残留（details = `` `<adapter>/<category>/<file>: key1, key2` ``） | — |

**关键语义裁定（实现依据，均取自计划原文）**：

- **`--offline` 跳过 ≠ warn**：§2.5 冻结「offline=true → ok（'已跳过'）」；「不可达 → warn」是
  非 offline 路径的判定。离线本身不是 homer 的问题（m2-report §5-3 要的就是「不碰网络的体检档位」）。
- **`__REQUIRED__` 残留 = warn**（§1-D6 / DESIGN §2.7）：这是「本机待补全的必填项」这一用户待办，
  不是故障，因此必须 exit 0。details 精确到**文件 + 顶层键**，键按字典序。
- **`machine` 落后于 HEAD = warn**：这正是 M2 §1-D5 的 push 漂移语义（`homer push` 之后
  `state.lastSyncCommit` 会领先/落后取决于命令）。`state` 是加速信息，`loadState` 本就降级不抛，
  故此处只有 warn 档。
- **config fail 时仍继续**（§2.5）：`repo` / `store-clean` / `remote` / `machine` 只依赖路径与 git，
  照常执行；`adapters` / `age` / `required` 需要结构化 config，报「已跳过（config 不可用）」而不是
  猜测。`ok` 已由 config 的 fail 定为 false，跳过不掩盖问题，且八项**始终全部出现**在报告里
  （`--json` 消费者的形状稳定）。
- **`checkAge` 只在「配置了密钥同步」时才要求 identity**：`secrets` 段全空 → ok。这直接支撑
  §3-P4 ⑦ 的真实环境冒烟（`homer doctor` 报 age 未配置 → ok、无 fail）。
- **`checkRequiredPlaceholders` 对半残 store 只 warn 不抛**：`readSnapshotFromStore` 遇缺
  `.homer-complete` 会抛 `CliError`；doctor 作为诊断工具必须始终产出报告，故捕获后降级为 warn。

### 1.2 与既有实现的耦合面（全部只读）

`core/config.validateConfig`、`core/git.{isGitRepo,isStoreClean,upstreamRef,headCommit,gitExec}`、
`core/state.loadState`、`core/store.readSnapshotFromStore`、`core/sync.REQUIRED_PLACEHOLDER`、
`core/age.{loadIdentity,identityFilePath,listSecrets,decryptSecretFromFile,createAgeCryptoPort}`、
`core/paths.expandHome`。**本 worker 未修改任何既有文件**（唯一改动的既有文件是任务授权的
`src/cli/commands/doctor.ts`）。

## 2. 测试覆盖（53 例）

| 文件 | 例数 | 覆盖 |
|---|---|---|
| `tests/core/doctor/checks.test.ts` | 34 | ①config ok/缺失/坏 JSON/校验失败；②③repo 非仓库 fail、无 upstream warn、ok、store 脏 warn 且 details 含具体文件；④remote `--offline` ok（含不可达远端仍 ok）、可达 ok、不可达 warn、非仓库/无 upstream warn；⑤adapters ok、缺失 warn、非目录 warn、`enabled:false` 不检查；⑥age 未配置 ok、identity 缺失 fail、recipients 空 fail、无 files ok、真实 port 解密成功 ok、vault 缺失 warn、**假 port 解密抛错 fail** + 假 port 成功 ok（记录调用次数）；⑦machine 无 state warn、落后 warn（details 含两 SHA）、一致 ok、unborn HEAD warn；⑧required 无残留 ok、残留 warn 且 `details` **精确等于** `['pi/settings/settings.json: apiKeys, token']`、嵌套键/`__REQUIRED__x`/非 JSON 不误报、半残 store 降级 warn、多 adapter 键序稳定 |
| `tests/core/doctor/index.test.ts` | 3 | 门面导出清单（七个检查函数 + 常量）、命令层 re-export 一致、超时常量 = 10s |
| `tests/cli/doctor.test.ts` | 16 | 八项齐全且**顺序**为 config→repo→store-clean→remote→adapters→age→machine→required；config 缺失时其余尽力而为；健康机器八项全 ok；`--offline` ok；远端不可达 warn 但 `ok=true`；注入假 port → age fail → `ok=false`；未配置密钥同步时假 port **零调用**；分发层 exit 码（仅 warn → 0、有 fail → 1、`--json` fail 场景 1）；`--json` 可 `JSON.parse` 且形状为 `DoctorReport`；`__REQUIRED__` 残留经 CLI 端到端 details 精确；`--help` 逐字等于 `DOCTOR_USAGE`；未知选项 strict 拦下；`renderDoctorReport` 标记/缩进/合计行/修复提示 |

全部用例走 `mkdtemp` 临时 home + `git init --bare` 假 origin（`tests/core/git/helpers.ts`），
**不碰真实 `~/.homer` / `~/.config` / 网络**（远端可达性用本地 bare 路径）。

## 3. 验证输出（原样）

```
$ npm run typecheck
> homer-cli@0.0.0 typecheck
> tsc --noEmit
（无输出 = 成功）

$ npm test
> vitest run
 Test Files  50 passed (50)
      Tests  1066 passed | 1 skipped (1067)
   Duration  26.10s
（1 skipped = 既有 `age CLI 互操作` 用例：本机无 age CLI，S0 结论）

$ npx vitest run tests/core/doctor tests/cli/doctor.test.ts
 Test Files  3 passed (3)
      Tests  53 passed (53)
```

真实 CLI 冒烟（`npm run build` 后，临时 home + 本地 bare origin）：

```
$ node bin/homer.js doctor --home "$HOME_DIR"
homer doctor: 通过
  ✓ config  homer.json 有效（1 个 adapter: pi）
  ✓ repo  git 仓库（upstream: origin/main）
  ✓ store-clean  store 工作区干净
  ✓ remote  远端可达: /tmp/.../origin.git（1 个分支）
  ✓ adapters  1 个 adapter root 均存在
  ✓ age  未配置密钥同步（secrets 段为空）
  ✓ machine  state 与 HEAD 一致（b33711c）
  ⚠ required  1 个文件残留 __REQUIRED__ 占位符（本机需补全这些必填项）
      - pi/settings/settings.json: apiKeys
  合计: ok 7  warn 1  fail 0
exit=0

$ node bin/homer.js doctor --home "$HOME_DIR" --json
ok=true [["config","ok"],["repo","ok"],["store-clean","ok"],["remote","ok"],
         ["adapters","ok"],["age","ok"],["machine","ok"],["required","warn"]]

$ echo '{ bad' > "$HOME_DIR/homer.json"; node bin/homer.js doctor --home "$HOME_DIR" --offline
homer doctor: 存在问题
  ✗ config  /tmp/.../homer.json 不是合法 JSON
      - Expected property name or '}' in JSON at position 2 (line 1 column 3)
  ✓ repo … ✓ store-clean … ✓ remote 远端可达性检查已跳过（--offline）
  ✓ adapters 已跳过（config 不可用）  ✓ age 已跳过（config 不可用）
  ✓ machine … ✓ required 已跳过（config 不可用）
  合计: ok 7  warn 0  fail 1
  有 fail 项：修复后重跑 `homer doctor`（warn 不影响退出码）。
exit=1
```

> 注：arm 上的 `required warn` 来自冒烟夹具刻意留下的 `__REQUIRED__`（等价于 §3-P4 ④ 场景），
> `--offline` 与 `config fail` 的分支行为由此一并实测。

## 4. 验收对照（§3-P2-W7 原文）

| 验收项 | 证据 |
|---|---|
| 八项各自 ok/warn/fail 至少一例 | 见 §1.1 矩阵（三文件 53 例） |
| config 缺失 / 损坏 | `checkConfig` 3 例 + 分发层 exit 1 例 |
| 非 git 仓库 | `checkRepoAndStore` 首例（repo fail + store-clean warn） |
| store 脏 | `checkRepoAndStore` 末例（details 含具体文件） |
| offline 跳过 + 离线 warn | `checkRemote` 4 例 + 分发层 2 例（不可达 → warn 但 `ok=true`） |
| adapter root 缺失 warn | `checkAdapters` 2 例 + 分发层「仅 warn → exit 0」例 |
| age 未配置 ok / identity 缺失 fail / 试解密失败 fail | `checkAge` 全部 8 例 |
| state 缺失或领先 warn | `checkMachine` 4 例 |
| `__REQUIRED__` 残留 warn 且 details 精确 | `checkRequiredPlaceholders` + 分发层 CLI 端到端（`toEqual` 精确断言） |
| `--json` 可 parse | 3 例（健康 / fail / `__REQUIRED__` 场景） |
| exit 码（有 fail → 1，仅 warn → 0） | 分发层 4 例 |
| age 检查注入 fake `AgeCryptoPort` 可测 | core 2 例 + CLI 2 例（含「未配置时零调用」） |

## 5. 未决问题 / 交接给 W8-W9

1. **`--offline` 的措辞**：计划只写「offline=true → ok（'已跳过'）」，实现取
   `远端可达性检查已跳过（--offline）`。若 W8/W9 的 e2e 断言需要逐字一致，请以本实现为准
   （`checkRemote` 单测锁定）。
2. **`config` fail 时 `adapters`/`age`/`required` 报 ok + 「已跳过」**：这是一处**措辞层自由裁量**
   ——八项必须齐全（`--json` 形状稳定）但又不能伪造判定结果。若 orchestrator 希望这三项在
   config 不可用时改为 `warn`，需要一次接口级修订（会影响 `ok` 语义：warn 不影响 exit code，
   故 exit 码不变，仅报告呈现变化）——**未私改，上报**。
3. **`checkAdapters` 的 root 展开**：用 `expandHome`（`~` 前缀）。adapter root 指向的目录
   若不存在 → warn；若存在但不是目录 → 也 warn（计划只写「存在性」）。
4. **`remote` 的 URL 解析**：优先 `branch.<name>.remote` + `git remote get-url`，失败才按
   `@{upstream}` 切 `/`（remote 名可能含 `/`，直接切会切错）。计划未规定解析路径，属实现细节。
5. **W6 的 `commitPaths` 与 doctor 无交集**（doctor 只读），P3-W8 的 `home` 需把 `deps.age`
   透传给 `runDoctor(opts, { age })`——签名已就绪。
