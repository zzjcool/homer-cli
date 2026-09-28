package agentd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestLocalExecutorConfirmDoesNotInit pins the executor contract: confirm
// means "apply", never "initialize". On an uninitialized home Push/Pull
// must fail cleanly and must NOT create homer.json as a side effect.
func TestLocalExecutorConfirmDoesNotInit(t *testing.T) {
	home := t.TempDir()
	executor := NewLocalExecutor(home)
	pushReport, err := executor.Push(context.Background(), true)
	if err == nil && pushReport.OK {
		t.Fatal("push on uninitialized home must not succeed")
	}
	pullReport, err := executor.Pull(context.Background(), true)
	if err == nil && pullReport.OK {
		t.Fatal("pull on uninitialized home must not succeed")
	}
	if _, statErr := os.Stat(filepath.Join(home, "homer.json")); statErr == nil {
		t.Fatal("executor must not create homer.json as a side effect")
	}
}
