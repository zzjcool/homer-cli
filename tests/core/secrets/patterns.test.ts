/**
 * 冻结 patterns 清单的测试（docs/m2-plan.md §2.2 / §3-P1-W1）。
 *
 * 覆盖：清单条数/顺序/id 稳定性、12 条各 ≥1 正例 + ≥2 反例、以及验收点名的三处
 * 不误报场景（`sk-ant-` 不报 openai、`__REQUIRED__` 不命中、短 token / `${...}` 不命中）。
 *
 * 断言口径分三层，避免把「跨 pattern 命中」误判成误报：
 *  1. `expectNotHit(candidate, id)` —— 反例：候选串**不得命中该 pattern**（允许被别的
 *     专用 pattern 命中，如 `sk-ant-` 被 anthropic 命中而**不是** openai —— 这正是验收点）；
 *  2. `expectClean(candidate)` —— 强反例：不得命中**任何** pattern（短 token / 占位符 / 模板）；
 *  3. `expectHit(candidate, id)` —— 正例：必须命中，且额外命中必须在白名单内。
 *
 * 夹具构造约束：形似真实密钥的字符串一律**分段拼装**，源文件里不出现连续的 token 形态。
 * 否则 GitHub secret scanning push protection 会拦下整个分支（本仓库实测被拦过一次）。
 */

import { describe, expect, it } from 'vitest';

import { SECRET_PATTERNS } from '../../../src/core/secrets/index.js';

/** 长且仅含合法字符的测试主体（长度 > 任何 pattern 的下界）。 */
const BODY = 'A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0';

/** 分段拼装（见文件头：避免源文件出现连续 token 形态）。 */
function shape(...parts: string[]): string {
  return parts.join('');
}

/* ---- 形似密钥的夹具常量（全部拼装） ---- */

const ANTHROPIC = shape('sk-ant-', 'api03-', 'AbCdEfGhIjKlMnOpQrStUvWx');
const OPENAI_PROJ = shape('sk-', 'proj-', BODY);
const OPENAI_BARE = shape('sk-', BODY);
const GHP = shape('ghp_', BODY);
const GHS = shape('ghs_', BODY);
const FINE_GRAINED_PAT = shape(
  'github_',
  'pat_',
  '11AABBC0D1e2f3g4h5i6j7k8l9m0n1o2p3q4r5s6t7u',
);
const AWS_AK = shape('AKIA', 'IOSFODNN7EXAMPLE');
const SLACK = shape('xox', 'b-123456789012-', '987654321098-', 'AbCdEfGhIjKlMnOp');
const SLACK_SHORT = shape('xox', 'b-1234');
const SLACK_WRONG_PREFIX = shape('xox', 'z-123456789012345');
const GOOGLE = shape('AIza', BODY.slice(0, 35));
const GOOGLE_WRONG_PREFIX = shape('AIzb', BODY.slice(0, 35));
const GITLAB = shape('glpat-', 'AbCdEfGhIjKlMnOpQrSt');
const NPM = shape('npm_', BODY.slice(0, 36));
const NPM_WRONG_PREFIX = shape('npmx_', BODY.slice(0, 36));
const STRIPE_RK = shape('rk_', 'live_', BODY.slice(0, 20));
const STRIPE_SK = shape('sk_', 'live_', BODY.slice(0, 20));
const STRIPE_TEST = shape('sk_', 'test_', BODY.slice(0, 20));
const STRIPE_PK = shape('pk_', 'live_', BODY.slice(0, 20));
const PEM_RSA = shape('-----BEGIN ', 'RSA PRIVATE KEY-----');
const PEM_OPENSSH = shape('-----BEGIN ', 'OPENSSH PRIVATE KEY-----');
const GENERIC_ASSIGNMENT = shape('"api_key": "', BODY.slice(0, 24), '"');

/* ---- 命中判定辅助 ---- */

/** 一个候选串会命中哪些 pattern（按清单顺序）。 */
function hitIds(candidate: string): string[] {
  return SECRET_PATTERNS.filter((pattern) => {
    pattern.regex.lastIndex = 0;
    return pattern.regex.test(candidate);
  }).map((pattern) => pattern.id);
}

