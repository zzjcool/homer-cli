# homer-cli M1 验收报告（P2 · M5-integrator）

> 阶段：`docs/m1-plan.md` §2-P2 / §4（M1 Done 定义）
> 基线：master `3ed2d11`（P1 四模块已合并：engine `035a607` / pi-adapter `b65ae5e` / store `7b41a76` / cli `3ed2d11`，全量 238 tests 绿）
> 提交：`feat(m1): e2e acceptance + integration fixes + m1-report`（本分支最新提交；哈希见 `git log -1 --format=%H`，避免自我引用导致哈希漂移）
> 结论：**M1 Done 达成**。`typecheck` + 全量 `npm test`（257 tests，含 19 项 e2e）绿；§4 三条机器可验收标准逐条通过。

---

## 1. 做了什么

### 1.1 新增 `tests/e2e/m1.test.ts`（计划 §2-P2 原文验收，19 个用例）

全程 `mkdtempSync` 临时目录 + `HOME` / `HOMER_HOME` 双隔离（假 HOME 下的 `.pi/agent` 才是 pi adapter 的 `~/.pi/agent`），绝不触碰真实 `~/.pi` / `~/.homer`。用例分组：

| 分组 | 覆盖 |
|---|---|
| `homer init` | store 出现 7 分类文件（§1.6 布局**硬编码绝对路径**精确匹配）；7 分类目录齐全；ignore / exclude 垃圾 0 条；`homer.json` 合法（`validateConfig` ok、version 1、root 保留 `~/.pi/agent`、7 分类与默认配置逐字一致）；init 后立即 status 无漂移且 diff 空 |
| 漂移 → status | 改 `settings.json` 一个键 + 新增 `skills/gamma/SKILL.md` + 删 `skills/beta/` 整目录 + 直改 store 中 `themes/dark.json` → **精确断言 `↑4 ↓0`，逐分类 `settings ↑1 / skills ↑2 / themes ↑1`，其余 4 分类全 0**；人类可读输出与 JSON 计数一致 |
| status --json | `JSON.parse` 可解析、分类顺序=`settings,skills,extensions,agents,models,prompts,themes`、计数与 `--verbose` 文本逐行一致 |
| diff | 含该键新旧值 `theme: light → dark`（且 merge 键行**唯此一行**）；skills 的删除 `-# beta` + 新增 `+# gamma`；themes 的 store 侧旧值 / 本地新值；无漂移分类（prompts / extensions / agents / models）输出**空字符串**；`--adapter` / `--category` 过滤生效 |
| 缝合点回归 | init 时缺失的 merge 文件之后新增 → 只算 `push 1` 不误报冲突；`store` 缺分类目录；pi root 不存在；引擎级 base 缺失三态 6 例 |
| 进程级入口 | 真实子进程 `node --import tsx src/cli/index.ts init --json` → 退出码 0 + stdout 合法 JSON；同 `HOMER_HOME` 下 `status --json` parseable；无 config → 退出码 1 |

`models.json` 只改 `excludeKeys: ['apiKeys']` 里的键，e2e 断言其为 **0 漂移**，锁死「剥离后才比较」的接缝。

### 1.2 缝隙修复（1 处真实 bug）

**`src/core/engine/drift.ts` — merge 分类「base 缺失」分支顺序错误（P1 模块间接缝 bug）。**

