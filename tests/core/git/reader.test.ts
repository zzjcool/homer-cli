/**
 * W3-git / reader.ts 验收（docs/m2-plan.md §2.4 D3/D4、§3-P1-W3）。
 *
 * 核心断言：`readStoreSnapshotAtCommit` 与 `readSnapshotFromStore`（工作区侧）**同构**——
 * `writeSnapshotToStore → commitAllStore → readStoreSnapshotAtCommit` 的 roundtrip
 * 与原始快照深比较相等（多分类 / 子目录 / 空分类），并且能读到**历史 commit**（base 语义）。
 */

import fs from 'node:fs';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { readStoreSnapshotAtCommit, readVaultFileAtCommit } from '../../../src/core/git/reader.js';
import { commitAllStore, commitPaths, ensureGitRepo, gitFetch } from '../../../src/core/git/git.js';
import { readSnapshotFromStore, writeSnapshotToStore, STORE_COMPLETE_MARKER } from '../../../src/core/store/store.js';
import type {
  AdapterSnapshot,
  CategoryConfig,
  HomerConfig,
  SnapshotEntry,
  SnapshotFiles,
} from '../../../src/core/types.js';
import { cleanupTmp, gitOk, initBare, initRepo, mkTmp, pathsFor, writeFile } from './helpers.js';

afterEach(cleanupTmp);

function jsonEntry(value: unknown): SnapshotEntry {
  return { kind: 'json', content: `${JSON.stringify(value, null, 2)}\n` };
}

function fileEntry(content: string): SnapshotEntry {
  return { kind: 'file', content };
}

/**
 * 多分类样例（与 tests/store/store.test.ts 的 sampleSnapshot 同形，便于交叉验证）：
 * - settings：merge 单文件 ×2（可 parse → kind json）
 * - skills：mirror 目录，含子目录
 * - notes：空分类（0 文件，目录仍存在）
 */
function sampleSnapshot(version = 'v1'): AdapterSnapshot {
  // Map 顺序 = relPath 排序（scan / store 的产出顺序）：keybindings < settings；
  // 否则 roundtrip 深比较会因 Map 键序不同而假失败。
  const settingsFiles: SnapshotFiles = new Map([
    ['keybindings.json', jsonEntry({ submit: 'ctrl+enter' })],
    ['settings.json', jsonEntry({ model: 'sonnet', theme: 'dark', version })],
  ]);
  const skillsFiles: SnapshotFiles = new Map([
    ['foo/SKILL.md', fileEntry(`# foo ${version}\n`)],
    ['foo/bar/note.md', fileEntry('nested note\n')],
    ['plain.md', fileEntry('top level\n')],
  ]);
  return {
    adapterId: 'pi',
    categories: [
      { adapterId: 'pi', category: 'settings', mode: 'merge', files: settingsFiles },
      { adapterId: 'pi', category: 'skills', mode: 'mirror', files: skillsFiles },
      { adapterId: 'pi', category: 'notes', mode: 'mirror', files: new Map() },
    ],
  };
}

function sampleConfig(): HomerConfig {
  const settings: CategoryConfig = { paths: ['settings.json', 'keybindings.json'], mode: 'merge' };
  const skills: CategoryConfig = { paths: ['skills/'], mode: 'mirror' };
  const notes: CategoryConfig = { paths: ['notes/'], mode: 'mirror' };
  return {
    version: 1,
    adapters: {
      pi: { root: '/tmp/pi-root', enabled: true, categories: { settings, skills, notes } },
      other: { root: '/tmp/other-root', enabled: true, categories: { data: { paths: ['data/'], mode: 'mirror' } } },
    },
  };
}

/** config 里 `other` adapter 在 commit 内无目录 → 空 Map（结构严格对齐 config）。 */
function otherEmpty(): AdapterSnapshot {
  return {
    adapterId: 'other',
    categories: [{ adapterId: 'other', category: 'data', mode: 'mirror', files: new Map() }],
  };
}

/** reader 的期望输出 = 输入快照 + config 中其余 enabled adapter（空 Map）。 */
function expectedFull(snapshot: AdapterSnapshot): AdapterSnapshot[] {
  return [snapshot, otherEmpty()];
}

/** 一个「已 init + store 已写 + 已 commit」的临时仓库。 */
function committedHome(
  snapshot: AdapterSnapshot = sampleSnapshot(),
  config: HomerConfig = sampleConfig(),
): { home: string; paths: ReturnType<typeof pathsFor> } {
  const home = mkTmp('reader');
  ensureGitRepo(home);
  gitOk(home, ['config', 'user.email', 'homer-test@example.invalid']);
  gitOk(home, ['config', 'user.name', 'Homer Test']);
  writeFile(path.join(home, 'homer.json'), `${JSON.stringify(config, null, 2)}\n`);

  const paths = pathsFor(home);
  writeSnapshotToStore(paths, snapshot);
  const sha = commitAllStore(home, 'store v1');
  expect(sha).toBeDefined();
  return { home, paths };
}

