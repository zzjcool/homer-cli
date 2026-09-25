package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

// Keep the process-boundary fixture in this wave even though the complete
// serve handshake is owned by the P1 command orchestrator. It is used by the
// adapter unit tests through the same executable shape: an address file is
// written immediately, then the process remains attached to stdin/stdout.
func TestV13PairFakeTailcatFixtureIsExecutable(t *testing.T) {
	dir := t.TempDir()
	tailcat := filepath.Join(dir, "tailcat")
	if err := os.WriteFile(tailcat, []byte("#!/bin/sh\nprintf 'fixture-address\\n' > \"$TAILCAT_ADDR_FILE\"\ncat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(tailcat); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("fake tailcat fixture = %v, err=%v", info, err)
	}
}
