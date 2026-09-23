/**
 * M3 · P1-W4 e2e 验收：`install.sh` 一键安装（docs/m3-plan.md §2.9 / §3-P1-W4）。
 *
 * 验收原文：
 *   - `sh -n install.sh` 语法通过
 *   - `npm run build && npm pack` → tarball → `HOMER_INSTALL_PACKAGE=<tarball>
 *     NPM_CONFIG_PREFIX=$(mktemp -d) sh install.sh` → `$PREFIX/bin/homer --help` exit 0
 *   - PATH 无 node（注入假 PATH）→ exit 1 + 安装指引文本
 *   - 幂等（二跑不炸）
 *
 * 隔离不变式：
 *   - 安装一律落到 `mkdtemp` 出来的前缀（`NPM_CONFIG_PREFIX` / `HOMER_INSTALL_PREFIX`），
 *     绝不碰真实全局 npm prefix（不用 sudo、不写 /usr/local/lib/node_modules）；
 *   - `npm pack` 落 tarball 到临时目录（`--pack-destination`），仓库工作区保持干净；
 *   - 假 PATH 用例只注入 `node`/`npm` 桩，不改本机任何文件。
 *
 * 唯一的外部前提：构建态 `tsc` 可跑、`npm install -g <本地 tarball>` 能把运行时依赖
 * （age-encryption / @clack/prompts）解析出来（本机 npm cache 已预热；CI 首次跑需要 registry）。
 */

import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';

/* ------------------------------------------------------------------ */
/* 常量 / harness                                                      */
/* ------------------------------------------------------------------ */

const REPO_ROOT = process.cwd();
const INSTALL_SH = path.join(REPO_ROOT, 'install.sh');

const created: string[] = [];

function mkTmp(prefix: string): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), `homer-install-e2e-${prefix}-`));
  created.push(dir);
  return dir;
}

afterEach(() => {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) {
      try {
        fs.chmodSync(dir, 0o755);
      } catch {
        /* 目录已不存在 / 权限无需回收 */
      }
      fs.rmSync(dir, { recursive: true, force: true });
    }
  }
});

/** 写一个可执行的 POSIX sh 桩（内容逐字写入，不加解释）。 */
function writeStub(dir: string, name: string, body: string): string {
  const file = path.join(dir, name);
  fs.writeFileSync(file, body, 'utf8');
  fs.chmodSync(file, 0o755);
  return file;
}

interface RunResult {
  status: number;
  stdout: string;
  stderr: string;
}

/**
 * 干净的基础环境：剔除 `VITEST`——`src/cli/index.ts` 靠它跳过入口副作用
 * （tests 导入 `run()` 时不重复跑命令），但这里启动的是**真实子进程**，
 * 必须让它真正执行 CLI（同 tests/e2e/m1.test.ts / m2.test.ts 的做法）。
 */
function cleanEnv(): NodeJS.ProcessEnv {
  const env = { ...process.env };
  delete env.VITEST;
  return env;
}

