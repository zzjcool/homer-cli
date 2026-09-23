export type SyncMode = 'merge' | 'mirror';

export interface CategoryConfig {
  paths: string[];              // 相对 adapter root；'xxx/' 结尾 = 目录，否则单文件
  mode: SyncMode;
  enabled?: boolean;            // 默认 true
  exclude?: string[];           // category 内文件名 glob（仅 '*' 通配）
  excludeKeys?: string[];       // merge 模式字段级排除（顶层键名）
}

export interface AdapterConfig {
  root: string;                 // '~' 需展开
  enabled?: boolean;
  categories: Record<string, CategoryConfig>;
  ignore?: string[];            // adapter 级忽略，相对 root 的路径 glob
  /**
   * symlink 逃逸 allowlist（docs/m3-plan.md §2.0-1 / D7，可选）：
   * 相对 adapter root 的 glob（`matchesIgnore` 语义）。命中 = 允许该 symlink 逃逸 root 并跟随；
   * 缺省 = 维持 M1 安全边界（跳过 + ScanError）。显式 opt-in，不做全局放开。
   */
  allowEscape?: string[];
}

/** 备份保留策略（docs/m2-plan.md §2.0-1；可选，缺省保留最近 7 个日期目录）。 */
export interface BackupConfig {
  keep?: number;                // 保留最近 N 个日期目录，默认 7
}

/** 密钥同步配置（docs/m2-plan.md §2.0-1 + docs/m3-plan.md §2.0-1；可选）。 */
export interface SecretsConfig {
  ignorePaths?: string[];       // store 相对路径 glob（matchesIgnore 语义）
  /** age X25519 recipients（age1...），secret push 的加密目标。 */
  recipients?: string[];
  /** secret 名 -> 目标路径（必须 '~' 或 '/' 开头；'~' 用 expandHome 展开）。name 须过 secretNameValid。 */
  files?: Record<string, string>;
}

export interface HomerConfig {
  version: 1;
  adapters: Record<string, AdapterConfig>;
  backup?: BackupConfig;
  secrets?: SecretsConfig;
}

export interface SnapshotEntry {
  kind: 'json' | 'file';        // 'json' = 可 JSON.parse；merge 分类中解析失败自动降级 'file'
  content: string;              // 原始文本（UTF-8）
}

/** key = category 内相对路径（见 §1.6 布局规则） */
export type SnapshotFiles = Map<string, SnapshotEntry>;

export interface CategorySnapshot {
  adapterId: string;
  category: string;
  mode: SyncMode;
  files: SnapshotFiles;
}

export interface AdapterSnapshot {
  adapterId: string;
  categories: CategorySnapshot[];
}
