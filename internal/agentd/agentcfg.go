package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zzjcool/homer-cli/internal/hub"
)

// AgentConfig is the shape of agent.json. It deliberately contains only
// non-secret connection state; the bearer token remains in process/CLI
// configuration and is never written here.
type AgentConfig struct {
	AgentID      string `json:"agentId"`
	Mode         string `json:"mode"`                 // "listen" | "connect"
	ConnectURL   string `json:"connectUrl,omitempty"` // connect mode hub address
	HubURL       string `json:"hubUrl,omitempty"`     // listen mode registration address
	ListenAddr   string `json:"listenAddr,omitempty"`
	AdvertiseURL string `json:"advertiseUrl,omitempty"`
	// AgentSecret is this machine's per-agent credential issued at
	// enrollment (Tailscale-style). It replaces the shared hub token for
	// agent traffic; agent.json stays 0600 so the secret is at rest safe.
	AgentSecret string `json:"agentSecret,omitempty"`
	// EnrollCode carries a one-time enrollment code passed via the install
	// command; it is consumed on first successful registration and never
	// persisted.
	EnrollCode string `json:"-"`
}

func agentConfigPath(home string) string {
	return filepath.Join(home, "agent.json")
}

// LoadAgentConfig reads home/agent.json. A missing, unreadable, or malformed
// file is treated as no persisted configuration so callers can still perform
// the explicit first-time bootstrap.
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

