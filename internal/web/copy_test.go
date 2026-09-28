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
	} {
		if strings.Contains(html, banned) {
			t.Fatalf("console copy contains git word %q — the console must speak in sync vocabulary", banned)
		}
	}
	for _, required := range []string{
		"项本机改动", "项中心改动", "项冲突",
		"还没选择要同步的目录",
		"有冲突，需要选择保留哪一边",
		"有尚未同步的改动",
		"都已对齐",
		"同步到其他机器", "从中心同步", "确认同步", "/api/sync?",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("console copy missing %q", required)
		}
	}
}
