package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
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
	hubURL     string
	credential string
	hubHTTP    *http.Client
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
	return report, nil
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
	sources.Remote = remote
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
	deps := &commands.PushDeps{UI: commands.HeadlessUI{}}
	if e.hubURL != "" {
		if adapters != nil {
			snapshot, _, err := e.downloadHubSnapshot(ctx)
			switch {
			case errors.Is(err, errNoHubSnapshot):
				deps.HubSnapshot = []core.AdapterSnapshot{}
			case err != nil:
				return commands.PushReport{}, err
			default:
				deps.HubSnapshot = snapshot
			}
		}
		captured := adapters
		deps.HubSink = func(snapshot []core.AdapterSnapshot) (int, error) {
			return e.uploadHubSnapshot(ctx, snapshot, captured)
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
		HomerHome:    e.homerHome,
		Yes:          confirm,
		Adapters:     adapters,
		PreferRemote: preferRemote,
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
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(e.hubURL, "/")+"/api/snapshot", nil)
	if err != nil {
		return nil, nil, err
	}
	if e.credential != "" {
		request.Header.Set("Authorization", "Bearer "+e.credential)
	}
	response, err := e.hubClient().Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict {
		return nil, nil, errNoHubSnapshot
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, nil, fmt.Errorf("hub snapshot: %s %s", response.Status, strings.TrimSpace(string(body)))
	}
	var payload hubSnapshotPayload
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, nil, err
	}
	return snapshotsFromWire(payload.Store), []byte(payload.HomerJSON), nil
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
		return nil // already configured
	}
	if len(meta) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o755); err != nil {
		return err
	}
	return os.WriteFile(paths.ConfigFile, meta, 0o644)
}

// uploadHubSnapshot pushes prepared snapshots to the hub.
func (e *localExecutor) uploadHubSnapshot(ctx context.Context, snapshot []core.AdapterSnapshot, adapters []string) (int, error) {
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
	body, err := json.Marshal(hubSnapshotPayload{Store: store, HomerJSON: string(meta), Adapters: adapters})
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
	response, err := e.hubClient().Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
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

// agentSyncDeps adapts the executor's hub transport into the web server's
// SyncDepsSource: the listen agent's own /api/pull and /api/push (invoked
// by the hub's dispatcher over direct HTTP) run through the same no-git
// transport as task execution.
type agentSyncDeps struct {
	executor *localExecutor
}

func (d agentSyncDeps) PullDeps() *commands.PullDeps {
	deps := &commands.PullDeps{NoFetch: true}
	if d.executor.hubURL != "" {
		// Download eagerly: the deps are consumed within one request.
		snapshot, meta, err := d.executor.downloadHubSnapshot(context.Background())
		if err == nil {
			if bootstrapErr := d.executor.bootstrapFromGeneration(snapshot, meta); bootstrapErr == nil {
				deps.HubSnapshot = snapshot
			}
		}
	}
	return deps
}

func (d agentSyncDeps) PushDeps(adapters []string) *commands.PushDeps {
	deps := &commands.PushDeps{}
	if d.executor.hubURL != "" {
		if adapters != nil {
			snapshot, _, err := d.executor.downloadHubSnapshot(context.Background())
			switch {
			case errors.Is(err, errNoHubSnapshot):
				deps.HubSnapshot = []core.AdapterSnapshot{}
			case err != nil:
				deps.HubErr = err
			default:
				deps.HubSnapshot = snapshot
			}
		}
		captured := adapters
		deps.HubSink = func(snapshot []core.AdapterSnapshot) (int, error) {
			if deps.HubErr != nil {
				return 0, deps.HubErr
			}
			return d.executor.uploadHubSnapshot(context.Background(), snapshot, captured)
		}
	}
	return deps
}
