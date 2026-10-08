# P0：CF/cloudflared WebSocket 透传验证报告

**日期**：2026-10-08
**范围**：`tests/spike/**` 独立 Go module；不改生产 `internal/**`、根 `go.mod/go.sum`、生产 cloudflared 配置或服务。
**TryCloudflare**：`https://ban-retreat-group-computers.trycloudflare.com`（仅临时隧道；V9 重启后临时地址为 `https://manufacturer-abraham-seafood-night.trycloudflare.com`）。
**cloudflared**：2026.10.0；显式使用 `--protocol http2`；日志确认两次均以 `protocol=http2` 注册，location=hkg13。

## 做了什么

- 新增独立模块 `github.com/zzjcool/homer-cli/tests/spike`，`go 1.23.4`，只依赖 `github.com/coder/websocket v1.8.12`。
- `wsecho` 提供 WebSocket echo、请求头/子协议回显、应用层 ping/pong、按需/周期推送、可配置 close code/reason、`/ndjson` chunked+flush 端点、`http.Server.WriteTimeout` 配置，以及不暴露 `http.Hijacker` 的测试路由。
- `wsecho -proxy-to ...` 提供隔离的 userland TCP 转发/blackhole，用于 V8；控制监听默认只绑定 loopback。
- `wsprobe` 输出 JSON，每项都有 pass/fail/not-run、耗时和实测数字，覆盖 V1~V12、R13；V3 五个空闲目标并行，V3/V4 长测试在后台进程并行执行。
- 本地测试拓扑：echo 源站 `127.0.0.1:17802`；userland proxy `127.0.0.1:17801`；loopback proxy control `127.0.0.1:17803`；V11 专用 `WriteTimeout=3s` 源站 `127.0.0.1:17804`。TryCloudflare 只指向 `http://127.0.0.1:17801`。
- `getent` 对 quick tunnel 返回 sb-tun fake-IP `198.18.4.83`，但本机 `curl` 与 WSS 均能访问，故 TryCloudflare 可用；没有启用备用 tunnel，也没有访问 `homerhw.openaaas.org`。

## 测试覆盖

- 单测覆盖：升级头与子协议、echo/大帧、close code/reason、protocol Ping/Pong、V3/V4 短时行为、50 并发 req/res、WriteTimeout hijack、NoHijacker 拒绝、NDJSON flush、源站 userland blackhole、按需推送、suite/percentile 工具函数。
- 运行 `go test -race -count=1 ./...`、`go test -count=1 ./...`、`go vet ./...`；全部 Go 验证通过。
- 实测完整 `-suite all`。整体 JSON 状态为 `fail`，原因是 V3 的 >100s 空闲连接被断开，以及 R13 经隧道缓冲；不是探针崩溃。两项都不是 G0 闸口。

## P0 实测结果

