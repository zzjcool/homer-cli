# homer-cli

> Dotfiles for humans and their AI agents.

一个 git 仓库管好你所有工具的配置（pi / herdr / opencode / …）。
新机器一条 `homer home` 全部归位，密钥安全送达。

## Status

**M1 已完成**（只读漂移感知）— 设计文档见 [DESIGN.md](./DESIGN.md)，实施计划见 [docs/m1-plan.md](./docs/m1-plan.md)，验收报告见 [docs/m1-report.md](./docs/m1-report.md)

- 市场调研：已完成（3 路并行，覆盖 pi 同步器 / 多 agent 工具 / 传统 dotfiles 管理器 / 商业配置同步机制）
- 定位：git 主通道 + age 密钥层 + tailcat 可选快车道 + 应用感知 adapter
- MVP 验收场景：新机器 `homer home <repo-url>` → pi + herdr + opencode 配置与密钥全部归位

### M1 已实现

```bash
npm install && npm run build
homer init    # 扫描 ~/.pi/agent 生成 homer.json + store 快照
homer status  # 漂移摘要 ↑n ↓n（--json 机器可读，--verbose 分类明细）
homer diff    # 文本级差异（merge 键级 / mirror 行级，⚡ 冲突标记）
```

- 三路判定引擎（§2.7 merge/mirror 矩阵，数组原子值、del-vs-modify 冲突）
- pi adapter 只读扫描（7 分类 + ignore 规则 + symlink 防逃逸/防循环）
- store 原子写入（tmp+rename + 完整性标记）
- 331 个测试（引擎矩阵 / fixture / e2e / 变异测试验证过强度）
- 下一里程碑 M2：push/pull/merge 写路径 + 备份 + 密钥扫描

## Naming

`homer` = homing（归位本能）+ Homeward Bound 的 Homer（归家犬）+ 荷马（《奥德赛》归乡史诗作者）。
npm 包名 `homer-cli`，安装后命令为 `homer`。

## M1 范围

M1 的目标是**跑通「看见漂移」**：仓库结构 + `homer.json` schema + pi adapter 只读 `diff`/`status`。
详细计划见 [docs/m1-plan.md](./docs/m1-plan.md)（含全部冻结 TS 接口）。

**做：**

- `homer init`：非交互扫描 pi adapter，生成 `homer.json` + store 快照
- `homer status`：分类级漂移计数（`push` / `pull` / `conflicts`），支持 `--json`
- `homer diff`：漂移的详细差异（merge 文件按键、mirror 文件按行）
- 三路判定引擎（`merge` / `mirror`）与 pi adapter 只读扫描

**不做（后续里程碑）：**

- 任何写路径：`push` / `pull` / `merge` / 备份 / 密钥扫描（M2）
- git 操作：base 从 git 历史读取（M2；M1 的 base = store 工作区）
- age 密钥、herdr/opencode adapter、pi 扩展形态（M3/M4）
- 交互式 init（M2+；M1 命令全部非交互）

**开发：**

```bash
npm install
npm run typecheck && npm test
npm run build && node bin/homer.js --help
# 未构建时可直接跑源码：
npx tsx src/cli/index.ts --help
```

运行时零依赖（devDependencies 仅供类型检查/测试/开发）。

## License

MIT
