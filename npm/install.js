'use strict';

// Best-effort postinstall for the npm compatibility channel. The package is a
// thin wrapper and deliberately never makes npm install fail when GitHub is
// unavailable; install.sh remains the supported binary channel.
const crypto = require('node:crypto');
const fs = require('node:fs');
const http = require('node:http');
const https = require('node:https');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const root = path.resolve(__dirname);
const vendorDir = path.join(root, 'vendor');
const vendorBinary = path.join(vendorDir, 'homer');

function platform() {
  const osName = process.platform === 'darwin' ? 'darwin' : process.platform === 'linux' ? 'linux' : '';
  const arch = process.arch === 'x64' ? 'amd64' : process.arch === 'arm64' ? 'arm64' : '';
  if (!osName || !arch) throw new Error(`unsupported npm platform: ${process.platform}/${process.arch}`);
  return { osName, arch, archive: `homer_${osName}_${arch}.tar.gz` };
}

function isArchive(file) {
  return /\.(?:tar\.gz|tgz)$/i.test(file);
}

function copyLocal(source, destination) {
  const normalized = source.startsWith('file://') ? new URL(source) : source;
  const file = normalized instanceof URL ? normalized : path.resolve(source);
  fs.copyFileSync(file, destination);
}

function download(url, destination, redirects = 0) {
  if (redirects > 5) return Promise.reject(new Error('too many redirects'));
  if (url.startsWith('file://') || !/^[a-z]+:/i.test(url)) {
    try {
      copyLocal(url, destination);
      return Promise.resolve();
    } catch (error) {
      return Promise.reject(error);
    }
  }
  const client = url.startsWith('https:') ? https : http;
  return new Promise((resolve, reject) => {
    const request = client.get(url, { headers: { 'User-Agent': 'homer-cli-npm' } }, (response) => {
      if (response.statusCode >= 300 && response.statusCode < 400 && response.headers.location) {
        response.resume();
        const next = new URL(response.headers.location, url).toString();
        download(next, destination, redirects + 1).then(resolve, reject);
        return;
      }
      if (response.statusCode !== 200) {
        response.resume();
        reject(new Error(`HTTP ${response.statusCode}`));
        return;
      }
      const output = fs.createWriteStream(destination);
      response.pipe(output);
      output.on('finish', () => output.close(resolve));
      output.on('error', reject);
    });
    request.setTimeout(15000, () => request.destroy(new Error('download timeout')));
    request.on('error', reject);
  });
}

function sha256(file) {
  return new Promise((resolve, reject) => {
    const hash = crypto.createHash('sha256');
    const input = fs.createReadStream(file);
    input.on('error', reject);
    input.on('data', (chunk) => hash.update(chunk));
    input.on('end', () => resolve(hash.digest('hex')));
  });
}

function checksumFor(text, basename) {
  for (const line of text.split(/\r?\n/)) {
    const fields = line.trim().split(/\s+/);
    if (fields.length < 2) continue;
    const name = fields[1].replace(/^\*/, '').split('/').pop();
    if (name === basename) return fields[0];
  }
  return '';
}

async function verify(archive, checksums) {
  const expected = checksumFor(fs.readFileSync(checksums, 'utf8'), path.basename(archive));
  if (!expected) throw new Error(`checksums.txt has no entry for ${path.basename(archive)}`);
  const actual = await sha256(archive);
  if (actual !== expected) throw new Error(`checksum mismatch for ${path.basename(archive)}`);
}

function findBinary(directory) {
  for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
    const candidate = path.join(directory, entry.name);
    if (entry.isDirectory()) {
      const nested = findBinary(candidate);
      if (nested) return nested;
    } else if (entry.isFile() && entry.name === 'homer') {
      return candidate;
    }
  }
  return '';
}

