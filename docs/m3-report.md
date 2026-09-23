# M3 验收报告：age 密钥层 + `homer home` + `doctor` + herdr/opencode adapter + install.sh

> 计划依据：`docs/m3-plan.md` §3-P4-W9（MVP e2e 主线 7 项）/ §5（M3 Done 定义）
> 设计依据：`DESIGN.md` §2.6（密钥轮转）/ §3（MVP 验收场景）/ §2.7（首次对接）
> 本报告所属波次：**P4（串行收尾 · W9-integrator）**
> 本分支：`m3-p4-w9-integrator`

---

## 0. 一句话结论

**M3 完成**：MVP 验收场景（DESIGN §3）被 `tests/e2e/m3.test.ts` 的 31 个用例**机器化证明**——
机器 A 装配 → 机器 B `homer home <origin> --yes` 一键归位 → 三工具目录逐字节 == A 侧 store、
密钥明文归位（0600）、`doctor --json` 无 fail、`status` 零漂移；换设备（加 recipient + 重加密）
与 `__REQUIRED__` / symlink 逃逸两条 M1/M2 已知限制均关闭。
全量 **1166 tests 绿**（基线 1132 + W9 新增 34）。

---

## 1. 交付物（按计划 §3-P4-W9 文件归属）

| 文件 | 变更 | 说明 |
|---|---|---|
| `src/cli/commands/init.ts` | ✅ 改 | **仅** `KNOWN_ADAPTERS` 注册 herdr/opencode 两行 + import（计划唯一授权的源码改动） |
| `src/core/doctor/checks.ts` | ✅ 改 | 缝隙修复：`checkAge` 的 vault 取数口径（见 §4） |
| `tests/e2e/m3.test.ts` | ✅ 新增 | MVP 验收场景 e2e 主线（7 组 / 31 例，含双假 HOME + 假 origin + 真子进程 CLI） |
| `tests/core/doctor/checks.test.ts` | ✅ 改 | age 取数口径 3 条回归（upstream 回落 / 双失败 / 工作区可解） |
| `tests/cli/init.test.ts` | ✅ 改 | 增例：`init` 默认注册三 adapter（homer.json 键序断言） |
| `tests/e2e/{m1,m2}.test.ts`、`tests/cli/missing-root.test.ts` | ✅ 改 | 各自口径限定 `--adapters pi`（三 adapter 由 m3.test.ts 覆盖） |
| `tests/core/age/cipher.test.ts` | ✅ 改 | 1 MiB 用例显式 30s 超时（默认 5s 在并行负载下擦边，见 §6-3） |
| `README.md` | ✅ 改 | M3 完成状态 + `home`/`secret`/`doctor` 用法 + `install.sh` + 三 adapter 分类口径 |
| `docs/m3-report.md` | ✅ 新增 | 本报告 |

**未改**：`src/cli/args.ts`、`src/cli/index.ts`（§2.1 冻结，P1-P4 禁改）、`src/core/types.ts`、
各 adapter 的 `defaults.ts`、`install.sh`、`src/cli/commands/{home,secret,doctor}.ts` 的逻辑。

---

## 2. MVP 验收场景 e2e 主线（7 组逐条对照）

文件：`tests/e2e/m3.test.ts`（31 例）。全程隔离：
`<tmp>/origin.git` 假 origin（`git init --bare`，无网络）+ 三台假 HOME（`A`/`B`/`C`）
+ **真实子进程**跑 CLI（`node --import tsx src/cli/index.ts …`，`HOME`/`HOMER_HOME` 指向临时目录）。
`afterAll` 回收全部临时目录；identity 全部进程内临时生成（`secret keygen`），**绝不碰真实 `~/.pi` /
`~/.config/{herdr,opencode}` / `~/.homer`**（⑦ 组是唯一读真实 HOME 的用例，且只读）。

### ① 机器 A 装配 —— 10 例

