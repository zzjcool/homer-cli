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

- 全量 Go 测试：**377 个 test / subtest 事件全部通过，0 fail**（含原有 7 组、review
  冻结 3 组及 dist installer smoke；`go test -v` 的 `=== RUN` 与 `--- PASS` 均为 377）。
- e2e package：`TestMVPSevenGroups` + `TestMVPSyncReviewGroups` 三个命名组 +
  `TestInstallDistBaseURLArchiveSmoke`，真实编译二进制跨进程运行。
- 发布矩阵：`linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64` 四个
  `CGO_ENABLED=0 go build` 产物均非空。
- npm：`npm pack`、本地 `HOMER_INSTALL_PACKAGE` postinstall、wrapper `--help`
  通过；`npm/` 自带 `typecheck` / `test` 是 Node 语法检查。

## 验证输出（原样）

### Go 全量 build / vet / format / test

```text
$ go build ./... && go vet ./... && if [ -n "$(gofmt -l . | grep -v vendor || true)" ]; then gofmt -l . | grep -v vendor; exit 1; fi && go test ./... -count=1
?    github.com/zzjcool/homer-cli/cmd/homer [no test files]
ok   github.com/zzjcool/homer-cli/internal/adapter 0.015s
ok   github.com/zzjcool/homer-cli/internal/adapter/herdr 0.003s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/adapter/opencode 0.002s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/adapter/pi 0.002s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/agecrypto 0.049s
ok   github.com/zzjcool/homer-cli/internal/backup 0.004s
ok   github.com/zzjcool/homer-cli/internal/cli 0.244s
ok   github.com/zzjcool/homer-cli/internal/cli/commands 1.091s
ok   github.com/zzjcool/homer-cli/internal/core 0.016s
ok   github.com/zzjcool/homer-cli/internal/doctor 0.182s
ok   github.com/zzjcool/homer-cli/internal/engine 0.010s
ok   github.com/zzjcool/homer-cli/internal/gitx 0.882s
ok   github.com/zzjcool/homer-cli/internal/orderedjson 0.007s
ok   github.com/zzjcool/homer-cli/internal/secretscan 0.004s
ok   github.com/zzjcool/homer-cli/internal/sync 0.131s
ok   github.com/zzjcool/homer-cli/tests/e2e 2.975s
```

### GoReleaser snapshot / 四平台归档

```text
$ GOBIN=/tmp/homer-tools go install github.com/goreleaser/goreleaser/v2@latest
$ /tmp/homer-tools/goreleaser release --snapshot --clean
  • starting release
  • skipping announce, publish, and validate...           reason=disabled during snapshot mode
  • snapshotting                                             version=ts-v1.0.0-SNAPSHOT-8069e96
  • building                                                paths=cmd/homer binaries=homer target=darwin_arm64_v8.0
  • building                                                paths=cmd/homer binaries=homer target=linux_amd64_v1
  • building                                                paths=cmd/homer binaries=homer target=linux_arm64_v8.0
  • building                                                paths=cmd/homer binaries=homer target=darwin_amd64_v1
  • archiving                                               name=dist/homer_darwin_arm64.tar.gz
  • archiving                                               name=dist/homer_darwin_amd64.tar.gz
  • archiving                                               name=dist/homer_linux_arm64.tar.gz
  • archiving                                               name=dist/homer_linux_amd64.tar.gz
  • calculating checksums
  • release succeeded after 2s

$ find dist -maxdepth 1 -type f -printf '%f\\n' | sort
artifacts.json
checksums.txt
config.yaml
homer_darwin_amd64.tar.gz
homer_darwin_arm64.tar.gz
homer_linux_amd64.tar.gz
homer_linux_arm64.tar.gz
metadata.json
```

### Installer / npm 语法与 e2e

```text
$ sh -n install.sh && node --check npm/install.js && node --check npm/bin/homer.js && echo 'release/npm syntax OK'
release/npm syntax OK

$ sh -n install.sh && go test ./tests/e2e -run 'Test(InstallDistBaseURLArchiveSmoke|MVPSyncReviewGroups)' -count=1 -v
=== RUN   TestInstallDistBaseURLArchiveSmoke
--- PASS: TestInstallDistBaseURLArchiveSmoke (0.91s)
=== RUN   TestMVPSyncReviewGroups
=== RUN   TestMVPSyncReviewGroups/03-pull-apply-and-backup-old-version
=== RUN   TestMVPSyncReviewGroups/04-remote-ahead-same-file-pull-rejects-conflict
=== RUN   TestMVPSyncReviewGroups/06-pull-delete-propagates-and-backs-up-deleted-content
--- PASS: TestMVPSyncReviewGroups (0.95s)
    --- PASS: TestMVPSyncReviewGroups/03-pull-apply-and-backup-old-version (0.16s)
    --- PASS: TestMVPSyncReviewGroups/04-remote-ahead-same-file-pull-rejects-conflict (0.06s)
    --- PASS: TestMVPSyncReviewGroups/06-pull-delete-propagates-and-backs-up-deleted-content (0.15s)
PASS
ok   github.com/zzjcool/homer-cli/tests/e2e 1.867s

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
$ command -v goreleaser || echo 'goreleaser: unavailable'
goreleaser: unavailable
$ GOBIN=/tmp/homer-tools go install github.com/goreleaser/goreleaser/v2@latest
$ /tmp/homer-tools/goreleaser release --snapshot --clean
  • release succeeded after 2s
```

