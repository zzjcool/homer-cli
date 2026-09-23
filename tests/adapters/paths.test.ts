/**
 * P2-W6 · `src/adapters/paths.ts` 验收（docs/m2-plan.md §2.6 / §3-P2-W6）。
 *
 * 核心不变式：`resolveCategoryFilePath` 与 `scanAdapter` 的 relPath 规则**互为逆映射**。
 * 本文件用 M1 e2e 同款 fixture（`tests/e2e/m1.test.ts` 的 pi 布局 + 硬编码的
 * `EXPECTED_STORE_FILES` 清单）做双侧断言：
 *   1. 单测侧：对每个期望的 store 相对路径，反向解析出的绝对路径 == 硬编码的期望绝对路径；
 *   2. 交叉侧：真的在假 HOME 下建出这套 fixture，用 `scanAdapter` 扫出 relPath 集合，
 *      再对每个 relPath 反向解析，验证「扫出来的 relPath → 绝对路径」正是原始文件本身。
 *
 * 覆盖：目录型 / 单文件型 / 多单文件型 category、`'..'` 与绝对路径 / 未知 basename → throw、
 * 空 / 尾斜杠 relPath → throw、可重复调用（纯函数）、与 config 失配时的报错信息。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { resolveCategoryFilePath } from '../../src/adapters/paths.js';
import { PI_ADAPTER_ID, DEFAULT_PI_ADAPTER } from '../../src/adapters/pi/defaults.js';
import { scanAdapter } from '../../src/adapters/pi/index.js';
import type { CategoryConfig } from '../../src/core/types.js';

const created: string[] = [];

function tmp(prefix = 'homer-adapters-paths-'): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), prefix));
  created.push(dir);
  return dir;
}

afterEach(() => {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) fs.rmSync(dir, { recursive: true, force: true });
  }
});

/** M1 e2e 的 pi fixture 布局（逐项与 tests/e2e/m1.test.ts 的 buildPiFixture 对齐）。 */
const FIXTURE_FILES = [
  'settings.json',
  'keybindings.json',
  'skills/alpha/SKILL.md',
  'skills/beta/SKILL.md',
  'extensions/tool.ts',
  'agents/scout.md',
  'prompts/review.md',
  'themes/dark.json',
  'models.json',
];

function buildFixture(agentRoot: string): void {
  for (const rel of FIXTURE_FILES) {
    const abs = path.join(agentRoot, rel);
    fs.mkdirSync(path.dirname(abs), { recursive: true });
    fs.writeFileSync(abs, `${rel}\n`, 'utf8');
  }
}

const PI = DEFAULT_PI_ADAPTER;
const CATEGORIES = PI.categories;

function catOf(name: string): CategoryConfig {
  const cat = CATEGORIES[name];
  if (cat === undefined) throw new Error(`fixture 里没有 category: ${name}`);
  return cat;
}

/* ------------------------------------------------------------------ */
/* 目录型                                                              */
/* ------------------------------------------------------------------ */

describe('resolveCategoryFilePath — 目录型 path', () => {
  it('skills/（目录型）→ <root>/skills/<relPath>', () => {
    const root = '/tmp/fake-agent';
    expect(resolveCategoryFilePath(root, catOf('skills'), 'alpha/SKILL.md')).toBe(
      path.join(root, 'skills', 'alpha', 'SKILL.md'),
    );
    expect(resolveCategoryFilePath(root, catOf('skills'), 'beta/SKILL.md')).toBe(
      path.join(root, 'skills', 'beta', 'SKILL.md'),
    );
  });

  it('多层嵌套 relPath 原样拼在目录 path 之下', () => {
    const root = '/tmp/fake-agent';
    expect(resolveCategoryFilePath(root, catOf('skills'), 'a/b/c/d.md')).toBe(
      path.join(root, 'skills', 'a', 'b', 'c', 'd.md'),
    );
  });

  it('声明多个目录型 path 时，最后一个匹配者胜出（对齐 scan 的 Map 覆盖语义）', () => {
    const root = '/tmp/fake-agent';
    const cfg: CategoryConfig = { paths: ['one/', 'two/'], mode: 'mirror' };
    expect(resolveCategoryFilePath(root, cfg, 'x.md')).toBe(path.join(root, 'two', 'x.md'));
  });

  it('目录 path 带多个尾斜杠（`skills///`）时被规范化', () => {
    const root = '/tmp/fake-agent';
    const cfg: CategoryConfig = { paths: ['skills///'], mode: 'mirror' };
    expect(resolveCategoryFilePath(root, cfg, 'a.md')).toBe(path.join(root, 'skills', 'a.md'));
  });
});

/* ------------------------------------------------------------------ */
/* 单文件型                                                            */
/* ------------------------------------------------------------------ */

