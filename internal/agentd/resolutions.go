package agentd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/resolutions"
)

// homerPathsForHome mirrors the executor's HOMER_HOME override while keeping
// staged-resolution IO independent of process-global environment state.
func homerPathsForHome(home string) core.HomerPaths {
	return core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return home
		}
		return os.Getenv(key)
	})
}

func (d *Daemon) resolutionPaths() core.HomerPaths {
	if d == nil {
		return homerPathsForHome("")
	}
	return homerPathsForHome(d.cfg.HomerHome)
}

// runResolveRecord records, clears, or lists the machine's staged conflict
// decisions. It deliberately uses only the configured Homer home, not the
// executor, so read-only test executors and fresh workspaces behave alike.
func (d *Daemon) runResolveRecord(ctx context.Context, options hub.TaskOptions) (resolutions.Report, error) {
	report := resolutions.Report{
		Action:  options.ResolutionAction,
		Entries: []resolutions.Entry{},
		Errors:  []string{},
	}
	if err := contextError(ctx); err != nil {
		return report, err
	}
	paths := d.resolutionPaths()

	switch options.ResolutionAction {
	case resolutions.ActionList:
		file, err := resolutions.Load(paths)
		if errors.Is(err, resolutions.ErrCorrupt) {
			file = resolutions.File{Version: resolutions.FileVersion, Entries: []resolutions.Entry{}}
			report.Warnings = append(report.Warnings, "已记录决定文件损坏，按空列表处理: "+err.Error())
		} else if err != nil {
			report.Errors = append(report.Errors, err.Error())
			return report, nil
		}
		report.OK = true
		report.Entries = append([]resolutions.Entry{}, file.Entries...)
	case resolutions.ActionRecord:
		result, err := resolutions.Record(paths, options.ResolutionChoice, options.Adapters, options.CenterGeneration, nil)
		if err != nil {
			report.Errors = append(report.Errors, err.Error())
			return report, nil
		}
		report.OK = true
		report.Recorded = append([]resolutions.Entry{}, result.Recorded...)
		report.Replaced = append([]resolutions.Entry{}, result.Replaced...)
		file, loadErr := resolutions.Load(paths)
		if loadErr != nil {
			// The record itself is already durable. Preserve its success and
			// provide the known entries if a follow-up read is unavailable.
			report.Warnings = append(report.Warnings, "记录已写入，但读取决定列表失败: "+loadErr.Error())
			report.Entries = append([]resolutions.Entry{}, result.Recorded...)
		} else {
			report.Entries = append([]resolutions.Entry{}, file.Entries...)
		}
	case resolutions.ActionClear:
		removed, err := resolutions.Clear(paths, options.Adapters)
		if err != nil {
			report.Errors = append(report.Errors, err.Error())
			return report, nil
		}
		report.OK = true
		report.Removed = append([]resolutions.Entry{}, removed...)
		file, loadErr := resolutions.Load(paths)
		if loadErr != nil {
			report.Warnings = append(report.Warnings, "决定已清除，但读取剩余列表失败: "+loadErr.Error())
			// Clear has already succeeded. A zero count is the only safe
			// count to return when the post-operation file cannot be read.
			report.Entries = []resolutions.Entry{}
		} else {
			report.Entries = append([]resolutions.Entry{}, file.Entries...)
		}
	default:
		report.Errors = append(report.Errors, fmt.Sprintf("unknown resolution action %q", options.ResolutionAction))
		return report, nil
	}
	report.Pending = len(report.Entries)
	return report, nil
}

type clearGuard struct {
	paths    core.HomerPaths
	consumed []resolutions.Entry
	warnings []string
}

// beginClear snapshots the selected entries before an immediate push/pull.
// finish removes only that exact snapshot, so a concurrent re-record survives.
func (d *Daemon) beginClear(options hub.TaskOptions) clearGuard {
	guard := clearGuard{paths: d.resolutionPaths()}
	if !options.ClearResolutions || len(options.Adapters) == 0 {
		return guard
	}
	file, err := resolutions.Load(guard.paths)
	if err != nil {
		guard.warnings = append(guard.warnings, "读取待清除决定失败，未清除任何条目: "+err.Error())
		d.logf("agent: unable to snapshot staged resolutions before clear: %v", err)
		return guard
	}
	selected := make(map[string]struct{}, len(options.Adapters))
	for _, adapter := range options.Adapters {
		selected[adapter] = struct{}{}
	}
	for _, entry := range file.Entries {
		if _, ok := selected[entry.Adapter]; ok {
			guard.consumed = append(guard.consumed, entry)
		}
	}
	return guard
}

