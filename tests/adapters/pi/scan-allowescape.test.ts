/**
 * P1-W3 · symlink 逃逸 allowlist（docs/m3-plan.md §1-D7 / §3-P1-W3，M1 已知限制的产品决策关闭）。
 *
 * 语义（冻结）：
 *   - `AdapterConfig.allowEscape?: string[]`：相对 **adapter root** 的 glob，判定用 `matchesIgnore`
 *     同一套语义（字面量逐字 + `*` 不跨 `/` + 尾 `/` = 目录前缀；不支持 `?` / `**`）；
 *   - 缺省 / 空数组 → 维持 M1 安全边界：跳过 + 记 ScanError；
 *   - 命中 → 跟随该 symlink（**只放行这一条链接**，其内部再次逃逸的链接需另写模式）；
 *   - 悬空跳过、回环截断（visited）行为**不变**。
 *
 * 全部 fixture 在 `mkdtemp` 下构造（root 与「外部」目录同级），绝不碰真实 HOME。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { DEFAULT_PI_ADAPTER, PI_ADAPTER_ID, scanAdapter } from '../../../src/adapters/pi/index.js';
import type { AdapterSnapshot, CategorySnapshot } from '../../../src/core/types.js';

let tmp: string;
/** adapter root 之外的「逃逸目标」目录（与 root 同级）。 */
let outside: string;
/** 第二个外部目录（测「放行链接内部再次逃逸」）。 */
let outside2: string;

beforeEach(() => {
  tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-pi-allowescape-'));
  outside = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-pi-allowescape-out-'));
  outside2 = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-pi-allowescape-out2-'));
});

afterEach(() => {
  for (const dir of [tmp, outside, outside2]) fs.rmSync(dir, { recursive: true, force: true });
});

function write(base: string, rel: string, content: string): void {
  const abs = path.join(base, rel);
  fs.mkdirSync(path.dirname(abs), { recursive: true });
  fs.writeFileSync(abs, content, 'utf8');
}

function scan(allowEscape?: string[]): ReturnType<typeof scanAdapter> {
  const config =
    allowEscape === undefined
      ? { ...DEFAULT_PI_ADAPTER, root: tmp }
      : { ...DEFAULT_PI_ADAPTER, root: tmp, allowEscape };
  return scanAdapter(PI_ADAPTER_ID, config);
}

function cat(snapshot: AdapterSnapshot, name: string): CategorySnapshot {
  const found = snapshot.categories.find((c) => c.category === name);
  if (!found) throw new Error(`category not found: ${name}`);
  return found;
}

function keysOf(snapshot: AdapterSnapshot, name: string): string[] {
  return [...cat(snapshot, name).files.keys()].sort();
}

/** 逃逸相关的 ScanError（按路径筛）。 */
function escapeErrors(errors: { path: string; message: string }[], needle: string) {
  return errors.filter((e) => e.path.includes(needle) && /逃逸/.test(e.message));
}

/* ------------------------------------------------------------------ */
/* 1. 目录型逃逸链接                                                    */
/* ------------------------------------------------------------------ */

