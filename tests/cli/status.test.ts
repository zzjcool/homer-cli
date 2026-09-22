/**
 * `homer status` 测试（§2-P1-M4 验收）：
 *   - 手工 snapshot（base==remote，local 改 1 键 + 增 1 文件 + 删 1 文件）→ push=3, pull=0
 *   - 反向构造 → ↓（pull）计数
 *   - --json 结构符合 StatusReport
 *   - 有漂移时 exit（runStatus 不抛错；dispatcher 返回 0）
 *   - 无 config → 提示先 init + exit 1
 *
 * 引擎 computeDrift 是真实实现（已合入 master）；快照全部手工构造，不 import store / scan 侧实现。
 */

import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { buildStatusReport, runStatus, type StatusReport } from '../../src/cli/commands/status.js';
import { run } from '../../src/cli/index.js';
import { CliError } from '../../src/cli/render.js';
import type { AdapterDrift } from '../../src/core/engine/index.js';
import { driftFixture, writeConfig } from './helpers.js';

describe('runStatus（注入三方快照）', () => {
  let home: string;

  beforeEach(() => {
    home = mkdtempSync(join(tmpdir(), 'homer-status-'));
    writeConfig(home);
  });

  afterEach(() => {
    rmSync(home, { recursive: true, force: true });
  });

  it('base==remote，local 改 1 键 + 增 1 文件 + 删 1 文件 → push=3, pull=0', () => {
    const src = driftFixture('push');
    const report = runStatus({ homerHome: home }, src);

    expect(report.adapters).toHaveLength(1);
    expect(report.adapters[0]?.push).toBe(3);
    expect(report.adapters[0]?.pull).toBe(0);
    expect(report.adapters[0]?.conflicts).toBe(0);

    const byName = Object.fromEntries((report.adapters[0]?.categories ?? []).map((c) => [c.name, c]));
    expect(byName['settings']).toMatchObject({ push: 1, pull: 0 });
    expect(byName['skills']).toMatchObject({ push: 2, pull: 0 });
  });

  it('反向构造（base==local，remote 变化）→ ↓ 计数', () => {
    const src = driftFixture('pull');
    const report = runStatus({ homerHome: home }, src);

    expect(report.adapters[0]?.push).toBe(0);
    expect(report.adapters[0]?.pull).toBe(3);
  });

  it('远程缺失时 remote 缺省 = base（M1 语义）', () => {
    const src = driftFixture('push');
    const report = runStatus({ homerHome: home }, { base: src.base, local: src.local });
    expect(report.adapters[0]?.push).toBe(3);
    expect(report.adapters[0]?.pull).toBe(0);
  });

  it('无漂移 → 全零', () => {
    const src = driftFixture('push');
    const report = runStatus({ homerHome: home }, { base: src.base, local: src.base, remote: src.base });
    expect(report.adapters[0]).toMatchObject({ push: 0, pull: 0, conflicts: 0 });
  });

  it('--json 结构符合 StatusReport（可 JSON.parse 且字段齐全）', () => {
    const src = driftFixture('push');
    const report = runStatus({ homerHome: home, json: true }, src);

    const parsed = JSON.parse(JSON.stringify(report)) as StatusReport;
    expect(parsed.adapters[0]?.id).toBe('pi');
    expect(typeof parsed.adapters[0]?.push).toBe('number');
    expect(typeof parsed.adapters[0]?.pull).toBe('number');
    expect(typeof parsed.adapters[0]?.conflicts).toBe('number');
    for (const category of parsed.adapters[0]?.categories ?? []) {
      expect(Object.keys(category).sort()).toEqual(['conflicts', 'name', 'pull', 'push']);
    }
  });

  it('无 homer.json → 抛 CliError（提示先 init）', () => {
    const empty = mkdtempSync(join(tmpdir(), 'homer-empty-'));
    try {
      expect(() => runStatus({ homerHome: empty })).toThrowError(CliError);
      const err = (() => { try { runStatus({ homerHome: empty }); return undefined; } catch (e) { return e as CliError; } })();
      expect(err?.hint).toMatch(/homer init/);
    } finally {
      rmSync(empty, { recursive: true, force: true });
    }
  });
});

describe('buildStatusReport', () => {
  it('把 CategoryDrift[] 聚合为 adapter 级计数（手工构造 drift，不跑引擎）', () => {
    const drifts: AdapterDrift[] = [
      {
        adapterId: 'pi',
        categories: [
          { adapterId: 'pi', category: 'settings', mode: 'merge', push: 1, pull: 0, conflicts: 0, ops: [], mergeConflicts: [], changedKeys: [] },
          { adapterId: 'pi', category: 'skills', mode: 'mirror', push: 2, pull: 0, conflicts: 0, ops: [], mergeConflicts: [], changedKeys: [] },
        ],
      },
    ];
    const report = buildStatusReport(drifts);
    expect(report).toEqual({
      adapters: [
        {
          id: 'pi',
          push: 3,
          pull: 0,
          conflicts: 0,
          categories: [
            { name: 'settings', push: 1, pull: 0, conflicts: 0 },
            { name: 'skills', push: 2, pull: 0, conflicts: 0 },
          ],
        },
      ],
    });
  });
});

describe('CLI dispatch: homer status', () => {
  let home: string;

  beforeEach(() => {
    home = mkdtempSync(join(tmpdir(), 'homer-status-cli-'));
  });

  afterEach(() => {
    rmSync(home, { recursive: true, force: true });
  });

  function capture(): { io: { out: (l: string) => void; err: (l: string) => void }; out: string[]; err: string[] } {
    const out: string[] = [];
    const err: string[] = [];
    return { io: { out: (l) => out.push(l), err: (l) => err.push(l) }, out, err };
  }

  it('无 config → exit 1 且提示先 init', async () => {
    const cap = capture();
    const code = await run(['status', '--home', home], cap.io);
    expect(code).toBe(1);
    expect(cap.err.join('\n')).toMatch(/homer init/);
    expect(cap.out).toEqual([]);
  });

  it('有 config 且有漂移 → exit 0（漂移是信息不是错误）', async () => {
    writeConfig(home);
    const cap = capture();
    const code = await run(['status', '--json', '--home', home], cap.io);
    expect(code).toBe(0);
    const parsed = JSON.parse(cap.out.join('\n')) as StatusReport;
    expect(parsed.adapters.map((a) => a.id)).toEqual(['pi']);
  });

  it('status --help 可用（exit 0，不要求 config）', async () => {
    const cap = capture();
    const code = await run(['status', '--help'], cap.io);
    expect(code).toBe(0);
    expect(cap.out.join('\n')).toContain('用法: homer status');
  });

  it('未知选项 → exit 1 + 用法', async () => {
    const cap = capture();
    const code = await run(['status', '--nope'], cap.io);
    expect(code).toBe(1);
    expect(cap.err.join('\n')).toContain('用法: homer status');
  });
});
