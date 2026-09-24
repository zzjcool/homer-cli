# homer-cli Go 重写实施计划（MVP = TS 版 M1-M3 全功能对等）

> 基线：TS 实现 master=5b65ee5（1202 tests）已归档 `legacy/ts` 分支 + `ts-v1.0.0` tag。Go 版照 DESIGN.md 与 m1/m2/m3-plan 的冻结语义重写，**TS 版各轮对抗 review 修复过的行为必须原样保留**。规格来源齐全，重写 = 按图纸重盖；结构自由映射 Go 惯例，语义零漂移。

## 0. 目标与 Non-goals

**目标**：Go 单二进制实现 TS 版 M1-M3 全功能——`init/status/diff/push/pull/merge/home/doctor/secret(keygen|push|pull|list)` + 三 adapter（pi/herdr/opencode）+ age 密钥层 + 三路判定引擎 + 备份 + 密钥扫描 + install.sh/goreleaser 发布管线。

**Non-goals**：
- M4/M5（pi 扩展形态、tailcat、adapter 插件机制、`homer sync`/`pair`/`history`/`unlock`）
- 任何语义变更/优化（键序、合并语义、退出码、目录布局、`__REQUIRED__` 机制全部原样）
- go-git（git 操作仍走子进程调 git CLI，与 TS 版 `execFile` 行为一致）
- 测试逐行翻译 1202 例（按行为域归类移植，见 §6）
- 真实 `~/.homer`、`~/.pi`、真实 git remote 上的任何自动化测试

## 1. 仓库布局切换（一次性 commit，master 上执行）

```bash
# 前置确认（幂等保险）
git -C /root/code/homer-cli rev-parse legacy/ts ts-v1.0.0   # 均存在

# 1) 先抽黄金向量（不可省，见 §4.2）：在 legacy/ts 的 worktree 里跑 TS 引擎，
#    生成 testdata/golden/*.json 落回 master（此时 master 还是 TS 结构，直接写 testdata/）
git worktree add ../homer-ts-vec legacy/ts
# 在 worktree 中用 vitest/tsx 跑导出脚本（见 §4.2 清单）→ 输出到 /root/code/homer-cli/testdata/golden/
#    注意向量只含测试自造数据，绝不含任何真实密钥

# 2) 一次 commit 完成目录替换
git rm -r -q src tests bin dist 2>/dev/null; rm -f package.json package-lock.json \
  tsconfig.json vitest.config.ts .vitest  # node_modules 本就未跟踪
git add -A && git commit -m "chore: switch repo to Go layout (TS archived at legacy/ts, tag ts-v1.0.0)"
```

**Go 结构（冻结）**：

```
/                              # go.mod 在根，module github.com/<owner>/homer-cli
├── go.mod                     # Go 当前稳定版（落地时取 go1.2x 最新）
├── cmd/homer/main.go          # 薄入口：调用 internal/cli.Run(os.Args)
├── internal/core/             # types.go paths.go config.go state.go errors.go entrykind.go scanguard.go
├── internal/orderedjson/      # 保序 JSON 解析/序列化（§4，重写最大风险点，独立包独立测试）
├── internal/engine/           # merge.go mirror.go drift.go（纯函数，零 fs/git）
├── internal/adapter/          # scan.go ignore.go adpaths.go + pi/ herdr/ opencode/（defaults.go）
├── internal/gitx/             # git.go reader.go（exec git CLI）
├── internal/sync/              # plan.go excludedkeys.go firstsync.go apply.go pipeline.go base.go
├── internal/backup/           # backup.go
├── internal/secretscan/       # patterns.go scan.go（改名避开与 core/secrets 目录歧义）
├── internal/agecrypto/        # keys.go vault.go port.go（filippo.io/age 封装）
├── internal/doctor/           # checks.go
├── internal/cli/              # run.go args.go ui.go render.go + commands/{init,status,diff,push,pull,merge,home,doctor,secret}.go
├── internal/testutil/         # TempHome 构造器、fake port、PATH shim、假 origin 工厂
├── testdata/golden/           # TS 引擎生成的黄金向量（§4.2）
├── install.sh  .goreleaser.yaml  goreleaser 用 .github/workflows/release.yml
├── npm/                       # 薄壳 npm 包（§7.3，独立 package.json）
└── docs/ DESIGN.md README.md  # 保留
```