| 证据 | 断言 |
|---|---|
| `init --json` 三 adapter 全注册 | `adapters` 精确等于 pi(7 分类) + herdr(config=1) + opencode(config/plugins/locks=1)；**首次 init 时 `skills=2`**（逃逸链接尚未放行） |
| init 报告逃逸告警（⑤ 前半） | `errors` 含 `pi` 且含「逃逸」 |
| homer.json | 三 adapter 键序 `['pi','herdr','opencode']`、root 保留 `~` 写法、`secrets.files` == `{a-secret: '~/.secrets/a.env'}`、recipient 匹配 `/^age1[02-9ac-hj-np-z]{58}$/` |
| `secret keygen` | 私钥文件 mode **0600**、内容含 `AGE-SECRET-KEY-`、**报告 JSON 中不含 `AGE-SECRET-KEY`**（私钥永不回显）、`.gitignore` 含 `keys/` |
| `secret push` | `status='pushed'`、`encrypted=[a-secret]`、`pushedToRemote=true`、vault 文件存在且**不含明文片段**、vault 不可执行且 ≤0644 |
| `push` | `status='pushed'`；store 精确清单 10 文件（herdr 1 / opencode 3 / pi 6，含 `skills/agent-browser/{SKILL.md,sub/a.ts}` = ⑤ 后半）；ignore 垃圾一条不进 |
| origin 齐备 | `git ls-tree -r HEAD` 含 `homer.json` + `secrets/a-secret.age` + 全部 store 文件；**无 `keys/`**；`sha(origin) == sha(A)` |
| bare origin 铁证 | `git grep` 明文片段 = 空；`git grep AGE-SECRET-KEY` = 空 |
| A 侧 doctor | `ok=true`、无 fail、`age=ok` |
| A 侧 status | `errors=[]`、三 adapter `↑0 ↓0`（allowEscape 生效后逃逸不再是告警） |

### ② 机器 B 一键归位（**核心场景**）—— 8 例

| 证据 | 断言 |
|---|---|
| `home --yes` | `status='homed'`、`ok=true`、`cloned=true`、`adapterIds=['pi','herdr','opencode']`、`firstContact.mode='merge'`、`errors=[]`、`conflicts=[]`、`deleted=[]`、`written.length == store 文件数` |
| 新设备无 identity | `secrets.skipped=[a-secret]`、`secrets.pulled=[]`、warnings 含 `identity` 与 `recipient` 两步提示（**不 fail 整个 home**） |
| **三工具目录逐字节 == A 侧 store** | 用扫描的逆映射（`resolveCategoryFilePath`）把 store 每个文件反算到工具目录绝对路径，逐字节比对（excludeKeys 文件除外，见下条）；并断言工具目录**无多余文件**（落点清单精确相等） |
| home 刚结束的落点清单 | `beforeAll` 内在其它命令跑之前快照 `homePlacedTree`（10 项），与 store 反算清单逐项相等 |
| `__REQUIRED__` 不变量 | store 的 `pi/models/models.json` 含 `"apiKeys": "__REQUIRED__"`；B 侧工具目录 `models.json` **不含** `__REQUIRED__` 且非密钥键照常归位 |
| **密钥归位** | 换设备两步后 `secret pull` → 目标明文 == A 明文、`mode 0600`、报告 JSON 不含明文片段 |
| B 侧 doctor --json | `ok=true`、无 fail、`age=ok`、`machine=ok`、`adapters=ok`；`required` 仅 warn |
| B 侧 status | 三 adapter 零漂移、`errors=[]`、`warnings=[]`、`state.lastSyncCommit == HEAD`、文本含「无漂移」 |

> **为什么 `home` 之后还有「换设备两步」**：`keys/` 是 gitignored 的机器本地状态，identity **永不随仓库走**，
> 故新设备首次必然要「本机出公钥 → 旧机登记 → 重加密 push → 本机 pull」（DESIGN §2.6 的轮转流程，
> 与 ③ 组同一机制）。`home` 本身已把**配置**一次性归位；密钥通道的两步是该机制的必然形态，
> 且 `home` 会如实报告 `secrets.skipped` + 提示，不静默、不误报失败。

### ③ 换设备 = 加 recipient + 重加密 —— 1 例（2600ms）

