import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { getHomerPaths, type HomerPaths } from '../../src/core/paths.js';
import { readSnapshotFromStore, writeSnapshotToStore, STORE_COMPLETE_MARKER } from '../../src/core/store/store.js';
import { CliError } from '../../src/core/errors.js';
import type { AdapterSnapshot, CategoryConfig, HomerConfig, SnapshotEntry, SnapshotFiles } from '../../src/core/types.js';

const created: string[] = [];

afterEach(() => {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) fs.rmSync(dir, { recursive: true, force: true });
  }
});

/** 所有测试都走 HOMER_HOME=<mktemp -d>，绝不碰真实 ~/.homer。 */
function tmpPaths(): HomerPaths {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-store-'));
  created.push(dir);
  return getHomerPaths({ HOMER_HOME: dir });
}

function jsonEntry(value: unknown): SnapshotEntry {
  return { kind: 'json', content: `${JSON.stringify(value, null, 2)}\n` };
}

function fileEntry(content: string): SnapshotEntry {
  return { kind: 'file', content };
}

/** 2 分类样例：merge（单文件 settings）+ mirror（目录 skills，含子目录文件）。 */
function sampleSnapshot(): AdapterSnapshot {
  const settingsFiles: SnapshotFiles = new Map([
    ['settings.json', jsonEntry({ model: 'sonnet', theme: 'dark' })],
    ['keybindings.json', jsonEntry({ submit: 'ctrl+enter' })],
  ]);
  const skillsFiles: SnapshotFiles = new Map([
    ['foo/SKILL.md', fileEntry('# foo\n')],
    ['foo/bar/note.md', fileEntry('nested note\n')],
    ['plain.md', fileEntry('top level\n')],
  ]);
  return {
    adapterId: 'pi',
    categories: [
      { adapterId: 'pi', category: 'settings', mode: 'merge', files: settingsFiles },
      { adapterId: 'pi', category: 'skills', mode: 'mirror', files: skillsFiles },
    ],
  };
}

function sampleConfig(
  patch: { settings?: Partial<CategoryConfig>; skills?: Partial<CategoryConfig> } = {},
): HomerConfig {
  const settings: CategoryConfig = {
    paths: ['settings.json', 'keybindings.json'],
    mode: 'merge',
    ...patch.settings,
  };
  const skills: CategoryConfig = { paths: ['skills/'], mode: 'mirror', ...patch.skills };
  return {
    version: 1,
    adapters: {
      pi: { root: '~/.pi/agent', categories: { settings, skills } },
    },
  };
}

describe('writeSnapshotToStore — store 布局', () => {
  it('写到 <home>/store/<adapterId>/<category>/<relPath>（固定断言路径）', () => {
    const paths = tmpPaths();
    writeSnapshotToStore(paths, sampleSnapshot());

    const expected = [
      path.join(paths.home, 'store', 'pi', 'settings', 'settings.json'),
      path.join(paths.home, 'store', 'pi', 'settings', 'keybindings.json'),
      path.join(paths.home, 'store', 'pi', 'skills', 'foo', 'SKILL.md'),
      path.join(paths.home, 'store', 'pi', 'skills', 'foo', 'bar', 'note.md'),
      path.join(paths.home, 'store', 'pi', 'skills', 'plain.md'),
    ];
    for (const file of expected) {
      expect(fs.existsSync(file), `缺少 ${file}`).toBe(true);
    }
    // 相对断言，锁死布局字面量
    for (const rel of [
      'store/pi/settings/settings.json',
      'store/pi/settings/keybindings.json',
      'store/pi/skills/foo/SKILL.md',
      'store/pi/skills/foo/bar/note.md',
      'store/pi/skills/plain.md',
    ]) {
      expect(fs.existsSync(path.join(paths.home, rel)), `缺少 ${rel}`).toBe(true);
    }
  });

  it('内容按原始文本（UTF-8）逐字节落盘，不做 JSON 重排', () => {
    const paths = tmpPaths();
    const snapshot = sampleSnapshot();
    writeSnapshotToStore(paths, snapshot);

    for (const category of snapshot.categories) {
      for (const [relPath, entry] of category.files) {
        const file = path.join(paths.storeDir, 'pi', category.category, relPath);
        expect(fs.readFileSync(file, 'utf8')).toBe(entry.content);
      }
    }
  });

  it('空分类仍创建目录（roundtrip 结构稳定）', () => {
    const paths = tmpPaths();
    writeSnapshotToStore(paths, {
      adapterId: 'pi',
      categories: [{ adapterId: 'pi', category: 'themes', mode: 'mirror', files: new Map() }],
    });
    const dir = path.join(paths.storeDir, 'pi', 'themes');
    expect(fs.statSync(dir).isDirectory()).toBe(true);
    expect(fs.readdirSync(dir)).toEqual([]);
  });

  it('拒绝逃逸 store 的 adapterId / relPath（含 ".."）', () => {
    const paths = tmpPaths();
    expect(() => writeSnapshotToStore(paths, { adapterId: '..', categories: [] })).toThrow(/包含 '\.\.'/);

    const escape: AdapterSnapshot = {
      adapterId: 'pi',
      categories: [
        { adapterId: 'pi', category: 'skills', mode: 'mirror', files: new Map([['../evil.txt', fileEntry('x')]]) },
      ],
    };
    expect(() => writeSnapshotToStore(paths, escape)).toThrow(/包含 '\.\.'/);
    expect(fs.existsSync(path.join(paths.storeDir, 'evil.txt'))).toBe(false);
  });
});

