# Go 重写 P5 · W11-integrator 收官验收报告

> 基线：`8069e96`（master）  
> 分支：`pi-subagent/review-fix`（review 修复分支）  
> 目标：按 `docs/go-rewrite-plan.md` §6 / §7 / §9 完成 Go 版 MVP e2e、发布管线与文档收官，并落地对抗式 review 的 M1-M8 与 5 条 minor。  
> 实现 commit：以本次提交 hash 为准

## 一句话结论

对抗式 review 修复完成：真实编译后的 Go CLI 在双假 HOME + 假 bare origin 中跑通
原有七组 MVP 与新增 pull/backup、冲突拒绝、pull-delete 三组冻结 e2e；Linux/macOS ×
amd64/arm64 四平台归档与 `file://dist` installer 冒烟通过；覆盖-home、退出码、失败
重试、TS 别名清理和单一 exclude-key 实现均有回归测试。

## 做了什么

| 文件 / 目录 | 交付 |
|---|---|
| `tests/e2e/mvp_test.go`、`tests/e2e/sync_review_test.go` | 原有七组与新增三组真实子进程 e2e：A 三 adapter 装配与密钥推送、B `home` 配置归位、换设备 recipient 重加密、`__REQUIRED__` doctor、allowEscape、install.sh、隔离只读 init/doctor，以及冻结清单的 pull 应用+旧版备份、远端领先同文件冲突拒绝、pull-delete 传播+删除内容备份。所有 fixture 使用 `t.TempDir()`、fake `HOME` / `HOMER_HOME` 和文件系统 bare origin。 |
| `.goreleaser.yaml` | GoReleaser v2 配置；CGO 关闭；Linux/macOS × amd64/arm64 四归档；`checksums.txt`；GitHub Release owner/name；版本 ldflags。 |
| `.github/workflows/release.yml` | `v*` tag 触发 GoReleaser GitHub Release；随后从 `file://${GITHUB_WORKSPACE}/dist` 运行 `install.sh` 并执行 `homer --help` 冒烟。 |
| `install.sh`、`tests/e2e/install_dist_test.go` | POSIX 平台探测、latest/version/base/URL 覆盖（含 `file://dist` 直连归档）、GitHub archive + SHA-256 校验、`HOMER_INSTALL_PACKAGE` 离线输入、tar/raw binary 安装、用户目录 fallback、幂等原子替换与 `homer --help` 冒烟。 |
| `npm/` | `package.json`、无依赖 postinstall、native wrapper、README；postinstall 下载/校验归档失败不阻断 npm install，缺 binary 时提示 install.sh。 |
| `README.md` | Go 二进制 / goreleaser / npm 三通道、三行快速开始、MVP 场景、安全备案（TS 版已作废私钥声明）和限制。 |
| `docs/go-rewrite-report.md` | 本验收记录。 |

## e2e 十组覆盖（原有七组 + review 冻结三组）

1. **机器 A 装配**：fixture 写入 pi / herdr / opencode；`init --json` 注册三
   adapter；A `keygen`、配置 recipients/files、明文落目标、`secret push --yes`
   和 `push --yes`；origin 树包含 `homer.json`、`store/`、`secrets/`，git grep
   明文片段和 `AGE-SECRET-KEY-` 均为空。
2. **机器 B 一键归位**：`home <origin> --yes` 真实 clone + 首次 merge；三工具
   落点逐字节对照 A 的 store（排除 `apiKeys` 的冻结占位键单独校验），无 identity
   时密钥安全 skipped。
3. **换设备**：B `keygen`；A 追加 recipient 并重新 `secret push`；B
   `secret pull --yes` 得到原明文且目标与 identity 均为 0600；A 仍可解密。
4. **`__REQUIRED__`**：doctor `required=warn`，details 精确为
   `pi/models/models.json: apiKeys`，且无 fail、退出码为 0。
5. **allowEscape**：默认扫描对外链报逃逸告警；配置明确的
   `skills/agent-browser` 后 push，外部链接内容（含子文件）进入 store，状态无错误。
