# 插件体系架构决策记录（2026-10-11）

> 状态：按插件体系冻结契约 v1.2 落地；端点用法见
> [Hub API 参考](../api-reference.md)。

## 1. 决策摘要

Homer 用统一插件目录表达三种不同的机器能力：adapter 管理配置同步范围，carrier 随同步流
携带横切数据，action 描述一次性机器操作。官方插件以 `pluginregistry` 为唯一注册源；机器
端继续使用现有执行原语，不增加新的 stream 消息，也不升协议版本。

## 2. 角色与机器原语

| role | 责任 | 机器原语与边界 |
|---|---|---|
| `adapter` | 声明一款工具的 root、categories 与同步策略 | 现有 `ScanAdapter` 扫描器及 pull/dispatch 路径；不引入新的执行器 |
| `carrier` | 随配置同步流传递的横切能力，当前为 keyring | 现有 `secret` 方法和 age 加密；密钥仍不以明文进入普通配置 store |
| `action` | 对选定机器执行一次操作，当前为 ssh-key | 现有 `ssh-key` 方法/API；`ActionSpec` 只描述 UI 表单，不决定执行方法 |

插件角色是能力目录与 UI 的分类，不是三套新的 agent 协议。机器端已有的同步、密钥和 ssh-key
执行路径保持不变。

## 3. 单一注册源：`internal/pluginregistry`

官方插件与 CLI 工具目录集中在 `internal/pluginregistry`。`Builtins()` 以稳定顺序返回官方
adapter、carrier、action；`homer init --adapters` 从其中筛选 adapter role，工具检查与官方
安装信息也复用该目录。这样注册 ID、展示名称、默认 AdapterConfig 与 UI 描述不再由多个
调用点各自维护。

注册目录只负责 Homer 编译内置的能力元数据，不是 Go `plugin` 动态加载器。用户安装状态由
hub 插件状态持久化；第三方 manifest 也不能向 agent 进程注入可执行代码。

## 4. keyring 与 ssh-key 转正

### keyring carrier

此前 keyring 由 `Apply` 静默补入 `homer.json`，插件状态对用户不可见。改造后将 `keyring`
注册为 `carrier`，由 hub 插件页显式安装/卸载；hub 只有在该插件已安装时才允许 key API 操作，
并在操作时确保 hub 配置包含 keyring adapter。密钥随原有 secret/age 流程加密传递，既不改变
密钥格式，也不增加机器端协议。

卸载有守卫：密钥仍绑定到已安装 adapter 时不允许卸载；adapter 的中心 store 仍有数据时也
不能绕过清理约束。卸载不会删除机器上的本地残留文件。

### ssh-key action

`ssh-key` 从硬编码的 UI 按钮提升为 `action` 插件。`ActionSpec` 给出 `method` 与表单字段，
例如必填的 `githubUser`；插件页据此渲染表单。`ActionSpec` 仅用于呈现，提交仍调用现有
`/api/agents/<id>/ssh-key` handler，由既有 `ssh-key` 方法执行，不允许插件指定任意机器命令。

## 5. 第三方扩展路线与信任边界

扩展分阶段推进，只有遇到真实需求才进入下一阶段，不预先引入脚本运行时：

| 阶段 | 机制 | 进入下一阶段的条件 |
|---|---|---|
| v1（当前） | `schemaVersion: 1` adapter-only JSON manifest；声明 `id`、`root`、`categories` 等数据 | 真实 adapter 需求无法由现有声明式 schema 表达 |
| v2 | 以 goja 提供受限 JavaScript 钩子 | 真实需求证明数据描述不足，且可以明确钩子的权限、输入与兼容边界 |
| v3 | 以 wazero 提供 WASM 扩展 | goja 路线的具体隔离、可移植性或生态需求无法满足，并有实际使用方 |

v1 只接受 `role: "adapter"`；carrier 依赖 Homer 内置的 secret/age 原语，action 依赖内置机器
方法，两者不能靠 JSON manifest 新增。manifest 是声明式配置，不是签名代码包或沙箱。尤其
`kind: "manifest"` 分类可声明 `listCmd` / `applyCmd`，这些命令仍按 Homer 现有确认闸门在机器上
执行；安装者必须审阅路径和命令，并信任 manifest 来源。schema 校验不等于对第三方作者背书。

## 6. 心跳拓展与覆盖可视化

机器现有 status 报告已包含 `report.adapters[].id`。hub 从 status 结果记录在线机器上报过的
adapter ID，并在 `GET /api/plugins` 的插件条目上提供 `machineCount`，用于展示插件覆盖情况。
它表示上报过该 ID 的在线机器数，不是插件安装次数或能力协商结果。

该数据沿用既有 status/heartbeat 周期在 hub 侧汇总，不增加 agent→hub stream 字段、不改变
`HeartbeatParams`、不新增协议消息，也不升 `stream.ProtocolVersion`。老机器没有相应 status
数据时不计入覆盖数；插件页可以将缺省 `machineCount` 显示为 0。

## 7. 约束与关联文档

- 机器端行为与现有 stream/sync/engine 协议保持兼容。
- 插件安装、卸载与 adapter 范围下发的 HTTP 示例见 [API 参考](../api-reference.md)。
- 面向使用者的角色说明、Claude manifest 示例与信任边界见 [README 插件体系](../../README.md#插件体系)。