`internal/` 保证不暴露 API；`HOMER_HOME` 环境变量覆盖机制原样保留（测试隔离基石）。

## 2. 冻结 Go 接口（核心签名，worker 逐字落地不得改动）

### 2.1 `internal/core`（类型 + 路径 + 配置 + state）

```go
type SyncMode string // "merge" | "mirror"

type CategoryConfig struct {
    Paths       []string  `json:"paths"`
    Mode        SyncMode `json:"mode"`
    Enabled     *bool    `json:"enabled,omitempty"`
    Exclude     []string `json:"exclude,omitempty"`
    ExcludeKeys []string `json:"excludeKeys,omitempty"`
}
type AdapterConfig struct {
    Root       string                     `json:"root"`
    Enabled    *bool                      `json:"enabled,omitempty"`
    Categories map[string]CategoryConfig `json:"categories"`
    Ignore     []string                   `json:"ignore,omitempty"`
    AllowEscape []string                  `json:"allowEscape,omitempty"`
}
type BackupConfig struct{ Keep *int `json:"keep,omitempty"` }
type SecretsConfig struct {
    IgnorePaths []string          `json:"ignorePaths,omitempty"`
    Recipients  []string          `json:"recipients,omitempty"`
    Files       map[string]string `json:"files,omitempty"`
}
type HomerConfig struct {
    Version int                      `json:"version"`
    Adapters map[string]AdapterConfig `json:"adapters"`
    Backup   *BackupConfig           `json:"backup,omitempty"`
    Secrets  *SecretsConfig          `json:"secrets,omitempty"`
}

type SnapshotEntry struct { Kind string; Content string } // Kind: "json"|"file"；Kind 判定与 entryKindFor 同构
type SnapshotFiles map[string]SnapshotEntry               // key = category 内相对路径（m1-plan §1.6 布局规则）
type CategorySnapshot struct { AdapterID, Category string; Mode SyncMode; Files SnapshotFiles }
type AdapterSnapshot struct { AdapterID string; Categories []CategorySnapshot }

type HomerPaths struct{ Home, StoreDir, ConfigFile, StateFile, BackupsDir, SecretsDir, KeysDir string }
func GetHomerPaths(env func(string) string) HomerPaths // env("HOMER_HOME") 覆盖 ~/.homer
func LoadConfig(p HomerPaths) (*HomerConfig, error)    // 缺失 → os.ErrNotExist 哨兵
func ValidateConfig(raw []byte) (*HomerConfig, []string) // 手写校验，错误清单与 TS 版同文案骨架
func LoadState(p HomerPaths) HomerState                 // 缺失/损坏 → 零值，不 panic
func SaveState(p HomerPaths, s HomerState) error        // tmp+rename 原子写

type CliError struct{ Msg string; Code int } // 替代 TS CliError；Code=1 默认
```

### 2.2 `internal/orderedjson`（**JSON 键序问题的唯一收口，重写最大风险点**）

```go
package orderedjson

type Object struct { Keys []string; M map[string]Value } // 保插入序
type Value interface{} // *Object | []Value | json.Number | string | bool | nil
// 注意：数字一律 json.Number（保留字面量文本），杜绝 Go float64 与 JS Number 的序列化分叉

func Parse(data []byte) (Value, error)
func Serialize(v Value) []byte        // 2-space indent，转义规则钉死为 JS JSON.stringify 兼容（黄金向量锁死）
func SerializeFile(v Value) []byte   // = Serialize(v) + '\n'，对应 TS 的 JSON.stringify(x,null,2)+'\n'
func DeepEqual(a, b Value) bool
func StripTopKeys(v Value, keys []string) Value // excludeKeys 顶层剥离（返回深拷贝）
```

**冻结语义**：所有 JSON 解析/序列化/比较**只经本包**，`encoding/json` 的 `map` 路径全仓禁用（lint 规则 + code review）。merge 输出的键序规则：**先按 base 键序（值为合并结果），再 local-only 键（按 local 序），再 remote-only 键（按 remote 序）**——这是 JS 对象展开语义的保序近似，黄金向量验证偏差并在 S0 修正规则文本。

