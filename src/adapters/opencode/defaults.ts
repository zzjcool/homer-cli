import type { AdapterConfig } from '../../core/types.js';

/** opencode adapter 的唯一标识（写进 CategorySnapshot.adapterId / AdapterSnapshot.adapterId）。 */
export const OPENCODE_ADAPTER_ID = 'opencode';

/**
 * opencode adapter 默认配置（docs/m3-plan.md §2.8 + docs/m3-scout-report.md §2.4 冻结版逐字落地）。
 *
 * 分类冻结依据（S0 侦察实测 `ls -la ~/.config/opencode`）：
 * - `opencode.json`  → config，**merge**（合法 JSON：provider/models/permission）；
 * - `package.json`   → plugins，**merge**（插件依赖声明，如 `@opencode-ai/plugin`）；
 * - `package-lock.json` + `bun.lock` → locks，**mirror**（锁文件，非 JSON 合并语义）；
 * - `node_modules/` / `.plugins.lock` / `*.log` / `.gitignore` → 忽略（依赖实体 / 插件锁 /
 *   日志 / opencode 自生成的 gitignore）。
 *
 * 安全注记（§2.8 冻结）：`opencode.json` 的 `provider.*.options.apiKey` 是嵌套键，M1 冻结的
 * `excludeKeys` 只支持顶层 —— 不做引擎扩展。真实长 key 由 M2 密钥扫描闸门在 push 时拦截；
 * 短占位值（如 `"ccrb"`）不命中。
 *
 * 注意：opencode 自带的 `.gitignore` 把 `package.json` / `bun.lock` 也 ignore，但 homer 的 store
 * 是**独立 git 仓库**（homer home），不受该文件影响，故这两个文件可正常同步。
 *
 * root 用 '~' 前缀，由 paths 层负责展开（本模块不碰 HOME）。
 */
export const DEFAULT_OPENCODE_ADAPTER: AdapterConfig = {
  root: '~/.config/opencode',
  enabled: true,
  categories: {
    config: { paths: ['opencode.json'], mode: 'merge' },
    plugins: { paths: ['package.json'], mode: 'merge' },
    locks: { paths: ['package-lock.json', 'bun.lock'], mode: 'mirror' },
  },
  ignore: ['node_modules/', '.plugins.lock', '*.log', '.gitignore'],
};
