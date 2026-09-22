# homer-cli — DESIGN

> Dotfiles for humans and their AI agents.
> 一个 git 仓库管好你所有工具的配置；新机器一条命令全部归位，密钥安全送达。

## 0. 一句话定位

`homer` 是一个**个人元数据管理工具**：单一 git 仓库管理用户所有软件工具（pi / herdr / opencode / claude code / shell / 编辑器等）的配置，支持跨机同步、分类选择同步、密钥安全投递、新机器一键初始化。

名字来源（三关合一）：
- **homing** —— 归位本能，配置自动找到回家的路
- **Homer** —— 《Homeward Bound 不可思议的旅程》里历尽千山万水回家的金毛犬
- **荷马（Homer）** —— 《奥德赛》归乡史诗的作者，nostos（归乡）主题的缔造者

npm 包名 `homer-cli`（裸名 `homer` 被一个 2022 年停更的僵尸 DNS 包占用，月下载 28 次，无实质冲突）；安装后命令就叫 `homer`。

## 1. 市场调研结论（2026-09-22，三路并行调研）

### 1.1 竞品格局

| 品类 | 代表 | 结论 |
|---|---|---|
| pi 专属同步器 | @dbaida/pi-sync（最完整）、SmileyChris/pi-sync（P2P CRDT）、pi-syncro、pi-p2p-sync、2×WebDAV、2×session 专用 | 至少 8 家，全部小项目；**无一家有 VS Code 式分类开关** |
| 多 agent 同步 | dotagents（3 个同名项目，getsentry 238★ 有企业背书）、friday（4★） | 已有先行者；**herdr 适配无人做、密钥投递无人做** |
| 传统 dotfiles 管理器 | chezmoi 21.7k★、mackup 15.3k★、home-manager 10.4k★、stow/dotbot/yadm/dotdrop | 成熟但**无应用感知**（只认路径不认应用）；多机冲突处理、状态可视化是全品类痛点 |
| 商业配置同步 | VS Code / JetBrains / Chrome / Firefox / Obsidian / Syncthing | 设计精华来源（见 §1.2） |

### 1.2 VS Code Settings Sync 四板斧（照抄清单）

1. **分类是一等公民**：Settings/Keybindings/Extensions/Snippets/UI State 可勾选
2. **machine 作用域默认不同步** + `settingsSync.ignoredSettings` 支持**负向列表**（`-key` 从默认忽略中移除）
3. **settings.json 三路按键合并**（base/local/remote，同键不同值进冲突列表）
4. **冲突 UI**：Accept Local / Accept Remote / Show Conflicts（diff 视图）
5. 首次对接与日常同步分离（Merge / Replace Local / Merge Manually）
6. SecretStorage 明确**不同步**——密钥永不进配置同步管道（我们要做得更好：安全投递）

### 1.3 密钥选型（已定论）

| 方案 | 结论 |
|---|---|
| blackbox | ❌ 官方 README 自述 "ABANDONED. DO NOT USE." |
| git-crypt | ❌ 不支持密钥撤销/轮转（红线），完整性仅 SHA-1 HMAC |
| **age** | ✅ 23.7k★，健康活跃；chezmoi 原生集成为参考 |
| sops | ✅ 23.2k★，偏团队场景，个人场景 age 更轻 |
| **tailcat P2P** | ✅ Tailscale 官方（7.6k★），WireGuard 直连，密钥点对点直传 |

chezmoi 双方案参考：① 密钥不进仓库——模板函数运行时从密码管理器取值；② 密钥进仓库但 age 加密。

### 1.4 需求真实性

- `topic:dotfiles` 2026 年 1-9 月新建 3461 仓库，比 2025 全年（2067）+68% —— 爆发年
- `claude-code × dotfiles` 交集 569 个仓库；高星用户（liby/dotfiles 151★）在用 chezmoi 手工拼 agent 配置
- 定位语汇采用已被验证的 "dotfiles for humans and their coding agents"；避开死词 "personal data locker"、"config management as code"

### 1.5 市场空白（三重交集无人占据）

**懂 agent 工具的配置管理 × 真正的密钥通道 × 新机器一键初始化**

- chezmoi 有密钥方案但不懂 agent 工具
- agent 工具（dotagents/friday 等）懂 agent 但密钥全靠排除/剥离
- 新机器一键初始化无人当主打卖点