### 2.3 `internal/engine`（纯函数，零 fs/git，语义矩阵照 m1-plan §1.2-1.4 逐条）

```go
type MergeConflict struct { KeyPath, Reason string; Base, Local, Remote Value } // Reason: both-modified|modify-vs-delete|array-both-changed
type MergeResult struct { Status string; Merged Value; Conflicts []MergeConflict }
func MergeJSON(base, local, remote Value) MergeResult
type DiffJSONResult struct{ Changed, Added, Deleted int; Keys []string }
func DiffJSON(base, local Value) DiffJSONResult

type MirrorOp struct { Type, Path, Reason string } // noop|push|push-delete|pull|pull-delete|conflict
func CompareFile(base, local, remote *SnapshotEntry, relPath string) MirrorOp // 字符串全等
func CompareCategory(base, local, remote SnapshotFiles) []MirrorOp             // 路径并集

type CategoryDrift struct { AdapterID, Category string; Mode SyncMode; Push, Pull, Conflicts int;
    Ops []MirrorOp; MergeConflicts []MergeConflict; ChangedKeys []string }
func ComputeDrift(base, local, remote []AdapterSnapshot) []CategoryDrift // remote 可为 nil = base
```

### 2.4 `internal/gitx`（git 子进程边界，含全部安全修复）

```go
type ExecResult struct{ OK bool; Stdout, Stderr string }
func Exec(home string, args []string, timeout time.Duration) ExecResult // 永不 error，默认 15s
func EnsureGitRepo(home string) error        // git init（若缺）+ 幂等 .gitignore（state.json/backups//keys/ 三行）
func HasUpstream(home string) bool
func HasPushTarget(home string) bool         // @{upstream} 或 branch.<name>.remote（S3 裁定，必保）
func UpstreamRef(home string) string         // "" = 无
func Fetch(home string) ExecResult
func Push(home string) ExecResult
func HeadCommit(home string) string
func CommitAllStore(home, msg string) string // "" = 无变更；只 add store/
func CommitPaths(home string, pathspecs []string, msg string) string // secret 管线专用
func MergeFFUpstream(home string) ExecResult
func IsStoreClean(home string) bool
func IsAncestorOf(home, ancestor, descendant string) bool
func ReadStoreSnapshotAtCommit(p HomerPaths, cfg *HomerConfig, commitish string) []AdapterSnapshot
func ReadVaultFileAtCommit(home, relPath, commitish string) ([]byte, error)
func CloneRepo(url, dest string, timeout time.Duration) error // argv 恒为 ["clone","--",url,dest]（C1 修复硬约束）
func AssertCloneableRepoURL(url string) error                // 拒绝 '-' 开头（C1 纵深防御）
```

### 2.5 `internal/cli`（UI port 与命令注入位）

```go
type PromptPort interface {
    Confirm(msg string, fallback bool) bool
    Select(msg string, opts []SelectOption, fallback string) string
}
func NewDefaultPromptPort(isTTY bool) PromptPort // 非 TTY → NonInteractive（永远返回 fallback）
// 命令层注入位（测试用）：每命令一个 XxxDeps struct，字段为函数/接口，与 TS 版 deps 同构：
// PushDeps{UI PromptPort; Sources func() SyncSources}
// PullDeps{UI; Sources; NoFetch, NoApply bool}
// MergeDeps{UI; Sources; NoFetch bool}
// HomeDeps{UI; Age AgeCryptoPort; Clone func(url, dest string) error}
// SecretDeps{UI; Age AgeCryptoPort}
```

flag 解析：标准库 `flag` 子命令风格（轻量，与 TS parseArgs 语义对齐：`--home/--json/-h` 通用；push `--yes/--no-push`；pull `--yes`；merge `--accept-local/--accept-remote`；home `--mode pull|merge|skip/--yes`；doctor `--offline`；secret 各子命令同 m3-plan §2.1 表）。退出码总表照 m2-plan §2.8 / m3-plan §2.6 原样。

### 2.6 `internal/agecrypto`（filippo.io/age 封装，AgeCryptoPort 同构）

