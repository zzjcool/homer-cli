/**
 * P1-W2 · `src/core/backup/backup.ts` 测试（docs/m2-plan.md §2.3 / §3-P1-W2）。
 *
 * 全部在 `HOMER_HOME=$(mkdtemp -d)` 下跑，绝不碰真实 `~/.homer`。
 * 覆盖：精确备份路径 `<backupsDir>/<YYYYMMDD>/<HHmmss>-<command>/<label>`（断言绝对路径 + 内容一致）、
 * 源不存在 → skipped、目录型源整体备份、label 含 '..' / 绝对路径 → throw（且无半套备份写入）、
 * label 校验先于任何写操作、空 targets、prune 保留策略（10 个日期目录 keep=7 → 恰删最旧 3 个）、
 * prune 幂等（backupsDir 缺失）、keep=0 / 非法 keep。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { backupFiles, pruneBackups } from '../../../src/core/backup/backup.js';
import { getHomerPaths, type HomerPaths } from '../../../src/core/paths.js';

const created: string[] = [];

function tmpPaths(prefix = 'homer-backup-'): HomerPaths {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), prefix));
  created.push(dir);
  return getHomerPaths({ HOMER_HOME: dir });
}

afterEach(() => {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) fs.rmSync(dir, { recursive: true, force: true });
  }
});

function write(file: string, content: string): void {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, content, 'utf8');
}

/** 备份目录名的时间分量（本次运行内必定存在且唯一的那个）。 */
function backupSubdirs(backupDir: string): string[] {
  return fs.readdirSync(backupDir).sort();
}

describe('backupFiles — 精确路径与内容', () => {
  it('备份到 <backupsDir>/<YYYYMMDD>/<HHmmss>-pull/<label>（绝对路径断言 + 内容一致）', () => {
    const paths = tmpPaths();
    const source = path.join(paths.home, 'src', 'settings.json');
    write(source, '{"theme":"light"}\n');

    const before = new Date();
    const result = backupFiles(paths, 'pull', [
      { sourceAbs: source, label: 'pi/settings/settings.json' },
    ]);
    const after = new Date();

    // 日期目录名 = 本地时区的 YYYYMMDD；时间目录名 = HHmmss-pull。
    const stamp = (d: Date): string =>
      `${d.getFullYear()}${String(d.getMonth() + 1).padStart(2, '0')}${String(d.getDate()).padStart(2, '0')}`;
    const dates = new Set([stamp(before), stamp(after)]);
    const [dateDir, timeDir] = path.relative(paths.backupsDir, result.backupDir).split(path.sep);

    expect(dateDir).toBeDefined();
    expect(dates.has(dateDir ?? '')).toBe(true);
    expect(timeDir).toMatch(/^\d{6}-pull$/);
    expect(result.backupDir).toBe(path.join(paths.backupsDir, dateDir!, timeDir!));

    // 精确路径存在 + 内容与源一致。
    const dest = path.join(
      paths.backupsDir,
      dateDir!,
      timeDir!,
      'pi',
      'settings',
      'settings.json',
    );
    expect(dest).toBe(path.join(result.backupDir, 'pi/settings/settings.json'));
    expect(fs.readFileSync(dest, 'utf8')).toBe('{"theme":"light"}\n');

    expect(result.backedUp).toEqual(['pi/settings/settings.json']);
    expect(result.skipped).toEqual([]);
    expect(backupSubdirs(path.join(paths.backupsDir, dateDir!))).toEqual([timeDir!]);
  });

  it('多 target：各自保留 label 的下级结构，命令名进目录名', () => {
    const paths = tmpPaths();
    const a = path.join(paths.home, 'a.json');
    const b = path.join(paths.home, 'skills', 'alpha', 'SKILL.md');
    write(a, 'A\n');
    write(b, '# alpha\n');

    const result = backupFiles(paths, 'merge', [
      { sourceAbs: a, label: 'pi/settings/settings.json' },
      { sourceAbs: b, label: 'pi/skills/alpha/SKILL.md' },
    ]);

    expect(path.basename(result.backupDir)).toMatch(/^\d{6}-merge$/);
    expect(fs.readFileSync(path.join(result.backupDir, 'pi/settings/settings.json'), 'utf8')).toBe('A\n');
    expect(fs.readFileSync(path.join(result.backupDir, 'pi/skills/alpha/SKILL.md'), 'utf8')).toBe('# alpha\n');
    expect(result.backedUp).toEqual(['pi/settings/settings.json', 'pi/skills/alpha/SKILL.md']);
  });

  it('目录型源（label 指向目录）→ 递归整体备份', () => {
    const paths = tmpPaths();
    const dir = path.join(paths.home, 'skills');
    write(path.join(dir, 'alpha', 'SKILL.md'), '# alpha\n');
    write(path.join(dir, 'beta', 'SKILL.md'), '# beta\n');

    const result = backupFiles(paths, 'pull', [{ sourceAbs: dir, label: 'pi/skills' }]);

    expect(result.backedUp).toEqual(['pi/skills']);
    expect(fs.readFileSync(path.join(result.backupDir, 'pi/skills/alpha/SKILL.md'), 'utf8')).toBe('# alpha\n');
    expect(fs.readFileSync(path.join(result.backupDir, 'pi/skills/beta/SKILL.md'), 'utf8')).toBe('# beta\n');
  });

  it('空 targets → 仍创建目录（时间戳目录），两个清单为空', () => {
    const paths = tmpPaths();
    const result = backupFiles(paths, 'pull', []);
    expect(result.backedUp).toEqual([]);
    expect(result.skipped).toEqual([]);
    expect(fs.existsSync(result.backupDir)).toBe(true);
  });
});

