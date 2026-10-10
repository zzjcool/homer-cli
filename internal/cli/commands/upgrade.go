package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/zzjcool/homer-cli/internal/core"
)

// UpgradeDownloadTimeout bounds one hub-channel download. Both the
// interactive and daemon channels use it.
const UpgradeDownloadTimeout = 5 * time.Minute

// minUpgradeBytes rejects a payload that is far too small to be a real
// homer binary. A valid magic with a truncated body would brick the
// machine exactly like a wrong-platform binary, so size is a platform
// safety check too.
const minUpgradeBytes = 1 << 20 // 1 MiB

// executableHeaderBytes is how much of the payload is read up front for
// the format-magic and CPU-architecture checks: 20 bytes covers the ELF
// e_machine field (offset 18) and the Mach-O cputype field (offset 4).
const executableHeaderBytes = 20

// UpgradeOptions carries the CLI flags for `homer upgrade`.
type UpgradeOptions struct {
	HomerHome string
	Home      string
	HubURL    string // hub base URL override (default: agent.json's hubUrl)
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

// RunUpgrade pulls a hub-served binary and atomically replaces this
// machine's executable — one command, no curl/token juggling (the user
// story: "为什么升级这么麻烦，能不能增加一个命令，我执行一下，自己就升级了").
//
// Platform safety (2026-10-10 Mac incident follow-up): the request always
// names this machine's GOOS/GOARCH, the response's X-Homer-Platform marker
// is verified, and the payload's magic bytes must match the platform. A hub
// that can only serve its own binary (the incident shape) fails loudly
// instead of bricking the machine.
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
	hubURL := strings.TrimSpace(opts.HubURL)
	if hubURL == "" {
		if cfg, ok := readAgentIdentity(paths.Home); ok {
			credential = cfg.secret
			hubURL = cfg.hubURL
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
		return UpgradeReport{OK: false, Status: "error", Note: "没有 hub 地址：请用 --hub <hub> 或先接入 agent"}
	}
	hubURL = strings.TrimRight(hubURL, "/")

	self, err := os.Executable()
	if err != nil {
		return UpgradeReport{OK: false, Status: "error", Note: "定位当前二进制失败: " + err.Error()}
	}
	self, _ = filepath.EvalSymlinks(self)

	write(">> 从 %s 下载新版本（本机平台 %s/%s）…", hubURL, runtime.GOOS, runtime.GOARCH)
	// The interactive channel gets the same bounded client the daemon
	// path uses; http.DefaultClient has no timeout and would hang forever
	// on a stuck tunnel.
	report := DownloadAndReplace(context.Background(), &http.Client{Timeout: UpgradeDownloadTimeout}, hubURL, credential, self)
	if !report.OK {
		return report
	}
	write("已升级: %s（%d 字节, sha256 %s）", self, report.SizeBytes, report.Hash[:min(len(report.Hash), 16)])
	report.FromHub = hubURL
	return report
}

// DownloadAndReplace is the shared hub-channel upgrade core: fetch
// /dl/homer with the machine's platform named in the query, verify the
// platform marker header, the payload magic AND architecture, then
// atomically replace target. Interactive `homer upgrade` and the agent
// daemon both funnel through it so the safety checks cannot drift apart
// again. ctx bounds and cancels the download (daemon shutdown or task
// cancellation aborts the replace instead of finishing it in the dark).
//
// Status values: "upgraded" on success; "platform-mismatch" when the hub
// served a binary for another platform or architecture (the machine is
// left untouched); "error" for transport/refusal failures.
func DownloadAndReplace(ctx context.Context, client *http.Client, hubURL, credential, self string) UpgradeReport {
	if client == nil {
		client = &http.Client{Timeout: UpgradeDownloadTimeout}
	}
	hubURL = strings.TrimRight(strings.TrimSpace(hubURL), "/")
	if hubURL == "" {
		return UpgradeReport{OK: false, Status: "error", Note: "没有 hub 地址：请用 --hub <hub> 或先接入 agent"}
	}
	if self == "" {
		return UpgradeReport{OK: false, Status: "error", Note: "没有目标二进制路径"}
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		hubURL+"/dl/homer?goos="+runtime.GOOS+"&goarch="+runtime.GOARCH, nil)
	if err != nil {
		return UpgradeReport{OK: false, Status: "error", Note: err.Error()}
	}
	request.Header.Set("Authorization", "Bearer "+credential)
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
	// Trust but verify (defense 1): the platform marker the hub attaches to
	// release-proxied binaries. Absent means "the hub's own platform" —
	// a pre-incident hub that ignores the query params and streams itself.
	if marker := response.Header.Get("X-Homer-Platform"); marker != "" && marker != runtime.GOOS+"/"+runtime.GOARCH {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return UpgradeReport{OK: false, Status: "platform-mismatch",
			Note: "hub 提供的是 " + marker + " 的 homer，这台机器是 " + runtime.GOOS + "/" + runtime.GOARCH + "，已拒绝替换: " + strings.TrimSpace(string(body))}
	}
	// Defense 2: even without a marker (old hub that ignores the platform
	// query and serves itself), the payload itself must be executable on
	// this machine — format magic first, then the CPU architecture inside
	// the header. A linux/amd64 ELF reaching darwin/arm64 dies here, and
	// so does an amd64 binary reaching an arm64 machine of the same OS
	// (the incident shape with a different axis).
	header := make([]byte, executableHeaderBytes)
	if _, err := io.ReadFull(response.Body, header); err != nil {
		return UpgradeReport{OK: false, Status: "error",
			Note: fmt.Sprintf("下载内容不完整（读取可执行头失败: %s）", err.Error())}
	}
	if platformErr := checkExecutable(runtime.GOOS, runtime.GOARCH, header); platformErr != nil {
		return UpgradeReport{OK: false, Status: "platform-mismatch",
			Note: "hub 提供的二进制不能在本机执行（" + platformErr.Error() + "），已拒绝替换。请让 hub 升级到最新版本，或从 GitHub Releases 安装对应平台版本"}
	}
	// A valid magic with nothing behind it would still brick the machine;
	// a real homer binary is several MB. Anything under 1 MiB is a broken
	// or hostile payload, not an upgrade.
	if response.ContentLength >= 0 && response.ContentLength < minUpgradeBytes {
		return UpgradeReport{OK: false, Status: "error",
			Note: fmt.Sprintf("下载内容只有 %d 字节，不是完整的 homer 二进制（至少 %d 字节），已拒绝替换", response.ContentLength, minUpgradeBytes)}
	}

	// Stream to a sibling temp file, then rename over the running binary
	// (atomic on both darwin and linux; the running process keeps the old
	// inode on linux, and on darwin a busy target still allows rename).
	tmp := self + ".upgrade"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return UpgradeReport{OK: false, Status: "error", Note: "写临时文件失败: " + err.Error()}
	}
	hasher := sha256.New()
	// The header bytes already consumed from the body above lead both the
	// file and the hash so the written binary is byte-identical to what
	// the hub served.
	if _, err := file.Write(header); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return UpgradeReport{OK: false, Status: "error", Note: "写临时文件失败: " + err.Error()}
	}
	_, _ = hasher.Write(header)
	size, copyErr := io.Copy(io.MultiWriter(file, hasher), response.Body)
	closeErr := file.Close()
	size += int64(len(header))
	if copyErr != nil {
		_ = os.Remove(tmp)
		return UpgradeReport{OK: false, Status: "error", Note: "下载中断: " + copyErr.Error()}
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return UpgradeReport{OK: false, Status: "error", Note: "写临时文件失败: " + closeErr.Error()}
	}
	if response.ContentLength >= 0 && size != response.ContentLength {
		_ = os.Remove(tmp)
		return UpgradeReport{OK: false, Status: "error",
			Note: fmt.Sprintf("下载不完整（收到 %d 字节，应为 %d 字节）", size, response.ContentLength)}
	}
	if size < minUpgradeBytes {
		_ = os.Remove(tmp)
		return UpgradeReport{OK: false, Status: "error",
			Note: fmt.Sprintf("下载内容只有 %d 字节，不是完整的 homer 二进制，已拒绝替换", size)}
	}
	// The last platform check: a cancelled or expired task must not swap
	// the binary out from under a hub that already reported a timeout.
	if ctxErr := ctx.Err(); ctxErr != nil {
		_ = os.Remove(tmp)
		return UpgradeReport{OK: false, Status: "error", Note: "升级任务已取消，保持原二进制: " + ctxErr.Error()}
	}
	if err := os.Rename(tmp, self); err != nil {
		_ = os.Remove(tmp)
		return UpgradeReport{OK: false, Status: "error", Note: "替换二进制失败: " + err.Error()}
	}
	hash := hex.EncodeToString(hasher.Sum(nil))
	return UpgradeReport{
		OK: true, Status: "upgraded", FromHub: hubURL, Binary: self,
		SizeBytes: size, Hash: hash,
	}
}

