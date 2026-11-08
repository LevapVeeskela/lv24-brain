import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { mkdtemp, mkdir, readFile, rm, stat, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  BuildError,
  PLATFORMS,
  assetFileName,
  generatePackages,
  parseChecksums,
  sha256Hex,
} from './build.mjs';

const require = createRequire(import.meta.url);
const launcher = require('./bin/brain.js');

const VERSION = '1.2.3';
const here = path.dirname(fileURLToPath(import.meta.url));

async function makeTempDir(t) {
  const dir = await mkdtemp(path.join(os.tmpdir(), 'brain-npm-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  return dir;
}

function fakeBinary(platform) {
  return Buffer.from(`fake brain binary for ${platform.packageName}`);
}

// Writes a fake GoReleaser dist: one binary per platform plus checksums.txt.
// options.skip: goreleaser names to omit from the directory (still listed in checksums).
// options.corrupt: goreleaser names whose checksum line is wrong.
async function makeDist(t, { version = VERSION, skip = [], corrupt = [] } = {}) {
  const dist = await makeTempDir(t);
  const lines = [];
  for (const platform of PLATFORMS) {
    const name = assetFileName(version, platform);
    const data = fakeBinary(platform);
    if (!skip.includes(name)) {
      await writeFile(path.join(dist, name), data);
    }
    const digest = corrupt.includes(name) ? '0'.repeat(64) : sha256Hex(data);
    lines.push(`${digest}  ${name}`);
  }
  lines.push(`${'a'.repeat(64)}  brain-server_${version}_linux_amd64`);
  await writeFile(path.join(dist, 'checksums.txt'), `${lines.join('\n')}\n`);
  return dist;
}

test('generates main and platform packages from a fake dist', async (t) => {
  const dist = await makeDist(t);
  const out = await makeTempDir(t);

  const written = await generatePackages({ version: VERSION, distDir: dist, outDir: out });
  assert.equal(written.length, PLATFORMS.length + 1);

  const main = JSON.parse(await readFile(path.join(out, 'lv24-brain', 'package.json'), 'utf8'));
  assert.equal(main.name, 'lv24-brain');
  assert.equal(main.version, VERSION);
  assert.deepEqual(main.bin, { brain: 'bin/brain.js' });
  assert.deepEqual(main.files, ['bin']);
  assert.equal(main.license, 'Apache-2.0');
  assert.equal(main.engines.node, '>=18');
  assert.deepEqual(Object.keys(main.optionalDependencies).sort(), PLATFORMS.map((p) => p.packageName).sort());
  for (const version of Object.values(main.optionalDependencies)) {
    assert.equal(version, VERSION);
  }
  assert.equal(main.scripts, undefined, 'no install scripts');

  for (const platform of PLATFORMS) {
    const dir = path.join(out, platform.packageName);
    const pkg = JSON.parse(await readFile(path.join(dir, 'package.json'), 'utf8'));
    assert.equal(pkg.name, platform.packageName);
    assert.equal(pkg.version, VERSION);
    assert.deepEqual(pkg.os, [platform.os]);
    assert.deepEqual(pkg.cpu, [platform.cpu]);
    assert.deepEqual(pkg.files, ['bin']);
    assert.equal(pkg.scripts, undefined, 'no install scripts');

    const binPath = path.join(dir, 'bin', platform.binaryName);
    assert.deepEqual(await readFile(binPath), fakeBinary(platform));
    if (process.platform !== 'win32') {
      const mode = (await stat(binPath)).mode;
      assert.notEqual(mode & 0o111, 0, `${platform.binaryName} must be executable`);
    }
  }

  const readme = await readFile(path.join(out, 'lv24-brain', 'README.md'), 'utf8');
  assert.equal(readme, await readFile(path.join(here, 'README.md'), 'utf8'));
  for (const platform of PLATFORMS) {
    const platformReadme = await readFile(path.join(out, platform.packageName, 'README.md'), 'utf8');
    assert.match(platformReadme, /npm i -g lv24-brain/);
  }

  const launcherCopy = await readFile(path.join(out, 'lv24-brain', 'bin', 'brain.js'), 'utf8');
  assert.equal(launcherCopy, await readFile(path.join(here, 'bin', 'brain.js'), 'utf8'));
});

test('asset-version selects GoReleaser names while package version stays explicit', async (t) => {
  const dist = await makeDist(t, { version: '0.0.1-next' });
  const out = await makeTempDir(t);

  await generatePackages({ version: '0.0.0-ci', assetVersion: '0.0.1-next', distDir: dist, outDir: out });

  const pkg = JSON.parse(await readFile(path.join(out, '@lv24/brain-linux-x64', 'package.json'), 'utf8'));
  assert.equal(pkg.version, '0.0.0-ci');
});

test('fails on checksum mismatch and writes nothing', async (t) => {
  const corrupt = [assetFileName(VERSION, PLATFORMS[2])];
  const dist = await makeDist(t, { corrupt });
  const out = await makeTempDir(t);

  await assert.rejects(
    generatePackages({ version: VERSION, distDir: dist, outDir: out }),
    (err) => err instanceof BuildError && /checksum mismatch/.test(err.message) && err.message.includes(corrupt[0]),
  );
  assert.deepEqual(await readdirSafe(out), []);
});

test('fails when a platform binary is missing', async (t) => {
  const skip = [assetFileName(VERSION, PLATFORMS[3])];
  const dist = await makeDist(t, { skip });
  const out = await makeTempDir(t);

  await assert.rejects(
    generatePackages({ version: VERSION, distDir: dist, outDir: out }),
    (err) => err instanceof BuildError && err.message.includes(PLATFORMS[3].packageName),
  );
  assert.deepEqual(await readdirSafe(out), []);
});

test('fails when checksums.txt has no entry for a binary', async (t) => {
  const dist = await makeDist(t);
  const lines = (await readFile(path.join(dist, 'checksums.txt'), 'utf8'))
    .split('\n')
    .filter((line) => !line.includes(assetFileName(VERSION, PLATFORMS[0])));
  await writeFile(path.join(dist, 'checksums.txt'), lines.join('\n'));
  const out = await makeTempDir(t);

  await assert.rejects(
    generatePackages({ version: VERSION, distDir: dist, outDir: out }),
    /no entry for brain_1\.2\.3_windows_amd64\.exe/,
  );
});

test('rejects an invalid version before touching the filesystem', async (t) => {
  const dist = await makeDist(t);
  const out = await makeTempDir(t);

  await assert.rejects(
    generatePackages({ version: 'v1.2.3', distDir: dist, outDir: out }),
    /invalid version/,
  );
});

test('parseChecksums accepts sha256sum and binary-mode lines', () => {
  const hex = 'AB'.repeat(32);
  const sums = parseChecksums(`${hex}  brain_1.0.0_linux_amd64\n${'cd'.repeat(32)} *brain_1.0.0_darwin_arm64\nnot a line\n`);
  assert.equal(sums.get('brain_1.0.0_linux_amd64'), hex.toLowerCase());
  assert.equal(sums.get('brain_1.0.0_darwin_arm64'), 'cd'.repeat(32));
  assert.equal(sums.size, 2);
});

test('launcher maps every release platform to its package name', () => {
  for (const platform of PLATFORMS) {
    assert.equal(launcher.platformPackageName(platform.os, platform.cpu), platform.packageName);
  }
  assert.equal(launcher.PLATFORM_PACKAGES.size, PLATFORMS.length);
  assert.equal(launcher.platformPackageName('linux', 'ia32'), null);
  assert.equal(launcher.platformPackageName('freebsd', 'x64'), null);
});

test('resolveBinary points at bin/brain.exe on Windows and bin/brain elsewhere', () => {
  const manifest = (pkg) => () => path.join('/prefix', 'node_modules', pkg, 'package.json');
  const win = launcher.resolveBinary('win32', 'x64', manifest('@lv24/brain-win32-x64'));
  assert.equal(win, path.join('/prefix', 'node_modules', '@lv24/brain-win32-x64', 'bin', 'brain.exe'));
  const linux = launcher.resolveBinary('linux', 'arm64', manifest('@lv24/brain-linux-arm64'));
  assert.equal(linux, path.join('/prefix', 'node_modules', '@lv24/brain-linux-arm64', 'bin', 'brain'));
});

test('resolveBinary explains a missing platform package and an unsupported platform', () => {
  const missing = () => {
    throw Object.assign(new Error('not found'), { code: 'MODULE_NOT_FOUND' });
  };
  assert.throws(() => launcher.resolveBinary('darwin', 'arm64', missing), /platform package @lv24\/brain-darwin-arm64 is not installed.*--no-optional/);
  assert.throws(() => launcher.resolveBinary('linux', 'ia32', missing), /unsupported platform linux\/ia32/);
});

test('runBrain forwards args, stdio policy and npm marker, and passes the exit status through', () => {
  const calls = [];
  const spawn = (binary, args, options) => {
    calls.push({ binary, args, options });
    return { status: 7, signal: null };
  };
  const stderr = { write: () => assert.fail('nothing should be written to stderr') };
  const code = launcher.runBrain(['open', '--json'], {
    platform: 'linux',
    arch: 'x64',
    resolve: () => '/p/node_modules/@lv24/brain-linux-x64/package.json',
    spawn,
    env: { PATH: '/bin' },
    stderr,
  });

  assert.equal(code, 7);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].binary, path.join('/p/node_modules/@lv24/brain-linux-x64', 'bin', 'brain'));
  assert.deepEqual(calls[0].args, ['open', '--json']);
  assert.equal(calls[0].options.stdio, 'inherit');
  assert.equal(calls[0].options.env.BRAIN_INSTALL_SOURCE, 'npm');
  assert.equal(calls[0].options.env.PATH, '/bin');
});

