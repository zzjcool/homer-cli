/**
 * `homer home <repo-url>` —— 新机器一键归位（docs/m3-plan.md §2.7）。
 *
 * **P0 落地范围（本文件）**：§2.7 的全部接口（类型 + `HOME_USAGE` + `runHome` stub
 * `throw CliError('尚未实现')`）+ 渲染钩子。真实流程由 **P3-W8** 在本文件内实现。
 *
 * ## 十步流程（§2.7 冻结，摘要）
 *
 *   1. `resolveHomerPaths`；home 目录存在且非空 → CliError
 *   2. `deps.clone`（默认真 `git clone`，timeout 60s；失败 → CliError 含 stderr 摘要）
 *   3. `loadConfig`（clone 下来的 homer.json）；缺失/非法 → CliError「该仓库不是 homer 配置中心」
 *   4. `scanAdapter` 逐 enabled adapter（本机现状，可为空/缺 root）
 *   5. remote = `readSnapshotFromStore`（clone 后工作区 == HEAD）
 *   6. **首次对接三选一**：`--mode` → 用之；`--yes` 无 mode → `merge`；否则 `ui.select`；
 *      非 TTY 且无 mode/yes → CliError（提示 `--mode` 或 `--yes`）
 *   7. `planFirstContact` → 非 skip 且有动作 → 非 `--yes` 时 confirm → `applyPullActions(backup=true)`
 *   8. `saveState`：`lastSyncCommit = HEAD`、`lastSyncCommand = 'pull'`、`lastSyncAt = now`
 *      （remote 已被看见；merge 模式保留的本地文件成为 push 漂移，见 §1-D5）
 *   9. **密钥归位**：`secrets.files` 非空 → `loadIdentity`；缺失 → `secrets.skipped` + warning；
 *      有 identity → 从工作区 vault 逐项解密（全有或全无）→ 备份后写目标（0600）；
 *      `undecryptable` → `secrets.errors` + warning，**不 fail 整个 home**（配置已归位，密钥可后补）
 *  10. `runDoctor`（透传 `deps.age`）→ 附于 `doctor`；`homed` exit 0（doctor 的 fail
 *      只呈现在报告里，不改变 home 退出码 —— doctor 独立运行时才 exit 1）
 *
 * ## Non-goal（M3 明确不做）
 *
 * home **不装依赖**（DESIGN §2.3 的「装依赖」延后：涉及各工具包管理器与网络副作用，
 * 风险大于收益；MVP 验收场景只需配置 + 密钥归位）。记录为 M4+ 候选。
 *
 * ## 类型来源
 *
 * `FirstContactMode` / `ApplyResult` / `PullConflictAction` / `DoctorReport` 分别属于
 * W5（`src/core/sync/first-sync.ts`）/ M2 既有（`src/core/sync/types.ts`）/ W7
 * （`src/core/doctor/checks.ts`）。P0 阶段前两者之一与后者尚不存在，故本文件用
 * `import type` 指向**已存在**的模块，对未落地者按 §2.4/§2.5 **逐字**本地声明
 * （后续切换为 import，形状不变 —— 见各 TODO）。
 */

import { CliError } from '../../core/errors.js';
import type { AgeCryptoPort } from '../../core/age/types.js';
import type { PromptPort } from '../ui.js';
import type { GitPort } from './git-port.js';
import type { ApplyResult } from '../../core/sync/types.js';
import type { DoctorReport } from './doctor.js';

// ---- §2.4 W5 类型（P0 逐字副本；W5 落地 `src/core/sync/first-sync.js` 后改为 import）----

/** TODO(P1-W5)：`first-sync.ts` 落地后改为 `import type { FirstContactMode } from '../../core/sync/first-sync.js';`。 */
export type FirstContactMode = 'pull' | 'merge' | 'skip';

/**
 * 冲突项（§2.4 / M2 `PullConflictAction` 的同构形状）。
 *
 * TODO(P1-W5/W8)：M2 已有的 `PullConflictAction` 定义于 `src/core/sync/types.ts`；
 * 本文件已 import 其同族的 `ApplyResult`。W8 落地时若形状与 M2 一致，直接改用
 * `import type { PullConflictAction } from '../../core/sync/types.js';` 更佳
 * （避免两处声明漂移）；此处按 §2.7 报告形状最小声明，只用到判别字段。
 */
