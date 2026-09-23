/**
 * M3 P0 脚手架验收（docs/m3-plan.md §2.0 / §2.1 / §3-P0-M0）。
 *
 * 覆盖 P0 新增的**可运行**部分（其余是纯类型/类型声明，由 typecheck 保证）：
 *   - §2.0-1 `SecretsConfig.recipients`/`files` + `AdapterConfig.allowEscape` 的校验；
 *   - §2.0-2 `HomerPaths.secretsDir`/`keysDir`；
 *   - §2.2 `recipientIsValid`/`secretNameValid` 与 `createAgeCryptoPort` stub；
 *   - §2.1 `COMMANDS`/USAGE 含三新命令 + 分发层 flag 表（含 secret 子命令族解析）。
 *
 * 全部临时目录走 mkdtemp，绝不碰真实 `~/.homer` / HOME。
 */

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { validateConfig } from '../../src/core/config.js';
import { getHomerPaths } from '../../src/core/paths.js';
import { COMMANDS, USAGE, splitCommand } from '../../src/cli/args.js';
import { run } from '../../src/cli/index.js';
import {
  createAgeCryptoPort,
  recipientIsValid,
  secretNameValid,
} from '../../src/core/age/types.js';
import {
  SECRET_SUBCOMMANDS,
  SECRET_USAGE,
  parseSecretSubcommand,
} from '../../src/cli/commands/secret.js';
import { HOME_USAGE } from '../../src/cli/commands/home.js';
import { DOCTOR_USAGE } from '../../src/cli/commands/doctor.js';
import { CliError } from '../../src/core/errors.js';
import type { HomerConfig } from '../../src/core/types.js';

const created: string[] = [];

function tmpHome(): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-m3-p0-'));
  created.push(dir);
  return dir;
}

afterEach(() => {
  while (created.length > 0) {
    const dir = created.pop();
    if (dir !== undefined) fs.rmSync(dir, { recursive: true, force: true });
  }
});

/** 真实的 age recipient（用 age-encryption 生成，非手写；见 §3.2 实测形态）。 */
async function realRecipient(): Promise<string> {
  const { generateIdentity, identityToRecipient } = await import('age-encryption');
  return identityToRecipient(await generateIdentity());
}

function validConfig(): HomerConfig {
  return {
    version: 1,
    adapters: {
      pi: { root: '~/.pi/agent', categories: { settings: { paths: ['settings.json'], mode: 'merge' } } },
    },
  };
}

describe('M3 P0 · HomerPaths additive（§2.0-2）', () => {
  it('secretsDir = <home>/secrets，keysDir = <home>/keys', () => {
    const home = tmpHome();
    const paths = getHomerPaths({ HOMER_HOME: home });
    expect(paths.secretsDir).toBe(path.join(home, 'secrets'));
    expect(paths.keysDir).toBe(path.join(home, 'keys'));
  });

  it('additive：M1/M2 既有派生路径逐字不变', () => {
    const home = tmpHome();
    const paths = getHomerPaths({ HOMER_HOME: home });
    expect(paths.storeDir).toBe(path.join(home, 'store'));
    expect(paths.configFile).toBe(path.join(home, 'homer.json'));
    expect(paths.stateFile).toBe(path.join(home, 'state.json'));
    expect(paths.backupsDir).toBe(path.join(home, 'backups'));
  });

  it('默认 home（无 HOMER_HOME）下两新路径也在 ~/.homer 下', () => {
    const paths = getHomerPaths({});
    expect(paths.secretsDir).toBe(path.join(os.homedir(), '.homer', 'secrets'));
    expect(paths.keysDir).toBe(path.join(os.homedir(), '.homer', 'keys'));
  });
});

describe('M3 P0 · secretNameValid（§2.2）', () => {
  it('合法：字母/数字开头 + [A-Za-z0-9._-]', () => {
    for (const name of ['a', 'A1', 'openai', 'anthropic-api', 'github.token', 'a_b-c.d']) {
      expect(secretNameValid(name), `${name} 应合法`).toBe(true);
    }
  });

  it('非法：空 / 非字母数字开头 / 含路径分隔或空白', () => {
    for (const name of ['', '.', '..', '.hidden', '-x', '_x', 'a/b', 'a\\b', 'a b', 'a\n', '../x', 'a/b/c']) {
      expect(secretNameValid(name), `${name} 应非法`).toBe(false);
    }
  });

  it('扁平性：任何含 "/" 的名都不合法（防子目录逃逸，§1-D3）', () => {
    for (const name of ['sub/key', 'a/../b', '/abs', 'dir/']) {
      expect(secretNameValid(name)).toBe(false);
    }
  });
});

