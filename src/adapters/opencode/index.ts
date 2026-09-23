export { OPENCODE_ADAPTER_ID, DEFAULT_OPENCODE_ADAPTER } from './defaults.js';
/**
 * 扫描实现直接复用 pi adapter 的通用 `scanAdapter`（docs/m3-plan.md §2.8）。
 *
 * 该函数签名 `(adapterId, config: AdapterConfig)` 本就 adapter 无关：分类与 ignore 全部由
 * config 声明驱动（分类数量/模式/目录型 vs 单文件型；ignore 为相对 root 的路径 glob）。
 * 因此 opencode 侧**零新增扫描逻辑**，只做 re-export —— 任何扫描语义修正自动对全部 adapter 生效。
 */
export { scanAdapter } from '../pi/index.js';
export type { ScanError, ScanOutcome } from '../pi/index.js';
