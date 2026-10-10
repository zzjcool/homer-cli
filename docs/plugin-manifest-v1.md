# 第三方插件 manifest（schemaVersion 1）

> 状态：v1 已落地（2026-10-11）。第三方 v1 仅支持 **adapter 型**（纯数据声明，
> 不携带任何代码）。演进路线见 [2026-10-11-plugin-architecture.md](plan/2026-10-11-plugin-architecture.md)。

## 提交方式

hub 控制台「插件」页粘贴 manifest JSON → `POST /api/plugins/install` body
`{"manifest": { ... }}`；或 API 直接提交。

## 字段

| 字段 | 必填 | 说明 |
|------|------|------|
| `schemaVersion` | ✅ | 必须为 `1`，否则 422 `schema-version` |
| `id` | ✅ | `^[a-z][a-z0-9-]*$`；不得与官方目录或已安装插件撞名（否则 409） |
| `role` | ✅ | v1 仅接受 `"adapter"`；其他值 422 `third-party-role` |
| `name` | 建议 | 展示名；缺省用 id |
| `description` | 可选 | 一句话描述 |
| `root` | ✅ | 机器上的配置根目录，支持 `~` |
| `categories` | ✅ | 分类声明，见下 |
| `ignore` | 可选 | glob 排除规则（`*` 不跨 `/`） |

`categories` 是 `名称 → 声明` 的 map，每个分类：

| 字段 | 说明 |
|------|------|
| `paths` | 分类下的文件/目录（相对 root） |
| `mode` | `merge`（JSON 按键三路合并）或 `mirror`（整镜像） |
| `kind` | `file` / `dir` / `manifest`（插件包清单，按名字同步） |
| `listCmd` / `applyCmd` | manifest 型分类的列举/安装命令（可选，机器上受控执行） |
| `idPattern` | manifest 输出行的插件名提取正则（可选） |

## 示例：claude adapter

```json
{
  "schemaVersion": 1,
  "id": "claude",
  "role": "adapter",
  "name": "Claude Code",
  "description": "Claude Code 的设置与插件同步",
  "root": "~/.claude",
  "categories": {
    "settings": { "paths": ["settings.json"], "mode": "merge", "kind": "file" },
    "agents":   { "paths": ["agents/"], "mode": "mirror", "kind": "dir" }
  },
  "ignore": ["logs/", "*.tmp"]
}
```

## 校验与信任边界

- 提交时走与官方声明同一套 `core` 校验（路径合法性、mode/kind 枚举）。
- **第三方 adapter 永不携带机器端代码**：机器上执行的解释器（ScanAdapter）
  是官方编译的；manifest 里的 `listCmd/applyCmd` 是仅有的命令声明，安装时
  用户在确认弹窗里可见。
- 完整 manifest 存于 `~/.homer/plugins.json` 的 `custom` 数组，hub 重启后恢复。
- 卸载 = 从注册表移除 + 发布不含该 adapter 的世代；机器本地文件不删。
