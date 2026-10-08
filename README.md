# homer-cli

> Dotfiles for humans and their AI agents.

`homer` 是 Go 单二进制配置归位工具：用 git 保存 pi / herdr / opencode
配置，用 age 加密投递密钥。主通道不依赖 Node 或 Go 环境，支持 Linux/macOS
的 amd64 与 arm64。

## 快速开始（三行版）

```sh
curl -fsSL https://raw.githubusercontent.com/zzjcool/homer-cli/master/install.sh | sh
homer init && homer secret keygen
homer push --yes                 # 首次 push 自动 git init + 建立基线
```

**心智模型：配置中心仓库就是 `~/.homer` 本身**（不是 `~/.homer/store/`）。
`homer.json`、`.gitignore` 与 `store/` 一起提交；`state.json`、`backups/` 和
`keys/` 只留在本机。首次 `push` 没有 remote 时仍会完成本地基线并给出 warning。

新机器可直接归位：

```sh
homer home <配置仓库 URL> --yes
```

## Hub 形态（v1.4）：网页控制台 + 多机 agent

单机网页控制台：

```sh
homer serve                     # http://127.0.0.1:7760
# 浏览器打开即可看本机与中心的差异，并在确认后同步
```

多机拓扑（中心节点部署在可达处，其余机器主动连接中心）：

```sh
# 中心节点（公网/可达机器）：
HOMER_HUB_TOKEN=<token> homer serve --addr 0.0.0.0:7760

# 每台机器上的 agent 都主动通过 WebSocket 连接 hub；agent 本机不监听端口
homer agent --hub http://<hub>:7760 --token <token>
```

`homer agent` 只有这一种连接方式。首次启动用 `--hub <url>` 指定 hub；
`--data-url <url>` 可选，默认与 `--hub` 相同，只用于 `/api/snapshot` 和
`/dl/*` 数据请求，不改变 WebSocket 控制连接。hub 与 agent 在同一台机器时，
可把数据请求指向 hub 的回环地址以绕开隧道：

```sh
homer agent --hub https://<hub> --data-url http://127.0.0.1:7760 --token <token>
```

旧的双模式 agent 参数已移除，不再监听 agent 入站端口。旧协议端点
`/agent/v1/enroll`、`/agent/v1/register`、`/agent/v1/poll`、
`/agent/v1/report` 返回 HTTP 410，提示旧版 agent 升级；新 agent 使用
WebSocket Stream 与 hub 通信。

hub 的 `/api/agents` 可查看各机器的在线状态和差异。控制台的「同步」会先把这台
hub 的改动写入中心，再让在线的其他机器应用（`POST /api/agents/<id>/push?confirm=true`），
配置经 git 远端在机器间真实流转。hub 不自建存储，同步语义复用
engine/sync/gitx。

鉴权：Bearer token（`HOMER_HUB_TOKEN` 或 `--token`）；未设 token 时仅允许回环地址
（非回环拒绝启动）。生产公网建议再加反代 TLS。

#### hw 现网 agent 迁移

部署新 hub/agent 版本时，先按旧 unit 的类型停止正在运行的 agent：

```sh
systemctl --user stop homer-agent.service  # 用户级 unit
# 若为系统级 unit，则改用：systemctl stop homer-agent.service
```

编辑 `homer-agent.service` 的 `ExecStart`，把旧命令中的 `--connect <url>`（也可能写成
`--connect=<url>`）改为 `--hub <url>`，例如：

```ini
# 旧版本
ExecStart=/path/to/homer agent --connect https://<hub>
# 新版本
ExecStart=/path/to/homer agent --hub https://<hub>
```

`agent.json` 中已有的 `agentSecret` 继续有效；旧的 `connectUrl` 字段会被忽略，
不会自动配置新连接地址，因此必须在 `ExecStart` 中显式提供 `--hub`。保留原有
必要环境变量和 token 配置。保存 unit 后重新加载并启动：

```sh
systemctl --user daemon-reload
systemctl --user start homer-agent.service
```

系统级 unit 请对应使用 `systemctl daemon-reload` 和 `systemctl start homer-agent.service`。
检查 agent 日志及控制台，确认机器重新上线。

