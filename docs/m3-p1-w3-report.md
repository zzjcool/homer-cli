# M3 · P1-W3 报告：symlink 逃逸 allowlist（`allowEscape`）

- 分支：`m3-p1-w3-allowlist`（worktree `/root/code/homer-cli-w3`，基线 `4251323`）
- commit：`b4b310df4fbd1404d4c7af354609450dbad23435`
- push：`origin/m3-p1-w3-allowlist`（新建）
- PR 创建入口（本机无 `gh`/`glab` CLI）：https://github.com/zzjcool/homer-cli/pull/new/m3-p1-w3-allowlist

## 做了什么

计划依据：`docs/m3-plan.md` §2.0-1（`AdapterConfig.allowEscape` 已由 P0 落地）、§1-D7、§3-P1-W3。

### 1. `src/adapters/pi/scan.ts`（M1 文件，改动最小化：只动 escape 判定处）

逃逸判定集中在两处，都改为「逃逸 → **先查 allowlist**，命中则视同 root 内链接继续跟随；否则维持原行为（skip + ScanError）」：

| 位置 | 判定对象 | allowlist 匹配基准 |
|---|---|---|
| `visitSymlink`（walk 中遇到的链接） | `skills/agent-browser` 这类条目 | `joinRel(rootRelBase, rel)` → root 相对路径 |
| `resolveConfiguredPath`（声明的 category path 自身是链接） | `skills/` / `settings.json` | 声明 path 去尾 `/` 后的 root 相对路径 |

- 新增 `WalkState.allowEscape` / `WalkState.rootRelBase`，`scanCategory` 透传 `config.allowEscape`。
  `rootRelBase` 存在的原因：快照 key（`rel`）相对 **category 目录**，而 `allowEscape` 是相对 **root** 的
  glob（§2.0-1 冻结语义），匹配前必须还原成 root 相对路径（`joinRel`）。
- 新增 `isEscapeAllowed(rootRel, allowEscape)`：直接复用 `matchesIgnore`，glob 语义与 adapter 级
  `ignore` 完全一致（字面量逐字 + `*` 不跨 `/` + 尾 `/` 目录前缀；不支持 `?` / `**`）。缺省 / 空数组 → false。
- **未改变**的行为：悬空链接静默跳过、`visited` 集合截断回环、root 内链接照常跟随、ignore/exclude 照常生效。
- allowlist 语义裁定（写进注释并被测试锁定）：只放行**命中的那一条链接**；逃逸子树内部再次逃逸的链接
  仍各自判定（要连带覆盖整棵用尾 `/` 模式，如 `skills/agent-browser/`）。

### 2. `src/core/config.ts`（allowEscape 校验）

**无需改动**：P0-M0 已按 §2.0-3 落地 `validateAdapter` 中的
`checkOptionalStringArray(raw['allowEscape'], '${where}.allowEscape', errors)`（见 `m3-p0-scaffold.test.ts`
的既有 2 例），与本次验收要求逐条一致，故按「合并去重」原则不再重复添加（未产生任何 diff）。

### 3. 测试

- 新增 `tests/adapters/pi/scan-allowescape.test.ts`（**17 例**）
- 追加 `tests/core/config.test.ts` 的 `validateConfig — adapters.*.allowEscape（P1-W3）`（**8 例**）

## 测试覆盖（对照验收逐条）

