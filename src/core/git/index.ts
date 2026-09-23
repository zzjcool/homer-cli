/**
 * `src/core/git/` 公共出口（barrel）。
 * W6 base.ts / pipeline.ts 与命令层从这里 import，避免深层路径散落。
 */

export {
  gitExec,
  isGitRepo,
  ensureGitRepo,
  hasUpstream,
  upstreamRef,
  configuredUpstream,
  gitFetch,
  gitPush,
  headCommit,
  refExists,
  commitAllStore,
  commitPaths,
  mergeFfUpstream,
  isStoreClean,
  isAncestorOf,
  GIT_DEFAULT_TIMEOUT_MS,
  GITIGNORE_REQUIRED_LINES,
  type GitExecResult,
} from './git.js';

export { readStoreSnapshotAtCommit, readVaultFileAtCommit } from './reader.js';
