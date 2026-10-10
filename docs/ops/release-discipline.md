# SOP：homer-cli 发布与跨平台二进制纪律

> 2026-10-10 Mac agent 变砖事故沉淀。适用：打 tag 发 Release、升级 agent、二进制变砖恢复。

## 发布纪律

1. **Release 资产只能由 goreleaser CI 产出**。本地交叉编译的二进制（即使 Mach-O 逐字段一致）
   不进正式 Release——构建环境差异（公证/quarantine 上下文）无法靠 diff 排除。
   事故案例：2026-10-10 本地编译的 darwin/arm64 进 Release，Mac 上 exec format error。
2. `gh release create` 预传资产后，CI goreleaser 会因资产重名 422 失败，且 rerun 不幂等。
   要么不预传资产等 CI 全量产出，要么 `gh release delete-asset` 后 rerun。
3. 版本号注入：`go build -ldflags "-s -w -X main.version=$(git describe --tags)"`。
   裸 `go build` 是 dev；控制台的「落后版本」对比依赖真实版本号上报。

## Mac 恢复命令（agent 变砖时）

```bash
curl -fsSL https://github.com/zzjcool/homer-cli/releases/latest/download/homer_darwin_arm64.tar.gz -o /tmp/homer.tar.gz \
  && tar xzf /tmp/homer.tar.gz -C /tmp \
  && mv /tmp/homer ~/.local/bin/homer.new && mv ~/.local/bin/homer.new ~/.local/bin/homer
# 若 exec format error 先清 quarantine：
xattr -d com.apple.quarantine ~/.local/bin/homer
~/.local/bin/homer --version && nohup ~/.local/bin/homer agent --hub https://homerhw.openaaas.org > /tmp/homer-agent.log 2>&1 &
```

## 升级语义（v1.3.5 起）

- Linux/macOS：hub 控制台「更新程序」下载后 `syscall.Exec` 原地换进程（`reexec_unix.go`），
  即点即生效，无需重启。
- 跨平台：hub 只有自己的平台二进制，其它平台从 GitHub Releases 代理（`serveReleaseBinary`），
  带 `X-Homer-Platform` 响应头，agent 侧校验拒绝错架构。
- Windows：`reexec_other.go` 空实现，升级后需手动重启。

## hub 自身升级（hw 机器）

```bash
cd ~/code/homer-cli && git pull
go build -ldflags "-s -w -X main.version=$(git describe --tags)" -o /tmp/homer ./cmd/homer
install -m 755 /tmp/homer ~/.local/bin/homer.new && mv ~/.local/bin/homer.new ~/.local/bin/homer
systemctl --user restart homer-serve   # hub；同机 agent 也跑这个二进制，记得 restart homer-agent
```
