#!/usr/bin/env node
import process from 'node:process';
import { parseArgs } from 'node:util';

import { USAGE, splitCommand, type Command } from './args.js';
import { CliError, renderInit, renderStatus } from './render.js';
import { INIT_USAGE, runInit } from './commands/init.js';
import { STATUS_USAGE, runStatus } from './commands/status.js';
import { DIFF_USAGE, runDiff } from './commands/diff.js';
import { PUSH_USAGE, renderPushReport, runPush } from './commands/push.js';
import { PULL_USAGE, renderPullReport, runPull } from './commands/pull.js';
import { MERGE_USAGE, renderMergeReport, runMerge } from './commands/merge.js';
import { HOME_USAGE, renderHomeReport, runHome } from './commands/home.js';
import { DOCTOR_USAGE, renderDoctorReport, runDoctor } from './commands/doctor.js';
import {
  SECRET_USAGE,
  parseSecretSubcommand,
  renderSecretKeygenReport,
  renderSecretListReport,
  renderSecretPullReport,
  renderSecretPushReport,
  runSecretKeygen,
  runSecretList,
  runSecretPull,
  runSecretPush,
} from './commands/secret.js';

export interface CliIO {
  out: (line: string) => void;
  err: (line: string) => void;
}

const defaultIO: CliIO = {
  out: (line) => process.stdout.write(`${line}\n`),
  err: (line) => process.stderr.write(`${line}\n`),
};

/** 所有命令共享的选项：`--home` + `--help`。 */
const COMMON_OPTIONS = {
  home: { type: 'string' },
  help: { type: 'boolean', short: 'h' },
} as const;

type ParseOutcome<T> = { ok: true; value: T } | { ok: false; message: string };

function safeParse<T>(fn: () => T): ParseOutcome<T> {
  try {
    return { ok: true, value: fn() };
  } catch (err) {
    return { ok: false, message: err instanceof Error ? err.message : String(err) };
  }
}

function usageError(command: Command, message: string, usage: string, io: CliIO): 1 {
  io.err(`homer ${command}: ${message}`);
  io.err('');
  io.err(usage);
  return 1;
}

/**
 * 命令分发。参数解析统一走 `node:util` 的 `parseArgs`（零运行时依赖，§1.7 冻结约定），
 * strict 模式：未知选项 / 位置参数一律报错并打印用法。
 *
 * 退出码约定（§1.7）：
 *   0  成功（含「有漂移」——漂移是信息不是错误；diff 无漂移时输出为空，也 exit 0）
 *   1  CLI 用法错误 / 未知选项 / 真错误（无 homer.json、拒绝覆盖已存在的 homer.json）
 */
export async function run(argv: readonly string[], io: CliIO = defaultIO): Promise<number> {
  const { command, rest } = splitCommand(argv);

  if (command === 'help') {
    io.out(USAGE);
    return 0;
  }

  if (command === undefined) {
    if (rest.length > 0) {
      io.err(`未知命令: ${rest[0] ?? ''}`);
      io.err('');
    }
    io.err(USAGE);
    return 1;
  }

  try {
    return await dispatch(command, rest, io);
  } catch (err) {
    if (err instanceof CliError) {
      io.err(err.message);
      if (err.hint !== undefined) io.err(err.hint);
      return 1;
    }
    io.err(`homer ${command}: ${err instanceof Error ? err.message : String(err)}`);
    return 1;
  }
}

async function dispatch(command: Command, rest: readonly string[], io: CliIO): Promise<number> {
  switch (command) {
    case 'init':
      return await dispatchInit(rest, io);
    case 'status':
      return dispatchStatus(rest, io);
    case 'diff':
      return dispatchDiff(rest, io);
    case 'push':
      return await dispatchPush(rest, io);
    case 'pull':
      return await dispatchPull(rest, io);
    case 'merge':
      return await dispatchMerge(rest, io);
    case 'home':
      return await dispatchHome(rest, io);
    case 'doctor':
      return await dispatchDoctor(rest, io);
    case 'secret':
      return await dispatchSecret(rest, io);
    case 'help':
      io.out(USAGE);
      return 0;
  }
}

function dispatchStatus(argv: readonly string[], io: CliIO): number {
  const parsed = safeParse(() =>
    parseArgs({
      args: [...argv],
      options: { ...COMMON_OPTIONS, json: { type: 'boolean' }, verbose: { type: 'boolean', short: 'v' } },
      allowPositionals: false,
      strict: true,
    }),
  );
  if (!parsed.ok) return usageError('status', parsed.message, STATUS_USAGE, io);

  const { home, help, json, verbose } = parsed.value.values;
  if (help === true) {
    io.out(STATUS_USAGE);
    return 0;
  }

  const report = runStatus({ homerHome: home, json, verbose });
  if (json === true) io.out(JSON.stringify(report, null, 2));
  else io.out(renderStatus(report, { verbose }));
  // 有漂移也 exit 0：漂移是信息不是错误。
  return 0;
}

