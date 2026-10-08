package agentd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/keyring"
	"github.com/zzjcool/homer-cli/internal/web"
)

const inspectAdapterConcurrency = 4

type inspectAdapterOutcome struct {
	index  int
	id     string
	report commands.StatusReport
	err    error
}

// inspectProgressTracker serializes progress sequence numbers even though the
// adapter scans finish on different goroutines.
type inspectProgressTracker struct {
	mu       sync.Mutex
	total    int
	done     int
	sent     map[string]struct{}
	progress func(any)
}

func newInspectProgressTracker(progress func(any), total int) *inspectProgressTracker {
	return &inspectProgressTracker{total: total, sent: make(map[string]struct{}), progress: progress}
}

func (p *inspectProgressTracker) setTotal(total int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.total = total
	p.mu.Unlock()
}

func (p *inspectProgressTracker) complete(id string, adapter *commands.StatusAdapterReport) {
	if p == nil || p.progress == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.sent[id]; exists {
		return
	}
	p.sent[id] = struct{}{}
	p.done++
	event := web.InspectEvent{Stage: "adapter", Done: p.done, Total: p.total}
	if adapter != nil {
		copy := *adapter
		copy.Categories = append([]commands.StatusCategoryReport(nil), adapter.Categories...)
		event.Adapter = &copy
	}
	// Keep the callback inside the lock so Done remains monotonic in the
	// order Request.Progress enqueues adapter events.
	p.progress(event)
}

func (p *inspectProgressTracker) flush(report commands.StatusReport, adapterIDs []string) {
	if p == nil || p.progress == nil {
		return
	}
	byID := make(map[string]*commands.StatusAdapterReport, len(report.Adapters))
	for i := range report.Adapters {
		byID[report.Adapters[i].ID] = &report.Adapters[i]
	}
	ids := append([]string(nil), adapterIDs...)
	seen := make(map[string]struct{}, len(ids)+len(report.Adapters))
	for _, id := range ids {
		seen[id] = struct{}{}
	}
	for _, adapter := range report.Adapters {
		if _, ok := seen[adapter.ID]; !ok {
			ids = append(ids, adapter.ID)
			seen[adapter.ID] = struct{}{}
		}
	}
	sort.Strings(ids)
	p.setTotal(maxInt(p.currentTotal(), len(ids)))
	for _, id := range ids {
		p.complete(id, byID[id])
	}
}

func (p *inspectProgressTracker) currentTotal() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.total
}

// inspect runs the combined read-only data-plane operation. Local executors
// can split status by adapter; other executors retain the established whole-
// status path and still share its singleflight with status requests.
func (d *Daemon) inspect(ctx context.Context, params web.InspectParams, progress func(any)) (web.InspectResult, error) {
	if d == nil {
		return web.InspectResult{}, errors.New("nil daemon")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return web.InspectResult{}, err
	}

	result := web.InspectResult{}
	local, isLocal := d.localExecutor()
	var config *core.HomerConfig
	configReadable := false
	var tracker *inspectProgressTracker
	var status commands.StatusReport
	var err error

	if isLocal {
		paths := executorPaths(local.homerHome)
		config, err = core.LoadConfig(paths)
		if err == nil {
			configReadable = true
			tracker = newInspectProgressTracker(progress, 0)
			status, err = d.statusFlightDo(ctx, func(scanCtx context.Context) (commands.StatusReport, error) {
				d.execMu.RLock()
				defer d.execMu.RUnlock()
				return inspectLocalStatus(scanCtx, local, paths, config, tracker)
			})
			if err != nil && ctx.Err() == nil {
				// Any optimization failure takes the frozen, whole-status route.
				// Reuse already emitted adapter progress and fill any missing rows
				// from the complete RunStatus report below.
				status, err = d.statusReport(ctx)
			}
		} else {
			// A missing homer.json is a supported new-machine state. The normal
			// executor path retains its marker and scans the built-in defaults.
			status, err = d.statusReport(ctx)
		}
	} else {
		status, err = d.statusReport(ctx)
	}
	if err != nil {
		return web.InspectResult{}, err
	}
	// RunStatus always returns an empty (non-nil) disabled list when there are
	// no disabled adapters. statusFlight's JSON clone omits this field and can
	// otherwise erase that distinction before inspect reaches the wire shape.
	if status.Disabled == nil {
		status.Disabled = []string{}
	}
	if tracker == nil {
		tracker = newInspectProgressTracker(progress, len(status.Adapters))
	}
	tracker.flush(status, nil)
	if len(params.Adapters) > 0 {
		status = selectInspectAdapters(status, params.Adapters)
	}
	result.Status = status

	// A new machine must not run Push's ensureInitialized path as a side
	// effect. Credential and keyring reads remain useful and are safe there.
	allowPushPreflight := !isLocal || configReadable
	if err := d.collectInspectDetails(ctx, params, allowPushPreflight, &result, progress); err != nil {
		return web.InspectResult{}, err
	}
	return result, nil
}

