# M3 P0-M0 scaffold 报告

> 计划依据：`docs/m3-plan.md` §2.0 / §2.1 / §2.2 / §3-P0-M0
> 基线：`c731e51`（750 tests 绿）
> 本分支：`m3-p0-scaffold`，commit `4c5be66`
> 远端分支：`origin/m3-p0-scaffold`

## 1. 做了什么（逐条对任务清单）

| # | 计划条目 | 落地 | 说明 |
|---|---|---|---|
| 1 | `package.json` + `age-encryption` | ✅ | `age-encryption@^0.3.1` 装为运行时依赖（`dependencies`），lock 同步更新。安装成功，本机 Node v22.23.1 实测 `generateIdentity → identityToRecipient → encrypt/decrypt` roundtrip 通过（见 §4） |
| 2 | `src/core/types.ts` additive（§2.0-1） | ✅ | `SecretsConfig` 增 `recipients?: string[]` / `files?: Record<string,string>`；`AdapterConfig` 增 `allowEscape?: string[]`。`git diff` 仅含新增可选字段，无既有字段改动（§4 附 diff） |
| 3 | `src/core/paths.ts` additive（§2.0-2） | ✅ | `HomerPaths` 增 `secretsDir`（`<home>/secrets`）/ `keysDir`（`<home>/keys`） |
| 4 | `src/core/config.ts` additive 校验（§2.0-3） | ✅ | `secrets.recipients` 逐项 `recipientIsValid`；`secrets.files` 的 name 过 `secretNameValid` + 值须 `~`/`/` 开头；`adapters.*.allowEscape` 字符串数组。缺省一律合法；M2 既有校验（`ignorePaths`/`backup.keep`）回归不变 |
| 5 | `src/core/age/types.ts`（§2.2） | ✅ | 全部类型（`AgeIdentity` / `AgeCryptoPort`）+ `recipientIsValid` / `secretNameValid` 纯校验实现 + `createAgeCryptoPort` stub `throw CliError('尚未实现')` |
| 6 | `secrets/patterns.ts` 第 13 条 + 测试（§2.0-4） | ✅ | 数组**追加** `age-secret-key`（既有 12 条顺序与内容逐字未动）；`patterns.test.ts` 扩到 13 条（正例/反例夹具 + 顺序断言 38 例全绿） |
| 7 | `git.ts` `.gitignore` 幂等维护 +`keys/`（§2.0-5） | ✅ | `GITIGNORE_REQUIRED_LINES` 追加 `keys/`；`ensureGitignore` 逻辑未改（只追加缺失行，字节级幂等） |
| 8 | `args.ts` + `index.ts` 三命令 dispatch（§2.1） | ✅ | `COMMANDS` 增 `home`/`doctor`/`secret`；`USAGE` 增三命令条目 + 示例；`index.ts` 增 3 个 dispatch（secret 的 4 个子命令按 §2.1 flag 表各自解析，未知子命令 → usageError） |
| 9 | `commands/{home,doctor,secret}.ts`（§2.5-2.7） | ✅ | §2.5/§2.6/§2.7 接口逐字落地 + stub `throw CliError('尚未实现')` + `HOME_USAGE`/`DOCTOR_USAGE`/`SECRET_USAGE` + 渲染钩子（`render*Report`），使 W6/W7/W8 实现时无需再改 `index.ts` |

**硬约束遵守情况**：既有 750 tests 全绿 → 现共 **794**（+44 新增）；全部改动为 additive；`index.ts`/`args.ts`/`types.ts` 未超出 §2.0/§2.1（此后 P1-P4 禁改）。

## 2. 测试覆盖（+44 例）

新增/修改的测试文件：