describe('M3 P0 · recipientIsValid（§2.2）', () => {
  it('真实的 age recipient（62 字符）合法', async () => {
    const recipient = await realRecipient();
    expect(recipient).toMatch(/^age1/);
    expect(recipientIsValid(recipient)).toBe(true);
  });

  it('非法：错误 HRP / 长度不对 / 含被排除的 bech32 字符（1/b/i/o）', () => {
    for (const bad of [
      '',
      'age1',
      'age1',
      'AGE1' + 'q'.repeat(58),
      'age1' + 'q'.repeat(57),
      'age1' + 'q'.repeat(59),
      'age1' + 'b'.repeat(58),
      'age1' + 'i'.repeat(58),
      'age1' + 'o'.repeat(58),
      'age1' + '1'.repeat(58),
      'age1' + 'A'.repeat(58),
      'age1' + 'q'.repeat(57) + '!',
    ]) {
      expect(recipientIsValid(bad), `${bad} 应非法`).toBe(false);
    }
  });

  it('大写 bech32 被拒（规范要求小写）', async () => {
    const recipient = await realRecipient();
    expect(recipientIsValid(recipient.toUpperCase())).toBe(false);
  });
});

describe('M3 P0 · createAgeCryptoPort（§2.2；P1-W1 已从 stub 换成真实现）', () => {
  // P0 时这里断言 stub 抛 CliError('尚未实现')。W1 按任务书替换了 stub（类型/签名未动），
  // 故本块的断言随之更新为「拿到真 port + types/cipher 两个入口是同一实现」。
  it('返回可用的 AgeCryptoPort（同一实现从 types 与 cipher 两个路径可见）', async () => {
    const port = createAgeCryptoPort();
    expect(typeof port.encrypt).toBe('function');
    expect(typeof port.decrypt).toBe('function');

    const { createAgeCryptoPort: fromCipher } = await import('../../src/core/age/cipher.js');
    expect(fromCipher).toBe(createAgeCryptoPort);
  });

  it('真实现可完成 roundtrip（防止 stub 被原样留下的回归）', async () => {
    const { generateIdentity } = await import('../../src/core/age/keys.js');
    const identity = generateIdentity();
    const port = createAgeCryptoPort();
    const ciphertext = await port.encrypt(Buffer.from('homer-m3-p0-port'), [identity.recipient]);
    expect(ciphertext.toString('utf8')).not.toContain('homer-m3-p0-port');
    await expect(port.decrypt(ciphertext, identity)).resolves.toEqual(Buffer.from('homer-m3-p0-port'));
  });
});

