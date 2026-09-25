# homer-cli v1.3 P3 收口报告

> 本报告对应分支 `feat/v13-p3`。P3 只修改进程边界 e2e、README 与本报告；
> P0–P2 的协议、tailcat 适配器和 CLI 接缝以 `927ce8e` 为基线。

## 波次结果

| 波次 | 结果 | 依据 |
|---|---|---|
| P0 | 通过 | `deb8124`：`state.Paired`、doctor age 展示、age vault 导出、pair scaffold。 |
| P1 | 通过 | `1e366c5`：帧协议、`pairtest.NewPipePair`、serve/join all-or-nothing 全链路。 |
| P2 | 通过 | `6bd72b1`：tailcat CLI 子进程适配器、CLI 注册和降级测试。 |
| 接缝修复 | 通过 | `927ce8e`：真实 CLI 默认装配 `TailcatTransport`，serve 在非 JSON 路径立即输出地址。 |
| P3 | 通过（机器验收）；真实双机待人工 | 本分支新增 `tests/e2e/v13_pair_test.go`、pair README 章节和人工清单。 |

### P3 e2e 覆盖

1. 用真实 `homer` 二进制并把 `PATH` 置空：`pair` 返回 1，stderr 同时包含
   官方 tailcat 安装链接、`homer secret push` 和 `homer secret pull`。
2. 用 PATH 注入的假 `tailcat` 写 `TAILCAT_ADDR_FILE` 后以 `/bin/cat` 挂起：CLI
   stdout 立即出现地址和“勿入 git”安全提示；终止 fake tailcat 进程组后，使用
   `pgrep -af tailcat` 轮询确认没有该 fixture 的残留进程。
3. 进程边界冒烟覆盖 `pair --help`、`-h`、`--json`、`--yes --json`、位置参数、
   未知/不支持选项和布尔值校验。
4. 写入三个 `state.json` paired 设备后运行真实 `homer doctor --offline --json`，
   断言 age check 显示“已配对 3 台设备”及三条脱敏指纹详情。
5. 协议全链路使用冻结的 Go 级 `pairtest.NewPipePair` fallback：A/B 进程内完成
   hello → confirm bypass → age 多 recipient 重加密 → 解密验证 → vault/destination
   落盘 → ack → paired state。A/B report 均为 `paired`，目标文件为 0600 且逐字节
   一致，B vault 可解密，A recipients/state 均已更新。

第 5 项按计划 R6 的取舍执行：fake tailcat 的进程级双向流在不同平台上依赖子进程
stdin/stdout 与 socket/FIFO 生命周期，容易把传输适配器的时序噪声误报为协议回归；
因此 CI 用冻结的 `PipeTransport` 覆盖完整协议，同时由第 1–3 项覆盖真实 CLI 的
启动、探测、地址输出、降级和进程组清理。真实 tailcat 双端与 DERP 路径留在下面的
人工清单，不宣称 CI 已完成真实双机验收。

## 测试数变化

计划文档记录的 v1.2 基线为 455。按 `go test -json ./...` 的 passing test event
计数，当前 `master`/`927ce8e` 实际为 499；P3 新增 18 个 test events（6 个顶层
测试，其中 CLI option matrix 含 12 个子测试），最终为 **517**，达到计划要求的
至少 515。P3 新增测试文件不修改既有测试断言。

## 冻结接口 diff 核对

- `git diff master...HEAD -- internal` 为空；P3 没有触碰冻结接口所属的
  `internal/core`、`internal/agecrypto`、`internal/pair`、`internal/cli` 或
  `internal/doctor` 文件。
- §2.1 的 `PairedDevice` / `HomerState` / `AddPairedDevice` / `PairedDisplay`、
  §2.2 的两个 age 导出、§2.3 的 `PairTransport`/`PairServer`/tailcat 常量与
  §2.4 的 `PairOptions`、报告类型和 `ExecutePair` 签名均保持 `927ce8e` 版本。
- P3 只通过公开 seam 调用 `commands.RunPairServe`、`commands.RunPairJoin` 和
  `pairtest.NewPipePair`；没有新增生产接口或调整协议帧格式。

## 人工双机验收清单（待真实环境执行）

以下项目需要两台不同网络/NAT 的真机、release 二进制和 tailcat v0.7.0+；CI 不伪造
“已执行”状态：

