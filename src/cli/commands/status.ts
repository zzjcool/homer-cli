/**
 * `homer status` —— 漂移概览（docs/m1-plan.md §1.7 / §2-P1-M4）。
 *
 * 冻结签名（§1.7）：
 *   StatusOptions / StatusReport / runStatus(opts): StatusReport
 *
 * M1 的 base / remote 来源（§1.4「remote 缺省 = base」）：
 *   base   = store 快照（readSnapshotFromStore，即最后一次同步态）
 *   local  = 实时扫描各 enabled adapter 的 root
 *   remote = M1 缺省 = base；**M2-W10 起**由 `collectSnapshotSources` 注入：git 仓库存在
 *            且 upstream 可读时 = upstream 快照（`↓` 计数由此激活，关闭 M1 已知限制 #1），
 *            否则回落 = base。
 *
 * `sources` 是第二参数（可选，生产不传）：允许调用方（单测 / 未来接 git remote 的 M2）
 * 注入三方快照，保持 §1.7 的单参签名依然可用。
 */

import { computeDrift, type AdapterDrift } from '../../core/engine/index.js';
import { loadConfig } from '../../core/config.js';
import {
  CliError,
  collectSnapshotSources,
  resolveHomerPaths,
  sourceErrorMessages,
  type CliDriftSources,
} from '../render.js';

export interface StatusOptions {
  homerHome?: string;
  json?: boolean;
  verbose?: boolean;
}

export interface StatusReport {
  adapters: {
    id: string;
    push: number;
    pull: number;
    conflicts: number;
    categories: { name: string; push: number; pull: number; conflicts: number }[];
  }[];
  /**
   * 采集期错误（additive，M-A）：live scan 读不到 adapter root 时的告警文本。
   * 空数组 = 无告警；`--json` 一并输出，文本输出渲染为 ⚠ 头行。
   * 漂移仍是信息，exit 码保持 0。
   */
  errors: string[];
  /**
   * 采集期告警（additive，M2-W10）：非致命但用户必须看见的事实。
   * 目前唯一来源是「store 工作区脏」（直改 store 后的 ↓ 计数说明），同样渲染为置顶 ⚠ 行。
   *
   * **仅在非空时出现**（`undefined` = 无告警）：保持 M1 冻结的 StatusReport 形状在
   * 常规路径上逐字不变，`--json` 消费者用 `report.warnings?.length` 判定。
   */
  warnings?: string[];
}

export const STATUS_USAGE = `用法: homer status [options]

显示本地（实时扫描）与 store 快照之间的漂移概览。
↑ = 本地相对 base 的变更（可 push）；↓ = 远端相对 base 的变更（可 pull）。

选项:
  --home <dir>       homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --json             输出机器可读 JSON（StatusReport）
  --verbose, -v      逐分类打印计数
  -h, --help         显示本帮助

漂移是信息而非错误：即使有漂移也以 0 退出；只有配置缺失等真错误才退出 1。`;

/** 把引擎的 AdapterDrift[] 聚合为 §1.7 冻结的 StatusReport（`errors` / `warnings` 为 additive 字段）。 */
export function buildStatusReport(
  drifts: readonly AdapterDrift[],
  errors: string[] = [],
  warnings: readonly string[] = [],
): StatusReport {
  const report: StatusReport = {
    errors,
    adapters: drifts.map((adapter) => {
      let push = 0;
      let pull = 0;
      let conflicts = 0;
      const categories = adapter.categories.map((category) => {
        push += category.push;
        pull += category.pull;
        conflicts += category.conflicts;
        return {
          name: category.category,
          push: category.push,
          pull: category.pull,
          conflicts: category.conflicts,
        };
      });
      return { id: adapter.adapterId, push, pull, conflicts, categories };
    }),
  };

  // `warnings` 只在非空时出现（additive，M2-W10）：这样 M1 冻结的 StatusReport 形状在
  // 「没有任何告警」的常规路径上逐字不变（既有 --json 消费者与快照测试不受影响），
  // 而带告警时 `--json` 消费者可用 `report.warnings?.length` 判定。
  if (warnings.length > 0) report.warnings = [...warnings];

  return report;
}

/**
 * 计算并返回漂移报告。
 * 无 homer.json → 抛 CliError（分发层捕获后提示先 `homer init`，exit 1）。
 */
export function runStatus(opts: StatusOptions, sources?: CliDriftSources): StatusReport {
  const paths = resolveHomerPaths(opts.homerHome);
  const config = loadConfig(paths);
  if (config === undefined) {
    throw new CliError(
      `未找到 homer 配置: ${paths.configFile}`,
      '请先运行 `homer init` 生成 homer.json 与 store 快照。',
    );
  }

  const src: CliDriftSources = sources ?? collectSnapshotSources(paths, config);
  return buildStatusReport(
    computeDrift(src.base, src.local, src.remote),
    sourceErrorMessages(src.errors ?? []),
    src.warnings ?? [],
  );
}
