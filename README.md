# homer-cli

> Dotfiles for humans and their AI agents.

一个 git 仓库管好你所有工具的配置（pi / herdr / opencode / …）。
新机器一条 `homer home` 全部归位，密钥安全送达。

## Status

**M3 已完成**（密钥层 + `home` 一键归位 + `doctor` + herdr/opencode adapter + curl 安装脚本）——
**MVP 验收场景达成**：新机器 `curl install.sh | sh` → `homer home <repo-url>` → 三工具配置 + 密钥全部归位。
设计文档见 [DESIGN.md](./DESIGN.md)，实施计划见 [docs/m3-plan.md](./docs/m3-plan.md)，
验收报告见 [docs/m3-report.md](./docs/m3-report.md)
（M2 报告：[docs/m2-report.md](./docs/m2-report.md)，M1 报告：[docs/m1-report.md](./docs/m1-report.md)）

- 市场调研：已完成（3 路并行，覆盖 pi 同步器 / 多 agent 工具 / 传统 dotfiles 管理器 / 商业配置同步机制）
- 定位：git 主通道 + age 密钥层 + tailcat 可选快车道 + 应用感知 adapter
- MVP 验收场景：新机器 `homer home <repo-url>` → pi + herdr + opencode 配置与密钥全部归位

### M3 已实现（一键归位：安装 → clone → 配置 + 密钥）

#### 一键安装

```bash
curl -fsSL https://raw.githubusercontent.com/<user>/homer-cli/main/install.sh | sh
# 等价手动：npm install -g homer-cli
```

脚本检测 `node >= 20` + `npm` → 全局安装 → 冒烟跑 `homer --help` → 打印下一步。
可测性注入位（CI / 内网）：`HOMER_INSTALL_PACKAGE`（默认 `homer-cli@latest`，可传本地 tarball / URL）、
`HOMER_INSTALL_PREFIX`、`HOMER_INSTALL_REGISTRY`。

#### 新机器一键归位

```bash
homer home git@github.com:me/my-config.git --yes
# → clone 配置仓库 → 三 adapter 配置归位（冲突保留本地）→ 解密密钥写回目标（0600）→ doctor 体检
homer doctor                 # 八项体检：config/repo/store-clean/remote/adapters/age/machine/required
```

- **不装工具依赖**（M3 明确不做）：只归位配置 + 密钥，依赖安装交给各工具自己的包管理器
- 首次对接三选一：`--mode pull`（远端覆盖）/ `merge`（合并保留本地，`--yes` 的默认）/ `skip`
- `homer init` 默认注册 **pi / herdr / opencode** 三个 adapter（`--adapters pi` 可限定）
- herdr：只同步 `config.toml`（mirror）；session/sock/log/release-notes 属运行时状态，忽略
- opencode：`opencode.json` + `package.json`（merge）、`package-lock.json` / `bun.lock`（mirror）；`node_modules/` 忽略
- **symlink 逃逸 allowlist**：adapter 的 `allowEscape: ['skills/agent-browser']` 显式放行 root 外的链接
  （默认仍是安全边界：跳过 + 记告警）

#### 密钥投递（age 加密，明文从不进 git）

```bash
homer secret keygen                  # 生成本机 X25519 identity → <home>/keys/age.txt（0600，gitignored）
homer secret push --yes              # secrets.files 的明文 → 用全部 recipients 加密 → secrets/<name>.age 入库
homer secret pull --yes              # 从远端读密文 → 本机 identity 解密 → 写回目标路径（0600，覆盖前备份）
homer secret list                    # 列出配置的密钥与 vault 文件状态
```

`homer.json` 的密钥段：

```jsonc
{
  "secrets": {
    "recipients": ["age1...", "age1..."],        // 公钥随仓库走；换设备 = 追加新公钥 + 重新 push
    "files": { "openai": "~/.config/openai/key" }, // secret 名 → 目标绝对路径（'~' 或 '/' 开头）
    "ignorePaths": ["pi/settings/settings.json"]  // 可选的密钥扫描豁免（store 相对路径）
  }
}
```

- 私钥永不入库、永不回显：`keys/` 由 `.gitignore` 排除，报告只含 `age1...` 公钥
- 密钥扫描的**第 13 条 pattern** 拦 `AGE-SECRET-KEY-...` 误入 store
- **换设备 = 加 recipient + 重加密**：新机 `secret keygen` → 旧机把新公钥追加进 `secrets.recipients`
  并 `secret push`（多 recipient 重新加密）→ 新机 `secret pull`。旧设备仍能解密（不撤销）
