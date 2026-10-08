# 「收取 hw」慢在哪 —— 实测摘要(scout-0)

环境: hostname=hw; hub 与 agent 同机; agent=hw-7735 为 connect 模式, connectUrl=https://homerhw.openaaas.org (经 CF tunnel 绕回本机 :7760)

## 实测
| 项 | 值 |
|---|---|
| 快照下载 回环 | 6~8ms (447,810B) |
| 快照下载 经 tunnel | 0.29~0.95s, 冷例 11.99s |
| choices 空闲 | 1.32~1.60s (53/58 次) |
| choices 撞 drift 窗口 | 3.3~4.75s (双峰) |
| keys list(agent 空闲) | 0.12s |
| keys list(排队在别的任务后) | 2.2~3.5s |
| precheck | 4.8~6.9s (稳定) |
| 本机 homer status | 0.97~0.99s, 读 21.6MB |
| 3 个并发最小任务 | 完成于 0.13 / 3.53 / 5.77s (严格串行) |
| 背靠背 keys | 0.12 -> 2.23 -> 2.23s |
| 整条弹窗链(choices+keys+precheck) | 3.6~11.8s |
| agent 周期性大读 | 每 ~55s 一次, ~21MB, 1~3s |
| 登录 shell PATH 探测 | 83~104ms/次(每个 listCmd 一次) |

## 假设裁决
1. 前端并发: 部分证实。choices+keys 并发; precheck 在 choices 返回后才发,且 hub 侧再并发 2 个机器任务
2. connect 同步执行 + 固定 sleep 2s: 证实
3. poll 前同步算 driftSummary: 证实(分布吻合,未逐条抓重叠)
4. listen 直连: 代码证实,hw 不适用
5. statusReport 流程: 证实
6. precheck = 2 个排队任务,到 agent 串在同一队列: 证实
7. execute 期间不 poll: 证实

## 瓶颈
1. connect 单槽串行(poll->execute->report->sleep 2s)。一次弹窗 4 个任务串行
2. 每个 status 任务: 全机扫描 ~1s + 447KB 快照经 tunnel 下载
3. hub 与 agent 同机仍绕 CF tunnel: 快照 6ms -> 0.3~0.95s(40~120x)

## 没测到
- hub 无 access log, 入队/取走/report 三段未打点
- choices 尾部只观察到与 drift 窗口重合, 未抓逐请求 trace
- 冷 tunnel 11.99s 仅 1 例
- 真实浏览器网络未测
- 21.5MB 读突发未逐文件归因
