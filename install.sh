#!/bin/sh
# homer-cli 一键安装脚本（POSIX sh，零依赖）
#
#   curl -fsSL https://raw.githubusercontent.com/zzjcool/homer-cli/main/install.sh | sh
#
# 行为（docs/m3-plan.md §2.9 / DESIGN §3 验收场景）：
#   1. 检测 node（>= 20）；缺失或过低 → 打印安装指引 + exit 1
#   2. 检测 npm；`npm install -g <package>` 安装 homer-cli
#   3. 冒烟：安装后的 `homer --help` 必须能跑（不在 PATH 时提示加 $prefix/bin）
#   4. 打印下一步（homer home <你的配置仓库 url>）
#
# 可测性注入位（env，CI / e2e 用）：
#   HOMER_INSTALL_PACKAGE   安装源。默认 `homer-cli@latest`（走 npm registry）。
#                           可传：本地 tarball 路径（`npm pack` 产物）、
#                           http(s) tarball URL（GitHub release 资产）、`homer-cli@<version>`。
#   HOMER_INSTALL_PREFIX    安装前缀（可执行文件落在 "$prefix/bin"）。
#                           默认取 npm 全局 prefix（等价 `npm prefix -g`，也受 NPM_CONFIG_PREFIX 影响）。
#   HOMER_INSTALL_REGISTRY  registry 源，仅在安装源是包名时生效。
#                           默认 https://registry.npmjs.org（可换成私有镜像）。
#
# 退出码：0 = 安装成功且 `homer --help` 可跑；1 = 环境不满足或安装失败。
#
# 实现注意：本脚本常被 `curl | sh` 以 stdin 形式执行，所以每个子进程都显式
# `< /dev/null`，避免有程序偷读 stdin 把脚本剩余部分吃掉；所有错误信息走 stderr，
# stdout 只留人类可读的进度与下一步指引。
set -eu

MIN_NODE_MAJOR=20

say() { printf '%s\n' "$*"; }
err() { printf '%s\n' "$*" >&2; }

print_node_guide() {
  err ''
  err "请先安装 Node.js >= ${MIN_NODE_MAJOR}（推荐 LTS 版本）："
  err '  · macOS         : brew install node'
  err '  · Linux         : nvm / 发行版包管理器 / 官方二进制'
  err '  · Windows / WSL : winget install OpenJS.NodeJS.LTS（WSL 里直接跑本脚本）'
  err '  · 官方下载页    : https://nodejs.org/en/download'
  err '  · nvm 安装文档  : https://github.com/nvm-sh/nvm#installing-and-updating'
  err ''
  err "装完重开终端（确认 \`node -v\` 输出 v${MIN_NODE_MAJOR}.x 或更高），再重跑本脚本。"
}

print_npm_guide() {
  err ''
  err 'npm 通常随 Node.js 一起安装；若确实缺失：'
  err '  · 重装 Node.js（官方安装包 / nvm 都自带 npm）'
  err '  · 或见 https://docs.npmjs.com/downloading-and-installing-node-js-and-npm'
  err ''
  err '确认 `npm -v` 有输出后，再重跑本脚本。'
}

say '==> homer-cli 安装器'

# ------------------------------------------------------------------ #
# 1. 检测 node >= MIN_NODE_MAJOR                                      #
# ------------------------------------------------------------------ #
if ! command -v node >/dev/null 2>&1; then
  err '✗ 未检测到 Node.js：PATH 里找不到 `node` 命令。'
  print_node_guide
  exit 1
fi

node_path=$(command -v node)
node_version=$(node --version </dev/null 2>/dev/null || true)
node_major=${node_version#v}
node_major=${node_major%%.*}
case "${node_major}" in
  '' | *[!0-9]*)
    err "✗ 无法解析 Node.js 版本（node --version 输出: ${node_version:-<空>}）。"
    print_node_guide
    exit 1
    ;;
esac
if [ "${node_major}" -lt "${MIN_NODE_MAJOR}" ]; then
  err "✗ Node.js 版本过低：${node_version}（homer 需要 v${MIN_NODE_MAJOR} 或更高）。"
  print_node_guide
  exit 1
fi
say "    node : ${node_path} (${node_version})"

# ------------------------------------------------------------------ #
# 2. 检测 npm                                                         #
# ------------------------------------------------------------------ #
if ! command -v npm >/dev/null 2>&1; then
  err '✗ 未检测到 npm。'
  print_npm_guide
  exit 1
fi
npm_path=$(command -v npm)
npm_version=$(npm --version </dev/null 2>/dev/null || true)
say "    npm  : ${npm_path} (${npm_version:-unknown})"

