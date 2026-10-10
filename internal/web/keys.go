package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/zzjcool/homer-cli/internal/keyring"
)

const keyBodyLimit = 64 << 10

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/keys":
		switch r.Method {
		case http.MethodGet:
			s.writeKey(w, keyring.Command{Action: "list"})
		case http.MethodPost:
			s.writeKeyCommand(w, r, "create")
		default:
			writeMethodNotAllowed(w)
		}
	case "/api/keys/encrypt":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		s.writeKeyCommand(w, r, "encrypt")
	case "/api/keys/unlock":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		s.writeKeyCommand(w, r, "unlock")
	case "/api/keys/password":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		s.writeKeyCommand(w, r, "passwd")
	case "/api/keys/op":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		cmd, err := readKeyCommand(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad-request", err.Error(), nil)
			return
		}
		s.writeKey(w, cmd)
	default:
		writeError(w, http.StatusNotFound, "not-found", "请求的资源不存在", nil)
	}
}

func (s *Server) writeKeyCommand(w http.ResponseWriter, r *http.Request, action string) {
	cmd, err := readKeyCommand(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error(), nil)
		return
	}
	cmd.Action = action
	s.writeKey(w, cmd)
}

func (s *Server) writeKey(w http.ResponseWriter, cmd keyring.Command) {
	writeMutex.Lock()
	defer writeMutex.Unlock()
	result := keyring.Apply(s.opts.HomerHome, cmd)
	status := http.StatusOK
	if !result.OK {
		status = http.StatusUnprocessableEntity
		if result.Status == "bad-action" {
			status = http.StatusBadRequest
		}
	}
	writeJSON(w, status, result)
}

// KeyAgentSource is the hub dispatcher. Consoles without it cannot operate a remote machine's keyring.
type KeyAgentSource interface {
	AgentKey(ctx context.Context, agentID string, cmd keyring.Command) (json.RawMessage, error)
}

func (s *Server) handleAgentKeys(w http.ResponseWriter, r *http.Request, agentID string) {
	actor, ok := s.opts.Agents.(KeyAgentSource)
	if !ok {
		writeError(w, http.StatusNotImplemented, "agents-disabled", "这台 hub 不能在机器上操作密钥", nil)
		return
	}
	cmd, err := readKeyCommand(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error(), nil)
		return
	}
	raw, err := actor.AgentKey(r.Context(), agentID, cmd)
	if err != nil {
		writeErrorValue(w, err)
		return
	}
	var result keyring.Result
	if json.Unmarshal(raw, &result) != nil {
		writeError(w, http.StatusInternalServerError, "internal", "agent 返回了无效报告", nil)
		return
	}
	status := http.StatusOK
	if !result.OK {
		status = http.StatusUnprocessableEntity
	}
	writeJSONBytes(w, status, raw)
}

func remoteReportOK(raw json.RawMessage) bool {
	var report struct {
		OK bool `json:"ok"`
	}
	return len(raw) > 0 && json.Unmarshal(raw, &report) == nil && report.OK
}

func scopeHasKeyring(adapters []string) bool {
	for _, id := range adapters {
		if id == "keyring" {
			return true
		}
	}
	return false
}

// boundKeyGaps lists the adapters in this dispatch that have a key bound to
// them while the dispatch does not carry the keyring. Such a dispatch would
// write the adapter's ordinary files and leave its credential file behind: the
// machine looks synced but cannot authenticate, and nothing on it says why.
// So the dispatch is refused instead of half applied.
//
// An unrestricted dispatch (no adapter list) covers every adapter in the
// center, so every adapter that has a key bound counts as selected.
func boundKeyGaps(home string, scope SyncScope) []string {
	if scopeHasKeyring(scope.Adapters) {
		return nil
	}
	listed := keyring.Apply(home, keyring.Command{Action: "list"})
	if !listed.OK {
		return nil
	}
	selected := map[string]struct{}{}
	for _, id := range scope.Adapters {
		if id != "" {
			selected[id] = struct{}{}
		}
	}
	seen := map[string]struct{}{}
	gaps := make([]string, 0)
	for _, key := range listed.Keys {
		for _, file := range key.Files {
			if file.Adapter == "" {
				continue
			}
			if scope.Explicit {
				if _, ok := selected[file.Adapter]; !ok {
					continue
				}
			}
			if _, dup := seen[file.Adapter]; dup {
				continue
			}
			seen[file.Adapter] = struct{}{}
			gaps = append(gaps, file.Adapter)
		}
	}
	sort.Strings(gaps)
	return gaps
}

