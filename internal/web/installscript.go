package web

import (
	"fmt"
	"strings"
)

// installScriptTemplate is the Tailscale-style one-line bootstrap served at
// /install.sh. The hub substitutes its own reachability base URL and the
// GOOS/GOARCH pair it was built for; agents on a matching platform download
// the hub's own binary from /dl/homer. The token NEVER lives in the script
// (it is served unauthenticated and may be cached by proxies) — it arrives
// as the --token argument appended by the join command.
const installScriptTemplate = `#!/bin/sh
# homer agent 一行安装（由 hub 生成）
# 用法: curl -fsSL <HUB>/install.sh | sh -s -- --token <hub-token>
set -eu

HUB={{.HUB}}
GOOS_EXPECT={{.GOOS}}
GOARCH_EXPECT={{.GOARCH}}
TOKEN=
LISTEN=
ADVERTISE=

while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="$2"; shift 2 ;;
    --listen) LISTEN="$2"; shift 2 ;;
    --advertise) ADVERTISE="$2"; shift 2 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done

if [ -z "$TOKEN" ]; then
  echo "缺少 --token <hub-token>（hub 控制台「接入新机器」里复制完整命令）" >&2
  exit 1
fi

GOOS_ACTUAL=$(uname -s | tr '[:upper:]' '[:lower:]')
GOARCH_ACTUAL=$(uname -m)
case "$GOARCH_ACTUAL" in
  x86_64|amd64) GOARCH_ACTUAL=amd64 ;;
  aarch64|arm64) GOARCH_ACTUAL=arm64 ;;
esac

HOMER_HOME="${HOMER_HOME:-$HOME/.homer}"
BIN_DIR="$HOME/.local/bin"
mkdir -p "$HOMER_HOME/keys" "$BIN_DIR"
chmod 700 "$HOMER_HOME/keys"

# 1. 写入 token（agent 侧优先级: --token > HOMER_HUB_TOKEN > 此文件）
printf '%s' "$TOKEN" > "$HOMER_HOME/keys/hub-token"
chmod 600 "$HOMER_HOME/keys/hub-token"

# 2. 安装二进制：平台匹配 → 直接下载 hub 自身的二进制（/dl/homer
#    需要 Bearer 鉴权，token 同携带）
if [ "$GOOS_ACTUAL" = "$GOOS_EXPECT" ] && [ "$GOARCH_ACTUAL" = "$GOARCH_EXPECT" ]; then
  echo ">> 平台匹配（$GOOS_ACTUAL/$GOARCH_ACTUAL），下载 homer 二进制…"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL -H "Authorization: Bearer $TOKEN" "$HUB/dl/homer" -o "$BIN_DIR/homer.tmp"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO- --header="Authorization: Bearer $TOKEN" "$HUB/dl/homer" > "$BIN_DIR/homer.tmp"
  else
    echo "!! 需要 curl 或 wget" >&2; exit 1
  fi
  chmod 755 "$BIN_DIR/homer.tmp"
  mv "$BIN_DIR/homer.tmp" "$BIN_DIR/homer"
  echo ">> 已安装到 $BIN_DIR/homer"
else
  # 3. 平台不匹配 → 源码构建指引（hub 二进制仅覆盖自身平台）
  echo "!! 平台不匹配（本机 $GOOS_ACTUAL/$GOARCH_ACTUAL，hub 提供 $GOOS_EXPECT/$GOARCH_EXPECT）"
  echo "   请从源码构建:"
  echo "     git clone https://github.com/zzjcool/homer-cli && cd homer-cli"
  echo "     go install ./cmd/homer"
  echo "   （需要 Go 1.22+；token 已写入 $HOMER_HOME/keys/hub-token）"
fi

# 4. 零参数接入（token 从文件、hub 地址从脚本尾部参数）。
#    agent 是常驻 daemon——放后台跑（nohup），终端立刻归还给用户。
#    前台 exec 会占住 ssh 会话：用户以为卡住、Ctrl+C、agent 死、
#    机器列表停在"等待首次心跳"。
AGENT_BIN=""
if [ -x "$BIN_DIR/homer" ]; then AGENT_BIN="$BIN_DIR/homer"; fi
if [ -z "$AGENT_BIN" ] && command -v homer >/dev/null 2>&1; then AGENT_BIN="$(command -v homer)"; fi
if [ -n "$LISTEN" ] && [ -z "$ADVERTISE" ]; then
  echo "缺少 --advertise <hub 能访问的地址>（中心直连需要告诉 hub 往哪连）" >&2
  exit 1
fi
if [ -n "$AGENT_BIN" ]; then
  LOG="$HOMER_HOME/agent.log"
  if [ -n "$LISTEN" ]; then
    nohup "$AGENT_BIN" agent --listen "$LISTEN" --advertise "$ADVERTISE" --hub "$HUB" >>"$LOG" 2>&1 &
    echo ">> agent 已在后台启动（中心直连 $ADVERTISE，日志: $LOG，PID: $!）"
  else
    nohup "$AGENT_BIN" agent --connect "$HUB" >>"$LOG" 2>&1 &
    echo ">> agent 已在后台启动（机器上报，日志: $LOG，PID: $!）"
  fi
  echo ">> 几秒后刷新控制台，这台机器会出现在机器列表。"
  echo ">> 停止: pkill -f 'homer agent'"
else
  echo ">> homer agent 未在 PATH，token 已保存；装好后运行:"
  if [ -n "$LISTEN" ]; then
    echo ">>   nohup homer agent --listen $LISTEN --advertise $ADVERTISE --hub $HUB >>$HOMER_HOME/agent.log 2>&1 &"
  else
    echo ">>   nohup homer agent --connect $HUB >>$HOMER_HOME/agent.log 2>&1 &"
  fi
fi
`

// RenderInstallScript produces the bootstrap shell script for a hub at
// baseURL, describing the platform pair the hub binary itself runs on.
func RenderInstallScript(baseURL, goos, goarch string) string {
	script := strings.ReplaceAll(installScriptTemplate, "{{.HUB}}", baseURL)
	script = strings.ReplaceAll(script, "{{.GOOS}}", goos)
	script = strings.ReplaceAll(script, "{{.GOARCH}}", goarch)
	return script
}

// FallbackInstructions renders the source-build path for mismatched
// platforms (referenced by tests; the script embeds the same lines).
func FallbackInstructions(goos, goarch string) string {
	return fmt.Sprintf("平台不匹配（%s/%s）：请 git clone + go install 从源码构建", goos, goarch)
}