容器化验收：`bash e2e/hub-smoke.sh`（origin + hub + agent-a + agent-b 四容器，
覆盖两个 WebSocket agent 的状态采集与配置跨机流转）。

### 应用版本与一键升级

每台机器的 agent 会随心跳上报适配器所驱动的应用的版本（pi、herdr、opencode、
VS Code，没装的不报）。控制台的机器卡片上有一行「应用」，`homer ps` 的最后一列
也是同一份信息。

- **什么算「落后」**：和整个机器群里任意一台上报过的最新正式版比（离线的机器也
  算数；预发布版不当基准），或者和适配器声明的最低版本比，取较新的。读不出版本的
  不会被标成落后；只有一台机器上报时没有对照，也不会被标成落后，但「详情」里每个
  应用都有「升级」，随时可以点。
- **怎么升**：落后的应用旁边直接有「升级」，卡片底部有「全部升级应用」（只含在线、
  能升级的）。点下去后升级在那台机器上进行，可能要几十秒到几分钟，请留在窗口里；
  升完立刻显示新版本。失败时会说明原因、给出命令输出的最后几行和官方安装命令，
  可以复制到那台机器的终端里运行。
- **安全**：控制台和 hub 只传应用的 ID，不传命令。机器按自己内置的表执行该应用
  自己的升级命令（pi：`pi update --self`，herdr：`herdr update`，opencode：
  `opencode upgrade`），表里没有的 ID 一律拒绝。VS Code 只报版本，要用系统的包
  管理器更新。
- **旧版 agent**：版本太老的 homer 不会上报应用版本，也不认识升级指令，先在控制台
  点「更新程序」把它换成中心现在这份。
- **给新的适配器加上这个能力**：在 `internal/adapter/tools.go` 的 `Tools()` 里加
  一项（`ID`、`Adapter`、`Label`、`Binary`、`VersionArgs`、`Install`，能自升级的再填
  `UpgradeArgs`，需要硬性下限时填 `MinVersion`）。上报、落后标记、升级按钮、
  `homer ps` 都不用再改。

## 安装

### 1. release 二进制（推荐）

```sh
curl -fsSL https://raw.githubusercontent.com/zzjcool/homer-cli/master/install.sh | sh
```

脚本探测 `uname -s` / `uname -m`，从 GitHub Release latest 下载对应的
`homer_<os>_<arch>.tar.gz` 与 `checksums.txt`，校验 SHA-256 后安装到
`~/.local/bin/homer`。已存在同名文件会原子替换，因此可以幂等重跑；若显式
请求 `/usr/local/bin` 但当前用户没有权限，脚本会在不阻塞输入的情况下提示并
降级到 `~/.local/bin`。

测试、镜像与内网可使用以下注入位：

```sh
HOMER_INSTALL_URL=https://mirror.example/homer_linux_amd64.tar.gz sh install.sh
HOMER_INSTALL_BASE_URL=https://mirror.example/releases sh install.sh
HOMER_INSTALL_VERSION=v1.2.3 sh install.sh
HOMER_INSTALL_PREFIX="$HOME/bin" sh install.sh
HOMER_INSTALL_PACKAGE=/tmp/homer_linux_amd64 sh install.sh  # 离线冒烟
```

`HOMER_INSTALL_PACKAGE` 是受控的本地测试输入；发布下载始终要求匹配的
`checksums.txt`。安装完成后脚本会运行 `homer --help` 冒烟检查。

### 2. goreleaser / 源码构建

仓库的 `.goreleaser.yaml` 生成 Linux/macOS × amd64/arm64 四个归档、
`checksums.txt` 并发布 GitHub Release：

```sh
go build -o "$HOME/.local/bin/homer" ./cmd/homer
# 发布维护者：goreleaser release --clean
```

本地无 goreleaser 时，可以用交叉编译验证发布矩阵：

```sh
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  GOOS=${target%/*} GOARCH=${target#*/} CGO_ENABLED=0 \
    go build -o "/tmp/homer-${target%/*}-${target#*/}" ./cmd/homer
 done
```

### 3. npm 薄壳（兼容既有 npm / pi 安装路径）

```sh
npm install -g homer-cli
```