## MR / PR

- v1.1 分支：`fix/v1.1-audit`
- 实现 commit：`1b45dcb`（完整 hash 见交付 git log）
- PR 创建入口：<https://github.com/zzjcool/homer-cli/pull/new/fix/v1.1-audit>（分支已 push；当前环境无 gh/API token，需在该入口确认创建）。

## 已知限制 / 未决问题

1. GoReleaser 系统 PATH 初始不可用，已按计划用 `go install` 安装到 `/tmp/homer-tools` 并成功运行
   `release --snapshot --clean`；四平台归档、checksum 与 `file://dist` installer smoke 已通过。
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
6. **Docker 验证 P2-1 已修复**：空 HOME 下 `home` 会在首次对接应用前为缺失的 enabled
   adapter root 创建目录；创建失败通过现有 adapter-root warning 通道报告，不再把
   `homed` + 零写入作为唯一信号。
7. **Docker 验证 P2-2 已修复**：`secret pull` 仍按 D4 不自动 ff 整仓，但成功输出现在会
   明确提示同步工作区后再 `homer push`，避免用户直接 push 撞 non-fast-forward。
8. **搁置项**：pull prune 失败 → 结构化 error（NON-ISSUE 方向，Go 更诚实，保留）；
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
| **minor 2** | W11 旧版记录的 unborn `no-drift` 语义由本轮审计裁定覆盖；v1.1 改为首次 push 自动 baseline，见下方 M1 语义裁定与 e2e。 |
| **minor 3** | secret push 幂等分支新增「vault 内容与 HEAD 一致，未产生新 commit」warning 与稳定加密回归。 |
| **minor 4** | status 与 sync excluded-key strip 统一调用 `engine.StripExcludeKeys`，删除 sync 私有 `compactSerialize`，并断言两边结果一致。 |
| **minor 5** | `PushDeps.UI` 改为显式 `selectPrompter`，共享 prompt helper 仅做显式 `confirmPrompter` / `selectPrompter` 断言，移除 reflect 与 `invokeSelect` / `invokePromptMethod`。 |

### 已知限制（本轮不改）

- pull prune 失败继续返回结构化 error；这是 Go 更诚实的 NON-ISSUE 方向，不回退为静默成功。
- npm 版本号不联动，README 已明确说明。
- e2e 共享 world 结构保留，与 TS 版同款叙事。
- config 声明序只做上述文档化裁定。

## v1.1 audit-driven remediation

审计依据：`/tmp/homer-advisor-audit/AUDIT.md` §三、§四；同时核对了
`legacy/ts:src/cli/commands/push.ts` 与 `legacy/ts:src/core/git/git.ts`，确认 TS 时代
的 `commitAllStore` 也只提交 `store/`，因此这是跨实现一起修正的既有缺陷，而不是仅 Go
重写漏移植。

### M1 语义裁定（V2）

冻结的 M1 降级矩阵已升级：`~/.homer` 非 git 仓库不再 `warning + no-drift` 空操作；
`homer push` 会调用 `ensureGitRepo`（`git init -b master` + 幂等 `.gitignore`），提交
`store/`、根目录 `homer.json` 与 `.gitignore` 的「建立同步基线」，无 remote 时完成
local-only 基线并给出接线提示，有 remote 时自动首推并建立 `origin/<branch>` upstream。
该变化以审计复现的端到端回归为准，后续 pull/merge 仍要求仓库与 upstream 已配置。

### V1-V7 修复与回归

- **V1**：`CommitAllStore` 的 pathspec 扩展为 `store/` + `homer.json` + `.gitignore`，保证配置中心识别文件随仓库提交。
- **V2**：首次/无漂移 `push` 也自动初始化 git、提交同步基线；无 remote 保留 local-only warning。
- **V3**：`init --remote <url>` 完成 init、origin 接线、基线 commit 与首推；`homer remote <url>` 幂等 add/set-url origin 并输出 `git -C <home> push -u`。
- **V4**：`home` 对配置中心识别失败明确指出根目录缺少 `homer.json`，兼容旧版 push 的恢复步骤与失败目录移除步骤。
- **V5**：新增 `homer version` / `--version`，开发构建显示 `dev`，GoReleaser 的 `-X main.version={{.Version}}` 注入链路接通。
- **V6**：pull/merge 的 fast-forward 分叉错误追加“两台机器都推送过”及 push / pull --rebase + merge 两条解法。
- **V7**：push 远端失败或未送达时输出实际 home、remote、branch 拼出的 `git -C <home> push -u origin master`（按分支实际值渲染）命令。

