# homer-cli

> Dotfiles for humans and their AI agents.

一个 git 仓库管好你所有工具的配置（pi / herdr / opencode / …）。
新机器一条 `homer home` 全部归位，密钥安全送达。

## Status

🚧 立项中 — 设计文档见 [DESIGN.md](./DESIGN.md)

- 市场调研：已完成（3 路并行，覆盖 pi 同步器 / 多 agent 工具 / 传统 dotfiles 管理器 / 商业配置同步机制）
- 定位：git 主通道 + age 密钥层 + tailcat 可选快车道 + 应用感知 adapter
- MVP 验收场景：新机器 `homer home <repo-url>` → pi + herdr + opencode 配置与密钥全部归位

## Naming

`homer` = homing（归位本能）+ Homeward Bound 的 Homer（归家犬）+ 荷马（《奥德赛》归乡史诗作者）。
npm 包名 `homer-cli`，安装后命令为 `homer`。

## License

MIT
