# homer-cli (npm compatibility wrapper)

`homer-cli` is a thin npm wrapper around the native Go `homer` binary. The
primary installation channel is the POSIX release installer:

```sh
curl -fsSL https://raw.githubusercontent.com/zzjcool/homer-cli/main/install.sh | sh
```

The npm package runs a best-effort `postinstall` download for Linux/macOS
amd64/arm64. A GitHub/network failure is intentionally non-fatal; the wrapper
prints a pointer to `install.sh` and exits with a useful message if no native
binary is available.

```sh
npm install -g homer-cli
# Offline/test installation:
HOMER_INSTALL_PACKAGE=/path/to/homer_linux_amd64 npm install -g ./npm
```

Use `HOMER_NPM_SKIP_INSTALL=1` to skip the optional postinstall download. For
an offline archive, place its matching `checksums.txt` beside the archive (or
set `HOMER_INSTALL_CHECKSUM`) so the postinstall can verify it. `goreleaser`
produces the release archives and checksums; this package is only a
compatibility path for npm-based tooling.