- **`tests/core/m3-p0-scaffold.test.ts`（新增，40 例）**：
  - §2.0-2 路径：`secretsDir`/`keysDir` 派生 + 既有路径逐字不变 + 默认 home 场景
  - §2.2 `secretNameValid`：合法集、非法集（空 / 非字母数字开头 / 含分隔符 / 空白）、扁平性防逃逸
  - §2.2 `recipientIsValid`：真实 recipient（`age-encryption` 现场生成）合法；错误 HRP / 长度 +6 种 / 被排除 bech32 字符（`1 b i o`）/ 大写全拒
  - §2.2 `createAgeCryptoPort` stub 抛 `CliError('尚未实现')`
  - §2.0-1/§2.0-3 校验：`files` 合法/name 非法/值相对路径/非对象；`recipients` 合法（真实值）/含非法项/非数组/含空串；`allowEscape` 合法/非法；M2 `ignorePaths` 回归
  - §2.1 `COMMANDS` / `splitCommand` / `USAGE` 三新命令
  - §2.1 `secret` 子命令族解析（4 子命令 / 无子命令 / 未知子命令）+ `SECRET_USAGE`
  - 分发层：`--help` 含三新命令；`secret` 无子命令 exit 1 + usage；`secret --help` exit 0；未知子命令 usageError；4 个子命令 `--help`；`home`/`doctor` `--help`；`home` 缺 `<repo-url>` / 多余位置参数 / 非法 `--mode` 全部 usageError；6 组未知选项仍被 strict 拦下；`home` 完整 flag 表被接受（stub 报"尚未实现"）
- **`tests/core/secrets/patterns.test.ts`（13 条改造）**：条数 12→13、顺序追加断言、正例含大写真实形态 + 小写写法、反例 3 条（短名 / 前缀不符 / `AGE-PUBLIC-KEY-`）、"age 私钥不被其它 pattern 抢走"专项
- **`tests/core/git/git.test.ts`（+1 例，改 2 处期望）**：首次运行含 `keys/` 行；二次/三次幂等 `keys/` 不重复；新增 `git check-ignore` 实证 `keys/age.txt` 被忽略

## 3. 验证输出（原样）

```
$ npm run typecheck
> homer-cli@0.0.0 typecheck
> tsc --noEmit
exit=0

$ npm test
> vitest run
 Test Files  38 passed (38)
      Tests  794 passed (794)
   Duration  24.78s

$ npm run build          # 供 bin/homer.js 冒烟
> tsc
（无输出 = 成功）

$ node bin/homer.js secret
homer secret: 缺少子命令（keygen | push | pull | list）

用法: homer secret <keygen|push|pull|list> [options]
... （完整 usage：4 子命令 + flag 表 + 退出码总表 + "私钥永不回显"）
exit=1

$ node bin/homer.js --help
homer — dotfiles for humans and their AI agents
用法:
  homer <command> [options]
命令:
  init / status / diff / push / pull / merge
  home      新机器一键归位：clone 配置仓库 → 应用配置 → 解密密钥 → doctor
  doctor    八项体检（配置 / 仓库 / 远端 / adapter / age / state / 占位符残留）
  secret    密钥投递：keygen | push | pull | list
示例: ... homer home ... / homer doctor --json / homer secret keygen / homer secret push --yes
exit=0

$ git diff src/core/types.ts     # 仅 additive（附 §4 全文）
```

`age-encryption` 依赖冒烟（本机 Node v22.23.1，为验证依赖可用性而跑，非测试夹具）：

```
identity = AGE-SECRET-KEY-1...（临时生成，完整值已脱敏——冒烟后即弃，从未用于真实 vault）
recipient = age10nv7c3jw5nuqh7rclyvpwyl07sjkjtw99699dpuyhqmd7fj2nglq87lsy9
ciphertext len = 211,  header = age-encryption.org/v1 -> X25519
plaintext = hello homer          # roundtrip OK
/^age1[02-9ac-hj-np-z]{58}$/.test(recipient) → true   （与 §2.2 recipientIsValid 正则完全吻合）
```

## 4. `git diff src/core/types.ts`（证明仅 additive）

```diff
@@ -13,6 +13,12 @@ export interface AdapterConfig {
   ignore?: string[];            // adapter 级忽略，相对 root 的路径 glob
+  /** symlink 逃逸 allowlist（docs/m3-plan.md §2.0-1 / D7，可选） ... */
+  allowEscape?: string[];
 }
@@ -20,9 +26,13 @@ export interface BackupConfig {
-/** 密钥扫描豁免（docs/m2-plan.md §2.0-1；可选）。 */
+/** 密钥同步配置（docs/m2-plan.md §2.0-1 + docs/m3-plan.md §2.0-1；可选）。 */
 export interface SecretsConfig {
   ignorePaths?: string[];       // store 相对路径 glob（matchesIgnore 语义）
+  /** age X25519 recipients（age1...），secret push 的加密目标。 */
+  recipients?: string[];
+  /** secret 名 -> 目标路径（必须 '~' 或 '/' 开头 ...）。name 须过 secretNameValid。 */
+  files?: Record<string, string>;
 }
```

