/**
 * 交互提示端口（docs/m2-plan.md §2.7）。
 *
 * 本文件是 **@clack/prompts 的唯一接入点**：命令层（§2.8 的 push/pull/merge）只依赖
 * `PromptPort`，不直接 import 任何提示库；测试通过 `Deps.ui` 注入替身。
 *
 * 三个工厂函数：
 *   - `createClackPromptPort`          —— 真实 TTY 下的交互实现（intro/outro/confirm/select）
 *   - `createNonInteractivePromptPort` —— 无 TTY / CI / 测试：永不阻塞，永远返回 fallback
 *   - `createDefaultPromptPort`        —— `process.stdout.isTTY ? clack : non-interactive`
 *
 * 命令层约定（§2.7 末段，命令层实现时必须遵守）：
 *   1. `--yes` 时**完全不创建 port**（连 `createDefaultPromptPort()` 都不调用）：
 *      不提示，直接走默认策略（默认策略 = 该操作「接受」）。
 *   2. 无 `--yes` 且非 TTY → port 返回 fallback。此时 `confirm` 的 fallback 必须是 `false`，
 *      命令层拿到 `false` **必须 abort**，并提示「非交互环境，请加 --yes」
 *      （非交互下不能默认同意破坏性操作，只能明确拒绝并要求显式 `--yes`）。
 *   3. 无 `--yes` 且 TTY → 用 clack port 真正提问。
 *
 * 取消语义：clack 的 `confirm` / `select` 被 Ctrl-C / Esc 取消时返回 `isCancel` 哨兵
 * 而非抛异常。端口把哨兵统一收敛为「拒绝/不确认」：`confirm → false`、`select → fallback`，
 * 命令层无需 import clack 也能正确处理取消。
 */

import process from 'node:process';

import { confirm, intro, isCancel, outro, select, type Option } from '@clack/prompts';

export interface PromptPort {
  confirm(message: string, fallback: boolean): Promise<boolean>;
  select<T extends string>(message: string, options: readonly { value: T; label: string }[], fallback: T): Promise<T>;
}

/**
 * 真实 TTY 下的 clack 实现。
 *
 * frame 用 `intro` 懒开一次（一个 port 实例 = 一次交互会话，避免每个问题都画一遍框），
 * 取消时用 `outro` 收尾 —— 问题文本只在 clack 的 prompt 里出现一次，不重复打印。
 */
export function createClackPromptPort(): PromptPort {
  let framed = false;
  const openFrame = (): void => {
    if (!framed) {
      intro('homer');
      framed = true;
    }
  };

  return {
    async confirm(message: string, fallback: boolean): Promise<boolean> {
      openFrame();
      const answer = await confirm({ message, initialValue: fallback });
      // 取消（Ctrl-C / Esc）视为拒绝 —— 绝不把「用户想中断」升级成「用户同意」。
      if (isCancel(answer)) {
        outro('已取消');
        return false;
      }
      return answer;
    },

    async select<T extends string>(
      message: string,
      options: readonly { value: T; label: string }[],
      fallback: T,
    ): Promise<T> {
      openFrame();
      const items: { value: T; label: string }[] = options.map((option) => ({
        value: option.value,
        label: option.label,
      }));
      // clack 的 `Option<Value>` 是对泛型求值的条件类型（Primitive 下 label 变可选），
      // 泛型函数体内无法直接构造；运行时形状一致（{ value, label }），故此处仅做断言。
      const answer = await select<T>({
        message,
        options: items as Option<T>[],
        initialValue: fallback,
      });
      // 取消 → fallback（命令层据此「保持现状」）。
      if (isCancel(answer)) {
        outro('已取消');
        return fallback;
      }
      return answer;
    },
  };
}

/**
 * 非交互实现：永远立即返回 fallback，不读 stdin、不阻塞。
 * 无状态，故为**单例**（`createDefaultPromptPort()` 在非 TTY 下 `toBe` 同一实例，
 * 便于命令层与测试做身份断言）。
 *
 * 边界行为（W5 验收明确钉住）：
 *   - `confirm` 的 fallback 为 `undefined`（TS 里不允许，JS 调用方 / `?? undefined` 可能带来）
 *     → 返回 `false`（安全默认：拒绝）。
 *   - `select` 的 fallback 为 `undefined` → 返回 `options[0].value`；选项列表为空 → `throw`
 *     （编程错误：没有任何可选项就不该发问）。fallback 合法时即使 options 为空也照常返回 fallback。
 */
const nonInteractivePort: PromptPort = {
  async confirm(_message: string, fallback: boolean): Promise<boolean> {
    return fallback ?? false;
  },

  async select<T extends string>(
    _message: string,
    options: readonly { value: T; label: string }[],
    fallback: T,
  ): Promise<T> {
    if (fallback !== undefined) return fallback;
    const first = options[0];
    if (first === undefined) {
      throw new Error('非交互环境 select：既无 fallback 也无可选项');
    }
    return first.value;
  },
};

export function createNonInteractivePromptPort(): PromptPort {
  return nonInteractivePort;
}

/**
 * 默认实现：TTY（真实终端）→ clack；否则（CI / 管道 / 重定向 / 测试进程）→ 非交互。
 * `--yes` 的命令路径不应调用本函数（§2.7 约定 1：完全不创建 port）。
 */
export function createDefaultPromptPort(): PromptPort {
  return process.stdout.isTTY === true ? createClackPromptPort() : nonInteractivePort;
}