- 现象（修复前，e2e 实测）：`homer init` 时 `~/.pi/agent` 里没有 `settingX.json`，之后本地新建该文件 → status 报 `push 1 + conflicts 1`，`mergeConflicts: [{keyPath: 'settings.json', reason: 'modify-vs-delete'}]`。这是**误报**：本地新增文件被当成了「本地删 / 远端改」的真歧义。
- 根因：`accumulateMergeFile` 里 `r === undefined`（远端删除）分支写在 `b === undefined`（base 缺失）分支**之前**。M1 中 `remote 缺省 = base`（§1.4），base 缺失 ⇒ remote 也缺失，于是「本地新增」直接落进 `r === undefined` 分支。
- 修法：把 base 缺失分支**上移**到单边删除分支之前，并对齐 §1.3 `mirror.compareFile` 的 `baseAbsent` 语义 —— 仅 local → push（已计）；仅 remote → `pull += 1`；两侧都有且内容不同 → `both-modified` 冲突。原来 `l.content !== r.content` 的 compare 逻辑原样保留在该分支内。
- 影响面：只影响「首次对接（base 缺失）」路径；base 存在时的三路语义（DESIGN §2.7 矩阵）完全不变。既有 engine 矩阵、CLI、store、pi、e2e 全部保持绿。
- **未改** `src/core/types.ts`（`git diff --stat` 为空），也未改任何冻结接口签名；只改实现内部分支顺序。

修复前后对比（同一构造）：

```
修复前  base 缺失 + 仅 local 有 settings.json →  push 1 / pull 0 / conflicts 1  ❌ 误报 modify-vs-delete
修复后  base 缺失 + 仅 local 有 settings.json →  push 1 / pull 0 / conflicts 0  ✅
```

### 1.3 其它

- `.gitignore`：补 `.pi/`（herdr subagent 运行目录）与 `.vitest/`，避免本地工具产物被 `git add -A` 误提交。这两个目录在本机工作区真实存在（`.pi/subagents/runs/**`），属清理而非功能改动。

---

## 2. 歧义复核：`readSnapshotFromStore` 遇 store 缺目录

**M3-store 上报的歧义**：store 里缺某个分类目录时，(a) 返回该分类的**空 `Map`**（M3 的选择），还是 (b) **跳过该分类**（快照中不出现该 `CategorySnapshot`）？

**复核方法**：不靠推理，直接用 e2e 实际行为裁定（`tests/e2e/m1.test.ts` 两个用例 + 引擎对照实验）。

对照实验（同样构造下差异只在 `CategorySnapshot` 是否出现）：

| store 状态 | 语义 | status 分类列表 | 计数 |
|---|---|---|---|
| 分类目录缺 + 本地有文件 | (a) 空 Map | **含该分类** | `push 1` |
| 分类目录缺 + 本地有文件 | (b) 跳过 | 含该分类（`local` 侧仍贡献该分类名，引擎取并集） | `push 1` |
| 分类目录缺 + 本地也空 | (a) 空 Map | 含该分类 | 全 0 |
| 分类目录缺 + 本地也空 | (b) 跳过 | 含该分类 | 全 0 |
| 整个 root 缺失（local 0 分类）+ 分类目录缺 | (a) 空 Map | **7 分类全列** | 全 0 |
| 整个 root 缺失（local 0 分类）+ 分类目录缺 | (b) 跳过 | **只剩 6 分类（缺的那类消失）** | 全 0 |

**裁定：M3 的「空 Map」选择是正确的**，理由：

1. **计数等价，但结构不等价**。引擎按 adapter / category **并集**遍历，所以两种选择在「local 侧有该分类」时计数完全一致（对照实验前 4 行）。真正的分歧只在 `local` 侧也没有该分类时（如 pi root 缺失）暴露：跳过会让该分类从 `StatusReport` 里**整体消失**，破坏 §1.7 冻结的「status 分类结构由 config 声明决定」这一契约 —— 用户会看到 6 个分类而非配置里写的 7 个，`status --json` 的输出形状随磁盘偶然状态漂移。
2. **错误方向更安全**。「空 Map」把缺失目录解释成「base 里该分类没有内容」，于是本地新增内容被判成 `push`（有待推送的东西）**而不是静默漏报**；「跳过」在极端情况下会把该分类的漂移整个吞掉。
3. **与 §1.6 的一致性**：`writeSnapshotToStore` 对空分类会「创建空目录，使 roundtrip 结构稳定」；读侧返回空 Map 与写侧同一口径，`write → read` 结构稳定不变形。

