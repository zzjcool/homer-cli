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

while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="$2"; shift 2 ;;
    --listen) echo "listen 模式已移除" >&2; exit 1 ;;
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
# POSIX portability: a bare variable reference must never sit directly
# before a multi-byte character. macOS /bin/sh (bash 3.2) without a UTF-8
# locale swallows the lead byte of a following fullwidth punctuation mark
# into the variable name, so the platform-mismatch notice used to die with
# "unbound variable" under set -eu. Braced references make the name explicit.
HOMER_HOME="${HOMER_HOME:-$HOME/.homer}"
BIN_DIR="$HOME/.local/bin"
mkdir -p "$HOMER_HOME/keys" "$BIN_DIR"
chmod 700 "$HOMER_HOME/keys"

# 1. 写入 token（agent 侧优先级: --token > HOMER_HUB_TOKEN > 此文件）
printf '%s' "$TOKEN" > "$HOMER_HOME/keys/hub-token"
chmod 600 "$HOMER_HOME/keys/hub-token"
# 带一次性接入码(hr_)重跑脚本，意思就是「用这个码重新接入」。agent 重启时，
# 只要 agent.json 里已有 secret，就会忽略 keys/hub-token 里的 hr_ 码（那个码
# 通常已经兑换过）。换 hub 或重装时旧 secret 属于旧身份，不清掉的话新码永远
# 用不上、agent 一直 401。所以这里把旧身份移到一边（保留备份，不直接删）。
case "$TOKEN" in
  hr_*)
    if [ -f "$HOMER_HOME/agent.json" ]; then
      mv "$HOMER_HOME/agent.json" "$HOMER_HOME/agent.json.before-reenroll" 2>/dev/null \
        && echo ">> 检测到旧的 agent.json，已改名为 agent.json.before-reenroll，将用新接入码重新接入"
    fi
    ;;
esac

# 2. 安装二进制。hub 在慢速隧道后面时，一次连接往往传不完，
#    所以优先下载 gzip（大约少一半），并用断点续传把多次连接拼起来。
#    /dl/homer 与 /dl/homer.gz 都要 Bearer 鉴权，token 同携带；
#    公开的 GitHub Release 下载永远不带 token（send_auth=0），避免把
#    hub 凭证发给 github.com 及其 CDN。
download() {
  url="$1"
  dest="$2"
  send_auth="${3:-1}"
  attempt=0
  while [ "$attempt" -lt 12 ]; do
    status=0
    if command -v curl >/dev/null 2>&1; then
      if [ "$send_auth" -eq 1 ]; then
        curl -fL --connect-timeout 20 --speed-time 30 --speed-limit 1024 -C - \
          -H "Authorization: Bearer $TOKEN" "$url" -o "$dest" || status=$?
      else
        curl -fL --connect-timeout 20 --speed-time 30 --speed-limit 1024 \
          "$url" -o "$dest" || status=$?
      fi
      if [ "$status" -eq 22 ]; then
        if [ "$send_auth" -eq 1 ]; then
          echo "!! 下载被拒绝（凭证无效或已过期）" >&2
        else
          echo "!! Release 下载被拒绝（HTTP 4xx，可能没有本平台的归档）" >&2
        fi
        return 1
      fi
    elif command -v wget >/dev/null 2>&1; then
      if [ "$send_auth" -eq 1 ]; then
        wget -q -c --timeout=20 --header="Authorization: Bearer $TOKEN" -O "$dest" "$url" || status=$?
      else
        wget -q -c --timeout=20 -O "$dest" "$url" || status=$?
      fi
    else
      echo "!! 需要 curl 或 wget" >&2
      return 1
    fi
    if [ "$status" -eq 0 ]; then
      return 0
    fi
    attempt=$((attempt + 1))
    echo ">> 下载中断，从已收到的部分继续（第 ${attempt} 次）…" >&2
    sleep 2
  done
  echo "!! 下载 homer 二进制失败" >&2
  return 1
}
if [ "$GOOS_ACTUAL" = "$GOOS_EXPECT" ] && [ "$GOARCH_ACTUAL" = "$GOARCH_EXPECT" ]; then
  echo ">> 平台匹配（${GOOS_ACTUAL}/${GOARCH_ACTUAL}），下载 homer 二进制…"
  if command -v gzip >/dev/null 2>&1; then
    download "$HUB/dl/homer.gz" "$BIN_DIR/homer.gz"
    gzip -dc "$BIN_DIR/homer.gz" > "$BIN_DIR/homer.tmp"
    rm -f "$BIN_DIR/homer.gz"
  else
    download "$HUB/dl/homer" "$BIN_DIR/homer.tmp"
  fi
  chmod 755 "$BIN_DIR/homer.tmp"
  mv "$BIN_DIR/homer.tmp" "$BIN_DIR/homer"
  echo ">> 已安装到 $BIN_DIR/homer"
  # ~/.local/bin 在桌面登录里通常已经进了 PATH。容器里的 sh 不会读
  # 那份配置，所以再链到默认 PATH 里的 /usr/local/bin。没有写权限就只
  # 把目录记进 shell 启动文件，下一次交互式 shell 能找到命令。
  case ":$PATH:" in
    *":$BIN_DIR:"*) ;;
    *)
      if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
        ln -sfn "$BIN_DIR/homer" /usr/local/bin/homer
        echo ">> 已链接到 /usr/local/bin/homer，可以直接运行 homer"
      else
        path_line='export PATH="$HOME/.local/bin:$PATH"'
        for rc in "$HOME/.profile" "$HOME/.bashrc"; do
          if [ -f "$rc" ] && grep -q '.local/bin' "$rc" 2>/dev/null; then
            continue
          fi
          printf '\n%s\n' "$path_line" >> "$rc"
        done
        echo ">> 当前 shell 还找不到 homer。新开一个终端，或执行: export PATH=\"\$HOME/.local/bin:\$PATH\""
      fi
      ;;
  esac