function dispatchDiff(argv: readonly string[], io: CliIO): number {
  const parsed = safeParse(() =>
    parseArgs({
      args: [...argv],
      options: { ...COMMON_OPTIONS, adapter: { type: 'string' }, category: { type: 'string' } },
      allowPositionals: false,
      strict: true,
    }),
  );
  if (!parsed.ok) return usageError('diff', parsed.message, DIFF_USAGE, io);

  const { home, help, adapter, category } = parsed.value.values;
  if (help === true) {
    io.out(DIFF_USAGE);
    return 0;
  }

  const text = runDiff({ homerHome: home, adapter, category });
  // 无漂移 → 空输出（不打印占位符，保证脚本可判空）。
  if (text !== '') io.out(text);
  return 0;
}

async function dispatchInit(argv: readonly string[], io: CliIO): Promise<number> {
  const parsed = safeParse(() =>
    parseArgs({
      args: [...argv],
      options: {
        ...COMMON_OPTIONS,
        adapters: { type: 'string', multiple: true },
        force: { type: 'boolean' },
        json: { type: 'boolean' },
      },
      allowPositionals: false,
      strict: true,
    }),
  );
  if (!parsed.ok) return usageError('init', parsed.message, INIT_USAGE, io);

  const { home, help, adapters: rawAdapters, force, json } = parsed.value.values;
  if (help === true) {
    io.out(INIT_USAGE);
    return 0;
  }

  const adapters =
    rawAdapters === undefined
      ? undefined
      : rawAdapters
          .flatMap((item) => item.split(','))
          .map((item) => item.trim())
          .filter((item) => item !== '');

  const report = await runInit({ homerHome: home, adapters, json }, { force: force === true });

  if (json === true) io.out(JSON.stringify(report, null, 2));
  else io.out(renderInit(report));
  return 0;
}

async function dispatchPush(argv: readonly string[], io: CliIO): Promise<number> {
  const parsed = safeParse(() =>
    parseArgs({
      args: [...argv],
      options: {
        ...COMMON_OPTIONS,
        json: { type: 'boolean' },
        yes: { type: 'boolean' },
        'no-push': { type: 'boolean' },
      },
      allowPositionals: false,
      strict: true,
    }),
  );
  if (!parsed.ok) return usageError('push', parsed.message, PUSH_USAGE, io);

  const { home, help, json, yes, 'no-push': noPush } = parsed.value.values;
  if (help === true) {
    io.out(PUSH_USAGE);
    return 0;
  }

  const report = await runPush({ homerHome: home, json, yes, noPush });
  if (json === true) io.out(JSON.stringify(report, null, 2));
  else io.out(renderPushReport(report));
  return report.status === 'pushed' || report.status === 'no-drift' ? 0 : 1;
}

async function dispatchPull(argv: readonly string[], io: CliIO): Promise<number> {
  const parsed = safeParse(() =>
    parseArgs({
      args: [...argv],
      options: { ...COMMON_OPTIONS, json: { type: 'boolean' }, yes: { type: 'boolean' } },
      allowPositionals: false,
      strict: true,
    }),
  );
  if (!parsed.ok) return usageError('pull', parsed.message, PULL_USAGE, io);

  const { home, help, json, yes } = parsed.value.values;
  if (help === true) {
    io.out(PULL_USAGE);
    return 0;
  }

  const report = await runPull({ homerHome: home, json, yes });
  if (json === true) io.out(JSON.stringify(report, null, 2));
  else io.out(renderPullReport(report));
  return report.status === 'applied' || report.status === 'no-drift' ? 0 : 1;
}

async function dispatchMerge(argv: readonly string[], io: CliIO): Promise<number> {
  const parsed = safeParse(() =>
    parseArgs({
      args: [...argv],
      options: {
        ...COMMON_OPTIONS,
        json: { type: 'boolean' },
        'accept-local': { type: 'boolean' },
        'accept-remote': { type: 'boolean' },
      },
      allowPositionals: false,
      strict: true,
    }),
  );
  if (!parsed.ok) return usageError('merge', parsed.message, MERGE_USAGE, io);

  const { home, help, json, 'accept-local': acceptLocal, 'accept-remote': acceptRemote } = parsed.value.values;
  if (help === true) {
    io.out(MERGE_USAGE);
    return 0;
  }

  const report = await runMerge({ homerHome: home, json, acceptLocal, acceptRemote });
  if (json === true) io.out(JSON.stringify(report, null, 2));
  else io.out(renderMergeReport(report));
  return report.status === 'resolved' || report.status === 'no-conflicts' ? 0 : 1;
}

