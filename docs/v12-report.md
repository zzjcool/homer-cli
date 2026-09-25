# homer-cli v1.2 P3-W5 验收记录与对抗式 review 修复

## 范围

本记录对应 `docs/v12-plan.md` 的 P3-W5：e2e 主线、custom adapter 全链路、
向导的进程内集成覆盖，以及 README 的 v1.2 使用与安全说明收口；并记录随后
adversarial review 的综合修复。W1-W4 的引擎、向导、disabled 呈现、manifest 应用和
`home` 远端命令门禁均作为前置能力，新增回归测试通过 Go 依赖 seam 与真实子进程
边界分别锁定安全行为。

## 覆盖内容

- `tests/e2e/v12_manifest_test.go`
  - 使用临时目录建立假 origin、机器 A/B/C 三套隔离 `HOME` / `HOMER_HOME`。
  - 每个 CLI 断言均运行编译后的 `homer` 子进程；A 的 PATH 注入假 `code`，其
    `--list-extensions` 返回两个 ID，`--install-extension` 写安装日志。
  - A 先 `init --json`，确认 VS Code manifest 虚拟文件，再通过配置补丁注册
    `fakecli`（普通 conf 文件 + plugins manifest）；list/apply 是共享 world 目录
    脚本，清单和日志按 `$HOME` 区分。
  - B 仅本地安装各自的一个 ID，`home origin --yes --json` 后确认文件归位、两个
    缺失 ID 均已安装、两个假安装日志、远端 manifest 门禁 warning，以及 store 中
    VS Code 虚拟文件的逐字节内容。
  - C 在非 TTY 无 `--yes`（显式 `--mode merge` 只用于选定首次对接模式）确认返回
    `aborted`，且错误包含 manifest 安全提示。
- `tests/e2e/v12_custom_test.go`
  - 手写 `homer.json` 声明 `fakecli` 的 `conf` / `plugins` / disabled 分类。
  - `status --json` 确认普通文件与 manifest 虚拟文件计数、disabled 汇总；push
    后确认 origin 不含 disabled 分类。
  - 新机 home 后确认配置文件逐字节一致、manifest 只安装缺失 ID、disabled 文件
    不出现。
  - 进程内 `RunInitWithDeps` + fake `WizardPort` 集成测试确认只选中的
    `pi/settings` 写入 store，其余 adapter/category 被禁用并不产生快照。

## 文档收口

README 现在明确说明：

- TTY 向导的三级选择、默认全选、`--all` / 非 TTY 旁路与 re-init 不重写 store
  的语义；
- VS Code 默认 adapter、manifest 虚拟文件、集合并集和“只装不卸”语义，以及
  `home` 的远端命令门禁；
- D4 残余向量：pull fast-forward 带入新 `homer.json` 后，下一次 status 可能执行
  新 manifest `listCmd` / `applyCmd` 声明，及 v1.3 指纹信任库计划。

## 验收记录

| 项目 | 结果 |
| --- | --- |
| `go vet ./...` | 通过 |
| `gofmt -l .` | 无输出（通过） |
| `go test ./...` | 通过；445 个 run action（148 个顶层测试，含 297 个子测试） |
| 冻结接口 diff | review 修复保持既有命令/报告调用签名；仅为 ScanOutcome/ScanProblem 与 VS Code 根目录增加 additive 能力 |
| e2e 子进程隔离 | 通过；假 HOME、假 origin、假命令均位于临时目录 |

本轮检查未添加 shell 插值：e2e 的假命令以可执行脚本文件作为命令路径，所有
差异通过进程环境 `$HOME` 和临时 world 目录表达。manifest 的安装日志也验证了
apply 是逐 ID 执行且远端 gate warning 被保留。

## Review 备注

初始验收按正确性、回归、覆盖和简洁性四个角度复核了 init → patch config → push
→ home / abort、custom adapter、disabled 和向导 seam；本次 adversarial review
进一步锁定 home 的确认前零命令、CLI 缺失降级、跨平台 VS Code 根目录、pull 配置
变更提示、进程边界和死代码/所有权收口。survey label 往返与 pull `--yes` manifest
留痕仍按冻结计划列为已知限制。

## 对抗式 review 综合修复记录

- **C1**：`home` 先选择首次对接模式，非 `--yes` 与 skip 阶段为 manifest 分类放入空本地快照并延迟 `listCmd`，确认后才扫描；`--yes` 保留既有 manifest 命令 warning。
- **M1**：CLI 缺失通过 `errors.As(*exec.Error)` 识别为非阻断 `ScanOutcome.Warnings`，以“空清单/远端全量待装”文案透传 status、pull、home。
- **M2**：VS Code 默认根目录按 `runtime.GOOS` 选择 Linux `~/.config/Code` 或 Darwin `~/Library/Application Support/Code`，两平台常量有回归测试。
- **M3**：pull 在 fast-forward 前读取 upstream 的 `homer.json` 比较 manifest `kind/listCmd/applyCmd`，将旧值 → 新值写入 warning、预览与非 `--yes` 确认。
- **M4**：删除零调用兼容别名、`WizardState.ReInit`、home 死包装，并内联 manifest command accessor。
- **M5**：`PullDeps` / `MergeDeps` 分别归属 `pull.go` / `merge.go`，共享 `GitPort` 仍留在 `push.go`。