# ------------------------------------------------------------------ #
# 3. 解析安装源 / 安装前缀                                            #
# ------------------------------------------------------------------ #
pkg=${HOMER_INSTALL_PACKAGE:-homer-cli@latest}
registry=${HOMER_INSTALL_REGISTRY:-https://registry.npmjs.org}

# 安装源是 registry 包名（而非本地路径 / tarball URL）时才带 --registry。
case "${pkg}" in
  *://* | /* | ./* | ../* | *.tgz | *.tar.gz) from_registry='' ;;
  *) from_registry=1 ;;
esac

prefix=''
if [ "${HOMER_INSTALL_PREFIX:-}" != '' ]; then
  prefix=${HOMER_INSTALL_PREFIX}
elif [ "${NPM_CONFIG_PREFIX:-}" != '' ]; then
  prefix=${NPM_CONFIG_PREFIX}
else
  prefix=$(npm prefix -g </dev/null 2>/dev/null || true)
fi

say "    安装源: ${pkg}"
if [ "${prefix}" != '' ]; then
  say "    前缀  : ${prefix}"
else
  say '    前缀  : <npm 默认全局 prefix>'
fi

# 幂等：已装过就覆盖安装（升级到本次指定的版本），不报错、不重复提示。
if command -v homer >/dev/null 2>&1; then
  say "    检测到已安装的 homer（$(command -v homer)），执行覆盖安装…"
elif [ "${prefix}" != '' ] && [ -x "${prefix}/bin/homer" ]; then
  say "    检测到已安装的 homer（${prefix}/bin/homer），执行覆盖安装…"
else
  say '    未检测到已安装的 homer，开始全新安装…'
fi

# ------------------------------------------------------------------ #
# 4. npm install -g                                                   #
# ------------------------------------------------------------------ #
set -- install -g "${pkg}" --no-fund --no-audit
if [ "${from_registry}" = 1 ]; then
  set -- "$@" --registry "${registry}"
fi
if [ "${prefix}" != '' ]; then
  set -- "$@" --prefix "${prefix}"
fi

# npm 输出走 stderr：stdout 保持为脚本自己的进度/指引，方便 `... | sh` 时人眼阅读。
if ! npm "$@" </dev/null 1>&2; then
  err ''
  err '✗ 安装失败：npm install -g 返回非零退出码。'
  err '  排查方向：'
  err '    · 网络 / registry 不可达（可用 HOMER_INSTALL_REGISTRY 指到镜像）'
  err "    · 安装前缀无写权限：$prefix"
  err '    · 版本不存在：确认 HOMER_INSTALL_PACKAGE 或 npm view homer-cli versions'
  exit 1
fi
say '    npm install -g 完成'

# ------------------------------------------------------------------ #
# 5. 冒烟：homer --help                                               #
# ------------------------------------------------------------------ #
homer_bin=''
if [ "${prefix}" != '' ] && [ -x "${prefix}/bin/homer" ]; then
  homer_bin="${prefix}/bin/homer"
elif command -v homer >/dev/null 2>&1; then
  homer_bin=$(command -v homer)
fi

if [ "${homer_bin}" = '' ]; then
  err '✗ 安装命令成功，但找不到 homer 可执行文件。'
  if [ "${prefix}" != '' ]; then
    err "  预期路径：${prefix}/bin/homer"
  fi
  err '  可手动确认：npm ls -g --depth=0 homer-cli'
  exit 1
fi

help_status=0
help_out=$("${homer_bin}" --help </dev/null 2>&1) || help_status=$?
if [ "${help_status}" -ne 0 ]; then
  err "✗ 冒烟失败：\`${homer_bin} --help\` 退出码 ${help_status}。"
  err "${help_out}"
  exit 1
fi
say '    冒烟通过: homer --help 正常'

# 不在 PATH 里就提示怎么加（含 `homer` 被 PATH 上另一个副本遮蔽的情况）。
if [ "${prefix}" != '' ]; then
  case ":${PATH}:" in
    *":${prefix}/bin:"*) ;;
    *)
      say ''
      say "提示：${prefix}/bin 不在 PATH 中，把它加进 shell 配置（~/.zshrc 或 ~/.bashrc）："
      say "  export PATH=\"${prefix}/bin:\$PATH\""
      ;;
  esac
fi

# ------------------------------------------------------------------ #
# 6. 下一步                                                           #
# ------------------------------------------------------------------ #
say ''
say "✓ homer 安装完成（${homer_bin}）"
say ''
say '下一步：'
say '  1. 新机器一键归位 : homer home <你的配置仓库 url>'
say '  2. 首次建立仓库   : homer init && homer push --yes'
say '  3. 密钥投递       : homer secret keygen / push / pull'
say '  4. 环境体检       : homer doctor'
