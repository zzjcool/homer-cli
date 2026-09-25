package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionAndVersionFlagPrintDevelopmentVersion(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "dev"
	for _, args := range [][]string{{"homer", "version"}, {"homer", "--version"}, {"version"}, {"--version"}} {
		var out, errOut bytes.Buffer
		if code := runWithIO(args, &out, &errOut); code != 0 {
			t.Fatalf("version args %v exit = %d, stderr=%q", args, code, errOut.String())
		}
		if got := strings.TrimSpace(out.String()); got != "homer version: dev" {
			t.Fatalf("version args %v output = %q", args, got)
		}
		if errOut.Len() != 0 {
			t.Fatalf("version args %v stderr = %q", args, errOut.String())
		}
	}
}

func TestSetVersionFeedsVersionCommand(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	t.Setenv("HOMER_NO_VERSION_CHECK", "1")
	SetVersion("v1.1.0-test")
	var out, errOut bytes.Buffer
	if code := runWithIO([]string{"homer", "version"}, &out, &errOut); code != 0 {
		t.Fatalf("version exit = %d, stderr=%q", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != "homer version: v1.1.0-test" {
		t.Fatalf("injected version output = %q", got)
	}
}
