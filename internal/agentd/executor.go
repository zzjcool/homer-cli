package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/resolutions"
	"github.com/zzjcool/homer-cli/internal/web"
)

type Executor interface {
	Status(ctx context.Context) (commands.StatusReport, error)
	Diff(ctx context.Context, params web.DiffParams) (string, error)
	Push(ctx context.Context, confirm bool, adapters []string, overwrite bool, allowSecrets bool) (commands.PushReport, error)
	Pull(ctx context.Context, confirm bool, adapters []string, preferRemote bool) (commands.PullReport, error)
}

// errNoHubSnapshot is the empty-center response. A collect treats it as
// "publish these adapters into a new generation"; a dispatch still fails.
var errNoHubSnapshot = errors.New("hub has no snapshot")

type localExecutor struct {
	homerHome string
	// No-git data plane transport: when hubURL is set, push uploads and
	// pull download through the hub's /api/snapshot endpoints.
	hubURL        string
	credential    string
	hubHTTP       *http.Client
	snapshotCache snapshotCache
	// lastDownloadGeneration is the numeric generation of the most recent
	// successful snapshot download, even when the cache could not store it
	// (no ETag from an intermediary). It is the CAS fallback base.
	lastDownloadGeneration int
}

// NewLocalExecutor returns the production task executor. The commands package
// remains the sole implementation of Homer status/diff/push/pull semantics;
// this adapter only supplies the daemon's workspace and frozen options.
func NewLocalExecutor(homerHome string) Executor {
	return &localExecutor{homerHome: homerHome}
}

func (e *localExecutor) Status(ctx context.Context) (commands.StatusReport, error) {
	if err := contextError(ctx); err != nil {
		return commands.StatusReport{}, err
	}
	opts := commands.StatusOptions{HomerHome: e.homerHome}
	report, err := e.statusReport(ctx, opts)
	if err != nil {
		// A fresh machine (no homer.json yet) is a LEGAL state — "new
		// machine awaiting dispatch", not a task failure. Returning the
		// error would surface as a 502 "agent unreachable" in the
		// console, which is both wrong and alarming. Degraded to an
		// empty report that explains itself; every other error still
		// propagates.
		if missingConfig(err) {
			return commands.StatusReport{
				Adapters: []commands.StatusAdapterReport{},
				Errors:   []string{err.Error()},
			}, nil
		}
		return commands.StatusReport{}, err
	}
	if err := contextError(ctx); err != nil {
		return commands.StatusReport{}, err
	}
	e.attachResolutions(&report)
	return report, nil
}

// attachResolutions enriches status with this machine's pending decisions.
// Resolution storage is advisory to status, so read failures only warn and
// never make an otherwise successful scan fail.
func (e *localExecutor) attachResolutions(report *commands.StatusReport) {
	if e == nil || report == nil {
		return
	}
	for _, message := range report.Errors {
		if strings.Contains(message, "未找到 homer 配置") {
			// The fresh-machine fallback has no initialized workspace to which
			// staged decisions can safely be attributed.
			return
		}
	}
	file, err := resolutions.Load(homerPathsForHome(e.homerHome))
	if err != nil {
		report.Warnings = append(report.Warnings, "读取已记录决定失败: "+err.Error())
		return
	}
	if len(file.Entries) == 0 {
		return
	}
	report.Resolutions = make([]commands.StatusResolution, 0, len(file.Entries))
	for _, entry := range file.Entries {
		report.Resolutions = append(report.Resolutions, commands.StatusResolution{
			Adapter:            entry.Adapter,
			Choice:             entry.Choice,
			RecordedAt:         entry.RecordedAt,
			GenerationAtRecord: entry.GenerationAtRecord,
		})
	}
}

// missingConfig reports whether the error is the fresh-machine "no
// homer.json yet" state, matched STRUCTURALLY on the sentinel — a
// message-text match would silently break on any copy change.
func missingConfig(err error) bool {
	return core.IsConfigNotInitialized(err)
}