```go
type AgeIdentity struct{ SecretKey, Recipient string } // 私钥仅在内存与 <keysDir>/age.txt(0600)
type AgeCryptoPort interface {
    Encrypt(plaintext []byte, recipients []string) ([]byte, error)
    Decrypt(ciphertext []byte, id AgeIdentity) ([]byte, error)
}
func NewAgeCryptoPort() AgeCryptoPort // filippo.io/age 唯一封装点
func GenerateIdentity() AgeIdentity
func WriteIdentityFile(p HomerPaths, id AgeIdentity) error // 拒覆盖 / 0600 / keysDir 0700 / 原子写
func LoadIdentity(p HomerPaths) (AgeIdentity, bool)
func RecipientValid(r string) bool   // ^age1[02-9ac-hj-np-z]{58}$
func SecretNameValid(n string) bool
func EncryptSecretToFile(...) / DecryptSecretFromFile(...) / ListSecrets(...)
```

**行为保留清单（来自 m3-review 修复，测试必须逐条移植）**：密钥备份目录 0700/文件 0600（M1-fix）、密文自检全文兜底 `bytes.Equal(cipher,plain)` → 拒写（minor-1）、fetch 失败且分叉时拒绝写旧密钥 error exit 1（M2-fix）、双源对称取数 upstream 优先回落工作区（M3-fix）、`secret pull --yes` 回滚高危场景直接 error。

## 3. 波次拆分（P0-P5，标注并行性）

```
P0（串行 2 步）── ① 黄金向量抽取（legacy/ts worktree）→ ② 布局切换 commit + Go scaffold
P1（并行 5，依赖 P0）──┬─ W1-orderedjson  (internal/orderedjson/ + testdata/golden 全绿)
                        ├─ W2-engine      (internal/engine/，纯函数)
                        ├─ W3-adapter     (internal/adapter/ 全部，含三 defaults + ignore glob)
                        ├─ W4-core        (internal/core/ 全部 + internal/backup/ + internal/secretscan/)
                        └─ W5-gitx       (internal/gitx/，全部签名含 C1/S3 修复)
P2（并行 2，依赖 P1）──┬─ W6-sync        (internal/sync/：plan/excludedkeys/firstsync/apply/pipeline/base)
                       └─ W7-agecrypto   (internal/agecrypto/ + 互操作验证 §5)
P3（并行 2，依赖 P2）──┬─ W8-readonly-cli (cli: run/args/ui/render + init/status/diff)
                       └─ W9-doctor-secret (internal/doctor/ + cli/commands/doctor.go secret.go)
P4（串行，依赖 P3）──── W10-write-cli (cli/commands/{push,pull,merge,home}.go + adapters/paths 逆映射)
P5（串行收尾）────────── W11-integrator (e2e MVP 七组 + goreleaser + install.sh + npm 薄壳 + README + 对抗 review)
```

各 worker 目录互斥；`internal/core/types.go`、`internal/orderedjson`、`internal/cli/run.go+args.go` 一经 P0/P1 落地即冻结，变更只能报 orchestrator。

### P0 · 布局切换 + scaffold（§1 命令序列）
- **黄金向量抽取（先于目录替换！）**：在 legacy/ts worktree 写一次性导出脚本，跑 TS `mergeJson/mirror compareCategory/planPull/planFirstContact` 于代表性输入，输入+输出落 `testdata/golden/{merge,mirror,planpull,firstcontact}/*.json`（≥40 组：merge 语义表 8 情形 × 键序变体、mirror 9 情形、excludeKeys 三重语义、首次对接三模式）。**只含自造数据，出现任何 `AGE-SECRET-KEY`/真实密钥即失败**。
- **验收**：`go build ./... && go vet ./... && go test ./...`（空测试可过）；`go run ./cmd/homer --help` 打印 usage；`git log --oneline -1` 为布局切换 commit；`git show legacy/ts:package.json` 可读（保底确认）。

### P1 · 五并行