describe('writeSnapshotToStore — 覆盖语义（增量写入的冻结定义）', () => {
  it('先清空 <storeDir>/<adapterId>/ 再写：上一轮的陈旧文件消失', () => {
    const paths = tmpPaths();
    writeSnapshotToStore(paths, sampleSnapshot());
    const stale = path.join(paths.storeDir, 'pi', 'skills', 'plain.md');
    expect(fs.existsSync(stale)).toBe(true);

    const next: AdapterSnapshot = {
      adapterId: 'pi',
      categories: [{ adapterId: 'pi', category: 'settings', mode: 'merge', files: new Map([['settings.json', fileEntry('{}')]]) }],
    };
    writeSnapshotToStore(paths, next);

    expect(fs.existsSync(stale)).toBe(false);
    expect(fs.existsSync(path.join(paths.storeDir, 'pi', 'skills'))).toBe(false);
    // `.homer-complete` 是写完整性标记（M-C），与快照内容并列于 adapter 目录
    expect(fs.readdirSync(path.join(paths.storeDir, 'pi')).sort()).toEqual(['.homer-complete', 'settings']);
  });

  it('清空范围仅限本 adapter，其他 adapter 目录不受影响', () => {
    const paths = tmpPaths();
    writeSnapshotToStore(paths, sampleSnapshot());
    writeSnapshotToStore(paths, {
      adapterId: 'herdr',
      categories: [{ adapterId: 'herdr', category: 'config', mode: 'mirror', files: new Map([['config.toml', fileEntry('a=1\n')]]) }],
    });

    writeSnapshotToStore(paths, {
      adapterId: 'pi',
      categories: [{ adapterId: 'pi', category: 'settings', mode: 'merge', files: new Map([['settings.json', fileEntry('{}')]]) }],
    });

    expect(fs.existsSync(path.join(paths.storeDir, 'herdr', 'config', 'config.toml'))).toBe(true);
    expect(fs.readdirSync(path.join(paths.storeDir, 'pi')).sort()).toEqual(['.homer-complete', 'settings']);
  });

  it('重复写同一快照幂等', () => {
    const paths = tmpPaths();
    writeSnapshotToStore(paths, sampleSnapshot());
    const before = fs.readFileSync(path.join(paths.storeDir, 'pi', 'settings', 'settings.json'), 'utf8');
    writeSnapshotToStore(paths, sampleSnapshot());
    expect(fs.readFileSync(path.join(paths.storeDir, 'pi', 'settings', 'settings.json'), 'utf8')).toBe(before);
  });
});