结论已固化为 e2e 回归用例：`tests/e2e/m1.test.ts` →「store 缺分类目录 → 该分类仍出现且 local 内容计为 push（M3 空 Map 语义）」。

---

## 3. 验证输出（原样）

### 3.1 `npm run typecheck`

```
$ npm run typecheck

> homer-cli@0.0.0 typecheck
> tsc --noEmit

exit=0
```

### 3.2 `npm test`（全量，含 e2e）

```
$ npm test

> homer-cli@0.0.0 test
> vitest run


 RUN  v5.0.1 /root/code/homer-cli


 Test Files  13 passed (13)
      Tests  257 passed (257)
   Start at  12:13:38
   Duration  1.07s (transform 59%, tests 23%, import 15%, worker 2%)
```

（基线 238 → 257：新增 19 个 e2e 用例，P1 既有 238 个全部保持通过。）

### 3.3 M1 Done §4 机器可验收标准（逐条，`node bin/homer.js`）

```
$ npm run build && node bin/homer.js --help
homer — dotfiles for humans and their AI agents

用法:
  homer <command> [options]
...

$ H=$(mktemp -d); HOMER_HOME="$H" node bin/homer.js init --json
HOMER_HOME=/tmp/tmp.Q11PXbiAWp
init exit=0
valid JSON, homerHome= /tmp/tmp.Q11PXbiAWp adapters= [ 'pi' ]

$ HOMER_HOME="$H" node bin/homer.js status --json
status exit=0
counts [{"id":"pi","push":0,"pull":0,"conflicts":0}]
STORE_CATEGORIES=agents extensions models prompts settings skills themes
```

✅ 退出码 0 + stdout 合法 JSON ✅ `status --json` parseable + 计数正确 ✅ 7 分类齐全。

### 3.4 e2e 单跑

```
$ npx vitest run tests/e2e
 Test Files  1 passed (1)
      Tests  19 passed (19)
```

### 3.5 真实环境只读冒烟（§2-P2 第 3 条）

本机 `~/.pi/agent` 是真实数据，仅执行只读扫描；`HOMER_HOME` 指向 `/tmp`，store 不落真实 HOME。

```
$ HOMER_HOME=/tmp/homer-smoke-$$ node bin/homer.js init --json     # exit=0
JSON OK; per-category fileCount: settings=1 skills=2 extensions=3 agents=8 models=1 prompts=1 themes=0

$ HOMER_HOME=/tmp/homer-smoke-$$ node bin/homer.js status
pi  ↑0 ↓0
无漂移
status exit=0
```

store 内容（17 个文件）与垃圾检查：

```
$ find /tmp/homer-smoke-$$ -type f | sed 's|/tmp/homer-smoke-$$/||' | sort
homer.json
store/pi/agents/advisor.md
store/pi/agents/designer.md
store/pi/agents/planner.md
store/pi/agents/prototype.md
store/pi/agents/reviewer.md
store/pi/agents/scout.md
store/pi/agents/search.md
store/pi/agents/worker.md
store/pi/extensions/herdr-agent-state.ts
store/pi/extensions/pi-autoresearch.json
store/pi/extensions/pi-sysmon.ts
store/pi/models/models.json
store/pi/prompts/ls-branch.md
store/pi/settings/settings.json
store/pi/skills/agent-browser/SKILL.md
store/pi/skills/herdr/SKILL.md

$ find /tmp/homer-smoke-$$ \( -name 'auth.json' -o -name 'trust.json' -o -name '*.bak*' \
    -o -name '*.log' -o -name 'run-history.jsonl' -o -path '*sessions*' -o -path '*npm*' \) | wc -l
0
```

✅ status `↑0 ↓0` ✅ store **无** `auth.json` / `trust.json` / `sessions/` / `npm/` / `*.bak*` / `*.log` / `run-history.jsonl`。真实 `~/.pi/agent/settings.json` mtime 未被改变（只读）。