else
  # 3. 平台不匹配 → 交叉下载预编译产物；GitHub 不可达或本平台没有
  #    归档时退回源码构建指引。下载是公开资源：不带 token（见 download
  #    的 send_auth 参数）、不做断点续传（避免跨版本拼接残留文件）。
  echo ">> 平台不匹配（本机 ${GOOS_ACTUAL}/${GOARCH_ACTUAL}，hub 提供 ${GOOS_EXPECT}/${GOARCH_EXPECT}）"
  RELEASE_FALLBACK=1
  RELEASE_SUPPORTED=0
  case "${GOOS_ACTUAL}/${GOARCH_ACTUAL}" in
    linux/amd64|linux/arm64|darwin/amd64|darwin/arm64) RELEASE_SUPPORTED=1 ;;
  esac
  if [ "$RELEASE_SUPPORTED" -eq 0 ]; then
    echo "!! GitHub Releases 没有本平台（${GOOS_ACTUAL}/${GOARCH_ACTUAL}）的预编译归档" >&2
  else
    RELEASE_BASE="https://github.com/zzjcool/homer-cli/releases/latest/download"
    RELEASE_ARCHIVE="homer_${GOOS_ACTUAL}_${GOARCH_ACTUAL}.tar.gz"
    REL_TMP=$(mktemp -d "${TMPDIR:-/tmp}/homer-release.XXXXXX")
    rm -f "$BIN_DIR/homer-rel.tar.gz"
    if download "$RELEASE_BASE/$RELEASE_ARCHIVE" "$BIN_DIR/homer-rel.tar.gz" 0; then
      # 解到独立目录再原子安装：只有「这次归档里确实解出了 homer」才算
      # 成功，不会被以前装的旧二进制冒充；也避免全解污染 BIN_DIR。
      if tar -xzf "$BIN_DIR/homer-rel.tar.gz" -C "$REL_TMP" 2>/dev/null \
        && [ -f "$REL_TMP/homer" ]; then
        chmod 755 "$REL_TMP/homer"
        mv -f "$REL_TMP/homer" "$BIN_DIR/homer"
        echo ">> 已从 GitHub Releases 安装 $RELEASE_ARCHIVE 到 $BIN_DIR/homer"
        RELEASE_FALLBACK=0
      else
        echo "!! Release 归档解压失败或里面没有 homer" >&2
      fi
    fi
    rm -rf "$REL_TMP" "$BIN_DIR/homer-rel.tar.gz"
  fi
  if [ "$RELEASE_FALLBACK" -eq 1 ]; then
    echo "   请从源码构建:"
    echo "     git clone https://github.com/zzjcool/homer-cli && cd homer-cli"
    echo "     go install ./cmd/homer"
    echo "   （需要 Go 1.22+；token 已写入 $HOMER_HOME/keys/hub-token）"
  fi