describe('allowEscape — 目录型逃逸 symlink', () => {
  beforeEach(() => {
    write(tmp, 'skills/foo/SKILL.md', '# foo\n');
    write(outside, 'browser/SKILL.md', '# agent-browser\n');
    write(outside, 'browser/sub/a.ts', 'export const a = 1;\n');
    fs.mkdirSync(path.join(tmp, 'skills'), { recursive: true });
    fs.symlinkSync(path.join(outside, 'browser'), path.join(tmp, 'skills', 'agent-browser'));
  });

  it('无 allowlist（缺省）→ 跳过 + ScanError，外部内容不进快照', () => {
    const { snapshot, errors } = scan();

    expect(keysOf(snapshot, 'skills')).toEqual(['foo/SKILL.md']);
    const contents = [...cat(snapshot, 'skills').files.values()].map((e) => e.content).join('\n');
    expect(contents).not.toContain('agent-browser');
    expect(escapeErrors(errors, 'agent-browser')).toHaveLength(1);
  });

  it("allowEscape: ['skills/agent-browser'] → 跟随，内容（含子目录）入快照且无 error", () => {
    const { snapshot, errors } = scan(['skills/agent-browser']);

    expect(keysOf(snapshot, 'skills')).toEqual([
      'agent-browser/SKILL.md',
      'agent-browser/sub/a.ts',
      'foo/SKILL.md',
    ]);
    const files = cat(snapshot, 'skills').files;
    expect(files.get('agent-browser/SKILL.md')?.content).toBe('# agent-browser\n');
    expect(files.get('agent-browser/sub/a.ts')?.content).toBe('export const a = 1;\n');
    expect(errors).toEqual([]);
  });

  it('glob 语义：`skills/*` 命中一层（不跨 /）；`skills/*/x` 不命中', () => {
    // `skills/*` ↔ `skills/agent-browser`（一层）→ 跟随
    const star = scan(['skills/*']);
    expect(keysOf(star.snapshot, 'skills')).toContain('agent-browser/SKILL.md');
    expect(star.errors).toEqual([]);

    // `skills/*/deeper`：`*` 不跨 `/`，无法命中 `skills/agent-browser` → 仍跳过
    const twoLevel = scan(['skills/*/deeper']);
    expect(keysOf(twoLevel.snapshot, 'skills')).toEqual(['foo/SKILL.md']);
    expect(escapeErrors(twoLevel.errors, 'agent-browser')).toHaveLength(1);
  });

  it('glob 语义：尾 `/` = 目录前缀（`skills/` 覆盖整棵子树）→ 跟随', () => {
    const { snapshot, errors } = scan(['skills/']);
    expect(keysOf(snapshot, 'skills')).toEqual([
      'agent-browser/SKILL.md',
      'agent-browser/sub/a.ts',
      'foo/SKILL.md',
    ]);
    expect(errors).toEqual([]);
  });

  it('allowlist 未命中（别的路径 / 空数组）→ 仍跳过 + ScanError', () => {
    for (const allowEscape of [['extensions/other'], ['skills/agent-browser2'], []]) {
      const { snapshot, errors } = scan(allowEscape);
      expect(keysOf(snapshot, 'skills'), JSON.stringify(allowEscape)).toEqual(['foo/SKILL.md']);
      expect(escapeErrors(errors, 'agent-browser'), JSON.stringify(allowEscape)).toHaveLength(1);
    }
  });

  it('匹配基准是 root 相对路径：`agent-browser` 这种 category 内写法不生效', () => {
    const { snapshot, errors } = scan(['agent-browser']);
    expect(keysOf(snapshot, 'skills')).toEqual(['foo/SKILL.md']);
    expect(escapeErrors(errors, 'agent-browser')).toHaveLength(1);
  });

  it('嵌套逃逸链接（skills/nested/agent-browser）需 `skills/nested/...` 模式；`skills/*` 不覆盖', () => {
    write(tmp, 'skills/nested/keep.md', 'K\n');
    fs.symlinkSync(path.join(outside, 'browser'), path.join(tmp, 'skills', 'nested', 'agent-browser'));

    const shallow = scan(['skills/*']);
    expect(keysOf(shallow.snapshot, 'skills')).toContain('agent-browser/SKILL.md');
    expect(escapeErrors(shallow.errors, 'nested/agent-browser')).toHaveLength(1);

    // 只放行 nested 那条：顶层 agent-browser 未被任何模式命中 → 仍跳过
    const deep = scan(['skills/nested/agent-browser']);
    expect(escapeErrors(deep.errors, 'skills/agent-browser')).toHaveLength(1);
    expect(keysOf(deep.snapshot, 'skills')).toEqual([
      'foo/SKILL.md',
      'nested/agent-browser/SKILL.md',
      'nested/agent-browser/sub/a.ts',
      'nested/keep.md',
    ]);
  });
});

/* ------------------------------------------------------------------ */
/* 2. 文件型逃逸链接                                                    */
/* ------------------------------------------------------------------ */

