# WS 重构进度(orchestrator 维护)

计划:docs/plan/2026-10-08-ws-stream-plan.md  基线 tag:pre-ws-master (dd29054)
计划提交:4b1a9da

## 已拍板
- 用户确认:开发阶段,完全推翻重做;支持 WS;底层抽象 Stream 并发处理
- 计划 F 节 4 条开放问题均采用默认(不做兼容迁移/不做自动探测/不做 524 立项/不收紧闸口),待用户另行指示

## 阶段状态
| 阶段 | worker | 分支 | 状态 |
|---|---|---|---|
| P0 CF WS spike | worker-0 | ws/p0-spike | 进行中(G0 闸口) |
| P4a shellenv 缓存 | worker-1 | ws/p4a-shellenv | 进行中 |
| P1 stream 库 | worker-2 | ws/p1-stream | 进行中(F1 检查点后开 P2/P3/P4c) |
| P2 hub | - | ws/p2-hub | 待 F1 |
| P3 agent | - | ws/p3-agent | 待 F1 |
| P4c web+UI | - | ws/p4c-web | 待 F1 |
| P4b agent 数据面 | - | ws/p4b-data | 待 P3 |
| I1 集成 | orchestrator | ws/integration | 待 |
| P5 CLI/docker/文档 | - | ws/p5-cli | 待 I1 |
| P6a/b 测试 | - | ws/p6-tests | 待 |
| P7 hw 性能验收 | orchestrator+用户 | - | 待 |

## 合并顺序
P1 → P4a → P2 → P3 → P4c → P4b → P5 → P6;每合一个跑 gofmt/build/vet

## 基线(旧)
弹窗链 3.6~11.8s;choices 空闲 1.3~1.6s;precheck 4.8~6.9s;3 并发任务严格串行。目标 P95<2s。

## 日志
- 2026-10-08 计划落盘并提交,tag pre-ws-master,派出 P0/P4a/P1 三个 worker
