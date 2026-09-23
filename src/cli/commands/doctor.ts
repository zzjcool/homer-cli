/**
 * `homer doctor` —— 八项体检（docs/m3-plan.md §2.5）。
 *
 * **P0 落地范围（本文件）**：§2.5 中 `src/cli/commands/doctor.ts` 的部分
 * （`DoctorOptions` / `DoctorDeps` / `DOCTOR_USAGE` / `runDoctor` stub）+ §2.5 `checks.ts` 的
 * 全部**类型**（P0 本文件内逐字声明，见下方 TODO）+ 渲染钩子。
 * 真实检查逻辑由 **P2-W7** 落地：`src/core/doctor/{checks,index}.ts` + 本文件实现。
 *
 * ## 八项检查（§2.5 / D6 冻结顺序）
 *
 *   config → repo / store-clean → remote → adapters → age → machine → required
 *
 * `config` fail 时**仍继续**其余检查（尽力而为），`ok=false`。
 * exit：无 `fail` → 0；有 `fail` → 1（`warn` 不影响退出码 —— 回应 m2-report §5-3）。
 *
 * ## TODO(P2-W7)：类型来源切换
 *
 * §2.5 把 `CheckStatus` / `DoctorCheckId` / `DoctorCheck` / `DoctorReport` 定义在
 * `src/core/doctor/checks.ts`（W7 的文件）。P0 阶段该文件不存在，故下面按 §2.5 **逐字**
 * 声明同名类型，保证 `runDoctor` 的冻结签名在 P0 即可 typecheck。
 * W7 落地 `checks.ts` 后，删除下方本地声明，改为：
 *   ```ts
 *   import type { CheckStatus, DoctorCheck, DoctorCheckId, DoctorReport } from '../../core/doctor/checks.js';
 *   export type { CheckStatus, DoctorCheck, DoctorCheckId, DoctorReport };
 *   ```
 * 形状逐字相同，故**不是**接口变更；届时 `runDoctor` 的签名与调用方（index.ts）零改动。
 */

import { CliError } from '../../core/errors.js';
import type { AgeCryptoPort } from '../../core/age/types.js';

// ---- §2.5 checks.ts 的类型（P0 逐字副本，见文件头 TODO）----

export type CheckStatus = 'ok' | 'warn' | 'fail';
export type DoctorCheckId =
  | 'config' | 'repo' | 'store-clean' | 'remote' | 'adapters' | 'age' | 'machine' | 'required';
export interface DoctorCheck { id: DoctorCheckId; status: CheckStatus; message: string; details?: string[]; }
/** `ok` = 无 `fail`（`warn` 不影响 ok 与退出码）。 */
export interface DoctorReport { checks: DoctorCheck[]; ok: boolean; }

// ---- §2.5 命令层接口 ----

export interface DoctorOptions { homerHome?: string; json?: boolean; offline?: boolean; }
export interface DoctorDeps { age?: AgeCryptoPort; }

export const DOCTOR_USAGE = `用法: homer doctor [options]

对本机 homer 环境做八项体检，逐项报告 ok / warn / fail：
  config       homer.json 存在且合法
  repo         home 是 git 仓库根
  store-clean  store 工作区干净（脏 → warn）
  remote       远端可达（--offline 跳过；不可达 → warn）
  adapters     各 enabled adapter root 存在（缺失 → warn「工具未安装？」）
  age          密钥同步配置与身份（未配置 → ok；identity 缺失 / 不可解 → fail）
  machine      state 与 HEAD 一致（缺失 / 领先 → warn）
  required     store 中的 __REQUIRED__ 占位符残留（有 → warn）

选项:
  --home <dir>       homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --offline          跳过远端可达性检查
  --json             输出机器可读 JSON（DoctorReport）
  -h, --help         显示本帮助

退出码: 无 fail → 0（含仅 warn）；有 fail → 1。`;

/**
 * 顺序执行八项检查并聚合报告（§2.5 冻结顺序）。
 *
 * P0 stub：实现见 W7。`deps.age` 是 age 检查的注入位（与 W1 解耦，§4-4）。
 */
export async function runDoctor(opts: DoctorOptions, deps?: DoctorDeps): Promise<DoctorReport> {
  // P0 stub：参数按 §2.5 冻结签名保留（W7 实现时逐项消费）。
  void opts;
  void deps;
  throw new CliError('尚未实现');
}

/** 人类可读渲染（P0 预置：W7 细化措辞，无需改分发层）。状态标记 ok/warn/fail。 */
export function renderDoctorReport(report: DoctorReport): string {
  const mark: Record<CheckStatus, string> = { ok: '✓', warn: '⚠', fail: '✗' };
  const lines: string[] = [`homer doctor: ${report.ok ? '通过' : '存在问题'}`];
  for (const check of report.checks) {
    lines.push(`  ${mark[check.status]} ${check.id}  ${check.message}`);
    for (const detail of check.details ?? []) lines.push(`      - ${detail}`);
  }
  return lines.join('\n');
}