| 项 | 结果 | 实测数字 / 说明 |
|---|---|---|
| V1 | **PASS** | HTTP `101`；Authorization 1 个值原样回显；`Sec-WebSocket-Protocol: homer.stream.v1` 被看到并协商为 `homer.stream.v1`。 |
| V2 | **PASS** | 文本帧 1 KiB / 1 MiB / 4 MiB / 8 MiB 均 SHA-256 无损；本轮 RTT 分别 `491.267ms / 3185.378ms / 11720.098ms / 21806.800ms`。 |
| V3 | **FAIL（非 G0 闸口）** | 空闲 `60s` 存活 `60.001s`；`100s` 存活 `100.023s`；目标 `150s/300s/600s` 的连接分别在 `124.997s/124.905s/123.906s` EOF，close code `-1`（无 WebSocket Close frame，底层读到 EOF）。首次观察到断开的时间 `123.906s`；实测界限 `>=100s 且 <150s`。 |
| V4 | **PASS** | 应用层 ping 间隔 `25s`，目标 `900s`；连接存活 `900.361s`，`37` 个 ping、`0` 个 missed pong，最大 pong RTT `1979.008ms`。 |
| V5 | **PASS** | WebSocket control Ping/Pong 透传；Pong RTT `246.536ms`。 |
| V6 | **PASS** | close `4001 / "spike-close-4001"` 与 `4401 / "spike-close-4401"` 均原码、原 reason 到达。 |
| V7 | **PASS** | 对隔离的 wsecho 源站进程执行 SIGKILL；客户端 `0.0414s` 感知，读错误 EOF，close code `-1`。 |
| V8 | **PASS** | 源站侧 userland TCP proxy 双向 blackhole；25s 应用 ping、75s ping timeout；`75.010s` 检测，发出 `3` 个 ping，proxy 丢弃 `132` bytes。 |
| V9 | **PASS** | 只杀/重启本轮以 `--url http://127.0.0.1:17801 --protocol http2` 启动的临时 cloudflared；旧连接 `0.043s` 断开，新 TryCloudflare 地址 `manufacturer-abraham-seafood-night.trycloudflare.com`；新连接重连用时 `68.374s`。 |
| V10 | **PASS** | 单连接 `50/50` req/res、50 个唯一 ID；RTT p50 `390.647ms`、p95 `390.791ms`（min `243.141ms`，max `390.802ms`）。 |
| V11 | **PASS** | 本机 Go `http.Server.WriteTimeout=3s`；Hijack 后等待 `4s` 仍能 echo，连接存活，echo RTT `0.359ms`。 |
| V12 | **PASS（观察到明确拒绝）** | 包装器不实现 `http.Hijacker`；coder/websocket `Accept` 返回 HTTP `501 Not Implemented`，Dial 明确拿到非 101，无 panic。P2 仍须按冻结计划在 hub 层映射成明确 HTTP `500`。 |
| R13 | **FAIL** | 源站 `/ndjson` 无 Content-Length、逐行 Flush；本机直连对照为 HTTP/1.1 `Transfer-Encoding: chunked`，5 行逐行约 1s 到达（首末差 `4.002s`）。经 TryCloudflare 后客户端为 HTTP/2.0，5 行都在约 `5.196s` 才出现，首末差仅 `0.0000162s`（约 `16.2µs`），明显被缓冲/合并。另两次复测也失败，首末差分别约 `20.1µs`、`20.5µs`。 |

### G0 与 A7 结论

- **G0 通过**：必须通过的 V1、V4、V6 全部通过。
- **A7 的 25s/75s 取值不需调整**：无应用帧空闲连接至少存活 100s，实际断开约 123.9s；75s timeout 小于该实测下界。25s 应用数据帧在 15 分钟内持续保活成功（900.361s、37 pong、0 丢失）。
- V3 的 150/300/600s 空闲断开不阻止 G0；报告保留为观察结果。R13 缓冲也不是 G0 阻塞项，但前端应按计划保留一次性渲染 fallback。

## 验证输出

### Go 单测、race、vet

命令与输出：

```text
$ timeout 180s bash -o pipefail -c 'cd /tmp/homer-cli-p0/tests/spike && go test -race -count=1 ./... 2>&1 | tee /tmp/spike-final-race.log'
?   	github.com/zzjcool/homer-cli/tests/spike/wsecho	[no test files]
?   	github.com/zzjcool/homer-cli/tests/spike/wsprobe	[no test files]
ok  	github.com/zzjcool/homer-cli/tests/spike/internal/spike	6.462s

$ timeout 60s bash -o pipefail -c 'cd /tmp/homer-cli-p0/tests/spike && go test -count=1 ./... 2>&1 | tee /tmp/spike-final-test.log'
?   	github.com/zzjcool/homer-cli/tests/spike/wsecho	[no test files]
?   	github.com/zzjcool/homer-cli/tests/spike/wsprobe	[no test files]
ok  	github.com/zzjcool/homer-cli/tests/spike/internal/spike	4.607s

$ timeout 120s bash -c 'cd /tmp/homer-cli-p0/tests/spike && go vet ./... > /tmp/spike-final-vet.log 2>&1; status=$?; cat /tmp/spike-final-vet.log; echo "go vet exit=$status"; exit "$status"'

go vet exit=0

$ timeout 120s sh -c 'cd /tmp/homer-cli-p0/tests/spike && go mod tidy && git -C /tmp/homer-cli-p0 status --short'
(no stdout/stderr; exit 0; go.mod/go.sum 无变化)
```