func (d *Daemon) localExecutor() (*localExecutor, bool) {
	if d == nil {
		return nil, false
	}
	d.execMu.RLock()
	local, ok := d.exec.(*localExecutor)
	d.execMu.RUnlock()
	return local, ok
}

func executorPaths(homerHome string) core.HomerPaths {
	return core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return homerHome
		}
		return os.Getenv(key)
	})
}

func inspectLocalStatus(ctx context.Context, executor *localExecutor, paths core.HomerPaths, config *core.HomerConfig, progress *inspectProgressTracker) (commands.StatusReport, error) {
	options := commands.StatusOptions{HomerHome: executor.homerHome}
	var remote []core.AdapterSnapshot
	if strings.TrimSpace(executor.hubURL) != "" {
		snapshot, _, err := executor.downloadHubSnapshot(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return commands.StatusReport{}, ctx.Err()
			}
			return commands.StatusReport{}, err
		}
		remote = snapshot
	}

	adapterIDs := inspectAdapterIDs(config, remote)
	progress.setTotal(len(adapterIDs))
	if len(adapterIDs) == 0 {
		return commands.StatusReport{}, errors.New("inspect config has no adapters")
	}
	outcomes := make([]commands.StatusReport, len(adapterIDs))
	jobs := make(chan int, len(adapterIDs))
	results := make(chan inspectAdapterOutcome, len(adapterIDs))
	workers := minInt(inspectAdapterConcurrency, len(adapterIDs))
	for worker := 0; worker < workers; worker++ {
		go func() {
			for index := range jobs {
				id := adapterIDs[index]
				report, err := inspectOneAdapter(ctx, options, paths, config, remote, id)
				results <- inspectAdapterOutcome{index: index, id: id, report: report, err: err}
			}
		}()
	}
	for index := range adapterIDs {
		jobs <- index
	}
	close(jobs)

	var firstErr error
	for range adapterIDs {
		outcome := <-results
		if outcome.err != nil && firstErr == nil {
			firstErr = outcome.err
		}
		outcomes[outcome.index] = outcome.report
		if outcome.err == nil {
			var adapter *commands.StatusAdapterReport
			if len(outcome.report.Adapters) > 0 {
				adapter = &outcome.report.Adapters[0]
			}
			progress.complete(outcome.id, adapter)
		}
	}
	if firstErr != nil {
		if ctx.Err() != nil {
			return commands.StatusReport{}, ctx.Err()
		}
		return commands.StatusReport{}, firstErr
	}
	merged, equivalent := mergeInspectStatusReports(outcomes)
	if !equivalent {
		return commands.StatusReport{}, errors.New("per-adapter inspect status did not merge equivalently")
	}
	merged.Disabled = commands.DisabledSummaries(*config)
	return merged, nil
}

func inspectOneAdapter(ctx context.Context, options commands.StatusOptions, paths core.HomerPaths, config *core.HomerConfig, remote []core.AdapterSnapshot, adapterID string) (report commands.StatusReport, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			report = commands.StatusReport{}
			err = fmt.Errorf("adapter %s inspect panic: %v", adapterID, recovered)
		}
	}()
	if err := ctx.Err(); err != nil {
		return commands.StatusReport{}, err
	}
	adapterConfig, configured := config.Adapters[adapterID]
	subConfig := &core.HomerConfig{
		Version:  config.Version,
		Backup:   config.Backup,
		Secrets:  config.Secrets,
		Adapters: make(map[string]core.AdapterConfig),
	}
	if configured {
		subConfig.Adapters[adapterID] = adapterConfig
	}
	sources, err := commands.CollectSnapshotSources(paths, subConfig)
	if err != nil {
		return commands.StatusReport{}, err
	}
	if remote != nil {
		selectedRemote := filterAdapterSnapshots(remote, adapterID)
		sources.Remote = commands.NormalizeHubRemote(selectedRemote, subConfig)
	}
	if err := ctx.Err(); err != nil {
		return commands.StatusReport{}, err
	}
	report, err = commands.RunStatus(options, sources)
	if err != nil {
		return commands.StatusReport{}, err
	}
	// RunStatus loads the full config for DisabledSummaries even when the
	// injected snapshots contain one adapter. Keep only that adapter's slice
	// so merging one-adapter reports reconstructs the full list exactly once.
	disabled := report.Disabled[:0]
	for _, item := range report.Disabled {
		if strings.SplitN(item, "/", 2)[0] == adapterID {
			disabled = append(disabled, item)
		}
	}
	report.Disabled = disabled
	return report, nil
}