describe('M3 P0 · validateConfig additive（§2.0-1 / §2.0-3）', () => {
  it('整段缺省（无 secrets / 无 allowEscape）→ 合法', () => {
    expect(validateConfig(validConfig()).ok).toBe(true);
  });

  it('secrets.files 合法（name 过 secretNameValid + 值 ~ 或 / 开头）→ 原样返回', () => {
    const files = { openai: '~/.config/openai/key', github: '/etc/homer/gh.key' };
    const result = validateConfig({ ...validConfig(), secrets: { files } });
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.config.secrets?.files).toEqual(files);
  });

  it('secrets.files 的 name 非法 → 报错且信息含该键', () => {
    for (const name of ['../escape', '.hidden', 'sub/dir']) {
      const result = validateConfig({ ...validConfig(), secrets: { files: { [name]: '~/x' } } });
      expect(result.ok, `name=${name} 应报错`).toBe(false);
      if (result.ok) continue;
      expect(result.errors.join('\n')).toContain('secret 名');
    }
  });

  it('secrets.files 的值非绝对 / 非 ~ 开头 → 报错', () => {
    for (const destination of ['relative/key', 'x', '']) {
      const result = validateConfig({ ...validConfig(), secrets: { files: { a: destination } } });
      expect(result.ok, `destination=${destination} 应报错`).toBe(false);
      if (result.ok) continue;
      expect(result.errors.join('\n')).toMatch(/secrets\.files\.a/);
    }
  });

  it('secrets.files 非对象 → 报错', () => {
    for (const files of ['a', 1, ['x']]) {
      const result = validateConfig({ ...validConfig(), secrets: { files } });
      expect(result.ok).toBe(false);
      if (result.ok) continue;
      expect(result.errors.join('\n')).toMatch(/secrets\.files 必须是对象/);
    }
  });

  it('secrets.recipients 合法（真实 recipient）→ 原样返回', async () => {
    const recipients = [await realRecipient(), await realRecipient()];
    const result = validateConfig({ ...validConfig(), secrets: { recipients } });
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.config.secrets?.recipients).toEqual(recipients);
  });

  it('secrets.recipients 含非法 recipient → 报错且带上标', () => {
    const result = validateConfig({ ...validConfig(), secrets: { recipients: ['age1short'] } });
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors.join('\n')).toMatch(/secrets\.recipients\[0\]/);
  });

  it('secrets.recipients 非数组 / 含空串 → 报错', () => {
    for (const recipients of ['age1x', [42], ['']]) {
      const result = validateConfig({ ...validConfig(), secrets: { recipients } });
      expect(result.ok, JSON.stringify(recipients)).toBe(false);
    }
  });

  it('additive 不破坏 M2 的 ignorePaths 校验', () => {
    expect(validateConfig({ ...validConfig(), secrets: { ignorePaths: ['a/b'] } }).ok).toBe(true);
    expect(validateConfig({ ...validConfig(), secrets: { ignorePaths: [42] } }).ok).toBe(false);
  });

  it('adapters.*.allowEscape 字符串数组 → 合法且原样返回', () => {
    const config = validConfig();
    config.adapters['pi']!.allowEscape = ['skills/agent-browser', 'extensions/*'];
    const result = validateConfig(config);
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.config.adapters['pi']?.allowEscape).toEqual(['skills/agent-browser', 'extensions/*']);
  });

  it('adapters.*.allowEscape 非字符串数组 / 含非串 → 报错', () => {
    for (const allowEscape of ['x', [42], { 0: 'x' }]) {
      const config = validConfig();
      (config.adapters['pi'] as unknown as Record<string, unknown>)['allowEscape'] = allowEscape;
      const result = validateConfig(config);
      expect(result.ok, JSON.stringify(allowEscape)).toBe(false);
      if (result.ok) continue;
      expect(result.errors.join('\n')).toMatch(/allowEscape/);
    }
  });
});

describe('M3 P0 · COMMANDS / USAGE（§2.1）', () => {
  it('COMMANDS 含三新命令，既有命令保留', () => {
    for (const command of ['init', 'status', 'diff', 'push', 'pull', 'merge', 'home', 'doctor', 'secret', 'help']) {
      expect(COMMANDS).toContain(command);
    }
  });

  it('splitCommand 识别三新命令并切出 rest', () => {
    expect(splitCommand(['home', 'git@host:repo.git'])).toEqual({
      command: 'home',
      rest: ['git@host:repo.git'],
    });
    expect(splitCommand(['doctor', '--json'])).toEqual({ command: 'doctor', rest: ['--json'] });
    expect(splitCommand(['secret', 'keygen'])).toEqual({ command: 'secret', rest: ['keygen'] });
  });

  it('USAGE 列出三新命令 + 示例', () => {
    for (const token of ['home', 'doctor', 'secret']) {
      expect(USAGE).toContain(token);
    }
    expect(USAGE).toContain('homer home');
    expect(USAGE).toContain('homer doctor');
    expect(USAGE).toContain('homer secret keygen');
  });
});

describe('M3 P0 · secret 子命令族解析（§2.1 / §2.6）', () => {
  it('四个子命令都被识别，rest 正确切分', () => {
    expect(SECRET_SUBCOMMANDS).toEqual(['keygen', 'push', 'pull', 'list']);
    for (const subcommand of SECRET_SUBCOMMANDS) {
      const split = parseSecretSubcommand([subcommand, '--json']);
      expect(split).toEqual({ ok: true, subcommand, rest: ['--json'] });
    }
  });

  it('无子命令 → subcommand undefined（由分发层打印用法）', () => {
    expect(parseSecretSubcommand([])).toEqual({ ok: true, subcommand: undefined, rest: [] });
  });

  it('未知子命令 → ok:false 且带原名（分发层 usageError）', () => {
    expect(parseSecretSubcommand(['frobnicate'])).toEqual({ ok: false, unknown: 'frobnicate' });
  });

  it('SECRET_USAGE 含四个子命令与标志', () => {
    for (const subcommand of SECRET_SUBCOMMANDS) expect(SECRET_USAGE).toContain(subcommand);
    expect(SECRET_USAGE).toContain('--no-push');
  });
});