// statusReport is RunStatus, plus the hub generation as the remote side
// when this executor has a hub. Local status only sees the store baseline,
// so a center change never showed up as ↓ or as a conflict until this.
func (e *localExecutor) statusReport(ctx context.Context, opts commands.StatusOptions) (commands.StatusReport, error) {
	if e == nil || e.hubURL == "" {
		return commands.RunStatus(opts)
	}
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return e.homerHome
		}
		return os.Getenv(key)
	})
	config, err := core.LoadConfig(paths)
	if err != nil {
		return commands.RunStatus(opts)
	}
	sources, err := commands.CollectSnapshotSources(paths, config)
	if err != nil {
		return commands.RunStatus(opts)
	}
	remote, _, downloadErr := e.downloadHubSnapshot(ctx)
	if downloadErr != nil {
		return commands.RunStatus(opts)
	}
	sources.Remote = commands.NormalizeHubRemote(remote, config)
	return commands.RunStatus(opts, sources)
}

func (e *localExecutor) Diff(ctx context.Context, params web.DiffParams) (string, error) {
	if err := contextError(ctx); err != nil {
		return "", err
	}
	if params.Path != "" {
		sides, err := commands.ReadLocalFile(commands.DiffOptions{
			HomerHome: e.homerHome,
			Adapter:   params.Adapter,
			Category:  params.Category,
			Path:      params.Path,
		})
		if err != nil {
			return "", err
		}
		raw, err := json.Marshal(sides)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	}
	text, err := commands.RunDiff(commands.DiffOptions{
		HomerHome: e.homerHome,
		Adapter:   params.Adapter,
		Category:  params.Category,
	})
	if err != nil {
		return "", err
	}
	if err := contextError(ctx); err != nil {
		return "", err
	}
	return text, nil
}

func (e *localExecutor) Push(ctx context.Context, confirm bool, adapters []string, overwrite bool, allowSecrets bool) (commands.PushReport, error) {
	if err := contextError(ctx); err != nil {
		return commands.PushReport{}, err
	}
	// Console 收取 on a brand-new machine: there is no homer.json yet, but
	// the button is still offered. The hub transport initializes from the
	// built-in adapters so local files can be uploaded. A plain local push
	// (no hub) must keep refusing to create homer.json.
	if e.hubURL != "" {
		if err := e.ensureInitialized(); err != nil {
			return commands.PushReport{}, err
		}
	}
	if e.hubURL != "" && adapters != nil {
		// Scoped hub push with a CAS-guarded publish (R2): the decision runs
		// against a downloaded snapshot, and the upload must land on the same
		// generation. A conflict means another machine moved the center while
		// this decision was being prepared — re-download and re-judge once,
		// then surface the conflict so the caller keeps its record.
		var report commands.PushReport
		var pushErr error
		for attempt := 0; attempt < 2; attempt++ {
			deps := &commands.PushDeps{UI: commands.HeadlessUI{}}
			snapshot, _, err := e.downloadHubSnapshot(ctx)
			switch {
			case errors.Is(err, errNoHubSnapshot):
				deps.HubSnapshot = []core.AdapterSnapshot{}
			case err != nil:
				return commands.PushReport{}, err
			default:
				deps.HubSnapshot = snapshot
			}
			captured := adapters
			decisionBase := e.snapshotCache.cachedGeneration()
			if decisionBase == 0 {
				decisionBase = e.lastDownloadGeneration
			}
			var casConflict error
			deps.HubSink = func(snapshot []core.AdapterSnapshot) (int, error) {
				generation, uploadErr := e.uploadHubSnapshotAt(ctx, snapshot, captured, decisionBase)
				if errors.Is(uploadErr, errGenerationConflict) {
					casConflict = uploadErr
				}
				return generation, uploadErr
			}
			report = commands.RunPush(commands.PushOptions{
				HomerHome:    e.homerHome,
				Yes:          confirm,
				Adapters:     adapters,
				Overwrite:    overwrite,
				AllowSecrets: allowSecrets,
			}, deps)
			if err := contextError(ctx); err != nil {
				return commands.PushReport{}, err
			}
			// A CAS conflict means RunPush tried to publish against a moved
			// center; its report carries the sink error already. Retry once
			// with a fresh download; a second conflict returns the report so
			// the caller keeps its recorded decision.
			if casConflict == nil || attempt > 0 {
				return report, nil
			}
			// Conflict on the first attempt: invalidate the cache so the
			// next loop iteration re-downloads the moved center.
			e.snapshotCache.invalidate()
		}
		return report, pushErr
	}
	// Unscoped push (adapters == nil) with a hub: the upload REPLACES the
	// generation wholesale, so there is no scoped-merge judgment to lose —
	// no CAS base is attached. This preserves the legacy bootstrap path.
	deps := &commands.PushDeps{UI: commands.HeadlessUI{}}
	if e.hubURL != "" {
		deps.HubSink = func(snapshot []core.AdapterSnapshot) (int, error) {
			return e.uploadHubSnapshotAt(ctx, snapshot, nil, 0)
		}
	}
	report := commands.RunPush(commands.PushOptions{
		HomerHome:    e.homerHome,
		Yes:          confirm,
		Adapters:     adapters,
		Overwrite:    overwrite,
		AllowSecrets: allowSecrets,
	}, deps)
	if err := contextError(ctx); err != nil {
		return commands.PushReport{}, err
	}
	return report, nil
}

