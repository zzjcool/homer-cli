import { homedir } from 'node:os';
import path from 'node:path';

/**
 * homer 本地工作区的全部路径。
 * 布局（DESIGN §2.1）：
 *   <home>/homer.json   主配置（随 git 仓库走）
 *   <home>/store/       adapter 分类快照
 *   <home>/state.json   本地同步状态（不入库）
 *   <home>/backups/     应用远端变更前的备份（不入库，见 docs/m2-plan.md §2.0-2）
 *   <home>/secrets/     age 密文 vault（**入库**，一密钥一 <name>.age，见 docs/m3-plan.md §2.0-2 / D3）
 *   <home>/keys/        本机 age 私钥（`age.txt`，0600；**不入库**，见 docs/m3-plan.md §2.0-2 / D2）
 */
export interface HomerPaths {
  home: string;
  storeDir: string;
  configFile: string;
  stateFile: string;
  backupsDir: string;
  secretsDir: string;
  keysDir: string;
}

/**
 * 展开开头的 `~` / `~/`。只处理开头，不做全局替换（与 shell 语义一致）。
 * 导出给 adapter 侧复用（scan.ts 曾自带一份同名私有实现，属重复代码）。
 */
export function expandHome(input: string): string {
  if (input === '~') return homedir();
  if (input.startsWith('~/') || input.startsWith('~\\')) {
    return path.join(homedir(), input.slice(2));
  }
  return input;
}

/**
 * 解析 homer 工作区路径。
 *
 * 优先级：显式传入的 `env.HOMER_HOME` > `process.env.HOMER_HOME` > `~/.homer`。
 * `HOMER_HOME` 覆盖是测试不碰真实 `~/.homer` 的关键；空字符串视为未设置。
 * 返回值中的 `home` 一律是绝对路径（相对路径按 cwd 解析）。
 */
export function getHomerPaths(env: { HOMER_HOME?: string | undefined } = process.env): HomerPaths {
  const raw = env.HOMER_HOME;
  const home =
    typeof raw === 'string' && raw.trim() !== ''
      ? path.resolve(expandHome(raw.trim()))
      : path.join(homedir(), '.homer');

  return {
    home,
    storeDir: path.join(home, 'store'),
    configFile: path.join(home, 'homer.json'),
    stateFile: path.join(home, 'state.json'),
    backupsDir: path.join(home, 'backups'),
    secretsDir: path.join(home, 'secrets'),
    keysDir: path.join(home, 'keys'),
  };
}