## 2. 产品设计

### 2.1 核心概念

```
~/.homer/                     ← 本地工作区（git clone of 配置中心仓库）
├── homer.json               ← 主配置：adapter 列表 + 分类开关 + 排除规则
├── store/                   ← 实际配置内容（按 adapter 分类）
│   ├── pi/
│   ├── herdr/
│   ├── opencode/
│   └── shell/
└── secrets/                 ← age 加密的密钥文件（可选）
```

- **配置中心 = 用户自己的私有 git 仓库**（GitHub/Gitea/自托管均可）
- **homer.json 也存在仓库里**——新机器只需提供仓库 URL，分类配置自动跟随
- 同步状态（上次 push/pull 的 commit）存本地 `~/.homer/state.json`，**不入库**（参考 Obsidian：同步策略本身不同步）

### 2.2 Adapter 架构（应用感知的核心）

每个工具一个 adapter，声明式描述：

```jsonc
{
  "adapters": {
    "pi": {
      "enabled": true,
      "categories": {
        "settings":    { "paths": ["settings.json", "keybindings.json"], "mode": "merge" },
        "skills":     { "paths": ["skills/"], "mode": "mirror" },
        "extensions":  { "paths": ["extensions/"], "mode": "mirror", "exclude": ["*cache*"] },
        "agents":      { "paths": ["agents/"], "mode": "mirror" },
        "models":      { "paths": ["models.json"], "mode": "merge", "excludeKeys": ["apiKeys"] },
        "prompts":     { "paths": ["prompts/"], "mode": "mirror", "enabled": false },
        "themes":      { "paths": ["themes/"], "mode": "mirror" }
      },
      "ignore": ["trust.json", "sessions/", "npm/", "git/", "auth.json", "trackingId"],
      "root": "~/.pi/agent"
    },
    "herdr":  { "root": "~/.config/herdr", "categories": { "...": "..." } },
    "opencode": { "root": "~/.config/opencode", "categories": { "...": "..." } }
  }
}
```

- **merge 模式**：JSON 字段级三路合并（VS Code settingsMerge 模式）
- **mirror 模式**：目录整树同步（文件级，冲突 last-writer-wins + 备份）
- **excludeKeys**：字段级排除（密钥占位符替换，参考 legout/pi-config 的 `__REQUIRED__` 方案）
- 首发三个 adapter：**pi**（最深）、**herdr**（独家）、**opencode**；预留 adapter 插件机制供社区贡献 claude/codex/shell 等

### 2.3 命令集

```
homer init <git-url>        # 绑定配置中心仓库（交互式引导）
homer home                 # 新机器一键归位：clone → 应用分类配置 → 解密密钥 → 装依赖 → doctor
homer push                 # 本地配置变更推送到仓库（push 前密钥扫描，参考 @dbaida/pi-sync）
homer pull                 # 拉取远端配置（先 diff 预览 → 确认 → 本地备份 → 应用）
homer sync                 # 智能同步：单向变更自动走，双向冲突进交互解决
homer status [--verbose]   # 漂移状态：↑本地未推 ↓远端未拉（footer 状态栏同款）
homer diff                 # 文本级差异预览
homer doctor               # 体检：仓库可达 / age 密钥 / adapter 路径 / 机器绑定设置
homer secret push|pull     # 密钥安全投递（tailcat P2P 优先，age 加密文件兜底）
homer pair                 # 两台机器在线配对（tailcat serve/cp）
homer history              # 同步历史
homer unlock --stale       # 清理残留锁
```

pi 扩展形态：`pi install npm:homer-cli` 后提供 `/homer status` 等命令 + footer 漂移显示（参考 @dbaida/pi-sync 的 `↑1 ↓0`）；CLI 独立可用（参考 pi-syncro 的双形态）。

### 2.4 传输通道分层

| 通道 | 用途 | 特性 |
|---|---|---|
| **git（主通道）** | 日常配置同步 | 异步、版本化、离线友好、天然历史 |
| **age 加密层** | 密钥文件入仓 | `secrets/` 目录 age 加密，密钥永不明文 |
| **tailcat（快车道）** | 密钥 P2P 直传、在线配对、临时大文件 | WireGuard E2E，需两端在线；可选依赖，未安装则自动降级 |
| **tailcat 备选** | 若无 tailcat：age 加密文件走 git，或提示手动传递 | 优雅降级 |