describe('allowEscape — 文件型逃逸 symlink', () => {
  beforeEach(() => {
    write(tmp, 'skills/foo/SKILL.md', '# foo\n');
    write(outside, 'target.md', 'OUTSIDE FILE\n');
    fs.symlinkSync(path.join(outside, 'target.md'), path.join(tmp, 'skills', 'link.md'));
  });

  it('无 allowlist → 跳过 + ScanError（不泄漏外部内容）', () => {
    const { snapshot, errors } = scan();
    expect(keysOf(snapshot, 'skills')).toEqual(['foo/SKILL.md']);
    expect(escapeErrors(errors, 'link.md')).toHaveLength(1);
  });

  it("allowEscape: ['skills/link.md'] → 跟随，外部文件内容入快照", () => {
    const { snapshot, errors } = scan(['skills/link.md']);
    expect(keysOf(snapshot, 'skills')).toEqual(['foo/SKILL.md', 'link.md']);
    expect(cat(snapshot, 'skills').files.get('link.md')?.content).toBe('OUTSIDE FILE\n');
    expect(errors).toEqual([]);
  });

  it('allowlist 命中其它路径 → 该文件链接仍跳过 + ScanError', () => {
    const { snapshot, errors } = scan(['skills/other.md']);
    expect(keysOf(snapshot, 'skills')).toEqual(['foo/SKILL.md']);
    expect(escapeErrors(errors, 'link.md')).toHaveLength(1);
  });
});

/* ------------------------------------------------------------------ */
/* 3. 声明的 category path 自身是逃逸 symlink                            */
/* ------------------------------------------------------------------ */

describe('allowEscape — 声明的 category path 自身为逃逸 symlink', () => {
  it('目录型 path：无 allowlist → 跳过 + ScanError；命中 → 以真实路径扫描', () => {
    write(outside, 'outside-skills/a.md', 'A\n');
    write(tmp, 'settings.json', '{"theme":"dark"}\n');
    fs.symlinkSync(path.join(outside, 'outside-skills'), path.join(tmp, 'skills'));

    const denied = scan();
    expect(keysOf(denied.snapshot, 'skills')).toEqual([]);
    expect(escapeErrors(denied.errors, 'skills')).toHaveLength(1);

    const allowed = scan(['skills']);
    expect(keysOf(allowed.snapshot, 'skills')).toEqual(['a.md']);
    expect(cat(allowed.snapshot, 'skills').files.get('a.md')?.content).toBe('A\n');
    expect(allowed.errors).toEqual([]);

    // 尾 `/` 前缀模式同样命中（matchesIgnore 对 relPath `skills` 的目录前缀语义）
    const allowedSlash = scan(['skills/']);
    expect(keysOf(allowedSlash.snapshot, 'skills')).toEqual(['a.md']);
    expect(allowedSlash.errors).toEqual([]);
  });

  it('单文件 path（settings.json）逃逸：无 allowlist 跳过；命中 → 正常读取', () => {
    write(tmp, 'skills/keep.md', 'K\n');
    write(outside, 'settings.json', '{"outside":true}\n');
    fs.symlinkSync(path.join(outside, 'settings.json'), path.join(tmp, 'settings.json'));

    const denied = scan();
    expect(cat(denied.snapshot, 'settings').files.size).toBe(0);
    expect(escapeErrors(denied.errors, 'settings.json')).toHaveLength(1);

    const allowed = scan(['settings.json']);
    expect(cat(allowed.snapshot, 'settings').files.get('settings.json')?.content).toBe('{"outside":true}\n');
    expect(allowed.errors).toEqual([]);
  });
});

/* ------------------------------------------------------------------ */
/* 4. 放行链接内部再次逃逸 / 回环 / 悬空 —— 既有行为不变                    */
/* ------------------------------------------------------------------ */

