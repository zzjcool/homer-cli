// Package hubclient is the homer CLI's connection to a running hub.
// Like the Docker client, it does not do the work itself: every call is an
// HTTP request to the daemon selected by HOMER_HOST or --host.
package hubclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultHost = "http://127.0.0.1:7760"

// Endpoint resolves the hub address the way Docker resolves DOCKER_HOST.
// An empty flag falls back to HOMER_HOST, then the local hub.
// tcp://host:port is accepted as http://host:port.
func Endpoint(flag string) string {
	raw := strings.TrimSpace(flag)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("HOMER_HOST"))
	}
	if raw == "" {
		raw = defaultHost
	}
	raw = strings.TrimRight(raw, "/")
	switch {
	case strings.HasPrefix(raw, "tcp://"):
		raw = "http://" + strings.TrimPrefix(raw, "tcp://")
	case strings.HasPrefix(raw, "http://"), strings.HasPrefix(raw, "https://"):
	default:
		raw = "http://" + raw
	}
	return raw
}

// Token is the hub credential: --token, then HOMER_HUB_TOKEN, then the
// token file written by `homer serve`.
func Token(flag, home string) string {
	if value := strings.TrimSpace(flag); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("HOMER_HUB_TOKEN")); value != "" {
		return value
	}
	path := filepath.Join(resolveHome(home), "keys", "hub-token")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func resolveHome(home string) string {
	if strings.TrimSpace(home) != "" {
		return strings.TrimSpace(home)
	}
	if value := strings.TrimSpace(os.Getenv("HOMER_HOME")); value != "" {
		return value
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		return ".homer"
	}
	return filepath.Join(dir, ".homer")
}

// LocalAgentID is this machine's id, persisted when it enrolled.
func LocalAgentID(home string) (string, error) {
	path := filepath.Join(resolveHome(home), "agent.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("这台机器还没有接入中心（没有 %s）。用 --id 指定机器，或先运行安装命令", path)
	}
	var cfg struct {
		AgentID string `json:"agentId"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil || strings.TrimSpace(cfg.AgentID) == "" {
		return "", fmt.Errorf("无法从 %s 读出机器 id。用 --id 指定", path)
	}
	return strings.TrimSpace(cfg.AgentID), nil
}

// Client talks to one hub.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New builds a client. A nil transport uses a client with no short timeout;
// callers pass Timeout per request because plugin installs take minutes.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{},
	}
}

// Do sends one API request. A refused connection is reported as a hub-down
// error, the same idea as Docker's "is the daemon running?".
func (c *Client) Do(method, path string, body any, timeout time.Duration) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequest(method, c.BaseURL+path, reader)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{}
	}
	if timeout > 0 {
		cloned := *client
		cloned.Timeout = timeout
		client = &cloned
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("无法连接中心 %s。请确认 homer serve 已在运行，或用 HOMER_HOST / --host 指定中心。\n%w", c.BaseURL, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, response.StatusCode, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		return payload, response.StatusCode, fmt.Errorf("中心 %s 拒绝了这个客户端。设置 HOMER_HUB_TOKEN，或确认本机 keys/hub-token 与中心一致", c.BaseURL)
	}
	return payload, response.StatusCode, nil
}