`npm/` 仅提供 wrapper 和 best-effort `postinstall` 二进制下载；GitHub 不可达
不会阻断 npm 安装，wrapper 会提示改用 `install.sh`。离线/测试安装可传
`HOMER_INSTALL_PACKAGE=/path/to/homer_linux_amd64`。不需要 npm 的生产环境请优先
使用 release 二进制。

## MVP 使用场景

### 机器 A：建立配置中心

```sh
homer init --json                         # 默认注册 pi / herdr / opencode
homer secret keygen                       # 私钥写入 ~/.homer/keys/age.txt（0600）
# 在 homer.json 写 secrets.recipients 与 secrets.files，并把明文落到目标路径
homer remote <配置仓库 URL>                # 可选：确保 ~/.homer 是仓库并配置 origin
# 或用 homer init --remote <配置仓库 URL> 一次完成 init + git init + 首推
homer secret push --yes                   # secrets/<name>.age 只存密文
homer push --yes                          # 首次建立基线；有 remote 自动 push -u
```

`homer remote` 只接线、不自动上传，输出的 `git -C ~/.homer push -u origin
master`（分支名按实际仓库显示）确认无误后运行即可。`homer init --remote` 则会
直接完成配置中心初始 commit 和首推。

四个内置 adapter 的默认范围：

- **pi**：上面这些分类之外，`~/.pi/agent` 里其余文件默认整棵收进中心（`files`）。
  `auth.json` 和 `mcp-auth.json` 是 pi 的密钥存储，不进明文，在界面里默认加密。
  其他文件可以在存储视图里审阅，再「转为加密」。`settings.json` /
  `keybindings.json` / `mcp.json` / `models.json`（merge，`models.json` 去掉
  `apiKeys`），说明文件、skills、extensions、agents、prompts、themes（mirror）。
  sessions、安装缓存、日志不收。
- **herdr**：仅同步 `~/.config/herdr/config.toml`。
- **opencode**：`opencode.json` / `package.json`（merge），lock 文件（mirror），
  `node_modules` 与运行时文件忽略。
- **vscode**：`settings.json` / `keybindings.json` 与扩展 manifest；macOS 根目录为
  `~/Library/Application Support/Code`，Linux 根目录为 `~/.config/Code`。

### 自定义 adapter（v1.2 起）

手写 `homer.json` 即可声明自定义 adapter；`homer init` 不会自动发现或删除它。
adapter ID 使用小写字母、数字和连字符（例如 `my-tool`）。下面是一个可直接复制的
完整配置片段：它包含普通文件分类、**manifest 分类（v1.2 起支持）**、忽略规则和
明确的 symlink 逃逸例外。

```json
{
  "version": 1,
  "adapters": {
    "my-tool": {
      "root": "~/.config/my-tool",
      "categories": {
        "config": {
          "paths": ["config.json"],
          "mode": "merge"
        },
        "profiles": {
          "paths": ["profiles/"],
          "mode": "mirror"
        },
        "plugins": {
          "kind": "manifest",
          "mode": "mirror",
          "listCmd": "my-tool plugin list --ids",
          "applyCmd": "my-tool plugin install"
        }
      },
      "ignore": ["cache/", "*.log"],
      "allowEscape": ["shared/credentials/"]
    }
  }
}
```

`root` 是工具配置根目录；普通分类按 `paths` 读取文件或目录。manifest 分类不写
`paths`：`listCmd` 的标准输出必须是每行一个 ID，`applyCmd` 会在确认后逐个安装远端
缺少的 ID（v1.2 只装不卸）。`ignore` 规则相对 `root` 生效；`allowEscape` 只应列出
确实需要允许的具体 symlink 路径，不能写 `*`、`**` 等裸通配。`home` 会触发安全确认：
远端 manifest 的 `listCmd` 与 `applyCmd` 都在确认后才执行；`--yes` 放行并在 report
中留痕。请只信任自己的配置仓库。

### init 选择向导（v1.2 起）

在 TTY 中直接运行 `homer init` 会打开三级选择：adapter → 分类 → 大目录的
一级条目。选项默认全选；空格切换、Enter 确认、`/` 过滤。向导的选择会写入
`enabled:false` 与分类 `exclude`，不会删除既有 adapter、路径、忽略规则、
`allowEscape`、备份或 secrets 字段。

