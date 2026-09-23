/**
 * P2-W6 · `src/core/sync/apply.ts` 验收（docs/m2-plan.md §2.5 / §3-P2-W6）。
 *
 * 全程隔离：`HOMER_HOME=$(mkdtemp)` + adapter root 也落在临时目录（假 HOME），
 * 绝不碰真实 `~/.homer` / `~/.pi`。所有断言都打在**绝对路径**上（风险清单第 1 条）。
 *
 * 覆盖：write 含父目录自动创建、delete 三态（备份存在→rm→空父目录清理；源不存在→只记 deleted）、
 * conflict 跳过保留本地、`backup=false` 不落 backups/、**备份文件内容 == 覆盖前内容**的精确断言、
 * 父目录清理止于 category 根、多动作合并到同一个 backupDir。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { applyPullActions } from '../../../src/core/sync/apply.js';
import { resolveCategoryFilePath } from '../../../src/adapters/paths.js';
import { getHomerPaths, type HomerPaths } from '../../../src/core/paths.js';
import type { HomerConfig } from '../../../src/core/types.js';
import type { PullPlan } from '../../../src/core/sync/types.js';

interface Harness {
  tmp: string;
  paths: HomerPaths;
  agentRoot: string;
  config: HomerConfig;
}

const created: string[] = [];

afterEach(() => {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) fs.rmSync(dir, { recursive: true, force: true });
  }
});

function setup(): Harness {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-sync-apply-'));
  created.push(tmp);

  const homerHome = path.join(tmp, 'homer');
  const agentRoot = path.join(tmp, 'home', '.pi', 'agent');
  fs.mkdirSync(agentRoot, { recursive: true });

  const config: HomerConfig = {
    version: 1,
    adapters: {
      pi: {
        root: agentRoot,
        enabled: true,
        categories: {
          // 目录型 ×2（一个 merge、一个 mirror）
          settings: { paths: ['settings.json', 'keybindings.json'], mode: 'merge' },
          skills: { paths: ['skills/'], mode: 'mirror' },
          themes: { paths: ['themes/'], mode: 'mirror' },
        },
      },
    },
  };

  return { tmp, paths: getHomerPaths({ HOMER_HOME: homerHome }), agentRoot, config };
}

function write(file: string, content: string): void {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, content, 'utf8');
}

/** 递归列出目录下所有文件（相对目录，posix），用于「父目录清理」断言。 */
function listFiles(dir: string, prefix = ''): string[] {
  if (!fs.existsSync(dir)) return [];
  return fs.readdirSync(dir, { withFileTypes: true })
    .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))
    .flatMap((entry) =>
      entry.isDirectory()
        ? listFiles(path.join(dir, entry.name), `${prefix}${entry.name}/`)
        : [`${prefix}${entry.name}`],
    );
}

/** 备份目录（`<backupsDir>/<date>/<time>-pull/`）→ 其下相对路径清单。 */
function backupFilesOf(backupDir: string): string[] {
  return listFiles(backupDir);
}

/* ------------------------------------------------------------------ */
/* write                                                               */
/* ------------------------------------------------------------------ */