async function main(): Promise<void> {
  const code = await run(process.argv.slice(2));
  process.exitCode = code;
}

// ---- M3 新命令（docs/m3-plan.md §2.1；此后 P1-P4 禁改本文件）----

/** 首次对接模式取值（`--mode` 校验用；与 W5 的 `FirstContactMode` 同集合）。 */
const FIRST_CONTACT_MODES = ['pull', 'merge', 'skip'] as const;

type FirstContactModeArg = (typeof FIRST_CONTACT_MODES)[number];

function isFirstContactMode(value: string): value is FirstContactModeArg {
  return (FIRST_CONTACT_MODES as readonly string[]).includes(value);
}

/**
 * `homer home <repo-url>`（§2.1 flag 表：`--home` `--mode pull|merge|skip` `--yes` `--json` `-h`）。
 *
 * `repo-url` 是**位置参数**（必填），故这里 `allowPositionals: true`——其余命令均不允许位置参数。
 * 缺少或多余的位置参数 → 用法错误（与 strict 模式对选项的处理一致，不静默忽略）。
 */
async function dispatchHome(argv: readonly string[], io: CliIO): Promise<number> {
  const parsed = safeParse(() =>
    parseArgs({
      args: [...argv],
      options: {
        ...COMMON_OPTIONS,
        mode: { type: 'string' },
        yes: { type: 'boolean' },
        json: { type: 'boolean' },
      },
      allowPositionals: true,
      strict: true,
    }),
  );
  if (!parsed.ok) return usageError('home', parsed.message, HOME_USAGE, io);

  const { home, help, mode, yes, json } = parsed.value.values;
  if (help === true) {
    io.out(HOME_USAGE);
    return 0;
  }

  if (parsed.value.positionals.length !== 1) {
    return usageError(
      'home',
      parsed.value.positionals.length === 0
        ? '缺少 <repo-url> 位置参数'
        : `多余的参数: ${parsed.value.positionals.slice(1).join(' ')}`,
      HOME_USAGE,
      io,
    );
  }
  const repoUrl = parsed.value.positionals[0] as string;

  if (mode !== undefined && !isFirstContactMode(mode)) {
    return usageError(
      'home',
      `--mode 只能是 pull / merge / skip（当前: ${mode}）`,
      HOME_USAGE,
      io,
    );
  }

  const report = await runHome({
    homerHome: home,
    repoUrl,
    json,
    yes,
    mode: mode === undefined ? undefined : (mode as FirstContactModeArg),
  });
  if (json === true) io.out(JSON.stringify(report, null, 2));
  else io.out(renderHomeReport(report));
  return report.status === 'homed' ? 0 : 1;
}

/**
 * `homer doctor`（§2.1 flag 表：`--home` `--offline` `--json` `-h`）。
 *
 * 退出码（§2.5 / D6）：无 `fail` → 0（**含仅 warn**）；有 fail → 1。
 */
async function dispatchDoctor(argv: readonly string[], io: CliIO): Promise<number> {
  const parsed = safeParse(() =>
    parseArgs({
      args: [...argv],
      options: { ...COMMON_OPTIONS, offline: { type: 'boolean' }, json: { type: 'boolean' } },
      allowPositionals: false,
      strict: true,
    }),
  );
  if (!parsed.ok) return usageError('doctor', parsed.message, DOCTOR_USAGE, io);

  const { home, help, offline, json } = parsed.value.values;
  if (help === true) {
    io.out(DOCTOR_USAGE);
    return 0;
  }

  const report = await runDoctor({ homerHome: home, json, offline });
  if (json === true) io.out(JSON.stringify(report, null, 2));
  else io.out(renderDoctorReport(report));
  return report.ok ? 0 : 1;
}

/**
 * `homer secret <keygen|push|pull|list>`（§2.1 / §2.6）。
 *
 * 子命令切分在 `secret.ts` 内（`parseSecretSubcommand`），本函数只按子命令解析各自的
 * flag 表（§2.1），使「子命令集合」的真相只有一份。
 *
 * `homer secret`（无子命令）→ 打印用法 + exit 1（缺必需子命令，同 `homer` 无参数的口径）；
 * `homer secret --help` → 用法 + exit 0；未知子命令 → usageError + exit 1。
 */
