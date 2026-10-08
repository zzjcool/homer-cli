// Package toolctl reports the version of the programs the adapters drive (pi,
// herdr, opencode, VS Code), decides when a machine has fallen behind, and
// upgrades a program in place.
//
// Everything is keyed by the tool IDs registered in package adapter. Nothing
// here accepts a command line from a caller: a request names a tool, and the
// agent runs the argument list the registry holds for it. A hub, a browser or
// a stray HTTP client can therefore ask for "pi" to be upgraded, but never for
// an arbitrary command to be run.
package toolctl

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zzjcool/homer-cli/internal/adapter"
	"github.com/zzjcool/homer-cli/internal/shellenv"
	"github.com/zzjcool/homer-cli/internal/upgrade"
)

const (
	// ProbeTimeout bounds one version check. A cold start of a Node based CLI
	// can take several seconds; a hung one must not stall the agent.
	ProbeTimeout = 20 * time.Second
	// UpgradeTimeout bounds one upgrade command. Installers download and
	// unpack; a slow link needs minutes, a stuck one must be stopped.
	UpgradeTimeout = 5 * time.Minute
)

var (
	probeTimeout   = ProbeTimeout
	upgradeTimeout = UpgradeTimeout
)

// Status is what one machine reports about one program it has installed.
// A program that is not installed is simply not reported.
type Status struct {
	// ID is the adapter.Tool ID.
	ID      string `json:"id"`
	Adapter string `json:"adapter,omitempty"`
	Label   string `json:"label,omitempty"`
	// Version is the dotted version, for example 0.87.1. Empty when the
	// program is installed but did not print one; Error then says why.
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
	// Upgradable is true when this machine can upgrade the program itself.
	Upgradable bool `json:"upgradable,omitempty"`
}

// ProbeLocal checks every registered program against this machine's current
// login PATH. Tools installed after the agent started show up here.
func ProbeLocal(ctx context.Context) []Status {
	return ProbeAll(ctx, shellenv.Path())
}

// ProbeAll checks every registered program, concurrently, against path and
// returns the installed ones in registry order.
func ProbeAll(ctx context.Context, path string) []Status {
	tools := adapter.Tools()
	found := make([]*Status, len(tools))
	var wg sync.WaitGroup
	for i, tool := range tools {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if status, ok := Probe(ctx, tool, path); ok {
				found[i] = &status
			}
		}()
	}
	wg.Wait()
	statuses := make([]Status, 0, len(tools))
	for _, status := range found {
		if status != nil {
			statuses = append(statuses, *status)
		}
	}
	return statuses
}

// Probe asks one program for its version. ok is false when the program is not
// on path, which is the normal case on a machine that does not use it.
func Probe(ctx context.Context, tool adapter.Tool, path string) (Status, bool) {
	bin, err := shellenv.LookIn(tool.Binary, path)
	if err != nil {
		return Status{}, false
	}
	status := Status{ID: tool.ID, Adapter: tool.Adapter, Label: tool.Label, Upgradable: tool.CanUpgrade()}
	out, err := run(ctx, bin, tool.VersionArgs, path, probeTimeout, nil)
	if err != nil {
		status.Error = "读不出版本：" + failureReason(out, err)
		return status, true
	}
	version := ParseVersion(out.stdout)
	if version == "" {
		version = ParseVersion(out.stderr)
	}
	if version == "" {
		status.Error = "读不出版本：输出里没有版本号"
		return status, true
	}
	status.Version = version
	return status, true
}

// versionPattern finds a dotted version such as 0.87.1, 1.138.0 or
// 2.0.0-beta.1 anywhere in a program's output ("herdr 0.9.1", "v1.2.3").
var versionPattern = regexp.MustCompile(`\d+\.\d+(?:\.\d+)*(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?`)

// ParseVersion returns the first dotted version in text, or "".
func ParseVersion(text string) string {
	return versionPattern.FindString(text)
}

// Behind reports whether version is older than reference. An unknown version
// or reference is never "behind": a machine is only flagged on evidence.
func Behind(version, reference string) bool {
	if version == "" || reference == "" {
		return false
	}
	return upgrade.IsNewer(reference, version)
}

// Reference returns, for every tool, the version machines are measured
// against: the newest release any machine reports, or the tool's declared
// floor when that is newer. fleet holds one report per machine.
func Reference(fleet [][]Status) map[string]string {
	return reference(fleet, adapter.Tools())
}