describe('applyPullActions — write', () => {
  it('父目录不存在时自动 mkdir -p，写入绝对路径 + 内容精确匹配', () => {
    const h = setup();
    const plan: PullPlan = {
      actions: [
        { type: 'write', adapterId: 'pi', category: 'skills', relPath: 'deep/new/SKILL.md', content: '# new\n' },
      ],
    };

    const result = applyPullActions(h.paths, h.config, plan);

    const expectedAbs = path.join(h.agentRoot, 'skills', 'deep', 'new', 'SKILL.md');
    expect(fs.readFileSync(expectedAbs, 'utf8')).toBe('# new\n');
    expect(result.written).toEqual([{ adapterId: 'pi', category: 'skills', relPath: 'deep/new/SKILL.md' }]);
    expect(result.deleted).toEqual([]);
    expect(result.conflicts).toEqual([]);
    // 新增文件无需备份 → 不创建 backups/，backupDir 保持 undefined
    expect(result.backupDir).toBeUndefined();
    expect(fs.existsSync(h.paths.backupsDir)).toBe(false);
  });

  it('单文件型 category：写入 <root>/settings.json（不创建多余目录）', () => {
    const h = setup();
    const result = applyPullActions(h.paths, h.config, {
      actions: [
        { type: 'write', adapterId: 'pi', category: 'settings', relPath: 'settings.json', content: '{"theme":"dark"}\n' },
        { type: 'write', adapterId: 'pi', category: 'settings', relPath: 'keybindings.json', content: '{"keys":[]}\n' },
      ],
    });

    expect(fs.readFileSync(path.join(h.agentRoot, 'settings.json'), 'utf8')).toBe('{"theme":"dark"}\n');
    expect(fs.readFileSync(path.join(h.agentRoot, 'keybindings.json'), 'utf8')).toBe('{"keys":[]}\n');
    expect(result.written).toHaveLength(2);
    // 覆盖不存在的老文件 → 无备份
    expect(result.backupDir).toBeUndefined();
  });

  it('覆盖已存在的本地文件：备份内容 == 覆盖前内容（精确断言）', () => {
    const h = setup();
    const targetAbs = path.join(h.agentRoot, 'skills', 'alpha', 'SKILL.md');
    const oldContent = '# alpha v1（用户的旧内容，含换行与中文）\n第二行\n';
    write(targetAbs, oldContent);

    const result = applyPullActions(h.paths, h.config, {
      actions: [
        { type: 'write', adapterId: 'pi', category: 'skills', relPath: 'alpha/SKILL.md', content: '# alpha v2\n' },
      ],
    });

    // 目标被覆盖
    expect(fs.readFileSync(targetAbs, 'utf8')).toBe('# alpha v2\n');

    // 备份存在且内容 == 覆盖前内容（字节级）
    expect(result.backupDir).toBeDefined();
    const backupDir = result.backupDir as string;
    const backedUpAbs = path.join(backupDir, 'pi', 'skills', 'alpha', 'SKILL.md');
    expect(fs.existsSync(backedUpAbs)).toBe(true);
    expect(fs.readFileSync(backedUpAbs, 'utf8')).toBe(oldContent);
    expect(backupFilesOf(backupDir)).toEqual(['pi/skills/alpha/SKILL.md']);
  });

  it('write 到已存在的目录位置（path 撞目录）→ 抛错而非静默失败', () => {
    const h = setup();
    // 让目标路径上先有一个**目录**
    fs.mkdirSync(path.join(h.agentRoot, 'settings.json', 'inner'), { recursive: true });

    expect(() =>
      applyPullActions(h.paths, h.config, {
        actions: [
          { type: 'write', adapterId: 'pi', category: 'settings', relPath: 'settings.json', content: 'x' },
        ],
      }),
    ).toThrow();
  });
});

/* ------------------------------------------------------------------ */
/* delete                                                              */
/* ------------------------------------------------------------------ */