describe('readStoreSnapshotAtCommit — roundtrip 与工作区同构', () => {
  it('write → commit → read 深比较等于原始快照（多分类 / 子目录 / 空分类）', () => {
    const snapshot = sampleSnapshot();
    const config = sampleConfig();
    const { paths } = committedHome(snapshot, config);

    const fromCommit = readStoreSnapshotAtCommit(paths, config, 'HEAD');
    expect(fromCommit).toEqual(expectedFull(snapshot));

    // 与工作区侧读取（readSnapshotFromStore）也必须一致——两条路径同构
    expect(fromCommit).toEqual(readSnapshotFromStore(paths, config));

    // 结构严格对齐 config：两个 enabled adapter 都在；other 缺目录 → 空 Map
    expect(fromCommit.map((a) => a.adapterId)).toEqual(['pi', 'other']);
    expect(fromCommit.map((a) => a.categories.map((c) => c.category))).toEqual([
      ['settings', 'skills', 'notes'],
      ['data'],
    ]);
    for (const category of fromCommit[0]?.categories ?? []) {
      expect(category.files.has(STORE_COMPLETE_MARKER)).toBe(false);
    }
    expect(fromCommit[0]?.categories[2]?.files.size).toBe(0); // 空分类（目录存在）→ 空 Map
    expect(fromCommit[1]?.categories[0]?.files.size).toBe(0); // 缺目录 → 空 Map（M1 裁定）
  });

  it('kind 判定与 entryKindFor(category.mode) 一致：merge 不可 parse → file', () => {
    const snapshot: AdapterSnapshot = {
      adapterId: 'pi',
      categories: [
        {
          adapterId: 'pi',
          category: 'settings',
          mode: 'merge',
          files: new Map([
            // 排序后：keybindings < settings
            ['keybindings.json', jsonEntry({ a: 1 })], // merge + parse → json
            ['settings.json', fileEntry('not json at all\n')], // merge 但不可 parse → file
          ]),
        },
        { adapterId: 'pi', category: 'skills', mode: 'mirror', files: new Map() },
        { adapterId: 'pi', category: 'notes', mode: 'mirror', files: new Map() },
      ],
    };
    const config = sampleConfig();
    const { paths } = committedHome(snapshot, config);

    const read = readStoreSnapshotAtCommit(paths, config, 'HEAD');
    const files = read[0]?.categories[0]?.files;
    expect(files?.get('settings.json')?.kind).toBe('file');
    expect(files?.get('keybindings.json')?.kind).toBe('json');
    expect(read).toEqual(expectedFull(snapshot));
  });

  it('镜像分类含子目录 / 顶层文件 → relPath 与布局规则一致', () => {
    const { paths } = committedHome();
    const read = readStoreSnapshotAtCommit(paths, sampleConfig(), 'HEAD');
    const skills = read[0]?.categories[1];
    expect([...(skills?.files.keys() ?? [])]).toEqual(['foo/SKILL.md', 'foo/bar/note.md', 'plain.md']);
  });
});

describe('readStoreSnapshotAtCommit — 历史 commit（D3 base 语义）', () => {
  it('读旧 commit 得到旧内容，读 HEAD 得到新内容', () => {
    const config = sampleConfig();
    const { home, paths } = committedHome(sampleSnapshot('v1'), config);

    const shaV1 = gitOk(home, ['rev-parse', 'HEAD']).trim();

    writeSnapshotToStore(paths, sampleSnapshot('v2'));
    writeFile(path.join(paths.storeDir, 'pi', 'skills', 'new.md'), 'added in v2\n');
    const shaV2 = commitAllStore(home, 'store v2');
    expect(shaV2).not.toBe(shaV1);

    const atV1 = readStoreSnapshotAtCommit(paths, config, shaV1);
    const atV2 = readStoreSnapshotAtCommit(paths, config, shaV2 as string);

    expect(atV1).toEqual(expectedFull(sampleSnapshot('v1')));
    expect(atV1[0]?.categories[0]?.files.get('settings.json')?.content).toContain('"v1"');
    expect(atV2[0]?.categories[0]?.files.get('settings.json')?.content).toContain('"v2"');
    expect(atV1[0]?.categories[1]?.files.has('new.md')).toBe(false);
    expect(atV2[0]?.categories[1]?.files.has('new.md')).toBe(true);
  });

  it('@{upstream} 可作 commitish（D4 remote 语义）', () => {
    const config = sampleConfig();
    const { home, paths } = committedHome(sampleSnapshot('v1'), config);

    // ensureGitRepo 用不带 -b 的 `git init`，默认分支名取决于环境（master/main）——
    // 用 symbolic-ref 取当前分支名，不硬编码。
    const branch = gitOk(home, ['symbolic-ref', '--short', 'HEAD']).trim();
    gitOk(home, ['remote', 'add', 'origin', home]); // 自指 remote，仅用于让 upstream 可解析
    gitOk(home, ['update-ref', `refs/remotes/origin/${branch}`, 'HEAD']);
    gitOk(home, ['branch', `--set-upstream-to=origin/${branch}`, branch]);

    const atUpstream = readStoreSnapshotAtCommit(paths, config, '@{upstream}');
    expect(atUpstream[0]?.categories[0]?.files.get('settings.json')?.content).toContain('"v1"');
  });

  it('失效 commitish → 空 Map 结构（不抛）', () => {
    const config = sampleConfig();
    const { paths } = committedHome();
    expect(() => readStoreSnapshotAtCommit(paths, config, 'not-a-real-ref')).not.toThrow();
    const read = readStoreSnapshotAtCommit(paths, config, 'not-a-real-ref');
    expect(read.map((a) => a.categories.every((c) => c.files.size === 0))).toEqual([true, true]);
  });
});

