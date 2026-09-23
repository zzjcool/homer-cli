/**
 * M2 对抗式 review 修复 · minor 1-5 的收敛回归。
 *
 * 这五条 minor 的共同点：**同一个事实曾在多处各写一份**（判定公式 / 提示文本 / 类型别名），
 * 于是任何一处漂移都会让「同一件事」在不同命令里给出不同结论。本文件钉住「只有一个实现点」：
 *
 *   1. root 回落守卫（`isRootUnreadable` / 措辞）→ `core/scan-guard.ts`
 *   2. CliError 前置消息文本 → `core/sync/pipeline.ts` 的常量
 *   3. `isJsonObject` → `core/entry-kind.ts` 的 `isPlainObject`
 *   4. e2e 组⑧ merge 路径 prune → 见 tests/cli/merge.test.ts 的 M1 套件
 *   5. README 口径说明 → 见 tests/cli/readme-secrets.test.ts
 */

import { readFileSync } from 'node:fs';
import path from 'node:path';

import { describe, expect, it } from 'vitest';

import {
  isRootUnreadable,
  scanErrorPrefix,
  scanProblemMessage,
  scanWarningMessages,
} from '../../src/core/scan-guard.js';
import { isPlainObject } from '../../src/core/entry-kind.js';
import { isJsonObject } from '../../src/core/sync/excluded-keys.js';
import {
  NOT_A_REPO_HINT,
  NO_UPSTREAM_HINT,
  NO_UPSTREAM_MESSAGE,
  notAGitRepoMessage,
} from '../../src/core/sync/pipeline.js';
import type { AdapterSnapshot } from '../../src/core/types.js';

const EMPTY_SNAPSHOT: AdapterSnapshot = { adapterId: 'pi', categories: [] };
const NONEMPTY_SNAPSHOT: AdapterSnapshot = {
  adapterId: 'pi',
  categories: [{ adapterId: 'pi', category: 'settings', mode: 'merge', files: new Map() }],
};

const ROOT_ERROR = [{ path: '/nonexistent', message: 'ENOENT: no such file or directory' }];
const SCAN_WARNING = [{ path: '/root/link', message: 'symlink 逃逸' }];

/* ------------------------------------------------------------------ */
/* minor 1：root 回落守卫三份拷贝收敛                                  */
/* ------------------------------------------------------------------ */

describe('minor 1 · root 回落守卫（core/scan-guard.ts 是唯一实现点）', () => {
  it('判定公式：空分类 + 有错 = root 不可读；任一侧不成立则不是', () => {
    expect(isRootUnreadable({ snapshot: EMPTY_SNAPSHOT, errors: ROOT_ERROR })).toBe(true);
    // 空分类但无错 → 正常空目录（不是不可读）
    expect(isRootUnreadable({ snapshot: EMPTY_SNAPSHOT, errors: [] })).toBe(false);
    // 有错但扫到了分类 → 局部告警，不是 root 级失败
    expect(isRootUnreadable({ snapshot: NONEMPTY_SNAPSHOT, errors: ROOT_ERROR })).toBe(false);
    // 无错无分类 → 干净的空 adapter
    expect(isRootUnreadable({ snapshot: EMPTY_SNAPSHOT, errors: [] })).toBe(false);
  });

  it('措辞前缀：根级失败与普通告警区分（M-A 要求）', () => {
    expect(scanErrorPrefix(true)).toBe('adapter root 不可读');
    expect(scanErrorPrefix(false)).toBe('扫描告警');
  });

  it('单条消息格式 `<prefix>: <adapterId> (<path>: <message>)`', () => {
    expect(scanProblemMessage('pi', ROOT_ERROR[0] as never, true)).toBe(
      'adapter root 不可读: pi (/nonexistent: ENOENT: no such file or directory)',
    );
    expect(scanProblemMessage('pi', SCAN_WARNING[0] as never, false)).toBe(
      '扫描告警: pi (/root/link: symlink 逃逸)',
    );
  });

  it('scanWarningMessages 按 outcome 统一给出多条消息（供 base / merge 共用）', () => {
    expect(
      scanWarningMessages('pi', { snapshot: EMPTY_SNAPSHOT, errors: ROOT_ERROR }),
    ).toEqual(['adapter root 不可读: pi (/nonexistent: ENOENT: no such file or directory)']);
    expect(
      scanWarningMessages('pi', { snapshot: NONEMPTY_SNAPSHOT, errors: SCAN_WARNING }),
    ).toEqual(['扫描告警: pi (/root/link: symlink 逃逸)']);
    expect(scanWarningMessages('pi', { snapshot: EMPTY_SNAPSHOT, errors: [] })).toEqual([]);
  });

  it('三处调用方都 import 同一实现（无就地重写）', () => {
    const roots = [
      'src/cli/render.ts',
      'src/core/sync/base.ts',
      'src/cli/commands/merge.ts',
    ];
    for (const rel of roots) {
      const file = path.join(process.cwd(), rel);
      const source = readFileSync(file, 'utf8');
      // 判定公式不得在任何调用方就地重现；必须是 import core/scan-guard 的回调
      expect(source, `${rel} 不得就地重写 root 判定公式`).not.toContain(
        'outcome.snapshot.categories.length === 0 && outcome.errors.length > 0',
      );
      expect(source, `${rel} 应 import core/scan-guard`).toContain('scan-guard.js');
    }
  });
});

