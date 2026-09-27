package web

import (
	"encoding/json"
	"errors"
	"net/http"
)

type errorBody struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Details []string `json:"details"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "内部错误", []string{err.Error()})
		return
	}
	writeJSONBytes(w, status, data)
}

func writeJSONBytes(w http.ResponseWriter, status int, data []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func writeError(w http.ResponseWriter, status int, code, message string, details []string) {
	if details == nil {
		details = []string{}
	}
	writeJSONBytes(w, status, mustMarshalJSON(errorEnvelope{Error: errorBody{
		Code:    code,
		Message: message,
		Details: details,
	}}))
}

func writeErrorValue(w http.ResponseWriter, err error) {
	status, code := errorStatusCode(err)
	message := errorMessage(code)
	details := []string{}
	if err != nil {
		details = []string{err.Error()}
	}
	writeError(w, status, code, message, details)
}

func errorStatusCode(err error) (int, string) {
	var agentErr *AgentError
	if errors.As(err, &agentErr) && agentErr != nil {
		status := agentErr.Status
		if status == 0 {
			status = agentStatusForCode(agentErr.Code)
		}
		if status == 0 {
			status = http.StatusInternalServerError
		}
		return status, agentErr.Code
	}
	switch {
	case err == nil:
		return http.StatusInternalServerError, "internal"
	case errors.Is(err, errUnauthorized):
		return http.StatusUnauthorized, "unauthorized"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

func agentStatusForCode(code string) int {
	switch code {
	case "agent-not-found":
		return http.StatusNotFound
	case "agent-unreachable":
		return http.StatusBadGateway
	case "agent-timeout":
		return http.StatusGatewayTimeout
	case "agents-disabled":
		return http.StatusNotImplemented
	default:
		return 0
	}
}

func errorMessage(code string) string {
	switch code {
	case "unauthorized":
		return "未授权：请提供有效的 Bearer token"
	case "not-found":
		return "请求的资源不存在"
	case "method-not-allowed":
		return "不支持的请求方法"
	case "not-initialized":
		return "请先运行 homer init 完成初始化"
	case "invalid-config":
		return "homer.json 配置无效"
	case "agents-disabled":
		return "多机 agent 视图未启用"
	case "agent-not-found":
		return "agent 不存在"
	case "agent-unreachable":
		return "agent 无法连接"
	case "agent-timeout":
		return "agent 请求超时"
	case "bad-request":
		return "请求参数无效"
	case "internal":
		return "内部错误"
	default:
		return code
	}
}

var errUnauthorized = errors.New("unauthorized")

func mustMarshalJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		return []byte(`{"error":{"code":"internal","message":"内部错误","details":[]}}`)
	}
	return data
}