describe('readSnapshotFromStore', () => {
  it('roundtrip：write → read 深比较相等（2 分类 merge+mirror，含子目录）', () => {
    const paths = tmpPaths();
    const snapshot = sampleSnapshot();
    writeSnapshotToStore(paths, snapshot);

    const read = readSnapshotFromStore(paths, sampleConfig());
    expect(read).toHaveLength(1);
    expect(read[0]).toEqual(snapshot);
  });

  it('roundtrip 保持文件键顺序（按 relPath 排序）与 kind/content', () => {
    const paths = tmpPaths();
    writeSnapshotToStore(paths, sampleSnapshot());

    const read = readSnapshotFromStore(paths, sampleConfig());
    const skills = read[0]?.categories.find((c) => c.category === 'skills');
    expect(skills).toBeDefined();
    expect([...(skills?.files.keys() ?? [])]).toEqual(['foo/SKILL.md', 'foo/bar/note.md', 'plain.md']);
    expect(skills?.mode).toBe('mirror');
  });

  it('kind 判定：merge 且可 parse → json；merge 不可 parse → file；mirror 可 parse → file', () => {
    const paths = tmpPaths();
    const config: HomerConfig = {
      version: 1,
      adapters: {
        pi: {
          root: '~/.pi/agent',
          categories: {
            settings: { paths: ['settings.json'], mode: 'merge' },
            models: { paths: ['models.json'], mode: 'merge' },
            skills: { paths: ['skills/'], mode: 'mirror' },
          },
        },
      },
    };
    const snapshot: AdapterSnapshot = {
      adapterId: 'pi',
      categories: [
        { adapterId: 'pi', category: 'settings', mode: 'merge', files: new Map([['settings.json', jsonEntry({ a: 1 })]]) },
        // 手动写坏 JSON（merge 分类解析失败应降级 file）
        { adapterId: 'pi', category: 'models', mode: 'merge', files: new Map([['models.json', fileEntry('{ broken')]]) },
        // mirror 分类即使内容本身是合法 JSON，也保持 file
        { adapterId: 'pi', category: 'skills', mode: 'mirror', files: new Map([['skills/a.json', fileEntry('{"a":1}')]]) },
      ],
    };
    writeSnapshotToStore(paths, snapshot);

    const read = readSnapshotFromStore(paths, config);
    const byName = new Map(read[0]?.categories.map((c) => [c.category, c]));
    expect(byName.get('settings')?.files.get('settings.json')?.kind).toBe('json');
    expect(byName.get('models')?.files.get('models.json')?.kind).toBe('file');
    expect(byName.get('skills')?.files.get('skills/a.json')?.kind).toBe('file');
  });

  it('store 缺目录 → 该分类返回空 files（不跳过、不抛错）', () => {
    const paths = tmpPaths();
    const read = readSnapshotFromStore(paths, sampleConfig());
    expect(read).toHaveLength(1);
    expect(read[0]?.adapterId).toBe('pi');
    expect(read[0]?.categories.map((c) => [c.category, c.files.size])).toEqual([
      ['settings', 0],
      ['skills', 0],
    ]);
  });

  it('只读 config 声明的 category，忽略 store 中的多余目录', () => {
    const paths = tmpPaths();
    fs.mkdirSync(path.join(paths.storeDir, 'pi', 'unknown'), { recursive: true });
    fs.writeFileSync(path.join(paths.storeDir, 'pi', 'unknown', 'x.txt'), 'x');
    writeSnapshotToStore(paths, sampleSnapshot());
    fs.mkdirSync(path.join(paths.storeDir, 'pi', 'unknown'), { recursive: true });
    fs.writeFileSync(path.join(paths.storeDir, 'pi', 'unknown', 'x.txt'), 'x');

    const read = readSnapshotFromStore(paths, sampleConfig());
    expect(read[0]?.categories.map((c) => c.category)).toEqual(['settings', 'skills']);
  });

  it('跳过 enabled:false 的 adapter 与 category', () => {
    const paths = tmpPaths();
    writeSnapshotToStore(paths, sampleSnapshot());
    const config = sampleConfig({ skills: { enabled: false } });
    config.adapters['herdr'] = {
      root: '~/.config/herdr',
      enabled: false,
      categories: { config: { paths: ['config.toml'], mode: 'mirror' } },
    };

    const read = readSnapshotFromStore(paths, config);
    expect(read.map((a) => a.adapterId)).toEqual(['pi']);
    expect(read[0]?.categories.map((c) => c.category)).toEqual(['settings']);
  });

  it('保持 config 中 adapter / category 的声明顺序，且 adapterId/category 字段回填正确', () => {
    const paths = tmpPaths();
    const config: HomerConfig = {
      version: 1,
      adapters: {
        zeta: { root: '~/z', categories: { one: { paths: ['a.txt'], mode: 'mirror' } } },
        alpha: { root: '~/a', categories: { two: { paths: ['b.txt'], mode: 'merge' } } },
      },
    };
    fs.mkdirSync(path.join(paths.storeDir, 'zeta', 'one'), { recursive: true });
    fs.writeFileSync(path.join(paths.storeDir, 'zeta', 'one', 'a.txt'), 'z');
    fs.writeFileSync(path.join(paths.storeDir, 'zeta', STORE_COMPLETE_MARKER), '');
    fs.mkdirSync(path.join(paths.storeDir, 'alpha', 'two'), { recursive: true });
    fs.writeFileSync(path.join(paths.storeDir, 'alpha', 'two', 'b.txt'), '{"ok":true}');
    fs.writeFileSync(path.join(paths.storeDir, 'alpha', STORE_COMPLETE_MARKER), '');

    const read = readSnapshotFromStore(paths, config);
    expect(read.map((a) => a.adapterId)).toEqual(['zeta', 'alpha']);
    expect(read[1]?.categories[0]).toMatchObject({ adapterId: 'alpha', category: 'two', mode: 'merge' });
    expect(read[1]?.categories[0]?.files.get('b.txt')?.kind).toBe('json');
  });

  it('roundtrip 覆盖空分类', () => {
    const paths = tmpPaths();
    const snapshot: AdapterSnapshot = {
      adapterId: 'pi',
      categories: [{ adapterId: 'pi', category: 'themes', mode: 'mirror', files: new Map() }],
    };
    writeSnapshotToStore(paths, snapshot);
    const config: HomerConfig = {
      version: 1,
      adapters: { pi: { root: '~/x', categories: { themes: { paths: ['themes/'], mode: 'mirror' } } } },
    };
    expect(readSnapshotFromStore(paths, config)).toEqual([snapshot]);
  });

  it('默认全程隔离：不写真实 ~/.homer', () => {
    const paths = tmpPaths();
    writeSnapshotToStore(paths, sampleSnapshot());
    expect(paths.home.startsWith(os.tmpdir())).toBe(true);
    expect(fs.existsSync(path.join(paths.home, 'store', 'pi', 'settings', 'settings.json'))).toBe(true);
  });
});

