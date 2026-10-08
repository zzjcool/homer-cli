# WS 重构进度(orchestrator 维护)

计划:docs/plan/2026-10-08-ws-stream-plan.md  基线 tag:pre-ws-master (dd29054)
计划提交:4b1a9da

## 已拍板
- 用户确认:开发阶段,完全推翻重做;支持 WS;底层抽象 Stream 并发处理
- 计划 F 节 4 条开放问题均采用默认(不做兼容迁移/不做自动探测/不做 524 立项/不收紧闸口),待用户另行指示

## 阶段状态
| 阶段 | worker | 分支 | 状态 |
|---|---|---|---|
| P0 CF WS spike | worker-5(worker-0 因计划验收命令错误停止) | 主 checkout tests/spike | ✅ 已合并 c52f6f1(tag ws/g0-passed):G0 通过 |
| P4a shellenv 缓存 | worker-3(worker-1 因旧测试冲突停止) | ws/p4a-shellenv | ✅ 已合并 2ef7ba9(tag ws/p4a-merged),build/vet/race 通过 |
| P1 stream 库 | worker-6(worker-2/4 因计划错误停止) | ws/p1-stream-v3 | ✅ 完整实现已合并 0a07931(tag ws/p1-merged);orchestrator 独立复验 build/vet/-race/count=30 全过 |
| P2 hub | worker-7 | ws/p2-hub | ✅ 已合并 b0fb343(tag ws/p2-merged);hub+web+stream 合并后 build/vet/race 全过 |
| P3 agent | worker-8 | ws/p3-agent | 进行中 |
| P4c web+UI | worker-9 | ws/p4c-web | ✅ 已合并 37a1a23(tag ws/p4c-merged);复验 web race/闸口矩阵/ETag/ChoicesStream/fanout 通过 |
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
- P0 完成,G0 通过。V3:CF 空闲约124s断;R13:经CF的NDJSON被缓冲(推翻增量渲染假设),已写入计划;性能验收只看整包 P95<2s
- 已关闭 worker 误开的 PR #4 #5 #6
- P1 合并 0a07931(tag ws/p1-merged)。TestHalfOpenDetectedByPing 属 P6a,不在 P1。P2/P3/P4c 的分支基于 F1 骨架(73769a1),合并时会有 stream 库后续补齐的差异,I1 统一处理
- P4c 合并 37a1a23。master 全仓暂不能整体编译(预期:web 删 AgentInfo.Mode 等,hub/agentd/cli/tests/e2e 待 P2/P3/I1/P5/P6 修)。tests/e2e 的 dispatch_unlock_ui_test.go、tool_upgrade_ui_test.go 仍引用 web.AgentInfo.Mode,归 P6
- P2 合并 b0fb343。复验:hub race count=15 稳定;握手/顶替4001/吊销4401/410墓碑/requireOnline 统一都在代码里。测试名与计划略有出入(TestAgentHubSupersedesPreviousSession 等),覆盖的场景齐
