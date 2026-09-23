/**
 * 密钥扫描类型（docs/m2-plan.md §2.2，P0 落地，**冻结**）。
 *
 * 仅类型定义；实现（patterns / scan / filterIgnored）在 P1-W1 落地。
 * 这里刻意不 import 任何运行时模块 —— 与 `core/types.ts` 同为纯类型模块。
 */

/** 单条密钥正则（`id` 稳定 = 报告与豁免匹配的键，`description` 供人类阅读）。 */
export interface SecretPattern {
  id: string;
  description: string;
  regex: RegExp;
}

/** 一条命中记录。 */
export interface SecretFinding {
  patternId: string;
  description: string;
  path: string;        // 'pi/settings/settings.json' 形式的 store 相对路径
  line: number;        // 1-based
  excerpt: string;     // 命中行（脱敏：命中串首尾各保留 4 字符，中段以 * 代替）
}