### 2.5 冲突处理（借鉴 VS Code）

1. JSON 配置（settings/models）：**三路按键合并**；同键不同值 → 冲突列表
2. 冲突解决：`homer merge` 交互式（Accept Local / Accept Remote / 手动编辑 diff）
3. 目录文件：last-writer-wins + 应用前本地备份到 `~/.homer/backups/`
4. 首次对接新机器：Pull（远端覆盖）/ Merge / Skip 三选一
5. **never auto-push**：自动同步只做安全 pull（参考 @dbaida/pi-sync 的保守策略）

### 2.6 安全设计

- push 前密钥扫描（常见 API key/token 正则），命中拒推并提示
- `secrets/` 独立于 `store/`，age X25519 加密（遵循 age 官方建议只支持 X25519）
- 密钥轮转：age 支持多 recipient，换设备 = 加新 recipient + 重加密（git-crypt 的撤销红线已避开）
- tailcat 通道密钥不落 git
- 本地状态/锁/备份在 `~/.homer/` 下，不污染各工具目录

## 3. MVP 范围（首个可用版本）

**验收场景 = 真实场景**：新机器上

```bash
curl -fsSL https://raw.githubusercontent.com/<user>/homer-cli/main/install.sh | sh
homer home https://github.com/<user>/my-config.git
# → pi + herdr + opencode 的配置、skills、extensions、密钥全部归位
# → pi 启动即用，herdr agents 就位，opencode 配置生效
```

MVP 必做：
- [ ] pi adapter 全量（7 分类 + excludeKeys + merge 模式）
- [ ] herdr / opencode adapter（路径级 + 基础分类）
- [ ] homer init / push / pull / status / diff / home / doctor
- [ ] age 密钥层（加密/解密/多 recipient）
- [ ] 密钥扫描拒推
- [ ] 三路合并 + 冲突交互
- [ ] curl 一键安装脚本
- [ ] pi 扩展形态（footer 状态）

MVP 不做（v2+）：tailcat 集成、adapter 插件市场、claude/codex adapter、状态仪表盘、锁与并发完善。

## 4. 技术选型

- **语言**：TypeScript + Node（与 pi 生态同构，pi 扩展形态可复用核心逻辑）
- **发布**：npm `homer-cli`（bin: `homer`）+ pi package（`pi.install` 清单）双形态
- **测试**：vitest；合并逻辑必须有字段级测试矩阵
- **零重运行时依赖优先**（参考 @dbaida/pi-sync 单文件零依赖的做法）；age 加密考虑 `age-encryption` npm 实现

## 5. 风险与对策

| 风险 | 对策 |
|---|---|
| agent 工具配置格式频繁变动（pi 迭代快） | adapter 声明式配置用户可自行修正路径；homer.json 跟随仓库走 |
| `.agents` Protocol 等标准仍 DRAFT | 不赌标准，按 per-tool adapter 落地，标准成熟可加投影层 |
| 竞品加速（getsentry/dotagents 有企业背书） | 押密钥投递 + herdr 独家 + 一键初始化三个他们没有的点 |
| 密钥安全责任重大 | 密钥扫描 + age 加密 + never auto-push 三重保险；文档明确 best-effort 边界 |
| 多机并发同步冲突 | 状态锁 + 三路合并 + 备份兜底；MVP 阶段单用户场景可容忍 |

## 6. 里程碑

1. **M1（骨架）**：仓库结构、homer.json schema、pi adapter 只读 diff/status —— 跑通"看见漂移"
2. **M2（同步内核）**：push/pull/merge/备份/密钥扫描 —— 跑通"安全往返"
3. **M3（密钥层 + home）**：age 集成、curl 安装脚本、herdr/opencode adapter —— 验收场景达成
4. **M4（pi 扩展形态）**：footer 状态、/homer 命令、自动 pull
5. **M5（v2）**：tailcat 快车道、adapter 插件机制、更多 adapter

---

*调研数据快照：2026-09-22。竞品星数/活跃度来自 GitHub API 与 npm registry 当日查询。*
