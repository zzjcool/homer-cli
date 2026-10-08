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
| P3 agent | worker-8 | ws/p3-agent | ✅ 已合并 9de73f8(tag ws/p3-merged) |
| P4c web+UI | worker-9 | ws/p4c-web | ✅ 已合并 37a1a23(tag ws/p4c-merged);复验 web race/闸口矩阵/ETag/ChoicesStream/fanout 通过 |
| P4b agent 数据面 | worker-10 | ws/p4b-data | ✅ 已合并(tag ws/p4b-merged) |
| I1 集成 | orchestrator | master | ✅ f18141c(tag ws/i1-integrated):CLI 改 --hub/--data-url、serve 装配 AgentHub、删 listen/connect;go build ./... 通过,internal/... -race 全绿 |
| P5 docker/脚本/文档 | worker-12 | ws/p5-docs | ✅ 已合并 95a3fb0(tag ws/p5-merged);README runbook 已对齐 hw 真实 unit(`--connect <url>` 空格写法) |
| P6a 集成测试 | worker-11 | ws/p6a-integration | 进行中(tests/integration+tests/bench) |
| P6b e2e 迁移 | worker-13 | ws/p6b-e2e | ✅ 已合并(tag ws/p6b-merged);orchestrator 独立复验:SOP 三守门员通过,全量 tests/e2e 77s 通过,tagged vet 编译通过 |
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
- P3 合并 9de73f8;I1:我自己改了 internal/cli(args/run/hub/upgrade + 测试)和 hubtoken 注释,机械性改动,冲突风险小所以没再派 worker
- **首次端到端冒烟(本机回环,真 hub+真 agent)**:choices 0.70~0.75s(旧 1.3~1.6s);4 并发任务全部 ~0.73s 同时完成(旧严格串行 0.13/3.5/5.8s)。不含 CF tunnel/快照缓存/precheck 合并,不能当 P7 验收数字
- 待办:tests/e2e 编译失败(dispatch_unlock_ui_test.go/tool_upgrade_ui_test.go 引用 web.AgentInfo.Mode,P6 修)
- 教训:不要用 pkill -f 带路径模式(会匹配到自己的 bash -c 命令行),用 PID 精确 kill
- 派出 P4b(10) P6a(11) P5(12) P6b(13) 四个 worker,文件范围互不重叠(agentd / tests/integration+bench / e2e+docs / tests/e2e)
- P5 合并。残留:e2e/console/docker-compose.yml 保留休眠容器名 box-listen(只 sleep,无 agent 进程),引用它的 e2e 由 P6b 处理
- P6b 合并。独立复验通过;抽查 TestRevokeKicksLiveAgent 有真断言(agent 日志吊销、hub 踢线日志、旧 secret 重连 401 可读提示、status 返回 503)
- P4b 合并。复验发现 TestAgentHubSupersedesPreviousSession 在全量并行下偶发失败:测试等待「任意会话」而非「会话被替换」,是测试竞态非产品缺陷;已修(等待 registry 会话换成非 firstSession),-race -count=200 通过
- **第二次端到端冒烟(本机回环,P4b 合并后,真 hub+真 agent)**:choices 整包 0.69~0.92s(基线 1.3~1.6s,撞车 3.3~4.75s);4 并发全部 0.73s(基线严格串行 0.13/3.5/5.8s);12 并发按 readSem=4 分三批 0.70/1.41/2.17s(设计如此);stream=1 首字节 0.69s;agent 离线后调用 1ms 返回 503(基线等 60s)
- 注意:单次 choices ~0.7s 由本机全量扫描决定(homer status 本机 0.78s),这是扫描成本,传输层已不是瓶颈;P7 在 hw 经 tunnel 的真实数字待测
