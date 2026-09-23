/**
 * herdr adapter 测试（docs/m3-plan.md §2.8 / §3-P1-W2 验收）。
 *
 * 隔离纪律：fixture 全部建在 `mkdtemp` 出来的**假 HOME** 下，并把 `process.env.HOME` 指向它，
 * 于是 `DEFAULT_HERDR_ADAPTER.root = '~/.config/herdr'` 经 `expandHome` 展开后落在假 HOME 内。
 * **绝不允许碰真实 `~/.config`**（见文件末尾的隔离断言测试）。
 */
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { DEFAULT_HERDR_ADAPTER, HERDR_ADAPTER_ID, scanAdapter } from '../../../src/adapters/herdr/index.js';
import { scanAdapter as piScanAdapter } from '../../../src/adapters/pi/index.js';
import { expandHome } from '../../../src/core/paths.js';
import type { AdapterSnapshot, CategorySnapshot, SnapshotEntry } from '../../../src/core/types.js';

let tmp: string;
let realHome: string | undefined;

beforeEach(() => {
  tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-herdr-scan-'));
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

/** 冻结版 config.toml（S0 侦察 §1.2 实测内容，含全部 4 个段）。 */
const CONFIG_TOML = [
  'onboarding = false',
  '[experimental]',
  'kitty_graphics = true',
  '[theme]',
  'name = "gruvbox"',
  'auto_switch = false',
  '[ui]',
  'status_indicators = "symbols"',
  '',
].join('\n');

/** 假 HOME 下的真实 herdr root（由默认 root 经 ~ 展开得到，等价 ~/.config/herdr）。 */
function herdrRoot(): string {
  return expandHome(DEFAULT_HERDR_ADAPTER.root);
}

describe('herdr adapter defaults（§2.8 + scout §1.5 冻结版逐字一致）', () => {
  it('DEFAULT_HERDR_ADAPTER 与冻结版逐字相同（快照）', () => {
    expect(HERDR_ADAPTER_ID).toBe('herdr');
    expect(DEFAULT_HERDR_ADAPTER).toEqual({
      root: '~/.config/herdr',
      enabled: true,
      categories: {
        config: { paths: ['config.toml'], mode: 'mirror' },
      },
      ignore: ['session.json', '*.sock', '*.log', '.plugins.lock', 'release-notes.json'],
    });
  });

  it('分类表 / ignore 列表逐项冻结（顺序也固定）', () => {
    expect(Object.keys(DEFAULT_HERDR_ADAPTER.categories)).toEqual(['config']);
    expect(DEFAULT_HERDR_ADAPTER.categories['config']).toEqual({
      paths: ['config.toml'],
      mode: 'mirror',
    });
    expect(DEFAULT_HERDR_ADAPTER.ignore).toEqual([
      'session.json',
      '*.sock',
      '*.log',
      '.plugins.lock',
      'release-notes.json',
    ]);
    // root 必须是 '~' 前缀（由 paths 层展开，adapter 自身不碰 HOME）
    expect(DEFAULT_HERDR_ADAPTER.root.startsWith('~/')).toBe(true);
    expect(DEFAULT_HERDR_ADAPTER.enabled).toBe(true);
  });

  it('scan 实现就是通用 scanAdapter（零新增扫描逻辑，re-export 同一函数）', () => {
    expect(scanAdapter).toBe(piScanAdapter);
  });
});

describe('scanAdapter(herdr) — 冻结分类 fixture', () => {
  beforeEach(() => {
    // 唯一的同步目标
    write('.config/herdr/config.toml', CONFIG_TOML);
    // 垃圾（fixture 里全部实际存在，用来证明 ignore 生效）
    write('.config/herdr/session.json', '{"workspaces":[{"cwd":"/root/code/homer-cli"}]}');
    write('.config/herdr/herdr.sock', 'MUST NOT APPEAR');
    write('.config/herdr/herdr-client.sock', 'MUST NOT APPEAR');
    write('.config/herdr/herdr-client.log', 'client log: MUST NOT APPEAR');
    write('.config/herdr/herdr-server.log', 'server log: MUST NOT APPEAR');
    write('.config/herdr/.plugins.lock', 'MUST NOT APPEAR');
    write('.config/herdr/release-notes.json', '{"version":"0.9.1","MUST":"NOT APPEAR"}');
  });

  it('快照精确匹配：只有 config/config.toml，垃圾 0 条', () => {
    const { snapshot, errors } = scanAdapter(HERDR_ADAPTER_ID, DEFAULT_HERDR_ADAPTER);

    expect(errors).toEqual([]);
    expect(snapshot.adapterId).toBe('herdr');
    expect(snapshot.categories.map((c) => c.category)).toEqual(['config']);
    expect(dump(snapshot)).toEqual({
      config: {
        'config.toml': { kind: 'file', content: CONFIG_TOML },
      },
    });
  });

  it('TOML 按 file：mirror 分类即使内容可 parse 也永不标 json', () => {
    const { snapshot } = scanAdapter(HERDR_ADAPTER_ID, DEFAULT_HERDR_ADAPTER);
    const c = cat(snapshot, 'config');
    expect(c.mode).toBe('mirror');
    expect(c.adapterId).toBe('herdr');
    expect(c.files.get('config.toml')?.kind).toBe('file');
    expect([...c.files.keys()]).toEqual(['config.toml']);
  });

  it('垃圾 0 条：session/sock/log/.plugins.lock/release-notes 全不出现', () => {
    const { snapshot } = scanAdapter(HERDR_ADAPTER_ID, DEFAULT_HERDR_ADAPTER);
    const allKeys = snapshot.categories.flatMap((c) => [...c.files.keys()]);
    const allText = snapshot.categories
      .flatMap((c) => [...c.files.values()])
      .map((e) => e.content)
      .join('\n');

    for (const junk of [
      'session.json',
      'herdr.sock',
      'herdr-client.sock',
      'herdr-client.log',
      'herdr-server.log',
      '.plugins.lock',
      'release-notes.json',
    ]) {
      expect(allKeys).not.toContain(junk);
    }
    expect(allText).not.toContain('MUST NOT APPEAR');
    expect(allText).not.toContain('/root/code/homer-cli'); // session.json 里的机器本地 cwd
    expect(allText).not.toContain('0.9.1');
  });

  it('config.toml 缺失（root 存在但无该文件）→ 空 files，不报错', () => {
    fs.rmSync(path.join(herdrRoot(), 'config.toml'));
    const { snapshot, errors } = scanAdapter(HERDR_ADAPTER_ID, DEFAULT_HERDR_ADAPTER);
    expect(errors).toEqual([]);
    expect(cat(snapshot, 'config').files.size).toBe(0);
  });

  it('ignore 之外的未知文件也不进快照（只有声明路径才被收集）', () => {
    write('.config/herdr/random-unknown.json', '{"unknown":true}');
    write('.config/herdr/sub/other.toml', 'x = 1');
    const { snapshot, errors } = scanAdapter(HERDR_ADAPTER_ID, DEFAULT_HERDR_ADAPTER);
    expect(errors).toEqual([]);
    expect(dump(snapshot)).toEqual({
      config: { 'config.toml': { kind: 'file', content: CONFIG_TOML } },
    });
  });
});

describe('scanAdapter(herdr) — root 缺失 / 关闭', () => {
  it('root 不存在 → 空 snapshot + errors，不 throw', () => {
    // 假 HOME 下刻意不创建 .config/herdr
    const expectedRoot = herdrRoot();
    expect(fs.existsSync(expectedRoot)).toBe(false);

    let outcome: ReturnType<typeof scanAdapter> | undefined;
    expect(() => {
      outcome = scanAdapter(HERDR_ADAPTER_ID, DEFAULT_HERDR_ADAPTER);
    }).not.toThrow();

    const { snapshot, errors } = outcome!;
    expect(snapshot.adapterId).toBe('herdr');
    expect(snapshot.categories).toEqual([]);
    expect(errors).toHaveLength(1);
    expect(errors[0]?.path).toBe(expectedRoot);
    expect(errors[0]?.message).toMatch(/ENOENT|no such file/);
  });

  it('root 是文件而非目录 → 空 snapshot + errors', () => {
    const root = herdrRoot();
    fs.mkdirSync(path.dirname(root), { recursive: true });
    fs.writeFileSync(root, 'not a dir');
    const { snapshot, errors } = scanAdapter(HERDR_ADAPTER_ID, DEFAULT_HERDR_ADAPTER);
    expect(snapshot.categories).toEqual([]);
    expect(errors).toHaveLength(1);
    expect(errors[0]?.message).toBe('not a directory');
  });

  it('enabled:false → 空 snapshot 且无 error', () => {
    write('.config/herdr/config.toml', CONFIG_TOML);
    const { snapshot, errors } = scanAdapter(HERDR_ADAPTER_ID, {
      ...DEFAULT_HERDR_ADAPTER,
      enabled: false,
    });
    expect(snapshot.categories).toEqual([]);
    expect(errors).toEqual([]);
  });
});

describe('隔离纪律：全程只碰 mkdtemp 假 HOME', () => {
  it('假 HOME 生效：~ 展开到 tmp 内，且真实 ~/.config 从未被读写', () => {
    const root = herdrRoot();
    expect(root.startsWith(tmp)).toBe(true);
    expect(root).toBe(path.join(tmp, '.config', 'herdr'));
    // 真实 HOME 已被测试覆盖，不会指向用户目录
    expect(os.homedir()).toBe(tmp);

    write('.config/herdr/config.toml', CONFIG_TOML);
    const { snapshot } = scanAdapter(HERDR_ADAPTER_ID, DEFAULT_HERDR_ADAPTER);
    expect(snapshot.categories).toHaveLength(1);

    // 断言我们写的东西确实在 tmp 下（而非真实 ~/.config）
    expect(fs.existsSync(path.join(tmp, '.config', 'herdr', 'config.toml'))).toBe(true);
  });
});
