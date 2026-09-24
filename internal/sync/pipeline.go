package sync

import (
	"fmt"
	"strings"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gitx"
)

// PrepareStoreSnapshot is the sole sync-side entrance for writing local
// snapshots to store. Excluded local keys become __REQUIRED__ placeholders;
// missing local keys are not fabricated.
func PrepareStoreSnapshot(local []core.AdapterSnapshot, config core.HomerConfig) []core.AdapterSnapshot {
	out := make([]core.AdapterSnapshot, len(local))
	for index, snapshot := range local {
		out[index] = ApplyExcludeKeyPlaceholders(snapshot, config)
	}
	return out
}

func prepareStoreSnapshot(local []core.AdapterSnapshot, config core.HomerConfig) []core.AdapterSnapshot {
	return PrepareStoreSnapshot(local, config)
}

// CommitStoreIfNeeded commits the portable configuration center (store/,
// homer.json, and .gitignore) and returns the new HEAD, or an empty string
// when there is no observable change or git cannot commit.
func CommitStoreIfNeeded(paths core.HomerPaths, message string) string {
	return gitx.CommitAllStore(paths.Home, message)
}

func commitStoreIfNeeded(paths core.HomerPaths, message string) string {
	return CommitStoreIfNeeded(paths, message)
}

const NOT_A_REPO_HINT = "请先运行 `homer push` 建立 git 历史与 remote。"
const NO_UPSTREAM_HINT = "请先 `git push -u <remote> <branch>`（或在 `~/.homer` 内 `git branch --set-upstream-to`）配置远端。"
const NO_UPSTREAM_MESSAGE = "未配置 git upstream，无法确定远端"

func NotAGitRepoMessage(home string) string {
	return fmt.Sprintf("工作区不是 git 仓库: %s", home)
}
func notAGitRepoMessage(home string) string { return NotAGitRepoMessage(home) }

// RequireCleanStore rejects non-repositories and any store-only working-tree
// change before pull/merge can touch the tool directory.
func RequireCleanStore(paths core.HomerPaths) error {
	if !gitx.IsGitRepo(paths.Home) {
		return core.NewCliError(NotAGitRepoMessage(paths.Home))
	}
	if !gitx.IsStoreClean(paths.Home) {
		return core.NewCliError("store 工作区有未提交的改动")
	}
	return nil
}

func requireCleanStore(paths core.HomerPaths) error { return RequireCleanStore(paths) }

func upstreamCommit(paths core.HomerPaths, ref string) string {
	if ref == "" {
		return ""
	}
	result := gitx.Exec(paths.Home, []string{"rev-parse", "--verify", ref + "^{commit}"}, 0)
	if !result.OK {
		return ""
	}
	return trimSpace(result.Stdout)
}

func trimSpace(value string) string { return strings.TrimSpace(value) }

// RequireFastForwardable verifies that the current HEAD can be advanced to
// the configured upstream without a merge. It does not perform the merge.
func RequireFastForwardable(paths core.HomerPaths) error {
	if !gitx.IsGitRepo(paths.Home) {
		return core.NewCliError(NotAGitRepoMessage(paths.Home))
	}
	ref := gitx.UpstreamRef(paths.Home)
	if ref == "" {
		return core.NewCliError(NO_UPSTREAM_MESSAGE)
	}
	remote := upstreamCommit(paths, ref)
	if remote == "" {
		return core.NewCliError(fmt.Sprintf("git upstream %s 不可解析（是否尚未 fetch？）", ref))
	}
	head := gitx.HeadCommit(paths.Home)
	if head == "" {
		return core.NewCliError("本地仓库没有 commit，无法快进到远端")
	}
	if head == remote || gitx.IsAncestorOf(paths.Home, head, remote) {
		return nil
	}
	return core.NewCliError(fmt.Sprintf("本地与远端已分叉（HEAD %s 不是 %s 的祖先）\n两台机器都推送过。解法：① 在本机 homer push（若另一机的改动已不需要）② git -C %s pull --rebase 后 homer merge（保留两边）", shortCommit(head), ref, paths.Home))
}

func requireFastForwardable(paths core.HomerPaths) error { return RequireFastForwardable(paths) }

func shortCommit(commit string) string {
	if len(commit) <= 7 {
		return commit
	}
	return commit[:7]
}