/* ------------------------------------------------------------------ */
/* minor 2：CliError 前置消息常量收敛                                  */
/* ------------------------------------------------------------------ */

describe('minor 2 · 前置检查文本常量（core/sync/pipeline.ts 是唯一来源）', () => {
  it('常量与构造函数文本稳定', () => {
    expect(NOT_A_REPO_HINT).toBe('请先运行 `homer push` 建立 git 历史与 remote。');
    expect(NO_UPSTREAM_MESSAGE).toBe('未配置 git upstream，无法确定远端');
    expect(NO_UPSTREAM_HINT).toContain('git push -u <remote> <branch>');
    expect(notAGitRepoMessage('/tmp/x')).toBe('工作区不是 git 仓库: /tmp/x');
  });

  it('pull / merge / pipeline 三处共用同一常量（无就地字面量）', () => {
    const files = [
      'src/cli/commands/pull.ts',
      'src/cli/commands/merge.ts',
      'src/core/sync/pipeline.ts',
    ];
    for (const rel of files) {
      const source = readFileSync(path.join(process.cwd(), rel), 'utf8');
      // 提示句不得作为字面量再次出现（只在定义点 pipeline.ts 出现）
      if (rel !== 'src/core/sync/pipeline.ts') {
        expect(source, `${rel} 不得内联 NO_UPSTREAM 提示句`).not.toContain(
          '请先 `git push -u <remote> <branch>`',
        );
        expect(source, `${rel} 不得内联 NOT_A_REPO 提示句`).not.toContain(
          '请先运行 `homer push` 建立 git 历史',
        );
      }
      // 主消息也不得内联
      if (rel !== 'src/core/sync/pipeline.ts') {
        expect(source, `${rel} 不得内联 NO_UPSTREAM 主消息`).not.toContain(
          "'未配置 git upstream，无法确定远端'",
        );
      }
    }
  });
});

/* ------------------------------------------------------------------ */
/* minor 3：isJsonObject 与 isPlainObject 同源                          */
/* ------------------------------------------------------------------ */

describe('minor 3 · isJsonObject 是 isPlainObject 的别名（不是第二份实现）', () => {
  it('同一函数引用（复制回潮的机械证据）', () => {
    expect(isJsonObject).toBe(isPlainObject);
  });

  it('语义快照：对象 → true；数组 / null / 原始值 → false', () => {
    for (const value of [{}, { a: 1 }]) expect(isJsonObject(value)).toBe(true);
    for (const value of [[], null, undefined, 'x', 1, true]) expect(isJsonObject(value)).toBe(false);
  });

  it('excluded-keys.ts 不再就地重写判定', () => {
    const source = readFileSync(path.join(process.cwd(), 'src/core/sync/excluded-keys.ts'), 'utf8');
    expect(source).not.toContain('!Array.isArray(value)');
    expect(source).toContain("from '../entry-kind.js'");
  });
});
