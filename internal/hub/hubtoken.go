package hub

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// hubTokenFilename lives under keys/, which requiredGitignoreLines already
// excludes from the synced repository — the same protection boundary as the
// age identity. Never put hub credentials into state.json: SaveState rewrites
// the whole file on every sync and load failures silently produce an empty
// state, which would rotate the token without anyone noticing.
const (
	hubTokenDirectory = "keys"
	hubTokenFilename  = "hub-token"
	hubTokenRandomHex = 32 // 64 hex chars, 256 bits of entropy
)

// EnsureHubToken resolves the hub token by priority:
//
//  1. flagToken (explicit --token): persisted to keys/hub-token — a
//     deliberate rotation.
//  2. HOMER_HUB_TOKEN: overrides this process only, never written back.
//  3. keys/hub-token contents.
//  4. freshly generated (reported via created=true) when the file is absent.
//
// The caller decides whether generation is allowed at all (loopback serves
// keep working without any token).
func EnsureHubToken(home, flagToken string) (token string, created bool, err error) {
	if trimmed := strings.TrimSpace(flagToken); trimmed != "" {
		if err := writeHubToken(home, trimmed); err != nil {
			return "", false, err
		}
		return trimmed, false, nil
	}
	if fromEnv := strings.TrimSpace(os.Getenv("HOMER_HUB_TOKEN")); fromEnv != "" {
		return fromEnv, false, nil
	}
	path := filepath.Join(home, hubTokenDirectory, hubTokenFilename)
	if data, readErr := os.ReadFile(path); readErr == nil {
		if token := strings.TrimSpace(string(data)); token != "" {
			return token, false, nil
		}
	}
	generated, genErr := generateHubToken()
	if genErr != nil {
		return "", false, genErr
	}
	if err := writeHubToken(home, generated); err != nil {
		return "", false, err
	}
	return generated, true, nil
}

// CreateHubToken returns the persisted hub token, generating one when the
// file is missing. force replaces it. The running hub keeps the previous
// value until it is restarted.
func CreateHubToken(home string, force bool) (token string, created bool, err error) {
	if !force {
		if existing, ok := ReadHubToken(home); ok {
			return existing, false, nil
		}
	}
	generated, genErr := generateHubToken()
	if genErr != nil {
		return "", false, genErr
	}
	if err := writeHubToken(home, generated); err != nil {
		return "", false, err
	}
	return generated, true, nil
}

// ReadHubToken returns the persisted token without generating one. Used by
// paths that must never rotate (e.g. --show-join).
func ReadHubToken(home string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(home, hubTokenDirectory, hubTokenFilename))
	if err != nil {
		return "", false
	}
	token := strings.TrimSpace(string(data))
	return token, token != ""
}

func generateHubToken() (string, error) {
	buf := make([]byte, hubTokenRandomHex)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成 hub token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func writeHubToken(home, token string) error {
	dir := filepath.Join(home, hubTokenDirectory)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建 keys 目录: %w", err)
	}
	// MkdirAll is subject to umask and does not touch an existing directory;
	// the explicit chmod keeps the invariant regardless.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("加固 keys 目录: %w", err)
	}
	path := filepath.Join(dir, hubTokenFilename)
	tmp, err := os.CreateTemp(dir, "."+hubTokenFilename+".tmp-")
	if err != nil {
		return fmt.Errorf("写入 hub token: %w", err)
	}
	tmpName := tmp.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("加固 hub token: %w", err)
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入 hub token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("写入 hub token: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("落盘 hub token: %w", err)
	}
	remove = false
	return nil
}

// LanIPv4 returns the address other machines should use to reach this
// host. Preference order: the default route's source IP (the unambiguous
// outbound NIC — docker0/bridges never carry the default route), then a
// unique global IPv4, and an error otherwise: guessing between multiple
// NICs silently registers an address the hub cannot reach, so the
// multi-homed case is reported to the caller.
func LanIPv4() (net.IP, error) {
	if ip := defaultRouteIPv4(); ip != nil {
		return ip, nil
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("枚举网卡: %w", err)
	}
	var candidates []net.IP
	for _, item := range interfaces {
		if item.Flags&net.FlagLoopback != 0 || item.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, addrErr := item.Addrs()
		if addrErr != nil {
			continue
		}
		for _, address := range addresses {
			ipNet, ok := address.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			candidates = append(candidates, ip)
		}
	}
	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return nil, fmt.Errorf("未找到全局 IPv4 地址；请手动填写本机地址")
	default:
		return nil, fmt.Errorf("本机有 %d 个全局 IPv4 地址（%s）；请手动填写本机地址", len(candidates), joinIPs(candidates))
	}
}

// defaultRouteIPv4 asks the OS for the default route's source address. The
// connected-UDP trick never sends a packet: it only makes the routing
// table pick an interface, then we read the chosen local source IP.
func defaultRouteIPv4() net.IP {
	conn, err := net.Dial("udp4", "192.0.2.1:80") // TEST-NET, never routed
	if err != nil {
		return nil
	}
	defer conn.Close()
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || local.IP == nil || local.IP.IsLoopback() || local.IP.IsUnspecified() {
		return nil
	}
	return local.IP.To4()
}

func joinIPs(ips []net.IP) string {
	parts := make([]string, len(ips))
	for index, ip := range ips {
		parts[index] = ip.String()
	}
	return strings.Join(parts, ", ")
}