func (e *localExecutor) Pull(ctx context.Context, confirm bool, adapters []string, preferRemote bool) (commands.PullReport, error) {
	return e.pull(ctx, confirm, adapters, preferRemote, nil)
}

// pull is the private extension point for applying center choices to only the
// selected adapters. Pull retains its frozen Executor signature and delegates
// with nil, which preserves PreferRemote's existing whole-selection behavior.
func (e *localExecutor) pull(ctx context.Context, confirm bool, adapters []string, preferRemote bool, preferRemoteAdapters []string) (commands.PullReport, error) {
	if err := contextError(ctx); err != nil {
		return commands.PullReport{}, err
	}
	deps := &commands.PullDeps{UI: commands.HeadlessUI{}, NoFetch: true}
	if e.hubURL != "" {
		snapshot, meta, err := e.downloadHubSnapshot(ctx)
		if err != nil {
			return commands.PullReport{}, err
		}
		if err := e.bootstrapFromGeneration(snapshot, meta); err != nil {
			return commands.PullReport{}, err
		}
		deps.HubSnapshot = snapshot
	}
	report := commands.RunPull(commands.PullOptions{
		HomerHome:            e.homerHome,
		Yes:                  confirm,
		Adapters:             adapters,
		PreferRemote:         preferRemote,
		PreferRemoteAdapters: preferRemoteAdapters,
	}, deps)
	if err := contextError(ctx); err != nil {
		return commands.PullReport{}, err
	}
	return report, nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

var _ Executor = (*localExecutor)(nil)

// NewLocalExecutorWithHub builds an executor whose push/pull use the hub's
// HTTP snapshot transport (the no-git data plane). Downloaded hub
// generations are applied through the normal pull pipeline; prepared
// snapshots upload through the push pipeline. The credential is the
// agent's bearer secret.
func NewLocalExecutorWithHub(homerHome, hubURL, credential string) Executor {
	return &localExecutor{homerHome: homerHome, hubURL: strings.TrimSpace(hubURL), credential: strings.TrimSpace(credential)}
}

// SetHubCredential replaces the bearer sent to /api/snapshot. The executor
// is built before a one-time enrollment code is redeemed, so the per-agent
// secret does not exist yet; the daemon pushes it here as soon as enroll
// succeeds. Leaving the original empty credential in place makes the first
// 下发 download the hub snapshot with no Authorization and the hub answers 401.
func (e *localExecutor) SetHubCredential(credential string) {
	if e == nil {
		return
	}
	e.credential = strings.TrimSpace(credential)
}

// ensureInitialized creates homer.json when this machine has never been
// initialized. RunInit with All is non-interactive: it records the built-in
// adapters and a store snapshot of whatever is already on disk.
func (e *localExecutor) ensureInitialized() error {
	if e == nil {
		return errors.New("nil executor")
	}
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return e.homerHome
		}
		return os.Getenv(key)
	})
	if _, err := os.Stat(paths.ConfigFile); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	_, err := commands.RunInit(commands.InitOptions{HomerHome: e.homerHome, All: true})
	return err
}

