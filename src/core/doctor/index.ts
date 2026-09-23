/**
 * `src/core/doctor/` 公共门面（docs/m3-plan.md §2.5 / §3-P2-W7）。
 *
 * 命令层（`src/cli/commands/doctor.ts`）与 P3-W8 的 `home` 都从这里 import，
 * 不直接深入 `checks.ts`，使「八项检查怎么拆文件」可自由重构。
 *
 * 显式逐项导出（不用 `export *`）：与 `core/age/index.ts` 同一约定——避免同名符号
 * 在 barrel 里产生歧义，也让「对外契约」一眼可数。
 */

export type { CheckStatus, DoctorCheck, DoctorCheckId, DoctorReport } from './checks.js';

export {
  checkAdapters,
  checkAge,
  checkConfig,
  checkMachine,
  checkRemote,
  checkRepoAndStore,
  checkRequiredPlaceholders,
  REMOTE_CHECK_TIMEOUT_MS,
} from './checks.js';