1. [ ] 两台机器安装 tailcat v0.7.0+ 与 homer v1.3 release 二进制。
2. [ ] A 执行 `homer pair`，记录地址出现耗时（预期小于 10 秒）。
3. [ ] 地址只经当面/私密信道传递，确认未进入 git、聊天记录、issue 或共享日志。
4. [ ] B 执行 `homer pair <tc-addr>`；A 核对 hostname 与 recipient 指纹后确认。
5. [ ] 用 `tailcat ping <addr>` 记录 direct 或 `via DERP(<region>)` 路径、耗时和重试。
6. [ ] B 检查每个 destination 内容逐字节一致、权限 0600、`secret list` vault present，
   且 `homer doctor` age 项通过。
7. [ ] A 检查 `homer.json` 含 B recipient、`state.json` 含 paired，doctor 显示已配对 1 台。
8. [ ] A 等待连接时 Ctrl-C：`pgrep -af tailcat` 无残留且地址文件删除；A 确认门选否时
   双方 exit 1 且 A 的 `homer.json` 不变。（fake tailcat 进程组部分已有自动化覆盖。）
9. [ ] B 再执行 `homer secret push`，确认 git 通道与 pair 通道互不干扰。

## Review 与未决项

本 worker 对新增 e2e 做了本地对抗检查：空 PATH、非零退出码、JSON/文本 stdout 与
stderr 分流、地址警告、进程组清理、0600 权限、state/doctor 展示和 PipeTransport
全链路均有断言；`git diff --check` 无告警。真实双机、DERP、自建 derper 和 Ctrl-C
人工用例仍未决，需按上表执行。由于当前 worker 受 frozen child 约束不能调度其它
pane，未执行额外 fresh-context review；没有因此修改生产代码。

## 验证输出

最终收口在本分支执行的原样输出如下；`gofmt -l` 没有输出：

```text
$ go build ./...
$ go vet ./...
$ gofmt -l . | grep -v vendor
$ go test -count=1 ./...
?    github.com/zzjcool/homer-cli/cmd/homer             [no test files]
ok   github.com/zzjcool/homer-cli/internal/adapter      0.015s
ok   github.com/zzjcool/homer-cli/internal/adapter/herdr 0.003s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/adapter/opencode 0.003s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/adapter/pi  0.004s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/adapter/vscode 0.003s
ok   github.com/zzjcool/homer-cli/internal/agecrypto    0.073s
ok   github.com/zzjcool/homer-cli/internal/backup       0.005s
ok   github.com/zzjcool/homer-cli/internal/cli          0.261s
ok   github.com/zzjcool/homer-cli/internal/cli/commands 1.720s
ok   github.com/zzjcool/homer-cli/internal/core         0.018s
ok   github.com/zzjcool/homer-cli/internal/doctor       0.207s
ok   github.com/zzjcool/homer-cli/internal/engine       0.008s
ok   github.com/zzjcool/homer-cli/internal/gitx        0.992s
ok   github.com/zzjcool/homer-cli/internal/manifest     0.169s
ok   github.com/zzjcool/homer-cli/internal/orderedjson  0.007s
ok   github.com/zzjcool/homer-cli/internal/pair        0.334s
ok   github.com/zzjcool/homer-cli/internal/pair/pairtest 0.014s
ok   github.com/zzjcool/homer-cli/internal/secretscan    0.003s
ok   github.com/zzjcool/homer-cli/internal/sync         0.311s
ok   github.com/zzjcool/homer-cli/tests/e2e              7.885s
$ CGO_ENABLED=0 go build -trimpath ./cmd/homer
verification: PASS
```

`npm run typecheck && npm test` 不适用：仓库没有 `package.json`，原样结果为：

```text
$ npm run typecheck && npm test
npm error code ENOENT
npm error syscall open
npm error path /root/code/homer-cli-p3/package.json
npm error errno -2
npm error enoent Could not read package.json: Error: no such file or directory, open '/root/code/homer-cli-p3/package.json'
npm error enoent This is related to npm not being able to find a package.json file.
npm error enoent
```

因此 v1.3 使用 Go 的 build/vet/gofmt/test 验收链；Go 全量为 517 个 passing test
events，所有 Go 终检命令 exit 0。

## MR / PR

分支已 push：`feat/v13-p3`。GitHub push hook 提供的 PR 创建 URL：
<https://github.com/zzjcool/homer-cli/pull/new/feat/v13-p3>

当前 worker 没有 `gh`/`glab` 或 GitHub API token；匿名 API 请求还受到 rate limit（HTTP
403），所以无法在无人值守环境中代替维护者点击创建 PR。上面的 URL 可直接打开并以
本提交为 base `master`、compare `feat/v13-p3` 创建 PR。