### 嵌套 module 的 `go run` smoke

已从 `tests/spike` 目录运行 `go run ./wsprobe`（V1/V6），验证上一轮修正后的嵌套 module 命令路径：

```text
$ timeout 45s bash -o pipefail -c 'cd /tmp/homer-cli-p0/tests/spike && go run ./wsprobe -url ws://127.0.0.1:17801/ws -suite V1,V6 -out /tmp/spike-go-run-smoke.json 2>&1 | tee /tmp/spike-go-run-smoke.log'
2026/10/08 23:39:33 wsprobe starting suite=V1,V6 url=ws://127.0.0.1:17801/ws
{
  "suite": "V1,V6",
  "url": "ws://127.0.0.1:17801/ws",
  "started_at": "2026-10-08T15:39:33.917362406Z",
  "finished_at": "2026-10-08T15:39:33.920792256Z",
  "status": "pass",
  "results": [
    {
      "id": "V1",
      "name": "Authorization and WebSocket subprotocol pass-through",
      "status": "pass",
      "pass": true,
      "duration_ms": 1,
      "metrics": {
        "authorization_echoed": true,
        "authorization_header_count": 1,
        "client_subprotocol": "homer.stream.v1",
        "http_status": 101,
        "offered_protocols": [
          "homer.stream.v1"
        ],
        "protocol_header_seen": true,
        "selected_subprotocol": "homer.stream.v1"
      }
    },
    {
      "id": "V6",
      "name": "custom close code and reason pass-through",
      "status": "pass",
      "pass": true,
      "duration_ms": 2,
      "metrics": {
        "checks": [
          {
            "close_error": true,
            "expected_code": 4001,
            "expected_reason": "spike-close-4001",
            "received_code": 4001,
            "received_reason": "spike-close-4001"
          },
          {
            "close_error": true,
            "expected_code": 4401,
            "expected_reason": "spike-close-4401",
            "received_code": 4401,
            "received_reason": "spike-close-4401"
          }
        ]
      }
    }
  ]
}
```

`wsecho` 也从嵌套 module 用 `go run ./wsecho -addr 127.0.0.1:17805` 启动；`/healthz` 返回 200，随后同一 go-run smoke 的 V1/V6 均通过，服务由 `timeout` 收尾。

### TryCloudflare 与 `wsprobe -suite all`

长项在后台进程运行，日志 `/tmp/spike-wsprobe-all.log`，JSON `/tmp/spike.json`。V3/V4 并行；完整测试进程命令（全套含 15 分钟 V4、V8、V9、V7，故给足 2400s；不是 900s）为：

```text
$ timeout 2400s /tmp/spike-wsprobe -url wss://ban-retreat-group-computers.trycloudflare.com/ws -write-timeout-url ws://127.0.0.1:17804/ws -proxy-control-url http://127.0.0.1:17803 -source-pid 1590591 -cloudflared-pid 1591714 -cloudflared-bin /home/zzjcool/.local/bin/cloudflared -cloudflared-origin http://127.0.0.1:17801 -cloudflared-log /tmp/spike-cloudflared.log -suite all -out /tmp/spike.json
```

cloudflared 原始日志关键行：

```text
2026-10-08T15:34:13Z INF |  Your quick Tunnel has been created! Visit it at (it may take some time to be reachable):  |
2026-10-08T15:34:13Z INF |  https://ban-retreat-group-computers.trycloudflare.com                                     |
2026-10-08T15:34:14Z INF Registered tunnel connection ... location=hkg13 protocol=http2
2026-10-08T15:53:28Z INF |  https://manufacturer-abraham-seafood-night.trycloudflare.com                              |
2026-10-08T15:53:29Z INF Registered tunnel connection ... location=hkg13 protocol=http2
```