describe('resolveCategoryFilePath — 单文件型 path', () => {
  it('settings 多单文件型：按 basename 匹配到对应的那个 path', () => {
    const root = '/tmp/fake-agent';
    expect(resolveCategoryFilePath(root, catOf('settings'), 'settings.json')).toBe(
      path.join(root, 'settings.json'),
    );
    expect(resolveCategoryFilePath(root, catOf('settings'), 'keybindings.json')).toBe(
      path.join(root, 'keybindings.json'),
    );
  });

  it('models 单文件型：models.json → <root>/models.json', () => {
    const root = '/tmp/fake-agent';
    expect(resolveCategoryFilePath(root, catOf('models'), 'models.json')).toBe(
      path.join(root, 'models.json'),
    );
  });

  it('单文件 path 带子目录（`sub/conf.json`）→ <root>/sub/conf.json，且匹配 basename', () => {
    const root = '/tmp/fake-agent';
    const cfg: CategoryConfig = { paths: ['sub/conf.json'], mode: 'merge' };
    expect(resolveCategoryFilePath(root, cfg, 'conf.json')).toBe(path.join(root, 'sub', 'conf.json'));
    // relPath 是 `sub/conf.json` 时不该匹配（scan 侧只会写 basename）
    expect(() => resolveCategoryFilePath(root, cfg, 'sub/conf.json')).toThrow(/无法映射/);
  });

  it('目录型 + 单文件型混合的 category：两者都按各自规则解析', () => {
    const root = '/tmp/fake-agent';
    const cfg: CategoryConfig = { paths: ['settings.json', 'extra/'], mode: 'merge' };
    // 单文件型 path 声明的 relPath → 该单文件本身
    const singleFileFirst: CategoryConfig = { paths: ['extra/', 'settings.json'], mode: 'merge' };
    expect(resolveCategoryFilePath(root, singleFileFirst, 'settings.json')).toBe(
      path.join(root, 'settings.json'),
    );
    // 目录型 path 里的嵌套文件照旧
    expect(resolveCategoryFilePath(root, cfg, 'nested/x.json')).toBe(
      path.join(root, 'extra', 'nested', 'x.json'),
    );
  });

  /**
   * **歧义情形的裁定**：category 同时声明单文件 `settings.json` 与目录 `extra/` 时，
   * relPath `settings.json` 同时能被两者产出。scan 把所有 path 收进同一个 Map，
   * **后写覆盖先写**，故只有「声明顺序里最后一个匹配者」才与 scan 的产出自洽。
   * 本用例用真实目录证明这一点（含文件内容，避免只看路径的假通过）。
   */
  it('歧义 relPath：最后一个声明匹配者胜出（与 scan 的 Map 覆盖语义一致，含内容断言）', () => {
    const tmpRoot = tmp();
    const agentRoot = path.join(tmpRoot, 'agent');
    fs.mkdirSync(path.join(agentRoot, 'extra'), { recursive: true });
    fs.writeFileSync(path.join(agentRoot, 'settings.json'), 'FROM-SINGLE\n', 'utf8');
    fs.writeFileSync(path.join(agentRoot, 'extra', 'settings.json'), 'FROM-DIR\n', 'utf8');

    const dirLast: CategoryConfig = { paths: ['settings.json', 'extra/'], mode: 'merge' };
    const singleLast: CategoryConfig = { paths: ['extra/', 'settings.json'], mode: 'merge' };

    // scan 对 dirLast 的产出：`settings.json` → extra/ 里的那份（目录后扫，覆盖前者）
    const outcome = scanAdapter(PI_ADAPTER_ID, {
      ...PI,
      root: agentRoot,
      categories: { settings: dirLast },
    });
    const scanned = outcome.snapshot.categories[0]?.files.get('settings.json');
    expect(scanned?.content).toBe('FROM-DIR\n');

    // 逆映射必须落在**同一个**文件上（否则 pull 会写错位置）
    expect(resolveCategoryFilePath(agentRoot, dirLast, 'settings.json')).toBe(
      path.join(agentRoot, 'extra', 'settings.json'),
    );
    expect(fs.readFileSync(resolveCategoryFilePath(agentRoot, dirLast, 'settings.json'), 'utf8')).toBe(
      scanned?.content,
    );

    // 反转声明顺序 → 单文件后扫，scan 取 root/settings.json；逆映射同步跟着翻转
    const outcome2 = scanAdapter(PI_ADAPTER_ID, {
      ...PI,
      root: agentRoot,
      categories: { settings: singleLast },
    });
    const scanned2 = outcome2.snapshot.categories[0]?.files.get('settings.json');
    expect(scanned2?.content).toBe('FROM-SINGLE\n');
    expect(resolveCategoryFilePath(agentRoot, singleLast, 'settings.json')).toBe(
      path.join(agentRoot, 'settings.json'),
    );
    expect(fs.readFileSync(resolveCategoryFilePath(agentRoot, singleLast, 'settings.json'), 'utf8')).toBe(
      scanned2?.content,
    );
  });
});

