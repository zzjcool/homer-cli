package pair

import (
	"errors"
	"os/exec"
)

// ErrNoTailcat is returned when the optional tailcat transport CLI is absent.
var ErrNoTailcat = errors.New("tailcat CLI not found in PATH")

// TailcatInstallHint is the fixed fallback guidance shown when pair cannot
// start because its optional transport dependency is unavailable.
const TailcatInstallHint = "未找到 tailcat（密钥配对的传输层）。\n安装: 参考官方 INSTALL https://github.com/tailscale/tailcat（静态二进制 / brew / 包管理器）\n或继续使用 git 通道: 旧机 `homer secret push`，新机 `homer secret pull`。"

// DetectTailcat resolves the tailcat binary. A nil resolver uses the real
// process PATH; tests can inject exec.LookPath-compatible behavior without
// changing process-global command wiring.
func DetectTailcat(lookPath func(string) (string, error)) (string, error) {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	path, err := lookPath("tailcat")
	if err != nil || path == "" {
		return "", ErrNoTailcat
	}
	return path, nil
}