/** 正例：必须命中，且额外命中必须落在 allowExtra 内。 */
function expectHit(candidate: string, expectedId: string, allowExtra: string[] = []): void {
  const hits = hitIds(candidate);
  expect(hits, `应命中 ${expectedId}: ${candidate}`).toContain(expectedId);
  for (const hit of hits) {
    if (hit === expectedId) continue;
    expect(allowExtra, `${candidate} 额外命中 ${hit}`).toContain(hit);
  }
}

/** 反例（针对单条 pattern）：该 pattern 不得命中。 */
function expectNotHit(candidate: string, patternId: string): void {
  expect(hitIds(candidate), `${candidate} 不应命中 ${patternId}`).not.toContain(patternId);
}

/** 强反例：不得命中任何 pattern。 */
function expectClean(candidate: string): void {
  expect(hitIds(candidate), `不应命中任何 pattern: ${candidate}`).toEqual([]);
}

/* ---- 逐条 fixture ---- */

/** 12 条 pattern 各自的正例（≥1）。 */
const POSITIVES: Record<string, string[]> = {
  'anthropic-api-key': [ANTHROPIC],
  'openai-api-key': [OPENAI_PROJ, OPENAI_BARE],
  'github-classic-token': [GHP, GHS],
  'github-fine-grained-pat': [FINE_GRAINED_PAT],
  'aws-access-key-id': [AWS_AK],
  'slack-token': [SLACK],
  'google-api-key': [GOOGLE],
  'gitlab-pat': [GITLAB],
  'npm-token': [NPM],
  'stripe-live-key': [STRIPE_RK, STRIPE_SK],
  'private-key-block': [PEM_RSA, PEM_OPENSSH],
  'generic-secret-assignment': [GENERIC_ASSIGNMENT],
};

/** 12 条 pattern 各自的反例（≥2）：不得命中**该条**（可能被别的 pattern 命中，见文件头）。 */
const NEGATIVES: Record<string, string[]> = {
  'anthropic-api-key': [OPENAI_PROJ, 'sk-ant-short', 'anthropic-key-placeholder'],
  'openai-api-key': [ANTHROPIC, 'sk-short', 'sk-'],
  'github-classic-token': [shape('ghx_', BODY), 'ghp_short', 'gh_token='],
  'github-fine-grained-pat': ['github_pat_short', shape('github_', 'ent_', BODY.slice(0, 30))],
  'aws-access-key-id': ['AKIA1234', shape('AKIB', 'IOSFODNN7EXAMPLE')],
  'slack-token': [SLACK_WRONG_PREFIX, SLACK_SHORT],
  'google-api-key': ['AIzaShort', GOOGLE_WRONG_PREFIX],
  'gitlab-pat': ['glpat-short', shape('glpat_', 'AbCdEfGhIjKlMnOpQrSt')],
  'npm-token': ['npm_short', NPM_WRONG_PREFIX],
  'stripe-live-key': [STRIPE_TEST, STRIPE_PK],
  'private-key-block': ['-----BEGIN PUBLIC KEY-----', '-----BEGIN CERTIFICATE-----'],
  'generic-secret-assignment': [
    '"api_key": "__REQUIRED__"',
    '"api_key": "${ANTHROPIC_API_KEY}"',
    '"token": "short"',
  ],
};

describe('SECRET_PATTERNS — 清单结构与顺序', () => {
  it('恰好 12 条，id 唯一且非空', () => {
    expect(SECRET_PATTERNS).toHaveLength(12);
    const ids = SECRET_PATTERNS.map((p) => p.id);
    expect(new Set(ids).size).toBe(ids.length);
    for (const pattern of SECRET_PATTERNS) {
      expect(pattern.id.length).toBeGreaterThan(0);
      expect(pattern.description.length).toBeGreaterThan(0);
      expect(pattern.regex).toBeInstanceOf(RegExp);
    }
  });

  it('顺序 = §2.2 冻结清单顺序（anthropic → openai → … → 通用赋值兜底）', () => {
    expect(SECRET_PATTERNS.map((p) => p.id)).toEqual([
      'anthropic-api-key',
      'openai-api-key',
      'github-classic-token',
      'github-fine-grained-pat',
      'aws-access-key-id',
      'slack-token',
      'google-api-key',
      'gitlab-pat',
      'npm-token',
      'stripe-live-key',
      'private-key-block',
      'generic-secret-assignment',
    ]);
  });

  it('每条 pattern 都给出了 ≥1 正例 / ≥2 反例夹具（防漏测）', () => {
    for (const pattern of SECRET_PATTERNS) {
      expect(POSITIVES[pattern.id]?.length ?? 0, `${pattern.id} 缺正例`).toBeGreaterThanOrEqual(1);
      expect(NEGATIVES[pattern.id]?.length ?? 0, `${pattern.id} 缺反例`).toBeGreaterThanOrEqual(2);
    }
  });
});

