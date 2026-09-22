# homer-cli M1 对抗式 review 修复报告

commit: `d8585ef` · branch: `pi-subagent/m1-review-fixes`（已 push 到 origin）· base: `a476beb` (master)

## 修复完成

- **M-A**（root 不存在 → 假 push + errors 被吞）：`collectSnapshotSources` 保留 `ScanOutcome.errors`，`StatusReport` / `InitReport` 增加 additive `errors: string[]`，status/diff 文本置顶 `⚠ adapter root 不可读: <id>`（非根级告警用 `⚠ 扫描告警: <id>`），root 不可读时判定侧 local 视作 = base → **不再产生假 push 计数**，exit 仍 0，init 也提示。
- **M-B**（symlink 回环膨胀 + 逃逸 root）：`walk` 用 visited realpath 集合截断回环；每个 symlink（含声明的 category path 自身）先 `realpathSync`，realpath 不在 root 之下 → skip + 记 `ScanError`；悬空链接静默跳过；root 内正常链接照常收集。
- **M-C**（store 写入非原子）：改为 tmp 目录（最后落 `.homer-complete`）→ rm 旧目录 → `renameSync` 原子替换，写失败清理 tmp 且旧目录保持完整；`readSnapshotFromStore` 遇「目录存在但缺标记」→ 抛 `CliError("store 不完整...")`，不再静默当空 base。

## 8 条 minor

| # | 状态 |
|---|---|
| 1 deepEqual NaN/-0 → `Object.is` | ✅ 含 NaN/-0 回归 |
| 2 `__proto__` / `constructor` hasOwnProperty 守卫 | ✅ merge + diff 双侧 |
| 3 diff 冲突行 `⚡` 前缀 | ✅ merge/mirror/文件级全覆盖 + 反向断言 |
| 4 `entryKindFor` 统一（新 `src/core/entry-kind.ts`）+ diff 降级行级 | ✅ scan/store/diff |
| 5 `expandHome` 去重（paths 导出） | ✅ |
| 6 `isPlainObject` 去重（三处改 import） | ✅ |
| 7 删 walk 内冗余 sort + fixture 锁定 | ✅ |
| 8 fixture 垃圾扩充 + e2e themes 行级精确断言 | ✅ |

## `stripExcludeKeys` 假漂移查证结论

**不可复现**（rev-correctness 报告项）。`JSON.stringify` 反而统一数字键顺序并消灭空白差异 → 减少假漂移（3000 次 fuzz 0 假漂移）。唯一残留边界：`drift.ts` 的 `l.content !== r.content` 是字节级比较，语义相等但**键插入顺序**不同仍可能判冲突 —— 但该分支仅显式注入 remote 且 base 缺失时可达（M1 生产路径不可达），且**不调用 strip 同样复现**，属 plan §1.3 冻结的「内容相等 = 字符串全等」，非 strip 引入。用 `tests/engine/strip-drift.test.ts`（12 例）钉住现状，未改冻结语义。

## 验证输出

```
$ npm run typecheck
> homer-cli@0.0.0 typecheck
> tsc --noEmit
（无错误）

$ npm test
 Test Files  17 passed (17)
      Tests  331 passed (331)
   Duration  1.22s
```

硬约束：`git diff --name-only master -- src/core/types.ts` → 0 行（未改 types.ts）。

## MR

分支已 push：`origin/pi-subagent/m1-review-fixes`。
**无法开 MR**：远端只有本分支（无 `master`/其它 base），本机无 `gh`/`glab`/token（`pull/new` 端点 302 → login），且硬约束禁止 push 到 main/master。手工建 PR：
https://github.com/zzjcool/homer-cli/pull/new/pi-subagent/m1-review-fixes

## 未决问题

- 真实 `~/.pi/agent/skills/agent-browser` 本身就是逃逸 symlink（→ `/root/.agents/skills/...`），新 containment 会把它排除并产出 `⚠ 扫描告警`。这是符合 M-B 安全边界的预期行为，但意味着该技能不会被同步；若希望支持「root 外但白名单内的链接」，需 M2 增加显式 allowlist 配置。
- `stripExcludeKeys` 的键序残留边界是否引入 canonical 序列化，留待 M2 决策（涉及冻结的 mirror「字符串全等」语义）。
