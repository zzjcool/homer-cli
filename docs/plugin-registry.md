# 插件注册源（pluginregistry）

> 状态：已落地（2026-10-11）。架构决策记录见 [2026-10-11-plugin-architecture.md](plan/2026-10-11-plugin-architecture.md)。

`internal/pluginregistry` 是官方插件的**单一注册源**：内置适配器（adapter）、
随行载体（carrier）、动作（action）都在这一个包里声明，其余系统从它派生，
不再有散落的平行清单。

## 官方目录

| id | role | 名称 | 内容 |
|----|------|------|------|
| `pi` | adapter | pi | `~/.pi/agent` 十个分类（settings/context/skills/extensions/agents/packages/models/prompts/themes/files） |
| `herdr` | adapter | herdr | `~/.config/herdr` config 分类 |
| `opencode` | adapter | opencode | `~/.config/opencode` config/plugins/locks |
| `vscode` | adapter | VS Code | 平台相关 root，settings/keybindings/extensions(manifest) |
| `keyring` | carrier | 密钥环 | `~/.homer/keyring` items——跟随同步的加密密钥载体 |
| `ssh-key` | action | 登录公钥 | 表单：GitHub 用户名；落到机器 `ssh-key` 方法 |

顺序即 `Builtins()` 的稳定展示顺序。

## 提供的接口

- `Builtins() []Plugin` —— 全部官方插件（深拷贝）
- `Builtin(id) (Plugin, bool)` —— 按 id 查找
- `Tools() []adapter.Tool` —— 机器上探测/升级的程序注册表（pi/herdr/opencode/vscode），由 toolctl 消费
- `ToolByID(id)`、`OfficialInstall(binary)` —— 工具查询
- `PluginCredentialFiles(p) []CredentialFile` —— adapter 插件的凭证文件（pi 2 条、opencode 1 条），collect 弹窗与存储抽屉消费

## 依赖方向

```
pluginregistry → adapter/{pi,herdr,opencode,vscode,keys} → core
toolctl / cli.commands / web / pluginruntime → pluginregistry
```

`internal/adapter` 不得反向 import pluginregistry（会成环）；
`adapter.Tool` 类型定义留在 adapter 包（toolctl/hub/agentd 的引用不动），
但内置清单（原 `adapter.Tools()` 等）已迁入 pluginregistry。

## 历史

改造前，注册点散落 6 处（`KNOWN_ADAPTERS`、`knownAdapterOrder`、init 报错文案、
`CategoryOrder` preferred 表、`Tools()`、UI 硬编码数组），互相靠手工同步。
现在 init/向导的候选集、程序探测表、凭证规则视图全部从 `Builtins()` 派生。