describe('SECRET_PATTERNS — 12 条各 ≥1 正例', () => {
  for (const pattern of SECRET_PATTERNS) {
    it(`${pattern.id} 命中正例`, () => {
      for (const candidate of POSITIVES[pattern.id] ?? []) {
        expectHit(candidate, pattern.id);
      }
    });
  }

  it('aws / anthropic 正例的唯一命中就是自身（无误报）', () => {
    expect(hitIds(AWS_AK)).toEqual(['aws-access-key-id']);
    expect(hitIds(ANTHROPIC)).toEqual(['anthropic-api-key']);
  });

  it('sk_live_ / rk_live_ 各自只命中 stripe-live-key（无跨 pattern 遮蔽）', () => {
    // `sk_live_` 用下划线，不满足 openai 的 `sk-` 前缀（下划线与连字符不同）。
    expect(hitIds(STRIPE_SK)).toEqual(['stripe-live-key']);
    expect(hitIds(STRIPE_RK)).toEqual(['stripe-live-key']);
    expectNotHit(STRIPE_SK, 'openai-api-key');
  });
});

describe('SECRET_PATTERNS — 12 条各 ≥2 反例', () => {
  for (const pattern of SECRET_PATTERNS) {
    it(`${pattern.id} 的反例不命中自身`, () => {
      for (const candidate of NEGATIVES[pattern.id] ?? []) {
        expectNotHit(candidate, pattern.id);
      }
    });
  }
});

describe('SECRET_PATTERNS — 验收点名的误报场景', () => {
  it('sk-ant- 不误报为 openai（负向断言即 (?!ant-)）', () => {
    expectNotHit(ANTHROPIC, 'openai-api-key');
    expect(hitIds(ANTHROPIC)).toEqual(['anthropic-api-key']);
  });

  it('__REQUIRED__ 占位符不命中任何 pattern（含通用赋值）', () => {
    for (const candidate of [
      '"api_key": "__REQUIRED__"',
      '"apiKeys": "__REQUIRED__"',
      '"password": "__REQUIRED__"',
      'api_key = "__REQUIRED__"',
    ]) {
      expectClean(candidate);
    }
  });

  it('${...} 模板不命中通用赋值（值字符集不含 $ 与 {）', () => {
    for (const candidate of [
      '"api_key": "${ANTHROPIC_API_KEY}"',
      '"secret": "${env:TOKEN}"',
      '"token": "${TOKEN}"',
    ]) {
      expectClean(candidate);
    }
  });

  it('短 token（低于各 pattern 下界）不命中', () => {
    for (const candidate of [
      'sk-short',
      'ghp_short',
      'npm_short',
      'glpat-short',
      SLACK_SHORT,
      'AIzaShort',
      'AKIA1234',
      'github_pat_short',
      '"api_key": "short"',
    ]) {
      expectClean(candidate);
    }
  });

  it('通用赋值保守：无引号包裹的裸值不命中', () => {
    expectClean(shape('api_key=', BODY));
    expectClean(shape('"api_key": ', BODY));
  });

  it('普通配置文本不命中（误报回归）', () => {
    for (const candidate of [
      '{"theme": "dark", "model": "claude-sonnet-4"}',
      '"apiKeys": ["alice", "bob"]',
      'password = "hunter2"',
      'token: abc',
      'sk-ant-',
      '-----BEGIN PGP SIGNATURE-----',
    ]) {
      expectClean(candidate);
    }
  });
});
