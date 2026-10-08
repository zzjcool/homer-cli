package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AgentConfig is the complete persisted connection configuration. Credentials
// are stored only in agent.json, which SaveAgentConfig creates with mode 0600.
type AgentConfig struct {
	AgentID     string `json:"agentId"`
	HubURL      string `json:"hubUrl,omitempty"`
	DataURL     string `json:"dataUrl,omitempty"`
	AgentSecret string `json:"agentSecret,omitempty"`
}

func agentConfigPath(home string) string {
	return filepath.Join(home, "agent.json")
}

// LoadAgentConfig treats a missing, unreadable, or malformed file as absent.
// encoding/json ignores legacy fields such as mode and connectUrl, allowing an
// old file to be read without retaining those removed configuration options.
func LoadAgentConfig(home string) (AgentConfig, bool) {
	if strings.TrimSpace(home) == "" {
		return AgentConfig{}, false
	}
	data, err := os.ReadFile(agentConfigPath(home))
	if err != nil {
		return AgentConfig{}, false
	}
	var cfg *AgentConfig
	if err := json.Unmarshal(data, &cfg); err != nil || cfg == nil {
		return AgentConfig{}, false
	}
	return *cfg, true
}

// SaveAgentConfig atomically writes agent.json with mode 0600.
func SaveAgentConfig(home string, cfg AgentConfig) error {
	if strings.TrimSpace(home) == "" {
		return errors.New("agent home 不能为空")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("创建 agent home: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("编码 agent 配置: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(home, ".agent.json.tmp-")
	if err != nil {
		return fmt.Errorf("创建 agent 配置临时文件: %w", err)
	}
	tmpName := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("设置 agent 配置权限: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入 agent 配置: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("刷新 agent 配置: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭 agent 配置: %w", err)
	}
	if err := os.Rename(tmpName, agentConfigPath(home)); err != nil {
		return fmt.Errorf("落盘 agent 配置: %w", err)
	}
	removeTemp = false
	return nil
}

// ResolveConfig fills missing fields from agent.json. Explicit Config values
// take precedence; an old connectUrl is ignored and cannot satisfy HubURL.
func ResolveConfig(cfg Config) (Config, error) {
	persisted, ok := LoadAgentConfig(cfg.Home)
	explicitToken := strings.TrimSpace(cfg.Token) != ""
	if explicitToken && ok && strings.TrimSpace(cfg.AgentSecret) == strings.TrimSpace(persisted.AgentSecret) {
		// The CLI supplies agentSecret from agent.json alongside explicit token
		// flags. An explicit token must win that persisted value.
		cfg.AgentSecret = ""
	}
	explicitCredential := strings.TrimSpace(cfg.AgentSecret) != "" ||
		explicitToken || strings.TrimSpace(cfg.EnrollCode) != ""
	if ok {
		if strings.TrimSpace(cfg.AgentID) == "" {
			cfg.AgentID = persisted.AgentID
		}
		if strings.TrimSpace(cfg.HubURL) == "" {
			cfg.HubURL = persisted.HubURL
		}
		if strings.TrimSpace(cfg.DataURL) == "" {
			cfg.DataURL = persisted.DataURL
		}
		if strings.TrimSpace(cfg.AgentSecret) == "" && !explicitCredential {
			cfg.AgentSecret = persisted.AgentSecret
		}
	}
	if strings.TrimSpace(cfg.HubURL) == "" {
		return cfg, errors.New("缺少 HubURL：请使用 homer agent --hub <url> 或设置 agent.json 的 hubUrl")
	}
	if strings.TrimSpace(cfg.AgentID) == "" {
		cfg.AgentID = DefaultAgentID()
	}
	return cfg, nil
}

func (d *Daemon) saveAgentConfig() error {
	if d == nil {
		return errors.New("nil agent daemon")
	}
	secret := d.persistedAgentSecret()
	if secret == "" {
		// An explicit shared token may override a previously saved secret for
		// this run. Preserve that secret on disk so a later run without an
		// override can still use the enrolled identity.
		if persisted, ok := LoadAgentConfig(agentConfigHome(d.cfg)); ok {
			secret = persisted.AgentSecret
		}
	}
	return SaveAgentConfig(agentConfigHome(d.cfg), AgentConfig{
		AgentID:     d.cfg.AgentID,
		HubURL:      strings.TrimSpace(d.cfg.HubURL),
		DataURL:     strings.TrimSpace(d.cfg.DataURL),
		AgentSecret: secret,
	})
}

func agentConfigHome(cfg Config) string { return cfg.Home }
