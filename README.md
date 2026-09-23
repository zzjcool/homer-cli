# homer-cli

> Dotfiles for humans and their AI agents.

一个 git 仓库管好你所有工具的配置（pi / herdr / opencode / …）。
新机器一条 `homer home` 全部归位，密钥安全送达。

## Status

**M2 已完成**（同步内核：push / pull / merge / 备份 / 密钥扫描）— 设计文档见 [DESIGN.md](./DESIGN.md)，
实施计划见 [docs/m2-plan.md](./docs/m2-plan.md)，验收报告见 [docs/m2-report.md](./docs/m2-report.md)
（M1 报告：[docs/m1-report.md](./docs/m1-report.md)）

- 市场调研：已完成（3 路并行，覆盖 pi 同步器 / 多 agent 工具 / 传统 dotfiles 管理器 / 商业配置同步机制）
- 定位：git 主通道 + age 密钥层 + tailcat 可选快车道 + 应用感知 adapter
- MVP 验收场景：新机器 `homer home <repo-url>` → pi + herdr + opencode 配置与密钥全部归位

### M2 已实现（安全往返：本地 ⇄ store ⇄ git 远端）

```bash
npm install && npm run build
homer init    # 扫描 ~/.pi/agent 生成 homer.json + store 快照
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

两者都是**冻结的保守口径**（宁可漏报也不误报普通文本），M2 不改正则；若需更强覆盖，M3 的 `secret` 子命令会提供显式扫描入口。

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
- 下一里程碑 M3：age 密钥层 + `homer home` / `doctor` / `secret` / `pair` + herdr adapter

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

- `secrets/` 目录与 age 加密层、`homer home` / `doctor` / `secret` / `pair`（M3）
- `homer sync` 智能编排命令（M2 的 pull+merge 已覆盖判定与写路径核心）
- herdr / opencode adapter、tailcat 快车道、adapter 插件机制（M5）
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
