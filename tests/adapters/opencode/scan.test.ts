/**
 * opencode adapter 测试（docs/m3-plan.md §2.8 / §3-P1-W2 验收）。
 *
 * 隔离纪律：fixture 全部建在 `mkdtemp` 出来的**假 HOME** 下，并把 `process.env.HOME` 指向它，
 * 于是 `DEFAULT_OPENCODE_ADAPTER.root = '~/.config/opencode'` 经 `expandHome` 展开后落在假 HOME 内。
 * **绝不允许碰真实 `~/.config`**。
 */
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import {
  DEFAULT_OPENCODE_ADAPTER,
  OPENCODE_ADAPTER_ID,
  scanAdapter,
} from '../../../src/adapters/opencode/index.js';
import { scanAdapter as piScanAdapter } from '../../../src/adapters/pi/index.js';
import { expandHome } from '../../../src/core/paths.js';
import type { AdapterSnapshot, CategorySnapshot, SnapshotEntry } from '../../../src/core/types.js';

let tmp: string;
let realHome: string | undefined;

beforeEach(() => {
  tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-opencode-scan-'));
  realHome = process.env.HOME;
  process.env.HOME = tmp; // 假 HOME：`~` 展开到 tmp 内
});

afterEach(() => {
  if (realHome === undefined) delete process.env.HOME;
  else process.env.HOME = realHome;
  fs.rmSync(tmp, { recursive: true, force: true });
});

/** 相对**假 HOME** 写文件（mkdtemp 假环境，禁碰真实 HOME）。 */
function write(rel: string, content: string): void {
  const abs = path.join(tmp, rel);
  fs.mkdirSync(path.dirname(abs), { recursive: true });
  fs.writeFileSync(abs, content, 'utf8');
}

/** 把 snapshot 转成纯对象，便于精确 toEqual。 */
function dump(snapshot: AdapterSnapshot): Record<string, Record<string, SnapshotEntry>> {
  const out: Record<string, Record<string, SnapshotEntry>> = {};
  for (const cat of snapshot.categories) {
    const files: Record<string, SnapshotEntry> = {};
    for (const [k, v] of cat.files) files[k] = v;
    out[cat.category] = files;
  }
  return out;
}

function cat(snapshot: AdapterSnapshot, name: string): CategorySnapshot {
  const found = snapshot.categories.find((c) => c.category === name);
  if (!found) throw new Error(`category not found: ${name}`);
  return found;
}

/** 精简版 opencode.json fixture（结构照 S0 §2.2 实测：provider/models/permission + 短占位 apiKey）。 */
const OPENCODE_JSON = JSON.stringify(
  {
    $schema: 'https://opencode.ai/config.json',
    provider: {
      ccrb: {
        models: { 'gpt-5': { name: 'gpt-5' } },
        options: { apiKey: 'ccrb', baseURL: 'http://0.0.0.0:3457/v1' },
      },
    },
    permission: { edit: 'ask' },
    disabled_providers: [],
  },
  null,
  2,
);
const PACKAGE_JSON = JSON.stringify(
  { dependencies: { '@opencode-ai/plugin': '1.17.12' } },
  null,
  2,
);
const PACKAGE_LOCK = JSON.stringify({ name: 'opencode-plugins', lockfileVersion: 3 }, null, 2);
const BUN_LOCK = '// bun lockfile v1\n\n[[package]]\nname = "@opencode-ai/plugin"\n';
/** opencode 自生成的 .gitignore（内容不参与同步，但要证明它不会被收）。 */
const TOOL_GITIGNORE = 'node_modules\npackage.json\nbun.lock\n.gitignore\n';

function opencodeRoot(): string {
  return expandHome(DEFAULT_OPENCODE_ADAPTER.root);
}