CI、脚本和其它非 TTY 环境应显式选择旁路：

```sh
homer init --all --json       # 全量内置 adapter，不启动向导
homer init --adapters pi,vscode --json
```

`--json`、`--all`、`--adapters` 都不会读取 stdin；非 TTY 下已有 `homer.json`
仍拒绝无意覆盖（除非使用 `--force`）。TTY 下对已有配置重新运行 `homer init`
是 re-init：向导预选当前状态，只增量更新 enabled/exclude，**不重写 store 快照**。
选择完成后运行 `homer push --yes` 才会提交当前本地状态；若确实要恢复内置默认
并重写快照，使用 `homer init --force`。

### VS Code 与 manifest 分类（v1.2 起）

内置 `vscode` adapter 默认启用，Linux 根目录为 `~/.config/Code`，macOS 根目录为
`~/Library/Application Support/Code`，同步 `settings.json`、`keybindings.json`，并把
`code --list-extensions` 的输出保存为 `store/vscode/extensions/extensions.manifest.txt`
这一虚拟文件。每行一个扩展 ID；本机没有 `code` 或 VS Code 根目录时会降级为空快照，不会
把缺失的工具误判成删除。CLI 缺失会在 status/pull/home 的 warning 中说明：本机按空清单
处理，pull 时远端清单将视为全量待装，安装失败会逐条进入 `manifest.failed`。

manifest 是集合并集语义：归位或 pull 时只对“远端有、本机 listCmd 没有”的 ID
逐个执行 `applyCmd`，**只装不卸**，本机已经安装的扩展以及本机多出的扩展都会保留。
每条 apply 失败会写入 `manifest.failed` 和 warning，但不会阻断其它 ID；命令不经
shell 执行且 manifest ID 必须通过安全字符集校验。

`homer home` 发现远端 manifest 安装任务时，会在预览中列出 `listCmd` /
`applyCmd` 并显示远端命令门禁；未给 `--yes` 时 manifest 分类会延迟到确认后才执行
`listCmd`，因此确认前不会运行远端命令。TTY 需要确认，非 TTY 没有 `--yes` 会以
`aborted` 结束；`--yes` 才会放行，并在 `report.warnings` 记录已按 `--yes` 确认
执行远端声明的命令。pull 在 fast-forward 前发现远端 manifest 命令变更时也会把
旧命令与新命令写入 warning 并纳入确认预览。请只使用自己信任的配置仓库。

### 机器 B：一键归位与换设备

```sh
homer home <origin> --yes                 # clone、首次 merge、doctor
homer secret keygen                       # 新设备生成 recipient
# 旧设备将新 recipient 追加到 homer.json 后：
homer secret push --yes
# 新设备：
homer secret pull --yes
homer doctor --json
homer status --json
```

`home` 不安装工具依赖；归位配置前会为 enabled adapter 自动创建缺失的工具根目录，
再把配置写回工具目录。若创建根目录失败，会通过 warning 明确报告。新设备没有
identity 时会安全地跳过密钥并给出补齐步骤。配置归位不覆盖本地冲突，受影响文件
先备份。密钥目标与密钥备份分别保持 0600，备份目录保持 0700。

`homer doctor` 按固定顺序检查 config、repo、store-clean、remote、adapters、age、
machine、required 八项。warn（例如离线、工具未安装、`__REQUIRED__` 残留）不改变
退出码；只有 fail 返回 1。`--offline` 可在无网络 CI 使用。

## 在线配对（v1.3）

`homer pair` 是密钥配对的单次快车道：tailcat 只负责建立一次性的
WireGuard/DERP 字节管道，Homer 自己的协议负责传输；管道中的每个密钥仍是
age 密文。pair 不同步 `store/`，也不自动提交或推送 `homer.json`。

### 用法

两台机器先各自完成 `homer init` / `homer home`，A 端有可读取的
`secrets.files` 目标，B 端运行过 `homer secret keygen`：

```sh
# 机器 A：输出一次性地址并等待 B
homer pair --home ~/.homer --yes

# 机器 B：把地址只通过当面或私密信道带入，不要粘贴到 git、issue、群聊
homer pair '<tc-addr>' --home ~/.homer --yes
```