/* ------------------------------------------------------------------ */
/* M-C：原子写入 + `.homer-complete` 完整性标记                         */
/* ------------------------------------------------------------------ */

describe('writeSnapshotToStore — 原子写入（M-C）', () => {
  it('每个 adapter 目录落 `.homer-complete` 标记（写全的哨兵）', () => {
    const paths = tmpPaths();
    writeSnapshotToStore(paths, sampleSnapshot());
    expect(fs.existsSync(path.join(paths.storeDir, 'pi', STORE_COMPLETE_MARKER))).toBe(true);
    // 标记在 adapter 根，不在各分类目录里
    expect(fs.existsSync(path.join(paths.storeDir, 'pi', 'settings', STORE_COMPLETE_MARKER))).toBe(false);
  });

  it('写入完成后不残留 tmp 目录（`<adapterDir>.tmp-<pid>`）', () => {
    const paths = tmpPaths();
    writeSnapshotToStore(paths, sampleSnapshot());
    const leftovers = fs.readdirSync(paths.storeDir).filter((name) => name.includes('.tmp-'));
    expect(leftovers).toEqual([]);
  });

  it('重复写入不产生 tmp 残留，且旧目录被完整替换（旧文件消失）', () => {
    const paths = tmpPaths();
    writeSnapshotToStore(paths, sampleSnapshot());
    writeSnapshotToStore(paths, {
      adapterId: 'pi',
      categories: [{ adapterId: 'pi', category: 'settings', mode: 'merge', files: new Map([['settings.json', fileEntry('{}')]]) }],
    });

    expect(fs.readdirSync(paths.storeDir).filter((n) => n.includes('.tmp-'))).toEqual([]);
    expect(fs.existsSync(path.join(paths.storeDir, 'pi', 'skills'))).toBe(false);
    // 新目录仍带标记（否则下次 read 会把它当半残）
    expect(fs.existsSync(path.join(paths.storeDir, 'pi', STORE_COMPLETE_MARKER))).toBe(true);
    expect(readSnapshotFromStore(paths, sampleConfig())[0]?.categories.map((c) => c.category)).toEqual(['settings', 'skills']);
  });

  it('写失败（非法 relPath）不破坏已有 store：旧内容与标记保持完整', () => {
    const paths = tmpPaths();
    writeSnapshotToStore(paths, sampleSnapshot());
    const before = readSnapshotFromStore(paths, sampleConfig());

    const evil: AdapterSnapshot = {
      adapterId: 'pi',
      categories: [
        { adapterId: 'pi', category: 'skills', mode: 'mirror', files: new Map([['../evil.txt', fileEntry('x')]]) },
      ],
    };
    expect(() => writeSnapshotToStore(paths, evil)).toThrow();
    // tmp 目录被清理，正式目录未被触碰
    expect(fs.readdirSync(paths.storeDir).filter((n) => n.includes('.tmp-'))).toEqual([]);
    expect(fs.existsSync(path.join(paths.storeDir, 'pi', STORE_COMPLETE_MARKER))).toBe(true);
    expect(readSnapshotFromStore(paths, sampleConfig())).toEqual(before);
  });
});

