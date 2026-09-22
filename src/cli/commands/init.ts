/**
 * `homer init` —— 扫描 adapter 生成 homer.json + store 快照（docs/m1-plan.md §1.7 / §2-P1-M4）。
 *
 * 冻结签名（§1.7）：
 *   InitOptions / InitReport / runInit(opts): Promise<InitReport>
 *
 * M1 最小版：非交互（交互式勾选在 §2.8 / M2+ 接入）。
 * 已存在 homer.json → 拒绝覆盖（CliError + exit 1），`force: true` 才允许覆盖
 * （`force` 走第二个可选参数，不改冻结的 InitOptions）。
 */

import fs from 'node:fs';

import type { AdapterConfig, AdapterSnapshot, HomerConfig } from '../../core/types.js';
import { saveConfig } from '../../core/config.js';
import { writeSnapshotToStore } from '../../core/store/store.js';
import { DEFAULT_PI_ADAPTER, PI_ADAPTER_ID, scanAdapter } from '../../adapters/pi/index.js';
import { CliError, resolveHomerPaths } from '../render.js';

export interface InitOptions {
  homerHome?: string;
  adapters?: string[];
  json?: boolean;
}

export interface InitReport {
  homerHome: string;
  adapters: { id: string; categories: { name: string; fileCount: number }[] }[];
}

/** M1 已知 adapter 注册表（后续 adapter 在此登记）。 */
const KNOWN_ADAPTERS: Record<string, AdapterConfig> = {
  [PI_ADAPTER_ID]: DEFAULT_PI_ADAPTER,
};

export interface InitRunOptions {
  /** 覆盖已存在的 homer.json（对应 CLI `--force`）。 */
  force?: boolean;
}

export const INIT_USAGE = `用法: homer init [options]

扫描已启用的 adapter，生成 homer.json 并把初始快照写入 store。

选项:
  --home <dir>          homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --adapters <ids>      只初始化指定 adapter（逗号分隔，默认全部已知 adapter）
  --force               覆盖已存在的 homer.json
  --json                输出机器可读 JSON（InitReport）
  -h, --help            显示本帮助

注意: 已存在 homer.json 时会拒绝覆盖并以退出码 1 结束，除非显式给出 --force。`;

/** 依据 `opts.adapters` 组装本次要初始化的 adapter 集合（保持已知注册表顺序）。 */
function selectAdapters(ids: string[] | undefined): Record<string, AdapterConfig> {
  if (ids === undefined || ids.length === 0) return { ...KNOWN_ADAPTERS };

  const selected: Record<string, AdapterConfig> = {};
  for (const id of ids) {
    const config = KNOWN_ADAPTERS[id];
    if (config === undefined) {
      throw new CliError(
        `未知 adapter: ${id}`,
        `M1 已知 adapter: ${Object.keys(KNOWN_ADAPTERS).join(', ')}`,
      );
    }
    selected[id] = config;
  }
  return selected;
}

/**
 * 扫描 → 写 store → 写 homer.json，返回 InitReport。
 * 已存在 homer.json 且未 `force` → CliError（分发层提示用户，exit 1）。
 */
export async function runInit(opts: InitOptions, runOpts: InitRunOptions = {}): Promise<InitReport> {
  const paths = resolveHomerPaths(opts.homerHome);

  if (fs.existsSync(paths.configFile) && runOpts.force !== true) {
    throw new CliError(
      `已存在 homer 配置: ${paths.configFile}`,
      '拒绝覆盖。如需重新初始化，请加 `--force`（会覆盖 homer.json 与 store 快照）。',
    );
  }

  const adapters = selectAdapters(opts.adapters);
  const config: HomerConfig = { version: 1, adapters };

  const reportAdapters: InitReport['adapters'] = [];
  for (const [adapterId, adapterConfig] of Object.entries(adapters)) {
    const outcome = scanAdapter(adapterId, adapterConfig);
    const snapshot: AdapterSnapshot = outcome.snapshot;
    writeSnapshotToStore(paths, snapshot);

    reportAdapters.push({
      id: adapterId,
      categories: snapshot.categories.map((category) => ({
        name: category.category,
        fileCount: category.files.size,
      })),
    });
  }

  saveConfig(paths, config);

  return { homerHome: paths.home, adapters: reportAdapters };
}