A 端收到 B 的 hostname 与 recipient 指纹后才会继续；TTY 默认会询问，脚本或
非 TTY 必须给 A 端 `--yes`。B 端的 `--yes` 是兼容选项、不产生额外确认。
`--json` 适合自动化，但 JSON 中的 `addr` 仍是 bearer secret，不能写入共享日志。
配对成功后 A 的 `homer.json` 会追加 B recipient，但 pair **不自动 commit**：

```sh
# A：让旧的 git 密钥通道也能解给 B，并提交 homer.json / vault
homer secret push --yes
homer push --yes

# 没有 tailcat 时，B 仍可走原有 git 通道
homer secret pull --yes
```

`secret push` 会用 `secrets.recipients` 的全部 recipient 重新加密 vault；这一步
使 B 既能使用 pair 收到的密钥，也能在后续从 git 通道 pull。地址泄露本身不能解开
age 密文，但仍应按敏感 bearer secret 处理。

### 安装 tailcat、`--key=new` 与 DERP

pair 需要可选的 tailcat CLI，不把 tailcat 作为 Go module 依赖。请按官方
[INSTALL 指引](https://github.com/tailscale/tailcat)安装（建议 v0.7.0+），并确认
两台机器的 `PATH` 都能找到 `tailcat`。PATH 中 `tailcat` 的来源校验由用户负责，Homer 不对 PATH 中的同名程序做来源背书。Homer 会明确启动：

```text
tailcat --key=new
```

而不是裸 `tailcat`。`--key=new` 强制本次 serve 使用 ephemeral key，避免机器上已有
`~/.config/tailcat/keys/default.private.json` 时复用 saved default key，造成地址
跨会话可复用。地址经 `TAILCAT_ADDR_FILE` 送回 Homer，地址文件为 0600，pair 结束
后删除；不要把地址存进配置仓库。

两端直连时延通常最低；无法直连时会退到公网 DERP。DERP 受网络、区域、map 拉取和
限流影响，没有端到端 SLA，跨区域延迟可能达到秒级，因此 pair 的接受/步骤超时按
慢路径留有余量。对低延迟或合规部署，可以运行自建 derper，并给 tailcat transport
显式注入自建 map：`TAILCAT_DERPMAP_URL=https://<your-map>/derpmap`（集成方使用
`TailcatOptions.ExtraEnv` 传入；Homer 不会把任意 ambient environment 静默泄露给
子进程）。先在小范围用 `tailcat ping` 观察 `via DERP(...)` 或 `via IP:port`，再决定
是否切换；自建 derper 的证书、ACL、可达端口和 map 发布由部署方负责。

### pair 安全模型（S1–S6）

- **S1 地址生命周期**：`--key=new`、one-shot serve、0600 临时地址文件和结束时
  删除共同限制地址的生命周期；地址只带外交换，并在输出中提示“勿入 git/聊天记录”。
- **S2 A 端确认门**：hello 之后先显示 hostname 与 recipient 指纹（不回显完整 key），
  再确认；拒绝或非 TTY 未给 `--yes` 时不写 `homer.json` / `state.json`，不发送密文。
- **S3 bundle 原子性**：A 先把全部 destination 明文读入内存，再用 A 现有 recipient
  加 B recipient 重新加密；B 全部解密验证成功后才备份、原子写 vault 和 0600 destination。
  写盘阶段仍存在应用进程窗口；重试 pair 依靠备份与幂等写入收敛，不能把该窗口宣称为严格事务。
- **S4 帧与路径防御**：单帧上限 16 MiB、bundle 上限 64 MiB；文件名必须属于本机
  `secrets.files`，目标路径只从本机配置解析，拒绝未知密钥名和路径穿越。
- **S5 中断清理**：pair 连接、serve 和 tailcat 子进程组在退出/中断时统一关闭，
  不留孤儿 tailcat；A 已写入 recipient 但未收到 ack 时会 warning，B 公钥可保留或重试。
- **S6 密钥分层**：tailcat 提供传输层 WireGuard/DERP，age 提供 at-rest 与 bundle
  端到端内容加密，git push/pull 是无 tailcat 时仍可用的兜底通道。

缺少 tailcat 时 `homer pair` 会明确以 exit 1 提示官方安装链接和 git 回退，不会
静默降级或破坏既有 git 主通道。

### 版本与诊断

```sh
homer version
homer --version
```

发布二进制通过 GoReleaser 注入版本；源码构建与开发构建显示 `dev`。

## 安全备案（必读）

- **仓库历史里存在一把已作废的 age 私钥**：Go 重写前的 TS 版开发期间，一次冒烟
  测试曾在 `docs/m3-p0-report.md` 里粘过一把 bech32 合法的 age identity
  （`AGE-SECRET-KEY-19WJMMZ92…`）。工作区已在 `c6cec6f` 脱敏，但该 commit 之前的
  历史仍可读到原文（历史重写代价大于收益，故不做）。**声明：该密钥仅供一次性的
  密钥层冒烟，曾从未被用作任何真实 vault 的 recipient，也从未被任何真实
  `~/.homer` 引用；现正式作废，请勿使用。**如果发现它曾被用于加密真实密钥，请
  立即轮换：`homer secret keygen` → 旧机登记新 recipient → `homer secret push` →
  各机 `homer secret pull`。
- 私钥只写入本机 `keys/age.txt`（0600），`keys/` 由 gitignore 排除；命令输出只含
  `age1...` recipient，绝不回显 identity。
- `secret push` 的扫描闸门阻止疑似 API key/token 进入 store；`secrets.ignorePaths`
  只能显式豁免确知安全的路径。`__REQUIRED__` 只表示需要在本机补全的排除键。
- repo URL 会作为 `git clone -- <url>` argv 传入，并拒绝以 `-` 开头的值，防止
  `--upload-pack=<cmd>` 形式的选项注入。
- `listCmd` 的标准输出会进入 store，并在 `homer push` 时随仓库上传；请勿配置会读取
  敏感文件的命令。
- `allowEscape` 只放行明确的 symlink 路径；`*`、`*/`、`**`、`**/` 等裸通配会被
  配置校验拒绝，默认仍禁止 root 外逃逸。
- **D4 残余向量（v1.2 已知限制）**：`homer pull` 的 fast-forward 可能把新的
  `homer.json` 一并带入工作区；本版本会在 fast-forward 前比较并警告新增/改写的
  manifest `kind` / `listCmd` / `applyCmd`，但 `--yes` 仍按语义放行，且下一次
  `status` 仍可能执行新的 `listCmd`。`home` 的远端命令门禁覆盖新机归位，但不能替代
  pull 后的信任判断。v1.3 将用 `state.json` 中的 `trustedManifests` 命令指纹库收口；
  在此之前请先审阅远端 diff，再运行 pull。

## 验收与文档

```sh
go build ./...
go vet ./...
gofmt -l . | grep -v vendor
go test ./...
sh -n install.sh
```

完整的 W11 验收记录、测试统计、发布矩阵与已知限制见
[docs/go-rewrite-report.md](docs/go-rewrite-report.md)；设计和冻结语义见
[DESIGN.md](DESIGN.md) 与 [docs/go-rewrite-plan.md](docs/go-rewrite-plan.md)。

## 已知限制（MVP）

- `home` 不替各工具安装依赖；依赖由 pi / herdr / opencode 自己管理。
- `secrets/` 是 age 密文投递通道，不参与 store 的三路 merge；换设备需要追加
  recipient 后重新加密 push。
- `secret pull` 按设计不自动快进整个 store 工作区；成功后会明确提示先执行
  `git -C <home> fetch origin && git -C <home> merge --ff-only origin/master`，避免随后
  `homer push` 遇到 non-fast-forward。doctor 会在需要时回落读取 upstream 密文进行
  可解性检查。
- `pull` / `merge` 检测到两台机器都推送造成的分叉时，会给出两条路径：放弃另一机改动就
  在本机 `homer push`，保留两边则 `git -C <home> pull --rebase` 后 `homer merge`。
- 不包含 M4/M5 的 `sync`、插件机制与并发锁；pair 只做一次一台的密钥快车道，
  不同步 store。真实双机、真实 DERP 路径和真实 HOME 仍需按发布清单人工验收。

## License

MIT
