/**
 * ui.ts（§2.7 prompt port）测试 —— P1-W5。
 *
 * 覆盖范围与验收一致：
 *   1. non-interactive port：confirm/select 永远返回 fallback（含 fallback 缺失 / 选项为空的边界）；
 *      不阻塞、不写 stdout。
 *   2. clack port：仅 import smoke + 类型契约（**不模拟 TTY、不真的发问**）。
 *   3. createDefaultPromptPort：在非 TTY 的测试进程内返回 non-interactive 实例。
 */

import process from 'node:process';

import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  createClackPromptPort,
  createDefaultPromptPort,
  createNonInteractivePromptPort,
  type PromptPort,
} from '../../src/cli/ui.js';

/** 断言 promise 在 ms 内 settle；否则视为「阻塞」。 */
async function settlesWithin<T>(promise: Promise<T>, ms = 200): Promise<T> {
  return Promise.race([
    promise,
    new Promise<never>((_, reject) => {
      const timer = setTimeout(() => reject(new Error(`promise 未在 ${ms}ms 内 settle（疑似阻塞在 stdin）`)), ms);
      timer.unref?.();
    }),
  ]);
}

const OPTIONS = [
  { value: 'local', label: '保留本地' },
  { value: 'remote', label: '采用远端' },
  { value: 'skip', label: '跳过' },
] as const;

describe('createNonInteractivePromptPort', () => {
  it('confirm 原样返回 fallback（true / false）', async () => {
    const ui = createNonInteractivePromptPort();
    expect(await settlesWithin(ui.confirm('覆盖本地文件？', true))).toBe(true);
    expect(await settlesWithin(ui.confirm('覆盖本地文件？', false))).toBe(false);
  });

  it('fallback 缺失（undefined）时 confirm 返回安全默认 false', async () => {
    const ui = createNonInteractivePromptPort();
    // TS 类型禁止 undefined，这里模拟 JS 调用方 / `?? undefined` 泄漏进来的情况。
    const missing = undefined as unknown as boolean;
    expect(await settlesWithin(ui.confirm('危险操作？', missing))).toBe(false);
  });

  it('select 原样返回 fallback', async () => {
    const ui = createNonInteractivePromptPort();
    expect(await settlesWithin(ui.select('冲突如何裁决？', OPTIONS, 'remote'))).toBe('remote');
    expect(await settlesWithin(ui.select('冲突如何裁决？', OPTIONS, 'skip'))).toBe('skip');
  });

  it('fallback 合法时，即使选项中不含该值也照常返回 fallback', async () => {
    const ui = createNonInteractivePromptPort();
    expect(await settlesWithin(ui.select('冲突如何裁决？', [OPTIONS[0]], 'skip'))).toBe('skip');
    expect(await settlesWithin(ui.select('冲突如何裁决？', [], 'skip'))).toBe('skip');
  });

  it('fallback 缺失（undefined）时 select 返回第一个选项的 value', async () => {
    const ui = createNonInteractivePromptPort();
    const missing = undefined as unknown as (typeof OPTIONS)[number]['value'];
    expect(await settlesWithin(ui.select('冲突如何裁决？', OPTIONS, missing))).toBe('local');
    // 单选项同样成立：返回该项而非硬编码值。
    expect(await settlesWithin(ui.select('冲突如何裁决？', [OPTIONS[2]], missing))).toBe('skip');
  });

  it('fallback 缺失且没有任何选项 → throw（编程错误，不静默吞掉）', async () => {
    const ui = createNonInteractivePromptPort();
    const missing = undefined as unknown as (typeof OPTIONS)[number]['value'];
    await expect(ui.select('冲突如何裁决？', [], missing)).rejects.toThrow(/既无 fallback 也无可选项/);
  });

  it('完全不写 stdout / stderr（非交互下静默，输出交给命令层）', async () => {
    const out = vi.spyOn(process.stdout, 'write').mockImplementation(() => true);
    const err = vi.spyOn(process.stderr, 'write').mockImplementation(() => true);
    try {
      const ui = createNonInteractivePromptPort();
      await ui.confirm('继续？', false);
      await ui.select('冲突如何裁决？', OPTIONS, 'local');
      expect(out).not.toHaveBeenCalled();
      expect(err).not.toHaveBeenCalled();
    } finally {
      out.mockRestore();
      err.mockRestore();
    }
  });

  it('无状态 → 工厂返回同一实例（单例）', () => {
    expect(createNonInteractivePromptPort()).toBe(createNonInteractivePromptPort());
  });
});

describe('createClackPromptPort', () => {
  it('import smoke：构造不抛错，返回 PromptPort 形状', () => {
    const ui = createClackPromptPort();
    expect(typeof ui.confirm).toBe('function');
    expect(typeof ui.select).toBe('function');
    expect(typeof createClackPromptPort).toBe('function');
  });

  it('类型契约：可赋给 PromptPort，泛型 select 保留字面量类型', () => {
    const ui: PromptPort = createClackPromptPort();
    // 只做类型层面的赋值校验 —— 不发问、不进入交互（不模拟 TTY）。
    const confirmFn: (message: string, fallback: boolean) => Promise<boolean> = ui.confirm;
    const selectFn: (
      message: string,
      options: readonly { value: 'local' | 'remote' | 'skip'; label: string }[],
      fallback: 'local' | 'remote' | 'skip',
    ) => Promise<'local' | 'remote' | 'skip'> = ui.select;
    expect(typeof confirmFn).toBe('function');
    expect(typeof selectFn).toBe('function');
    // 运行时形状与 PromptPort 一致：两个方法都是普通函数。
    expect(Object.keys(ui).sort()).toEqual(['confirm', 'select']);
  });

  it('与 non-interactive 实例是两个不同的对象', () => {
    expect(createClackPromptPort()).not.toBe(createNonInteractivePromptPort());
  });
});

describe('createDefaultPromptPort', () => {
  afterEach(() => {
    // 保证任何 TTY 打桩都被还原（测试进程本身是非 TTY）。
    delete (process.stdout as { isTTY?: boolean }).isTTY;
  });

  it('非 TTY（CI / 测试 / 管道）→ non-interactive 实例', async () => {
    expect(process.stdout.isTTY).toBeUndefined(); // 前置条件：vitest 进程非 TTY
    const ui = createDefaultPromptPort();
    expect(ui).toBe(createNonInteractivePromptPort());
    // 行为也必须是 fallback 语义（命令层据此 abort 并提示 --yes）。
    expect(await settlesWithin(ui.confirm('继续？', false))).toBe(false);
    expect(await settlesWithin(ui.select('冲突如何裁决？', OPTIONS, 'remote'))).toBe('remote');
  });

  it('TTY → 返回 clack 实现（只验分支选择，不发问）', () => {
    Object.defineProperty(process.stdout, 'isTTY', { value: true, configurable: true });
    const ui = createDefaultPromptPort();
    expect(ui).not.toBe(createNonInteractivePromptPort());
    expect(typeof ui.confirm).toBe('function');
    expect(typeof ui.select).toBe('function');
  });

  it('isTTY 为 false（重定向到文件）→ non-interactive', () => {
    Object.defineProperty(process.stdout, 'isTTY', { value: false, configurable: true });
    expect(createDefaultPromptPort()).toBe(createNonInteractivePromptPort());
  });
});