test('runBrain maps signals and spawn errors to exit status 1', () => {
  const resolve = () => '/p/node_modules/@lv24/brain-linux-x64/package.json';
  const signalled = launcher.runBrain([], {
    platform: 'linux', arch: 'x64', resolve,
    spawn: () => ({ status: null, signal: 'SIGKILL' }),
    stderr: { write: () => {} },
  });
  assert.equal(signalled, 1);

  let message = '';
  const failed = launcher.runBrain([], {
    platform: 'linux', arch: 'x64', resolve,
    spawn: () => ({ error: new Error('EACCES') }),
    stderr: { write: (text) => { message += text; } },
  });
  assert.equal(failed, 1);
  assert.match(message, /cannot run .*: EACCES/);
});

test('runBrain prints a clear error and exits 1 when the platform package is missing', () => {
  let message = '';
  const code = launcher.runBrain([], {
    platform: 'win32', arch: 'arm64',
    resolve: () => { throw new Error('missing'); },
    spawn: () => assert.fail('must not spawn without a binary'),
    stderr: { write: (text) => { message += text; } },
  });
  assert.equal(code, 1);
  assert.match(message, /@lv24\/brain-win32-arm64 is not installed/);
});

async function readdirSafe(dir) {
  const { readdir } = await import('node:fs/promises');
  try {
    return await readdir(dir);
  } catch {
    return [];
  }
}
