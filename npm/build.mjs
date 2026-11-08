#!/usr/bin/env node
// Generates the npm packages from GoReleaser release assets:
//   lv24-brain                 launcher (bin/brain.js) + optionalDependencies
//   @lv24-brain/<os>-<cpu>      one package per platform, containing only the binary
//
// Usage:
//   node npm/build.mjs --version 1.2.3 --dist release-assets --out npm-out
//       [--asset-version 1.2.3]   GoReleaser version embedded in asset names (default: --version)
//
// Every binary is checked against checksums.txt in the --dist directory before
// anything is written. No dependencies beyond Node built-ins.

import { createHash } from 'node:crypto';
import { access, chmod, copyFile, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const LICENSE_PATH = path.join(here, '..', 'LICENSE');
const LAUNCHER_PATH = path.join(here, 'bin', 'brain.js');

export const MAIN_PACKAGE = 'lv24-brain';
// Platform packages live in the lv24-brain npm organization (unscoped names
// like lv24-brain-darwin-arm64 were rejected by npm's spam detection).
export const PLATFORM_SCOPE = '@lv24-brain';
export const REPOSITORY_URL = 'https://github.com/LevapVeeskela/lv24-brain';
export const LICENSE = 'Apache-2.0';

// goos/goarch are GoReleaser's names (asset file names); os/cpu are npm's.
export const PLATFORMS = Object.freeze(
  [
    { goos: 'windows', goarch: 'amd64', os: 'win32', cpu: 'x64' },
    { goos: 'windows', goarch: 'arm64', os: 'win32', cpu: 'arm64' },
    { goos: 'darwin', goarch: 'amd64', os: 'darwin', cpu: 'x64' },
    { goos: 'darwin', goarch: 'arm64', os: 'darwin', cpu: 'arm64' },
    { goos: 'linux', goarch: 'amd64', os: 'linux', cpu: 'x64' },
    { goos: 'linux', goarch: 'arm64', os: 'linux', cpu: 'arm64' },
  ].map((platform) => {
    const exe = platform.goos === 'windows';
    return Object.freeze({
      ...platform,
      exe,
      packageName: `${PLATFORM_SCOPE}/${platform.os}-${platform.cpu}`,
      binaryName: exe ? 'brain.exe' : 'brain',
    });
  }),
);

const VERSION_PATTERN = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/;
const CHECKSUM_LINE = /^([0-9a-fA-F]{64})\s+\*?(.+)$/;

export class BuildError extends Error {
  constructor(message) {
    super(message);
    this.name = 'BuildError';
  }
}

export function assertVersion(version) {
  if (typeof version !== 'string' || !VERSION_PATTERN.test(version)) {
    throw new BuildError(`invalid version "${version}": expected X.Y.Z or X.Y.Z-prerelease`);
  }
  return version;
}

export function assetFileName(assetVersion, platform) {
  const suffix = platform.exe ? '.exe' : '';
  return `brain_${assetVersion}_${platform.goos}_${platform.goarch}${suffix}`;
}

export function parseChecksums(text) {
  const sums = new Map();
  for (const line of text.split(/\r?\n/)) {
    const match = CHECKSUM_LINE.exec(line.trim());
    if (match) {
      sums.set(match[2], match[1].toLowerCase());
    }
  }
  return sums;
}

export function sha256Hex(data) {
  return createHash('sha256').update(data).digest('hex');
}

export function mainPackageJson(version) {
  return {
    name: MAIN_PACKAGE,
    version,
    description: 'LV24 Brain: a curated project memory CLI (local, git or server mode)',
    license: LICENSE,
    repository: { type: 'git', url: `git+${REPOSITORY_URL}.git` },
    homepage: `${REPOSITORY_URL}#readme`,
    bugs: { url: `${REPOSITORY_URL}/issues` },
    keywords: ['brain', 'cli', 'memory', 'knowledge', 'agents'],
    bin: { brain: 'bin/brain.js' },
    engines: { node: '>=18' },
    files: ['bin'],
    optionalDependencies: Object.fromEntries(
      PLATFORMS.map((platform) => [platform.packageName, version]),
    ),
    publishConfig: { access: 'public' },
  };
}

export function platformPackageJson(platform, version) {
  return {
    name: platform.packageName,
    version,
    description: `LV24 Brain CLI binary for ${platform.os}/${platform.cpu}`,
    license: LICENSE,
    repository: { type: 'git', url: `git+${REPOSITORY_URL}.git` },
    os: [platform.os],
    cpu: [platform.cpu],
    files: ['bin'],
    publishConfig: { access: 'public' },
  };
}

async function exists(filePath) {
  try {
    await access(filePath);
    return true;
  } catch {
    return false;
  }
}

async function readBinaries(distDir, assetVersion) {
  const checksumPath = path.join(distDir, 'checksums.txt');
  if (!(await exists(checksumPath))) {
    throw new BuildError(`checksums.txt not found in ${distDir}`);
  }
  const sums = parseChecksums(await readFile(checksumPath, 'utf8'));

  const binaries = [];
  for (const platform of PLATFORMS) {
    const assetName = assetFileName(assetVersion, platform);
    const assetPath = path.join(distDir, assetName);
    if (!(await exists(assetPath))) {
      throw new BuildError(`missing binary for ${platform.packageName}: ${assetName}`);
    }
    const expected = sums.get(assetName);
    if (expected === undefined) {
      throw new BuildError(`checksums.txt has no entry for ${assetName}`);
    }
    const data = await readFile(assetPath);
    const actual = sha256Hex(data);
    if (actual !== expected) {
      throw new BuildError(
        `checksum mismatch for ${assetName}: expected ${expected}, got ${actual}`,
      );
    }
    binaries.push({ platform, data });
  }
  return binaries;
}

async function writePackageFiles(pkgDir, packageJson, files) {
  await rm(pkgDir, { recursive: true, force: true });
  await mkdir(path.join(pkgDir, 'bin'), { recursive: true });
  await writeFile(
    path.join(pkgDir, 'package.json'),
    `${JSON.stringify(packageJson, null, 2)}\n`,
    'utf8',
  );
  for (const file of files) {
    const target = path.join(pkgDir, file.relPath);
    if (file.source) {
      await copyFile(file.source, target);
    } else {
      await writeFile(target, file.data);
    }
    if (file.executable && process.platform !== 'win32') {
      await chmod(target, 0o755);
    }
  }
  if (await exists(LICENSE_PATH)) {
    await copyFile(LICENSE_PATH, path.join(pkgDir, 'LICENSE'));
  }
}

// Verifies every binary first and writes nothing unless all of them pass.
export async function generatePackages({ version, assetVersion = version, distDir, outDir }) {
  assertVersion(version);
  assertVersion(assetVersion);
  const binaries = await readBinaries(distDir, assetVersion);

  const written = [];
  for (const { platform, data } of binaries) {
    const pkgDir = path.join(outDir, platform.packageName);
    await writePackageFiles(
      pkgDir,
      platformPackageJson(platform, version),
      [{ relPath: path.join('bin', platform.binaryName), data, executable: true }],
    );
    written.push(pkgDir);
  }

  const mainDir = path.join(outDir, MAIN_PACKAGE);
  await writePackageFiles(mainDir, mainPackageJson(version), [
    { relPath: path.join('bin', 'brain.js'), source: LAUNCHER_PATH, executable: true },
  ]);
  written.push(mainDir);
  return written;
}

function parseArgs(argv) {
  const args = {};
  for (let i = 0; i < argv.length; i += 2) {
    const key = argv[i];
    const value = argv[i + 1];
    if (!key?.startsWith('--') || value === undefined) {
      throw new BuildError(`expected --name value pairs, got "${key ?? ''}"`);
    }
    args[key.slice(2)] = value;
  }
  return args;
}

async function main(argv) {
  const args = parseArgs(argv);
  for (const required of ['version', 'dist', 'out']) {
    if (!args[required]) {
      throw new BuildError(`missing --${required}`);
    }
  }
  const written = await generatePackages({
    version: args.version,
    assetVersion: args['asset-version'] ?? args.version,
    distDir: path.resolve(args.dist),
    outDir: path.resolve(args.out),
  });
  for (const dir of written) {
    console.log(dir);
  }
}

const isEntry =
  process.argv[1] !== undefined &&
  import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href;

if (isEntry) {
  main(process.argv.slice(2)).catch((err) => {
    console.error(`npm/build.mjs: ${err.message}`);
    process.exitCode = err instanceof BuildError ? 1 : 2;
  });
}