describe('readSnapshotFromStore — 完整性校验（M-C）', () => {
  it('adapter 目录存在但缺 `.homer-complete` → 抛 CliError（不当空 base）', () => {
    const paths = tmpPaths();
    writeSnapshotToStore(paths, sampleSnapshot());
    fs.rmSync(path.join(paths.storeDir, 'pi', STORE_COMPLETE_MARKER));

    expect(() => readSnapshotFromStore(paths, sampleConfig())).toThrowError(CliError);
    // 不再静默返回空 files（那会让 status 报全量 push）
    let threw: unknown;
    try {
      readSnapshotFromStore(paths, sampleConfig());
    } catch (err) {
      threw = err;
    }
    expect(threw).toBeInstanceOf(CliError);
    expect((threw as CliError).message).toMatch(/store 不完整/);
    expect((threw as CliError).hint).toMatch(/homer init/);
  });

  it('模拟「写完一半就崩」：手工建目录但无标记 → read 报错', () => {
    const paths = tmpPaths();
    fs.mkdirSync(path.join(paths.storeDir, 'pi', 'settings'), { recursive: true });
    fs.writeFileSync(path.join(paths.storeDir, 'pi', 'settings', 'settings.json'), '{}');
    // 没有标记 = 半残目录
    expect(() => readSnapshotFromStore(paths, sampleConfig())).toThrowError(/store 不完整/);
  });

  it('adapter 目录整体不存在（未 init）仍按「空 base」处理，不抛错', () => {
    const paths = tmpPaths();
    const read = readSnapshotFromStore(paths, sampleConfig());
    expect(read[0]?.categories.map((c) => c.files.size)).toEqual([0, 0]);
  });

  it('多个 adapter 中只有一个半残 → 在该 adapter 处报错', () => {
    const paths = tmpPaths();
    writeSnapshotToStore(paths, sampleSnapshot());
    fs.mkdirSync(path.join(paths.storeDir, 'herdr', 'config'), { recursive: true });
    const config = sampleConfig();
    config.adapters['herdr'] = { root: '~/.config/herdr', categories: { config: { paths: ['config.toml'], mode: 'mirror' } } };

    expect(() => readSnapshotFromStore(paths, config)).toThrowError(/store 不完整.*herdr/);
  });

  it('标记本身不进快照 files', () => {
    const paths = tmpPaths();
    writeSnapshotToStore(paths, sampleSnapshot());
    const read = readSnapshotFromStore(paths, sampleConfig());
    const allKeys = read.flatMap((a) => a.categories.flatMap((c) => [...c.files.keys()]));
    expect(allKeys).not.toContain(STORE_COMPLETE_MARKER);
  });
});
