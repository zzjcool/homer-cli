/**
 * CLI 测试共享工具：手工构造 snapshot（§2-P1-M4 要求单测用构造数据打桩）。
 * 不依赖 adapter 扫描 / store 读写，保证 CLI 的计数与渲染逻辑可独立验证。
 */

import { mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

import type {
  AdapterSnapshot,
  CategorySnapshot,
  SnapshotEntry,
  SnapshotFiles,
  SyncMode,
} from '../../src/core/types.js';

/**
 * 写一个最小合法 homer.json。
 * 默认 `categories: {}`（避免真扫描磁盘）；注入快照的用例只需命令能读到 config。
 */
export function writeConfig(home: string, adapters: Record<string, unknown> = { pi: { root: join(home, 'pi-agent'), enabled: true, categories: {} } }): void {
  mkdirSync(home, { recursive: true });
  writeFileSync(join(home, 'homer.json'), `${JSON.stringify({ version: 1, adapters }, null, 2)}\n`, 'utf8');
}

export function fileEntry(content: string): SnapshotEntry {
  return { kind: 'file', content };
}

export function jsonEntry(value: unknown): SnapshotEntry {
  return { kind: 'json', content: JSON.stringify(value) };
}

export function rawJsonEntry(content: string): SnapshotEntry {
  return { kind: 'json', content };
}

export function category(
  adapterId: string,
  name: string,
  mode: SyncMode,
  files: Record<string, SnapshotEntry>,
): CategorySnapshot {
  return { adapterId, category: name, mode, files: new Map(Object.entries(files)) };
}

export function adapter(adapterId: string, categories: CategorySnapshot[]): AdapterSnapshot {
  return { adapterId, categories };
}

export function files(entries: Record<string, SnapshotEntry>): SnapshotFiles {
  return new Map(Object.entries(entries));
}

/**
 * 构造一组「漂移」三方快照：
 *   - settings（merge）：local 改 1 键 → push 1
 *   - skills（mirror）：local 增 1 文件（push 1）+ 删 1 文件（push-delete → push 1）
 * 合计 push = 3、pull = 0。
 * `direction = 'pull'` 时反向（base == local，remote 变化）→ push = 0、pull = 3。
 */
export function driftFixture(direction: 'push' | 'pull'): {
  base: AdapterSnapshot[];
  local: AdapterSnapshot[];
  remote: AdapterSnapshot[];
} {
  const settingsBase = { theme: 'light', keep: 1 };
  const settingsChanged = { theme: 'dark', keep: 1 };

  const skillsBase = { 'foo.md': fileEntry('A\n'), 'bar.md': fileEntry('B\n') };
  const skillsLocal = { 'foo.md': fileEntry('A\n'), 'new.md': fileEntry('N\n') };
  const skillsRemote = { 'foo.md': fileEntry('A\n'), 'new.md': fileEntry('N\n') };

  if (direction === 'push') {
    return {
      base: [
        adapter('pi', [
          category('pi', 'settings', 'merge', { 'settings.json': jsonEntry(settingsBase) }),
          category('pi', 'skills', 'mirror', skillsBase),
        ]),
      ],
      local: [
        adapter('pi', [
          category('pi', 'settings', 'merge', { 'settings.json': jsonEntry(settingsChanged) }),
          category('pi', 'skills', 'mirror', skillsLocal),
        ]),
      ],
      remote: [
        adapter('pi', [
          category('pi', 'settings', 'merge', { 'settings.json': jsonEntry(settingsBase) }),
          category('pi', 'skills', 'mirror', skillsBase),
        ]),
      ],
    };
  }

  return {
    base: [
      adapter('pi', [
        category('pi', 'settings', 'merge', { 'settings.json': jsonEntry(settingsBase) }),
        category('pi', 'skills', 'mirror', skillsBase),
      ]),
    ],
    local: [
      adapter('pi', [
        category('pi', 'settings', 'merge', { 'settings.json': jsonEntry(settingsBase) }),
        category('pi', 'skills', 'mirror', skillsBase),
      ]),
    ],
    remote: [
      adapter('pi', [
        category('pi', 'settings', 'merge', { 'settings.json': jsonEntry(settingsChanged) }),
        category('pi', 'skills', 'mirror', skillsRemote),
      ]),
    ],
  };
}