describe('readStoreSnapshotAtCommit — config 对齐与过滤', () => {
  it('enabled:false 的 adapter / category 不出现', () => {
    const config = sampleConfig();
    const disabled: HomerConfig = {
      version: 1,
      adapters: {
        pi: {
          root: '/tmp/pi-root',
          enabled: true,
          categories: {
            ...config.adapters['pi']?.categories,
            skills: {
              ...(config.adapters['pi']?.categories['skills'] as CategoryConfig),
              enabled: false,
            },
          },
        },
        other: { root: '/tmp/other-root', enabled: false, categories: { data: { paths: ['data/'], mode: 'mirror' } } },
      },
    };
    const { paths } = committedHome(sampleSnapshot(), disabled);

    const read = readStoreSnapshotAtCommit(paths, disabled, 'HEAD');
    expect(read.map((a) => a.adapterId)).toEqual(['pi']);
    expect(read[0]?.categories.map((c) => c.category)).toEqual(['settings', 'notes']);
    expect(read[0]?.categories.find((c) => c.category === 'skills')).toBeUndefined();
  });

  it('.homer-complete 被跳过（工作区读取同样跳过）', () => {
    const { home, paths } = committedHome();
    expect(fs.existsSync(path.join(paths.storeDir, 'pi', STORE_COMPLETE_MARKER))).toBe(true);
    expect(gitOk(home, ['ls-tree', '-r', '--name-only', 'HEAD', '--', 'store/'])).toContain(
      `store/pi/${STORE_COMPLETE_MARKER}`,
    );

    for (const read of [readStoreSnapshotAtCommit(paths, sampleConfig(), 'HEAD'), readSnapshotFromStore(paths, sampleConfig())]) {
      for (const category of read[0]?.categories ?? []) {
        expect([...category.files.keys()].some((k) => k.includes(STORE_COMPLETE_MARKER))).toBe(false);
      }
    }
  });

  it('多单文件型 category（paths 多文件）→ relPath = 各自 basename', () => {
    const config: HomerConfig = {
      version: 1,
      adapters: {
        pi: {
          root: '/tmp/pi-root',
          enabled: true,
          categories: { settings: { paths: ['settings.json', 'keybindings.json'], mode: 'merge' } },
        },
      },
    };
    const { paths } = committedHome(
      {
        adapterId: 'pi',
        categories: [
          {
            adapterId: 'pi',
            category: 'settings',
            mode: 'merge',
            files: new Map([
              ['settings.json', jsonEntry({ a: 1 })],
              ['keybindings.json', jsonEntry({ b: 2 })],
            ]),
          },
        ],
      },
      config,
    );

    const read = readStoreSnapshotAtCommit(paths, config, 'HEAD');
    expect([...(read[0]?.categories[0]?.files.keys() ?? [])]).toEqual(['keybindings.json', 'settings.json']);
    expect(read).toEqual(readSnapshotFromStore(paths, config));
  });

  it('非 git 目录（无 commit）→ 空 Map 结构，不抛', () => {
    const home = mkTmp('reader-nongit');
    const paths = pathsFor(home);
    expect(() => readStoreSnapshotAtCommit(paths, sampleConfig(), 'HEAD')).not.toThrow();
    const read = readStoreSnapshotAtCommit(paths, sampleConfig(), 'HEAD');
    expect(read).toHaveLength(2);
    expect(read.every((a) => a.categories.every((c) => c.files.size === 0))).toBe(true);
  });

  it('git 不可用（PATH 抹掉）→ 空 Map 结构，不抛', () => {
    const { paths } = committedHome();
    const saved = process.env['PATH'];
    try {
      process.env['PATH'] = '/nonexistent-homer-path';
      const read = readStoreSnapshotAtCommit(paths, sampleConfig(), 'HEAD');
      expect(read.every((a) => a.categories.every((c) => c.files.size === 0))).toBe(true);
    } finally {
      if (saved === undefined) delete process.env['PATH'];
      else process.env['PATH'] = saved;
    }
  });

  it('空/非法 commitish → 空 Map 结构，不抛', () => {
    const { paths } = committedHome();
    for (const commitish of ['', 'not-a-real-ref', 'HEAD~99']) {
      expect(() => readStoreSnapshotAtCommit(paths, sampleConfig(), commitish)).not.toThrow();
      const read = readStoreSnapshotAtCommit(paths, sampleConfig(), commitish);
      expect(read.every((a) => a.categories.every((c) => c.files.size === 0))).toBe(true);
    }
  });
});