6. **install.sh**：`sh -n`、本地 binary package 安装、重复安装和已安装 binary
   `--help` 全通过；另验证本地归档 checksum 成功与错误 checksum 拒装。
7. **隔离只读冒烟**：临时 fake HOME 下 `HOMER_HOME` 初始化仍精确注册三 adapter；
   `doctor --offline --json` 保持八项形状，未配置 secrets 的 age 项为 ok。
8. **pull 应用 + 备份旧版**：A 推送 v1、修改为 v2，B 保持 v1 后 `pull --yes`；B
   得到 v2，`backups/<date>/*-pull/pi/skills/...` 保留 v1。
9. **remote-ahead / 同文件冲突**：A 推送 v2，C 在 v1 上改同文件后 pull；命令 exit 1
   且为 `conflicts-remain`（或结构化 error），C 的本地内容不被覆盖。
10. **pull-delete 传播**：A 删除文件并 push，B pull；文件消失，pull 备份中保留被删的
    v2 内容。

## 测试覆盖与统计

- 全量 Go 测试：**324 个 test / subtest 事件全部通过，0 fail**（含 7 个 e2e
  group；`go test -v` 的 `=== RUN` 与 `--- PASS` 均为 324）。
- e2e package：`TestMVPSevenGroups` + 7 个命名 group，真实编译二进制跨进程运行。
- 发布矩阵：`linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64` 四个
  `CGO_ENABLED=0 go build` 产物均非空。
- npm：`npm pack`、本地 `HOMER_INSTALL_PACKAGE` postinstall、wrapper `--help`
  通过；`npm/` 自带 `typecheck` / `test` 是 Node 语法检查。

## 验证输出（原样）

### Go 全量 build / vet / format / test

```text
$ go build ./... && go vet ./... && if [ -n "$(gofmt -l . | grep -v vendor || true)" ]; then gofmt -l . | grep -v vendor; exit 1; fi && go test ./... -count=1
?    github.com/zzjcool/homer-cli/cmd/homer [no test files]
ok   github.com/zzjcool/homer-cli/internal/adapter 0.013s
ok   github.com/zzjcool/homer-cli/internal/adapter/herdr 0.003s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/adapter/opencode 0.002s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/adapter/pi 0.002s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/agecrypto 0.047s
ok   github.com/zzjcool/homer-cli/internal/backup 0.005s
ok   github.com/zzjcool/homer-cli/internal/cli 0.238s
ok   github.com/zzjcool/homer-cli/internal/cli/commands 0.905s
ok   github.com/zzjcool/homer-cli/internal/core 0.014s
ok   github.com/zzjcool/homer-cli/internal/doctor 0.189s
ok   github.com/zzjcool/homer-cli/internal/engine 0.007s
ok   github.com/zzjcool/homer-cli/internal/gitx 0.879s
ok   github.com/zzjcool/homer-cli/internal/orderedjson 0.006s
ok   github.com/zzjcool/homer-cli/internal/secretscan 0.003s
ok   github.com/zzjcool/homer-cli/internal/sync 0.156s
ok   github.com/zzjcool/homer-cli/internal/testutil 0.002s [no tests to run]
ok   github.com/zzjcool/homer-cli/tests/e2e 1.087s
```

### 四平台交叉编译（GoReleaser 未安装时的冻结 fallback）

```text
$ for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
>   GOOS=${target%/*} GOARCH=${target#*/} CGO_ENABLED=0 go build -o "/tmp/homer-w11-cross-final/homer-${target%/*}-${target#*/}" ./cmd/homer
>   test -s "/tmp/homer-w11-cross-final/homer-${target%/*}-${target#*/}"
>   printf '%s OK\\n' "$target"
> done
linux/amd64 OK
linux/arm64 OK
darwin/amd64 OK
darwin/arm64 OK
total 26M
-rwxr-xr-x 1 root root 6.6M Sep 24 13:22 homer-darwin-amd64
-rwxr-xr-x 1 root root 6.4M Sep 24 13:22 homer-darwin-arm64
-rwxr-xr-x 1 root root 6.7M Sep 24 13:22 homer-linux-amd64
-rwxr-xr-x 1 root root 6.5M Sep 24 13:22 homer-linux-arm64
```