describe('backupFiles — skipped（源不存在）', () => {
  it('源不存在 → 进 skipped，不产生备份文件', () => {
    const paths = tmpPaths();
    const exists = path.join(paths.home, 'exists.json');
    write(exists, 'E\n');

    const result = backupFiles(paths, 'pull', [
      { sourceAbs: path.join(paths.home, 'nope.json'), label: 'pi/settings/gone.json' },
      { sourceAbs: exists, label: 'pi/settings/settings.json' },
    ]);

    expect(result.skipped).toEqual(['pi/settings/gone.json']);
    expect(result.backedUp).toEqual(['pi/settings/settings.json']);
    expect(fs.existsSync(path.join(result.backupDir, 'pi/settings/gone.json'))).toBe(false);
    expect(fs.readFileSync(path.join(result.backupDir, 'pi/settings/settings.json'), 'utf8')).toBe('E\n');
  });

  it('全部源不存在 → backedUp 空，skipped 齐全', () => {
    const paths = tmpPaths();
    const result = backupFiles(paths, 'pull', [
      { sourceAbs: path.join(paths.home, 'a'), label: 'pi/a' },
      { sourceAbs: path.join(paths.home, 'b'), label: 'pi/b' },
    ]);
    expect(result.backedUp).toEqual([]);
    expect(result.skipped).toEqual(['pi/a', 'pi/b']);
  });
});

describe('backupFiles — label 安全校验', () => {
  const badLabels: [string, string][] = [
    ['含 .. 段', 'pi/../../etc/passwd'],
    ['以 .. 开头', '../outside.json'],
    ['相对 .. 前缀', '..\\win\\outside.json'],
    ['绝对路径', '/etc/passwd'],
    ['空串', ''],
  ];

  for (const [name, label] of badLabels) {
    it(`label ${name} → throw`, () => {
      const paths = tmpPaths();
      const source = path.join(paths.home, 'settings.json');
      write(source, 'S\n');

      expect(() => backupFiles(paths, 'pull', [{ sourceAbs: source, label }])).toThrow();
      // 校验先于任何写操作：连 backupsDir 都不该出现。
      expect(fs.existsSync(paths.backupsDir)).toBe(false);
    });
  }

  it('非法 command → throw', () => {
    const paths = tmpPaths();
    expect(() => backupFiles(paths, '../evil', [])).toThrow();
  });

  it('前面有合法 target 时，非法 label 仍先于写操作被拦下（不留半套备份）', () => {
    const paths = tmpPaths();
    const source = path.join(paths.home, 'settings.json');
    write(source, 'S\n');

    expect(() =>
      backupFiles(paths, 'pull', [
        { sourceAbs: source, label: 'pi/settings/settings.json' },
        { sourceAbs: source, label: '../escape.json' },
      ]),
    ).toThrow();
    expect(fs.existsSync(paths.backupsDir)).toBe(false);
  });
});

