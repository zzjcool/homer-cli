/**
 * M-B 回归：scanAdapter 的 symlink 处理（containment / 回环 / 悬空）。
 *
 * 修复前的两个真实缺陷：
 *   1. symlink 分支只 `statSync` 跟随，walk 不维护「已访问目录」集合
 *      → `skills/loop -> skills` 这类回环被反复展开，快照出现大量重复条目；
 *   2. 未做 root containment → `skills/escape -> <root 外目录>` 会把外部文件读进快照（逃逸）。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { DEFAULT_PI_ADAPTER, PI_ADAPTER_ID, scanAdapter } from '../../../src/adapters/pi/index.js';
import type { AdapterSnapshot } from '../../../src/core/types.js';

let tmp: string;
/** 与 adapter root 同级的「外部」目录（逃逸目标）。 */
let outside: string;

beforeEach(() => {
  tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-pi-symlink-'));
  outside = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-pi-outside-'));
});

afterEach(() => {
  fs.rmSync(tmp, { recursive: true, force: true });
  fs.rmSync(outside, { recursive: true, force: true });
});

function write(rel: string, content: string): void {
  const abs = path.join(tmp, rel);
  fs.mkdirSync(path.dirname(abs), { recursive: true });
  fs.writeFileSync(abs, content, 'utf8');
}

function writeOutside(rel: string, content: string): void {
  const abs = path.join(outside, rel);
  fs.mkdirSync(path.dirname(abs), { recursive: true });
  fs.writeFileSync(abs, content, 'utf8');
}

function scan(): ReturnType<typeof scanAdapter> {
  return scanAdapter(PI_ADAPTER_ID, { ...DEFAULT_PI_ADAPTER, root: tmp });
}

function cat(snapshot: AdapterSnapshot, name: string) {
  const found = snapshot.categories.find((c) => c.category === name);
  if (!found) throw new Error(`category not found: ${name}`);
  return found;
}

describe('scanAdapter — symlink containment (M-B)', () => {
  it('回环 symlink（skills/loop -> skills）不膨胀：每个真实文件只出现一次', () => {
    write('skills/foo/SKILL.md', '# foo\n');
    write('skills/bar/SKILL.md', '# bar\n');
    fs.symlinkSync(path.join(tmp, 'skills'), path.join(tmp, 'skills', 'loop'));

    const { snapshot, errors } = scan();

    const keys = [...cat(snapshot, 'skills').files.keys()].sort();
    expect(keys).toEqual(['bar/SKILL.md', 'foo/SKILL.md']);
    // 无重复（修复前会有 loop/foo/SKILL.md、loop/loop/... 等重复条目）
    expect(new Set(keys).size).toBe(keys.length);
    expect(keys.some((k) => k.includes('loop'))).toBe(false);
    // 回环是「正常剪枝」而非错误，故 errors 允许为空
    expect(errors.filter((e) => e.path.includes('loop'))).toEqual([]);
  });

  it('逃逸 symlink（skills/escape -> root 外目录）：不读外部内容 + 记 ScanError', () => {
    write('skills/foo/SKILL.md', '# foo\n');
    writeOutside('secret.md', 'TOP SECRET\n');
    fs.symlinkSync(outside, path.join(tmp, 'skills', 'escape'));

    const { snapshot, errors } = scan();

    const keys = [...cat(snapshot, 'skills').files.keys()].sort();
    expect(keys).toEqual(['foo/SKILL.md']);
    const contents = [...cat(snapshot, 'skills').files.values()].map((e) => e.content).join('\n');
    expect(contents).not.toContain('TOP SECRET');

    const escapeErrors = errors.filter((e) => e.path.includes('escape'));
    expect(escapeErrors).toHaveLength(1);
    expect(escapeErrors[0]?.message).toMatch(/逃逸|escape|outside root/);
  });

  it('文件 symlink 指向 root 内正常文件 → 正常收集', () => {
    write('skills/real/SKILL.md', '# real\n');
    fs.mkdirSync(path.join(tmp, 'skills', 'linked'), { recursive: true });
    fs.symlinkSync(path.join(tmp, 'skills', 'real', 'SKILL.md'), path.join(tmp, 'skills', 'linked', 'SKILL.md'));

    const { snapshot, errors } = scan();

    const files = cat(snapshot, 'skills').files;
    expect([...files.keys()].sort()).toEqual(['linked/SKILL.md', 'real/SKILL.md']);
    expect(files.get('linked/SKILL.md')?.content).toBe('# real\n');
    expect(errors).toEqual([]);
  });

  it('文件 symlink 指向 root 外的文件 → 跳过 + 记 ScanError（不泄漏外部内容）', () => {
    write('skills/foo/SKILL.md', '# foo\n');
    writeOutside('target.md', 'OUTSIDE FILE\n');
    fs.symlinkSync(path.join(outside, 'target.md'), path.join(tmp, 'skills', 'link.md'));

    const { snapshot, errors } = scan();

    const keys = [...cat(snapshot, 'skills').files.keys()];
    expect(keys).toEqual(['foo/SKILL.md']);
    expect([...cat(snapshot, 'skills').files.values()].map((e) => e.content).join('\n')).not.toContain('OUTSIDE FILE');
    expect(errors.filter((e) => e.path.endsWith('link.md'))).toHaveLength(1);
  });

  it('悬空 symlink → 静默跳过（等价「路径不存在」，不报错、不进快照）', () => {
    write('skills/foo/SKILL.md', '# foo\n');
    fs.symlinkSync(path.join(tmp, 'skills', 'missing-target'), path.join(tmp, 'skills', 'dangling'));

    const { snapshot, errors } = scan();

    expect([...cat(snapshot, 'skills').files.keys()]).toEqual(['foo/SKILL.md']);
    expect(errors).toEqual([]);
  });

  it('目录自我 symlink（skills/self -> skills/foo）不重复展开', () => {
    write('skills/foo/SKILL.md', '# foo\n');
    fs.symlinkSync(path.join(tmp, 'skills', 'foo'), path.join(tmp, 'skills', 'foo', 'self'));

    const { snapshot } = scan();
    expect([...cat(snapshot, 'skills').files.keys()]).toEqual(['foo/SKILL.md']);
  });

  it('软链目录位于 root 内更深层（skills/a/b -> skills/a）仍被 visited 截断', () => {
    write('skills/a/one.md', '1\n');
    write('skills/a/nested/two.md', '2\n');
    fs.symlinkSync(path.join(tmp, 'skills', 'a'), path.join(tmp, 'skills', 'a', 'nested', 'up'));

    const { snapshot } = scan();
    const keys = [...cat(snapshot, 'skills').files.keys()].sort();
    expect(keys).toEqual(['a/nested/two.md', 'a/one.md']);
  });
});

