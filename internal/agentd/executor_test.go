package agentd

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A fresh machine (no homer.json yet) must answer status with a LEGAL
// report: the built-in adapters fall back to defaults and scan what is
// actually installed (the user story: "为什么不是直接扫描他的 pi 当前
// 安装的插件"), plus an Errors marker line for the console badge. The
// 502 the user saw came from treating this as an executor error.
func TestStatusOnFreshMachineIsLegalEmpty(t *testing.T) {
	home := t.TempDir() // no homer.json anywhere
	executor := NewLocalExecutorWithHub(home, "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	report, err := executor.Status(ctx)
	if err != nil {
		t.Fatalf("fresh machine status must not error: %v", err)
	}
	// Errors must carry the fresh-machine marker (drift badge keys on
	// it) — even though the scan itself succeeded via defaults.
	marked := false
	for _, message := range report.Errors {
		if strings.Contains(message, "未找到 homer 配置") {
			marked = true
		}
	}
	if !marked {
		t.Fatalf("fresh machine report must carry the marker in Errors: %+v", report.Errors)
	}
}
