package agentd

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A fresh machine (no homer.json yet) must answer status with a LEGAL
// empty report — "new machine awaiting dispatch" is a machine state,
// not a task failure. The 502 the user saw ("查询失败：请求失败（502）")
// came from treating this as an executor error.
func TestStatusOnFreshMachineIsLegalEmpty(t *testing.T) {
	home := t.TempDir() // no homer.json anywhere
	executor := NewLocalExecutorWithHub(home, "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	report, err := executor.Status(ctx)
	if err != nil {
		t.Fatalf("fresh machine status must not error: %v", err)
	}
	if len(report.Adapters) != 0 {
		t.Fatalf("fresh machine must report zero adapters, got %+v", report.Adapters)
	}
	if len(report.Errors) == 0 || !strings.Contains(report.Errors[0], "未找到 homer 配置") {
		t.Fatalf("fresh machine must explain itself in report.Errors: %+v", report.Errors)
	}
}