以下为 `/tmp/spike.json` 原始结果（`status=fail` 是 V3、R13 的实测发现）：

```json
{
  "suite": "all",
  "url": "wss://ban-retreat-group-computers.trycloudflare.com/ws",
  "started_at": "2026-10-08T15:36:28.028509989Z",
  "finished_at": "2026-10-08T15:54:32.161250051Z",
  "status": "fail",
  "results": [
    {
      "id": "V1",
      "name": "Authorization and WebSocket subprotocol pass-through",
      "status": "pass",
      "pass": true,
      "duration_ms": 223,
      "metrics": {
        "authorization_echoed": true,
        "authorization_header_count": 1,
        "client_subprotocol": "homer.stream.v1",
        "http_status": 101,
        "offered_protocols": [
          "homer.stream.v1"
        ],
        "protocol_header_seen": true,
        "selected_subprotocol": "homer.stream.v1"
      }
    },
    {
      "id": "V2",
      "name": "text frame round-trip sizes",
      "status": "pass",
      "pass": true,
      "duration_ms": 37478,
      "metrics": {
        "checks": [
          {
            "echo_bytes": 1024,
            "lossless": true,
            "message_type": "MessageText",
            "round_trip_ms": 491.267268,
            "sha256": "dba4a6315b76548b7a4dd079ef6aa29a7b34fa8b92c11668473441715c5f0af5",
            "size_bytes": 1024
          },
          {
            "echo_bytes": 1048576,
            "lossless": true,
            "message_type": "MessageText",
            "round_trip_ms": 3185.378405,
            "sha256": "8816f31ba2861e2a7ad907085905efdea5b458d26ed6fe4929ae21467ba1fa97",
            "size_bytes": 1048576
          },
          {
            "echo_bytes": 4194304,
            "lossless": true,
            "message_type": "MessageText",
            "round_trip_ms": 11720.097865,
            "sha256": "f2bcbf4281cc30e36ce6b7d49fabf4da570e03c0bb03ab5609f27a4955d9f248",
            "size_bytes": 4194304
          },
          {
            "echo_bytes": 8388608,
            "lossless": true,
            "message_type": "MessageText",
            "round_trip_ms": 21806.799653,
            "sha256": "50f0d8e7aa1c18e1a0783f97f94be8f20ab0b56cd000e090d4c3035182e9add0",
            "size_bytes": 8388608
          }
        ],
        "message_sizes_bytes": [
          1024,
          1048576,
          4194304,
          8388608
        ]
      }
    },
    {
      "id": "V3",
      "name": "idle connection survival",
      "status": "fail",
      "pass": false,
      "duration_ms": 125289,
      "metrics": {
        "checks": [
          {
            "close_code": 0,
            "close_reason": "",
            "observed_seconds": 60.001481327,
            "survived": true,
            "target_seconds": 60
          },
          {
            "close_code": 0,
            "close_reason": "",
            "observed_seconds": 100.02343123,
            "survived": true,
            "target_seconds": 100
          },
          {
            "close_code": -1,
            "close_reason": "",
            "observed_seconds": 124.905323692,
            "survived": false,
            "target_seconds": 300
          },
          {
            "close_code": -1,
            "close_reason": "",
            "observed_seconds": 123.906423694,
            "survived": false,
            "target_seconds": 600
          },
          {
            "close_code": -1,
            "close_reason": "",
            "observed_seconds": 124.996581892,
            "survived": false,
            "target_seconds": 150
          }
        ],
        "first_disconnect_observed_seconds": 123.906423694,
        "idle_limit_bracket_seconds": {
          "at_least": 100,
          "less_than_or_equal_to": 150
        },
        "max_survived_target_seconds": 100,
        "target_idle_seconds": [
          60,
          100,
          150,
          300,
          600
        ]
      },
      "error": "one or more idle connections closed before their target duration"
    },
    {
      "id": "V4",
      "name": "application ping keepalive",
      "status": "pass",
      "pass": true,
      "duration_ms": 901153,
      "metrics": {
        "actual_duration_seconds": 900.36139245,
        "application_pings": 37,
        "connection_survived": true,
        "max_pong_rtt_ms": 1979.00838,
        "missed_pongs": 0,
        "ping_interval_seconds": 25,
        "target_duration_seconds": 900
      }
    },
    {
      "id": "V5",
      "name": "WebSocket protocol ping/pong",
      "status": "pass",
      "pass": true,
      "duration_ms": 1032,
      "metrics": {
        "client_subprotocol": "homer.stream.v1",
        "pong_rtt_ms": 246.53632,
        "protocol_ping_forwarded": true
      }
    },
    {
      "id": "V6",
      "name": "custom close code and reason pass-through",
      "status": "pass",
      "pass": true,
      "duration_ms": 1301,
      "metrics": {
        "checks": [
          {
            "close_error": true,
            "expected_code": 4001,
            "expected_reason": "spike-close-4001",
            "received_code": 4001,
            "received_reason": "spike-close-4001"
          },
          {
            "close_error": true,
            "expected_code": 4401,
            "expected_reason": "spike-close-4401",
            "received_code": 4401,
            "received_reason": "spike-close-4401"
          }
        ]
      }
    },
    {
      "id": "V7",
      "name": "source process kill detection",
      "status": "pass",
      "pass": true,
      "duration_ms": 228,
      "metrics": {
        "client_detected_after_seconds": 0.041367029,
        "close_code": -1,
        "close_reason": "",
        "read_error": "failed to get reader: failed to read frame header: EOF",
        "source_cmdline": "/tmp/spike-wsecho -addr 127.0.0.1:17802 ",
        "source_killed": true,
        "source_pid": 1590591
      }
    },
    {
      "id": "V8",
      "name": "source-side blackhole detection",
      "status": "pass",
      "pass": true,
      "duration_ms": 75869,
      "metrics": {
        "application_pings_sent": 3,
        "configured_ping_timeout_seconds": 75,
        "detection": "application-ping-timeout",
        "detection_after_seconds": 75.01009863,
        "ping_interval_seconds": 25,
        "proxy_stats": {
          "active_connections": 1,
          "blackhole": true,
          "bytes_dropped": 132,
          "bytes_in": 81859599,
          "bytes_out": 81859467
        },
        "proxy_stats_error": ""
      }
    },
    {
      "id": "V9",
      "name": "temporary cloudflared restart and reconnect",
      "status": "pass",
      "pass": true,
      "duration_ms": 69402,
      "metrics": {
        "cloudflared_cmdline_before": "/home/zzjcool/.local/bin/cloudflared tunnel --protocol http2 --url http://127.0.0.1:17801 ",
        "cloudflared_pid_before_restart": 1591714,
        "new_connection_reconnected": true,
        "new_trycloudflare_url": "https://manufacturer-abraham-seafood-night.trycloudflare.com",
        "old_connection_disconnected": true,
        "old_disconnect_after_seconds": 0.042944852,
        "protocol": "http2",
        "reconnect_after_seconds": 68.373971231,
        "restarted_origin": "http://127.0.0.1:17801"
      }
    },
    {
      "id": "V10",
      "name": "50 concurrent request/response RTT",
      "status": "pass",
      "pass": true,
      "duration_ms": 1177,
      "metrics": {
        "requests": 50,
        "responses": 50,
        "rtt_max_ms": 390.801664,
        "rtt_min_ms": 243.141252,
        "rtt_p50_ms": 390.647311,
        "rtt_p95_ms": 390.790698,
        "single_connection": true,
        "unique_ids": 50
      }
    },
    {
      "id": "V11",
      "name": "WriteTimeout after hijack",
      "status": "pass",
      "pass": true,
      "duration_ms": 4002,
      "metrics": {
        "configured_server_write_timeout_seconds": 3,
        "connection_alive_after_wait": true,
        "echoed_bytes": 29,
        "round_trip_ms": 0.358643,
        "wait_after_hijack_seconds": 4
      }
    },
    {
      "id": "V12",
      "name": "middleware without http.Hijacker",
      "status": "pass",
      "pass": true,
      "duration_ms": 212,
      "metrics": {
        "dial_error": "failed to WebSocket dial: expected handshake response status code 101 but got 501",
        "http_status": 501,
        "middleware_test_route": "https://ban-retreat-group-computers.trycloudflare.com/no-hijacker/ws",
        "response_body": "Not Implemented",
        "upgrade_rejected": true,
        "writer_has_hijacker": false
      }
    },
    {
      "id": "R13",
      "name": "NDJSON line streaming through the tunnel",
      "status": "fail",
      "pass": false,
      "duration_ms": 5196,
      "metrics": {
        "content_length": -1,
        "content_type": "application/x-ndjson",
        "expected_first_to_last_seconds": 4,
        "first_line_arrival_seconds": 5.196333775,
        "first_line_to_last_line_seconds": 0.000016202999999492818,
        "http_status": 200,
        "last_line_arrival_seconds": 5.196349978,
        "line_arrival_seconds": [
          5.196333775,
          5.196339083,
          5.196342854,
          5.196346626,
          5.196349978
        ],
        "line_count": 5,
        "line_sequences": [
          1,
          2,
          3,
          4,
          5
        ],
        "response_protocol": "HTTP/2.0",
        "streamed_progressively": false,
        "transfer_encoding": null
      },
      "error": "NDJSON lines appear buffered: first-to-last 0.000s, expected at least 2.000s"
    }
  ]
}
```

