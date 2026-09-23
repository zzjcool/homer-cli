/**
 * 交互提示端口（docs/m2-plan.md §2.7，**冻结**）。
 *
 * P0 只落地 `PromptPort` 类型本身：§2.8 的三个命令文件（push/pull/merge）的 Deps
 * 都引用它，P0 验收要求 `npm run typecheck` 全绿，故类型必须先存在。
 * 三个工厂函数（createClackPromptPort / createNonInteractivePromptPort / createDefaultPromptPort，
 * @clack/prompts 唯一接入点）由 **P1-W5** 在本文件内补齐 —— 本文件归 W5 所有。
 *
 * 命令层约定（§2.7）：`--yes` 时完全不创建 port；无 `--yes` 且非 TTY → port 返回 fallback。
 */
export interface PromptPort {
  confirm(message: string, fallback: boolean): Promise<boolean>;
  select<T extends string>(message: string, options: readonly { value: T; label: string }[], fallback: T): Promise<T>;
}
