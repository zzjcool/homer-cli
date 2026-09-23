/**
 * 可预期的用户级错误（配置缺失 / 拒绝覆盖 / store 不完整等）。
 *
 * 放在 core/ 而非 cli/render.ts，是为了让 core 模块（store.ts）也能抛同一种错误，
 * 而不产生 `cli/render → core/store → cli/render` 的循环依赖。
 * `src/cli/render.ts` 原样 re-export 本类型，保持既有 import 路径可用。
 *
 * 分发层（src/cli/index.ts）捕获后打印 `message`（+ 可选 `hint`）并 exit 1。
 */
export class CliError extends Error {
  readonly hint: string | undefined;

  constructor(message: string, hint?: string) {
    super(message);
    this.name = 'CliError';
    this.hint = hint;
  }
}

/**
 * `CliError` → 报告 `errors` 数组（`message` + 可选 `hint`）。
 *
 * 唯一实现点：`home.ts` 与 `secret.ts` 曾各自持有一份逐字相同的私有 `errorLines`
 * （对抗式 review minor 4 列出的重复块之一）。放在与 `CliError` 同层，两个命令层
 * 各自 `import` 同一个函数，不再是副本。
 */
export function cliErrorLines(err: CliError): string[] {
  return err.hint === undefined ? [err.message] : [err.message, err.hint];
}
