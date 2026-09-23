/**
 * minor 5（对抗式 review）：README 的「密钥扫描口径」段落必须与 `scanContent` 的**实际行为**一致。
 *
 * 单有文档句子不够——文档会漂移。这里把 README 的两条自称口径拿真实扫描器**重放**：
 *   1. 未引号赋值（`KEY=value` 风格）不在通用兜底 pattern 的拦截范围（带引号才拦）；
 *   2. 同行多密钥「先命中先报」（只报第一条）。
 * 任一条被改正则 / 改文档而另一侧没跟 → 这个测试红。
 */

import { readFileSync } from 'node:fs';
import path from 'node:path';

import { describe, expect, it } from 'vitest';

import { scanContent } from '../../src/core/secrets/index.js';

const README = readFileSync(path.join(process.cwd(), 'README.md'), 'utf8');

/** 假 token：分段拼装，源码里不出现完整字面量。 */
const ANTHROPIC = ['sk', '-ant-', 'api03', '-', 'readme', '-', 'notareal', '-', '0123456789'].join('');
const GITHUB = ['ghp', '_', 'ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789'].join('');
const GENERIC_VALUE = 'abcdefghij0123456789abcdefghij';

describe('minor 5 · README 密钥扫描口径与实现一致', () => {
  it('README 含「密钥扫描口径」段落，并写明两条边界', () => {
    expect(README).toContain('密钥扫描口径');
    expect(README).toContain('未引号的赋值不在拦截范围');
    expect(README).toContain('同行多密钥先命中先报');
  });

  it('口径 1：未引号 `KEY=value` 不被通用兜底命中；带引号才命中', () => {
    // 与 README 的自称一致：不带引号的通用赋值不拦。
    expect(scanContent(`API_KEY=${GENERIC_VALUE}`, 'p.md')).toHaveLength(0);
    expect(scanContent(`API_KEY=${GENERIC_VALUE}`, 'p.md')).toEqual([]);
    // 带引号（单引号 / 双引号）命中通用兜底。
    expect(scanContent(`api_key: "${GENERIC_VALUE}"`, 'p.md').map((f) => f.patternId)).toEqual([
      'generic-secret-assignment',
    ]);
    expect(scanContent(`secret='${GENERIC_VALUE}'`, 'p.md').map((f) => f.patternId)).toEqual([
      'generic-secret-assignment',
    ]);
  });

  it('口径 1 的例外：带具体前缀的 pattern 不受引号限制（未引号也命中）', () => {
    // README 明确写「带具体前缀的 12 类 pattern 不受此限制」。
    expect(scanContent(`API_KEY=${ANTHROPIC}`, 'p.md').map((f) => f.patternId)).toEqual([
      'anthropic-api-key',
    ]);
  });

  it('口径 2：同行多密钥先命中先报（每行至多一条）', () => {
    const findings = scanContent(`a ${ANTHROPIC} b ${GITHUB}`, 'p.md');
    expect(findings).toHaveLength(1);
    expect(findings[0]?.patternId).toBe('anthropic-api-key');
  });
});
