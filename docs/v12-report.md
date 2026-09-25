# homer-cli v1.2 P3-W5 验收记录

## 范围

本记录对应 `docs/v12-plan.md` 的 P3-W5：e2e 主线、custom adapter 全链路、
向导的进程内集成覆盖，以及 README 的 v1.2 使用与安全说明收口。W1-W4 的引擎、
向导、disabled 呈现、manifest 应用和 `home` 远端命令门禁均作为本轮黑盒前置能力，
测试只通过编译后二进制或冻结的 Go 依赖 seam 使用它们。

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
| 冻结接口 diff | 本轮仅新增 `tests/e2e` 与文档，未修改 §2 冻结接口实现 |
| e2e 子进程隔离 | 通过；假 HOME、假 origin、假命令均位于临时目录 |

本轮检查未添加 shell 插值：e2e 的假命令以可执行脚本文件作为命令路径，所有
差异通过进程环境 `$HOME` 和临时 world 目录表达。manifest 的安装日志也验证了
apply 是逐 ID 执行且远端 gate warning 被保留。

## Review 备注

按正确性、回归、覆盖和简洁性四个角度复核了新增测试：主线覆盖 init → patch
config → push → home / abort；custom 测试覆盖手写声明、disabled 和逐字节文件；
向导测试只注入 `WizardPort`，不模拟脆弱的 TTY。没有发现需放宽冻结接口或改动
W1-W4 实现的问题。

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

仓库是 Go 项目且没有 `package.json`；因此 `npm run typecheck && npm test` 不适用，
该命令因缺少 `package.json` 返回 ENOENT，未将无关的 npm 文件引入本次变更。