describe('applyPullActions — delete', () => {
  it('源存在：备份 → rm → 空父目录清理（逐级向上，止于 category 根之前）', () => {
    const h = setup();
    const targetAbs = path.join(h.agentRoot, 'skills', 'foo', 'bar', 'SKILL.md');
    const content = '# foo bar\n';
    write(targetAbs, content);

    const result = applyPullActions(h.paths, h.config, {
      actions: [{ type: 'delete', adapterId: 'pi', category: 'skills', relPath: 'foo/bar/SKILL.md' }],
    });

    // rm 生效
    expect(fs.existsSync(targetAbs)).toBe(false);
    // 空父目录链清理：foo/bar 与 foo 都被删
    expect(fs.existsSync(path.join(h.agentRoot, 'skills', 'foo'))).toBe(false);
    // **category 根保留**（skills/ 不是本次删除的产物）
    expect(fs.existsSync(path.join(h.agentRoot, 'skills'))).toBe(true);
    // adapter root 当然保留
    expect(fs.existsSync(h.agentRoot)).toBe(true);

    // 备份内容 == 删除前内容
    expect(result.deleted).toEqual([{ adapterId: 'pi', category: 'skills', relPath: 'foo/bar/SKILL.md' }]);
    expect(result.backupDir).toBeDefined();
    const backedUpAbs = path.join(result.backupDir as string, 'pi', 'skills', 'foo', 'bar', 'SKILL.md');
    expect(fs.readFileSync(backedUpAbs, 'utf8')).toBe(content);
  });

  it('空父目录清理在遇到非空兄弟目录时停手（不误删仍被使用的目录）', () => {
    const h = setup();
    write(path.join(h.agentRoot, 'skills', 'foo', 'bar', 'SKILL.md'), 'x\n');
    write(path.join(h.agentRoot, 'skills', 'foo', 'keep', 'other.md'), 'y\n');

    applyPullActions(h.paths, h.config, {
      actions: [{ type: 'delete', adapterId: 'pi', category: 'skills', relPath: 'foo/bar/SKILL.md' }],
    });

    expect(fs.existsSync(path.join(h.agentRoot, 'skills', 'foo', 'bar'))).toBe(false);
    // foo/ 仍含 keep/ → 不删
    expect(fs.existsSync(path.join(h.agentRoot, 'skills', 'foo', 'keep', 'other.md'))).toBe(true);
    expect(listFiles(h.agentRoot)).toEqual(['skills/foo/keep/other.md']);
  });

  it('单文件型 category 的 delete：只删文件，不动 root，也不清理任何目录', () => {
    const h = setup();
    write(path.join(h.agentRoot, 'settings.json'), '{"a":1}\n');
    write(path.join(h.agentRoot, 'other-untracked.txt'), 'keep me\n');

    const result = applyPullActions(h.paths, h.config, {
      actions: [{ type: 'delete', adapterId: 'pi', category: 'settings', relPath: 'settings.json' }],
    });

    expect(fs.existsSync(path.join(h.agentRoot, 'settings.json'))).toBe(false);
    expect(fs.existsSync(path.join(h.agentRoot, 'other-untracked.txt'))).toBe(true);
    expect(result.backupDir).toBeDefined();
    expect(fs.readFileSync(path.join(result.backupDir as string, 'pi/settings/settings.json'), 'utf8')).toBe('{"a":1}\n');
  });

  it('源不存在：只记 deleted（不备份、不创建 backupsDir、不抛）', () => {
    const h = setup();
    const result = applyPullActions(h.paths, h.config, {
      actions: [
        { type: 'delete', adapterId: 'pi', category: 'skills', relPath: 'gone/SKILL.md' },
        { type: 'delete', adapterId: 'pi', category: 'themes', relPath: 'dark.json' },
      ],
    });

    expect(result.deleted).toEqual([
      { adapterId: 'pi', category: 'skills', relPath: 'gone/SKILL.md' },
      { adapterId: 'pi', category: 'themes', relPath: 'dark.json' },
    ]);
    expect(result.backupDir).toBeUndefined();
    expect(fs.existsSync(h.paths.backupsDir)).toBe(false);
    expect(result.written).toEqual([]);
    expect(result.conflicts).toEqual([]);
  });

  it('目录型 category 里只剩一个空目录链时：整条链被清空但 category 根保留', () => {
    const h = setup();
    write(path.join(h.agentRoot, 'themes', 'a', 'b', 'c.json'), '{}\n');

    applyPullActions(h.paths, h.config, {
      actions: [{ type: 'delete', adapterId: 'pi', category: 'themes', relPath: 'a/b/c.json' }],
    });

    expect(listFiles(path.join(h.agentRoot, 'themes'))).toEqual([]);
    expect(fs.existsSync(path.join(h.agentRoot, 'themes'))).toBe(true);
  });
});

/* ------------------------------------------------------------------ */
/* conflict                                                            */
/* ------------------------------------------------------------------ */