describe('pruneBackups — 保留最近 keep 个日期目录', () => {
  /** 构造 n 个日期目录，每个内含一个备份子目录与文件（确认整目录被删而非只删文件）。 */
  function seedDates(paths: HomerPaths, dates: string[]): void {
    for (const date of dates) {
      const inner = path.join(paths.backupsDir, date, '120000-pull', 'pi');
      write(path.join(inner, 'settings.json'), `${date}\n`);
    }
  }

  it('10 个日期目录 keep=7 → 恰删最旧 3 个，最近 7 个保留', () => {
    const paths = tmpPaths();
    const dates = [
      '20250101', '20250102', '20250103', '20250104', '20250105',
      '20250106', '20250107', '20250108', '20250109', '20250110',
    ];
    seedDates(paths, dates);

    const result = pruneBackups(paths, 7);

    expect(result.removed).toEqual(['20250103', '20250102', '20250101']);
    expect(result.kept).toEqual([
      '20250110', '20250109', '20250108', '20250107', '20250106', '20250105', '20250104',
    ]);
    for (const date of ['20250101', '20250102', '20250103']) {
      expect(fs.existsSync(path.join(paths.backupsDir, date))).toBe(false);
    }
    for (const date of result.kept) {
      expect(fs.existsSync(path.join(paths.backupsDir, date, '120000-pull', 'pi/settings.json'))).toBe(true);
    }
    expect(backupSubdirs(paths.backupsDir)).toEqual([...result.kept].sort());
  });

  it('缺省 keep = 7', () => {
    const paths = tmpPaths();
    const dates = Array.from({ length: 9 }, (_, i) => `2025020${i + 1}`);
    seedDates(paths, dates);

    const result = pruneBackups(paths);
    expect(result.kept).toHaveLength(7);
    expect(result.removed).toEqual(['20250202', '20250201']);
  });

  it('日期目录数 ≤ keep → 什么都不删', () => {
    const paths = tmpPaths();
    seedDates(paths, ['20250301', '20250302']);
    expect(pruneBackups(paths, 7)).toEqual({ removed: [], kept: ['20250302', '20250301'] });
  });

  it('keep=0 → 删除全部日期目录', () => {
    const paths = tmpPaths();
    seedDates(paths, ['20250401', '20250402']);
    const result = pruneBackups(paths, 0);
    expect(result.removed).toEqual(['20250402', '20250401']);
    expect(result.kept).toEqual([]);
    expect(backupSubdirs(paths.backupsDir)).toEqual([]);
  });

  it('幂等：连续两次 prune 结果稳定（第二次无删除）', () => {
    const paths = tmpPaths();
    seedDates(paths, ['20250501', '20250502', '20250503']);
    const first = pruneBackups(paths, 2);
    const second = pruneBackups(paths, 2);
    expect(first.removed).toEqual(['20250501']);
    expect(second).toEqual({ removed: [], kept: ['20250503', '20250502'] });
  });

  it('backupsDir 不存在 → 空结果，不 throw', () => {
    const paths = tmpPaths();
    expect(fs.existsSync(paths.backupsDir)).toBe(false);
    expect(pruneBackups(paths, 7)).toEqual({ removed: [], kept: [] });
  });

  it('非日期名条目（人工遗留目录 / 文件）不参与保留策略', () => {
    const paths = tmpPaths();
    seedDates(paths, ['20250601', '20250602']);
    fs.mkdirSync(path.join(paths.backupsDir, 'tmp-manual'), { recursive: true });
    fs.writeFileSync(path.join(paths.backupsDir, 'note.txt'), 'note\n', 'utf8');

    const result = pruneBackups(paths, 1);
    expect(result.removed).toEqual(['20250601']);
    expect(result.kept).toEqual(['20250602']);
    expect(fs.existsSync(path.join(paths.backupsDir, 'tmp-manual'))).toBe(true);
    expect(fs.existsSync(path.join(paths.backupsDir, 'note.txt'))).toBe(true);
  });

  it('非法 keep（-1 / 1.5）→ throw', () => {
    const paths = tmpPaths();
    expect(() => pruneBackups(paths, -1)).toThrow(/keep/);
    expect(() => pruneBackups(paths, 1.5)).toThrow(/keep/);
  });
});

