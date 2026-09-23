/**
 * 冻结的密钥正则清单（docs/m2-plan.md §2.2，12 条，**顺序即语义**）。
 *
 * 「按序匹配，先命中先报」的落地解释（scan.ts）：
 *   逐行扫描，pattern 按本数组顺序尝试；同一行一旦有 pattern 命中即报告该条并停止
 *   对该行的后续尝试 —— 因此 `sk-ant-...` 只会报 anthropic，不会连带报 openai，
 *   而通用赋值（最后一条，兜底）也不会覆盖前面已命中的专用 pattern。
 *
 * 两条**逐字落地时的必要翻译**（语义等价，已显式记录）：
 *   1. 清单里的 `(?i)` 是文档写法；JS 不支持 inline flag，改为 RegExp 的 `i` 标志。
 *   2. 通用赋值里的 `/` 在字符类中写作 `\/`（JS 正则字面量的安全写法，语义不变）。
 *
 * 经实测，12 条之间**无相互遮蔽**（`sk_live_` 用下划线，不满足第 2 条 openai 的 `sk-` 前缀），
 * 因此每条专用 pattern 都是其正例的唯一命中；「先命中先报」只影响同一行上多密钥时的报告数量。
 *
 * 零运行时依赖：只 import 类型。
 */

import type { SecretPattern } from './types.js';

/** 12 条冻结 pattern（顺序 = §2.2 清单顺序；「先命中先报」的口径见文件头注释）。 */
export const SECRET_PATTERNS: readonly SecretPattern[] = Object.freeze([
  {
    id: 'anthropic-api-key',
    description: 'Anthropic API key (sk-ant-)',
    regex: /sk-ant-[A-Za-z0-9_-]{20,}/,
  },
  {
    id: 'openai-api-key',
    description: 'OpenAI API key (sk-)',
    regex: /sk-(?!ant-)[A-Za-z0-9_-]{20,}/,
  },
  {
    id: 'github-classic-token',
    description: 'GitHub token (classic, ghp_/gho_/ghu_/ghs_/ghr_)',
    regex: /gh[pousr]_[A-Za-z0-9]{36,}/,
  },
  {
    id: 'github-fine-grained-pat',
    description: 'GitHub fine-grained personal access token (github_pat_)',
    regex: /github_pat_[A-Za-z0-9_]{22,}/,
  },
  {
    id: 'aws-access-key-id',
    description: 'AWS access key ID (AKIA)',
    regex: /AKIA[0-9A-Z]{16}/,
  },
  {
    id: 'slack-token',
    description: 'Slack token (xox?-...)',
    regex: /xox[baprsce]-[A-Za-z0-9-]{10,}/,
  },
  {
    id: 'google-api-key',
    description: 'Google API key (AIza)',
    regex: /AIza[0-9A-Za-z_-]{35}/,
  },
  {
    id: 'gitlab-pat',
    description: 'GitLab personal access token (glpat-)',
    regex: /glpat-[A-Za-z0-9_-]{20,}/,
  },
  {
    id: 'npm-token',
    description: 'npm access token (npm_)',
    regex: /npm_[A-Za-z0-9]{36}/,
  },
  {
    id: 'stripe-live-key',
    description: 'Stripe live secret/restricted key (sk_live_/rk_live_)',
    regex: /[sr]k_live_[A-Za-z0-9]{20,}/,
  },
  {
    id: 'private-key-block',
    description: 'PEM private key block header',
    regex: /-----BEGIN (RSA |EC |OPENSSH |PGP |DSA )?PRIVATE KEY( BLOCK)?-----/,
  },
  {
    // 保守兜底：必须带 key/secret/token/password 上下文 + 引号包裹的 ≥20 字符值。
    // `__REQUIRED__`（12 字符）与 `${...}`（`$`/`{` 不在值字符集内）天然不命中。
    id: 'generic-secret-assignment',
    description: '保守通用密钥赋值（api_key/secret/token/password = "..."）',
    regex: /(api[_-]?key|secret|token|password)["']?\s*[:=]\s*["'][A-Za-z0-9+\/_-]{20,}["']/i,
  },
]);