// refuseBoundKeyGap writes the refusal and reports true when the dispatch
// would leave a bound key behind. Every path that writes another machine from
// the center goes through this one gate, so the console, the CLI and a script
// cannot disagree about it.
func (s *Server) refuseBoundKeyGap(w http.ResponseWriter, scope SyncScope) bool {
	gaps := boundKeyGaps(s.opts.HomerHome, scope)
	if len(gaps) == 0 {
		return false
	}
	messages := make([]string, 0, len(gaps))
	for _, adapter := range gaps {
		messages = append(messages, adapter+" 绑定了密钥，密钥必须连同配置一起下发并解开；这次没有带上密钥，所以什么都没有写。请把 "+adapter+" 和密钥一起下发（控制台勾选 "+adapter+" 即可自动带上），并填口令。")
	}
	writeError(w, http.StatusUnprocessableEntity, "unlock-required", "下发带了密钥，要先解开。", messages)
	return true
}

// dispatchUnlockKeys are the keys whose plaintext must be written on the
// target. A dispatch that does not carry the keyring has nothing to open.
func dispatchUnlockKeys(home string, scope SyncScope) []keyring.Summary {
	if !scopeHasKeyring(scope.Adapters) {
		return nil
	}
	listed := keyring.Apply(home, keyring.Command{Action: "list"})
	if !listed.OK {
		return nil
	}
	selected := map[string]struct{}{}
	for _, id := range scope.Adapters {
		if id == "" || id == "keyring" {
			continue
		}
		selected[id] = struct{}{}
	}
	out := make([]keyring.Summary, 0)
	for _, key := range listed.Keys {
		if keyFollowsSelection(key, selected) {
			out = append(out, key)
		}
	}
	return out
}

func keyFollowsSelection(key keyring.Summary, selected map[string]struct{}) bool {
	if len(selected) == 0 {
		return len(key.Files) > 0
	}
	for _, file := range key.Files {
		if _, ok := selected[file.Adapter]; ok {
			return true
		}
	}
	return false
}

// dispatchUnlockErrors refuses a dispatch whose keys cannot be opened.
// The pull has not started. Messages never include the password.
func (s *Server) dispatchUnlockErrors(scope SyncScope) []string {
	keys := dispatchUnlockKeys(s.opts.HomerHome, scope)
	if len(keys) == 0 {
		return nil
	}
	if _, ok := s.opts.Agents.(KeyAgentSource); !ok {
		return []string{"这台 hub 不能在机器上解开密钥"}
	}
	provided := map[string]string{}
	for _, unlock := range scope.Unlocks {
		provided[unlock.ID] = unlock.Password
	}
	messages := make([]string, 0)
	for _, key := range keys {
		name := key.Name
		if name == "" {
			name = key.ID
		}
		password, ok := provided[key.ID]
		if !ok || strings.TrimSpace(password) == "" {
			messages = append(messages, "请先填写「"+name+"」的口令。口令能解开才会下发。")
			continue
		}
		result := keyring.Apply(s.opts.HomerHome, keyring.Command{Action: "check", ID: key.ID, Password: password})
		if result.OK {
			continue
		}
		line := name + "：口令不正确"
		if result.Status != "bad-password" && len(result.Errors) > 0 {
			line = name + "：" + result.Errors[0]
		}
		messages = append(messages, line)
	}
	return messages
}

func (s *Server) unlockDispatched(ctx context.Context, agentID string, scope SyncScope) []string {
	keys := dispatchUnlockKeys(s.opts.HomerHome, scope)
	if len(keys) == 0 {
		return nil
	}
	actor, ok := s.opts.Agents.(KeyAgentSource)
	if !ok {
		return []string{"这台 hub 不能在机器上解开密钥"}
	}
	provided := map[string]string{}
	for _, unlock := range scope.Unlocks {
		provided[unlock.ID] = unlock.Password
	}
	messages := make([]string, 0)
	for _, key := range keys {
		name := key.Name
		if name == "" {
			name = key.ID
		}
		raw, err := actor.AgentKey(ctx, agentID, keyring.Command{Action: "unlock", ID: key.ID, Password: provided[key.ID]})
		if err != nil {
			messages = append(messages, name+" 没有解开")
			continue
		}
		var result keyring.Result
		if json.Unmarshal(raw, &result) != nil || !result.OK {
			line := name + " 没有解开"
			if len(result.Errors) > 0 {
				line = name + "：" + result.Errors[0]
			}
			messages = append(messages, line)
		}
	}
	return messages
}

func readKeyCommand(r *http.Request) (keyring.Command, error) {
	if r.Body == nil {
		return keyring.Command{}, errors.New("请求体为空")
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, keyBodyLimit+1))
	if err != nil {
		return keyring.Command{}, errors.New("读取请求体失败")
	}
	if len(body) > keyBodyLimit {
		return keyring.Command{}, errors.New("请求体过大")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return keyring.Command{}, errors.New("请求体为空")
	}
	var cmd keyring.Command
	if err := json.Unmarshal(body, &cmd); err != nil {
		return keyring.Command{}, errors.New("密钥请求不是合法 JSON")
	}
	cmd.Password = strings.TrimSpace(cmd.Password)
	return cmd, nil
}