async function dispatchSecret(argv: readonly string[], io: CliIO): Promise<number> {
  const first = argv[0];
  if (first === undefined) {
    io.err('homer secret: 缺少子命令（keygen | push | pull | list）');
    io.err('');
    io.err(SECRET_USAGE);
    return 1;
  }
  if (first === '-h' || first === '--help') {
    io.out(SECRET_USAGE);
    return 0;
  }

  const split = parseSecretSubcommand(argv);
  if (!split.ok) return usageError('secret', `未知子命令: ${split.unknown}`, SECRET_USAGE, io);
  const sub = split.subcommand;
  if (sub === undefined) {
    io.err(SECRET_USAGE);
    return 1;
  }

  switch (sub) {
    case 'keygen':
      return await dispatchSecretKeygen(split.rest, io);
    case 'push':
      return await dispatchSecretPush(split.rest, io);
    case 'pull':
      return await dispatchSecretPull(split.rest, io);
    case 'list':
      return dispatchSecretList(split.rest, io);
  }
}

/** `--home` `--json` `-h`。 */
async function dispatchSecretKeygen(argv: readonly string[], io: CliIO): Promise<number> {
  const parsed = safeParse(() =>
    parseArgs({
      args: [...argv],
      options: { ...COMMON_OPTIONS, json: { type: 'boolean' } },
      allowPositionals: false,
      strict: true,
    }),
  );
  if (!parsed.ok) return usageError('secret', parsed.message, SECRET_USAGE, io);

  const { home, help, json } = parsed.value.values;
  if (help === true) {
    io.out(SECRET_USAGE);
    return 0;
  }

  const report = await runSecretKeygen({ homerHome: home, json });
  if (json === true) io.out(JSON.stringify(report, null, 2));
  else io.out(renderSecretKeygenReport(report));
  // §2.6：成功 → 0；identity 已存在 → 1（实现抛 CliError）。
  return report.ok ? 0 : 1;
}

/** `--home` `--yes` `--no-push` `--json` `-h`。 */
async function dispatchSecretPush(argv: readonly string[], io: CliIO): Promise<number> {
  const parsed = safeParse(() =>
    parseArgs({
      args: [...argv],
      options: {
        ...COMMON_OPTIONS,
        json: { type: 'boolean' },
        yes: { type: 'boolean' },
        'no-push': { type: 'boolean' },
      },
      allowPositionals: false,
      strict: true,
    }),
  );
  if (!parsed.ok) return usageError('secret', parsed.message, SECRET_USAGE, io);

  const { home, help, json, yes, 'no-push': noPush } = parsed.value.values;
  if (help === true) {
    io.out(SECRET_USAGE);
    return 0;
  }

  const report = await runSecretPush({ homerHome: home, json, yes, noPush });
  if (json === true) io.out(JSON.stringify(report, null, 2));
  else io.out(renderSecretPushReport(report));
  // §2.6：pushed / no-secrets → 0；其余 → 1。
  return report.status === 'pushed' || report.status === 'no-secrets' ? 0 : 1;
}

/** `--home` `--yes` `--json` `-h`。 */
async function dispatchSecretPull(argv: readonly string[], io: CliIO): Promise<number> {
  const parsed = safeParse(() =>
    parseArgs({
      args: [...argv],
      options: { ...COMMON_OPTIONS, json: { type: 'boolean' }, yes: { type: 'boolean' } },
      allowPositionals: false,
      strict: true,
    }),
  );
  if (!parsed.ok) return usageError('secret', parsed.message, SECRET_USAGE, io);

  const { home, help, json, yes } = parsed.value.values;
  if (help === true) {
    io.out(SECRET_USAGE);
    return 0;
  }

  const report = await runSecretPull({ homerHome: home, json, yes });
  if (json === true) io.out(JSON.stringify(report, null, 2));
  else io.out(renderSecretPullReport(report));
  // §2.6：applied / no-secrets → 0；其余 → 1。
  return report.status === 'applied' || report.status === 'no-secrets' ? 0 : 1;
}

/** `--home` `--json` `-h`。list 恒 exit 0（§2.6）。 */
function dispatchSecretList(argv: readonly string[], io: CliIO): number {
  const parsed = safeParse(() =>
    parseArgs({
      args: [...argv],
      options: { ...COMMON_OPTIONS, json: { type: 'boolean' } },
      allowPositionals: false,
      strict: true,
    }),
  );
  if (!parsed.ok) return usageError('secret', parsed.message, SECRET_USAGE, io);

  const { home, help, json } = parsed.value.values;
  if (help === true) {
    io.out(SECRET_USAGE);
    return 0;
  }

  const report = runSecretList({ homerHome: home, json });
  if (json === true) io.out(JSON.stringify(report, null, 2));
  else io.out(renderSecretListReport(report));
  return 0;
}
// bin/homer.js 以 `import('../dist/cli/index.js')` 薄壳形式加载本模块，
// 因此入口副作用必须发生在 import 时。测试导入 run() 时跳过（vitest 会设 VITEST）。
if (!process.env.VITEST) {
  await main();
}
