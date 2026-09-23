/**
 * `scanContent` / `scanSnapshots` / `filterIgnored` 测试（docs/m2-plan.md §2.2 / §3-P1-W1）。
 *
 * 覆盖验收点：1-based 行号、excerpt 脱敏（首尾各 4 字符，中段 `*`）、同一行「先命中先报」、
 * 跨 adapter / 跨分类聚合 + 确定性顺序、`secrets.ignorePaths` 豁免（matchesIgnore 语义）。
 *
 * 纯函数测试：零 fs、零 HOME 依赖。
 *
 * 夹具同样**分段拼装**（理由见 patterns.test.ts 文件头：GitHub push protection 会拦连续 token 形态）。
 */

import { describe, expect, it } from 'vitest';

import type { AdapterSnapshot, CategorySnapshot, SnapshotEntry } from '../../../src/core/types.js';
import {
  filterIgnored,
  scanContent,
  scanSnapshots,
  type SecretFinding,
} from '../../../src/core/secrets/index.js';
import { maskSecret } from '../../../src/core/secrets/scan.js';

const ANTHROPIC = ['sk-ant-', 'api03-', 'AbCdEfGhIjKlMnOpQrStUvWx'].join(''); // len 37
const GHP = ['ghp_', 'A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0'].join(''); // len 44
const SLACK = ['xox', 'b-123456789012-', '987654321098'].join('');
const STORE_PATH = 'pi/settings/settings.json';

/** 期望的脱敏形态（独立实现，避免直接复用被测逻辑）。 */
function masked(secret: string): string {
  return `${secret.slice(0, 4)}${'*'.repeat(secret.length - 8)}${secret.slice(-4)}`;
}

function entry(content: string): SnapshotEntry {
  return { kind: 'file', content };
}

function category(
  adapterId: string,
  name: string,
  files: Record<string, string>,
): CategorySnapshot {
  const map = new Map<string, SnapshotEntry>();
  for (const [relPath, content] of Object.entries(files)) map.set(relPath, entry(content));
  return { adapterId, category: name, mode: 'mirror', files: map };
}

function snapshot(adapterId: string, categories: CategorySnapshot[]): AdapterSnapshot {
  return { adapterId, categories };
}

describe('scanContent — 基本命中与行号', () => {
  it('干净内容 → 空数组', () => {
    expect(scanContent('{\n  "theme": "dark"\n}\n', STORE_PATH)).toEqual([]);
    expect(scanContent('', STORE_PATH)).toEqual([]);
  });

  it('命中行号为 1-based（第 1 / 第 3 行）', () => {
    const content = ['{', `  "apiKey": "${ANTHROPIC}",`, '}'].join('\n');
    const findings = scanContent(content, STORE_PATH);
    expect(findings).toHaveLength(1);
    expect(findings[0]?.line).toBe(2);
    expect(findings[0]?.patternId).toBe('anthropic-api-key');
    expect(findings[0]?.path).toBe(STORE_PATH);
  });

  it('首个字符即命中 → line === 1', () => {
    const findings = scanContent(`token=${GHP}`, STORE_PATH);
    expect(findings[0]?.line).toBe(1);
    expect(findings[0]?.patternId).toBe('github-classic-token');
  });

  it('CRLF / CR 换行同样给出正确行号', () => {
    const crlf = scanContent(`{"a": 1}\r\n{"b": "${ANTHROPIC}"}\r\n`, STORE_PATH);
    expect(crlf).toHaveLength(1);
    expect(crlf[0]?.line).toBe(2);

    const cr = scanContent(`{"a": 1}\r{"b": "${ANTHROPIC}"}`, STORE_PATH);
    expect(cr).toHaveLength(1);
    expect(cr[0]?.line).toBe(2);
  });

  it('多行多处命中 → 按行号升序、每处一条', () => {
    const content = [
      `key1 = "${ANTHROPIC}"`,
      'clean line',
      `key2 = "${GHP}"`,
    ].join('\n');
    const findings = scanContent(content, STORE_PATH);
    expect(findings.map((f) => [f.line, f.patternId])).toEqual([
      [1, 'anthropic-api-key'],
      [3, 'github-classic-token'],
    ]);
  });

  it('同一行多个不同 pattern → 只报先命中的那一条（先命中先报）', () => {
    const findings = scanContent(`${ANTHROPIC} and ${GHP} on one line`, STORE_PATH);
    expect(findings).toHaveLength(1);
    expect(findings[0]?.patternId).toBe('anthropic-api-key');
  });

  it('path 原样透传（store 相对路径）', () => {
    const findings = scanContent(`"x": "${ANTHROPIC}"`, 'pi/models/models.json');
    expect(findings[0]?.path).toBe('pi/models/models.json');
  });
});