// checkExecutable verifies a payload header against the executable formats
// the named platform can run: the format magic first, then the CPU
// architecture encoded in the header (ELF e_machine at offset 18; Mach-O
// cputype at offset 4, little-endian on LE targets). This closes the
// same-OS/different-arch hole a marker-less old hub would otherwise leave
// (a linux/amd64 ELF would pass a bare format check on linux/arm64).
func checkExecutable(goos, goarch string, header []byte) error {
	if len(header) < executableHeaderBytes {
		return fmt.Errorf("可执行头不完整（%d 字节）", len(header))
	}
	switch goos {
	case "darwin":
		switch {
		case bytes.Equal(header[:4], []byte{0xcf, 0xfa, 0xed, 0xfe}), // Mach-O 64-bit LE
			bytes.Equal(header[:4], []byte{0xce, 0xfa, 0xed, 0xfe}), // Mach-O 32-bit LE
			bytes.Equal(header[:4], []byte{0xfe, 0xed, 0xfa, 0xce}), // Mach-O 32-bit BE
			bytes.Equal(header[:4], []byte{0xfe, 0xed, 0xfa, 0xcf}): // Mach-O 64-bit BE
			// Thin binaries: read cputype (offset 4) and validate against
			// GOARCH. Big-endian binaries from Go builds do not exist today;
			// treating them as unmatchable is safe and honest.
			cputype := binary.LittleEndian.Uint32(header[4:8])
			want, ok := machoCPU[goarch]
			if !ok {
				return nil // unknown GOARCH: the format check is all we have
			}
			if cputype != want {
				return fmt.Errorf("架构不匹配（Mach-O cputype 0x%x，本机 %s）", cputype, goarch)
			}
			return nil
		case bytes.Equal(header[:4], []byte{0xca, 0xfe, 0xba, 0xbe}), // fat 32
			bytes.Equal(header[:4], []byte{0xca, 0xfe, 0xba, 0xbf}): // fat 64
			// Fat/universal binaries: the per-slice architectures live past
			// the fat header, beyond the 20 bytes read here. Accept the
			// container format; goreleaser does not publish universal builds
			// today, so this is future-proofing, not a live path.
			return nil
		default:
			return fmt.Errorf("内容不是 Mach-O 可执行文件")
		}
	case "linux":
		if !bytes.Equal(header[:4], []byte{0x7f, 'E', 'L', 'F'}) {
			return fmt.Errorf("内容不是 ELF 可执行文件")
		}
		// EI_DATA at offset 5: 1 = little-endian (all Go linux targets).
		if header[5] != 1 {
			return fmt.Errorf("不支持的 ELF 字节序")
		}
		machine := binary.LittleEndian.Uint16(header[18:20])
		want, ok := elfMachine[goarch]
		if !ok {
			return nil // unknown GOARCH: the format check is all we have
		}
		if machine != want {
			return fmt.Errorf("架构不匹配（ELF e_machine 0x%x，本机 %s）", machine, goarch)
		}
		return nil
	default:
		return nil // unknown platform: no header to enforce
	}
}

// machoCPU maps GOARCH to the Mach-O cputype constant (macho.TypeExec
// thin binaries; LE only — BE Go builds do not exist).
var machoCPU = map[string]uint32{
	"amd64": 0x01000007, // x86_64
	"arm64": 0x0100000c, // arm64
}

// elfMachine maps GOARCH to the ELF e_machine constant.
var elfMachine = map[string]uint16{
	"amd64": 0x3e,
	"arm64": 0xb7,
}

// agentIdentity is the minimal agent.json projection upgrade needs
// (agentd cannot be imported here: import cycle).
type agentIdentity struct {
	secret string
	hubURL string
}

func readAgentIdentity(home string) (agentIdentity, bool) {
	data, err := os.ReadFile(filepath.Join(home, "agent.json"))
	if err != nil {
		return agentIdentity{}, false
	}
	var cfg struct {
		AgentSecret string `json:"agentSecret"`
		HubURL      string `json:"hubUrl"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return agentIdentity{}, false
	}
	return agentIdentity{secret: cfg.AgentSecret, hubURL: cfg.HubURL}, true
}
