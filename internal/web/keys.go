package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