| 验收项 | 用例 |
|---|---|
| 逃逸 symlink 无 allowlist → 跳过 + ScanError | 目录型 / 文件型 / 声明的 path 自身三类，「无 allowlist（缺省）」例 |
| 既有逃逸相关 12 例回归全绿 | `tests/adapters/pi/scan-symlink.test.ts` 12/12（未改动该文件） |
| `allowEscape: ['skills/agent-browser']` → 跟随且内容入快照 | 目录型命中例（含子目录 `sub/a.ts`，errors 为空）；文件型 `['skills/link.md']` |
| allowlist 未命中路径 → 仍跳过 | `['extensions/other']` / `['skills/agent-browser2']` / `[]` 三值循环 + 文件型未命中 + 嵌套场景 |
| 回环截断 / 悬空跳过行为不变 | 「放行目录内链接指回 root 内 → visited 截断」「allowlist 命中的悬空链接 → 仍静默跳过」「root 内正常链接不受影响」 |
| glob 语义（字面量 + `*` 不跨 `+` 尾 `/` 前缀） | `['skills/*']` 命中一层；`['skills/*/deeper']` 不命中；`['skills/']` 前缀覆盖整棵；`['skills/agent-browser/']` 覆盖逃逸子树 |
| 匹配基准 = root 相对 | `['agent-browser']`（category 内写法）不生效 → 仍跳过 |
| validateConfig 非法 `allowEscape` 报错 | 非 string[]（字符串/对象/数字/null）、元素非串/空串、多 adapter 定位、loadConfig 读回 + 非法 throw、saveConfig 拒绝落盘 |
| 不改既有安全语义 | 「allowlist 不绕过 ignore / exclude」 |

## 验证输出（原样）

```
$ cd /root/code/homer-cli-w3 && git status --short
 M src/adapters/pi/scan.ts
 M tests/core/config.test.ts
?? tests/adapters/pi/scan-allowescape.test.ts

$ npm run typecheck
> homer-cli@0.0.0 typecheck
> tsc --noEmit
（无输出，exit 0）

$ npx vitest run tests/adapters/pi tests/core/config
 RUN  v5.0.1 /root/code/homer-cli-w3

 Test Files  5 passed (5)
      Tests  116 passed (116)
   Start at  06:42:48
   Duration  438ms (transform 60%, import 20%, tests 18%, worker 2%)

$ npx vitest run tests/adapters/pi/scan-symlink.test.ts
 Test Files  1 passed (1)
      Tests  12 passed (12)

$ npx vitest run tests/adapters/pi/scan-allowescape.test.ts tests/core/config.test.ts
 Test Files  2 passed (2)
      Tests  40 passed (40)

$ npm test
 Test Files  39 passed (39)
      Tests  819 passed (819)
   Start at  06:42:52
   Duration  25.40s (tests 65%, transform 28%, import 7%)
```

`819 = 794（基线）+ 25（本次新增：17 scan-allowescape + 8 config）`，全绿。

## 未决问题 / 需要 orchestrator 知晓

1. **共享 checkout 被别的 worker 抢占**：任务给的「工作目录 `/root/code/homer-cli`」在我实现期间被
   切到了 `m3-p1-w4-install` 分支（`git reflog`: `checkout: moving from m3-p0-scaffold to m3-p1-w4-install`
   → `merge tmp-w2`），且出现了 W4 的未跟踪文件 `install.sh` / `tests/e2e/install.test.ts`。
   我按隔离要求改为在**独立 git worktree** `/root/code/homer-cli-w3`（新分支 `m3-p1-w3-allowlist`，
   基线 `4251323`）工作，并把我在共享 checkout 里留下的改动（`git checkout --` + 删掉我创建的
   `tests/adapters/pi/scan-allowescape.test.ts`）**清理干净**，当前共享 checkout 只剩 W4 的
   `?? install.sh` / `?? tests/e2e/install.test.ts`，与我的工作无交叉。
   → 建议 orchestrator 后续给每个 P1 worker 独立 worktree，避免共享 checkout 串味。
2. **`src/core/config.ts` 零 diff**：P0 已完整落地 allowEscape 校验，未重复实现（符合「合并去重」）。
   如计划本意是让 W3 再补一层校验，请指明差异点。
3. **PR 未由 CLI 创建**：本机无 `gh` / `glab`，已 push 分支，PR 入口见文件头 URL。
4. **worktree 内的 `node_modules` 是指向主 checkout 的符号链接**（gitignored，仅为本 worktree 跑
   typecheck/test 用），不影响提交内容。