/* ------------------------------------------------------------------ */
/* 声明的 category path 自身是 symlink 时同样做 containment             */
/* ------------------------------------------------------------------ */

describe('scanAdapter — 配置 path 自身为 symlink（containment 一致）', () => {
  it('目录型 path（skills/）是指向 root 内的 symlink → 以真实路径扫描', () => {
    // skills/ 本身是 symlink，指向 root 内真实目录 real-skills/
    write('real-skills/a.md', 'A\n');
    fs.symlinkSync(path.join(tmp, 'real-skills'), path.join(tmp, 'skills'));

    const { snapshot, errors } = scan();

    expect([...cat(snapshot, 'skills').files.keys()]).toEqual(['a.md']);
    expect(errors).toEqual([]);
  });

  it('目录型 path（skills/）是指向 root 外的 symlink → 跳过 + 记 ScanError', () => {
    fs.mkdirSync(tmp, { recursive: true });
    writeOutside('outside.md', 'OUTSIDE\n');
    fs.symlinkSync(outside, path.join(tmp, 'skills'));

    const { snapshot, errors } = scan();

    expect([...cat(snapshot, 'skills').files.keys()]).toEqual([]);
    expect([...cat(snapshot, 'skills').files.values()].map((e) => e.content).join('\n')).not.toContain('OUTSIDE');
    expect(errors.filter((e) => e.path.endsWith('skills'))).toHaveLength(1);
  });

  it('单文件 path（settings.json）是指向 root 外的 symlink → 跳过 + 记 ScanError', () => {
    write('skills/keep.md', 'K\n');
    writeOutside('settings.json', '{"outside":true}\n');
    fs.symlinkSync(path.join(outside, 'settings.json'), path.join(tmp, 'settings.json'));

    const { snapshot, errors } = scan();

    expect(cat(snapshot, 'settings').files.size).toBe(0);
    expect(errors.filter((e) => e.path.endsWith('settings.json'))).toHaveLength(1);
  });

  it('单文件 path 是指向 root 内的 symlink → 正常读取（内容与目标一致）', () => {
    write('real-settings.json', '{"theme":"real"}\n');
    fs.symlinkSync(path.join(tmp, 'real-settings.json'), path.join(tmp, 'settings.json'));
    write('skills/keep.md', 'K\n');

    const { snapshot, errors } = scan();

    expect(cat(snapshot, 'settings').files.get('settings.json')?.content).toBe('{"theme":"real"}\n');
    expect(errors).toEqual([]);
  });

  it('声明的 path 是悬空 symlink → 静默跳过（不报错）', () => {
    fs.symlinkSync(path.join(tmp, 'nowhere'), path.join(tmp, 'settings.json'));
    write('skills/keep.md', 'K\n');

    const { snapshot, errors } = scan();
    expect(cat(snapshot, 'settings').files.size).toBe(0);
    expect(errors).toEqual([]);
  });
});