describe('backupFiles + pruneBackups — 组合（retention 端到端）', () => {
  it('11 次备份（伪造历史日期目录）后 prune(7) 只留最近 7 个日期目录', () => {
    const paths = tmpPaths();
    const source = path.join(paths.home, 'settings.json');
    write(source, 'S\n');

    // 伪造 10 个历史日期目录，再用 backupFiles 产生今天那一份。
    for (let i = 0; i < 10; i += 1) {
      write(
        path.join(paths.backupsDir, `2024${String(i + 1).padStart(4, '0')}`, '000000-pull', 'pi', 'x.json'),
        `history-${i}\n`,
      );
    }
    const result = backupFiles(paths, 'pull', [{ sourceAbs: source, label: 'pi/settings/settings.json' }]);
    const today = path.relative(paths.backupsDir, result.backupDir).split(path.sep)[0]!;

    const pruned = pruneBackups(paths, 7);
    expect(pruned.kept).toContain(today);
    expect(pruned.removed).toHaveLength(4);
    // 今天的备份（含内容）未被误删。
    expect(fs.readFileSync(path.join(result.backupDir, 'pi/settings/settings.json'), 'utf8')).toBe('S\n');
  });
});

describe('backupFiles — HOMER_HOME 隔离', () => {
  it('全部写入都落在临时 HOMER_HOME 内（backupsDir 前缀断言）', () => {
    const paths = tmpPaths();
    const source = path.join(paths.home, 'src.json');
    write(source, 'S\n');
    const result = backupFiles(paths, 'pull', [{ sourceAbs: source, label: 'pi/settings/settings.json' }]);
    expect(result.backupDir.startsWith(paths.backupsDir + path.sep)).toBe(true);
    expect(paths.backupsDir).toBe(path.join(paths.home, 'backups'));
  });
});

/* ------------------------------------------------------------------ */
/* 对抗式 review M1：备份目录树权限收紧（opts.mode）                      */
/* ------------------------------------------------------------------ */