func inspectAdapterIDs(config *core.HomerConfig, remote []core.AdapterSnapshot) []string {
	// RunStatus receives sorted base/local snapshots first, followed by remote
	// adapters that are not enabled in the local config. Preserve that order
	// so concatenating one-adapter reports is field-for-field identical.
	configured := make([]string, 0, len(config.Adapters))
	for id, adapter := range config.Adapters {
		if adapter.Enabled != nil && !*adapter.Enabled {
			continue
		}
		configured = append(configured, id)
	}
	sort.Strings(configured)
	out := make([]string, 0, len(configured)+len(remote))
	seen := make(map[string]struct{}, len(configured)+len(remote))
	for _, id := range configured {
		out = append(out, id)
		seen[id] = struct{}{}
	}
	for _, adapter := range remote {
		if _, ok := seen[adapter.AdapterID]; ok {
			continue
		}
		seen[adapter.AdapterID] = struct{}{}
		out = append(out, adapter.AdapterID)
	}
	return out
}

func filterAdapterSnapshots(snapshots []core.AdapterSnapshot, adapterID string) []core.AdapterSnapshot {
	filtered := make([]core.AdapterSnapshot, 0, 1)
	for _, snapshot := range snapshots {
		if snapshot.AdapterID == adapterID {
			filtered = append(filtered, snapshot)
		}
	}
	return filtered
}

func mergeInspectStatusReports(reports []commands.StatusReport) (commands.StatusReport, bool) {
	merged := commands.StatusReport{
		Adapters: make([]commands.StatusAdapterReport, 0),
		Errors:   make([]string, 0),
		Disabled: make([]string, 0),
	}
	seenAdapters := make(map[string]struct{})
	for _, report := range reports {
		for _, adapter := range report.Adapters {
			if _, exists := seenAdapters[adapter.ID]; exists {
				return commands.StatusReport{}, false
			}
			seenAdapters[adapter.ID] = struct{}{}
			merged.Adapters = append(merged.Adapters, adapter)
		}
		merged.Errors = appendUniqueInspectStrings(merged.Errors, report.Errors...)
		merged.Disabled = appendUniqueInspectStrings(merged.Disabled, report.Disabled...)
		merged.Warnings = appendUniqueInspectStrings(merged.Warnings, report.Warnings...)
	}
	// CollectSnapshotSources emits adapter scan warnings before repo-wide git
	// warnings. Each isolated scan can repeat the latter; put repeated warnings
	// after adapter-specific warnings to preserve the whole-scan order.
	warningCounts := make(map[string]int, len(merged.Warnings))
	for _, report := range reports {
		seen := make(map[string]struct{}, len(report.Warnings))
		for _, warning := range report.Warnings {
			if _, ok := seen[warning]; ok {
				continue
			}
			seen[warning] = struct{}{}
			warningCounts[warning]++
		}
	}
	uniqueWarnings := make([]string, 0, len(merged.Warnings))
	repeatedWarnings := make([]string, 0, len(merged.Warnings))
	for _, warning := range merged.Warnings {
		if warningCounts[warning] > 1 {
			repeatedWarnings = append(repeatedWarnings, warning)
		} else {
			uniqueWarnings = append(uniqueWarnings, warning)
		}
	}
	merged.Warnings = append(uniqueWarnings, repeatedWarnings...)
	if len(merged.Warnings) == 0 {
		merged.Warnings = nil
	}
	return merged, true
}

func selectInspectAdapters(report commands.StatusReport, requested []string) commands.StatusReport {
	if len(requested) == 0 {
		return report
	}
	selected := make(map[string]struct{}, len(requested))
	for _, id := range requested {
		selected[strings.TrimSpace(id)] = struct{}{}
	}
	adapters := make([]commands.StatusAdapterReport, 0, len(report.Adapters))
	for _, adapter := range report.Adapters {
		if _, ok := selected[adapter.ID]; ok {
			adapters = append(adapters, adapter)
		}
	}
	disabled := make([]string, 0, len(report.Disabled))
	for _, item := range report.Disabled {
		adapterID := strings.SplitN(item, "/", 2)[0]
		if _, ok := selected[adapterID]; ok {
			disabled = append(disabled, item)
		}
	}
	report.Adapters = adapters
	report.Disabled = disabled
	return report
}

func appendUniqueInspectStrings(dst []string, values ...string) []string {
	seen := make(map[string]struct{}, len(dst)+len(values))
	for _, value := range dst {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		dst = append(dst, value)
	}
	return dst
}