// Resolve applies a console conflict choice on this machine. "local" keeps
// the machine's content and uploads it; "center" overwrites local files
// from the hub generation. A plain push/pull refuses while conflicts
// remain, so the choice has to go through merge.
func (e *localExecutor) Resolve(ctx context.Context, choice string) (commands.MergeReport, error) {
	if err := contextError(ctx); err != nil {
		return commands.MergeReport{}, err
	}
	if choice != "local" && choice != "center" {
		return commands.MergeReport{}, fmt.Errorf("unknown resolve choice %q", choice)
	}
	deps := &commands.MergeDeps{UI: commands.HeadlessUI{}, NoFetch: true}
	if e.hubURL != "" {
		snapshot, _, err := e.downloadHubSnapshot(ctx)
		if err != nil {
			return commands.MergeReport{}, err
		}
		deps.HubSnapshot = snapshot
		if choice == "local" {
			deps.HubSink = func(snapshot []core.AdapterSnapshot) (int, error) {
				return e.uploadHubSnapshot(ctx, snapshot, nil)
			}
		}
	}
	report := commands.RunMerge(commands.MergeOptions{
		HomerHome:    e.homerHome,
		AcceptLocal:  choice == "local",
		AcceptRemote: choice == "center",
	}, deps)
	if err := contextError(ctx); err != nil {
		return commands.MergeReport{}, err
	}
	return report, nil
}

// hubSnapshotPayload mirrors the wire format of /api/snapshot.
type hubSnapshotPayload struct {
	Generation int                          `json:"generation"`
	HomerJSON  string                       `json:"homerJson"`
	Store      map[string]map[string]string `json:"store"`
	Adapters   []string                     `json:"adapters,omitempty"`
}

