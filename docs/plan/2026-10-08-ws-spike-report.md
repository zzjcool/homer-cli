# P0 CF/cloudflared WebSocket 透传验证报告

**状态：阻塞，未完成实测。** 本报告按冻结约束记录发现的计划冲突；没有连接 Cloudflare，也没有启动或重启 cloudflared、wsecho、homer-serve 等服务。

## 做了什么

- 阅读了 `docs/plan/2026-10-08-ws-stream-plan.md` 的第 0、A、B/P0 节，以及指定的两份 cloudflared SOP。
- 验证了 Go 1.23.4 与仓库布局。P0 要求 `tests/spike` 有独立 `go.mod`，同时验收命令从主模块根目录运行 `go run ./tests/spike/wsprobe`。Go 根模块命令不会进入嵌套模块；若不额外提供 `go.work`，根目录命令不能按该验收路径解析 spike 包。
- 根据“发现计划有错时停下来报告”的约束，未继续实现或执行 V1–V12/R13；未保留任何 spike 源码。需要计划所有者先明确接受从 `tests/spike` 目录运行，或授权使用外部 workspace 等等价调用方式。没有新增主模块依赖或改动生产文件。

## 测试覆盖 / 逐项结果

下表的 FAIL 表示“未执行、因此没有通过验收”，不是已观测到 Cloudflare 行为失败。所有数字均为本次实际执行数；未测量的网络指标明确记为 N/A。

| 项目 | 结果 | 数字 / 观测 |
|---|---|---|
| V1 Authorization、子协议透传及 101 | FAIL（未执行） | 101 次握手：0；Authorization/子协议观测：N/A |
| V2 1KB/1MB/4MB/8MB 帧往返 | FAIL（未执行） | 完成帧数：0；各尺寸成功/失败：N/A |
| V3 空闲 60/100/150/300/600s | FAIL（未执行） | 空闲连接数：0；观测时长：0s；首次断开时间/close code：N/A |
| V4 应用层 ping 25s 保活 15min | FAIL（未执行） | ping/pong：0/0；观测时长：0s / 900s |
| V5 WebSocket 协议 ping/pong | FAIL（未执行） | control ping/pong：0/0 |
| V6 close 4001/4401 与 reason | FAIL（未执行） | 4001 到达数：0；4401 到达数：0；reason：N/A |
| V7 源站 kill -9 后客户端感知 | FAIL（未执行） | 源站终止试验：0；感知耗时：N/A |
| V8 源站侧 userland blackhole 与 ping 超时发现 | FAIL（未执行） | blackhole 试验：0；发现耗时：N/A |
| V9 临时 cloudflared 重启与客户端重连 | FAIL（未执行） | 重启次数：0；重连耗时：N/A |
| V10 单连接 50 并发 req/res RTT | FAIL（未执行） | 请求数：0/50；p50/p95：N/A |
| V11 `WriteTimeout=3s` 下 Hijack 连接存活 | FAIL（未执行） | 本地连接试验：0；等待时长：0s（目标 >3s） |
| V12 不实现 Hijacker 的 ResponseWriter 包装链 | FAIL（未执行） | 包装链试验：0；错误表现：N/A |
| R13 隧道内 chunked NDJSON 逐行到达 | FAIL（未执行） | 收到行数：0；首行/末行时间差：N/A |

## 验收结论

- **G0 未通过**：V1、V4、V6 均未执行，不能宣称通过。
- **CF 实测空闲上限**：未知，未测；没有可报告的断开时刻或有效下限。
- **A7 的 25s/75s**：本次无实测证据判断是否需要调整。先维持冻结值，不应据此宣称已验证；补测后再决定是否调整。
- **未决问题**：请计划所有者消除“独立嵌套 `go.mod`”与“从主模块根目录直接运行 `go run ./tests/spike/wsprobe`”之间的冲突，并明确可使用的验收命令/外部 workspace。获得决定后才能继续 P0。

## 验证输出

命令：
```text
timeout 20 bash -c 'd=$(mktemp -d); mkdir -p "$d/root/sub/cmd"; printf "module example.com/root\\ngo 1.23.4\\n" > "$d/root/go.mod"; printf "module example.com/sub\\ngo 1.23.4\\n" > "$d/root/sub/go.mod"; printf "package main\\nfunc main(){}\\n" > "$d/root/sub/cmd/main.go"; cd "$d/root"; go run ./sub/cmd; s=$?; rm -rf "$d"; exit $s'
```
结果：
```text
main module (example.com/root) does not contain package example.com/root/sub/cmd

Command exited with code 1
```

命令：
```text
timeout 60 bash -lc 'npm run typecheck && npm test'
```
结果（仓库根目录无 `package.json`，因此 npm 验证不可用）：
```text
npm error code ENOENT
npm error syscall open
npm error path /home/zzjcool/code/homer-cli/.pi-subagents/runs/r-8a8c3f56/worktrees/worker-0/package.json
npm error errno -2
npm error enoent Could not read package.json: Error: ENOENT: no such file or directory, open '/home/zzjcool/code/homer-cli/.pi-subagents/runs/r-8a8c3f56/worktrees/worker-0/package.json'
npm error enoent This is related to npm not being able to find a file.
npm error enoent
npm error A complete log of this run can be found in: /home/zzjcool/.npm/_logs/2026-10-08T15_06_37_990Z-debug-0.log

Command exited with code 254
```

## MR 链接

未创建：本次按计划冲突停止，只有阻塞报告，没有可交付的 spike 实现。