describe('readVaultFileAtCommit — 二进制 blob 读取（§2.3 additive）', () => {
  it('读回 Buffer，逐字节等于写入内容（含 0x00 / 0xFF，防 UTF-8 置换）', () => {
    const home = initRepo('readvault-binary');
    const bytes = Buffer.from([0x00, 0xff, 0x80, 0x41, 0x0a, 0xc3, 0x28, 0x00, 0xfe]);
    writeFile(path.join(home, 'secrets', 'bin.age'), '');
    fs.writeFileSync(path.join(home, 'secrets', 'bin.age'), bytes);
    commitPaths(home, ['secrets/'], 'seed');

    const read = readVaultFileAtCommit(home, 'secrets/bin.age', 'HEAD');
    expect(read).toBeInstanceOf(Buffer);
    expect(read!.equals(bytes)).toBe(true);
  });

  it('不同 commitish 读到各自版本；HEAD~1 里不存在的路径 → undefined', () => {
    const home = initRepo('readvault-revs');
    writeFile(path.join(home, 'secrets', 'old.age'), 'v1\n');
    const c1 = commitPaths(home, ['secrets/'], 'c1');
    writeFile(path.join(home, 'secrets', 'old.age'), 'v2\n');
    writeFile(path.join(home, 'secrets', 'added.age'), 'new\n');
    commitPaths(home, ['secrets/'], 'c2');

    expect(readVaultFileAtCommit(home, 'secrets/old.age', 'HEAD')!.toString()).toBe('v2\n');
    expect(readVaultFileAtCommit(home, 'secrets/old.age', c1!)!.toString()).toBe('v1\n');
    // 该文件在此 commit 不存在（而不是空内容）→ undefined，与「缺失」同构。
    expect(readVaultFileAtCommit(home, 'secrets/added.age', c1!)).toBeUndefined();
  });

  it('`@{upstream}` 可作 commitish（pull 的远端读取口）', () => {
    const bare = initBare('readvault-upstream');
    const local = initRepo('readvault-upstream-local');
    writeFile(path.join(local, 'secrets', 'a.age'), 'local-v1\n');
    commitPaths(local, ['secrets/'], 'seed');
    gitOk(local, ['remote', 'add', 'origin', bare]);
    gitOk(local, ['push', '-u', 'origin', 'main']);

    writeFile(path.join(local, 'secrets', 'a.age'), 'local-v2\n');
    const second = commitPaths(local, ['secrets/'], 'v2');
    expect(second).toBeDefined();
    gitOk(local, ['push']);

    // 远端已被看到：@ (upstream) 指向最新已推送版本。
    expect(gitFetch(local).ok).toBe(true);
    expect(readVaultFileAtCommit(local, 'secrets/a.age', '@{upstream}')!.toString()).toBe('local-v2\n');
  });

  it('路径不存在 / 非法 commitish / 非仓库 / git 不可用 → undefined（不抛）', () => {
    const home = initRepo('readvault-miss');
    writeFile(path.join(home, 'secrets', 'a.age'), 'c1\n');
    commitPaths(home, ['secrets/'], 'seed');

    expect(readVaultFileAtCommit(home, 'secrets/nope.age', 'HEAD')).toBeUndefined();
    expect(readVaultFileAtCommit(home, 'secrets/a.age', 'definitely-not-a-ref')).toBeUndefined();
    expect(readVaultFileAtCommit(mkTmp('readvault-nonrepo'), 'secrets/a.age', 'HEAD')).toBeUndefined();
  });

  it('空 blob（合法空文件）→ 空 Buffer（与 undefined 可区分）', () => {
    const home = initRepo('readvault-emptyblob');
    writeFile(path.join(home, 'secrets', 'empty.age'), '');
    commitPaths(home, ['secrets/'], 'seed');

    const read = readVaultFileAtCommit(home, 'secrets/empty.age', 'HEAD');
    expect(read).toBeInstanceOf(Buffer);
    expect(read!.length).toBe(0);
  });
});
