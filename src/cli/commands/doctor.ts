/**
 * `homer doctor` —— 八项体检（docs/m3-plan.md §2.5 / §1-D6，P2-W7 实现）。
 *
 * 本文件只做**编排**：顺序执行 `src/core/doctor/checks.ts` 的八项检查、聚合 `DoctorReport`、
 * 渲染人类可读文本。判定逻辑全部在 core 层（纯函数 + 薄 fs/git），命令层不复制判定。
 *
 * ## 八项检查（§2.5 / D6 冻结顺序）
 *
 *   config → repo / store-clean → remote → adapters → age → machine → required
 *
 * `config` fail 时**仍继续**其余检查（尽力而为），`ok=false`。
 * exit：无 `fail` → 0；有 `fail` → 1（`warn` 不影响退出码 —— 回应 m2-report §5-3）。
 *
 * ## 依赖注入
 *
 * `DoctorDeps.age` 是 age 检查的 `AgeCryptoPort` 注入位（§2.5 / §4-4）：真实运行走
 * `createAgeCryptoPort()`，单测可注入假 port 造出「identity 与密文不匹配 / 解密抛错」
 * 这类无法稳定复现的场景。port 的构造是廉价的（`cipher.ts` 延迟加载 age-encryption），
 * 且 `checkAge` 在未配置密钥同步时提前返回、根本不碰它。
 *
 * ## 与 P0 stub 的关系
 *
 * P0 时本文件按 §2.5 逐字声明了 `CheckStatus` / `DoctorCheckId` / `DoctorCheck` / `DoctorReport`
 * （`checks.ts` 尚未存在）。W7 落地 `checks.ts` 后改为**从这里 import 并 re-export**，
 * 形状逐字相同（不是接口变更），`runDoctor` 的签名与分发层（`index.ts`）零改动。
 */

import { loadConfig } from '../../core/config.js';
import { createAgeCryptoPort } from '../../core/age/index.js';
import {
  checkAdapters,
  checkAge,
  checkConfig,
  checkMachine,
  checkRemote,
  checkRepoAndStore,
  checkRequiredPlaceholders,
} from '../../core/doctor/checks.js';
import type { AgeCryptoPort } from '../../core/age/types.js';
import type { CheckStatus, DoctorCheck, DoctorCheckId, DoctorReport } from '../../core/doctor/checks.js';
import type { HomerConfig } from '../../core/types.js';
import { resolveHomerPaths } from '../render.js';

// §2.5 的四个类型现在归属 `core/doctor/checks.ts`（W7 落地）；此处 re-export 保持
// 「从 doctor 命令模块取类型」的既有 import 路径可用（P0 已落地行为）。
export type { CheckStatus, DoctorCheck, DoctorCheckId, DoctorReport };

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

/** 「跳过」检查（config 不可用时的降级项；`remote --offline` 用的是同一措辞套路）。 */
function skipped(id: DoctorCheckId, reason: string): DoctorCheck {
  return { id, status: 'ok', message: `已跳过（${reason}）` };
}

/**
 * 顺序执行八项检查并聚合报告（§2.5 冻结顺序）。
 *
 * 实现要点：
 *   - `checkConfig` 负责读 `homer.json` 并把「缺失 / 非法 JSON / 校验失败」区分为 fail；
 *     随后用 `loadConfig` **再读一次**（失败即抛）来拿结构化配置——这里的 catch 保证
 *     doctor 永远不因坏配置抛异常（诊断工具必须始终产出报告）。
 *   - config 不可用 → adapters / age / required 三项报「已跳过」而不是猜测。
 *     `ok` 已由 config 的 fail 决定为 false，跳过不会掩盖问题。
 *   - 其余检查（repo / store-clean / remote / machine）只依赖路径与 git，照常执行。
 */
export async function runDoctor(opts: DoctorOptions, deps?: DoctorDeps): Promise<DoctorReport> {
  const paths = resolveHomerPaths(opts.homerHome);
  const checks: DoctorCheck[] = [];

  // ① config（fail 也继续，尽力而为）
  const configCheck = checkConfig(paths);
  checks.push(configCheck);

  let config: HomerConfig | undefined;
  try {
    config = loadConfig(paths);
  } catch {
    config = undefined; // 坏配置已由 checkConfig 报告为 fail，这里只做降级。
  }

  // ② repo + ③ store-clean
  checks.push(...checkRepoAndStore(paths));

  // ④ remote（--offline → 跳过）
  checks.push(checkRemote(paths, { offline: opts.offline === true }));

  // ⑤ adapters
  checks.push(
    config === undefined
      ? skipped('adapters', 'config 不可用')
      : checkAdapters(config),
  );

  // ⑥ age（crypto 可注入；未配置密钥同步时 checkAge 提前返回，连 port 都不构造）
  checks.push(
    config === undefined
      ? skipped('age', 'config 不可用')
      : await checkAge(paths, config, deps?.age ?? createAgeCryptoPort()),
  );

  // ⑦ machine
  checks.push(checkMachine(paths));

  // ⑧ required（__REQUIRED__ 残留 → warn + 文件+键 details）
  checks.push(
    config === undefined
      ? skipped('required', 'config 不可用')
      : checkRequiredPlaceholders(paths, config),
  );

  return { checks, ok: checks.every((check) => check.status !== 'fail') };
}

/* ------------------------------------------------------------------ */
/* 渲染                                                                */
/* ------------------------------------------------------------------ */

const STATUS_MARK: Record<CheckStatus, string> = { ok: '✓', warn: '⚠', fail: '✗' };

/**
 * 人类可读渲染：逐项 `标记 id  消息`，`details` 缩进为 `- ` 子行；
 * 末尾附计数摘要（有 fail 时提示退出码语义）。纯函数，不碰 fs / stdout。
 */
export function renderDoctorReport(report: DoctorReport): string {
  const lines: string[] = [`homer doctor: ${report.ok ? '通过' : '存在问题'}`];

  for (const check of report.checks) {
    lines.push(`  ${STATUS_MARK[check.status]} ${check.id}  ${check.message}`);
    for (const detail of check.details ?? []) lines.push(`      - ${detail}`);
  }

  const count = (status: CheckStatus): number =>
    report.checks.filter((check) => check.status === status).length;
  lines.push(`  合计: ok ${count('ok')}  warn ${count('warn')}  fail ${count('fail')}`);

  if (!report.ok) {
    lines.push('  有 fail 项：修复后重跑 `homer doctor`（warn 不影响退出码）。');
  }

  return lines.join('\n');
}