// downloadHubSnapshot fetches the hub's current generation and converts it
// into the adapter-snapshot form the pull pipeline consumes.
func (e *localExecutor) downloadHubSnapshot(ctx context.Context) ([]core.AdapterSnapshot, []byte, error) {
	if e == nil {
		return nil, nil, errors.New("nil executor")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	flight, leader := e.snapshotCache.beginDownload()
	if !leader {
		select {
		case <-flight.done:
			return cloneAdapterSnapshots(flight.snapshots), append([]byte(nil), flight.meta...), flight.err
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	snapshots, meta, err := e.downloadHubSnapshotUnshared(ctx)
	e.snapshotCache.finishDownload(flight, snapshots, meta, err)
	return cloneAdapterSnapshots(snapshots), append([]byte(nil), meta...), err
}

func (e *localExecutor) downloadHubSnapshotUnshared(ctx context.Context) ([]core.AdapterSnapshot, []byte, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(e.hubURL), "/")
	if baseURL == "" {
		return nil, nil, errors.New("hub data URL is empty")
	}

	// A 304 is only useful while the decoded generation it names is still in
	// memory. If an upload invalidates that entry while this request is in
	// flight, retry once without a validator instead of returning stale data.
	for attempt := 0; attempt < 2; attempt++ {
		etag, cacheEpoch, cached := e.snapshotCache.requestState()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/snapshot", nil)
		if err != nil {
			return nil, nil, err
		}
		if e.credential != "" {
			request.Header.Set("Authorization", "Bearer "+e.credential)
		}
		if cached {
			request.Header.Set("If-None-Match", etag)
		}
		response, err := e.hubClient().Do(request)
		if err != nil {
			return nil, nil, err
		}
		if response.StatusCode == http.StatusNotModified {
			responseETag := response.Header.Get("ETag")
			_ = response.Body.Close()
			if responseETag != "" && responseETag != etag {
				e.snapshotCache.invalidateIf(etag, cacheEpoch)
				if attempt == 0 {
					continue
				}
				return nil, nil, fmt.Errorf("hub snapshot: 304 etag %q does not match requested generation %q", responseETag, etag)
			}
			if snapshots, meta, ok := e.snapshotCache.get(etag, cacheEpoch); ok {
				return snapshots, meta, nil
			}
			if attempt == 0 {
				continue
			}
			return nil, nil, errors.New("hub snapshot: received 304 without a cached generation")
		}
		if response.StatusCode == http.StatusConflict {
			_ = response.Body.Close()
			if cached {
				e.snapshotCache.invalidateIf(etag, cacheEpoch)
			}
			return nil, nil, errNoHubSnapshot
		}
		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			return nil, nil, fmt.Errorf("hub snapshot: %s %s", response.Status, strings.TrimSpace(string(body)))
		}
		var payload hubSnapshotPayload
		decodeErr := json.NewDecoder(response.Body).Decode(&payload)
		_ = response.Body.Close()
		if decodeErr != nil {
			return nil, nil, decodeErr
		}
		e.lastDownloadGeneration = payload.Generation
		snapshots := snapshotsFromWire(payload.Store)
		meta := []byte(payload.HomerJSON)
		responseETag := response.Header.Get("ETag")
		storedEpoch := e.snapshotCache.store(cacheEpoch, responseETag, payload.Generation, snapshots, meta)
		if responseETag == "" {
			// ETag support is part of the hub contract, but an intermediary or
			// older hub may omit it. The body is still usable; simply do not cache.
			return cloneAdapterSnapshots(snapshots), append([]byte(nil), meta...), nil
		}
		if cachedSnapshots, cachedMeta, ok := e.snapshotCache.get(responseETag, storedEpoch); ok {
			return cachedSnapshots, cachedMeta, nil
		}
		// The response lost a race to an invalidation or a newer generation.
		// Retry once without the stale response's validator.
		if attempt == 0 {
			continue
		}
		return nil, nil, errors.New("hub snapshot: generation changed during consecutive downloads")
	}
	return nil, nil, errors.New("hub snapshot: conditional request retry exhausted")
}

// bootstrapFromGeneration seeds an uninitialized workspace from the hub's
// current generation: homer.json becomes the machine's config. The store
// stays EMPTY on purpose — with an empty baseline the first pull judges
// every center file as new content and applies it cleanly; seeding the
// store with the generation would instead read the fresh machine's empty
// tool directories as "deleted everything" and conflict. The post-apply
// step of that first pull writes the real baseline. An existing config is
// never overwritten (a configured machine pulls normally).
func (e *localExecutor) bootstrapFromGeneration(_ []core.AdapterSnapshot, meta []byte) error {
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return e.homerHome
		}
		return os.Getenv(key)
	})
	if _, err := os.Stat(paths.ConfigFile); err == nil {
		return addMissingAdapters(paths, meta)
	}
	if len(meta) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o755); err != nil {
		return err
	}
	return os.WriteFile(paths.ConfigFile, meta, 0o644)
}

// addMissingAdapters brings a configured machine up to the center's adapter
// list. An adapter the center knows (the keyring is added on whichever
// machine first creates a key) but this machine's homer.json lacks can never
// be dispatched: the machine only trusts its own config and answers "没有适配器".
// Only absent adapters are added, from the center's definition; anything the
// machine already has is left exactly as the user set it.
func addMissingAdapters(paths core.HomerPaths, meta []byte) error {
	if len(meta) == 0 {
		return nil
	}
	center, problems := core.ValidateConfig(meta)
	if center == nil {
		log.Printf("agent: ignoring invalid hub homer.json config while bootstrapping: %s", strings.Join(problems, "; "))
		return nil // an unreadable center config must not block a pull
	}
	local, err := core.LoadConfig(paths)
	if err != nil {
		return nil // a broken local config is reported by the pull itself
	}
	added := false
	for id, adapter := range center.Adapters {
		if _, ok := local.Adapters[id]; ok {
			continue
		}
		if local.Adapters == nil {
			local.Adapters = map[string]core.AdapterConfig{}
		}
		local.Adapters[id] = adapter
		added = true
	}
	if !added {
		return nil
	}
	return core.SaveConfig(paths, *local)
}

