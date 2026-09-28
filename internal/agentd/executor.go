package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/web"
)

type Executor interface {
	Status(ctx context.Context) (commands.StatusReport, error)
	Diff(ctx context.Context, params web.DiffParams) (string, error)
	Push(ctx context.Context, confirm bool) (commands.PushReport, error)
	Pull(ctx context.Context, confirm bool) (commands.PullReport, error)
}

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
	report, err := commands.RunStatus(commands.StatusOptions{HomerHome: e.homerHome})
	if err != nil {
		return commands.StatusReport{}, err
	}
	if err := contextError(ctx); err != nil {
		return commands.StatusReport{}, err
	}
	return report, nil
}

func (e *localExecutor) Diff(ctx context.Context, params web.DiffParams) (string, error) {
	if err := contextError(ctx); err != nil {
		return "", err
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

func (e *localExecutor) Push(ctx context.Context, confirm bool) (commands.PushReport, error) {
	if err := contextError(ctx); err != nil {
		return commands.PushReport{}, err
	}
	deps := &commands.PushDeps{UI: commands.HeadlessUI{}}
	if e.hubURL != "" {
		deps.HubSink = func(snapshot []core.AdapterSnapshot) error {
			return e.uploadHubSnapshot(ctx, snapshot)
		}
	}
	report := commands.RunPush(commands.PushOptions{HomerHome: e.homerHome, Yes: confirm}, deps)
	if err := contextError(ctx); err != nil {
		return commands.PushReport{}, err
	}
	return report, nil
}

func (e *localExecutor) Pull(ctx context.Context, confirm bool) (commands.PullReport, error) {
	if err := contextError(ctx); err != nil {
		return commands.PullReport{}, err
	}
	deps := &commands.PullDeps{UI: commands.HeadlessUI{}, NoFetch: true}
	if e.hubURL != "" {
		snapshot, err := e.downloadHubSnapshot(ctx)
		if err != nil {
			return commands.PullReport{}, err
		}
		deps.HubSnapshot = snapshot
	}
	report := commands.RunPull(commands.PullOptions{HomerHome: e.homerHome, Yes: confirm}, deps)
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
	return &localExecutor{homerHome: homerHome, hubURL: strings.TrimSpace(hubURL), credential: credential}
}

// hubSnapshotPayload mirrors the wire format of /api/snapshot.
type hubSnapshotPayload struct {
	Generation int                          `json:"generation"`
	HomerJSON  string                       `json:"homerJson"`
	Store      map[string]map[string]string `json:"store"`
}

// downloadHubSnapshot fetches the hub's current generation and converts it
// into the adapter-snapshot form the pull pipeline consumes.
func (e *localExecutor) downloadHubSnapshot(ctx context.Context) ([]core.AdapterSnapshot, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(e.hubURL, "/")+"/api/snapshot", nil)
	if err != nil {
		return nil, err
	}
	if e.credential != "" {
		request.Header.Set("Authorization", "Bearer "+e.credential)
	}
	response, err := e.hubClient().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("hub snapshot: %s %s", response.Status, strings.TrimSpace(string(body)))
	}
	var payload hubSnapshotPayload
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return snapshotsFromWire(payload.Store), nil
}

// uploadHubSnapshot pushes prepared snapshots to the hub.
func (e *localExecutor) uploadHubSnapshot(ctx context.Context, snapshot []core.AdapterSnapshot) error {
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
	body, err := json.Marshal(hubSnapshotPayload{Store: store})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(e.hubURL, "/")+"/api/snapshot", bytes.NewReader(body))
	if err != nil {
		return err
	}
	if e.credential != "" {
		request.Header.Set("Authorization", "Bearer "+e.credential)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := e.hubClient().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("hub upload: %s %s", response.Status, strings.TrimSpace(string(responseBody)))
	}
	return nil
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
