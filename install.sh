#!/bin/sh
# Install the pre-built homer binary published by GitHub Releases.
#
# The release path is deliberately POSIX-only so it can be used from a clean
# machine before Node, Go, or a package manager is installed.  CI and offline
# tests can inject HOMER_INSTALL_PACKAGE with a local binary or archive.
set -eu

say() {
  printf '%s\n' "$*"
}

warn() {
  printf 'homer install: warning: %s\n' "$*" >&2
}

die() {
  printf 'homer install: error: %s\n' "$*" >&2
  exit 1
}

OS=$(uname -s 2>/dev/null || true)
case "$OS" in
  Linux) HOMER_OS=linux ;;
  Darwin) HOMER_OS=darwin ;;
  *) die "unsupported operating system: ${OS:-unknown} (supported: Linux and Darwin)" ;;
esac

MACHINE=$(uname -m 2>/dev/null || true)
case "$MACHINE" in
  x86_64|amd64) HOMER_ARCH=amd64 ;;
  arm64|aarch64) HOMER_ARCH=arm64 ;;
  *) die "unsupported architecture: ${MACHINE:-unknown} (supported: amd64 and arm64)" ;;
esac

HOME_DIR=${HOME:-}
[ -n "$HOME_DIR" ] || die 'HOME is not set; set HOME or HOMER_INSTALL_PREFIX explicitly'

PREFIX=${HOMER_INSTALL_PREFIX:-"$HOME_DIR/.local/bin"}
ARCHIVE_NAME="homer_${HOMER_OS}_${HOMER_ARCH}.tar.gz"
TMP_ROOT=${TMPDIR:-/tmp}/homer-install-$$
DOWNLOAD_DIR="$TMP_ROOT/download"
EXTRACT_DIR="$TMP_ROOT/extract"
mkdir -p "$DOWNLOAD_DIR" "$EXTRACT_DIR"
cleanup() {
  rm -rf "$TMP_ROOT"
}
trap cleanup EXIT HUP INT TERM

# download SOURCE DEST supports regular local paths and file:// URLs as well
# as curl/wget URLs.  The local forms make the installer testable without a
# network and do not weaken the checksum check for release downloads.
download() {
  source=$1
  destination=$2
  case "$source" in
    file://*)
      cp "${source#file://}" "$destination"
      ;;
    /*|./*|../*)
      cp "$source" "$destination"
      ;;
    *)
      if command -v curl >/dev/null 2>&1; then
        curl -fsSL --retry 2 --connect-timeout 10 "$source" -o "$destination"
      elif command -v wget >/dev/null 2>&1; then
        wget -q --tries=2 --timeout=10 "$source" -O "$destination"
      else
        die 'curl or wget is required to download a release'
      fi
      ;;
  esac
}

sha256() {
  file=$1
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" | awk '{print $1}'
  else
    die 'sha256sum or shasum is required to verify the release'
  fi
}

# Read the checksum matching a basename from a standard goreleaser
# checksums.txt line: <sha256>  <archive-name>.
checksum_from_file() {
  checksum_file=$1
  checksum_name=$2
  awk -v wanted="$checksum_name" '
    NF >= 2 {
      name = $2
      sub(/^\*/, "", name)
      sub(/^.*\//, "", name)
      if (name == wanted) { print $1; exit }
    }
  ' "$checksum_file"
}

verify_checksum() {
  package_file=$1
  checksum_file=$2
  expected=$(checksum_from_file "$checksum_file" "$(basename "$package_file")")
  [ -n "$expected" ] || die "checksums.txt has no entry for $(basename "$package_file")"
  actual=$(sha256 "$package_file")
  [ "$actual" = "$expected" ] || die "checksum mismatch for $(basename "$package_file")"
}

PACKAGE_SOURCE=${HOMER_INSTALL_PACKAGE:-}
CHECKSUM_SOURCE=${HOMER_INSTALL_CHECKSUM_URL:-}
if [ -n "$PACKAGE_SOURCE" ]; then
  case "$PACKAGE_SOURCE" in
    file://*) PACKAGE_SOURCE=${PACKAGE_SOURCE#file://} ;;
  esac
  [ -f "$PACKAGE_SOURCE" ] || die "HOMER_INSTALL_PACKAGE is not a file: $PACKAGE_SOURCE"
  PACKAGE_FILE="$DOWNLOAD_DIR/$(basename "$PACKAGE_SOURCE")"
  cp "$PACKAGE_SOURCE" "$PACKAGE_FILE"

  # An offline archive can carry a sibling checksums.txt or an explicit
  # checksum file/value. Raw local binaries are accepted for the no-network
  # smoke test; published archives always take the remote verification path.
  LOCAL_CHECKSUM=${HOMER_INSTALL_CHECKSUM:-}
  if [ -n "$LOCAL_CHECKSUM" ]; then
    if [ -f "$LOCAL_CHECKSUM" ]; then
      verify_checksum "$PACKAGE_FILE" "$LOCAL_CHECKSUM"
    else
      actual=$(sha256 "$PACKAGE_FILE")
      [ "$actual" = "$LOCAL_CHECKSUM" ] || die "checksum mismatch for $(basename "$PACKAGE_FILE")"
    fi
  elif case "$PACKAGE_FILE" in *.tar.gz|*.tgz) true ;; *) false ;; esac; then
    SIBLING_CHECKSUM=$(dirname "$PACKAGE_SOURCE")/checksums.txt
    if [ -f "$SIBLING_CHECKSUM" ]; then
      cp "$SIBLING_CHECKSUM" "$DOWNLOAD_DIR/checksums.txt"
      verify_checksum "$PACKAGE_FILE" "$DOWNLOAD_DIR/checksums.txt"
    else
      warn "no local checksums.txt found; trusting injected HOMER_INSTALL_PACKAGE"
    fi
  fi
