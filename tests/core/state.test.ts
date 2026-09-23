/**
 * P0 · `src/core/state.ts` 测试（docs/m2-plan.md §2.1 / §3-P0）。
 *
 * 全部在 `HOMER_HOME=$(mkdtemp -d)` 下跑，绝不碰真实 `~/.homer`。
 * 覆盖：roundtrip、文件缺失、JSON 损坏、顶层非对象、字段级容忍（类型错的选项字段丢弃）、
 * 未知键忽略、原子写（无 tmp 残留 + 覆盖写）、父目录自动创建。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { getHomerPaths, type HomerPaths } from '../../src/core/paths.js';
import { loadState, saveState, type HomerState } from '../../src/core/state.js';

const created: string[] = [];

function tmpPaths(): HomerPaths {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-state-'));
  created.push(dir);
  return getHomerPaths({ HOMER_HOME: dir });
}

afterEach(() => {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) fs.rmSync(dir, { recursive: true, force: true });
  }
});

describe('loadState — 降级（永不 throw）', () => {
  it('文件缺失 → { version: 1 }', () => {
    const paths = tmpPaths();
    expect(fs.existsSync(paths.stateFile)).toBe(false);
    expect(loadState(paths)).toEqual({ version: 1 });
  });

  it('JSON 损坏（截断）→ { version: 1 }', () => {
    const paths = tmpPaths();
    fs.writeFileSync(paths.stateFile, '{ "version": 1, "lastSyncComm', 'utf8');
    expect(loadState(paths)).toEqual({ version: 1 });
  });

  it('顶层是数组 / 字符串 / null → { version: 1 }', () => {
    const paths = tmpPaths();
    for (const raw of ['[]', '"nope"', 'null', '42']) {
      fs.writeFileSync(paths.stateFile, raw, 'utf8');
      expect(loadState(paths)).toEqual({ version: 1 });
    }
  });

  it('stateFile 是目录（EISDIR）同样降级，不抛', () => {
    const paths = tmpPaths();
    fs.mkdirSync(paths.stateFile, { recursive: true });
    expect(loadState(paths)).toEqual({ version: 1 });
  });

  it('空文件 → { version: 1 }', () => {
    const paths = tmpPaths();
    fs.writeFileSync(paths.stateFile, '', 'utf8');
    expect(loadState(paths)).toEqual({ version: 1 });
  });
});

describe('saveState / loadState — roundtrip', () => {
  it('全字段 roundtrip 深比较相等', () => {
    const paths = tmpPaths();
    const state: HomerState = {
      version: 1,
      lastSyncCommit: 'a'.repeat(40),
      lastSyncAt: '2026-01-02T03:04:05.000Z',
      lastSyncCommand: 'push',
    };
    saveState(paths, state);
    expect(loadState(paths)).toEqual(state);
  });

  it('三个 lastSyncCommand 取值都能 roundtrip', () => {
    const paths = tmpPaths();
    for (const command of ['push', 'pull', 'merge'] as const) {
      saveState(paths, { version: 1, lastSyncCommand: command });
      expect(loadState(paths).lastSyncCommand).toBe(command);
    }
  });

  it('最小 state（仅 version）roundtrip', () => {
    const paths = tmpPaths();
    saveState(paths, { version: 1 });
    expect(loadState(paths)).toEqual({ version: 1 });
  });

  it('落盘格式为 2 空格缩进 + 末尾换行（与 homer.json 一致）', () => {
    const paths = tmpPaths();
    saveState(paths, { version: 1, lastSyncCommit: 'deadbeef' });
    const text = fs.readFileSync(paths.stateFile, 'utf8');
    expect(text).toBe('{\n  "version": 1,\n  "lastSyncCommit": "deadbeef"\n}\n');
  });
});

describe('loadState — 字段级容忍', () => {
  it('类型不对的可选字段被丢弃，其余保留', () => {
    const paths = tmpPaths();
    fs.writeFileSync(
      paths.stateFile,
      JSON.stringify({ version: 1, lastSyncCommit: 42, lastSyncAt: 'T', lastSyncCommand: 'sync' }),
      'utf8',
    );
    expect(loadState(paths)).toEqual({ version: 1, lastSyncAt: 'T' });
  });

  it('lastSyncCommand 非冻结取值（如 M2 未定义的 sync）被丢弃', () => {
    const paths = tmpPaths();
    fs.writeFileSync(paths.stateFile, JSON.stringify({ version: 1, lastSyncCommand: 'sync' }), 'utf8');
    expect(loadState(paths).lastSyncCommand).toBeUndefined();
  });

  it('空白字符串字段被丢弃', () => {
    const paths = tmpPaths();
    fs.writeFileSync(paths.stateFile, JSON.stringify({ version: 1, lastSyncCommit: '   ' }), 'utf8');
    expect(loadState(paths).lastSyncCommit).toBeUndefined();
  });

  it('未知键被忽略（消费方拿到干净形状）', () => {
    const paths = tmpPaths();
    fs.writeFileSync(
      paths.stateFile,
      JSON.stringify({ version: 1, lastSyncCommit: 'abc', futureField: { x: 1 } }),
      'utf8',
    );
    expect(loadState(paths)).toEqual({ version: 1, lastSyncCommit: 'abc' });
  });
});

describe('saveState — 原子写', () => {
  it('覆盖写：旧值整体被替换，不残留旧字段', () => {
    const paths = tmpPaths();
    saveState(paths, { version: 1, lastSyncCommit: 'old', lastSyncAt: 'T1' });
    saveState(paths, { version: 1, lastSyncCommit: 'new' });
    expect(loadState(paths)).toEqual({ version: 1, lastSyncCommit: 'new' });
  });

  it('写完后目录里没有 .tmp-<pid> 残留', () => {
    const paths = tmpPaths();
    saveState(paths, { version: 1, lastSyncCommit: 'x' });
    const leftovers = fs.readdirSync(paths.home).filter((name) => name.includes('.tmp-'));
    expect(leftovers).toEqual([]);
  });

  it('home 目录不存在时自动创建父目录', () => {
    const paths = getHomerPaths({ HOMER_HOME: path.join(tmpPaths().home, 'nested', 'homer') });
    expect(fs.existsSync(paths.home)).toBe(false);
    saveState(paths, { version: 1, lastSyncCommit: 'x' });
    expect(fs.existsSync(paths.stateFile)).toBe(true);
  });

  it('state.json 与 backupsDir 都在 home 下（§2.0-2 布局）', () => {
    const paths = tmpPaths();
    expect(paths.stateFile).toBe(path.join(paths.home, 'state.json'));
    expect(paths.backupsDir).toBe(path.join(paths.home, 'backups'));
  });
});
