package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/manifest"
	"github.com/zzjcool/homer-cli/internal/pluginregistry"
	"github.com/zzjcool/homer-cli/internal/shellenv"
	syncx "github.com/zzjcool/homer-cli/internal/sync"
)

func pullLook(deps *PullDeps) func(string) (string, error) {
	if deps != nil && deps.Look != nil {
		return deps.Look
	}
	return nil
}

func homeLook(deps *HomeDeps) func(string) (string, error) {
	if deps != nil && deps.Look != nil {
		return deps.Look
	}
	return nil
}

func resolveLook(look func(string) (string, error)) func(string) (string, error) {
	if look != nil {
		return look
	}
	return shellenv.Look
}

func applyAdapterIDs(plan syncx.PullPlan, tasks []manifest.Task) []string {
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, action := range plan.Actions {
		add(action.AdapterID)
	}
	for _, task := range tasks {
		add(task.AdapterID)
	}
	sort.Strings(ids)
	return ids
}

// missingToolErrors refuses a dispatch when an adapter about to be applied
// runs a CLI that is not installed. The message carries the official
// copy-paste installer so the user can install and retry.
func missingToolErrors(config core.HomerConfig, ids []string, look func(string) (string, error)) []string {
	look = resolveLook(look)
	errors := make([]string, 0)
	for _, id := range ids {
		cfg, ok := config.Adapters[id]
		if !ok || (cfg.Enabled != nil && !*cfg.Enabled) {
			continue
		}
		for _, binary := range missingBinaries(cfg, look) {
			install, _ := pluginregistry.OfficialInstall(binary)
			errors = append(errors, missingToolMessage(id, binary, install))
		}
	}
	return errors
}

func missingBinaries(cfg core.AdapterConfig, look func(string) (string, error)) []string {
	seen := make(map[string]struct{})
	missing := make([]string, 0)
	names := make([]string, 0, len(cfg.Categories))
	for name := range cfg.Categories {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		category := cfg.Categories[name]
		if category.Enabled != nil && !*category.Enabled {
			continue
		}
		for _, command := range []string{category.ListCmd, category.ApplyCmd} {
			binary := commandBinary(command)
			if binary == "" {
				continue
			}
			if _, ok := pluginregistry.OfficialInstall(binary); !ok {
				continue
			}
			if _, dup := seen[binary]; dup {
				continue
			}
			seen[binary] = struct{}{}
			if toolInstalled(command, look) {
				continue
			}
			missing = append(missing, binary)
		}
	}
	sort.Strings(missing)
	return missing
}

func commandBinary(command string) string {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 0 {
		return ""
	}
	return filepath.Base(fields[0])
}

func toolInstalled(command string, look func(string) (string, error)) bool {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 0 {
		return true
	}
	bin := fields[0]
	if strings.ContainsAny(bin, `/\`) {
		info, err := os.Stat(bin)
		return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
	}
	_, err := look(bin)
	return err == nil
}

func missingToolMessage(adapterID, binary, install string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "未安装 %s，无法下发适配器 %s。", binary, adapterID)
	if install != "" {
		b.WriteString("\n复制下面这一行安装后再下发：\n")
		b.WriteString(install)
	}
	return b.String()
}