else
  VERSION=${HOMER_INSTALL_VERSION:-latest}
  BASE_URL=${HOMER_INSTALL_BASE_URL:-https://github.com/zzjcool/homer-cli/releases}
  DIRECT_URL=${HOMER_INSTALL_URL:-}

  if [ -n "$DIRECT_URL" ]; then
    case "$DIRECT_URL" in
      */.tar.gz|*.tar.gz|*.tgz|*/homer_*) PACKAGE_URL=$DIRECT_URL ;;
      */) PACKAGE_URL=${DIRECT_URL}${ARCHIVE_NAME} ;;
      *) PACKAGE_URL=${DIRECT_URL}/${ARCHIVE_NAME} ;;
    esac
    if [ -z "$CHECKSUM_SOURCE" ]; then
      CHECKSUM_SOURCE=$(dirname "$PACKAGE_URL")/checksums.txt
    fi
  elif [ "$VERSION" = latest ]; then
    case "$BASE_URL" in
      */latest/download) PACKAGE_URL="$BASE_URL/$ARCHIVE_NAME" ;;
      */releases) PACKAGE_URL="$BASE_URL/latest/download/$ARCHIVE_NAME" ;;
      */dist|*/dist/) PACKAGE_URL="${BASE_URL%/}/$ARCHIVE_NAME" ;;
      *) PACKAGE_URL="$BASE_URL/releases/latest/download/$ARCHIVE_NAME" ;;
    esac
    [ -n "$CHECKSUM_SOURCE" ] || CHECKSUM_SOURCE=$(dirname "$PACKAGE_URL")/checksums.txt
  else
    TAG=$VERSION
    case "$TAG" in v*) ;; *) TAG="v$TAG" ;; esac
    case "$BASE_URL" in
      */download|*/releases) PACKAGE_URL="$BASE_URL/download/$TAG/$ARCHIVE_NAME" ;;
      *) PACKAGE_URL="$BASE_URL/releases/download/$TAG/$ARCHIVE_NAME" ;;
    esac
    [ -n "$CHECKSUM_SOURCE" ] || CHECKSUM_SOURCE=$(dirname "$PACKAGE_URL")/checksums.txt
  fi

  PACKAGE_FILE="$DOWNLOAD_DIR/$ARCHIVE_NAME"
  say "homer: downloading $PACKAGE_URL"
  download "$PACKAGE_URL" "$PACKAGE_FILE" || die "failed to download release archive"
  CHECKSUM_FILE="$DOWNLOAD_DIR/checksums.txt"
  download "$CHECKSUM_SOURCE" "$CHECKSUM_FILE" || die "failed to download checksums.txt"
  verify_checksum "$PACKAGE_FILE" "$CHECKSUM_FILE"
fi

case "$PACKAGE_FILE" in
  *.tar.gz|*.tgz)
    tar -xzf "$PACKAGE_FILE" -C "$EXTRACT_DIR" || die 'failed to extract release archive'
    BINARY=$(find "$EXTRACT_DIR" -type f -name homer -perm -u+x -print 2>/dev/null | head -n 1 || true)
    if [ -z "$BINARY" ]; then
      BINARY=$(find "$EXTRACT_DIR" -type f -name homer -print 2>/dev/null | head -n 1 || true)
    fi
    [ -n "$BINARY" ] || die 'release archive does not contain a homer binary'
    ;;
  *)
    BINARY="$PACKAGE_FILE"
    ;;
esac

[ -f "$BINARY" ] || die "binary not found: $BINARY"

# Prefer an unprivileged per-user directory. If a caller explicitly requests
# /usr/local/bin, use non-interactive sudo only when it is already authorised;
# otherwise fall back instead of hanging on a password prompt.
TARGET="$PREFIX/homer"
INSTALL_WITH_SUDO=0
if mkdir -p "$PREFIX" 2>/dev/null; then
  :
elif [ "$PREFIX" = /usr/local/bin ] && command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
  INSTALL_WITH_SUDO=1
else
  if [ "$PREFIX" = /usr/local/bin ]; then
    PREFIX="$HOME_DIR/.local/bin"
    TARGET="$PREFIX/homer"
    mkdir -p "$PREFIX" || die "cannot create fallback install directory: $PREFIX"
    warn "cannot write /usr/local/bin without an authorised sudo; installing to $PREFIX"
  else
    die "cannot create install directory: $PREFIX"
  fi
fi

if [ "$INSTALL_WITH_SUDO" -eq 1 ]; then
  sudo install -m 0755 "$BINARY" "$TARGET"
else
  STAGED="$TARGET.tmp-$$"
  cp "$BINARY" "$STAGED"
  chmod 0755 "$STAGED"
  mv -f "$STAGED" "$TARGET"
fi

"$TARGET" --help >/dev/null 2>&1 || die "installed binary failed the --help smoke test: $TARGET"
say "✓ homer 安装完成（$TARGET）"
say ''
say '下一步：'
say '  1. 新机器一键归位 : homer home <你的配置仓库 url> --yes'
say '  2. 首次建立仓库   : homer init && homer push --yes'
say '  3. 密钥投递       : homer secret keygen / push / pull'
say '  4. 环境体检       : homer doctor'
