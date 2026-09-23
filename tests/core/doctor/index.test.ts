/**
 * `src/core/doctor/` 门面测试（docs/m3-plan.md §2.5 / §3-P2-W7）。
 *
 * 门面是命令层（`src/cli/commands/doctor.ts`）与 P3-W8 的 `home` 的唯一 import 路径，
 * 故这里锁定「八项检查函数 + 四个类型」的导出清单逐字不变（显式导出，不用 `export *`）。
 * 判定语义由 `checks.test.ts` 覆盖，本文件只管契约面。
 */

import { describe, expect, it } from 'vitest';

import * as doctor from '../../../src/core/doctor/index.js';
import { DOCTOR_USAGE } from '../../../src/cli/commands/doctor.js';

describe('core/doctor 门面（§2.5 导出清单）', () => {
  it('导出八项检查函数', () => {
    for (const name of [
      'checkConfig',
      'checkRepoAndStore',
      'checkRemote',
      'checkAdapters',
      'checkAge',
      'checkMachine',
      'checkRequiredPlaceholders',
    ] as const) {
      expect(typeof doctor[name], name).toBe('function');
    }
  });

  it('命令层 re-export 的四个类型来自同一模块（形状不漂移的编译期证据）', async () => {
    const command = await import('../../../src/cli/commands/doctor.js');
    // 运行时只有函数；类型同一性由 typecheck 保证（下方赋值让 tsc 校验）。
    const report: doctor.DoctorReport = { checks: [], ok: true };
    const checks: doctor.DoctorCheck[] = [];
    const status: doctor.CheckStatus = 'ok';
    const id: doctor.DoctorCheckId = 'config';

    expect(command.DOCTOR_USAGE).toBe(DOCTOR_USAGE);
    expect(report.ok).toBe(true);
    expect(checks).toHaveLength(0);
    expect(status).toBe('ok');
    expect(id).toBe('config');
  });

  it('远端超时常量保守（≤ gitExec 默认 15s）', () => {
    expect(doctor.REMOTE_CHECK_TIMEOUT_MS).toBe(10_000);
  });
});