### npm 脚本检查

仓库是 Go 项目且没有根 `package.json`，因此 `npm run typecheck` / `npm test` 不适用；未为此新增任何根文件。两条命令均因同一 ENOENT 退出：

```text
$ timeout 30s sh -c 'cd /tmp/homer-cli-p0 && npm run typecheck'
npm error code ENOENT
npm error syscall open
npm error path /tmp/homer-cli-p0/package.json
npm error errno -2
npm error enoent Could not read package.json: Error: ENOENT: no such file or directory, open '/tmp/homer-cli-p0/package.json'
npm error enoent This is related to npm not being able to find a file.
npm error enoent
npm error A complete log of this run can be found in: /home/zzjcool/.npm/_logs/2026-10-08T15_37_05_192Z-debug-0.log

$ timeout 30s sh -c 'cd /tmp/homer-cli-p0 && npm test'
npm error code ENOENT
npm error syscall open
npm error path /tmp/homer-cli-p0/package.json
npm error errno -2
npm error enoent Could not read package.json: Error: ENOENT: no such file or directory, open '/tmp/homer-cli-p0/package.json'
npm error enoent This is related to npm not being able to find a file.
npm error enoent
npm error A complete log of this run can be found in: /home/zzjcool/.npm/_logs/2026-10-08T15_37_10_104Z-debug-0.log
```

## MR 链接

https://github.com/zzjcool/homer-cli/pull/6

## 未决问题

1. **R13 缓冲确认**：经 TryCloudflare 的 NDJSON 行没有增量到达，尽管源站已 flush；需在前端保留/实施计划中的一次性渲染 fallback。此发现不阻止 G0。
2. **V12 状态码差异**：coder/websocket 在不含 `http.Hijacker` 的 ResponseWriter 上返回 501；P2 的 hub 外层应明确转换为计划要求的 HTTP 500 并打日志。
3. **V3 空闲窗口**：实测 100s 存活、约124s EOF、150s 未存活；因此不要把“完全空闲可活 150s”当作保证，但应用层 25s/75s 参数由 V4 实测支持。
4. 未触碰任何生产 cloudflared/homer-serve 配置或服务；本轮所有 spike 服务、wsprobe、临时 cloudflared 均已停止。`pgrep -af cloudflared` 只剩原有的 PID 1551475、监听 8787/8765/18432 的其他实例；均未动。