C 机器 `home --yes` → `secret keygen` → A 追加 C 公钥（commit）→ A `secret push --yes`
（**密文字节确实变化** + 新 commit + origin 跟上）→ C `secret pull --yes` 成功（明文一致 + 0600）；
**同时** B（旧设备）与 A（原设备）仍能 `secret pull` 成功 —— 多 recipient「旧新设备都可解」铁证；
`secret list` 三相一致；轮转后 origin 依旧 `git grep` 无明文、无私钥。

### ④ `__REQUIRED__` 残留进 doctor（M2 遗留关闭）—— 3 例

- store 里 `apiKeys = "__REQUIRED__"`（证明 push 侧置换真的发生）；
- A 侧 `required` = **warn**、message 含 `__REQUIRED__`、**details 精确等于** `['pi/models/models.json: apiKeys']`、
  不影响 `ok`（`doctor` 退出码 0）；
- B 侧同样 warn 且 details 精确（占位符随仓库跨机可见）。

### ⑤ symlink 逃逸 allowlist（M1 已知限制关闭）—— 4 例

- 无 `allowEscape` → init 跳过 + 记 ScanError（`skills` 只有 alpha/beta，M1 安全边界默认仍生效）；
- `allowEscape: ['skills/agent-browser']` → root 外内容（含子目录）逐字节进 store，`push` 零告警；
- A 侧 `status` 零告警零漂移（逃逸不再是告警）；
- **allowlist 只放行命中的那一条**：临时机器构造 `allowed` / `blocked` 两个逃逸链接，只写
  `['skills/allowed']` → 用真实 CLI 扫描复核：`blocked` 仍报逃逸告警、`allowed` 不报且其内容进入判定
  （store 尚无 → 计 push 1）。

### ⑥ install.sh 冒烟（引用 W4 用例）—— 3 例

- `install.sh` 存在、`sh -n` 语法通过、`#!/bin/sh` 开头、暴露 `HOMER_INSTALL_PACKAGE` /
  `HOMER_INSTALL_PREFIX`、下一步指引含 `homer home`；
- **引用** `tests/e2e/install.test.ts`（W4 既有用例：`npm pack` → 隔离 `NPM_CONFIG_PREFIX` 安装 →
  `$PREFIX/bin/homer --help` exit 0；含幂等与「PATH 无 node」负例）——本组只钉「W4 用例仍在 +
  关键证据字面仍在」，**不重复执行**（避免全量 e2e 时长翻倍）；
- CLI 自身 `--help` 子进程 exit 0 且列出 M3 三命令（`homer home` / `homer doctor` / `homer secret`）。

> 另在 §5 的 Done 命令里**真实执行**了一次全局安装冒烟（见 §3）。

### ⑦ 真实环境只读冒烟（可选）—— 2 例

`HOMER_HOME=<tmp>` + **真实 HOME**（当前进程 `homedir()` 基准，故 init 真扫真实
`~/.pi/agent`、`~/.config/herdr`、`~/.config/opencode`；**写入只落临时 HOMER_HOME**）：

- `init --json` → 三 adapter 齐全；真实存在的 root 分类非空（真扫）；未安装的报 root 不可读；
  断言真实 `~/.homer` **未被创建**；
- `doctor --offline --json` → 八项齐全且顺序固定、`age=未配置密钥同步 → ok`、
  `remote` 为 `远端可达性检查已跳过（--offline）`；唯一可能 fail 的是「临时 HOMER_HOME 不是 git 仓库」。

**实测**（本机真实环境，见 §3.4）：`init` 扫出 pi(settings/skills/extensions/agents/models/prompts/themes)
+ herdr(config) + opencode(config/plugins/locks)。

---

## 3. §5 M3 Done 验收输出（原样）

### 3.1 全量类型检查 + 测试

```bash
$ npm run typecheck && npm test
> tsc --noEmit

 RUN  v5.0.1 /root/code/homer-cli-w9

 Test Files  53 passed (53)
      Tests  1166 passed | 1 skipped (1167)
   Start Time  10:34:06
   Duration  26.94s (tests 76%, transform 23%, import 5%)
```

连跑 3 次全量，均 **1166 passed**（无 flake）。

### 3.2 MVP 主线（`tests/e2e/m3.test.ts`）

```bash
$ npx vitest run tests/e2e/m3.test.ts
 Test Files  1 passed (1)
      Tests  31 passed (31)
   Duration  10.42s
```