func (d *Daemon) collectInspectDetails(ctx context.Context, params web.InspectParams, allowPush bool, result *web.InspectResult, progress func(any)) error {
	type checkResult struct {
		index    int
		stage    string
		present  map[string]bool
		secrets  []web.InspectSecret
		keys     *keyring.Result
		errors   []string
		warnings []string
	}
	jobs := make([]func() checkResult, 0, 3)
	if len(params.Credentials) > 0 {
		paths := append([]string(nil), params.Credentials...)
		jobs = append(jobs, func() checkResult {
			check := checkResult{stage: "credentials"}
			if err := ctx.Err(); err != nil {
				check.errors = []string{err.Error()}
				return check
			}
			probe := keyring.Apply(d.cfg.HomerHome, keyring.Command{Action: "exists", Paths: paths})
			check.present = probe.Exists
			if !probe.OK {
				check.errors = append(check.errors, probe.Errors...)
				if len(check.errors) == 0 {
					check.errors = append(check.errors, "credential existence probe failed: "+probe.Status)
				}
			}
			return check
		})
	}
	if allowPush {
		adapters := append([]string(nil), params.Adapters...)
		if len(adapters) == 0 {
			adapters = nil
		}
		jobs = append(jobs, func() checkResult {
			check := checkResult{stage: "secrets", secrets: make([]web.InspectSecret, 0)}
			if err := ctx.Err(); err != nil {
				check.errors = []string{err.Error()}
				return check
			}
			report, err := d.executorPush(ctx, false, adapters, false, false)
			if err != nil {
				check.errors = append(check.errors, err.Error())
				return check
			}
			check.warnings = append(check.warnings, report.Warnings...)
			if report.Status == commands.PushStatusSecretsRejected {
				for _, finding := range report.Secrets {
					if strings.TrimSpace(finding.Path) == "" {
						continue
					}
					check.secrets = append(check.secrets, web.InspectSecret{
						Path: finding.Path, Description: finding.Description, Line: finding.Line,
					})
				}
				check.secrets = dedupeInspectSecrets(check.secrets)
				return check
			}
			if report.Status != commands.PushStatusAborted {
				check.errors = append(check.errors, report.Errors...)
			}
			return check
		})
	}
	if params.WantKeys {
		jobs = append(jobs, func() checkResult {
			check := checkResult{stage: "keys"}
			if err := ctx.Err(); err != nil {
				check.errors = []string{err.Error()}
				return check
			}
			listed := keyring.Apply(d.cfg.HomerHome, keyring.Command{Action: "list"})
			check.keys = &listed
			if !listed.OK {
				check.errors = append(check.errors, listed.Errors...)
				if len(check.errors) == 0 {
					check.errors = append(check.errors, "keys list failed: "+listed.Status)
				}
			}
			return check
		})
	}
	if len(jobs) == 0 {
		return nil
	}

	completed := make(chan checkResult, len(jobs))
	for index, job := range jobs {
		index, job := index, job
		go func() {
			check := job()
			check.index = index
			completed <- check
		}()
	}
	results := make([]checkResult, len(jobs))
	for range jobs {
		select {
		case check := <-completed:
			results[check.index] = check
			switch check.stage {
			case "credentials":
				if progress != nil {
					progress(web.InspectEvent{Stage: check.stage, Present: check.present})
				}
			case "secrets":
				if progress != nil {
					progress(web.InspectEvent{Stage: check.stage, Secrets: check.secrets})
				}
			case "keys":
				if progress != nil {
					progress(web.InspectEvent{Stage: check.stage, Keys: check.keys})
				}
			}
		case <-ctx.Done():
			// Read probes are bounded and send to a fully buffered channel, so
			// they can finish naturally without blocking the canceled task.
			return ctx.Err()
		}
	}
	for _, check := range results {
		result.Errors = appendUniqueInspectStrings(result.Errors, check.errors...)
		result.Status.Warnings = appendUniqueInspectStrings(result.Status.Warnings, check.warnings...)
		switch check.stage {
		case "credentials":
			result.Present = check.present
		case "secrets":
			if len(check.secrets) > 0 {
				result.Secrets = dedupeInspectSecrets(append(result.Secrets, check.secrets...))
			}
		case "keys":
			result.Keys = check.keys
		}
	}
	if len(result.Status.Warnings) == 0 {
		result.Status.Warnings = nil
	}
	if len(result.Errors) == 0 {
		result.Errors = nil
	}
	return nil
}

func dedupeInspectSecrets(secrets []web.InspectSecret) []web.InspectSecret {
	if len(secrets) == 0 {
		return []web.InspectSecret{}
	}
	seen := make(map[string]struct{}, len(secrets))
	out := make([]web.InspectSecret, 0, len(secrets))
	for _, secret := range secrets {
		if _, ok := seen[secret.Path]; ok {
			continue
		}
		seen[secret.Path] = struct{}{}
		out = append(out, secret)
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