func (g clearGuard) finish(ok bool) []string {
	warnings := append([]string(nil), g.warnings...)
	if !ok || len(g.consumed) == 0 {
		return warnings
	}
	if _, err := resolutions.RemoveIfSame(g.paths, g.consumed); err != nil {
		warnings = append(warnings, "清除已记录决定失败，条目仍保留: "+err.Error())
	}
	return warnings
}

type ResolvedPullRequest struct {
	Confirm          bool
	Adapters         []string
	CenterGeneration int
	AllowSecrets     bool
}

// PullApplyingResolutions applies only decisions belonging to the explicit
// selection. Local decisions are published before any pull begins (D4's
// revised ordering); successful outcomes are consumed using per-entry CAS.
func (e *localExecutor) PullApplyingResolutions(ctx context.Context, req ResolvedPullRequest) (commands.PullReport, error) {
	if e == nil {
		return commands.PullReport{}, errors.New("nil executor")
	}
	legacyPull := func() (commands.PullReport, error) {
		return e.Pull(ctx, req.Confirm, req.Adapters, false)
	}
	if !req.Confirm || strings.TrimSpace(e.hubURL) == "" || len(req.Adapters) == 0 {
		return legacyPull()
	}

	paths := homerPathsForHome(e.homerHome)
	file, err := resolutions.Load(paths)
	if err != nil {
		report, pullErr := legacyPull()
		report.Warnings = append(report.Warnings, "读取已记录决定失败，已按普通下发执行: "+err.Error())
		return report, pullErr
	}

	entriesByAdapter := resolutions.Index(file.Entries)
	adapterOrder := uniqueAdapterIDs(req.Adapters)
	selectedEntries := make(map[string]resolutions.Entry)
	for _, adapter := range adapterOrder {
		if entry, ok := entriesByAdapter[adapter]; ok {
			selectedEntries[adapter] = entry
		}
	}
	if len(selectedEntries) == 0 {
		return legacyPull()
	}

	localIDs := make([]string, 0)
	centerIDs := make([]string, 0)
	staleOutcomes := make(map[string]commands.ResolutionOutcome)
	for _, adapter := range adapterOrder {
		entry, ok := selectedEntries[adapter]
		if !ok {
			continue
		}
		if resolutions.Stale(entry, req.CenterGeneration) {
			staleOutcomes[adapter] = commands.ResolutionOutcome{
				Adapter: adapter,
				Choice:  entry.Choice,
				Status:  resolutions.OutcomeStale,
				Message: "中心世代已变化，决定未应用且仍保留。",
			}
			continue
		}
		switch entry.Choice {
		case resolutions.ChoiceLocal:
			localIDs = append(localIDs, adapter)
		case resolutions.ChoiceCenter:
			centerIDs = append(centerIDs, adapter)
		}
	}

	outcomes := make(map[string]commands.ResolutionOutcome, len(selectedEntries))
	for adapter, outcome := range staleOutcomes {
		outcomes[adapter] = outcome
	}
	localWarnings := make([]string, 0)

	// D4 revision: publish local decisions before fetching/applying the center
	// snapshot. A lengthy pull must not widen the overwrite-generation window.
	if len(localIDs) > 0 {
		pushReport, pushErr := e.Push(ctx, true, localIDs, true, req.AllowSecrets)
		localWarnings = append(localWarnings, pushReport.Warnings...)
		status := ""
		message := ""
		switch {
		case pushErr != nil:
			status = resolutions.OutcomeFailed
			message = "本机内容未能发布到中心: " + pushErr.Error()
		case pushReport.Status == commands.PushStatusPushed:
			status = resolutions.OutcomePublished
		case pushReport.Status == commands.PushStatusNoDrift:
			status = resolutions.OutcomeNoop
		case pushReport.Status == commands.PushStatusSecretsRejected:
			status = resolutions.OutcomeFailed
			message = "pi 没有写入中心：发现疑似密钥，决定已保留。到「处理冲突」选「记录并立即执行」并确认密钥提示"
		default:
			status = resolutions.OutcomeFailed
			reason := strings.Join(pushReport.Errors, "；")
			if reason == "" {
				reason = "本机内容未能发布到中心（状态: " + string(pushReport.Status) + ")"
			}
			message = "本机内容未能发布到中心: " + reason
		}
		for _, adapter := range localIDs {
			entry := selectedEntries[adapter]
			outcome := commands.ResolutionOutcome{Adapter: adapter, Choice: entry.Choice, Status: status}
			if status == resolutions.OutcomeFailed {
				if !strings.Contains(message, "决定已保留") {
					message = "决定已保留；" + message
				}
				outcome.Message = message
			}
			outcomes[adapter] = outcome
		}
	}

	rest := make([]string, 0, len(req.Adapters))
	seenRest := make(map[string]struct{}, len(req.Adapters))
	localSet := make(map[string]struct{}, len(localIDs))
	for _, adapter := range localIDs {
		localSet[adapter] = struct{}{}
	}
	for _, adapter := range req.Adapters {
		if _, skip := localSet[adapter]; skip {
			continue
		}
		if _, seen := seenRest[adapter]; seen {
			continue
		}
		seenRest[adapter] = struct{}{}
		rest = append(rest, adapter)
	}

	report := commands.NewPullReport(commands.PullStatusNoDrift)
	var pullErr error
	if len(rest) > 0 {
		report, pullErr = e.pull(ctx, true, rest, false, centerIDs)
		if pullErr != nil {
			failed := commands.NewPullReport(commands.PullStatusError)
			failed.Errors = append(failed.Errors, "下发中心快照失败: "+pullErr.Error())
			report = failed
		}
	}
	report.Warnings = append(report.Warnings, localWarnings...)

	for _, adapter := range centerIDs {
		entry := selectedEntries[adapter]
		outcome := commands.ResolutionOutcome{Adapter: adapter, Choice: entry.Choice}
		switch {
		case report.Status == commands.PullStatusError || report.Status == commands.PullStatusAborted:
			outcome.Status = resolutions.OutcomeFailed
			outcome.Message = "决定已保留；下发状态为 " + string(report.Status) + "。"
		case pullReportHasConflict(report, adapter):
			outcome.Status = resolutions.OutcomeFailed
			outcome.Message = "决定已保留；该 adapter 下发后仍有冲突。"
		case pullReportChangedAdapter(report, adapter):
			outcome.Status = resolutions.OutcomeApplied
		default:
			outcome.Status = resolutions.OutcomeNoop
		}
		outcomes[adapter] = outcome
	}

	failedOutcomes := 0
	for _, adapter := range adapterOrder {
		outcome, ok := outcomes[adapter]
		if !ok {
			continue
		}
		report.Resolutions = append(report.Resolutions, outcome)
		if outcome.Status == resolutions.OutcomeFailed {
			failedOutcomes++
			report.Errors = append(report.Errors, adapter+"："+outcome.Message)
			continue
		}
		if outcome.Status != resolutions.OutcomeApplied && outcome.Status != resolutions.OutcomePublished && outcome.Status != resolutions.OutcomeNoop {
			continue
		}
		if _, removeErr := resolutions.RemoveIfSame(paths, []resolutions.Entry{selectedEntries[adapter]}); removeErr != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("清除 %s 的已执行决定失败，条目仍保留: %v", adapter, removeErr))
		}
	}
	if failedOutcomes > 0 {
		report.OK = false
		if report.Status != commands.PullStatusConflictsRemain {
			report.Status = commands.PullStatusError
		}
	}
	if report.Resolutions == nil {
		report.Resolutions = []commands.ResolutionOutcome{}
	}
	if report.Warnings == nil {
		report.Warnings = []string{}
	}
	if report.Errors == nil {
		report.Errors = []string{}
	}

	return report, nil
}