- **W1-orderedjson**：实现 §2.2 全部签名；验收 = `testdata/golden/merge/*.json` 的 parse→serialize roundtrip 与 TS 输出**逐字节相等**；JS 转义兼容（`<>&` 不转义、`\u` 规则）用向量锁死；`StripTopKeys` 行为与 TS `stripKeys` 向量一致。
- **W2-engine**：依赖 W1 的 Value 类型；验收 = golden merge/mirror 全绿 + 语义表逐行用例（≥12 断言组 merge、9 情形 mirror、drift 三类聚合含降级与 excludeKeys 剥离不计漂移）。
- **W3-adapter**：通用 scan + ignore glob（字面量 + `*` 不跨 `/` + 尾 `/` 前缀，**表驱动 ≥10 例**）+ allowEscape（含裸 `*`/`**/` 护栏拒绝，minor-5）+ 三 defaults（与 m3-plan §2.8/S0 冻结版逐字一致）+ symlink 逃逸默认跳过 + ScanError。验收：三 adapter fixture（mkdtemp 假 HOME）快照精确匹配（垃圾 0 条、JSON 损坏降级 file、逃逸链接 allowlist 命中/未命中）。
- **W4-core**：config/state/paths（`HOMER_HOME` 覆盖）+ store 读写（增量语义 = 清空 `<storeDir>/<adapterID>/` 重写）+ backup（`<date>/<HHmmss>-<command>/` 布局、prune keep=7、`opts mode{dir,file}`——密钥链 0700/0600、普通保持默认）+ secretscan（13 条 pattern：TS 版 12 条 + AGE-SECRET-KEY，逐条正反例 ≥1+2，脱敏 excerpt）。验收：全测试走 `t.TempDir()` + `HOMER_HOME`；roundtrip 深比较；备份权限断言（`os.Stat` mode 精确）。
- **W5-gitx**：§2.4 全部。验收：`git init --bare` 假 origin + clone 的临时仓库上覆盖 ensureGitRepo 幂等 / commitAllStore 空变更返回 "" / roundtrip 深比较（store 写→commit→read at commit）/ IsAncestorOf / ff 成功与分叉失败 / **`CloneRepo` argv 断言含 `--`** + `AssertCloneableRepoURL` 拒绝 `-ofoo` 与 `--upload-pack=…`（PATH shim 或直接单测函数）。

### P2 · 两并行

- **W6-sync**：plan（mirror 三动作映射 / merge 键冲突整文件 conflict + keyPaths / remote 删 + local 未改→delete / 降级 mirror）/ excludedkeys（占位符 `__REQUIRED__`、merge 后植回本地 excluded 键、远端占位符永不流入、local 缺失不植回）/ firstsync（三模式语义矩阵）/ apply（备份已存在→写、删除备份→rm→空父目录清理、conflict 跳过保留本地）/ pipeline（requireCleanStore / requireFastForwardable / commitStoreIfNeeded）/ base（collectSyncSources：git 模式 base=state commit，无 state→store 工作区，root 不可读→local:=base，fetch 失败降级矩阵）。验收：golden planpull/firstcontact 向量全绿 + excludeKeys 矩阵 ≥8 例 + S1-S4 接缝裁定行为各 1 例（首次基线 commit、upstream 无 store/ → remote:=base、S3 push target、S4 带冲突 pull 不前移 base）。
- **W7-agecrypto**：§2.6 全部。验收：keygen→write→load roundtrip（0600/0700、umask 不放宽）/ 多 recipient 各自可解 / 重复 keygen 拒覆盖 / 私钥不进任何 error 消息 / 密文自检（采样 + 全文兜底）/ **互操作（§5 硬要求）**。

### P3 · 两并行

- **W8-readonly-cli**：run/args 分发 + ui port（非 TTY 降级）+ render + init（非交互扫描→写 homer.json+store，已存在拒绝覆盖）/ status（`--json` 形状与 TS StatusReport 对齐）/ diff（键行 `key: old → new`、行级 `+/-`）。验收：手工 snapshot 打桩（不 import W6 实现）；`↑3 ↓1` 计数精确断言。
- **W9-doctor-secret**：八项检查（顺序 config→repo→store-clean→remote→adapters→age→machine→required；`__REQUIRED__` warn + details；`--offline`；exit 有 fail→1）/ secret 四子命令全流程（keygen 只回显 recipient；push 全有或全无 + `git grep` 明文为空；pull 分叉拒写旧密钥 + 双源对称 + 0600 + 备份；list）。验收：全部注入 deps + 假 origin + 临时 identity。