// SaveAgentConfig atomically writes home/agent.json with mode 0600. The
// temporary file is created next to the destination so rename is atomic on
// the filesystem containing the agent home.
func SaveAgentConfig(home string, cfg AgentConfig) error {
	if strings.TrimSpace(home) == "" {
		return errors.New("agent home 不能为空")
	}
	if cfg.Mode != "" && cfg.Mode != "listen" && cfg.Mode != "connect" {
		return fmt.Errorf("agent 配置 mode 无效: %q", cfg.Mode)
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

// ResolveConfig fills empty runtime fields from the persisted agent config.
// Explicit CLI values always win. The persisted mode is used only when the
// caller did not explicitly choose a mode, preventing unrelated fields from a
// previous mode from making an explicit choice ambiguous.
func ResolveConfig(cfg Config) (Config, error) {
	persisted, ok := LoadAgentConfig(cfg.Home)
	if strings.TrimSpace(cfg.AgentID) == "" && ok {
		cfg.AgentID = persisted.AgentID
	}

	explicitListen := strings.TrimSpace(cfg.ListenAddr) != ""
	explicitConnect := strings.TrimSpace(cfg.ConnectURL) != ""
	if explicitListen && explicitConnect {
		return cfg, modeError(cfg)
	}

	switch {
	case explicitListen:
		// The explicit listen address establishes the mode. Only listen-mode
		// registration fields are eligible for completion.
		if strings.TrimSpace(cfg.HubURL) == "" && ok {
			cfg.HubURL = persisted.HubURL
		}
		if strings.TrimSpace(cfg.AdvertiseURL) == "" && ok {
			cfg.AdvertiseURL = persisted.AdvertiseURL
		}
	case explicitConnect:
		// The explicit connect URL establishes the mode. Do not import a
		// persisted listen address and accidentally turn this into both modes.
	default:
		if ok {
			switch persisted.Mode {
			case "listen":
				if strings.TrimSpace(cfg.ListenAddr) == "" {
					cfg.ListenAddr = persisted.ListenAddr
				}
				if strings.TrimSpace(cfg.HubURL) == "" {
					cfg.HubURL = persisted.HubURL
				}
				if strings.TrimSpace(cfg.AdvertiseURL) == "" {
					cfg.AdvertiseURL = persisted.AdvertiseURL
				}
			case "connect":
				if strings.TrimSpace(cfg.ConnectURL) == "" {
					cfg.ConnectURL = persisted.ConnectURL
				}
			case "":
				// Be tolerant of an older file that omitted mode when its
				// fields still identify exactly one mode.
				if strings.TrimSpace(persisted.ListenAddr) != "" && strings.TrimSpace(persisted.ConnectURL) == "" {
					if strings.TrimSpace(cfg.ListenAddr) == "" {
						cfg.ListenAddr = persisted.ListenAddr
					}
					if strings.TrimSpace(cfg.HubURL) == "" {
						cfg.HubURL = persisted.HubURL
					}
					if strings.TrimSpace(cfg.AdvertiseURL) == "" {
						cfg.AdvertiseURL = persisted.AdvertiseURL
					}
				} else if strings.TrimSpace(persisted.ConnectURL) != "" && strings.TrimSpace(persisted.ListenAddr) == "" {
					if strings.TrimSpace(cfg.ConnectURL) == "" {
						cfg.ConnectURL = persisted.ConnectURL
					}
				} else {
					return cfg, missingModeError()
				}
			default:
				return cfg, fmt.Errorf("agent 配置中的 mode 无效: %q；请先完成一次带参数注册或 homer agent --connect <url>", persisted.Mode)
			}
		}
	}

	if _, err := cfg.Mode(); err != nil {
		if !explicitListen && !explicitConnect {
			return cfg, missingModeError()
		}
		return cfg, err
	}
	return cfg, nil
}

func modeError(cfg Config) error {
	if _, err := cfg.Mode(); err != nil {
		return err
	}
	return errors.New("agent 模式无效")
}

func missingModeError() error {
	return errors.New("无法确定 agent 模式：请先完成一次带参数注册或 homer agent --connect <url>")
}

// DeriveAdvertiseURL turns a concrete IP listen address into the URL the hub
// can dial. IPv4 wildcard binds are resolved only when the machine has one
// unambiguous non-loopback IPv4 address; IPv6 wildcard binds and malformed
// addresses are intentionally rejected instead of guessed.
func DeriveAdvertiseURL(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil || strings.TrimSpace(port) == "" {
		return "", advertiseError(listenAddr, err)
	}
	if portNumber, portErr := strconv.Atoi(port); portErr != nil || portNumber < 0 || portNumber > 65535 {
		return "", advertiseError(listenAddr, portErr)
	}

	host = strings.TrimSpace(host)
	if host == "" {
		ip, err := discoverLanIPv4()
		if err != nil {
			return "", advertiseDiscoveryError(err)
		}
		if ip == nil {
			return "", advertiseError(listenAddr, errors.New("LanIPv4 返回空地址"))
		}
		return "http://" + net.JoinHostPort(ip.String(), port), nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", advertiseError(listenAddr, errors.New("监听地址必须使用 IP"))
	}
	if ip.IsUnspecified() {
		if ip.To4() == nil {
			return "", advertiseError(listenAddr, errors.New("IPv6 通配地址不支持自动推导"))
		}
		lanIP, err := discoverLanIPv4()
		if err != nil {
			return "", advertiseDiscoveryError(err)
		}
		if lanIP == nil {
			return "", advertiseError(listenAddr, errors.New("LanIPv4 返回空地址"))
		}
		return "http://" + net.JoinHostPort(lanIP.String(), port), nil
	}
	return "http://" + net.JoinHostPort(ip.String(), port), nil
}

func advertiseError(listenAddr string, cause error) error {
	if cause == nil {
		return fmt.Errorf("无法从监听地址 %q 推导 advertise；请用 --advertise <url>", listenAddr)
	}
	return fmt.Errorf("无法从监听地址 %q 推导 advertise：%v；请用 --advertise <url>", listenAddr, cause)
}

func advertiseDiscoveryError(err error) error {
	if strings.Contains(err.Error(), "--advertise <url>") {
		return err
	}
	return fmt.Errorf("%v；请用 --advertise <url>", err)
}

// discoverLanIPv4 is kept behind a package variable so tests in this package
// can replace the environment-dependent part without changing the pure
// address parsing cases. The production implementation is the hub's single-
// NIC resolver; this keeps advertise selection consistent with hub joins.
var discoverLanIPv4 = hub.LanIPv4

func agentConfigHome(cfg Config) string {
	return cfg.Home
}

func (d *Daemon) saveAgentConfig(mode string) error {
	if d == nil {
		return errors.New("nil agent daemon")
	}
	home := agentConfigHome(d.cfg)
	if strings.TrimSpace(home) == "" {
		// Config.Home is wired by the CLI. Embedded callers that predate the
		// persistence field may omit it; keep their daemon behavior unchanged.
		return nil
	}
	cfg := AgentConfig{
		AgentID:     d.cfg.AgentID,
		Mode:        mode,
		AgentSecret: d.cfg.AgentSecret,
	}
	switch mode {
	case "connect":
		cfg.ConnectURL = d.cfg.ConnectURL
	case "listen":
		cfg.HubURL = d.cfg.HubURL
		cfg.ListenAddr = d.cfg.ListenAddr
		cfg.AdvertiseURL = d.cfg.AdvertiseURL
	default:
		return fmt.Errorf("无法持久化未知 agent 模式 %q", mode)
	}
	return SaveAgentConfig(home, cfg)
}