/* ------------------------------------------------------------------ */
/* throw 路径                                                          */
/* ------------------------------------------------------------------ */

describe('resolveCategoryFilePath — 非法输入一律 throw（编程错误）', () => {
  const root = '/tmp/fake-agent';

  it("relPath 含 '..' 段 → throw", () => {
    expect(() => resolveCategoryFilePath(root, catOf('skills'), '../escape.md')).toThrow(/\.\./);
    expect(() => resolveCategoryFilePath(root, catOf('skills'), 'a/../../etc/passwd')).toThrow(/\.\./);
    expect(() => resolveCategoryFilePath(root, catOf('skills'), '..')).toThrow(/\.\./);
  });

  it('真相：`..` 输入绝不会产出 root 之外的路径（先 throw，不靠路径规范化兜底）', () => {
    // 反证：如果实现是「join 后规范化」，`../escape.md` 会得到 /tmp/escape.md。
    // 本实现选择更安全的一侧：直接拒绝该输入。
    expect(() => resolveCategoryFilePath(root, catOf('skills'), '../escape.md')).toThrow();
  });

  it('relPath 是绝对路径（posix / Windows）→ throw', () => {
    expect(() => resolveCategoryFilePath(root, catOf('skills'), '/etc/passwd')).toThrow(/绝对路径/);
    expect(() => resolveCategoryFilePath(root, catOf('settings'), 'C:\\windows\\system32')).toThrow(/绝对路径/);
  });

  it('未知 basename（单文件型 category 里没有这个文件）→ throw', () => {
    expect(() => resolveCategoryFilePath(root, catOf('settings'), 'nope.json')).toThrow(/无法映射/);
    expect(() => resolveCategoryFilePath(root, catOf('settings'), 'auth.json')).toThrow(/无法映射/);
  });

  it('只有单文件型 path 的 category + 子目录 relPath → throw（不静默造目录）', () => {
    expect(() => resolveCategoryFilePath(root, catOf('models'), 'a/b.json')).toThrow(/无法映射/);
  });

  it('relPath 为空 / 以分隔符结尾 → throw', () => {
    expect(() => resolveCategoryFilePath(root, catOf('skills'), '')).toThrow(/不能为空/);
    expect(() => resolveCategoryFilePath(root, catOf('skills'), 'a/b/')).toThrow(/以 '\/' 结尾/);
  });

  it('报错信息里带出 category.paths，方便定位失配的 config', () => {
    expect(() => resolveCategoryFilePath(root, catOf('settings'), 'x.json')).toThrow(
      /settings\.json.*keybindings\.json|paths/,
    );
  });
});

/* ------------------------------------------------------------------ */
/* 与 scan 互为逆映射（M1 e2e 同款 fixture）                            */
/* ------------------------------------------------------------------ */

/** M1 e2e 硬编码的 store 文件清单 → (category, relPath, 期望绝对路径)。 */
const EXPECTED_STORE_FILES: { storeRel: string; category: string; relPath: string; agentRel: string }[] = [
  { storeRel: 'pi/agents/scout.md', category: 'agents', relPath: 'scout.md', agentRel: 'agents/scout.md' },
  { storeRel: 'pi/extensions/tool.ts', category: 'extensions', relPath: 'tool.ts', agentRel: 'extensions/tool.ts' },
  { storeRel: 'pi/models/models.json', category: 'models', relPath: 'models.json', agentRel: 'models.json' },
  { storeRel: 'pi/prompts/review.md', category: 'prompts', relPath: 'review.md', agentRel: 'prompts/review.md' },
  {
    storeRel: 'pi/settings/keybindings.json',
    category: 'settings',
    relPath: 'keybindings.json',
    agentRel: 'keybindings.json',
  },
  {
    storeRel: 'pi/settings/settings.json',
    category: 'settings',
    relPath: 'settings.json',
    agentRel: 'settings.json',
  },
  {
    storeRel: 'pi/skills/alpha/SKILL.md',
    category: 'skills',
    relPath: 'alpha/SKILL.md',
    agentRel: 'skills/alpha/SKILL.md',
  },
  {
    storeRel: 'pi/skills/beta/SKILL.md',
    category: 'skills',
    relPath: 'beta/SKILL.md',
    agentRel: 'skills/beta/SKILL.md',
  },
  { storeRel: 'pi/themes/dark.json', category: 'themes', relPath: 'dark.json', agentRel: 'themes/dark.json' },
];

