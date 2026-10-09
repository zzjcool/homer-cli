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
		"join-listen", "btn-copy-listen", "listenCommand", "loadCollectPrecheck", "precheckToken", "/api/sync/precheck",
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
		"全选", "反选", "全不选",
		"密钥跟着这次一起",
		"口令只在这一步使用",
		"下一步", "上一步", "先解开这些密钥，然后才会下发。",
		"机器用这个接入码连上之后，窗口会自己关上。",
		"密钥还没收到中心，这个适配器不能下发：",
		"绑定了密钥，但密钥这次带不上，所以不能下发。",
		"插件没有安装成功",
		"更新程序", "全部更新", "a.outdated",
		"但有插件没有装上",
		"正在下发并解开…",
		"stream=1", "splitNDJSONLines", "AbortController", "scopeStreamFailed", "已读取 ",
		"读取收取选项失败（尚未完成）", "没有收到完整的机器检查结果，不能继续收取。",
		"缺的会逐个下载安装，请留在这个窗口，装完才会结束。",
		"item.id === \"keyring\" && app.scopeDirection !== \"resolve\"",
		"以这台机器为准", "以中心为准", "/api/resolve?",
		"需要现在选择保留哪一边",
		"没有冲突的内容已经写到",
		"配置目录，扫描时读不到",
		"/api/console",
		// 配置查看（存储视角 / 机器视角）
		"查看存储内容", "服务器存储 · 当前内容", "/api/storage",
		"默认加密", "待加密", "转为加密",
		"正在向这台机器实时查询", "未收取", "待下发",
		// 机器卡片上的资源快照（agent 心跳上报）
		"内存", "网卡", "负载", "磁盘", "已运行", "等待机器上报系统信息",
		"上次心跳", "版本未上报", "还没有心跳",
		// 机器上的应用版本（pi / herdr / opencode…）与一键升级
		"应用版本", "全部升级应用", "/tool-upgrade", "a.tools",
		"正在升级", "有应用没有升级成功", "复制安装命令", "已是已知最新版本",
		"要用这台机器的包管理器更新",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("console copy missing %q", required)
		}
	}
}
