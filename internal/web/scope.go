package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gens"
	syncx "github.com/zzjcool/homer-cli/internal/sync"
)

// SyncScope is an explicit adapter selection for collect or dispatch.
// Explicit is false when the caller left the operation unrestricted.
type SyncScope struct {
	Explicit     bool
	Adapters     []string
	Overwrite    bool
	AllowSecrets bool
	// Unlocks are passwords for keys that ride with this dispatch.
	// They stay on the hub request and are not copied into an agent task.
	Unlocks []KeyUnlock
	// ApplyResolutions lets an explicit-adapter pull consume the recorded
	// decisions for those adapters (staged-resolution plan, D3). Hub-internal;
	// readSyncScope never parses this from a request body.
	ApplyResolutions bool
	// ClearResolutions asks a successful push/pull to clear the recorded
	// decisions for its adapters (record-and-execute-now fallback). Hub-internal.
	ClearResolutions bool
	// ConfigPolicy selects how to align a machine's adapter definition with
	// the center before dispatch. Unlike resolution fields, this is user input.
	ConfigPolicy map[string]string
	// CenterGeneration is the hub generation the task was built with. Hub-internal.
	CenterGeneration int
}

// KeyUnlock is one key the operator can open. The password is an input
// only and must not be written into logs or a sync report.
type KeyUnlock struct {
	ID       string
	Password string
}

var errNoCenterSnapshot = errors.New("中心还没有任何快照（先从一台机器收取）")

// readSyncScope reads an adapter selection from the query string or, if
// the query does not mention adapters, from a JSON body. A missing
// selection is unrestricted. An empty or invalid selection is an error.
func readSyncScope(r *http.Request) (SyncScope, error) {
	if r == nil {
		return SyncScope{}, nil
	}
	scope := SyncScope{}
	_, adaptersInQuery := r.URL.Query()["adapters"]
	if adaptersInQuery {
		ids, err := syncx.ParseAdapterIDs(r.URL.Query()["adapters"])
		if err != nil {
			return SyncScope{}, err
		}
		scope = SyncScope{Explicit: true, Adapters: ids}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return SyncScope{}, err
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 {
		var generic map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &generic); err != nil {
			return SyncScope{}, fmt.Errorf("请求体不是有效的 JSON")
		}
		if !adaptersInQuery {
			if raw, ok := generic["adapters"]; ok {
				if string(raw) == "null" {
					return SyncScope{}, fmt.Errorf("请选择至少一个适配器")
				}
				var ids []string
				if err := json.Unmarshal(raw, &ids); err != nil {
					return SyncScope{}, fmt.Errorf("adapters 必须是字符串数组")
				}
				parsed, err := syncx.ParseAdapterIDs(ids)
				if err != nil {
					return SyncScope{}, err
				}
				scope = SyncScope{Explicit: true, Adapters: parsed}
			}
			if raw, ok := generic["allowSecrets"]; ok {
				var allow bool
				if err := json.Unmarshal(raw, &allow); err != nil {
					return SyncScope{}, fmt.Errorf("allowSecrets 必须是布尔值")
				}
				scope.AllowSecrets = allow
			}
			if raw, ok := generic["unlocks"]; ok {
				unlocks, err := parseUnlocks(raw)
				if err != nil {
					return SyncScope{}, err
				}
				scope.Unlocks = unlocks
			}
		}
		if raw, ok := generic["configPolicy"]; ok {
			policy, err := parseConfigPolicy(raw)
			if err != nil {
				return SyncScope{}, err
			}
			scope.ConfigPolicy = policy
		}
	}
	if r.URL.Query().Get("overwrite") == "true" {
		scope.Overwrite = true
	}
	if r.URL.Query().Get("allowSecrets") == "true" {
		scope.AllowSecrets = true
	}
	return scope, nil
}

func parseConfigPolicy(raw json.RawMessage) (map[string]string, error) {
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil, fmt.Errorf("configPolicy 必须是对象")
	}
	var policy map[string]string
	if err := json.Unmarshal(raw, &policy); err != nil || policy == nil {
		return nil, fmt.Errorf("configPolicy 必须是适配器 ID 到 center 或 keep 的对象")
	}
	for adapterID, choice := range policy {
		if !core.ValidAdapterID(adapterID) {
			return nil, fmt.Errorf("configPolicy 包含无效的适配器 ID %q", adapterID)
		}
		if choice != "center" && choice != "keep" {
			return nil, fmt.Errorf("configPolicy[%s] 必须是 center 或 keep", adapterID)
		}
	}
	return policy, nil
}

func parseUnlocks(raw json.RawMessage) ([]KeyUnlock, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var rows []struct {
		ID       string `json:"id"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("unlocks 必须是数组")
	}
	out := make([]KeyUnlock, 0, len(rows))
	for _, row := range rows {
		id := strings.TrimSpace(row.ID)
		if id == "" {
			return nil, fmt.Errorf("unlocks 缺少密钥 id")
		}
		out = append(out, KeyUnlock{ID: id, Password: row.Password})
	}
	return out, nil
}

func (s *Server) requireCenterAdapters(ids []string) error {
	head, ok := gens.New(s.opts.HomerHome).Read()
	if !ok {
		return errNoCenterSnapshot
	}
	store, err := readGenerationStore(head.StoreDir)
	if err != nil {
		return err
	}
	missing := make([]string, 0)
	for _, id := range ids {
		if _, ok := store[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("中心没有适配器 %s", strings.Join(missing, "、"))
	}
	return nil
}

func (s *Server) centerAdapterIDs() ([]string, bool, error) {
	head, ok := gens.New(s.opts.HomerHome).Read()
	if !ok {
		return nil, false, nil
	}
	store, err := readGenerationStore(head.StoreDir)
	if err != nil {
		return nil, true, err
	}
	ids := make([]string, 0, len(store))
	for id := range store {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, true, nil
}

func readGenerationStore(dir string) (map[string]map[string]string, error) {
	store := map[string]map[string]string{}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("存储目录无效")
	}
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		segments := strings.SplitN(rel, "/", 2)
		if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if store[segments[0]] == nil {
			store[segments[0]] = map[string]string{}
		}
		store[segments[0]][segments[1]] = string(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return store, nil
}