### 3.3 `install.sh` 语法 + 真实安装冒烟（§5 原文命令）

```bash
$ sh -n install.sh && echo OK
OK

$ npm run build && npm pack --pack-destination /tmp/w9pack
homer-cli-0.0.0.tgz        # 255475 bytes

$ PREFIX=$(mktemp -d)
$ HOMER_INSTALL_PACKAGE=$(ls /tmp/w9pack/*.tgz) NPM_CONFIG_PREFIX=$PREFIX \
    HOMER_INSTALL_PREFIX=$PREFIX sh install.sh
✓ homer 安装完成（/tmp/tmp.1jnLsduMK6/bin/homer）

下一步：
  1. 新机器一键归位 : homer home <你的配置仓库 url>
  2. 首次建立仓库   : homer init && homer push --yes
  3. 密钥投递       : homer secret keygen / push / pull
  4. 环境体检       : homer doctor
# exit 0

$ $PREFIX/bin/homer --help
homer — dotfiles for humans and their AI agents

用法:
  homer <command> [options]
# exit 0
```

### 3.4 真实环境只读冒烟（人工核验项）

```bash
$ T=$(mktemp -d); HOMER_HOME=$T/homer node --import tsx src/cli/index.ts init --json
{
  "homerHome": "/tmp/tmp.CiadEt2jUe/homer",
  "adapters": [
    { "id": "pi", "categories": [ settings:1, skills:1, extensions:3, agents:8, models:1, prompts:1, themes:0 ] },
    { "id": "herdr",    "categories": [ { "name": "config", "fileCount": 1 } ] },
    { "id": "opencode", "categories": [ config:1, plugins:1, locks:1 ] }
  ]
}

$ HOMER_HOME=$T/homer node --import tsx src/cli/index.ts doctor --offline --json
  config      ok     homer.json 有效（3 个 adapter: pi, herdr, opencode）
  repo        fail   工作区不是 git 仓库: /tmp/…/homer          ← 未 init push，预期
  store-clean warn   store 工作区状态不可判定（不是 git 仓库）
  remote      ok     远端可达性检查已跳过（--offline）
  adapters    ok     3 个 adapter root 均存在
  age         ok     未配置密钥同步（secrets 段为空）             ← ⑦ 组核心
  machine     warn   state.json 未记录同步状态（lastSyncCommit 缺失）
  required    ok     已跳过（config 不可用）… / 未发现残留
# 真实 ~/.pi / ~/.config/{herdr,opencode} 零写入；~/.homer 未被创建
```

### 3.5 bare origin 安全铁证（e2e 内自动化）

```
✓ bare origin 铁证：git grep 无明文片段、无私钥字符串
✓ origin 齐备：store/ + secrets/ + homer.json；keys/ 永不入库
✓ 重加密后 C（新）/ B（旧）/ A（原）三方都能 secret pull，密文确实变化且只含密文
```

---

## 4. 缝隙修复：`doctor` 的 age 检查取数口径（W6 ↔ W7 接缝）

### 4.1 症状（P4 集成时暴露，非任何单模块内部测试能发现）

一台**完全可用**的新机器（`home` → `keygen` → 旧机登记 + 重加密 push → `secret pull` 成功、
目标明文已 0600 归位）执行 `homer doctor` 会得到 **`age: fail`（「1 个密钥无法解密」）**，
且该状态**永久存在**：

```bash
$ homer secret pull --yes      # applied：目标明文已正确归位
$ homer doctor --json | jq '.checks[] | select(.id=="age")'
{"id":"age","status":"fail","message":"1 个密钥无法解密（本机 identity 不是 recipient？）"}
$ homer pull --yes             # no-drift（store 无漂移 → 不前移 HEAD）
$ homer doctor --json | jq ... # 仍然 fail
```

### 4.2 根因（两个模块各自正确，组合起来错）

