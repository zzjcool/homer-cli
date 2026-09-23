/**
 * 密钥扫描（docs/m2-plan.md §2.2，P1-W1）。**纯函数、零 fs、零副作用。**
 *
 * 语义（冻结口径）：
 *  - 逐行扫描；每行按 `SECRET_PATTERNS` 顺序尝试，**首个命中即报告并停止该行**（「先命中先报」）。
 *    因此 `sk-ant-...` 只报 anthropic，不会连带报 openai；通用赋值（末条兜底）也不会
 *    覆盖同一行上已被专用 pattern 命中的串。
 *  - `line` 1-based（按 `\n` / `\r\n` / `\r` 切分）。
 *  - `excerpt` = 命中行原文，其中**命中串**脱敏为「首尾各留 4 字符，中段 `*`」；
 *    行内同一串的其它出现一并脱敏（避免逐条报告时重复泄露同一密钥）。
 *  - `path` = store 相对路径，形如 `pi/settings/settings.json`
 *    （= `${adapterId}/${category}/${relPath}`，与 store 布局 §1.6 同构）。
 *
 * 豁免 glob 语义复用 M1 冻结的 `matchesIgnore`（W1 计划明确要求从 adapters/pi/ignore 复用，
 * 不另写一套 glob）—— core → adapters 的这处 import 由 §2.2 指定，不是意外分层。
 */

import type { AdapterSnapshot } from '../types.js';
import type { SecretFinding } from './types.js';
import { SECRET_PATTERNS } from './patterns.js';
import { matchesIgnore } from '../../adapters/pi/ignore.js';

/** 脱敏保留的首/尾字符数（§2.2 冻结）。 */
const MASK_KEEP = 4;

/**
 * 单个命中串脱敏：首尾各 4 字符，中段 `*`；过短（≤8）则整体打码。
 *
 * 导出仅为让测试直接锁住短串的防御分支（12 条 pattern 的最小命中长度均 ≥20，
 * 经 `scanContent` 不可达）——**不属于** §2.2 冻结的 barrel 导出面（`index.ts` 不转发）。
 */
export function maskSecret(secret: string): string {
  if (secret.length <= MASK_KEEP * 2) return '*'.repeat(secret.length);
  const stars = '*'.repeat(secret.length - MASK_KEEP * 2);
  return `${secret.slice(0, MASK_KEEP)}${stars}${secret.slice(-MASK_KEEP)}`;
}

/** 命中行脱敏：把行内出现过的命中串统一替换为打码形态。 */
function maskLine(line: string, secret: string): string {
  const masked = maskSecret(secret);
  return line.split(secret).join(masked);
}

/**
 * 扫描单段文本内容，返回命中列表（按行号升序；每行至多一条）。
 */
export function scanContent(content: string, path: string): SecretFinding[] {
  const findings: SecretFinding[] = [];
  if (content.length === 0) return findings;

  const lines = content.split(/\r\n|\n|\r/);
  for (let i = 0; i < lines.length; i += 1) {
    const line = lines[i] as string;
    if (line.length === 0) continue;

    for (const pattern of SECRET_PATTERNS) {
      pattern.regex.lastIndex = 0; // 兼容调用方传 /g 正则的极端情况；本模块的 pattern 无 /g
      const match = pattern.regex.exec(line);
      if (match === null) continue;

      findings.push({
        patternId: pattern.id,
        description: pattern.description,
        path,
        line: i + 1,
        excerpt: maskLine(line, match[0]),
      });
      break; // 先命中先报：同一行不再尝试后续 pattern
    }
  }

  return findings;
}

/**
 * 扫描多个 adapter 快照，聚合为一份命中列表（跨 adapter / 跨分类）。
 *
 * 顺序确定性：adapter 按入参顺序，分类按入参顺序，文件按 relPath 排序 ——
 * 输出与快照 Map 的插入顺序无关，可安全用于快照（golden）断言。
 */
export function scanSnapshots(snapshots: readonly AdapterSnapshot[]): SecretFinding[] {
  const findings: SecretFinding[] = [];

  for (const snapshot of snapshots) {
    for (const category of snapshot.categories) {
      const relPaths = [...category.files.keys()].sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
      for (const relPath of relPaths) {
        const entry = category.files.get(relPath);
        if (entry === undefined) continue;
        const storePath = `${snapshot.adapterId}/${category.category}/${relPath}`;
        findings.push(...scanContent(entry.content, storePath));
      }
    }
  }

  return findings;
}

/**
 * `secrets.ignorePaths` 豁免（§2.2 / §2.4）：path 命中任一 glob 即丢弃该条命中。
 * glob 语义 = `matchesIgnore`（`*` 不跨 `/`；以 `/` 结尾 = 目录前缀整棵豁免）。
 * `undefined` / 空数组 → 原样返回（复制，不共享引用）。
 */
export function filterIgnored(
  findings: SecretFinding[],
  ignorePaths: string[] | undefined,
): SecretFinding[] {
  if (ignorePaths === undefined || ignorePaths.length === 0) return [...findings];
  return findings.filter((finding) => !matchesIgnore(finding.path, ignorePaths));
}
