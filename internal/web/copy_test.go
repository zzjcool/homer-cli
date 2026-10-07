package web

import (
	"strings"
	"testing"
)

// TestConsoleUserCopy freezes the user-facing console vocabulary: git
// mechanics never surface on the hub page. The console speaks about
// machines, changes, and the center — not push/pull/merge/commit.
func TestConsoleUserCopy(t *testing.T) {
	html := string(staticIndex)
	for _, banned := range []string{
		"homer merge", "homer push", "homer pull", "homer init",
		"可推送", "可拉取", "确认推送", "确认拉取",
		"allow-secrets", "commit", "bare", "仓库",
		"/api/push", "/api/pull", "查看状态", "查看差异", "处理漂移", "推送", "拉取",
		"同步到其他机器", "从中心同步", "以本机为准", "这台机器的改动",
		"处理改动",
	} {
		if strings.Contains(html, banned) {
			t.Fatalf("console copy contains git word %q — the console must speak in sync vocabulary", banned)
		}
	}
	for _, required := range []string{
		"项未收取", "项待下发", "项冲突",
		"中心还没有任何内容",
		"有冲突，需要选择保留哪一边",
		"有尚未同步的改动",
		"全部对齐",
		"收取", "下发", "确认收取", "确认下发", "/api/sync?",
		"/api/sync/choices", "勾选要动的适配器", "body: { adapters: adapters }",
		"全选", "清除",
		"以这台机器为准", "以中心为准", "/api/resolve?",
		"需要现在选择保留哪一边",
		"没有冲突的内容已经写到",
		"配置目录，扫描时读不到",
		"/api/console",
		// 配置查看（存储视角 / 机器视角）
		"查看存储内容", "服务器存储 · 当前内容", "/api/storage",
		"正在向这台机器实时查询", "未收取", "待下发",
		// 机器卡片上的资源快照（agent 心跳上报）
		"内存", "网卡", "负载", "磁盘", "已运行", "等待机器上报系统信息",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("console copy missing %q", required)
		}
	}
}