### 3.6 硬约束核验

```
$ git diff --stat src/core/types.ts
(空)
```

✅ 未改 `src/core/types.ts`，未改任何冻结接口签名。

---

## 4. 已知限制（M1 边界，均属计划 Non-goals）

1. **`homer status` 的 `↓`（pull）恒为 0**：M1 中 `remote 缺省 = base`（§1.4），`base == remote` 时不可能存在「仅远端变化」，故 CLI 生产路径的 `pull` 恒 0。`pull` 计数逻辑本身已被 P1 engine 单测与本次 e2e 引擎级回归（显式传入 `remote`）覆盖，M2 接 git reader 后即可显式触发。
2. **直改 store 记 `push` 而非 `pull`**：同上，M1 的 store 即 base；`themes/dark.json` 被直改时，漂移被表达为「本地相对 base 的变更 = push」。语义到 M2 引入真正的 remote 后才分离。
3. **e2e 用「删 `skills/beta/` 整个目录」而非「删单个文件」**表达 mirror 删除：目录型 category 的 relPath 为 `beta/SKILL.md`，两种删法在引擎里都归 `push-delete`；选目录整删是为了同时覆盖 scan 侧子树消失的路径。
4. **`extensions` 的 `exclude: ['*cache*']` 语义偏宽**：除文件名外还匹配任一路径段，`cache/` 整棵会被排除。这是 `scan.ts` 对 §1.5 冻结 glob 的既定实现（已有 P1 测试锁定），本次未改。
5. **`pi root` 不存在时 `homer init` 仍 exit 0**（写空快照 + error 只记在 `ScanOutcome.errors`，CLI 未透出）。§2-P2 §4 未要求 exit≠0，且「root 不存在」在 M1 语义下等价于「该工具未安装」，故保守保持现状；若 M2 希望 init 提示，需要产品决策而非本次私自改约定。
6. **无 git remote / `state.json` 语义**：`state.json` 已在 `HomerPaths` 中就位但 M1 未写入（Non-goal），M2 记录「上次同步 commit」时启用。
7. **未做交互式 init / `--force` 之外的覆盖策略**：按 Non-goal，`@clack/prompts` 交互层留待 M2+。

---

## 5. 结论

- M1 Done（§4）三条机器可验收标准全部通过：全量 `typecheck && test` 绿（257 tests，13 files）、`init --json` 退出码 0 + 合法 JSON、`status --json` parseable + 计数正确。
- 发现并修复 1 处 P1 接缝真实 bug（`drift.ts` base 缺失分支顺序 → `homer init` 后新增 merge 文件被误报 `modify-vs-delete` 冲突），已附引擎级回归用例。
- M3-store「store 缺目录返回空 Map」的歧义经 e2e 实测裁定为**正确**，理由与回归用例见 §2。
- 未改 `src/core/types.ts` 与任何冻结签名；无未决接口冲突需要上报。
- 变更集（4 文件）：`tests/e2e/m1.test.ts`（新增）、`docs/m1-report.md`（新增）、`src/core/engine/drift.ts`（接缝修复）、`.gitignore`（忽略 `.pi/` / `.vitest/`）。

### 未决问题

1. **无法开 MR/PR**：本仓库 `git remote -v` 为空（`/root/code/homer-cli` 是本地仓库，无 origin），因此无法 push 分支 / 开 MR。提交已落在当前 `master`（`git log -1`，提交信息 `feat(m1): e2e acceptance + integration fixes + m1-report`）。若需要 MR 流程，请先配置 remote 后由 orchestrator 指定目标分支，我再补 push + MR（本地不自行 merge，也不 push 到 main/master）。
2. **`homer init` 在 pi root 缺失时静默 exit 0**：见 §4 第 5 条，需产品决策（M2+）。
3. **真实环境冒烟为只读单次**：未对真实 `~/.pi/agent` 做任何写操作，因此「store 与真实目录长期一致性」未验证（M2 写路径的事）。
