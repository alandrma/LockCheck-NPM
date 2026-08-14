#!/usr/bin/env node
import { readFileSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';

const USAGE = `
lockcheck.mjs — validate the claim "a dependency can't be upgraded due to a conflict"

USAGE:
  node lockcheck.mjs <package-lock.json> [options]

OPTIONS:
  --target <name>            Deep analysis of one dependency:
                             who uses it, with which version ranges,
                             and whether those ranges really block an upgrade.
  --upgrade <name>@<version> Simulate an upgrade: is the new version accepted
                             by ALL dependents per their ranges (semver)?
  --json                     Output as JSON (for scripting / diffing across versions).
  --no-tree                  Skip the multi-version (dedup) analysis for --target.

EXAMPLES:
  node lockcheck.mjs package-lock.json --target axios
  node lockcheck.mjs package-lock.json --upgrade axios@1.7.0
  node lockcheck.mjs package-lock.json --target axios --json
`.trim();

/* ------------------------- SEMVER MINIMAL ------------------------- */

function parseVersion(v) {
  const s = String(v);
  const m = s.trim().match(/^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$/);
  if (!m) return null;
  return { major: +m[1], minor: +m[2], patch: +m[3], pre: m[4] ?? '' };
}

function cmp(a, b) {
  if (a.major !== b.major) return a.major - b.major;
  if (a.minor !== b.minor) return a.minor - b.minor;
  if (a.patch !== b.patch) return a.patch - b.patch;
  if (a.pre === b.pre) return 0;
  if (a.pre === '') return 1;  // release > prerelease
  if (b.pre === '') return -1;
  return a.pre < b.pre ? -1 : 1;
}

function lt(a, b) { return cmp(a, b) < 0; }
function lte(a, b) { return cmp(a, b) <= 0; }
function gt(a, b) { return cmp(a, b) > 0; }
function gte(a, b) { return cmp(a, b) >= 0; }
function eq(a, b) { return cmp(a, b) === 0; }

// Split "||" into alternatives, each alternative is a space-separated comparator set.
function parseRange(range) {
  return String(range)
    .split(/\s*\|\|\s*/)
    .map(alt => alt.trim())
    .filter(Boolean)
    .map(alt =>
      alt.split(/\s+/).map(c => {
        const opMatch = c.match(/^(\^|~|>=|<=|>|<|=)?(.*)$/);
        return { op: opMatch[1] ?? '=', raw: opMatch[2] };
      })
    );
}

function satisfies(version, range) {
  const v = parseVersion(version);
  if (!v) return false;
  for (const alt of parseRange(range)) {
    if (alt.every(comp => comparatorOk(v, comp))) return true;
  }
  return false;
}

function comparatorOk(v, comp) {
  if (comp.raw === '' || comp.raw === '*' || comp.raw === 'x' || comp.raw === 'X') return true;
  const target = parseVersion(comp.raw.replace(/[xX]/g, '0'));
  if (!target) return false;

  // prerelease version only matches if comparator mentions prerelease (semver rule)
  if (v.pre !== '' && !comp.raw.includes('-') && comp.op !== '=') return false;

  switch (comp.op) {
    case '=': return eq(v, target);
    case '>': return gt(v, target);
    case '>=': return gte(v, target);
    case '<': return lt(v, target);
    case '<=': return lte(v, target);
    case '^': {
      if (target.major > 0) return gte(v, target) && v.major === target.major;
      if (target.minor > 0) return gte(v, target) && v.major === 0 && v.minor === target.minor;
      return gte(v, target) && v.major === 0 && v.minor === 0 && v.patch === target.patch;
    }
    case '~': {
      return gte(v, target) && v.major === target.major && v.minor === target.minor;
    }
    default: return false;
  }
}

/* ------------------------- LOCKFILE PARSING ------------------------- */

function parseLockfile(path) {
  const raw = JSON.parse(readFileSync(path, 'utf8'));
  const lv = raw.lockfileVersion ?? 1;
  const packages = new Map(); // key = install path ("node_modules/foo" or "node_modules/@s/p")

  if (typeof raw.packages === 'object' && raw.packages !== null) {
    for (const [k, v] of Object.entries(raw.packages)) {
      if (!v || typeof v !== 'object') continue;
      packages.set(k, { ...v, __path: k });
    }
    // attach root
    const root = packages.get('') ?? {};
    return { lockfileVersion: lv, packages, root };
  }

  // lockfileVersion 1 (dependencies tree)
  const root = { dependencies: {}, devDependencies: raw.devDependencies, ...(raw.packages ?? {}) };
  packages.set('', root);
  const flatten = (node, prefix) => {
    if (!node.dependencies) return;
    for (const [name, meta] of Object.entries(node.dependencies)) {
      const p = prefix ? `${prefix}/node_modules/${name}` : `node_modules/${name}`;
      packages.set(p, { ...meta, __path: p });
      if (meta.dependencies) flatten(meta, p);
    }
  };
  flatten(raw, '');
  return { lockfileVersion: lv, packages, root };
}

function nameFromPath(p) {
  const seg = p.split('/');
  const last = seg[seg.length - 1];
  if (last.startsWith('@')) return seg.slice(-2).join('/');
  return last;
}

function versionOf(entry) {
  return entry.version ?? entry.__version ?? '?';
}

/* ------------------------- REVERSE GRAPH ------------------------- */

function buildReverseGraph(lock) {
  // name -> [{ byPath, byName, range, kind, childPath }]
  const rev = new Map();
  const push = (name, info) => {
    if (!rev.has(name)) rev.set(name, []);
    rev.get(name).push(info);
  };

  const resolveNearest = (parentPath, name) => {
    let cur = parentPath;
    for (;;) {
      const candidate = cur === '' ? `node_modules/${name}` : `${cur}/node_modules/${name}`;
      if (lock.packages.has(candidate)) return candidate;
      if (cur === '') return null;
      const idx = cur.lastIndexOf('/node_modules/');
      cur = idx === -1 ? '' : cur.slice(0, idx);
    }
  };

  const walk = (parentPath) => {
    const entry = lock.packages.get(parentPath);
    if (!entry) return;
    const deps = lock.lockfileVersion === 1
      ? { ...(entry.requires ?? {}) }
      : {
          ...(entry.dependencies ?? {}),
          ...(entry.optionalDependencies ?? {}),
          ...(entry.devDependencies ?? {}),
          ...(entry.peerDependencies ?? {}),
        };
    for (const [name, range] of Object.entries(deps)) {
      const childPath = resolveNearest(parentPath, name);
      if (!childPath || !lock.packages.has(childPath)) continue;
      push(name, {
        byPath: parentPath,
        byName: parentPath === '' ? '(root)' : nameFromPath(parentPath),
        range: range ?? null,
        kind: parentPath === '' ? 'root' : 'dependencies',
        childPath,
      });
    }
  };

  for (const path of lock.packages.keys()) walk(path);
  return rev;
}

// All installed versions of a package name.
function installedVersions(lock, name) {
  const out = new Map(); // version -> [paths]
  for (const [path, entry] of lock.packages) {
    if (path === '') continue;
    if (nameFromPath(path) === name) {
      const v = versionOf(entry);
      if (!out.has(v)) out.set(v, []);
      out.get(v).push(path);
    }
  }
  return out;
}

/* ------------------------- ANALYZERS ------------------------- */

function pkgSummary(lock, name) {
  const versions = installedVersions(lock, name);
  const rev = buildReverseGraph(lock);
  const dependents = rev.get(name) ?? [];
  return {
    name,
    installedVersions: [...versions.keys()],
    versionCount: versions.size,
    dependents: dependents.map(d => ({
      from: d.byPath === '' ? '(root)' : d.byPath,
      range: d.range,
      resolved: d.range ? checkResolution(d.range, versions) : null,
    })),
  };
}

function checkResolution(range, versionsMap) {
  // Compare each installed version against the requested range.
  return [...versionsMap.keys()].map(v => ({ version: v, ok: satisfies(v, range) }));
}

// For a target version, which dependents would reject it (semver conflict)?
function simulateUpgrade(lock, name, newVersion) {
  const rev = buildReverseGraph(lock);
  const dependents = rev.get(name) ?? [];
  return {
    name,
    proposed: newVersion,
    accepted: dependents.filter(d => d.range == null || satisfies(newVersion, d.range)),
    rejected: dependents.filter(d => d.range != null && !satisfies(newVersion, d.range)),
    noRange: dependents.filter(d => d.range == null),
  };
}

// Detect stale/obsolete: for each package, if multiple installed versions exist,
// find which are "too old" relative to the newest installed major.
function staleScan(lock) {
  const names = new Set();
  for (const [path] of lock.packages) {
    if (path === '') continue;
    names.add(nameFromPath(path));
  }
  const rev = buildReverseGraph(lock);
  const stale = [];

  for (const name of names) {
    const versions = installedVersions(lock, name);
    if (versions.size <= 1) continue;

    const majors = new Set([...versions.keys()].map(v => parseVersion(v)?.major));
    const newest = [...versions.keys()].sort((a, b) => cmp(parseVersion(a), parseVersion(b))).pop();
    const newestMajor = parseVersion(newest)?.major;

    for (const [ver, paths] of versions) {
      const vv = parseVersion(ver);
      if (!vv) continue;
      const isNewest = ver === newest;
      if (isNewest) continue;
      const dependents = (rev.get(name) ?? []).filter(d => {
        return paths.some(p => d.childPath === p);
      });
      stale.push({
        name,
        version: ver,
        paths,
        newestMajor: parseVersion(newest)?.major,
        newest,
        gapFromNewest: [...versions.keys()].filter(v => v !== ver).length,
        dependents: dependents.map(d => ({
          from: d.byPath === '' ? '(root)' : d.byPath,
          range: d.range,
        })),
      });
    }
  }
  return stale;
}

// Check whether every dependent of the target's old version would accept the newest
// version satisfying their ranges — i.e. is the "too old" dep actually forced?
function canConsolidate(lock, name, oldVersion) {
  const versions = installedVersions(lock, name);
  const rev = buildReverseGraph(lock);
  const dependents = rev.get(name) ?? [];
  const relevant = dependents.filter(d => {
    return (versions.get(oldVersion) ?? []).some(p => d.childPath === p);
  });
  const candidates = [...versions.keys()].filter(v => v !== oldVersion)
    .sort((a, b) => cmp(parseVersion(a), parseVersion(b))).reverse();
  for (const cand of candidates) {
    const allOk = relevant.every(d => d.range == null || satisfies(cand, d.range));
    if (allOk) return { canConsolidate: true, to: cand };
  }
  return { canConsolidate: false, to: null };
}

/* ------------------------- OUTPUT ------------------------- */

function reportTarget(lock, name, { json } = {}) {
  const summary = pkgSummary(lock, name);
  const stale = staleScan(lock).filter(s => s.name === name);
  const consolidation = [];

  for (const s of stale) {
    consolidation.push({ ...s, consolidation: canConsolidate(lock, name, s.version) });
  }

  if (json) {
    return JSON.stringify({ target: summary, stale: consolidation }, null, 2);
  }

  const L = [];
  const line = (s = '') => L.push(s);
  const bar = '─'.repeat(64);

  line(bar);
  line(`TARGET: ${name}`);
  line(`  Installed versions : ${summary.installedVersions.join(', ')}`);
  line(`  Version count      : ${summary.versionCount}`);
  line(bar);
  line('');
  if (summary.dependents.length === 0) {
    line('  ❌ Nothing depends on this package (no dependents).');
    line('     So it CANNOT be the blocker of any upgrade.');
  } else {
    line('  Dependents (who uses it, and with what range):');
    line('');
    for (const d of summary.dependents) {
      const r = d.range ?? '(no range)';
      const okVersions = (d.resolved ?? []).filter(x => x.ok).map(x => x.version).join(', ');
      line(`    • ${d.from}`);
      line(`        range     : ${r}`);
      line(`        satisfied : ${okVersions || '(none)'}`);
      line('');
    }
  }

  if (stale.length) {
    line(bar);
    line('  VERSION GAP ANALYSIS (old version vs newest installed):');
    for (const s of consolidation) {
      line('');
      line(`  ● ${s.name}@${s.version}  (newest installed: ${s.newest})`);
      for (const d of s.dependents) {
        line(`      used by: ${d.from}  (${d.range ?? 'no range'})`);
      }
      if (s.consolidation.canConsolidate) {
        line(`      ✅ CAN be consolidated to ${s.consolidation.to} — all dependents`);
        line(`         of this old version accept the new one per their ranges.`);
        line(`         Meaning: the "can't upgrade due to conflict" claim is NOT proven`);
        line(`         (it's reluctance, not a semver conflict).`);
      } else {
        line(`      ❌ Consolidation to another version is impossible without changing ranges:`);
        line(`         at least one dependent's range rejects every other installed version.`);
        line(`         (A REAL semver conflict — check the dependents' ranges above.)`);
      }
    }
  }
  return L.join('\n');
}

function reportUpgrade(lock, target, { json } = {}) {
  const [name, ver] = target.split('@');
  if (!ver) return 'ERROR: --upgrade format must be <name>@<version>. Example: --upgrade axios@1.7.0';
  const res = simulateUpgrade(lock, name, ver);
  if (json) return JSON.stringify(res, null, 2);

  const L = [];
  const line = (s = '') => L.push(s);
  const bar = '─'.repeat(64);
  line(bar);
  line(`UPGRADE SIMULATION: ${name} → ${ver}`);
  line(bar);
  line('');
  if (res.accepted.length) {
    line(`  ✅ ACCEPTED by ${res.accepted.length} dependent(s):`);
    for (const d of res.accepted) {
      line(`      • ${d.byPath === '' ? '(root)' : d.byPath}  (${d.range ?? 'no range'})`);
    }
    line('');
  }
  if (res.rejected.length) {
    line(`  ❌ REJECTED by ${res.rejected.length} dependent(s) (range does not allow ${ver}):`);
    const rootRej = res.rejected.filter(d => d.byPath === '');
    const transRej = res.rejected.filter(d => d.byPath !== '');
    for (const d of res.rejected) {
      const src = d.byPath === '' ? '(root / project package.json)' : d.byPath;
      line(`      • ${src}  (${d.range})`);
    }
    line('');
    if (rootRej.length) {
      line('  ⚠️  IMPORTANT: the main rejection comes from ROOT (project package.json).');
      line('     Root ranges are CONTROLLED by the developer — changeable at any time,');
      line('     there is no technical "conflict" here. The "can\'t upgrade" claim is weak.');
      line('');
    }
    if (transRej.length) {
      line('  Transitive rejections (from other dependencies):');
      const byRange = new Map();
      for (const d of transRej) {
        if (!byRange.has(d.range)) byRange.set(d.range, []);
        byRange.get(d.range).push(d.byName);
      }
      for (const [r, pkgs] of byRange) {
        line(`      range "${r}" declared by: ${[...new Set(pkgs)].join(', ')}`);
      }
      line('');
      line('  Next validation step (needs npm registry access):');
      line('     npm view <name> versions  → check the actual latest version & "version gap"');
      line('     then: npm install <name>@<latest> --save-exact  → real test');
    }
  } else {
    line(`  ✅ Version ${ver} is ACCEPTED by ALL dependents per their ranges.`);
    line('     No semver conflict blocks this upgrade.');
    line('     If the developer claims a "conflict", ask them to show the rejecting range.');
  }
  return L.join('\n');
}

function reportDefault(lock, { json } = {}) {
  const stale = staleScan(lock);
  if (json) return JSON.stringify({ stale }, null, 2);

  const L = [];
  const line = (s = '') => L.push(s);
  const bar = '─'.repeat(64);
  line(bar);
  line(`SCAN: ${lock.lockfileVersion >= 2 ? `lockfile v${lock.lockfileVersion}` : 'lockfile v1'}`);
  line(`Installed packages: ${lock.packages.size}`);
  line(`Multi-version packages (dedup opportunity / sign of outdated deps): ${stale.length}`);
  line(bar);
  line('');
  if (!stale.length) {
    line('  ✅ No package is installed at multiple versions at once.');
    line('     (No sign of a "stuck obsolete" dependency.)');
    return L.join('\n');
  }
  for (const s of stale.slice(0, 60)) {
    line(`  ● ${s.name}: ${s.version}  (another major in tree: ${s.newestMajor})`);
    for (const d of s.dependents.slice(0, 5)) {
      line(`      used by: ${d.from}  (${d.range ?? 'no range'})`);
    }
    if (s.dependents.length > 5) line(`      ... and ${s.dependents.length - 5} more`);
    line('');
  }
  if (stale.length > 60) line(`  ... and ${stale.length - 60} more packages (use --json for full output)`);
  line('');
  line('  Tip: node lockcheck.mjs <lock> --target <name>  for per-package details.');
  return L.join('\n');
}

/* ------------------------- MAIN ------------------------- */

function main() {
  const argv = process.argv.slice(2);
  const json = argv.includes('--json');
  const noTree = argv.includes('--no-tree');
  const iTarget = argv.indexOf('--target');
  const iUpgrade = argv.indexOf('--upgrade');
  const lockArg = argv.find(a => !a.startsWith('--')) ?? 'package-lock.json';

  const target = iTarget !== -1 ? argv[iTarget + 1] : null;
  const upgrade = iUpgrade !== -1 ? argv[iUpgrade + 1] : null;

  if (argv.includes('--help') || argv.includes('-h')) {
    console.log(USAGE);
    process.exit(0);
  }

  let lock;
  try {
    lock = parseLockfile(resolve(lockArg));
  } catch (e) {
    console.error(`❌ Failed to read ${lockArg}: ${e.message}`);
    console.error(USAGE);
    process.exit(1);
  }

  let out;
  if (upgrade) out = reportUpgrade(lock, upgrade, { json });
  else if (target) out = reportTarget(lock, target, { json, noTree });
  else out = reportDefault(lock, { json });

  console.log(out);
}

main();