func reference(fleet [][]Status, tools []adapter.Tool) map[string]string {
	newest := map[string]string{}
	consider := func(id, version string, allowPrerelease bool) {
		if version == "" || (!allowPrerelease && strings.Contains(version, "-")) {
			return
		}
		if current, ok := newest[id]; !ok || upgrade.IsNewer(version, current) {
			newest[id] = version
		}
	}
	// A machine on a beta must not make every stable machine look old, so a
	// prerelease never becomes the fleet's reference. A declared floor is the
	// maintainer's own statement and is taken as written.
	for _, report := range fleet {
		for _, status := range report {
			consider(status.ID, status.Version, false)
		}
	}
	for _, tool := range tools {
		consider(tool.ID, tool.MinVersion, true)
	}
	return newest
}

// UpgradeResult is the outcome of one upgrade, shown to the person who
// clicked the button. Every field is a plain sentence or a plain value.
type UpgradeResult struct {
	OK bool `json:"ok"`
	// Status is one of: upgraded, unchanged, failed, busy, unsupported,
	// not-installed, unknown-tool.
	Status string `json:"status"`
	Tool   string `json:"tool"`
	Label  string `json:"label,omitempty"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
	Note   string `json:"note,omitempty"`
	// Output is the end of what the upgrade command printed, cleaned of
	// colors and progress bars, so a failure can be read without a terminal.
	Output string `json:"output,omitempty"`
	// Manual is the official one-line installer the person can run on the
	// machine when the console cannot do it.
	Manual string `json:"manual,omitempty"`
}

// UpgradeLocal upgrades a registered program on this machine.
func UpgradeLocal(ctx context.Context, id string) UpgradeResult {
	return Upgrade(ctx, id, shellenv.Path())
}

// upgradeSlot admits one upgrade at a time. Package managers do not like
// being run twice in parallel, and a double click should not start a second.
var upgradeSlot = make(chan struct{}, 1)

// Upgrade runs the registered upgrade command for the tool with this ID.
func Upgrade(ctx context.Context, id, path string) UpgradeResult {
	defer shellenv.Invalidate()

	tool, ok := adapter.ToolByID(id)
	if !ok {
		return UpgradeResult{Status: "unknown-tool", Tool: clip(id, 40), Note: "不认识这个应用：" + clip(id, 40)}
	}
	return upgradeTool(ctx, tool, path)
}

func upgradeTool(ctx context.Context, tool adapter.Tool, path string) UpgradeResult {
	result := UpgradeResult{Tool: tool.ID, Label: tool.Label}
	before, installed := Probe(ctx, tool, path)
	if !installed {
		result.Status = "not-installed"
		result.Note = "这台机器上没有安装 " + tool.Label + "。"
		result.Manual = tool.Install
		return result
	}
	result.Before = before.Version
	if !tool.CanUpgrade() {
		result.Status = "unsupported"
		result.Note = tool.Label + " 要用这台机器的包管理器更新，这里只能看版本。"
		return result
	}
	select {
	case upgradeSlot <- struct{}{}:
		defer func() { <-upgradeSlot }()
	default:
		result.Status = "busy"
		result.Note = "这台机器正在升级另一个应用，等它结束再试。"
		return result
	}

	bin, err := shellenv.LookIn(tool.Binary, path)
	if err != nil {
		result.Status = "not-installed"
		result.Note = "这台机器上没有安装 " + tool.Label + "。"
		result.Manual = tool.Install
		return result
	}
	// CI keeps installers from asking questions nobody can answer.
	out, err := run(ctx, bin, tool.UpgradeArgs, path, upgradeTimeout, []string{"CI=1"})
	result.Output = tidy(out.stdout+out.stderr, 40, 4096)
	if err != nil {
		result.Status = "failed"
		result.Note = tool.Label + " 升级没有成功：" + failureReason(out, err)
		result.Manual = tool.Install
		return result
	}

	after, _ := Probe(ctx, tool, path)
	result.After = after.Version
	result.OK = true
	switch {
	case after.Version == "":
		result.Status = "upgraded"
		result.Note = tool.Label + " 的升级命令已经执行完，但读不出新版本。"
	case after.Version == before.Version:
		result.Status = "unchanged"
		result.Note = fmt.Sprintf("%s 版本没有变化（%s）。", tool.Label, after.Version)
		if last := lastLine(out.stdout + "\n" + out.stderr); last != "" {
			result.Note += "它回复：" + last
		}
	default:
		result.Status = "upgraded"
		result.Note = fmt.Sprintf("%s %s → %s", tool.Label, orUnknown(before.Version), after.Version)
	}
	return result
}

func orUnknown(version string) string {
	if version == "" {
		return "未知版本"
	}
	return version
}