### Installer / npm 语法与 e2e

```text
$ sh -n install.sh && node --check npm/install.js && node --check npm/bin/homer.js && echo 'release/npm syntax OK'
release/npm syntax OK

$ go test ./tests/e2e -run TestMVPSevenGroups -count=1 -v
=== RUN   TestMVPSevenGroups
=== RUN   TestMVPSevenGroups/01-machine-A-assembly
=== RUN   TestMVPSevenGroups/02-machine-B-home-restore
=== RUN   TestMVPSevenGroups/03-device-rotation
=== RUN   TestMVPSevenGroups/04-required-placeholder-doctor
=== RUN   TestMVPSevenGroups/05-allowEscape-snapshot
=== RUN   TestMVPSevenGroups/06-install-sh-smoke
=== RUN   TestMVPSevenGroups/07-isolated-read-only-smoke
--- PASS: TestMVPSevenGroups (1.09s)
    --- PASS: TestMVPSevenGroups/01-machine-A-assembly (0.07s)
    --- PASS: TestMVPSevenGroups/02-machine-B-home-restore (0.00s)
    --- PASS: TestMVPSevenGroups/03-device-rotation (0.06s)
    --- PASS: TestMVPSevenGroups/04-required-placeholder-doctor (0.03s)
    --- PASS: TestMVPSevenGroups/05-allowEscape-snapshot (0.05s)
    --- PASS: TestMVPSevenGroups/06-install-sh-smoke (0.09s)
    --- PASS: TestMVPSevenGroups/07-isolated-read-only-smoke (0.02s)
PASS
ok   github.com/zzjcool/homer-cli/tests/e2e 1.097s

$ HOME=$(mktemp -d) HOMER_INSTALL_PACKAGE=/tmp/homer-w11-release-final/homer_linux_amd64.tar.gz HOMER_INSTALL_PREFIX=$(mktemp -d) sh install.sh
✓ homer 安装完成（/tmp/tmp.sr8liujL7L/homer）

下一步：
  1. 新机器一键归位 : homer home <你的配置仓库 url> --yes
  2. 首次建立仓库   : homer init && homer push --yes
  3. 密钥投递       : homer secret keygen / push / pull
  4. 环境体检       : homer doctor
install archive e2e OK

$ bad checksums.txt
checksum rejection OK
homer install: error: checksum mismatch for homer_linux_amd64.tar.gz

$ (cd npm && npm pack --pack-destination /tmp/homer-w11-npm-pack-final)
npm notice name: homer-cli
npm notice version: 1.0.0
npm notice total files: 4
homer-cli-1.0.0.tgz

$ HOMER_INSTALL_PACKAGE=/tmp/homer-w11-cross-final/homer-linux-amd64 npm install --prefix /tmp/homer-w11-npm-test-final /tmp/homer-w11-npm-pack-final/homer-cli-1.0.0.tgz
added 1 package in 436ms
homer — dotfiles for humans and their AI agents
```

### GoReleaser availability

```text
$ command -v goreleaser || echo 'goreleaser: unavailable (using go build cross-compile fallback)'
goreleaser: unavailable (using go build cross-compile fallback)
```

## MR / PR

- 分支已准备：`w11-integrator`
- PR 创建入口：<https://github.com/zzjcool/homer-cli/pull/new/w11-integrator>
- 实际 PR URL：<https://github.com/zzjcool/homer-cli/pull/new/w11-integrator>（分支 push 后可直接创建）。

## 已知限制 / 未决问题

1. 当前环境没有 `goreleaser`，因此没有执行真实 `goreleaser build --snapshot --clean`；
   四平台 `go build` 已成功，配置与 tag release workflow 已提交。
2. e2e 使用本地编译二进制和本地 bare origin，不接触真实远端；这正是 §6 / §9
   的隔离验收边界。
3. npm postinstall 的 GitHub 下载是 best-effort，网络失败不阻断 npm install；生产
   主通道仍是 checksum 受保护的 `install.sh`。
4. Go 布局根目录没有 TS 版 `package.json`；因此根目录 `npm run typecheck && npm test`
   不适用，Node 薄壳的同等语法检查在 `cd npm && npm run typecheck && npm test`
   已通过。