/** 以 `/bin/sh <script>` 跑安装脚本；env 覆盖项合并进当前环境（PATH / HOME 继承）。 */
function runInstall(
  script: string,
  env: Record<string, string>,
  opts: { cwd?: string; timeoutMs?: number } = {},
): RunResult {
  try {
    const stdout = execFileSync('/bin/sh', [script], {
      cwd: opts.cwd ?? REPO_ROOT,
      env: { ...cleanEnv(), ...env },
      encoding: 'utf8',
      timeout: opts.timeoutMs ?? 180_000,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    return { status: 0, stdout, stderr: '' };
  } catch (error) {
    const e = error as NodeJS.ErrnoException & {
      status?: number | null;
      stdout?: string | Buffer;
      stderr?: string | Buffer;
    };
    return {
      status: typeof e.status === 'number' ? e.status : 1,
      stdout: e.stdout?.toString() ?? '',
      stderr: e.stderr?.toString() ?? '',
    };
  }
}

/* ------------------------------------------------------------------ */
/* 共享 fixture：构建 + npm pack 出 tarball（一次，供全部安装用例复用） */
/* ------------------------------------------------------------------ */

let tarball = '';
/** pack 目录跨用例存活（不能被 afterEach 回收），收尾交给 afterAll。 */
let packDir = '';

beforeAll(() => {
  packDir = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-install-e2e-pack-'));
  execFileSync('npm', ['run', 'build'], { cwd: REPO_ROOT, stdio: ['ignore', 'pipe', 'pipe'] });
  execFileSync('npm', ['pack', '--pack-destination', packDir], {
    cwd: REPO_ROOT,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const packed = fs.readdirSync(packDir).filter((f) => f.endsWith('.tgz'));
  expect(packed).toHaveLength(1);
  tarball = path.join(packDir, packed[0] as string);
  expect(fs.existsSync(tarball)).toBe(true);
}, 300_000);

afterAll(() => {
  if (packDir !== '') fs.rmSync(packDir, { recursive: true, force: true });
});

/* ------------------------------------------------------------------ */
/* 1. 语法 / 静态契约                                                  */
/* ------------------------------------------------------------------ */

describe('install.sh — 语法与契约', () => {
  it('sh -n 语法通过（POSIX sh）', () => {
    const stdout = execFileSync('/bin/sh', ['-n', INSTALL_SH], {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    expect(stdout).toBe('');
  });

  it('暴露可配置安装源 / 前缀 / registry，默认走 npm registry', () => {
    const src = fs.readFileSync(INSTALL_SH, 'utf8');
    expect(src).toContain('HOMER_INSTALL_PACKAGE');
    expect(src).toContain('HOMER_INSTALL_PREFIX');
    expect(src).toContain('HOMER_INSTALL_REGISTRY');
    expect(src).toContain('homer-cli@latest'); // 默认包名+tag
    expect(src).toContain('https://registry.npmjs.org'); // 默认 registry（可覆盖）
    expect(src.startsWith('#!/bin/sh')).toBe(true);
  });
});

/* ------------------------------------------------------------------ */
/* 2. 主验收：tarball 安装 → homer --help                              */
/* ------------------------------------------------------------------ */

describe('install.sh — 真实安装（隔离 prefix）', () => {
  it(
    'HOMER_INSTALL_PACKAGE=<tarball> → $PREFIX/bin/homer --help exit 0',
    () => {
      const prefix = mkTmp('prefix');
      const res = runInstall(INSTALL_SH, {
        HOMER_INSTALL_PACKAGE: tarball,
        NPM_CONFIG_PREFIX: prefix,
        HOMER_INSTALL_PREFIX: prefix,
      });

      expect(res.stderr).not.toContain('✗');
      expect(res.status).toBe(0);
      expect(res.stdout).toContain('homer 安装完成');

      // 布局：bin/homer 是 shim，指向 lib/node_modules/homer-cli
      const bin = path.join(prefix, 'bin', 'homer');
      expect(fs.existsSync(bin)).toBe(true);
      expect(fs.existsSync(path.join(prefix, 'lib', 'node_modules', 'homer-cli', 'package.json'))).toBe(true);
      expect(fs.existsSync(path.join(prefix, 'lib', 'node_modules', 'homer-cli', 'dist', 'cli', 'index.js'))).toBe(
        true,
      );

      // 冒烟：直接执行安装出来的二进制
      const help = execFileSync(bin, ['--help'], { encoding: 'utf8', env: cleanEnv(), cwd: prefix });
      expect(help).toContain('homer');
      expect(help).toContain('homer home');
      // 安装脚本自己也做了冒烟，并把结论写在 stdout
      expect(res.stdout).toContain('冒烟通过');
    },
    240_000,
  );

  it(
    '幂等：同一 prefix 二跑不炸，仍是 exit 0 且可执行',
    () => {
      const prefix = mkTmp('prefix');
      const env = {
        HOMER_INSTALL_PACKAGE: tarball,
        NPM_CONFIG_PREFIX: prefix,
        HOMER_INSTALL_PREFIX: prefix,
      };

      const first = runInstall(INSTALL_SH, env);
      expect(first.status).toBe(0);

      const second = runInstall(INSTALL_SH, env);
      expect(second.status).toBe(0);
      expect(second.stderr).not.toContain('✗');
      expect(second.stdout).toContain('覆盖安装'); // 二次跑识别到已安装
      expect(second.stdout).toContain('冒烟通过');

      const help = execFileSync(path.join(prefix, 'bin', 'homer'), ['--help'], {
        encoding: 'utf8',
        env: cleanEnv(),
        cwd: prefix,
      });
      expect(help).toContain('homer');
    },
    300_000,
  );

  it(
    '安装到 prefix 时不写真实全局 npm prefix（隔离验证）',
    () => {
      const prefix = mkTmp('prefix');
      const res = runInstall(INSTALL_SH, {
        HOMER_INSTALL_PACKAGE: tarball,
        NPM_CONFIG_PREFIX: prefix,
        HOMER_INSTALL_PREFIX: prefix,
      });
      expect(res.status).toBe(0);
      // 安装物只出现在隔离 prefix 下
      expect(fs.existsSync(path.join(prefix, 'lib', 'node_modules', 'homer-cli'))).toBe(true);
      // 输出里报的前缀就是隔离前缀
      expect(res.stdout).toContain(prefix);
    },
    240_000,
  );
});

/* ------------------------------------------------------------------ */
/* 3. 环境不满足：node 缺失 / 过低 / npm 缺失                          */
/* ------------------------------------------------------------------ */

describe('install.sh — 环境前置检查', () => {
  it('PATH 无 node → exit 1 + 安装指引', () => {
    const emptyBin = mkTmp('emptybin'); // 空 PATH：连 node 都找不到
    const res = runInstall(INSTALL_SH, { PATH: emptyBin });

    expect(res.status).toBe(1);
    expect(res.stderr).toContain('未检测到 Node.js');
    // 安装指引的关键要素：版本要求 + 下载页 + 重跑提示
    expect(res.stderr).toContain('Node.js >= 20');
    expect(res.stderr).toContain('https://nodejs.org/en/download');
    expect(res.stderr).toContain('再重跑本脚本');
    expect(res.stdout).not.toContain('homer 安装完成');
  });

  it('node 版本过低（v18 桩）→ exit 1 + 版本提示', () => {
    const bin = mkTmp('oldnode');
    writeStub(bin, 'node', '#!/bin/sh\necho v18.20.0\n');
    const res = runInstall(INSTALL_SH, { PATH: bin });

    expect(res.status).toBe(1);
    expect(res.stderr).toContain('Node.js 版本过低');
    expect(res.stderr).toContain('v18.20.0');
    expect(res.stderr).toContain('Node.js >= 20');
  });

  it('有 node 无 npm → exit 1 + npm 指引', () => {
    const bin = mkTmp('nonpm');
    writeStub(bin, 'node', '#!/bin/sh\necho v22.11.0\n');
    const res = runInstall(INSTALL_SH, { PATH: bin });

    expect(res.status).toBe(1);
    expect(res.stderr).toContain('未检测到 npm');
    expect(res.stderr).toContain('docs.npmjs.com');
  });

  it(
    'npm install 失败（坏包 + 不可达 registry）→ exit 1 + 排查提示',
    () => {
      const prefix = mkTmp('prefix');
      const res = runInstall(INSTALL_SH, {
        HOMER_INSTALL_PACKAGE: 'homer-cli@0.0.0-does-not-exist',
        HOMER_INSTALL_REGISTRY: 'http://127.0.0.1:9/',
        NPM_CONFIG_PREFIX: prefix,
        HOMER_INSTALL_PREFIX: prefix,
        // 缩短 npm 失败前的等待
        npm_config_fetch_retries: '0',
        npm_config_fetch_timeout: '5000',
      });

      expect(res.status).toBe(1);
      expect(res.stderr).toContain('安装失败');
      expect(res.stderr).toContain('HOMER_INSTALL_REGISTRY');
      expect(res.stdout).not.toContain('homer 安装完成');
    },
    120_000,
  );
});
