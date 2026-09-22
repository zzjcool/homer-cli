#!/usr/bin/env node
import process from 'node:process';
import { USAGE, splitCommand, type Command } from './args.js';

export interface CliIO {
  out: (line: string) => void;
  err: (line: string) => void;
}

const defaultIO: CliIO = {
  out: (line) => process.stdout.write(`${line}\n`),
  err: (line) => process.stderr.write(`${line}\n`),
};

/**
 * P0 占位实现：真正的 init/status/diff 由 P1 的 M4-cli worker 接入。
 * 未知命令给出友好报错；--help / 无参数打印用法。
 */
export function run(argv: readonly string[], io: CliIO = defaultIO): number {
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

  return dispatchPlaceholder(command, rest, io);
}

function dispatchPlaceholder(command: Command, rest: readonly string[], io: CliIO): number {
  const flags = rest.length > 0 ? ` ${rest.join(' ')}` : '';
  io.err(`homer ${command}${flags}: 尚未实现（P0 scaffold 占位）`);
  io.err('该命令将在 M1 的 P1 阶段（M4-cli）接入真实实现。');
  return 1;
}

async function main(): Promise<void> {
  const code = run(process.argv.slice(2));
  process.exitCode = code;
}

// bin/homer.js 以 `import('../dist/cli/index.js')` 薄壳形式加载本模块，
// 因此入口副作用必须发生在 import 时。测试导入 run() 时跳过（vitest 会设 VITEST）。
if (!process.env.VITEST) {
  await main();
}