function archiveURL(info) {
  const direct = process.env.HOMER_INSTALL_URL || '';
  if (direct) {
    if (/\.(?:tar\.gz|tgz)$/i.test(direct) || /\/homer_[^/]+$/.test(direct)) return direct;
    return `${direct.replace(/\/$/, '')}/${info.archive}`;
  }
  const version = process.env.HOMER_INSTALL_VERSION || 'latest';
  const base = process.env.HOMER_NPM_BASE_URL || process.env.HOMER_INSTALL_BASE_URL || 'https://github.com/zzjcool/homer-cli/releases';
  if (version === 'latest') {
    if (base.endsWith('/latest/download')) return `${base}/${info.archive}`;
    if (base.endsWith('/releases')) return `${base}/latest/download/${info.archive}`;
    return `${base.replace(/\/$/, '')}/releases/latest/download/${info.archive}`;
  }
  const tag = version.startsWith('v') ? version : `v${version}`;
  if (base.endsWith('/download')) return `${base}/${tag}/${info.archive}`;
  if (base.endsWith('/releases')) return `${base}/download/${tag}/${info.archive}`;
  return `${base.replace(/\/$/, '')}/releases/download/${tag}/${info.archive}`;
}

async function install() {
  if (process.env.HOMER_NPM_SKIP_INSTALL === '1') {
    console.log('homer-cli: postinstall skipped (HOMER_NPM_SKIP_INSTALL=1)');
    return;
  }
  const info = platform();
  const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'homer-npm-'));
  try {
    const packageOverride = process.env.HOMER_INSTALL_PACKAGE || '';
    const packageFile = path.join(temp, packageOverride ? path.basename(packageOverride) : info.archive);
    let archive = false;
    if (packageOverride) {
      copyLocal(packageOverride, packageFile);
      archive = isArchive(packageFile);
      if (archive) {
        const checksumOverride = process.env.HOMER_INSTALL_CHECKSUM || '';
        const sibling = path.join(path.dirname(path.resolve(packageOverride)), 'checksums.txt');
        if (checksumOverride) {
          if (fs.existsSync(checksumOverride)) {
            await verify(packageFile, checksumOverride);
          } else {
            const actual = await sha256(packageFile);
            if (actual !== checksumOverride) throw new Error(`checksum mismatch for ${path.basename(packageFile)}`);
          }
        } else if (fs.existsSync(sibling)) {
          await verify(packageFile, sibling);
        }
      }
    } else {
      const url = archiveURL(info);
      await download(url, packageFile);
      archive = isArchive(url);
      if (archive) {
        const checksumURL = process.env.HOMER_INSTALL_CHECKSUM_URL || `${url.slice(0, url.lastIndexOf('/') + 1)}checksums.txt`;
        const checksums = path.join(temp, 'checksums.txt');
        await download(checksumURL, checksums);
        await verify(packageFile, checksums);
      }
    }

    let binary = packageFile;
    if (archive) {
      const extracted = path.join(temp, 'extract');
      fs.mkdirSync(extracted);
      const result = spawnSync('tar', ['-xzf', packageFile, '-C', extracted], { stdio: 'ignore' });
      if (result.error || result.status !== 0) throw result.error || new Error('tar extraction failed');
      binary = findBinary(extracted);
      if (!binary) throw new Error('release archive does not contain homer');
    }
    fs.mkdirSync(vendorDir, { recursive: true });
    const staged = `${vendorBinary}.tmp-${process.pid}`;
    fs.copyFileSync(binary, staged);
    fs.chmodSync(staged, 0o755);
    fs.renameSync(staged, vendorBinary);
    console.log(`homer-cli: installed Go binary at ${vendorBinary}`);
  } finally {
    fs.rmSync(temp, { recursive: true, force: true });
  }
}

install().catch((error) => {
  console.warn(`homer-cli: binary download skipped (${error.message})`);
  console.warn('homer-cli: use install.sh for the primary channel or retry with HOMER_INSTALL_PACKAGE.');
  // A failed optional download must never make npm install fail.
  process.exitCode = 0;
});
