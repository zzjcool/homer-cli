/**
 * render.ts 纯函数测试（§2-P1-M4：merge 键行 / mirror 行级 +/- / 空输出 = 无漂移）。
 */

import { describe, expect, it } from 'vitest';

import { diffLines, formatValue, renderInit, renderStatus, splitLines } from '../../src/cli/render.js';

describe('renderStatus', () => {
  const report = {
    adapters: [
      {
        id: 'pi',
        push: 3,
        pull: 0,
        conflicts: 0,
        categories: [
          { name: 'settings', push: 1, pull: 0, conflicts: 0 },
          { name: 'skills', push: 2, pull: 0, conflicts: 0 },
        ],
      },
    ],
  };

  it('汇总行包含 ↑ / ↓ 计数', () => {
    expect(renderStatus(report)).toBe('pi  ↑3 ↓0');
  });

  it('--verbose 逐分类打印', () => {
    const text = renderStatus(report, { verbose: true });
    expect(text.split('\n')).toEqual(['pi  ↑3 ↓0', '  settings  ↑1 ↓0', '  skills  ↑2 ↓0']);
  });

  it('全零 → 无漂移', () => {
    const zero = {
      adapters: [{ id: 'pi', push: 0, pull: 0, conflicts: 0, categories: [] }],
    };
    expect(renderStatus(zero)).toBe('pi  ↑0 ↓0\n无漂移');
  });

  it('冲突数打标', () => {
    const withConflict = {
      adapters: [{ id: 'pi', push: 0, pull: 0, conflicts: 2, categories: [] }],
    };
    expect(renderStatus(withConflict)).toContain('冲突2');
  });

  it('空 adapter 列表给出说明', () => {
    expect(renderStatus({ adapters: [] })).toContain('没有启用的 adapter');
  });
});

describe('renderInit', () => {
  it('按 adapter / 分类汇总文件数', () => {
    const text = renderInit({
      homerHome: '/tmp/homer',
      adapters: [
        {
          id: 'pi',
          categories: [
            { name: 'settings', fileCount: 2 },
            { name: 'skills', fileCount: 3 },
          ],
        },
      ],
    });
    expect(text.split('\n')).toEqual(['homer init: /tmp/homer', '  pi: 5 个文件', '    settings: 2', '    skills: 3']);
  });
});

describe('formatValue', () => {
  it('字符串原样输出（不加引号）', () => {
    expect(formatValue('dark')).toBe('dark');
  });
  it('数字 / 布尔 / null 用 JSON 字面量', () => {
    expect(formatValue(1)).toBe('1');
    expect(formatValue(true)).toBe('true');
    expect(formatValue(null)).toBe('null');
  });
  it('对象 / 数组紧凑 JSON', () => {
    expect(formatValue({ a: 1 })).toBe('{"a":1}');
    expect(formatValue([1, 2])).toBe('[1,2]');
  });
  it('undefined（键不存在）→ (无)', () => {
    expect(formatValue(undefined)).toBe('(无)');
  });
});

describe('splitLines / diffLines', () => {
  it('splitLines: 空串 → []，末尾换行不产生空行', () => {
    expect(splitLines('')).toEqual([]);
    expect(splitLines('a\n')).toEqual(['a']);
    expect(splitLines('a\nb')).toEqual(['a', 'b']);
  });

  it('内容相同 → 空 diff（无漂移）', () => {
    expect(diffLines('a\nb\n', 'a\nb\n')).toEqual([]);
  });

  it('文件新增 → 全文 +', () => {
    expect(diffLines('', 'x\ny\n')).toEqual(['+x', '+y']);
  });

  it('文件删除 → 全文 -', () => {
    expect(diffLines('x\ny\n', '')).toEqual(['-x', '-y']);
  });

  it('行级修改 → 上下文 + -/+', () => {
    expect(diffLines('a\nb\n', 'a\nc\n')).toEqual([' a', '-b', '+c']);
  });

  it('纯新增行', () => {
    expect(diffLines('a\n', 'a\nb\n')).toEqual([' a', '+b']);
  });
});