func uniqueAdapterIDs(adapters []string) []string {
	seen := make(map[string]struct{}, len(adapters))
	unique := make([]string, 0, len(adapters))
	for _, adapter := range adapters {
		if _, ok := seen[adapter]; ok {
			continue
		}
		seen[adapter] = struct{}{}
		unique = append(unique, adapter)
	}
	return unique
}

func pullReportHasConflict(report commands.PullReport, adapter string) bool {
	for _, conflict := range report.Conflicts {
		if conflict.AdapterID == adapter {
			return true
		}
	}
	return false
}

func pullReportChangedAdapter(report commands.PullReport, adapter string) bool {
	for _, file := range report.Applied.Written {
		if file.AdapterID == adapter {
			return true
		}
	}
	for _, file := range report.Applied.Deleted {
		if file.AdapterID == adapter {
			return true
		}
	}
	return false
}

func (d *Daemon) executorPullResolved(ctx context.Context, req ResolvedPullRequest) (commands.PullReport, error) {
	d.execMu.RLock()
	defer d.execMu.RUnlock()
	local, ok := d.exec.(*localExecutor)
	if !ok {
		return commands.PullReport{}, fmt.Errorf("该 executor 不能应用已记录的冲突决定")
	}
	report, err := local.PullApplyingResolutions(ctx, req)
	for _, outcome := range report.Resolutions {
		d.logf("staged resolution adapter=%s choice=%s generation=%d status=%s", outcome.Adapter, outcome.Choice, req.CenterGeneration, outcome.Status)
	}
	return report, err
}