describe('scanContent — excerpt 脱敏', () => {
  it('命中串首尾各留 4 字符，中段全部为 *', () => {
    const findings = scanContent(`  "apiKey": "${ANTHROPIC}",`, STORE_PATH);
    expect(findings[0]?.excerpt).toBe(`  "apiKey": "${masked(ANTHROPIC)}",`);
  });

  it('脱敏后摘要中不含原始密钥的完整串', () => {
    const findings = scanContent(`"apiKey": "${ANTHROPIC}"`, STORE_PATH);
    const excerpt = findings[0]?.excerpt ?? '';
    expect(excerpt).not.toContain(ANTHROPIC);
    expect(excerpt).toContain('sk-a');
    expect(excerpt).toContain('UvWx');
    expect(excerpt).toContain('*'.repeat(ANTHROPIC.length - 8));
  });

  it('星号数量 === 密钥长度 - 8', () => {
    const findings = scanContent(GHP, STORE_PATH);
    const stars = (findings[0]?.excerpt.match(/\*/g) ?? []).length;
    expect(stars).toBe(GHP.length - 8);
  });

  it('同一行同一密钥出现多次 → 全部脱敏（不残留明文）', () => {
    const findings = scanContent(`"a": "${ANTHROPIC}", "b": "${ANTHROPIC}"`, STORE_PATH);
    expect(findings).toHaveLength(1);
    const excerpt = findings[0]?.excerpt ?? '';
    expect(excerpt).not.toContain(ANTHROPIC);
    expect(excerpt.split('*'.repeat(ANTHROPIC.length - 8))).toHaveLength(3);
  });

  it('密钥超长时仍只保留首尾 4 字符', () => {
    const long = `${ANTHROPIC}${'Z'.repeat(50)}`;
    const findings = scanContent(`"k": "${long}"`, STORE_PATH);
    const excerpt = findings[0]?.excerpt ?? '';
    expect(excerpt).toContain(`${long.slice(0, 4)}${'*'.repeat(long.length - 8)}${long.slice(-4)}`);
  });
});

describe('maskSecret — 边界', () => {
  it('长度 ≤ 8 → 整体打码', () => {
    expect(maskSecret('abc')).toBe('***');
    expect(maskSecret('abcdefgh')).toBe('********');
  });

  it('长度 9 → 首尾各 4 + 1 个 *', () => {
    expect(maskSecret('123456789')).toBe('1234*6789');
  });

  it('长度为 0 → 空串', () => {
    expect(maskSecret('')).toBe('');
  });
});

describe('scanSnapshots — 跨 adapter / 跨分类聚合', () => {
  it('聚合多个 adapter 的命中，path 为 <adapter>/<category>/<relPath>', () => {
    const pi = snapshot('pi', [
      category('pi', 'settings', { 'settings.json': `"apiKey": "${ANTHROPIC}"` }),
      category('pi', 'models', { 'models.json': `"ghToken": "${GHP}"` }),
    ]);
    const herdr = snapshot('herdr', [
      category('herdr', 'settings', { 'settings.json': `"xox": "${SLACK}"` }),
    ]);

    const findings = scanSnapshots([pi, herdr]);
    expect(findings.map((f) => f.path)).toEqual([
      'pi/settings/settings.json',
      'pi/models/models.json',
      'herdr/settings/settings.json',
    ]);
    expect(findings.map((f) => f.patternId)).toEqual([
      'anthropic-api-key',
      'github-classic-token',
      'slack-token',
    ]);
  });

  it('同分类下多文件按 relPath 排序 → 输出确定性（不受 Map 插入顺序影响）', () => {
    const b = category('pi', 'skills', { 'zz/SKILL.md': `"${ANTHROPIC}"` });
    const a = category('pi', 'skills', { 'aa/SKILL.md': `"${GHP}"` });
    const findings = scanSnapshots([snapshot('pi', [b, a])]);
    expect(findings.map((f) => f.path)).toEqual([
      'pi/skills/zz/SKILL.md',
      'pi/skills/aa/SKILL.md',
    ]);
  });

  it('嵌套 relPath 保持 posix 分隔符', () => {
    const findings = scanSnapshots([
      snapshot('pi', [category('pi', 'skills', { 'foo/skills/bar/SKILL.md': ANTHROPIC })]),
    ]);
    expect(findings[0]?.path).toBe('pi/skills/foo/skills/bar/SKILL.md');
  });

  it('干净快照 → 空数组；空数组入参 → 空数组', () => {
    expect(scanSnapshots([snapshot('pi', [category('pi', 'settings', { 'settings.json': '{}' })])]))
      .toEqual([]);
    expect(scanSnapshots([])).toEqual([]);
  });

  it('同一行内命中只报一条，但不同行分别报（跨 file / 跨 category 不丢）', () => {
    const s = snapshot('pi', [
      category('pi', 'settings', { 'settings.json': `a\nb\nc\n"k": "${ANTHROPIC}"` }),
      category('pi', 'models', { 'models.json': `"k": "${GHP}"` }),
    ]);
    const findings = scanSnapshots([s]);
    expect(findings).toHaveLength(2);
    expect(findings[0]).toMatchObject({ line: 4, patternId: 'anthropic-api-key' });
    expect(findings[1]).toMatchObject({ line: 1, patternId: 'github-classic-token' });
  });
});