fi

# 4. 零参数接入。和 Tailscale 一样：安装脚本自己退出，daemon 交给
#    服务管理器在后台跑（systemctl enable --now）。没有 systemd 的
#    环境才退回 nohup。容器里如果没有常驻的主进程，光 nohup 也会
#    跟着容器一起停。
AGENT_BIN=""
if [ -x "$BIN_DIR/homer" ]; then AGENT_BIN="$BIN_DIR/homer"; fi
if [ -z "$AGENT_BIN" ] && command -v homer >/dev/null 2>&1; then AGENT_BIN="$(command -v homer)"; fi
if [ -n "$AGENT_BIN" ]; then
  AGENT_ARGS="agent --hub $HUB"
  started=0
  if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    if [ "$(id -u)" -eq 0 ]; then
      mkdir -p /etc/systemd/system
      cat > /etc/systemd/system/homer-agent.service <<EOF
[Unit]
Description=homer agent
After=network-online.target

[Service]
Type=simple
Environment=HOME=$HOME
Environment=HOMER_HOME=$HOMER_HOME
ExecStart=$AGENT_BIN $AGENT_ARGS
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
      if systemctl daemon-reload && systemctl enable --now homer-agent.service; then
        echo ">> agent 已交给 systemd 在后台运行（homer-agent.service）"
        started=1
      fi
    elif [ -n "${XDG_RUNTIME_DIR:-}" ] && systemctl --user show-environment >/dev/null 2>&1; then
      unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
      mkdir -p "$unit_dir"
      cat > "$unit_dir/homer-agent.service" <<EOF
[Unit]
Description=homer agent
After=network-online.target

[Service]
Type=simple
Environment=HOME=$HOME
Environment=HOMER_HOME=$HOMER_HOME
ExecStart=$AGENT_BIN $AGENT_ARGS
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
EOF
      if systemctl --user daemon-reload && systemctl --user enable --now homer-agent.service; then
        echo ">> agent 已交给用户服务在后台运行（homer-agent.service）"
        started=1
      fi
    fi
  fi
  if [ "$started" -eq 0 ]; then
    LOG="$HOMER_HOME/agent.log"
    # setsid 让 agent 脱离安装脚本，脚本可以马上退出。
    if command -v setsid >/dev/null 2>&1; then
      setsid "$AGENT_BIN" $AGENT_ARGS >>"$LOG" 2>&1 < /dev/null &
    else
      nohup "$AGENT_BIN" $AGENT_ARGS >>"$LOG" 2>&1 &
    fi
    agent_pid=$!
    echo ">> agent 已在后台启动（日志: ${LOG}，PID: ${agent_pid}）"
    if [ -f /.dockerenv ]; then
      echo ">> 这个容器没有 systemd。请让容器自己保持运行，否则主进程退出时 agent 会一起停。"
    fi
  fi
  echo ">> 几秒后刷新控制台，这台机器会出现在机器列表。"
  echo ">> 停止: systemctl stop homer-agent，或 pkill -f 'homer agent'"
else
  echo ">> homer agent 未在 PATH，token 已保存；装好后运行:"
  echo ">>   nohup homer agent --hub $HUB >>$HOMER_HOME/agent.log 2>&1 &"
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