- `secrets/` 与 `store/` **零耦合**：`secret push` 只 commit `secrets/`，`secret pull` 不动 store 工作区
- 首次归位若本机尚无 identity：`home` 仍把**配置**全部归位，密钥进 `secrets.skipped` + 提示两步补齐
  （配置已可用，密钥可后补，不阻塞）

#### `doctor` 八项体检

| 项 | 含义 | 判定 |
|---|---|---|
| `config` | `homer.json` 存在且合法 | 缺失 / 非法 → fail |
| `repo` | home 是 git 仓库根 | 非仓库 → fail；无 upstream → warn |
| `store-clean` | `store/` 工作区干净 | 脏 → warn |
| `remote` | 远端可达（`git ls-remote`，10s） | `--offline` → 跳过；不可达 → warn |
| `adapters` | 各 enabled adapter root 存在 | 缺失 → warn（工具未安装？） |
| `age` | identity 与密文可解（工作区优先、回落 `@{upstream}`） | identity 缺失 / 无可解来源 → fail |
| `machine` | `state.lastSyncCommit` 与 HEAD | 缺失 / 落后 → warn |
| `required` | store 里的 `__REQUIRED__` 占位符残留 | 有 → warn（details 精确到文件 + 键） |

退出码：**无 fail → 0**（含仅 warn，回应 M2 遗留的「离线/未安装不应算失败」）；有 fail → 1。
`--offline` 跳过远端可达性（CI / 断网）。

#### MVP 主线（`tests/e2e/m3.test.ts` 自动化，31 个用例）

1. **机器 A 装配**：三工具 fixture → `init`（三 adapter）→ `secret keygen` → 写 `secrets.*` → 明文落目标
   → `secret push --yes` → `push --yes` → origin 齐备（`store/` + `secrets/` + `homer.json`，`git grep` 无明文、无私钥）
2. **机器 B 一键归位**：全新假 HOME → `homer home <origin> --yes` → 三工具目录逐字节 == A 侧 store
   → 换设备两步后密钥明文 == A 明文且 0600 → `doctor --json` 无 fail → `status` 零漂移 + `state == HEAD`
3. **换设备**：C `keygen` → A 追加公钥 + 重加密 `secret push` → C `secret pull` 成功，且 B / A 仍可解
4. **`__REQUIRED__` 残留**：`doctor` 的 `required` warn，details 精确到 `pi/models/models.json: apiKeys`
5. **symlink 逃逸 allowlist**：allowEscape 命中后 root 外内容入 store，未命中仍按逃逸跳过
6. **install.sh 冒烟**：`sh -n` + 注入位契约 + W4 的 tarball 安装用例（隔离 prefix）
7. **真实环境只读冒烟**：临时 `HOMER_HOME` 扫出三 adapter；`doctor --offline` 报 age 未配置 ok

### M2 已实现（安全往返：本地 ⇄ store ⇄ git 远端）

```bash
npm install && npm run build
homer init    # 扫描 ~/.pi/agent（+ herdr / opencode）生成 homer.json + store 快照
homer push    # 本地快照 → 密钥扫描（命中拒推）→ store → git commit(+push) → state
homer pull    # git fetch → 三路判定 → 预览/确认 → 备份 → 应用到工具目录（含删除传播）
homer merge   # 逐项裁决冲突（--accept-local / --accept-remote 批量降级）
homer status  # 漂移摘要 ↑n ↓n（git 模式下 ↓ 反映远端未拉变更）
homer diff    # 文本级差异（merge 键级 / mirror 行级，⚡ 冲突标记）
```

- `~/.homer` 本身是 git 仓库；`homer push` 首次运行自动 `git init`（无 remote 时进入 local-only 模式）
- base 升级为 `state.json.lastSyncCommit` 指向的 git 历史（缺 state 时回落 store 工作区 = M1 语义）
- 应用远端变更前把受影响的现有文件备份到 `~/.homer/backups/<date>/`，按日期保留最近 7 份
- 密钥扫描 ≥10 类 API key/token 正则，push 前强制扫描；`homer.json` 的 `secrets.ignorePaths` 可精确豁免
- state.json / backups/ 由 `~/.homer/.gitignore` 排除，不入库

#### 密钥扫描口径（冻结的保守口径，已知边界）

扫描的是**将要写入 store 的字节**（`excludeKeys` 已换成 `__REQUIRED__` 占位符之后），命中即拒推（exit 1）。
`homer merge` 的写 store 路径与 push 共用同一道闸门（不会绕过）。两条刻意的边界：