五项 minor 状态：

1. `isCommandMissing` 已改为 `errors.As` 类型判定，exit error 不再误判；✅
2. manifest 分类声明 `exclude` / `excludeKeys` 已在 `validateConfig` 直接拒绝；✅
3. manifest 子进程已设 process group、超时杀组，stdout 以 16 MiB 上限截断并产生 `ScanProblem`；✅
4. 子进程环境已收敛为 `PATH/HOME/TERM/LANG`，`HOMER_*` 泄漏 PoC 有回归测试；✅
5. `status` 已复用导出的 `adapter.CategoryOrder`，移除重复排序表；✅

已知限制（按冻结计划搁置）：pull 的 `--yes` manifest 执行留痕仍未补成与 home
完全对称的 warning；survey label 往返测试不模拟 TTY；`listCmd` 输出本身可能读取
敏感文件（README 安全节已披露其会进入 store 并随 push 上传）。

## 原样验证输出

```text
$ go build ./...
$ go vet ./...
$ gofmt -l . | grep -v vendor
$ go test ./...
?   	github.com/zzjcool/homer-cli/cmd/homer	[no test files]
?   	github.com/zzjcool/homer-cli/internal/adapter/vscode	[no test files]
ok  	github.com/zzjcool/homer-cli/internal/adapter	(cached)
ok  	github.com/zzjcool/homer-cli/internal/adapter/herdr	(cached) [no tests to run]
ok  	github.com/zzjcool/homer-cli/internal/adapter/opencode	(cached) [no tests to run]
ok  	github.com/zzjcool/homer-cli/internal/adapter/pi	(cached) [no tests to run]
ok  	github.com/zzjcool/homer-cli/internal/agecrypto	(cached)
ok  	github.com/zzjcool/homer-cli/internal/backup	(cached)
ok  	github.com/zzjcool/homer-cli/internal/cli	(cached)
ok  	github.com/zzjcool/homer-cli/internal/cli/commands	(cached)
ok  	github.com/zzjcool/homer-cli/internal/core	(cached)
ok  	github.com/zzjcool/homer-cli/internal/doctor	(cached)
ok  	github.com/zzjcool/homer-cli/internal/engine	(cached)
ok  	github.com/zzjcool/homer-cli/internal/gitx	(cached)
ok  	github.com/zzjcool/homer-cli/internal/manifest	(cached)
ok  	github.com/zzjcool/homer-cli/internal/orderedjson	(cached)
ok  	github.com/zzjcool/homer-cli/internal/secretscan	(cached)
ok  	github.com/zzjcool/homer-cli/internal/sync	(cached)
ok  	github.com/zzjcool/homer-cli/tests/e2e	4.822s
```

仓库根目录是 Go 项目；本轮同时在 `npm/` wrapper 目录执行了其 Node 语法检查脚本，未改动 npm 文件。

## Review 修复终检原样验证输出

```text
$ go build ./...
$ go vet ./...
$ gofmt -l .
$ go test -count=1 ./...
?    github.com/zzjcool/homer-cli/cmd/homer [no test files]
ok   github.com/zzjcool/homer-cli/internal/adapter 0.017s
ok   github.com/zzjcool/homer-cli/internal/adapter/herdr 0.002s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/adapter/opencode 0.002s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/adapter/pi 0.003s [no tests to run]
ok   github.com/zzjcool/homer-cli/internal/adapter/vscode 0.002s
ok   github.com/zzjcool/homer-cli/internal/agecrypto 0.050s
ok   github.com/zzjcool/homer-cli/internal/backup 0.004s
ok   github.com/zzjcool/homer-cli/internal/cli 0.229s
ok   github.com/zzjcool/homer-cli/internal/cli/commands 1.591s
ok   github.com/zzjcool/homer-cli/internal/core 0.009s
ok   github.com/zzjcool/homer-cli/internal/doctor 0.168s
ok   github.com/zzjcool/homer-cli/internal/engine 0.007s
ok   github.com/zzjcool/homer-cli/internal/gitx 0.931s
ok   github.com/zzjcool/homer-cli/internal/manifest 0.160s
ok   github.com/zzjcool/homer-cli/internal/orderedjson 0.008s
ok   github.com/zzjcool/homer-cli/internal/secretscan 0.004s
ok   github.com/zzjcool/homer-cli/internal/sync 0.278s
ok   github.com/zzjcool/homer-cli/tests/e2e 4.830s

$ (cd npm && npm run typecheck && npm test)

> homer-cli@1.0.0 typecheck
> node --check install.js && node --check bin/homer.js


> homer-cli@1.0.0 test
> node --check install.js && node --check bin/homer.js
```

共 **455 个 Go test action** 通过（无失败、无跳过）。

## 交付

- 分支：`fix/v1.2-adversarial-review`
- MR/PR：<https://github.com/zzjcool/homer-cli/pull/new/fix/v1.2-adversarial-review>
- fix commit hash 以交付时 `git rev-parse HEAD` 为准；本轮未本地 merge 到 `master`。
