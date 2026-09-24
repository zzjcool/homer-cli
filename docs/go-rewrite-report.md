# Go 重写 P5 · W11-integrator 收官验收报告

> 基线：`a00f0cd`（master）  
> 分支：`w11-integrator`  
> 目标：按 `docs/go-rewrite-plan.md` §6 / §7 / §9 完成 Go 版 MVP e2e、发布管线与文档收官。  
> 最终 commit：见本分支最后一个 commit（提交后回填）

## 一句话结论

W11 完成：真实编译后的 Go CLI 在双假 HOME + 假 bare origin 中跑通七组 MVP
验收；Linux/macOS × amd64/arm64 四平台交叉构建成功；二进制 installer 的本地
package、checksum、幂等安装与 `--help` 冒烟通过；npm 兼容薄壳可打包并能从本地
binary 形态安装。

## 做了什么

| 文件 / 目录 | 交付 |
|---|---|
| `tests/e2e/mvp_test.go` | 七组真实子进程 e2e：A 三 adapter 装配与密钥推送、B `home` 配置归位、换设备 recipient 重加密、`__REQUIRED__` doctor 精确 details、allowEscape 快照、install.sh、隔离只读 init/doctor。所有 fixture 使用 `t.TempDir()`，fake `HOME` / `HOMER_HOME` 和文件系统 bare origin。 |
| `.goreleaser.yaml` | GoReleaser v2 配置；CGO 关闭；Linux/macOS × amd64/arm64 四归档；`checksums.txt`；GitHub Release owner/name；版本 ldflags。 |
| `.github/workflows/release.yml` | `v*` tag 触发 GoReleaser GitHub Release。 |
| `install.sh` | POSIX 平台探测、latest/version/base/URL 覆盖、GitHub archive + SHA-256 校验、`HOMER_INSTALL_PACKAGE` 离线输入、tar/raw binary 安装、用户目录 fallback、幂等原子替换与 `homer --help` 冒烟。 |
| `npm/` | `package.json`、无依赖 postinstall、native wrapper、README；postinstall 下载/校验归档失败不阻断 npm install，缺 binary 时提示 install.sh。 |
| `README.md` | Go 二进制 / goreleaser / npm 三通道、三行快速开始、MVP 场景、安全备案（TS 版已作废私钥声明）和限制。 |
| `docs/go-rewrite-report.md` | 本验收记录。 |

## e2e 七组覆盖

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
$ go build ./... && go vet ./... && test -z "$(gofmt -l . | grep -v '^vendor/' || true)" && go test ./... -count=1
?    github.com/zzjcool/homer-cli/cmd/homer [no test files]
ok   github.com/zzjcool/homer-cli/internal/adapter 0.012s
ok   github.com/zzjcool/homer-cli/internal/adapter/herdr 0.002s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/adapter/opencode 0.003s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/adapter/pi 0.002s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/agecrypto 0.054s
ok   github.com/zzjcool/homer-cli/internal/backup 0.005s
ok   github.com/zzjcool/homer-cli/internal/cli 0.254s
ok   github.com/zzjcool/homer-cli/internal/cli/commands 0.901s
ok   github.com/zzjcool/homer-cli/internal/core 0.018s
ok   github.com/zzjcool/homer-cli/internal/doctor 0.192s
ok   github.com/zzjcool/homer-cli/internal/engine 0.008s
ok   github.com/zzjcool/homer-cli/internal/gitx 0.918s
ok   github.com/zzjcool/homer-cli/internal/orderedjson 0.012s
ok   github.com/zzjcool/homer-cli/internal/secretscan 0.004s
ok   github.com/zzjcool/homer-cli/internal/sync 0.145s
ok   github.com/zzjcool/homer-cli/internal/testutil 0.004s
ok   github.com/zzjcool/homer-cli/tests/e2e 1.096s
```

### 四平台交叉编译（GoReleaser 未安装时的冻结 fallback）

```text
$ for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do GOOS=... GOARCH=... CGO_ENABLED=0 go build ...; printf '%s OK\n' "$target"; done
linux/amd64 OK
linux/arm64 OK
darwin/amd64 OK
darwin/arm64 OK
total 26M
-rwxr-xr-x 1 root root 6.6M ... homer-darwin-amd64
-rwxr-xr-x 1 root root 6.4M ... homer-darwin-arm64
-rwxr-xr-x 1 root root 6.7M ... homer-linux-amd64
-rwxr-xr-x 1 root root 6.5M ... homer-linux-arm64
```

### Installer / npm 语法与 e2e

```text
$ sh -n install.sh && node --check npm/install.js && node --check npm/bin/homer.js
release/npm syntax OK

$ go test ./tests/e2e -run TestMVPSevenGroups -count=1 -v
--- PASS: TestMVPSevenGroups (1.07s)
    --- PASS: TestMVPSevenGroups/01-machine-A-assembly (0.07s)
    --- PASS: TestMVPSevenGroups/02-machine-B-home-restore (0.00s)
    --- PASS: TestMVPSevenGroups/03-device-rotation (0.06s)
    --- PASS: TestMVPSevenGroups/04-required-placeholder-doctor (0.03s)
    --- PASS: TestMVPSevenGroups/05-allowEscape-snapshot (0.05s)
    --- PASS: TestMVPSevenGroups/06-install-sh-smoke (0.10s)
    --- PASS: TestMVPSevenGroups/07-isolated-read-only-smoke (0.02s)
PASS
ok   github.com/zzjcool/homer-cli/tests/e2e 1.077s

$ HOMER_INSTALL_PACKAGE=/tmp/homer-w11-release/homer_linux_amd64.tar.gz HOMER_INSTALL_PREFIX=/tmp/... sh install.sh
✓ homer 安装完成 (/tmp/.../homer)

$ bad checksums.txt
checksum rejection OK
homer install: error: checksum mismatch for homer_linux_amd64.tar.gz

$ (cd npm && npm pack --pack-destination /tmp/homer-w11-npm-pack)
npm notice name: homer-cli
npm notice version: 1.0.0
npm notice total files: 4
homer-cli-1.0.0.tgz

$ HOMER_INSTALL_PACKAGE=/tmp/homer-w11-cross/homer-linux-amd64 npm install --prefix /tmp/homer-w11-npm-test /tmp/homer-w11-npm-pack/homer-cli-1.0.0.tgz
added 1 package in 415ms
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
- 实际 PR URL：提交并 push 后回填；若当前环境不能创建 GitHub PR，则以上入口是可直接打开的 MR/PR 链接。

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
