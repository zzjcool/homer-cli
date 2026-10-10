package pi_test

import (
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/adapter/pi"
	"github.com/zzjcool/homer-cli/internal/pluginregistry"
)

func TestPackageCommandsHaveOfficialInstaller(t *testing.T) {
	packages := pi.DefaultPIAdapter.Categories["packages"]
	for _, command := range []string{packages.ListCmd, packages.ApplyCmd} {
		fields := strings.Fields(command)
		if len(fields) == 0 {
			t.Fatalf("empty command in packages category")
		}
		install, ok := pluginregistry.OfficialInstall(fields[0])
		if !ok || !strings.Contains(install, "https://pi.dev/install") {
			t.Fatalf("command %q install = %q ok=%v", command, install, ok)
		}
	}
}
