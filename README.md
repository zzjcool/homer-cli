# homer-cli

> Dotfiles for humans and their AI agents.

`homer` 是 Go 单二进制配置归位工具：用 git 保存 pi / herdr / opencode
配置，用 age 加密投递密钥。主通道不依赖 Node 或 Go 环境，支持 Linux/macOS
的 amd64 与 arm64。

## 快速开始（三行版）

```sh
curl -fsSL https://raw.githubusercontent.com/zzjcool/homer-cli/main/install.sh | sh
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

## 安装

### 1. release 二进制（推荐）

```sh
curl -fsSL https://raw.githubusercontent.com/zzjcool/homer-cli/main/install.sh | sh
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

三 adapter 的默认范围：

- **pi**：`settings.json` / `models.json`（merge），skills、extensions、agents、
  prompts、themes（mirror）；运行时 sessions、auth、日志等忽略。
- **herdr**：仅同步 `~/.config/herdr/config.toml`。
- **opencode**：`opencode.json` / `package.json`（merge），lock 文件（mirror），
  `node_modules` 与运行时文件忽略。

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
确实需要允许的具体 symlink 路径，不能写 `*`、`**` 等裸通配。来自远端仓库的
manifest `listCmd` / `applyCmd` 会在 `home` 时触发安全确认，请只信任自己的配置仓库。

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

内置 `vscode` adapter 默认启用，根目录为 `~/.config/Code`，同步
`settings.json`、`keybindings.json`，并把 `code --list-extensions` 的输出保存为
`store/vscode/extensions/extensions.manifest.txt` 这一虚拟文件。每行一个扩展 ID；
本机没有 `code` 或 VS Code 根目录时会降级为空快照，不会把缺失的工具误判成删除。

manifest 是集合并集语义：归位或 pull 时只对“远端有、本机 listCmd 没有”的 ID
逐个执行 `applyCmd`，**只装不卸**，本机已经安装的扩展以及本机多出的扩展都会保留。
每条 apply 失败会写入 `manifest.failed` 和 warning，但不会阻断其它 ID；命令不经
shell 执行且 manifest ID 必须通过安全字符集校验。

`homer home` 发现远端 manifest 安装任务时，会在预览中列出 `listCmd` /
`applyCmd` 并显示远端命令门禁。TTY 需要确认，非 TTY 没有 `--yes` 会以
`aborted` 结束；`--yes` 才会放行，并在 `report.warnings` 记录已按 `--yes` 确认
执行远端声明的命令。请只使用自己信任的配置仓库。

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
- `allowEscape` 只放行明确的 symlink 路径；`*`、`*/`、`**`、`**/` 等裸通配会被
  配置校验拒绝，默认仍禁止 root 外逃逸。
- **D4 残余向量（v1.2 已知限制）**：`homer pull` 的 fast-forward 可能把新的
  `homer.json` 一并带入工作区；如果该配置新增或改写了 manifest 的 `listCmd` /
  `applyCmd`，下一次 `status` 扫描就会执行新的 `listCmd`。`home` 的远端命令门禁
  覆盖新机归位，但不能替代 pull 后的信任判断。v1.3 将用 `state.json` 中的
  `trustedManifests` 命令指纹库收口；在此之前请先审阅远端 diff，再运行 pull。

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
- 不包含 M4/M5 的 `sync`、`pair`、tailcat、插件机制与并发锁；真实 HOME / 真实远端
  不属于自动化测试对象。

## License

MIT
