// Package sshkey installs a GitHub user's public keys into the agent
// user's authorized_keys so an administrator can SSH in after a machine
// joins. Only public keys are handled; nothing here reads or writes a
// private key.
package sshkey

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const keysBaseDefault = "https://github.com"

// keysBaseURL is the origin of the "<user>.keys" endpoint. Tests point it
// at a local server; production leaves it empty and uses github.com.
var keysBaseURL string

var keysClient = &http.Client{Timeout: 15 * time.Second}

var githubUserPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)

var publicKeyPattern = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp256|ecdsa-sha2-nistp384|ecdsa-sha2-nistp521|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) [A-Za-z0-9+/]+={0,3}(?: .+)?$`)

// Report is the agent task result for a key install.
type Report struct {
	OK         bool     `json:"ok"`
	Installed  int      `json:"installed"`
	Path       string   `json:"path,omitempty"`
	GitHubUser string   `json:"githubUser,omitempty"`
	Errors     []string `json:"errors"`
}

// FetchGitHubKeys downloads the public keys GitHub publishes at
// https://github.com/<user>.keys. The username is restricted to GitHub's
// own character rules so it cannot change the request host or path.
func FetchGitHubKeys(ctx context.Context, username string) ([]string, error) {
	username = strings.TrimSpace(username)
	if err := ValidateUser(username); err != nil {
		return nil, err
	}
	base := keysOrigin()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/"+username+".keys", nil)
	if err != nil {
		return nil, err
	}
	response, err := keysClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("读取 GitHub 公钥失败: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("读取 GitHub 公钥失败: %w", err)
	}
	if response.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("GitHub 用户 %s 不存在", username)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("读取 GitHub 公钥失败: %s", response.Status)
	}
	keys := Parse(string(body))
	if len(keys) == 0 {
		return nil, fmt.Errorf("GitHub 用户 %s 没有公钥", username)
	}
	return keys, nil
}

// ValidateUser reports whether username can be used in a GitHub keys URL.
// keysOrigin is the "<user>.keys" server. HOMER_GITHUB_KEYS_BASE overrides
// github.com so an end-to-end run can serve a fixture on the docker network.
func keysOrigin() string {
	if keysBaseURL != "" {
		return keysBaseURL
	}
	if env := strings.TrimSpace(os.Getenv("HOMER_GITHUB_KEYS_BASE")); env != "" {
		return env
	}
	return keysBaseDefault
}

func ValidateUser(username string) error {
	if !githubUserPattern.MatchString(username) {
		return fmt.Errorf("GitHub 用户名无效")
	}
	return nil
}

// Parse keeps lines that are a single OpenSSH public key. Option prefixes
// and anything else are dropped.
func Parse(text string) []string {
	var keys []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if publicKeyPattern.MatchString(line) {
			keys = append(keys, line)
		}
	}
	return keys
}

// Install writes keys into <home>/.ssh/authorized_keys for the user the
// agent runs as. A previous install for the same GitHub user is replaced.
// Keys outside that marked block are left as they are.
func Install(home, githubUser string, keys []string) Report {
	githubUser = strings.TrimSpace(githubUser)
	if err := ValidateUser(githubUser); err != nil {
		return Report{Errors: []string{err.Error()}}
	}
	keys = Parse(strings.Join(keys, "\n"))
	if len(keys) == 0 {
		return Report{GitHubUser: githubUser, Errors: []string{"没有可用的公钥"}}
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return Report{GitHubUser: githubUser, Errors: []string{"创建 .ssh 失败: " + err.Error()}}
	}
	if err := os.Chmod(sshDir, 0o700); err != nil {
		return Report{GitHubUser: githubUser, Errors: []string{"设置 .ssh 权限失败: " + err.Error()}}
	}
	path := filepath.Join(sshDir, "authorized_keys")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return Report{GitHubUser: githubUser, Errors: []string{"读取 authorized_keys 失败: " + err.Error()}}
	}
	updated := replaceBlock(string(existing), githubUser, keys)
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		return Report{GitHubUser: githubUser, Errors: []string{"写入 authorized_keys 失败: " + err.Error()}}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return Report{GitHubUser: githubUser, Errors: []string{"设置 authorized_keys 权限失败: " + err.Error()}}
	}
	return Report{OK: true, Installed: len(keys), Path: path, GitHubUser: githubUser, Errors: []string{}}
}

func replaceBlock(existing, githubUser string, keys []string) string {
	begin := "# BEGIN homer github " + githubUser
	end := "# END homer github " + githubUser
	block := begin + "\n" + strings.Join(keys, "\n") + "\n" + end + "\n"
	start := strings.Index(existing, begin)
	stop := strings.Index(existing, end)
	if start >= 0 && stop >= start {
		stop += len(end)
		if stop < len(existing) && existing[stop] == '\n' {
			stop++
		}
		return existing[:start] + block + existing[stop:]
	}
	trimmed := strings.TrimRight(existing, "\n")
	if trimmed == "" {
		return block
	}
	return trimmed + "\n" + block
}