既有字段零改动、零删除、零重命名。

## 5. 需要 orchestrator 知晓的决策与偏差（2 处）

### 5.1 ⚠️ 第 13 条 pattern 的字符类加了 `i` 标志（安全语义偏差，已实测确认必要）

**计划原文**（§2.0-4）：`AGE-SECRET-KEY-[a-z0-9]{20,}`。

**实测**（§4 冒烟 + `docs/m3-scout-report.md` §3.2）：真实 age 私钥是
`AGE-SECRET-KEY-19WJMMZ92...` —— bech32 主体**全大写**。若逐字落下 `[a-z0-9]`（无 `i`），
第 13 条将**永不命中真实私钥**，安全闸门形同虚设，与 §2.0-4 的语义
（「age 私钥误入 store 拒推」）直接冲突。

**落地方式**：字符类保持计划原样 `[a-z0-9]`，仅加 `i` 标志 → 实际覆盖 `[A-Za-z0-9]`：
既匹配真实大写私钥，也匹配计划里的小写写法。**严格更宽、无收窄**，与 M2 已冻结的
「文档 `(?i)` → RegExp `i` 标志」翻译同款（`patterns.ts` 文件头已记录为「第三条翻译」）。
测试同时锁定大写（真实形态）与小写两种正例。

> 这是**实现细节**层面的等价扩宽，非接口变更；但属计划文本与现实的冲突，按纪律显式上报。

### 5.2 两处新增依赖 W1/W5/W7 文件的类型，P0 用逐字副本 + TODO 过渡

`secret.ts` 的 `SecretListReport.secrets: VaultEntryStatus[]`（§2.6）与 `home.ts` 的
`FirstContactMode`（§2.4），以及 `doctor.ts` 的 `CheckStatus`/`DoctorCheck`/`DoctorReport`
（§2.5），其权威定义分别属于 **W1** 的 `src/core/age/vault.ts`、**W5** 的
`src/core/sync/first-sync.ts`、**W7** 的 `src/core/doctor/checks.ts` —— 这些文件在 P0
阶段不存在（属后续 worker 的文件，P0 不越界创建）。

处理：在各命令文件内**逐字声明**同一形状，并留 `TODO(P1-W1/P1-W5/P2-W7)` 说明落地后
改为 `import type { ... }`（**1 行改动，形状不变**，故不构成接口变更，`index.ts` 零改动）。
`home.ts` 的 `ApplyResult` 直接 import M2 既有的 `src/core/sync/types.js`。

`home.ts` 中 `firstContact.conflicts` 因 §2.7 未指明冲突项类型，最小声明为
`HomeConflictSummary`（只用判别字段），并注明 W8 落地时若与 M2 的 `PullConflictAction`
形状一致应直接改用后者（避免两处声明漂移）。

## 6. 未决问题 / 风险

1. **PR 未创建**：本机无 `gh` CLI 也无 `GITHUB_TOKEN`，无法自动开 PR。分支已推送，
   PR 可由以下任一方式创建：
   - https://github.com/zzjcool/homer-cli/pull/new/m3-p0-scaffold
   - 或 orchestrator 侧 `gh pr create --head m3-p0-scaffold`
2. **`docs/m3-scout-report.md` 未纳入本 commit**：该文件是并行 worker S0-scout 的产出，
   非 M0 授权文件，故保持 untracked 留给其 owner 提交（避免跨 worker 抢文件）。
3. **`npm run build` 产出的 `dist/`** 已被 `.gitignore` 忽略，未入库。
4. **不属 P0 但已被本 commit 锁定的接口**：`index.ts`/`args.ts` 冻结的 flag 表中，
   `home` 用 `allowPositionals: true`（`repo-url` 是位置参数），其余命令维持 `false`；
   `secret` 无子命令 exit 1（缺必需子命令）、`secret --help` exit 0。
   若 W6/W8 需要不同退出码语义，请在此后**通过命令实现**表达，不要改 `index.ts`。