describe('M3 P0 · 分发层（§2.1 flag 表 + usage）', () => {
  function capture() {
    const out: string[] = [];
    const err: string[] = [];
    return { io: { out: (l: string) => out.push(l), err: (l: string) => err.push(l) }, out, err };
  }

  it('--help 含三新命令', async () => {
    const cap = capture();
    const code = await run(['--help'], cap.io);
    expect(code).toBe(0);
    const text = cap.out.join('\n');
    expect(text).toContain('home');
    expect(text).toContain('doctor');
    expect(text).toContain('secret');
  });

  it('homer secret（无子命令）→ 打印 usage，exit 1', async () => {
    const cap = capture();
    const code = await run(['secret'], cap.io);
    expect(code).toBe(1);
    expect(cap.err.join('\n')).toContain('用法: homer secret');
    expect(cap.err.join('\n')).toContain('缺少子命令');
  });

  it('homer secret --help → usage，exit 0', async () => {
    const cap = capture();
    const code = await run(['secret', '--help'], cap.io);
    expect(code).toBe(0);
    expect(cap.out.join('\n')).toContain('用法: homer secret');
  });

  it('homer secret 未知子命令 → usageError，exit 1', async () => {
    const cap = capture();
    const code = await run(['secret', 'frobnicate'], cap.io);
    expect(code).toBe(1);
    expect(cap.err.join('\n')).toContain('未知子命令');
    expect(cap.err.join('\n')).toContain('用法: homer secret');
  });

  it('各子命令 --help → 各自 usage，exit 0', async () => {
    for (const subcommand of SECRET_SUBCOMMANDS) {
      const cap = capture();
      const code = await run(['secret', subcommand, '--help'], cap.io);
      expect(code, `secret ${subcommand} --help`).toBe(0);
      expect(cap.out.join('\n')).toContain('用法: homer secret');
    }
  });

  it('home --help → HOME_USAGE，exit 0；doctor --help → DOCTOR_USAGE，exit 0', async () => {
    const homeCap = capture();
    expect(await run(['home', '--help'], homeCap.io)).toBe(0);
    expect(homeCap.out.join('\n')).toContain('用法: homer home');

    const doctorCap = capture();
    expect(await run(['doctor', '--help'], doctorCap.io)).toBe(0);
    expect(doctorCap.out.join('\n')).toContain('用法: homer doctor');
  });

  it('home 缺 <repo-url> → usageError，exit 1', async () => {
    const cap = capture();
    const code = await run(['home'], cap.io);
    expect(code).toBe(1);
    expect(cap.err.join('\n')).toContain('缺少 <repo-url>');
  });

  it('home 多余位置参数 → usageError，exit 1', async () => {
    const cap = capture();
    const code = await run(['home', 'url-a', 'url-b'], cap.io);
    expect(code).toBe(1);
    expect(cap.err.join('\n')).toContain('多余的参数');
  });

  it('home --mode 非法值 → usageError，exit 1', async () => {
    const cap = capture();
    const code = await run(['home', 'git@host:repo.git', '--mode', 'frobnicate'], cap.io);
    expect(code).toBe(1);
    expect(cap.err.join('\n')).toContain('--mode 只能是 pull / merge / skip');
  });

  it('未知选项仍被 strict 拦下（三新命令同样）', async () => {
    for (const argv of [
      ['doctor', '--frobnicate'],
      ['secret', 'keygen', '--frobnicate'],
      ['secret', 'push', '--frobnicate'],
      ['secret', 'pull', '--frobnicate'],
      ['secret', 'list', '--frobnicate'],
      ['home', 'url', '--frobnicate'],
    ]) {
      const cap = capture();
      expect(await run(argv, cap.io), argv.join(' ')).toBe(1);
    }
  });

  it('home 的 flag 表：--yes / --json / --mode 都被接受（实现是 stub → 报"尚未实现"）', async () => {
    const cap = capture();
    const code = await run(
      ['home', 'git@host:repo.git', '--home', tmpHome(), '--mode', 'merge', '--yes', '--json'],
      cap.io,
    );
    // stub 抛 CliError('尚未实现') → 分发层捕获，exit 1（参数解析本身已通过）。
    expect(code).toBe(1);
    expect(cap.err.join('\n')).toContain('尚未实现');
  });

  it('HOME_USAGE / DOCTOR_USAGE / SECRET_USAGE 均为非空用法文本', () => {
    for (const usage of [HOME_USAGE, DOCTOR_USAGE, SECRET_USAGE]) {
      expect(usage.startsWith('用法:')).toBe(true);
    }
  });
});