describe('backupFiles — opts.mode 权限收紧（对抗式 review M1）', () => {
  /** 递归收集备份目录树里所有条目的 mode（目录与文件分开）。 */
  function modes(root: string): { dirs: number[]; files: number[] } {
    const dirs: number[] = [];
    const files: number[] = [];
    const walk = (dir: string): void => {
      for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
        const abs = path.join(dir, entry.name);
        if (entry.isDirectory()) {
          dirs.push(fs.statSync(abs).mode & 0o777);
          walk(abs);
          continue;
        }
        files.push(fs.statSync(abs).mode & 0o777);
      }
    };
    walk(root);
    return { dirs, files };
  }

  it('不传 opts.mode → 行为与改动前逐字相同（不改变任何权限）', () => {
    const paths = tmpPaths();
    const source = path.join(paths.home, 'src.json');
    write(source, 'S\n');
    const result = backupFiles(paths, 'pull', [{ sourceAbs: source, label: 'pi/settings/settings.json' }]);

    // 这里只断言「与系统默认一致」而非固定数值：umask 因环境而异（additive 的语义就是不改）。
    const expectedDir = 0o777 & ~process.umask();
    expect(fs.statSync(result.backupDir).mode & 0o777).toBe(expectedDir);
  });

  it('mode { dir: 0o700, file: 0o600 } → 目录链与备份文件全部收紧', () => {
    const paths = tmpPaths();
    const source = path.join(paths.home, '.secrets', 'a.env');
    write(source, 'SECRET-V1\n');
    // 源文件本身故意放宽到 0644：收紧必须来自 backupFiles，而非「复制了源权限」。
    fs.chmodSync(source, 0o644);

    const result = backupFiles(
      paths,
      'secret',
      [{ sourceAbs: source, label: 'secret/a-secret' }],
      { mode: { dir: 0o700, file: 0o600 } },
    );

    // 目录链：backupsDir / 日期目录 / 时间目录 全部 0700。
    expect(fs.statSync(paths.backupsDir).mode & 0o777).toBe(0o700);
    expect(fs.statSync(path.dirname(result.backupDir)).mode & 0o777).toBe(0o700);
    expect(fs.statSync(result.backupDir).mode & 0o777).toBe(0o700);

    const backup = path.join(result.backupDir, 'secret', 'a-secret');
    expect(fs.readFileSync(backup, 'utf8')).toBe('SECRET-V1\n');
    expect(fs.statSync(backup).mode & 0o777).toBe(0o600);

    const { dirs, files } = modes(result.backupDir);
    expect(new Set(dirs)).toEqual(new Set([0o700]));
    expect(new Set(files)).toEqual(new Set([0o600]));
  });

  it('目录型源 + mode → 递归出的子目录与文件同样收紧', () => {
    const paths = tmpPaths();
    const dir = path.join(paths.home, 'skills');
    write(path.join(dir, 'alpha', 'SKILL.md'), '# alpha\n');
    fs.chmodSync(dir, 0o755);
    fs.chmodSync(path.join(dir, 'alpha'), 0o755);

    const result = backupFiles(
      paths,
      'secret',
      [{ sourceAbs: dir, label: 'secret/bundle' }],
      { mode: { dir: 0o700, file: 0o600 } },
    );

    const { dirs, files } = modes(result.backupDir);
    expect(dirs.length).toBeGreaterThan(0);
    expect(new Set(dirs)).toEqual(new Set([0o700]));
    expect(new Set(files)).toEqual(new Set([0o600]));
    // 源目录自身的权限不被改动（只收紧备份副本）。
    expect(fs.statSync(dir).mode & 0o777).toBe(0o755);
  });

  it('全 skipped（无文件）时目录仍存在且已收紧（不因“无备份”就留宽松目录）', () => {
    const paths = tmpPaths();
    const result = backupFiles(paths, 'secret', [], { mode: { dir: 0o700, file: 0o600 } });
    expect(fs.existsSync(result.backupDir)).toBe(true);
    expect(fs.statSync(result.backupDir).mode & 0o777).toBe(0o700);
    expect(fs.statSync(paths.backupsDir).mode & 0o777).toBe(0o700);
  });

  it('只给 dir（不给 file）→ 目录收紧、文件权限不动', () => {
    const paths = tmpPaths();
    const source = path.join(paths.home, 'src.json');
    write(source, 'S\n');
    fs.chmodSync(source, 0o644);

    const result = backupFiles(
      paths,
      'pull',
      [{ sourceAbs: source, label: 'pi/settings/settings.json' }],
      { mode: { dir: 0o700 } },
    );
    const backup = path.join(result.backupDir, 'pi', 'settings', 'settings.json');
    expect(fs.statSync(result.backupDir).mode & 0o777).toBe(0o700);
    expect(fs.statSync(backup).mode & 0o777).toBe(0o644);
  });
});
