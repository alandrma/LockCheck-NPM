#!/usr/bin/env node
import { readFileSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';

const USAGE = `
lockcheck.mjs — validasi klaim "dependensi tidak bisa di-upgrade karena konflik"

USAGE:
  node lockcheck.mjs <package-lock.json> [opsi]

OPSI:
  --target <nama>            Analisis mendalam satu dependensi:
                             siapa yang memakainya, dengan range versi apa,
                             apakah range-nya benar-benar membatasi upgrade.
  --upgrade <nama>@<versi>   Simulasi upgrade: apakah versi baru tsb diterima
                             oleh SEMUA dependents sesuai range mereka (semver).
  --json                     Output sebagai JSON (untuk scripting / diff antar versi).
  --no-tree                  Skip analisis multi-version (dedup) pada --target.

CONTOH:
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
  line(`  Versi terpasang : ${summary.installedVersions.join(', ')}`);
  line(`  Jumlah versi    : ${summary.versionCount}`);
  line(bar);
  line('');
  if (summary.dependents.length === 0) {
    line('  ❌ Tidak ada yang memakai paket ini (tidak ada dependents).');
    line('     Artinya paket ini TIDAK menjadi penghalang upgrade apa pun.');
  } else {
    line('  Dependents (siapa yang memakai, dan dengan range berapa):');
    line('');
    for (const d of summary.dependents) {
      const r = d.range ?? '(tanpa range)';
      const okVersions = (d.resolved ?? []).filter(x => x.ok).map(x => x.version).join(', ');
      line(`    • ${d.from}`);
      line(`        range     : ${r}`);
      line(`        terpenuhi : ${okVersions || '(tidak ada)'}`);
      line('');
    }
  }

  if (stale.length) {
    line(bar);
    line('  ANALISIS KESENJANGAN VERSI (old version vs newest installed):');
    for (const s of consolidation) {
      line('');
      line(`  ● ${s.name}@${s.version}  (terbaru yang terpasang: ${s.newest})`);
      for (const d of s.dependents) {
        line(`      dipakai oleh: ${d.from}  (${d.range ?? 'tanpa range'})`);
      }
      if (s.consolidation.canConsolidate) {
        line(`      ✅ BISA di-konsolidasi ke ${s.consolidation.to} — semua dependents`);
        line(`         dari versi lama ini menerima versi baru tsb menurut range-nya.`);
        line(`         Artinya: klaim "tidak bisa di-upgrade karena konflik" TIDAK terbukti`);
        line(`         (hanya soal mau/enggan, bukan konflik semver).`);
      } else {
        line(`      ❌ Konsolidasi ke versi lain TIDAK mungkin tanpa mengubah range:`);
        line(`         setidaknya satu dependent range-nya menolak semua versi lain yang terpasang.`);
        line(`         (Konflik semver NYATA — cek range dependents di atas.)`);
      }
    }
  }
  return L.join('\n');
}

function reportUpgrade(lock, target, { json } = {}) {
  const [name, ver] = target.split('@');
  if (!ver) return 'ERROR: format --upgrade harus <nama>@<versi>. Contoh: --upgrade axios@1.7.0';
  const res = simulateUpgrade(lock, name, ver);
  if (json) return JSON.stringify(res, null, 2);

  const L = [];
  const line = (s = '') => L.push(s);
  const bar = '─'.repeat(64);
  line(bar);
  line(`SIMULASI UPGRADE: ${name} → ${ver}`);
  line(bar);
  line('');
  if (res.accepted.length) {
    line(`  ✅ DITERIMA oleh ${res.accepted.length} dependent:`);
    for (const d of res.accepted) {
      line(`      • ${d.byPath === '' ? '(root)' : d.byPath}  (${d.range ?? 'tanpa range'})`);
    }
    line('');
  }
  if (res.rejected.length) {
    line(`  ❌ DITOLAK oleh ${res.rejected.length} dependent (range tidak mengizinkan ${ver}):`);
    const rootRej = res.rejected.filter(d => d.byPath === '');
    const transRej = res.rejected.filter(d => d.byPath !== '');
    for (const d of res.rejected) {
      const src = d.byPath === '' ? '(root / package.json proyek)' : d.byPath;
      line(`      • ${src}  (${d.range})`);
    }
    line('');
    if (rootRej.length) {
      line('  ⚠️  PENTING: penolakan TERBESAR berasal dari ROOT (package.json proyek).');
      line('     Range di root DIKONTROL oleh developer sendiri — bisa diubah kapan saja,');
      line('     tidak ada "konflik" teknis di sini. Klaim "tidak bisa di-upgrade" lemah.');
      line('');
    }
    if (transRej.length) {
      line('  Penolakan transitif (dari dependensi lain):');
      const byRange = new Map();
      for (const d of transRej) {
        if (!byRange.has(d.range)) byRange.set(d.range, []);
        byRange.get(d.range).push(d.byName);
      }
      for (const [r, pkgs] of byRange) {
        line(`      range "${r}" dinyatakan oleh: ${[...new Set(pkgs)].join(', ')}`);
      }
      line('');
      line('  Langkah validasi berikutnya (butuh akses npm registry):');
      line('     npm view <nama> versions   → cek versi terbaru aktual & "jarak versi"');
      line('     lalu: npm install <nama>@<versi-terbaru> --save-exact  → uji nyata');
    }
  } else {
    line(`  ✅ Versi ${ver} DITERIMA oleh SEMUA dependents sesuai range mereka.`);
    line('     Tidak ada konflik semver yang menghalangi upgrade ini.');
    line('     Jika developer bilang "konflik", minta mereka menunjukkan range yang menolak.');
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
  line(`Paket terpasang: ${lock.packages.size}`);
  line(`Paket multi-versi (dedup opportunity / tanda dependensi usang): ${stale.length}`);
  line(bar);
  line('');
  if (!stale.length) {
    line('  ✅ Tidak ada paket yang terpasang dalam beberapa versi sekaligus.');
    line('     (Tidak ada tanda dependensi "obsolete yang menempel".)');
    return L.join('\n');
  }
  for (const s of stale.slice(0, 60)) {
    line(`  ● ${s.name}: ${s.version}  (mayor lain di tree: ${s.newestMajor})`);
    for (const d of s.dependents.slice(0, 5)) {
      line(`      dipakai oleh: ${d.from}  (${d.range ?? 'tanpa range'})`);
    }
    if (s.dependents.length > 5) line(`      ... dan ${s.dependents.length - 5} lagi`);
    line('');
  }
  if (stale.length > 60) line(`  ... dan ${stale.length - 60} paket lain (gunakan --json untuk lengkap)`);
  line('');
  line('  Gunakan: node lockcheck.mjs <lock> --target <nama>  untuk detail per paket.');
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
    console.error(`❌ Gagal membaca ${lockArg}: ${e.message}`);
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