describe('opencode adapter defaults（§2.8 + scout §2.4 冻结版逐字一致）', () => {
  it('DEFAULT_OPENCODE_ADAPTER 与冻结版逐字相同（快照）', () => {
    expect(OPENCODE_ADAPTER_ID).toBe('opencode');
    expect(DEFAULT_OPENCODE_ADAPTER).toEqual({
      root: '~/.config/opencode',
      enabled: true,
      categories: {
        config: { paths: ['opencode.json'], mode: 'merge' },
        plugins: { paths: ['package.json'], mode: 'merge' },
        locks: { paths: ['package-lock.json', 'bun.lock'], mode: 'mirror' },
      },
      ignore: ['node_modules/', '.plugins.lock', '*.log', '.gitignore'],
    });
  });

  it('三分类与 ignore 逐项冻结（顺序也固定）', () => {
    expect(Object.keys(DEFAULT_OPENCODE_ADAPTER.categories)).toEqual(['config', 'plugins', 'locks']);
    expect(DEFAULT_OPENCODE_ADAPTER.categories['config']).toEqual({
      paths: ['opencode.json'],
      mode: 'merge',
    });
    expect(DEFAULT_OPENCODE_ADAPTER.categories['plugins']).toEqual({
      paths: ['package.json'],
      mode: 'merge',
    });
    expect(DEFAULT_OPENCODE_ADAPTER.categories['locks']).toEqual({
      paths: ['package-lock.json', 'bun.lock'],
      mode: 'mirror',
    });
    expect(DEFAULT_OPENCODE_ADAPTER.ignore).toEqual([
      'node_modules/',
      '.plugins.lock',
      '*.log',
      '.gitignore',
    ]);
    expect(DEFAULT_OPENCODE_ADAPTER.root.startsWith('~/')).toBe(true);
    expect(DEFAULT_OPENCODE_ADAPTER.enabled).toBe(true);
  });

  it('scan 实现就是通用 scanAdapter（零新增扫描逻辑，re-export 同一函数）', () => {
    expect(scanAdapter).toBe(piScanAdapter);
  });
});

describe('scanAdapter(opencode) — 冻结三分类 fixture', () => {
  beforeEach(() => {
    // 三分类代表文件
    write('.config/opencode/opencode.json', OPENCODE_JSON);
    write('.config/opencode/package.json', PACKAGE_JSON);
    write('.config/opencode/package-lock.json', PACKAGE_LOCK);
    write('.config/opencode/bun.lock', BUN_LOCK);

    // 垃圾（fixture 里全部实际存在，用来证明 ignore / 未声明路径不入快照）
    write('.config/opencode/node_modules/@opencode-ai/plugin/package.json', '{"MUST":"NOT APPEAR"}');
    write('.config/opencode/node_modules/@opencode-ai/plugin/index.js', 'MUST NOT APPEAR');
    write('.config/opencode/.plugins.lock', 'MUST NOT APPEAR');
    write('.config/opencode/opencode.log', 'MUST NOT APPEAR');
    write('.config/opencode/.gitignore', TOOL_GITIGNORE);
  });

  it('快照精确匹配：三分类 4 文件，垃圾 0 条', () => {
    const { snapshot, errors } = scanAdapter(OPENCODE_ADAPTER_ID, DEFAULT_OPENCODE_ADAPTER);

    expect(errors).toEqual([]);
    expect(snapshot.adapterId).toBe('opencode');
    expect(snapshot.categories.map((c) => c.category)).toEqual(['config', 'plugins', 'locks']);
    expect(dump(snapshot)).toEqual({
      config: {
        'opencode.json': { kind: 'json', content: OPENCODE_JSON },
      },
      plugins: {
        'package.json': { kind: 'json', content: PACKAGE_JSON },
      },
      locks: {
        'bun.lock': { kind: 'file', content: BUN_LOCK },
        'package-lock.json': { kind: 'file', content: PACKAGE_LOCK },
      },
    });
  });

  it('JSON 分类 kind 正确：merge → json；mirror 的锁文件永远 file', () => {
    const { snapshot } = scanAdapter(OPENCODE_ADAPTER_ID, DEFAULT_OPENCODE_ADAPTER);

    const config = cat(snapshot, 'config');
    expect(config.mode).toBe('merge');
    expect(config.adapterId).toBe('opencode');
    expect(config.files.get('opencode.json')?.kind).toBe('json');

    const plugins = cat(snapshot, 'plugins');
    expect(plugins.mode).toBe('merge');
    expect(plugins.files.get('package.json')?.kind).toBe('json');

    const locks = cat(snapshot, 'locks');
    expect(locks.mode).toBe('mirror');
    expect(locks.files.get('package-lock.json')?.kind).toBe('file');
    expect(locks.files.get('bun.lock')?.kind).toBe('file');
  });

  it('垃圾 0 条：node_modules / .plugins.lock / *.log / .gitignore 全不出现', () => {
    const { snapshot } = scanAdapter(OPENCODE_ADAPTER_ID, DEFAULT_OPENCODE_ADAPTER);
    const allKeys = snapshot.categories.flatMap((c) => [...c.files.keys()]);
    const allText = snapshot.categories
      .flatMap((c) => [...c.files.values()])
      .map((e) => e.content)
      .join('\n');

    for (const junk of [
      'node_modules/@opencode-ai/plugin/package.json',
      'node_modules/@opencode-ai/plugin/index.js',
      '.plugins.lock',
      'opencode.log',
      '.gitignore',
    ]) {
      expect(allKeys).not.toContain(junk);
    }
    expect(allText).not.toContain('MUST NOT APPEAR');
    expect(allText).not.toContain('MUST BE EXCLUDED');
  });

  it('node_modules 里的同名 package.json 不会被单文件 path 误收', () => {
    const { snapshot } = scanAdapter(OPENCODE_ADAPTER_ID, DEFAULT_OPENCODE_ADAPTER);
    // 单文件型 path 的 relPath = 文件名本身；且只读 root 下的 declared path，
    // 故 node_modules 内同 basename 的文件不可能覆盖它。
    expect([...cat(snapshot, 'plugins').files.keys()]).toEqual(['package.json']);
    expect(cat(snapshot, 'plugins').files.get('package.json')?.content).toBe(PACKAGE_JSON);
  });

  it('bun.lock 缺失 → 只有 package-lock.json（缺文件不算错）', () => {
    fs.rmSync(path.join(opencodeRoot(), 'bun.lock'));
    const { snapshot, errors } = scanAdapter(OPENCODE_ADAPTER_ID, DEFAULT_OPENCODE_ADAPTER);
    expect(errors).toEqual([]);
    expect([...cat(snapshot, 'locks').files.keys()]).toEqual(['package-lock.json']);
  });

  it('opencode.json 损坏 → merge 分类降级 kind:file，仍被收集', () => {
    write('.config/opencode/opencode.json', '{ "provider": { ');
    const { snapshot, errors } = scanAdapter(OPENCODE_ADAPTER_ID, DEFAULT_OPENCODE_ADAPTER);
    expect(errors).toEqual([]);
    expect(cat(snapshot, 'config').files.get('opencode.json')).toEqual({
      kind: 'file',
      content: '{ "provider": { ',
    });
  });

  it('空目录（三分类文件全缺）→ 三分类各 0 文件，不报错', () => {
    for (const f of ['opencode.json', 'package.json', 'package-lock.json', 'bun.lock']) {
      fs.rmSync(path.join(opencodeRoot(), f));
    }
    const { snapshot, errors } = scanAdapter(OPENCODE_ADAPTER_ID, DEFAULT_OPENCODE_ADAPTER);
    expect(errors).toEqual([]);
    expect(snapshot.categories.map((c) => c.category)).toEqual(['config', 'plugins', 'locks']);
    expect(snapshot.categories.every((c) => c.files.size === 0)).toBe(true);
  });
});

