/**
 * 密钥扫描模块 barrel（docs/m2-plan.md §2.2）。
 *
 * 导出面（冻结，供 W6 的 base.ts / W7 的 push 命令消费）：
 *   SECRET_PATTERNS / scanContent / scanSnapshots / filterIgnored
 * 类型（P0 冻结于 types.ts）一并转发，使消费者只 import 本 barrel。
 */

export type { SecretFinding, SecretPattern } from './types.js';
export { SECRET_PATTERNS } from './patterns.js';
export { filterIgnored, scanContent, scanSnapshots } from './scan.js';
