# P3-W9 · doctor / secret 报告

> 计划：`docs/go-rewrite-plan.md` §2.6、§3-P3-W9  
> 分支：`p3-w9-doctor-secret`  
> 实现 commit：`f83ba64`  
> 独立 worktree：`/root/code/homer-cli-w9`

## 做了什么

- `internal/doctor/checks.go`
  - 实现 config → repo → store-clean → remote → adapters → age → machine → required 八项检查。
  - 配置失败时保留八项 JSON 形状，下游依赖配置的项目以 `ok` + `已跳过` 呈现；支持 `--offline` 语义、`__REQUIRED__` 文件:键详情和 fail→exit 1 判定。
  - age 检查使用 `agecrypto.AgeCryptoPort` 注入，并实现工作区 vault 优先、upstream 回落。
- `internal/cli/commands/doctor.go`
  - 实现 doctor 编排、文本渲染、ordered JSON 输出、退出码和 W8 可调用的独立 `ExecuteDoctor` 入口。
- `internal/cli/commands/secret.go`
  - 实现 keygen / push / pull / list 及报告渲染和 JSON 输出。
  - push 先完整读取源文件，支持多 recipient、`--no-push`、`CommitPaths(secrets/)`，并保留 missing-source 全有或全无。
  - pull 实现 upstream 优先/工作区回落、fetch 失败分叉拒绝旧密文、`--yes` 高危回滚拒绝、全量解密后再写目标、0600 目标和 0700/0600 密钥备份。
- 新增 12 个 Go 测试：八项检查状态/顺序/JSON/退出码，以及 secret 全流程、双 recipient、git grep 铁证、missing-source、备份权限、undecryptable 零写入和 fetch 分叉保护。

## 测试覆盖

- doctor：每项检查的成功/警告/失败路径、offline、配置失败降级、required 文件:键详情、文本渲染和 JSON 形状。
- secret：临时 bare origin + 临时 identity 下的 keygen、重复拒覆盖、push/pull/list；多 recipient 解密；明文/私钥不进 git；仅 secrets/ 提交；目标和备份权限；fetch 失败拒绝回滚；解密失败零写入。

## 验证输出

```text
$ go test ./... -count=1
?    github.com/zzjcool/homer-cli/cmd/homer [no test files]
ok   github.com/zzjcool/homer-cli/internal/adapter 0.016s
ok   github.com/zzjcool/homer-cli/internal/adapter/herdr 0.003s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/adapter/opencode 0.002s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/adapter/pi 0.002s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/agecrypto 0.052s
ok   github.com/zzjcool/homer-cli/internal/backup 0.004s
ok   github.com/zzjcool/homer-cli/internal/cli 0.002s [no test files]
ok   github.com/zzjcool/homer-cli/internal/cli/commands 0.745s
ok   github.com/zzjcool/homer-cli/internal/core 0.009s
ok   github.com/zzjcool/homer-cli/internal/doctor 0.181s
ok   github.com/zzjcool/homer-cli/internal/engine 0.010s
ok   github.com/zzjcool/homer-cli/internal/gitx 0.883s
ok   github.com/zzjcool/homer-cli/internal/orderedjson 0.007s
ok   github.com/zzjcool/homer-cli/internal/secretscan 0.004s
ok   github.com/zzjcool/homer-cli/internal/sync 0.135s
ok   github.com/zzjcool/homer-cli/internal/testutil 0.003s [no test files]

$ go vet ./...
# (no output; exit 0)

$ go build ./...
# (no output; exit 0)

$ gofmt -l internal/doctor/checks.go internal/doctor/checks_test.go internal/cli/commands/doctor.go internal/cli/commands/doctor_test.go internal/cli/commands/secret.go internal/cli/commands/secret_test.go
# (no output; exit 0)
```

本仓库已经切换为 Go 布局，根目录没有 `package.json`，因此通用 TS 命令 `npm run typecheck && npm test` 不适用；Go 的 `go test`、`go vet`、`go build` 已作为对应验证执行。

## MR / PR 链接

- GitHub PR 创建入口（分支已推送）：`https://github.com/zzjcool/homer-cli/pull/new/p3-w9-doctor-secret`

## 未决问题

- `internal/cli/run.go`、`args.go`、`ui.go`、`render.go` 按 W8 边界未修改。当前 P0 `run.go` 仍是 secret 占位分发且没有 doctor 分支；本 worker 提供了独立的 `ExecuteSecret` / `ExecuteDoctor` 入口，待 W8 分发层调用。未擅自改动冻结文件。
