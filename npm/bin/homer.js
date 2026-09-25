#!/usr/bin/env node
'use strict';

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const packageRoot = path.resolve(__dirname, '..');
const wrapper = path.resolve(__filename);
const candidates = [
  process.env.HOMER_BINARY,
  path.join(packageRoot, 'vendor', 'homer'),
  path.join(os.homedir(), '.local', 'bin', 'homer'),
  '/usr/local/bin/homer',
]
  .filter(Boolean)
  .map((candidate) => path.resolve(candidate))
  // Guard against a candidate resolving to this wrapper itself (e.g.
  // HOMER_BINARY pointing at bin/homer.js), which would spawn us
  // recursively.
  .filter((candidate) => candidate !== wrapper);

const binary = candidates.find((candidate) => fs.existsSync(candidate));
if (!binary) {
  process.stderr.write(
    'homer-cli: the Go binary is not installed. Use curl -fsSL https://raw.githubusercontent.com/zzjcool/homer-cli/master/install.sh | sh\n',
  );
  process.stderr.write('homer-cli: offline installs can set HOMER_INSTALL_PACKAGE when installing this package.\n');
  process.exit(1);
}

const result = spawnSync(binary, process.argv.slice(2), { stdio: 'inherit' });
if (result.error) {
  process.stderr.write(`homer-cli: failed to run ${binary}: ${result.error.message}\n`);
  process.exit(1);
}
process.exit(result.status === null ? 1 : result.status);