// uploadHubSnapshot pushes prepared snapshots to the hub.
// errGenerationConflict marks a CAS rejection from the hub: the center moved
// between the download this decision was based on and the upload attempt.
var errGenerationConflict = errors.New("hub upload: 中心世代已变化")

func (e *localExecutor) uploadHubSnapshot(ctx context.Context, snapshot []core.AdapterSnapshot, adapters []string) (int, error) {
	return e.uploadHubSnapshotAt(ctx, snapshot, adapters, 0)
}

// uploadHubSnapshotAt uploads with a CAS precondition: baseGeneration > 0
// requires the hub to still be at that generation (R2). A conflict is
// returned as errGenerationConflict so callers can re-decide.
func (e *localExecutor) uploadHubSnapshotAt(ctx context.Context, snapshot []core.AdapterSnapshot, adapters []string, baseGeneration int) (int, error) {
	store := map[string]map[string]string{}
	for _, adapter := range snapshot {
		for _, category := range adapter.Categories {
			for relPath, entry := range category.Files {
				if store[adapter.AdapterID] == nil {
					store[adapter.AdapterID] = map[string]string{}
				}
				store[adapter.AdapterID][category.Category+"/"+relPath] = entry.Content
			}
		}
	}
	// homer.json rides along: an empty machine bootstraps its config from
	// the generation's meta on first pull.
	meta := []byte("{}")
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" {
			return e.homerHome
		}
		return os.Getenv(key)
	})
	if data, err := os.ReadFile(paths.ConfigFile); err == nil {
		meta = data
	}
	body, err := json.Marshal(hubSnapshotPayload{Store: store, HomerJSON: string(meta), Adapters: adapters, Generation: baseGeneration})
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(e.hubURL, "/")+"/api/snapshot", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	if e.credential != "" {
		request.Header.Set("Authorization", "Bearer "+e.credential)
	}
	request.Header.Set("Content-Type", "application/json")
	// Invalidate before and after sending: a concurrent GET started after the
	// first invalidation must not repopulate the pre-upload generation while
	// this POST is still in flight.
	e.snapshotCache.invalidate()
	defer e.snapshotCache.invalidate()
	response, err := e.hubClient().Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict && baseGeneration > 0 {
		responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return 0, fmt.Errorf("%w: %s", errGenerationConflict, strings.TrimSpace(string(responseBody)))
	}
	if response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return 0, fmt.Errorf("hub upload: %s %s", response.Status, strings.TrimSpace(string(responseBody)))
	}
	var uploaded struct {
		OK         bool `json:"ok"`
		Generation int  `json:"generation"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&uploaded); err != nil {
		return 0, fmt.Errorf("hub upload: %w", err)
	}
	if !uploaded.OK {
		return 0, fmt.Errorf("hub upload: hub 未确认（generation 缺失）")
	}
	return uploaded.Generation, nil
}

// snapshotsFromWire converts the flat wire store into adapter snapshots
// (kind: file — JSON merge categories carry their content inline).
func snapshotsFromWire(store map[string]map[string]string) []core.AdapterSnapshot {
	adapters := make([]core.AdapterSnapshot, 0, len(store))
	for adapterID, files := range store {
		categories := map[string]*core.CategorySnapshot{}
		for name, content := range files {
			segments := strings.SplitN(name, "/", 2)
			if len(segments) != 2 {
				continue
			}
			category, rel := segments[0], segments[1]
			if categories[category] == nil {
				categories[category] = &core.CategorySnapshot{
					AdapterID: adapterID,
					Category:  category,
					Mode:      core.SyncModeMirror,
					Files:     core.SnapshotFiles{},
				}
			}
			categories[category].Files[rel] = core.SnapshotEntry{Kind: "file", Content: content}
		}
		snapshot := core.AdapterSnapshot{AdapterID: adapterID, Categories: []core.CategorySnapshot{}}
		for _, category := range categories {
			snapshot.Categories = append(snapshot.Categories, *category)
		}
		adapters = append(adapters, snapshot)
	}
	return adapters
}

func (e *localExecutor) hubClient() *http.Client {
	if e.hubHTTP != nil {
		return e.hubHTTP
	}
	return &http.Client{Timeout: 120 * time.Second}
}
