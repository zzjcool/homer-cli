package web

import (
	"net/http"
	"testing"
)

func TestAgentOutdatedErrorMessageAndStatus(t *testing.T) {
	if got := errorMessage("agent-outdated"); got != "这台机器的 homer 版本太旧，不能记录决定，请先更新程序" {
		t.Fatalf("agent-outdated message = %q", got)
	}
	if got := agentStatusForCode("agent-outdated"); got != http.StatusConflict {
		t.Fatalf("agent-outdated status = %d, want %d", got, http.StatusConflict)
	}
}