| 模块 | 冻结语义 | 单独看 |
|---|---|---|
| W6 `secret pull` | 从 **`@{upstream}`** 读密文，**不 ff 整仓**（§1-D4：避免 store 工作区被默默推进） | ✅ 按计划 |
| W2 `homer pull` | `plan.actions.length === 0` → **`no-drift` 提前返回**，不执行 `mergeFfUpstream` | ✅ 按计划 |
| W7 `checkAge` | vault 只从**工作区** `secrets/` 读（`listSecrets` + `decryptSecretFromFile`） | ✅ 按计划 |

新设备的 workspace vault 有两条形成路径都会「陈旧到本机解不开」：

1. **`clone` 时的历史版本**：`home` clone 到 A 在「登记 B 之前」的 commit，工作区 `secrets/a-secret.age`
   只对 A 可解（甚至此时 B 还没 keygen）。随后 A 追加 B recipient + 重加密 push；B `secret pull`
   把**新**密文解出来写目标 —— 但工作区那份**旧**密文始终没被更新（`secret pull` 不 ff、`homer pull` no-drift）。
2. **`keys/` 不在仓库里**：新设备 identity 只能在 clone 之后生成，故 `home` 阶段不可能已经解出密钥
   （这也是 ② 组「home 后还要换设备两步」的原因）。

于是 `doctor` 拿到的是「旧的、本机解不开的」密文 → `decryptSecretFromFile` 抛错 → fail。
这与「刚刚成功解密」自相矛盾，且直接违反 §5 的 Done 判据（机器 B 的 doctor 无 fail）。

### 4.3 修复（`src/core/doctor/checks.ts`，最小改动）

`checkAge` 的 vault 取数改为**「工作区优先，缺失或解不开时回落 `@{upstream}`」**（与 `secret pull` 的
实际取数口径同源；`upstreamRef` + `readVaultFileAtCommit` 都是已有冻结接口，纯本地 `git show`，
**不发网络**，故 `--offline` 下行为一致）：

- 工作区存在 → 仍走 `decryptSecretFromFile`（**错误文案与改动前逐字相同**）；
- 工作区缺失**或解密失败** → 用 `crypto.decrypt(readVaultFileAtCommit(upstream))` 再试；
- 两者都拿不到/都解不开 → 仍 `fail`（真实故障照常报出）；
- 两者都没有该密文 → 仍 `warn`（`missing-vault` 语义不变）。

判定阶梯（identity 缺失 → fail、recipients 空 → fail、未配置 secrets → ok）**一字未动**；
`checkAge` 的**冻结签名未变**（仍是 `(paths, config, crypto)`）。

### 4.4 回归证据（3 条新例，`tests/core/doctor/checks.test.ts`）

| 用例 | 断言 |
|---|---|
| 工作区 vault 缺失但 `@{upstream}` 有密文 | `age = ok`（**变异测试验证**：临时改回「只看工作区」→ 该例变红，改回即绿） |
| 工作区与 `@{upstream}` 都解不开 | 仍 `fail`，message 含「无法解密」、details 含 token |
| 工作区 vault 存在且可解（无 upstream） | `ok`，无回落文案 |

---

## 5. 已知限制（M3 范围内的显式不做 / 残余风险）

计划 §0 Non-goals 的逐条落实：

1. **`home` 不装依赖**（DESIGN §2.3 的「装依赖」延后）：涉及各工具包管理器与网络副作用，风险 > 收益；
   MVP 场景只需配置 + 密钥归位。**M4+ 候选**。
2. **`secrets/` 无三路合并**：secrets 通道无 base 概念，M3 = 整文件覆盖（后写胜）。
3. **`secret pull` 不 ff store 工作区**（§1-D4 刻意设计）：因此工作区的 `secrets/` 可能落后于
   `@{upstream}`。doctor 侧已按 §4 修复为「工作区优先 + upstream 回落」；`homer status` 只看 `store/`，
   不受影响。**用户可见后果**：`homer pull` 不推进 HEAD（工作区 `secrets/` 需手动 `git pull` 或
   依赖 doctor 的回落读取）。
4. **首次对接无逐文件/逐键裁决 UI**：`merge` 模式下冲突文件**整体保留本地**（§1-D5），随后成为
   push 漂移，由 `status` 可见、`merge`/`push` 收敛。