export interface HomeConflictSummary {
  adapterId: string;
  category: string;
  relPath: string;
  keyPaths?: string[];
}

// ---- §2.7 命令层接口 ----

export interface HomeOptions {
  homerHome?: string;              // 目标工作区（默认 ~/.homer）；目录必须不存在或为空
  repoUrl: string;                 // 位置参数（必填）
  json?: boolean;
  yes?: boolean;
  mode?: FirstContactMode;         // --mode；缺省 + --yes → 'merge'（最安全默认）
}

export interface HomeDeps {
  ui?: PromptPort;
  age?: AgeCryptoPort;
  git?: Partial<GitPort>;
  /** clone 注入位（测试用假 origin）。缺省 = execFile('git', ['clone', url, dest], timeout 60s)。 */
  clone?: (repoUrl: string, destDir: string) => Promise<void>;
}

export interface HomeReport {
  ok: boolean;
  status: 'homed' | 'aborted' | 'error';
  cloned: boolean;
  adapterIds: string[];
  firstContact: { mode: FirstContactMode; applied: ApplyResult; conflicts: HomeConflictSummary[] } | undefined;
  secrets: { pulled: string[]; skipped: string[]; errors: string[] };
  doctor: DoctorReport | undefined;
  warnings: string[];
  errors: string[];
}

export const HOME_USAGE = `用法: homer home <repo-url> [options]

新机器一键归位：clone 配置仓库 → 应用各 adapter 分类配置 → 首次对接三选一
→ 用本机 age identity 解密密钥归位 → 体检。

选项:
  --home <dir>             目标工作区（默认 $HOMER_HOME 或 ~/.homer；须不存在或为空目录）
  --mode pull|merge|skip   首次对接模式（pull=远端覆盖 / merge=合并保留本地 / skip=暂不应用）
  --yes                    跳过交互确认；未给 --mode 时默认 merge（最安全）
  --json                   输出机器可读 JSON（HomeReport）
  -h, --help               显示本帮助

说明:
  不安装任何工具依赖（只归位配置与密钥）。
  非交互环境必须给出 --mode 或 --yes。
  merge 模式保留的本地文件随后表现为「待 push 漂移」（远端已被看见，本地 = 意图真相）。

退出码: homed → 0；aborted / error → 1（doctor 的 fail 不影响 home 退出码）。`;

/**
 * 执行一键归位（§2.7 十步流程）。
 *
 * P0 stub：实现见 W8。`deps.clone` 是假 origin 注入位；`deps.age` 透传给 doctor 的 age 检查。
 */
export async function runHome(opts: HomeOptions, deps?: HomeDeps): Promise<HomeReport> {
  // P0 stub：参数按 §2.7 冻结签名保留（W8 实现时逐项消费）。
  void opts;
  void deps;
  throw new CliError('尚未实现');
}

/** 人类可读渲染（P0 预置：W8 细化措辞，无需改分发层）。 */
export function renderHomeReport(report: HomeReport): string {
  const lines: string[] = [`homer home: ${report.status}`];
  lines.push(`  已 clone: ${report.cloned ? '是' : '否'}`);
  if (report.adapterIds.length > 0) lines.push(`  adapter: ${report.adapterIds.join(', ')}`);
  if (report.firstContact !== undefined) {
    lines.push(
      `  首次对接: ${report.firstContact.mode}` +
        `（写入 ${report.firstContact.applied.written} / 冲突 ${report.firstContact.conflicts.length}）`,
    );
  }
  if (report.secrets.pulled.length > 0) lines.push(`  密钥归位: ${report.secrets.pulled.length} 个`);
  if (report.secrets.skipped.length > 0) lines.push(`  密钥跳过: ${report.secrets.skipped.length} 个`);
  if (report.doctor !== undefined) lines.push(`  体检: ${report.doctor.ok ? '通过' : '存在问题'}`);
  for (const warning of report.warnings) lines.push(`  ⚠ ${warning}`);
  for (const error of report.errors) lines.push(`  ✗ ${error}`);
  return lines.join('\n');
}
