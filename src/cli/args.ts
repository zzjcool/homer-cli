import { parseArgs } from 'node:util';

/** M1 支持的命令（P1 由 M4-cli worker 接入真实实现）。 */
export const COMMANDS = ['init', 'status', 'diff', 'help'] as const;
export type Command = (typeof COMMANDS)[number];

export interface ParsedArgs {
  command: Command | undefined;
  /** 命令名之后的子参数（已剥离命令位）。 */
  rest: string[];
}

/**
 * 从 argv 中取出命令名与剩余参数。
 * 只做位置参数切分；选项解析交给各命令自己的 parseArgs 调用。
 */
export function splitCommand(argv: readonly string[]): ParsedArgs {
  const [first, ...rest] = argv;
  if (first === undefined) return { command: undefined, rest: [] };
  if (first === '--help' || first === '-h') return { command: 'help', rest };
  const command = (COMMANDS as readonly string[]).includes(first)
    ? (first as Command)
    : undefined;
  return command === undefined
    ? { command: undefined, rest: [...argv] }
    : { command, rest };
}

export const USAGE = `homer — dotfiles for humans and their AI agents

用法:
  homer <command> [options]

命令:
  init      扫描 adapter 并生成 homer.json + store 快照
  status    显示本地/仓库之间的漂移概览
  diff      显示漂移的详细差异

全局选项:
  -h, --help    显示本帮助

示例:
  homer init
  homer status --json
  homer diff --category settings

提示: 直接运行 TypeScript 源码可用 \`npx tsx src/cli/index.ts <command>\`。
`;

/** 复用 node:util parseArgs，保持 CLI 约定「零运行时依赖」。 */
export { parseArgs };
