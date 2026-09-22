import type { AdapterConfig } from '../../core/types.js';

/** pi adapter 的唯一标识（写进 CategorySnapshot.adapterId / AdapterSnapshot.adapterId）。 */
export const PI_ADAPTER_ID = 'pi';

/**
 * pi adapter 默认配置（计划 §1.5 逐字落地）。
 *
 * - root 用 '~' 前缀，由 paths 层负责展开（本模块不碰 HOME）。
 * - 7 个分类：settings / skills / extensions / agents / models / prompts / themes。
 * - ignore 是 adapter 级忽略（相对 root 的路径 glob）。
 */
export const DEFAULT_PI_ADAPTER: AdapterConfig = {
  root: '~/.pi/agent',
  enabled: true,
  categories: {
    settings: { paths: ['settings.json', 'keybindings.json'], mode: 'merge' },
    skills: { paths: ['skills/'], mode: 'mirror' },
    extensions: { paths: ['extensions/'], mode: 'mirror', exclude: ['*cache*'] },
    agents: { paths: ['agents/'], mode: 'mirror' },
    models: { paths: ['models.json'], mode: 'merge', excludeKeys: ['apiKeys'] },
    prompts: { paths: ['prompts/'], mode: 'mirror' },
    themes: { paths: ['themes/'], mode: 'mirror' },
  },
  ignore: [
    'auth.json',
    'trust.json',
    'sessions/',
    'npm/',
    'git/',
    'tmp/',
    'bin/',
    '*.bak',
    '*.bak-*',
    '*.bak*',
    '*.log',
    'run-history.jsonl',
  ],
};