### P4 · W10-write-cli（串行，写路径是高危区）

push（密钥扫描拒推→remote-ahead/conflicts 拒绝→confirm→store→commit→push；push 死锁恢复：推送失败 exit 1 + 本地 commit 保留，复推成功）/ pull（前置四检查→plan→预览→备份→apply→ff→state；`--yes` 冲突保留本地 + conflicts-remain exit 1）/ merge（accept-local/accept-remote 批量、逐项 select、ff→base 前移→remote 项备份写入→重扫→push 管线；推送失败维持 exit 0 + warning）/ home（§m3-plan 2.7 十步流程：非空目录拒、clone `--`+URL 校验、clone 后 `git status --porcelain` 复验 stray（TOCTOU minor-2）、三选一默认 merge、state=HEAD、密钥缺失不 fail 整体、doctor 附报告）+ `adpaths.go`（relPath↔绝对路径逆映射，`..`/未知 basename → error）。验收：全命令 `--json` 可 parse 且与文本计数一致、退出码表逐条、假 origin 全流程、resolveCategoryFilePath 与 scan 互逆双侧硬断言。

### P5 · W11-integrator

- e2e MVP 七组（§6 清单）+ `go vet`/lint（`gofmt -l` 空、可加 golangci-lint）+ 对抗 review 三路（正确性/覆盖/简洁，fresh context）+ README 重写（Go 版安装/安全备案段保留）。

## 4. JSON 键序方案（单列风险，冻结）

**问题**：TS `JSON.stringify` 保插入序 + merge 引擎键级比较/构造；Go `map` 无序 + `encoding/json` 重排 → 语义等价但键序不同的文件会产生假冲突/假漂移（TS 版已显式记录为保守冲突，Go 版**不得**比 TS 更差）。

**方案**：全仓 JSON 只走 `internal/orderedjson`（§2.2）——自研保序 parser（`json.Decoder` token 流构造 `*Object`）+ JS 兼容序列化器 + `json.Number` 保字面量。merge 输出键序 = base 序→local-only→remote-only。**正确性锚点 = P0 从 legacy/ts 抽的黄金向量**（≥40 组，逐字节比对）。lint 禁止业务代码 import `encoding/json`（`orderedjson` 内部除外）。文件级相等仍为**字符串全等**（与 TS 冻结语义一致，不引入 canonical 序列化——维持 m2-plan §0 显式决策）。

## 5. age 互操作验证（硬要求，单列）

**目标**：TS 版（age-encryption npm）加密的旧 vault 密文，Go 版（filippo.io/age，age v1 规范原厂实现）必须能解；反向亦然。

**验证方案（W7 落地，四层）**：
1. **黄金密文向量**：P0 抽取时用 TS 版对固定明文（测试自造）以一次性测试 identity 加密，密文+该测试 identity 落 `testdata/golden/age/`（**明文标注 test-only throwaway key，代码注释与 README 双声明，绝不使用真实密钥**——吸取 m3-report M4 教训）→ W7 断言 Go 解密逐字节相等。
2. **age CLI 互操作**：若 CI/本机有 `age` CLI，`echo | age -r <recipient>` 加密 → Go 解密（m3-plan W1 同款）。
3. **多 recipient 跨实现**：TS 加密（recipients=[A,B]）→ Go 以 A、B 两个 identity 各自解密成功。
4. **真实回归（人工核验项）**：发布前在测试机用真实旧 vault（若存在）跑 `homer secret list` + 试解密 doctor 检查。
filippo.io/age 与 age-encryption 同实现 age v1 spec + X25519，理论互通；黄金向量是实证兜底。

## 6. 测试移植策略（行为域矩阵，不逐行翻译）

