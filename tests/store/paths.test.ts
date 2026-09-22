import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { getHomerPaths } from '../../src/core/paths.js';

const created: string[] = [];

function tmpHome(): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-paths-'));
  created.push(dir);
  return dir;
}

afterEach(() => {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) fs.rmSync(dir, { recursive: true, force: true });
  }
});

describe('getHomerPaths', () => {
  it('HOMER_HOME 覆盖生效，派生路径都在其下', () => {
    const home = tmpHome();
    const paths = getHomerPaths({ HOMER_HOME: home });

    expect(paths.home).toBe(home);
    expect(paths.storeDir).toBe(path.join(home, 'store'));
    expect(paths.configFile).toBe(path.join(home, 'homer.json'));
    expect(paths.stateFile).toBe(path.join(home, 'state.json'));
  });

  it('未设置 HOMER_HOME → 默认 ~/.homer（不碰磁盘）', () => {
    const paths = getHomerPaths({});
    expect(paths.home).toBe(path.join(os.homedir(), '.homer'));
    expect(paths.storeDir).toBe(path.join(os.homedir(), '.homer', 'store'));
    expect(paths.configFile).toBe(path.join(os.homedir(), '.homer', 'homer.json'));
    expect(paths.stateFile).toBe(path.join(os.homedir(), '.homer', 'state.json'));
  });

  it('空字符串 / 纯空白 HOMER_HOME 视为未设置', () => {
    const fallback = path.join(os.homedir(), '.homer');
    expect(getHomerPaths({ HOMER_HOME: '' }).home).toBe(fallback);
    expect(getHomerPaths({ HOMER_HOME: '   ' }).home).toBe(fallback);
  });

  it('支持 ~ 展开', () => {
    const paths = getHomerPaths({ HOMER_HOME: '~/custom-homer' });
    expect(paths.home).toBe(path.join(os.homedir(), 'custom-homer'));
  });

  it('相对路径按 cwd 解析为绝对路径', () => {
    const paths = getHomerPaths({ HOMER_HOME: 'relative-homer-home' });
    expect(path.isAbsolute(paths.home)).toBe(true);
    expect(paths.home).toBe(path.resolve('relative-homer-home'));
  });

  it('默认参数读取 process.env.HOMER_HOME', () => {
    const home = tmpHome();
    const previous = process.env['HOMER_HOME'];
    process.env['HOMER_HOME'] = home;
    try {
      expect(getHomerPaths().home).toBe(home);
    } finally {
      if (previous === undefined) delete process.env['HOMER_HOME'];
      else process.env['HOMER_HOME'] = previous;
    }
  });
});
