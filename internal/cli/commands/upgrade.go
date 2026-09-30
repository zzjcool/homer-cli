package commands

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzjcool/homer-cli/internal/core"
)

// UpgradeOptions carries the CLI flags for `homer upgrade`.
type UpgradeOptions struct {
	HomerHome string
	Home      string
	Connect   string // hub base URL override (default: agent.json's connectUrl)
	Out       io.Writer
	ErrOut    io.Writer
}

// UpgradeReport is the machine-readable result of a self-upgrade.
type UpgradeReport struct {
	OK        bool   `json:"ok"`
	Status    string `json:"status"`
	FromHub   string `json:"fromHub"`
	Binary    string `json:"binary"`
	SizeBytes int64  `json:"sizeBytes"`
	Hash      string `json:"hash"`
	Note      string `json:"note,omitempty"`
}

// RunUpgrade pulls the hub's own binary and atomically replaces this
// machine's executable — one command, no curl/token juggling (the user
// story: "为什么升级这么麻烦，能不能增加一个命令，我执行一下，自己就升级了").
func RunUpgrade(opts UpgradeOptions) UpgradeReport {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	write := func(format string, args ...any) {
		fmt.Fprintf(out, format+"\n", args...)
	}

	homerHome := opts.HomerHome
	if homerHome == "" {
		homerHome = opts.Home
	}
	paths := core.GetHomerPaths(func(key string) string {
		if key == "HOMER_HOME" && homerHome != "" {
			return homerHome
		}
		return os.Getenv(key)
	})

	// Credential: this machine's per-agent secret first (the enrolled
	// path — keys/hub-token holds a burned hr_ code after enrollment),
	// then the file token (hub-token deployments), then nothing.
	credential := ""
	hubURL := strings.TrimSpace(opts.Connect)
	if hubURL == "" {
		if cfg, ok := readAgentIdentity(paths.Home); ok {
			credential = cfg.secret
			hubURL = cfg.connectURL
		}
	}
	if credential == "" {
		if data, err := os.ReadFile(filepath.Join(paths.Home, "keys", "hub-token")); err == nil {
			credential = strings.TrimSpace(string(data))
		}
	}
	// No credential at all: a GitHub-release install (hub-less machine).
	// The caller falls back to the GitHub channel on this status.
	if credential == "" {
		return UpgradeReport{OK: false, Status: "no-credential", Note: "没有机器凭证（agent.json 的 secret 或 keys/hub-token）——非 hub 接入机器"}
	}
	if hubURL == "" {
		return UpgradeReport{OK: false, Status: "error", Note: "没有 hub 地址：请用 --connect <hub> 或先接入 agent"}
	}
	hubURL = strings.TrimRight(hubURL, "/")

	self, err := os.Executable()
	if err != nil {
		return UpgradeReport{OK: false, Status: "error", Note: "定位当前二进制失败: " + err.Error()}
	}
	self, _ = filepath.EvalSymlinks(self)

	write(">> 从 %s 下载新版本…", hubURL)
	request, err := http.NewRequest(http.MethodGet, hubURL+"/dl/homer", nil)
	if err != nil {
		return UpgradeReport{OK: false, Status: "error", Note: err.Error()}
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	client := &http.Client{Timeout: 5 * time.Minute}
	response, err := client.Do(request)
	if err != nil {
		return UpgradeReport{OK: false, Status: "error", Note: "下载失败: " + err.Error()}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return UpgradeReport{OK: false, Status: "error",
			Note: fmt.Sprintf("下载失败: HTTP %d %s", response.StatusCode, strings.TrimSpace(string(body)))}
	}

	// Stream to a sibling temp file, then rename over the running binary
	// (atomic on Linux; the running process keeps the old inode).
	tmp := self + ".upgrade"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return UpgradeReport{OK: false, Status: "error", Note: "写临时文件失败: " + err.Error()}
	}
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hasher), response.Body)
	closeErr := file.Close()
	if err != nil {
		_ = os.Remove(tmp)
		return UpgradeReport{OK: false, Status: "error", Note: "下载中断: " + err.Error()}
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return UpgradeReport{OK: false, Status: "error", Note: "写临时文件失败: " + closeErr.Error()}
	}
	if err := os.Rename(tmp, self); err != nil {
		_ = os.Remove(tmp)
		return UpgradeReport{OK: false, Status: "error", Note: "替换二进制失败: " + err.Error()}
	}

	write(">> 已升级: %s（%d 字节, sha256 %s）", self, size, hex.EncodeToString(hasher.Sum(nil))[:16])
	write(">> 重启 agent 后生效: pkill -f 'homer agent'; nohup %s agent --connect %s >> %s/agent.log 2>&1 &", self, hubURL, paths.Home)
	return UpgradeReport{
		OK: true, Status: "upgraded", FromHub: hubURL, Binary: self,
		SizeBytes: size, Hash: hex.EncodeToString(hasher.Sum(nil)),
	}
}

// agentIdentity is the minimal agent.json projection upgrade needs
// (agentd cannot be imported here: import cycle).
type agentIdentity struct {
	secret     string
	connectURL string
}

func readAgentIdentity(home string) (agentIdentity, bool) {
	data, err := os.ReadFile(filepath.Join(home, "agent.json"))
	if err != nil {
		return agentIdentity{}, false
	}
	var cfg struct {
		AgentSecret string `json:"agentSecret"`
		ConnectURL  string `json:"connectUrl"`
		HubURL      string `json:"hubUrl"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return agentIdentity{}, false
	}
	return agentIdentity{secret: cfg.AgentSecret, connectURL: cfg.ConnectURL}, true
}