describe('applyPullActions — conflict', () => {
  it('跳过并保留本地内容，记入 conflicts（不写、不删、不备份）', () => {
    const h = setup();
    const targetAbs = path.join(h.agentRoot, 'settings.json');
    const localContent = '{"theme":"local"}\n';
    write(targetAbs, localContent);

    const result = applyPullActions(h.paths, h.config, {
      actions: [
        {
          type: 'conflict',
          adapterId: 'pi',
          category: 'settings',
          relPath: 'settings.json',
          reason: 'modify-vs-modify',
          localContent,
          remoteContent: '{"theme":"remote"}\n',
        },
        { type: 'write', adapterId: 'pi', category: 'skills', relPath: 'x/SKILL.md', content: '# x\n' },
      ],
    });

    // 本地原样保留
    expect(fs.readFileSync(targetAbs, 'utf8')).toBe(localContent);
    expect(result.conflicts).toEqual([
      { adapterId: 'pi', category: 'settings', relPath: 'settings.json' },
    ]);
    // 同一 plan 里的其它动作照常应用
    expect(result.written).toEqual([{ adapterId: 'pi', category: 'skills', relPath: 'x/SKILL.md' }]);
    // conflict 不触发备份（本地文件根本没被碰）
    expect(result.backupDir).toBeUndefined();
  });

  it('conflict 的本地文件不存在时也不创建（保留「本地缺失」这一事实）', () => {
    const h = setup();
    const result = applyPullActions(h.paths, h.config, {
      actions: [
        {
          type: 'conflict',
          adapterId: 'pi',
          category: 'skills',
          relPath: 'missing/SKILL.md',
          reason: 'local-delete-vs-remote-modify',
          remoteContent: '# remote\n',
        },
      ],
    });

    expect(fs.existsSync(path.join(h.agentRoot, 'skills', 'missing', 'SKILL.md'))).toBe(false);
    expect(result.conflicts).toHaveLength(1);
    expect(result.backupDir).toBeUndefined();
  });
});

/* ------------------------------------------------------------------ */
/* backup 开关 / 汇总                                                   */
/* ------------------------------------------------------------------ */