| # | 行为域 | 来源（legacy/ts tests/） | 最低例数 | 关键断言 |
|---|---|---|---|---|
| 1 | merge 语义矩阵 | tests/engine/merge | 25 | 8 情形 × 键序/嵌套/数组原子性；golden 向量 |
| 2 | mirror 9 情形 + 并集 | tests/engine/mirror | 15 | 3×3 全枚举 + compareCategory 并集 |
| 3 | drift 聚合 + 降级 + excludeKeys 不计漂移 | tests/engine/drift + strip-drift | 12 | merge/mirror/降级三类 |
| 4 | orderedjson roundtrip + JS 转义 | （新增，golden） | 20 | 逐字节与 TS 输出相等 |
| 5 | ignore glob 表驱动 | tests/adapters/pi/ignore | 12 | `*.bak-*`、`sessions/` 前缀、`*` 不跨 `/` |
| 6 | 三 adapter 快照 fixture | tests/adapters/{pi,herdr,opencode} | 18 | 垃圾 0 条、kind 判定、root 缺失不炸、allowEscape |
| 7 | store roundtrip + config 校验 + state | tests/{store,core/config,core/state} | 20 | 布局硬断言、HOMER_HOME 隔离 |
| 8 | secretscan 13 pattern 正反例 | tests/core/secrets | 40 | 13×(1 正+2 反)、脱敏、ignorePaths 豁免 |
| 9 | backup 权限 + prune | tests/core/backup + m3-fix | 12 | **密钥链 0700/0600**、普通默认、keep=7 |
| 10 | gitx 全函数 + C1 注入防护 | tests/core/git + home review 套件 | 18 | `clone --` argv、`-` URL 拒绝、roundtrip、S3 |
| 11 | sync plan/excludeKeys 三重语义/first-sync | tests/core/sync | 30 | 占位符永不流入、本地键植回、三模式 |
| 12 | push/pull/merge 命令（注入 deps） | tests/cli/{push,pull,merge} | 45 | 退出码总表、**push 死锁恢复**、merge 完成语义、S1-S4 |
| 13 | age 层 + 互操作（§5） | tests/core/age | 15 | 黄金密文、多 recipient、fetch 拒旧密钥、双源对称 |
| 14 | doctor 八项 | tests/{core,cli}/doctor | 16 | 顺序、warn/fail、`__REQUIRED__` details |
| 15 | secret 四子命令 | tests/cli/secret | 20 | keygen 只回显 recipient、明文不入 git |
| 16 | home + 首次对接 + TOCTOU | tests/cli/home | 18 | 三模式、stray 复验、密钥缺失不 fail |
| 17 | **e2e MVP 七组** | tests/e2e/{m1,m2,m3} | 7 组 | 见下 |

**e2e MVP 七组（全程 `t.TempDir()` 双假 HOME + 假 bare origin + `go run`/编译后子进程）**：① 安全往返（A init→push→改/删→push，commit 含增删改）② 密钥拒推（origin 无新 commit + ignorePaths 豁免）③ pull 应用 + 备份旧版 ④ remote-ahead → pull 合并 ⑤ 冲突 → merge --accept-remote → 双端一致 ⑥ pull-delete 传播 + 备份 ⑦ **m3 主线**：A（三工具 init→keygen→secret push→push）→ B `home --yes` → 逐字节归位 + 密钥 0600 + doctor 无 fail + status 零漂移 + 换设备加 recipient 重加密 + bare origin `git grep` 明文/私钥为空。

**总量目标 ≈ 350-380 例**（行为域覆盖对等，非行数对等）。所有测试统一 `testutil.TempHome()`（`os.MkdirTemp` + `HOMER_HOME` + 假 HOME 环境注入 + `t.Cleanup`），CI 加“测试期间真实 `~/.homer` mtime 不变”的守卫脚本。

## 7. 发布管线

### 7.1 goreleaser（`.goreleaser.yaml`）

```yaml
version: 2
builds:
  - id: homer
    main: ./cmd/homer
    binary: homer
    env: [CGO_ENABLED=0]
    goos: [linux, darwin]
    goarch: [amd64, arm64]
    ldflags: ["-s -w -X main.version={{.Version}}"]
archives: [{ formats: [tar.gz] }]  # name_template 含 {{ .Os }}_{{ .Arch }}
checksum: { name_template: 'checksums.txt' }
release: { github: { owner: <owner>, name: homer-cli } }
```

`.github/workflows/release.yml`：tag push → goreleaser release。本地验收：`goreleaser build --snapshot --clean` 四平台产物 + `checksums.txt`。

### 7.2 install.sh（重写为二进制通道）

