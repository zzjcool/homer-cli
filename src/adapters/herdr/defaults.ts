import type { AdapterConfig } from '../../core/types.js';

/** herdr adapter 的唯一标识（写进 CategorySnapshot.adapterId / AdapterSnapshot.adapterId）。 */
export const HERDR_ADAPTER_ID = 'herdr';

/**
 * herdr adapter 默认配置（docs/m3-plan.md §2.8 + docs/m3-scout-report.md §1.5 冻结版逐字落地）。
 *
 * 分类冻结依据（S0 侦察实测 `ls -la ~/.config/herdr`）：
 * - `config.toml` 是**唯一**用户设置文件（onboarding / theme / ui / experimental 全部内联；
 *   主题与 agent 定义无独立用户文件）→ config 分类，mirror 同步；
 * - `session.json` / `*.sock` / `*.log` / `.plugins.lock` / `release-notes.json` 全部为
 *   运行时状态、socket、日志与机器相关缓存 → 忽略；
 * - `~/.herdr/worktrees/**` 与 `~/.local/state/herdr/**` 是机器本地 git checkout / XDG 运行时状态，
 *   **不纳入** adapter（既不进 root，也不进 ignore 列表——它们根本不在 root 下）。
 *
 * ignore 是 adapter 级忽略（相对 root 的路径 glob，见 src/adapters/pi/ignore.ts 的冻结语义）。
 * root 用 '~' 前缀，由 paths 层负责展开（本模块不碰 HOME）。
 */
export const DEFAULT_HERDR_ADAPTER: AdapterConfig = {
  root: '~/.config/herdr',
  enabled: true,
  categories: {
    config: { paths: ['config.toml'], mode: 'mirror' },
  },
  ignore: ['session.json', '*.sock', '*.log', '.plugins.lock', 'release-notes.json'],
};