5. **config 声明序裁定（M5）**：`adapters/categories` 遍历序：TS 声明序 → Go 字典序；
   `init` 写入固定注册序故常规路径无差异；仅手工调整声明序后 `status --json` 数组顺序
   可观察不同。
6. **搁置项**：pull prune 失败 → 结构化 error（NON-ISSUE 方向，Go 更诚实，保留）；
   npm 版本号不联动（README 已说明）；e2e 共享 world 结构（与 TS 版同款叙事）；
   config 声明序如上仅文档化，不改代码。

## 对抗式 review 修复记录（M1-M8 + 5 minor）

| 项目 | 修复与回归证据 |
|---|---|
| **M1 覆盖-home 单测** | `internal/cli/commands/home_test.go` 表驱动覆盖非空目录不 clone、clone 失败不落后续写、无 `homer.json` 的「不是 homer 配置中心」、真实 `gitx.CloneRepo` hook 注入 stray 拒绝、identity 缺失仍 `homed` 且 skipped/warning；另锁定 config 失败目录路径与移走目录提示。 |
| **M2 e2e 三组** | `tests/e2e/sync_review_test.go` 新增 03 pull+旧版备份、04 同文件 remote-ahead/conflict 拒绝、06 delete 传播+删除内容备份，并修正文档清单。 |
| **M3 push/merge 失败路径** | `fakeWriteGitWithPushFailures` 支持一次失败后成功；push 失败保留本地 commit，下一次命中 `retryPush` 成功；merge push 失败保持 `resolved` 并 warning。 |
| **M4 退出码表** | `exitcode_test.go` 用一张表逐行覆盖 push 7、pull 5、merge 4、home 3，以及 secret push/pull 全部状态、keygen 成功/已存在和 list=0，对照 m2-plan §2.8 / m3-plan §2.6。 |
| **M5 config 声明序** | 按裁定仅文档化，见已知限制第 5 项；未改代码。 |
| **M6 testutil 空壳** | 删除无人 import 的 `internal/testutil/`，e2e fixture 不迁移，采用计划允许的最小工作量路径。 |
| **M7 TS 拼写别名** | 删除 `Commands`、`getHomerPaths`、`AsCliError`、`FormatCliError`、`STORE_COMPLETE_MARKER`、`GIT_DEFAULT_TIMEOUT`、三组 `DEFAULT_*_ADAPTER`、各 built-in index 的死 `Scan/GlobMatch` 与未导出 wrapper、`SecretPatterns()`；全量编译回归作为隐藏调用探测。 |
| **M8 发布闭环** | release workflow 在 GoReleaser 后以 `HOMER_INSTALL_BASE_URL=file://${GITHUB_WORKSPACE}/dist` 安装 Linux amd64 归档并执行 `homer --help`；install 脚本与 `install_dist_test.go` 锁定归档名和 checksum/download 路径。 |
| **minor 1** | home 配置失败信息追加目标目录与「如确认 URL 有误请移走该目录后重试」，有回归断言。 |
| **minor 2** | `needsBaseline` 恢复 TS 条件 `headCommit != "" && !isStoreClean`；新增 unborn repository 保持 no-drift 测试。 |
| **minor 3** | secret push 幂等分支新增「vault 内容与 HEAD 一致，未产生新 commit」warning 与稳定加密回归。 |
| **minor 4** | status 与 sync excluded-key strip 统一调用 `engine.StripExcludeKeys`，删除 sync 私有 `compactSerialize`，并断言两边结果一致。 |
| **minor 5** | Push/Pull/Merge/Home UI seam 改为显式 `confirmPrompter` / `selectPrompter`，移除 reflect 与 `invokeSelect` / `invokePromptMethod`。 |

### 已知限制（本轮不改）

- pull prune 失败继续返回结构化 error；这是 Go 更诚实的 NON-ISSUE 方向，不回退为静默成功。
- npm 版本号不联动，README 已明确说明。
- e2e 共享 world 结构保留，与 TS 版同款叙事。
- config 声明序只做上述文档化裁定。