- **未引号的赋值不在拦截范围**：兜底的通用 `KEY=value` 风格只拦截**带引号**的值
  （`api_key: "..."` / `secret='...'`）。`API_KEY=abc...` 这类不带引号的写法不命中，
  因为无引号版本在 shell / YAML / Markdown 里的误报率过高（会把普通句子当成密钥）。
  带具体前缀的 12 类 pattern（`sk-ant-` / `ghp_` / `AKIA` / `AIza` …）不受此限制，仍会命中。
- **同行多密钥先命中先报**：每行按 pattern 顺序匹配，**首个命中即报告并停止该行**。
  因此一行里同时写 `sk-ant-...` 与 `ghp_...` 只报第一个；其余密钥的移除由下次重扫（改完再 push）兜底。

两者都是**冻结的保守口径**（宁可漏报也不误报普通文本）；M3 新增 age 私钥 pattern（第 13 条）作为补充。

### M1 已实现

```bash
homer init    # 扫描 ~/.pi/agent 生成 homer.json + store 快照
homer status  # 漂移摘要 ↑n ↓n（--json 机器可读，--verbose 分类明细）
homer diff    # 文本级差异（merge 键级 / mirror 行级，⚡ 冲突标记）
```

- 三路判定引擎（§2.7 merge/mirror 矩阵，数组原子值、del-vs-modify 冲突）
- pi adapter 只读扫描（7 分类 + ignore 规则 + symlink 防逃逸/防循环）
- store 原子写入（tmp+rename + 完整性标记）
- 723 个测试（引擎矩阵 / fixture / e2e 含假 origin 完整往返 / 变异测试验证过强度）
- 下一里程碑 M3：age 密钥层 + `homer home` / `doctor` / `secret` + herdr adapter → **M3 已完成**（见上方 Status）
## Naming

`homer` = homing（归位本能）+ Homeward Bound 的 Homer（归家犬）+ 荷马（《奥德赛》归乡史诗作者）。
npm 包名 `homer-cli`，安装后命令为 `homer`。

## M2 范围

M2 的目标是**跑通「安全往返」**：本地 ⇄ store ⇄ git 远端 的完整写路径，
含密钥扫描拒推、应用前备份、交互式冲突解决。详细计划见 [docs/m2-plan.md](./docs/m2-plan.md)
（含全部冻结 TS 接口），验收报告见 [docs/m2-report.md](./docs/m2-report.md)。

**做：**

- `homer push`：本地快照 → 密钥扫描 → store → git commit(+push) → state
- `homer pull`：git fetch → 三路判定 → 预览/确认 → 备份 → 应用（含删除传播）→ ff → state
- `homer merge`：交互式冲突解决（Accept Local / Accept Remote；`--accept-*` 批量降级）
- 备份 + 按日期保留策略（默认 7 份）；≥10 类密钥正则扫描拒推
- `status` / `diff` 接入 git remote（`↓` 计数激活）

**不做（后续里程碑）：**

- ~~`secrets/` 目录与 age 加密层、`homer home` / `doctor` / `secret`~~ → **M3 已交付**（`homer pair` 仍留 M5）
- `homer sync` 智能编排命令（M2 的 pull+merge 已覆盖判定与写路径核心）
- ~~herdr / opencode adapter~~ → **M3 已交付**；tailcat 快车道、adapter 插件机制（M5）
- pi 扩展形态 / `--resolve <json>` 回调协议（M4）
- 并发锁 / `homer unlock`（v2）

## M1 范围

M1 的目标是**跑通「看见漂移」**：仓库结构 + `homer.json` schema + pi adapter 只读 `diff`/`status`。
详细计划见 [docs/m1-plan.md](./docs/m1-plan.md)（含全部冻结 TS 接口）。

**做：**

- `homer init`：非交互扫描 pi adapter，生成 `homer.json` + store 快照
- `homer status`：分类级漂移计数（`push` / `pull` / `conflicts`），支持 `--json`
- `homer diff`：漂移的详细差异（merge 文件按键、mirror 文件按行）
- 三路判定引擎（`merge` / `mirror`）与 pi adapter 只读扫描

**开发：**

```bash
npm install
npm run typecheck && npm test
npm run build && node bin/homer.js --help
# 未构建时可直接跑源码：
npx tsx src/cli/index.ts --help
```

运行时依赖仅 `@clack/prompts`（交互层，无 TTY 时自动降级为非交互）；
`--yes` / `--accept-local` / `--accept-remote` 提供完全无交互的降级路径。

## License

MIT
