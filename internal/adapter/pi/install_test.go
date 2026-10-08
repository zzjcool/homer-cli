package pi

import (
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/adapter"
)

func TestPackageCommandsHaveOfficialInstaller(t *testing.T) {
	packages := DefaultPIAdapter.Categories["packages"]
	for _, command := range []string{packages.ListCmd, packages.ApplyCmd} {
		fields := strings.Fields(command)
		if len(fields) == 0 {
			t.Fatalf("empty command in packages category")
		}
		install, ok := adapter.OfficialInstall(fields[0])
		if !ok || !strings.Contains(install, "https://pi.dev/install") {
			t.Fatalf("command %q install = %q ok=%v", command, install, ok)
		}
	}
}