describe('applyPullActions — backup 选项与结果汇总', () => {
  it('backup=false：覆盖与删除都不落 backups/，但磁盘变更照常发生', () => {
    const h = setup();
    const overwritten = path.join(h.agentRoot, 'settings.json');
    const deleted = path.join(h.agentRoot, 'skills', 'gone', 'SKILL.md');
    write(overwritten, 'OLD\n');
    write(deleted, 'GONE\n');

    const result = applyPullActions(
      h.paths,
      h.config,
      {
        actions: [
          { type: 'write', adapterId: 'pi', category: 'settings', relPath: 'settings.json', content: 'NEW\n' },
          { type: 'delete', adapterId: 'pi', category: 'skills', relPath: 'gone/SKILL.md' },
        ],
      },
      { backup: false },
    );

    expect(fs.readFileSync(overwritten, 'utf8')).toBe('NEW\n');
    expect(fs.existsSync(deleted)).toBe(false);
    expect(result.backupDir).toBeUndefined();
    expect(fs.existsSync(h.paths.backupsDir)).toBe(false);
    expect(result.written).toHaveLength(1);
    expect(result.deleted).toHaveLength(1);
  });

  it('多动作的备份合并到同一个 backupDir（用户可一处找回全部）', () => {
    const h = setup();
    write(path.join(h.agentRoot, 'settings.json'), 'OLD-SETTINGS\n');
    write(path.join(h.agentRoot, 'skills', 'a', 'SKILL.md'), 'OLD-SKILL\n');

    const result = applyPullActions(h.paths, h.config, {
      actions: [
        { type: 'write', adapterId: 'pi', category: 'settings', relPath: 'settings.json', content: 'NEW\n' },
        { type: 'delete', adapterId: 'pi', category: 'skills', relPath: 'a/SKILL.md' },
        { type: 'write', adapterId: 'pi', category: 'themes', relPath: 'new.json', content: '{}\n' },
      ],
    });

    const backupDir = result.backupDir as string;
    expect(backupDir).toBeDefined();
    // 只有「被覆盖 / 被删除且已存在」的两个文件进了备份；新增文件不进
    expect(backupFilesOf(backupDir)).toEqual(['pi/settings/settings.json', 'pi/skills/a/SKILL.md']);
    expect(fs.readFileSync(path.join(backupDir, 'pi/settings/settings.json'), 'utf8')).toBe('OLD-SETTINGS\n');
    expect(fs.readFileSync(path.join(backupDir, 'pi/skills/a/SKILL.md'), 'utf8')).toBe('OLD-SKILL\n');
    // backupsDir 下恰有一个日期目录 → 恰一个时间目录
    const dateDirs = fs.readdirSync(h.paths.backupsDir);
    expect(dateDirs).toHaveLength(1);
    expect(fs.readdirSync(path.join(h.paths.backupsDir, dateDirs[0] as string))).toHaveLength(1);
  });

  it('空 plan → 全空结果，零磁盘写入', () => {
    const h = setup();
    const result = applyPullActions(h.paths, h.config, { actions: [] });
    expect(result).toEqual({ written: [], deleted: [], conflicts: [] });
    expect(fs.existsSync(h.paths.backupsDir)).toBe(false);
  });

  it('plan 里出现 config 未声明的 category → 抛 CliError（plan 与 config 失配）', () => {
    const h = setup();
    expect(() =>
      applyPullActions(h.paths, h.config, {
        actions: [{ type: 'write', adapterId: 'pi', category: 'nope', relPath: 'x.md', content: 'x' }],
      }),
    ).toThrow(/plan 与 config 失配/);
  });

  it('plan 里出现逃逸 relPath（..）→ 抛错且整批拒绝（无任何写操作）', () => {
    const h = setup();
    expect(() =>
      applyPullActions(h.paths, h.config, {
        actions: [
          { type: 'write', adapterId: 'pi', category: 'skills', relPath: 'x.md', content: 'ok\n' },
          { type: 'write', adapterId: 'pi', category: 'skills', relPath: '../escape.md', content: 'bad\n' },
        ],
      }),
    ).toThrow(/\.\./);

    // 解析阶段先于一切：连合法动作也不落盘（原子性），逃逸目标当然也不存在
    expect(fs.existsSync(path.join(h.agentRoot, 'skills', 'x.md'))).toBe(false);
    expect(fs.existsSync(path.join(h.agentRoot, 'escape.md'))).toBe(false);
    expect(fs.existsSync(path.join(h.tmp, 'escape.md'))).toBe(false);
  });

  it('目录型 + 单文件型混合声明时：清理边界与 resolve 的裁定一致（不在 root 下乱删）', () => {
    const h = setup();
    // paths 顺序 = ['settings.json', 'extra/']：对嵌套 relPath，目录型最后匹配 → 目标在 extra/ 下
    const config: HomerConfig = {
      version: 1,
      adapters: {
        pi: {
          root: h.agentRoot,
          enabled: true,
          categories: {
            mixed: { paths: ['settings.json', 'extra/'], mode: 'merge' },
          },
        },
      },
    };

    write(path.join(h.agentRoot, 'extra', 'a', 'b.md'), 'B\n');
    write(path.join(h.agentRoot, 'settings.json'), 'S\n');

    const result = applyPullActions(h.paths, config, {
      actions: [{ type: 'delete', adapterId: 'pi', category: 'mixed', relPath: 'a/b.md' }],
    });

    // 删除发生在 extra/ 下；空父目录 extra/a 被清掉，但 category 目录根 extra/ 保留
    // （与 skills/ 同理，也与 scan / store 的「空分类目录仍存在」语义一致）
    expect(fs.existsSync(path.join(h.agentRoot, 'extra', 'a'))).toBe(false);
    expect(fs.existsSync(path.join(h.agentRoot, 'extra'))).toBe(true);
    expect(listFiles(path.join(h.agentRoot, 'extra'))).toEqual([]);
    // root 下的其它内容绝不动
    expect(fs.existsSync(path.join(h.agentRoot, 'settings.json'))).toBe(true);
    // listFiles 只列文件（不列目录）：extra/ 虽空但仍在，settings.json 未动
    expect(listFiles(h.agentRoot)).toEqual(['settings.json']);
    expect(result.deleted).toEqual([{ adapterId: 'pi', category: 'mixed', relPath: 'a/b.md' }]);
  });

  it('写入位置与 resolveCategoryFilePath 的输出完全一致（跨模块不变式）', () => {
    const h = setup();

    const result = applyPullActions(h.paths, h.config, {
      actions: [
        { type: 'write', adapterId: 'pi', category: 'skills', relPath: 'nested/dir/SKILL.md', content: '# x\n' },
        { type: 'write', adapterId: 'pi', category: 'settings', relPath: 'keybindings.json', content: '{}\n' },
      ],
    });

    for (const written of result.written) {
      const categoryConfig = h.config.adapters['pi']?.categories[written.category];
      expect(categoryConfig).toBeDefined();
      const expectedAbs = resolveCategoryFilePath(h.agentRoot, categoryConfig as never, written.relPath);
      expect(fs.existsSync(expectedAbs), `${written.category}/${written.relPath}`).toBe(true);
    }

    // 硬编码的绝对路径断言（防两个模块一起错）
    expect(fs.existsSync(path.join(h.agentRoot, 'skills', 'nested', 'dir', 'SKILL.md'))).toBe(true);
    expect(fs.existsSync(path.join(h.agentRoot, 'keybindings.json'))).toBe(true);
  });

  it('删除不会把备份目录误当成工具目录内容（backupsDir 在 HOMER_HOME 下，互不干扰）', () => {
    const h = setup();
    write(path.join(h.agentRoot, 'settings.json'), 'OLD\n');
    applyPullActions(h.paths, h.config, {
      actions: [{ type: 'write', adapterId: 'pi', category: 'settings', relPath: 'settings.json', content: 'NEW\n' }],
    });
    // 备份落在 HOMER_HOME/backups 而不是工具目录
    expect(fs.existsSync(h.paths.backupsDir)).toBe(true);
    expect(listFiles(h.agentRoot)).toEqual(['settings.json']);
  });

  it('config.root 用 `~` 前缀时按 HOME 展开（与 scan 侧 expandHome 同规则）', () => {
    const h = setup();
    const fakeHome = path.join(h.tmp, 'home');
    const savedHome = process.env['HOME'];
    process.env['HOME'] = fakeHome;
    try {
      const config: HomerConfig = {
        version: 1,
        adapters: {
          pi: {
            root: '~/.pi/agent',
            enabled: true,
            categories: { skills: { paths: ['skills/'], mode: 'mirror' } },
          },
        },
      };

      const result = applyPullActions(h.paths, config, {
        actions: [{ type: 'write', adapterId: 'pi', category: 'skills', relPath: 'a/SKILL.md', content: '# a\n' }],
      });

      // `~/.pi/agent` → <HOME>/.pi/agent
      const expected = path.join(fakeHome, '.pi', 'agent', 'skills', 'a', 'SKILL.md');
      expect(fs.readFileSync(expected, 'utf8')).toBe('# a\n');
      expect(result.written).toHaveLength(1);
    } finally {
      if (savedHome === undefined) delete process.env['HOME'];
      else process.env['HOME'] = savedHome;
    }
  });

  it('config 失配的校验发生在任何写入之前（整批拒绝，不留下半套副作用）', () => {
    const h = setup();
    write(path.join(h.agentRoot, 'settings.json'), 'OLD\n');

    expect(() =>
      applyPullActions(h.paths, h.config, {
        actions: [
          // 第一个动作合法且目标已存在（解析阶段可算出需要备份）
          { type: 'write', adapterId: 'pi', category: 'settings', relPath: 'settings.json', content: 'NEW\n' },
          // 第二个动作的 category 不存在 → 整批抛错
          { type: 'write', adapterId: 'pi', category: 'ghost', relPath: 'x.md', content: 'x' },
        ],
      }),
    ).toThrow(/plan 与 config 失配/);

    // 合法动作也没被应用，backups/ 也没被创建（解析先于一切）
    expect(fs.readFileSync(path.join(h.agentRoot, 'settings.json'), 'utf8')).toBe('OLD\n');
    expect(fs.existsSync(h.paths.backupsDir)).toBe(false);
  });
});
