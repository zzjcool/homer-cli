# WS 重构进度(orchestrator 维护)

计划:docs/plan/2026-10-08-ws-stream-plan.md  基线 tag:pre-ws-master (dd29054)
计划提交:4b1a9da

## 已拍板
- 用户确认:开发阶段,完全推翻重做;支持 WS;底层抽象 Stream 并发处理
- 计划 F 节 4 条开放问题均采用默认(不做兼容迁移/不做自动探测/不做 524 立项/不收紧闸口),待用户另行指示

## 阶段状态
| 阶段 | worker | 分支 | 状态 |
|---|---|---|---|
| P0 CF WS spike | worker-5(worker-0 因计划验收命令错误停止) | 主 checkout tests/spike | 进行中(G0 闸口;注意 G0 未过前 P2/P3 已基于 F1 提前开工,风险见下) |
| P4a shellenv 缓存 | worker-3(worker-1 因旧测试冲突停止) | ws/p4a-shellenv | ✅ 已合并 2ef7ba9(tag ws/p4a-merged),build/vet/race 通过 |
| P1 stream 库 | worker-6(worker-2/4 因计划错误停止) | ws/p1-stream-v3 | F1 已合并 master(tag ws/f1-merged=73769a1);补齐阶段进行中 |
| P2 hub | worker-7 | ws/p2-hub | 进行中 |
| P3 agent | worker-8 | ws/p3-agent | 进行中 |
| P4c web+UI | worker-9 | ws/p4c-web | 进行中 |
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
- 接口变更单 #1:A6 InspectEvent 的 `Done, Total int` 共用非法 json tag 会丢字段,已拆成两字段(commit 08ea503)。worker-2 因此停止,改派 worker-4 基于 08ea503
- P4a:旧测试 TestCommandEnvReadsLoginPATHEveryCall 断言「每次都读」,与缓存语义冲突,批准改名改断言并让 toolctl.Upgrade 后 Invalidate;worker-1 的 resume 因 worktree 路径冲突失败,改派 worker-3
- P4a 合并 master 2ef7ba9。worker 习惯自行开 PR,已关闭 #4 #5,后续任务卡明确「不开 PR」
- P0 第一次因嵌套 go.mod 与根目录验收命令冲突停止,计划已修(d1e098a),改派 worker-5(worktree=false,只写 tests/spike 与 spike 报告)
- 接口变更单 #2:A6 InspectSecret 补 json tag(98cb6d3)
- P1 F1 检查点到达,orchestrator 独立验证 build/vet/race 全过后合并 master(78ca44f),tag ws/f1-merged
- 同时在跑 5 个 worker:P0(5) P1 补齐(6) P2(7) P3(8) P4c(9)。P2/P3 在 G0 之前开工是已知风险:若 G0 不过(CF 不透传 WS),要停下来改传输层,Session/协议层仍可复用
- 注意:P3 分支基于旧 hub 代码,P2 删除旧符号后需 I1 集成统一修复