describe('allowEscape — 与既有安全行为共存', () => {
  it('只放行命中的那一条链接：放行目录内部的逃逸链接仍需各自命中', () => {
    write(outside, 'browser/SKILL.md', '# browser\n');
    write(outside2, 'deep/secret.md', 'DEEP SECRET\n');
    fs.mkdirSync(path.join(tmp, 'skills'), { recursive: true });
    fs.symlinkSync(path.join(outside, 'browser'), path.join(tmp, 'skills', 'agent-browser'));
    fs.symlinkSync(path.join(outside2, 'deep'), path.join(outside, 'browser', 'nested'));

    const onlyTop = scan(['skills/agent-browser']);
    expect(keysOf(onlyTop.snapshot, 'skills')).toEqual(['agent-browser/SKILL.md']);
    expect([...cat(onlyTop.snapshot, 'skills').files.values()].map((e) => e.content).join('\n')).not.toContain(
      'DEEP SECRET',
    );
    expect(escapeErrors(onlyTop.errors, 'nested')).toHaveLength(1);

    // 尾 `/` 前缀覆盖整棵（含逃逸子树）
    const subtree = scan(['skills/agent-browser/']);
    expect(keysOf(subtree.snapshot, 'skills')).toEqual(['agent-browser/SKILL.md', 'agent-browser/nested/secret.md']);
    expect(subtree.errors).toEqual([]);
  });

  it('放行目录内的链接指回 root 内 → 仍被 visited 截断（不重复展开、无 error）', () => {
    write(tmp, 'skills/foo/SKILL.md', '# foo\n');
    write(outside, 'browser/SKILL.md', '# browser\n');
    fs.mkdirSync(path.join(tmp, 'skills'), { recursive: true });
    fs.symlinkSync(path.join(outside, 'browser'), path.join(tmp, 'skills', 'agent-browser'));
    fs.symlinkSync(path.join(tmp, 'skills'), path.join(outside, 'browser', 'back'));

    const { snapshot, errors } = scan(['skills/agent-browser']);
    const keys = keysOf(snapshot, 'skills');
    expect(keys).toEqual(['agent-browser/SKILL.md', 'foo/SKILL.md']);
    expect(new Set(keys).size).toBe(keys.length);
    expect(errors).toEqual([]);
  });

  it('allowlist 命中但链接悬空 → 仍静默跳过（等价「路径不存在」，不报错）', () => {
    write(tmp, 'skills/foo/SKILL.md', '# foo\n');
    fs.symlinkSync(path.join(tmp, 'skills', 'missing-target'), path.join(tmp, 'skills', 'dangling'));

    const { snapshot, errors } = scan(['skills/dangling']);
    expect(keysOf(snapshot, 'skills')).toEqual(['foo/SKILL.md']);
    expect(errors).toEqual([]);
  });

  it('root 内正常 symlink 行为不受 allowlist 影响（无 allowlist 也照样跟随）', () => {
    write(tmp, 'real-skills/a.md', 'A\n');
    fs.symlinkSync(path.join(tmp, 'real-skills'), path.join(tmp, 'skills'));

    const { snapshot, errors } = scan();
    expect(keysOf(snapshot, 'skills')).toEqual(['a.md']);
    expect(errors).toEqual([]);
  });

  it('allowlist 不绕过 ignore / exclude（逃逸子树内的 ignored / excluded 路径仍不收集）', () => {
    write(outside, 'browser/SKILL.md', '# browser\n');
    write(outside, 'browser/private/secret.md', 'SECRET\n');
    write(outside, 'browser/cache/blob.bin', 'BLOB\n');
    fs.mkdirSync(path.join(tmp, 'skills'), { recursive: true });
    fs.symlinkSync(path.join(outside, 'browser'), path.join(tmp, 'skills', 'agent-browser'));

    const outcome = scanAdapter(PI_ADAPTER_ID, {
      ...DEFAULT_PI_ADAPTER,
      root: tmp,
      allowEscape: ['skills/agent-browser/'],
      // 既有语义不变：adapter 级 ignore 由 walk 对 **category 内 relPath** 严格匹配
      //（M1 行为，W3 不动）；exclude 走 category 内 basename / 分段 glob。
      ignore: [...(DEFAULT_PI_ADAPTER.ignore ?? []), 'agent-browser/private/'],
      categories: {
        ...DEFAULT_PI_ADAPTER.categories,
        skills: {
          paths: ['skills/'],
          mode: 'mirror',
          exclude: ['*cache*'],
        },
      },
    });

    const keys = keysOf(outcome.snapshot, 'skills');
    expect(keys).toEqual(['agent-browser/SKILL.md']);
    const contents = [...cat(outcome.snapshot, 'skills').files.values()].map((e) => e.content).join('\n');
    expect(contents).not.toContain('SECRET');
    expect(contents).not.toContain('BLOB');
  });
});