新增回归包括 `internal/gitx` 配置中心 pathspec/首推行为、remote/init/version/divergence
命令单测，以及 `tests/e2e/v11_audit_test.go`：A `init` → 手工 `git remote add` →
`push --yes` → bare origin 检查 `homer.json` / `.gitignore` → B clone 后 `home --yes`。

### v1.1 验证记录

新增 e2e 主闭环为 `TestV11AuditMainline`；全量 Go 测试按 `=== RUN` / `--- PASS` 事件计数为
**388 tests/subtests，388 pass，0 fail**（基线 381 + 本轮 7 个回归组）。以下为最终验证命令与
原样输出：

```text
$ npm --prefix npm run typecheck && npm --prefix npm test

> homer-cli@1.0.0 typecheck
> node --check install.js && node --check bin/homer.js


> homer-cli@1.0.0 test
> node --check install.js && node --check bin/homer.js

$ go build ./... && go vet ./... && test -z "$(gofmt -l . | grep -v vendor || true)" && go test ./... -count=1
?    github.com/zzjcool/homer-cli/cmd/homer [no test files]
ok   github.com/zzjcool/homer-cli/internal/adapter 0.013s
ok   github.com/zzjcool/homer-cli/internal/adapter/herdr 0.002s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/adapter/opencode 0.002s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/adapter/pi 0.002s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/agecrypto 0.047s
ok   github.com/zzjcool/homer-cli/internal/backup 0.004s
ok   github.com/zzjcool/homer-cli/internal/cli 0.227s
ok   github.com/zzjcool/homer-cli/internal/cli/commands 1.509s
ok   github.com/zzjcool/homer-cli/internal/core 0.015s
ok   github.com/zzjcool/homer-cli/internal/doctor 0.172s
ok   github.com/zzjcool/homer-cli/internal/engine 0.008s
ok   github.com/zzjcool/homer-cli/internal/gitx 0.909s
ok   github.com/zzjcool/homer-cli/internal/orderedjson 0.006s
ok   github.com/zzjcool/homer-cli/internal/secretscan 0.003s
ok   github.com/zzjcool/homer-cli/internal/sync 0.276s
ok   github.com/zzjcool/homer-cli/tests/e2e 3.611s
$ go test ./... -count=1 -v > /tmp/homer-v11-go-test-final.log 2>&1 && printf ...
RUN_COUNT=388
PASS_COUNT=388
FAIL_COUNT=0
$ git diff --check
```

GoReleaser ldflags 接线手验：

```text
$ /tmp/homer-v11-version-final version
homer version: v1.1.0-audit
$ /tmp/homer-v11-version-final --version
homer version: v1.1.0-audit
```

修复后二进制手动复跑审计主闭环（临时双 HOME + bare origin）：

```text
$ homer init --json
...（init 注册 pi / herdr / opencode，errors=[]）
$ git -C "$HOMER_HOME" init -b master && git -C "$HOMER_HOME" remote add origin "$ORIGIN"
Initialized empty Git repository in /tmp/homer-v11-manual-final2.TbALVw/a-home/.homer/.git/
$ homer push --yes --json
{"ok":true,"status":"pushed","changedFiles":[],"pushedToRemote":true,"warnings":["未配置 git upstream，remote 视作 = base（M1 语义）","本地快照已在 store 中，本次不重写 store 内容；将提交 store + homer.json + .gitignore，建立同步基线"]}
$ git --git-dir "$ORIGIN" ls-tree -r --name-only master
.gitignore
homer.json
store/herdr/.homer-complete
store/opencode/.homer-complete
store/pi/.homer-complete
$ HOME="$ROOT/b-home" HOMER_HOME="$ROOT/b-home/.homer" homer home "$ORIGIN" --yes --json
{"ok":true,"status":"homed","cloned":true,"firstContact":{"mode":"merge","applied":{"written":[],"deleted":[],"conflicts":[]},"conflicts":[]},"doctor":{"ok":true,...},"errors":[]}
$ HOME="$ROOT/b-home" HOMER_HOME="$ROOT/b-home/.homer" homer --version
homer version: dev
MANUAL_ROOT=/tmp/homer-v11-manual-final2.TbALVw
```

手动闭环结果：bare origin 含 `homer.json` 与 `.gitignore`，新机器 `home --yes` 返回
`status=homed` / `doctor.ok=true`，版本命令和 ldflags 注入均通过。实现 commit 为
`1b45dcb`；当前工作分支为 `fix/v1.1-audit`，PR 创建入口见上。