describe('scanAdapter(opencode) — root 缺失 / 关闭', () => {
  it('root 不存在 → 空 snapshot + errors，不 throw', () => {
    const expectedRoot = opencodeRoot();
    expect(fs.existsSync(expectedRoot)).toBe(false);

    let outcome: ReturnType<typeof scanAdapter> | undefined;
    expect(() => {
      outcome = scanAdapter(OPENCODE_ADAPTER_ID, DEFAULT_OPENCODE_ADAPTER);
    }).not.toThrow();

    const { snapshot, errors } = outcome!;
    expect(snapshot.adapterId).toBe('opencode');
    expect(snapshot.categories).toEqual([]);
    expect(errors).toHaveLength(1);
    expect(errors[0]?.path).toBe(expectedRoot);
    expect(errors[0]?.message).toMatch(/ENOENT|no such file/);
  });

  it('root 是文件而非目录 → 空 snapshot + errors', () => {
    const root = opencodeRoot();
    fs.mkdirSync(path.dirname(root), { recursive: true });
    fs.writeFileSync(root, 'not a dir');
    const { snapshot, errors } = scanAdapter(OPENCODE_ADAPTER_ID, DEFAULT_OPENCODE_ADAPTER);
    expect(snapshot.categories).toEqual([]);
    expect(errors).toHaveLength(1);
    expect(errors[0]?.message).toBe('not a directory');
  });

  it('enabled:false → 空 snapshot 且无 error', () => {
    write('.config/opencode/opencode.json', OPENCODE_JSON);
    const { snapshot, errors } = scanAdapter(OPENCODE_ADAPTER_ID, {
      ...DEFAULT_OPENCODE_ADAPTER,
      enabled: false,
    });
    expect(snapshot.categories).toEqual([]);
    expect(errors).toEqual([]);
  });
});

describe('隔离纪律：全程只碰 mkdtemp 假 HOME', () => {
  it('假 HOME 生效：~ 展开到 tmp 内，且真实 ~/.config 从未被读写', () => {
    const root = opencodeRoot();
    expect(root.startsWith(tmp)).toBe(true);
    expect(root).toBe(path.join(tmp, '.config', 'opencode'));
    expect(os.homedir()).toBe(tmp);

    write('.config/opencode/opencode.json', OPENCODE_JSON);
    const { snapshot } = scanAdapter(OPENCODE_ADAPTER_ID, DEFAULT_OPENCODE_ADAPTER);
    expect(snapshot.categories).toHaveLength(3);
    expect(fs.existsSync(path.join(tmp, '.config', 'opencode', 'opencode.json'))).toBe(true);
  });
});