`#!/bin/sh`（POSIX）：uname/detect 平台（linux|darwin × x86_64|aarch64）→ 从 GitHub releases latest 下载 `homer_<os>_<arch>.tar.gz` + `checksums.txt` 校验（`sha256sum`/`shasum` 探测）→ 解压至 `~/.local/bin`（可 `HOMER_INSTALL_PREFIX` 覆盖）→ `homer --help` 冒烟。注入位：`HOMER_INSTALL_VERSION` / `HOMER_INSTALL_BASE_URL`（测试指向本地 `python3 -m http.server` 假源）。验收：`sh -n` + 假源安装全平台矩阵 + 校验失败拒装 + 幂等。

### 7.3 npm 薄壳包（决策点，建议：保留）

**决策：保留 `npm/homer-cli` 薄壳包**，主通道 = 二进制。薄壳：`package.json`（bin: `homer` → install 脚本下载的 wrapper，或 postinstall 从 GitHub releases 拉取对应平台二进制到 `node_modules/.bin`；**下载失败不阻断 `npm install`**，wrapper 检测缺失时提示 install.sh）。理由：pi 生态 `pi install npm:homer-cli` 是既有安装路径（DESIGN §4 双形态），砍掉破坏存量；成本仅一个目录。**风险记录**：postinstall 拉网依赖 GitHub 可达性，README 明示离线场景用 install.sh。

## 8. 风险表与回滚点

| # | 风险 | 概率/影响 | 对策 |
|---|---|---|---|
| 1 | **JSON 键序/转义分叉**（Go 无序 map vs JS 保序；`\u` 转义、float 格式） | 高/高 | orderedjson 单点收口 + P0 黄金向量逐字节锚定 + lint 禁 encoding/json；向量不足发现的规则偏差在 W1 内修，不外溢 |
| 2 | **age 互操作**（旧 vault 解不开 = 硬失败） | 低/致命 | filippo.io/age 是规范原厂；四层验证（§5）；黄金密文向量 P0 先行；万一失败 → 密文格式排查单独立项，不阻塞其余波次 |
| 3 | **子进程差异**（exec vs execFile 的 argv 注入/超时/信号语义；git 版本差异） | 中/中 | exec.Command 精确 argv（无 shell）、timeout 用 context、`--` 硬约束测试；CI 固定 git 版本范围 |
| 4 | 文件权限/umask（Go 默认 0666&umask vs TS chmod 语义） | 中/中 | 所有安全敏感写入显式 `os.Chmod`（0600/0700）+ umask(0) 测试断言不放宽 |
| 5 | symlink/逃逸语义（Go filepath.WalkDir vs TS 手写遍历的目录顺序） | 中/中 | 目录遍历顺序显式 sort（键序稳定）；allowEscape/回环/悬空用例原样移植 |
| 6 | 行为回归（1202 例只移植 ~370，语义漏网） | 中/中 | 行为域清单 + 各 m*-report 修复条目逐条对应测试（§6 表）；对抗 review 三路兜底 |

**回滚点**：P0 布局切换 commit（TS 永远可从 `legacy/ts`+tag 找回）；P1-P4 各 worker 独立 commit，失败 2 次回滚该 commit 降级 orchestrator 串行修复，不阻塞同波其他 worker；`internal/core/types.go`/`orderedjson`/`cli/run.go` 冻结，接口缺陷一律上报统一修订。

## 9. 机器可验收标准（Go 版 Done 定义）

```bash
go build ./... && go vet ./... && gofmt -l . | grep -v vendor && go test ./...   # 全绿，含 golden 向量与 e2e 七组
goreleaser build --snapshot --clean    # linux/darwin × amd64/arm64 四产物
sh -n install.sh && <假源安装矩阵测试通过>
# e2e 主线（自动化）：机器 A 三工具装配+密钥推送 → 机器 B go run ./cmd/homer home <origin> --yes
#   → 逐字节归位 + 密钥 0600 + doctor 无 fail + status 零漂移 + bare origin 无明文/私钥
# age 互操作：testdata/golden/age 的 TS 密文 Go 解密逐字节相等（CI 常驻）
```

外加人工核验：真实环境 `homer doctor` 可读、`~/.homer/keys/age.txt` 600、真实旧 vault（若有）试解密成功、`~/.pi` 等工具目录零写入。