describe('resolveCategoryFilePath — 与 scan 的 relPath 规则互为逆映射', () => {
  it('对 M1 e2e 硬编码清单逐条断言绝对路径（目录型 / 单文件型）', () => {
    const agentRoot = '/tmp/fake-agent';
    for (const entry of EXPECTED_STORE_FILES) {
      expect(
        resolveCategoryFilePath(agentRoot, catOf(entry.category), entry.relPath),
        `${entry.storeRel} 的反向映射`,
      ).toBe(path.join(agentRoot, entry.agentRel));
    }
  });

  it('真实 fixture：scan 产出的每个 relPath 反向解析后回到原文件（双侧闭合）', () => {
    const tmpRoot = tmp();
    const agentRoot = path.join(tmpRoot, 'agent');
    buildFixture(agentRoot);
    const rootReal = fs.realpathSync(agentRoot);

    const outcome = scanAdapter(PI_ADAPTER_ID, { ...PI, root: agentRoot });
    expect(outcome.errors).toEqual([]);

    // scan 应该恰好收集到 fixture 的全部文件（每个分类的 relPath 集合 = 该分类的文件）
    const scanned = new Map<string, string[]>();
    for (const category of outcome.snapshot.categories) {
      scanned.set(category.category, [...category.files.keys()].sort());
    }

    expect(scanned.get('skills')).toEqual(['alpha/SKILL.md', 'beta/SKILL.md']);
    expect(scanned.get('settings')).toEqual(['keybindings.json', 'settings.json']);
    expect(scanned.get('models')).toEqual(['models.json']);
    expect(scanned.get('agents')).toEqual(['scout.md']);
    expect(scanned.get('extensions')).toEqual(['tool.ts']);
    expect(scanned.get('prompts')).toEqual(['review.md']);
    expect(scanned.get('themes')).toEqual(['dark.json']);

    // 逆映射断言：scan 的每个 relPath → 原文件绝对路径，且文件确实存在、内容一致
    let checked = 0;
    for (const category of outcome.snapshot.categories) {
      for (const relPath of category.files.keys()) {
        const resolved = resolveCategoryFilePath(rootReal, catOf(category.category), relPath);
        expect(fs.existsSync(resolved), `${category.category}/${relPath} → ${resolved}`).toBe(true);
        expect(resolved.startsWith(`${rootReal}${path.sep}`)).toBe(true);
        // 内容一致（fixture 写的就是 `${rel}`，目录型 relPath 还要拼回声明目录）
        expect(fs.readFileSync(resolved, 'utf8')).toContain(relPath.split('/').pop() ?? '');
        checked += 1;
      }
    }
    expect(checked).toBe(FIXTURE_FILES.length);
  });

  it('scan 的 relPath 集合与 store 清单的 relPath 字段完全一致（同一份 fixture 双口径）', () => {
    const tmpRoot = tmp();
    const agentRoot = path.join(tmpRoot, 'agent');
    buildFixture(agentRoot);

    const outcome = scanAdapter(PI_ADAPTER_ID, { ...PI, root: agentRoot });
    const fromScan = new Set(
      outcome.snapshot.categories.flatMap((category) =>
        [...category.files.keys()].map((relPath) => `${PI_ADAPTER_ID}/${category.category}/${relPath}`),
      ),
    );
    const fromHardcoded = new Set(EXPECTED_STORE_FILES.map((entry) => entry.storeRel));
    expect([...fromScan].sort()).toEqual([...fromHardcoded].sort());
  });

  it('纯函数：同一输入重复调用结果稳定', () => {
    const root = '/tmp/fake-agent';
    const first = resolveCategoryFilePath(root, catOf('skills'), 'a/b.md');
    const second = resolveCategoryFilePath(root, catOf('skills'), 'a/b.md');
    expect(first).toBe(second);
  });

  it('root 为相对路径时按相对路径原样拼接（root 展开由调用方负责）', () => {
    expect(resolveCategoryFilePath('relroot', catOf('themes'), 'dark.json')).toBe(
      path.join('relroot', 'themes', 'dark.json'),
    );
  });

  it('所有 7 个默认 category 的目录型 / 单文件型方向都能解析（冒烟）', () => {
    const root = '/tmp/fake-agent';
    for (const [name, cfg] of Object.entries(CATEGORIES)) {
      const sample = cfg.paths[0]?.endsWith('/')
        ? 'sample.json'
        : (cfg.paths[0]?.split('/').pop() ?? '');
      expect(sample).not.toBe('');
      expect(() => resolveCategoryFilePath(root, catOf(name), sample)).not.toThrow();
    }
  });
});