5. **无 canonical JSON 序列化**（维持 M2 决定）；**无 `homer unlock` / 并发锁**（v2）。
6. **opencode 的嵌套 key 不清扫**：`provider.*.options.apiKey` 是嵌套键，M1 冻结的 `excludeKeys`
   只支持顶层 —— 不做引擎扩展（breaking 风险）。真实长 key 由 M2 密钥扫描闸门在 push 时拦截；
   短占位值（如 `"ccrb"`）不命中。**建议**：真实 key 走 secret 通道或改环境变量引用。
7. **`homer pair` / tailcat 快车道**：M5（不在 M3）。
8. **⑦ 组依赖真实 HOME 内容**：CI（无真实工具安装）时降级为「隔离假 HOME 下三 adapter 全注册」，
   不假装扫到内容（用例内有显式分支）。

---

## 6. 过程中发现并处理的问题

1. **`init` 注册 herdr/opencode 引发 10 个既有测试失败**（预期副作用）：M1/M2 的 e2e 用
   `Object.keys(config.adapters)).toEqual(['pi'])`、store 精确清单、`status.adapters[0]` 等钉死了
   「只有一个 adapter」。处理：**不改断言语义**，而是给这些用例加 `--adapters pi`，显式限定它们
   各自的验证口径（pi 扫描语义 / M2 往返），三 adapter 的默认注册由 `m3.test.ts` 断言。
   `tests/cli/init.test.ts` 则**增例**记录新的默认键序 `['pi','herdr','opencode']`。
2. **`checkAge` 的 W6↔W7 接缝**（§4）：真实 bug，已修 + 3 条回归 + 变异验证。
3. **`tests/core/age/cipher.test.ts` 的 1 MiB 用例 5s 擦边超时**：该用例本机耗时 ≈5.0s，
   默认 `testTimeout: 5000` 在 M3 e2e 加入的额外并行负载下会间歇性超时（master 上单独跑也接近上限）。
   处理：给该用例显式 `30_000` 超时（不改断言、不改 `vitest.config.ts` 全局值）。
4. **自身测试 harness 的一个严重 bug（已修）**：`home` 后的落点快照最初写成
   `[...piRoot(B), herdrRoot(B), opencodeRoot(B)]` —— 对**字符串**用展开运算符会把路径拆成单个字符，
   于是 `listFiles('/')` 遍历了整个文件系统（45,706,945 项 / 6.8GB 堆），表现为 vitest worker OOM、
   单文件跑 195s。改为 `[piRoot(B), herdrRoot(B), opencodeRoot(B)]` 后单文件 **10s** 跑完。
   （记录在此：这类「harness 自身把根目录当输入」的错误极难从 OOM 表象倒推。）
5. **`doctor` 在非 git 仓库上 exit 1**（`config`/`repo` fail 时）是**有意判定**，故 ⑦ 组的
   `doctor` 调用显式容忍 exit 1 并解析 `--json` 报告。

---

## 7. 未决问题（交接给 orchestrator）

1. **`checkAge` 的取数口径修复是否需要写入计划**：§2.5 只写「有 vault 文件且全部试解密」，
   未规定 vault 取数来源。本修复按「与 `secret pull` 同源（工作区优先 + upstream 回落）」裁定，
   且已在 `checks.ts` 文件头、§4 与本报告显式记录。**签名未变、单模块测试全绿、不变式不破坏**，
   故按 AGENTS.md「缝隙修复按需修」执行；若 orchestrator 认为该口径应固化为计划条款，请回写
   `docs/m3-plan.md` §2.5。
2. **`decryptSecretFromFile` 仍只吃工作区文件**（W6 报告 §5-4 已记录）：doctor 现在的回落路径
   直接用 `crypto.decrypt` + `loadIdentity`（与 `secret pull` 同款），未给 `decryptSecretFromFile`
   加 commit 参数（那会改 §2.2 冻结签名）。如需统一入口，属接口级修订。
3. **⑦ 组是「只读冒烟」而非「真实验收」**：真实环境依赖本机已装 pi/herdr/opencode；
   CI 下自动降级。M3 的机器化证明是 ①②③（双假 HOME + 假 origin）。
4. **`homer pair` / tailcat / pi 扩展形态 / adapter 插件机制**仍是 M4/M5 范围。
