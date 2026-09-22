#!/usr/bin/env node
// homer CLI 薄壳：只负责加载构建产物，不含逻辑。
// 发布形态（npm i -g homer-cli）走 dist；开发态未构建时给出提示。
import { existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const entry = join(here, '..', 'dist', 'cli', 'index.js');

if (!existsSync(entry)) {
  process.stderr.write(
    'homer: 未找到构建产物 dist/cli/index.js\n' +
      '请先运行 `npm run build`，或用 `npx tsx src/cli/index.ts` 直接跑源码。\n',
  );
  process.exit(1);
}

await import(entry);