describe('filterIgnored — secrets.ignorePaths 豁免', () => {
  const findings: SecretFinding[] = [
    { patternId: 'a', description: 'A', path: 'pi/settings/settings.json', line: 1, excerpt: 'x' },
    { patternId: 'b', description: 'B', path: 'pi/skills/foo/SKILL.md', line: 2, excerpt: 'y' },
    { patternId: 'c', description: 'C', path: 'herdr/settings/settings.json', line: 3, excerpt: 'z' },
  ];

  it('undefined / 空数组 → 原样返回（新数组，不共享引用）', () => {
    const kept = filterIgnored(findings, undefined);
    expect(kept).toEqual(findings);
    expect(kept).not.toBe(findings);

    expect(filterIgnored(findings, [])).toEqual(findings);
  });

  it('精确路径 glob 只豁免该文件', () => {
    const kept = filterIgnored(findings, ['pi/settings/settings.json']);
    expect(kept.map((f) => f.path)).toEqual([
      'pi/skills/foo/SKILL.md',
      'herdr/settings/settings.json',
    ]);
  });

  it('`*` 不跨 `/`（matchesIgnore 语义）：pi/*/*.json 只命中同层级路径', () => {
    const findings: SecretFinding[] = [
      { patternId: 'a', description: 'A', path: 'pi/settings.json', line: 1, excerpt: 'x' },
      { patternId: 'b', description: 'B', path: 'pi/models/models.json', line: 2, excerpt: 'y' },
      { patternId: 'c', description: 'C', path: 'herdr/settings/settings.json', line: 3, excerpt: 'z' },
    ];

    // pi/*/*.json 匹配三段路径（pi/x/y.json），不跨更深的层级
    const kept = filterIgnored(findings, ['pi/*/*.json']);
    expect(kept.map((f) => f.path)).toEqual(['pi/settings.json', 'herdr/settings/settings.json']);

    // */*/settings.json 匹配三段路径，不跨更深的层级
    const deeper: SecretFinding[] = [
      { patternId: 'a', description: 'A', path: 'pi/settings/settings.json', line: 1, excerpt: 'x' },
      { patternId: 'b', description: 'B', path: 'pi/skills/foo/SKILL.md', line: 2, excerpt: 'y' },
      { patternId: 'c', description: 'C', path: 'herdr/settings/settings.json', line: 3, excerpt: 'z' },
    ];
    const kept2 = filterIgnored(deeper, ['*/*/settings.json']);
    expect(kept2.map((f) => f.path)).toEqual(['pi/skills/foo/SKILL.md']);
  });

  it('单段 `*` 不跨 `/`：pi/* 只命中两层以内的路径', () => {
    // my-inner 里的 findings 全是 ≥3 段路径，因此 'pi/*' 一条也不豁免
    expect(filterIgnored(findings, ['pi/*']).map((f) => f.path)).toEqual(findings.map((f) => f.path));
  });

  it('目录前缀 glob（尾 `/`）豁免整棵子树', () => {
    const kept = filterIgnored(findings, ['pi/']);
    expect(kept.map((f) => f.path)).toEqual(['herdr/settings/settings.json']);

    const kept2 = filterIgnored(findings, ['pi/skills/']);
    expect(kept2.map((f) => f.path)).toEqual([
      'pi/settings/settings.json',
      'herdr/settings/settings.json',
    ]);
  });

  it('多条 glob 任一命中即豁免', () => {
    const kept = filterIgnored(findings, ['pi/settings/settings.json', 'herdr/']);
    expect(kept.map((f) => f.path)).toEqual(['pi/skills/foo/SKILL.md']);
  });

  it('不匹配的 glob → 一条都不豁免', () => {
    expect(filterIgnored(findings, ['opencode/'])).toEqual(findings);
  });

  it('与 scanSnapshots 串联：豁免后命中数下降', () => {
    const s = snapshot('pi', [
      category('pi', 'settings', { 'settings.json': `"k": "${ANTHROPIC}"` }),
      category('pi', 'skills', { 'SKILL.md': `"k": "${GHP}"` }),
    ]);
    const all = scanSnapshots([s]);
    expect(all).toHaveLength(2);
    const kept = filterIgnored(all, ['pi/skills/']);
    expect(kept).toHaveLength(1);
    expect(kept[0]?.path).toBe('pi/settings/settings.json');
  });
});
