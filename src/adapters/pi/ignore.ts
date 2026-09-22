/**
 * pi adapter 私有 ignore / exclude 匹配（签名冻结，见 docs/m1-plan.md §1.5）。
 *
 * glob 规则（冻结，禁止膨胀）：
 *  - 字面量字符逐字匹配；
 *  - `*` 匹配任意非 `/` 字符序列（可以为空）；
 *  - 模式以 `/` 结尾 = 目录前缀匹配：`sessions/` 命中 `sessions/x` 与 `sessions/a/b`；
 *  - 不支持 `?`（按字面字符处理）与 `**`（等价于单个 `*`，不跨 `/`）。
 * 不引入 picomatch。
 */

/** 归一化：去掉前导 './' 与 '/'，保留尾部 '/'（目录前缀语义依赖它）。 */
function normalize(input: string): string {
  let out = input;
  while (out.startsWith('./')) out = out.slice(2);
  while (out.startsWith('/')) out = out.slice(1);
  return out;
}

function escapeLiteral(ch: string): string {
  return ch.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/** 把单个 glob 模式编译成正则**主体**（不带锚点）。 */
function globBody(pattern: string): string {
  let body = '';
  for (const ch of pattern) {
    if (ch === '*') body += '[^/]*';
    else body += escapeLiteral(ch);
  }
  return body;
}

/**
 * 单个模式匹配（不含目录前缀语义，调用方需自行传入 `xxx/` 形态）。
 * 尾部 `/` 的模式走目录前缀：`^glob(prefix)(/.*)?$`。
 */
export function globMatch(candidate: string, pattern: string): boolean {
  const path = normalize(candidate);
  const pat = normalize(pattern);
  if (pat === '') return false;
  if (pat.endsWith('/')) {
    const prefix = pat.slice(0, -1);
    if (prefix === '') return false;
    return new RegExp(`^${globBody(prefix)}(?:/.*)?$`).test(path);
  }
  return new RegExp(`^${globBody(pat)}$`).test(path);
}

/**
 * adapter 级忽略判定：relPath 相对 adapter root，命中任一 pattern 即为 true。
 * relPath 为目录时同样适用（`sessions/` 命中 relPath `sessions`）。
 */
export function matchesIgnore(relPath: string, patterns: string[]): boolean {
  if (!patterns || patterns.length === 0) return false;
  return patterns.some((pattern) => globMatch(relPath, pattern));
}
