# lockcheck

**Validate the "we can't upgrade that dependency" claim — from `package-lock.json` alone.**

Every developer has heard (or made) this excuse:

> *"We can't upgrade `axios` — it conflicts with the obsolete `legacy-ui` package,
> and we can't upgrade `legacy-ui` because the version gap is too far."*

`lockcheck` tells you whether that excuse is **technically real (semver conflict)**
or just **reluctance to update**. No network access, no `npm install`, no
`node_modules` needed — just your `package-lock.json`.

---

## Why

Dependency rot hides behind plausible-sounding stories. A lockfile records exactly
*who depends on what, and with which version range* — that's everything needed to
check whether a "conflict" actually blocks an upgrade, or whether the blocker is
just a range the developer controls (and chose not to widen).

`lockcheck` answers the three questions that come up in every such discussion:

| Question | Command |
|---|---|
| Will version `X@newver` be accepted by every dependent, per their ranges? | `--upgrade X@newver` |
| Who actually pulls in the "obsolete" package, and with what range? | `--target Y` |
| Is the "version gap too far" claim real, or can old versions be consolidated? | `--target Y` |

---

## Features

- **Zero dependencies** — pure Go, stdlib only. Compiles to a single static
  binary; the original `.mjs` version (stock Node.js `>= 18`) is kept alongside
  for reference.
- **Works offline** — only reads `package-lock.json`. No registry, no network.
- **Interactive HTML graphs** — `--html` exports a standalone dependency graph
  (SVG + inline JS, no CDN/libraries) with zoom, pan, tooltips, search, and
  accept/reject edge coloring in `--upgrade` mode.
- **Supports all lockfile formats** — npm `lockfileVersion` 1, 2, and 3.
- **Reverse dependency graph** — reconstructed from the lockfile, not from a live tree.
- **Semver simulation** — proposed versions are tested against *every* dependent's
  range (`^`, `~`, `>=`, `<=`, `>`, `<`, exact, `||` unions, wildcards).
- **Root-aware verdicts** — a rejection that originates from the project's own
  `package.json` is flagged as *developer-controlled*, not a technical conflict.
- **Consolidation analysis** — detects packages installed at multiple versions and
  reports whether the old copies could be merged into a newer one.
- **JSON output** — `--json` for scripting, CI, or diffing across lockfiles.

---

## Build & usage

```bash
# Build the binary (Go >= 1.22, no external dependencies)
go build -o lockcheck .

# General scan: packages installed at multiple versions (markers of rot)
./lockcheck package-lock.json

# Deep-dive on one package: who uses it, with what range
./lockcheck package-lock.json --target jquery

# Simulate an upgrade: is <name>@<version> accepted by all dependents?
./lockcheck package-lock.json --upgrade axios@1.7.0

# Same, as JSON for scripting
./lockcheck package-lock.json --upgrade axios@1.7.0 --json

# Save an interactive dependency graph as a standalone HTML file
./lockcheck package-lock.json --target jquery --html graph.html
./lockcheck package-lock.json --upgrade jquery@3.7.1 --html graph.html
```

The `--html` output is a **fully self-contained HTML file** — no external
libraries, no CDN, works offline. It renders the reverse dependency chain around
a package (who uses it, with what ranges) as an interactive SVG graph with:

- zoom (scroll) and pan (drag)
- hover tooltips showing name / installed versions / level
- a search box to highlight packages
- in `--upgrade` mode: **green edges** = range accepts the new version,
  **red edges** = range rejects it

Without `--target`/`--upgrade`, `--html` renders the outdated (multi-version)
packages and which packages pull the old copy.

Scan a whole fleet of projects:

```bash
for f in */package-lock.json; do
  echo "== $f =="
  ./lockcheck "$f"
done
```

---

## How it works

1. **Parse** — reads the `packages` map (lockfile v2/v3) or the nested
   `dependencies` / `requires` tree (lockfile v1).
2. **Rebuild the reverse graph** — for every package, records each dependent and
   the exact version range it requests. Resolution follows npm's real algorithm:
   nearest `node_modules/<name>` walking up the path.
3. **Simulate** — `--upgrade` tests the proposed version against every dependent's
   range using an embedded semver matcher. A rejection is only *real* if a range
   actually refuses the version.
4. **Consolidate** — when a package exists at multiple versions, checks whether
   every dependent of the old copy would accept a newer installed version.
   If yes, the "conflict" is **not proven** — it's inertia, not semver.
5. **Classify** — rejections are split into **root** (project's own `package.json`,
   freely editable) vs **transitive** (pinned by other dependencies).

---

## Example output

```text
SIMULASI UPGRADE: axios → 1.7.0
────────────────────────────────────────────────────────────────
  ❌ DITOLAK oleh 1 dependent (range tidak mengizinkan 1.7.0):
      • (root / package.json proyek)  (^0.21.0)

  ⚠️  PENTING: penolakan TERBESAR berasal dari ROOT (package.json proyek).
     Range di root DIKONTROL oleh developer sendiri — bisa diubah kapan saja,
     tidak ada "konflik" teknis di sini. Klaim "tidak bisa di-upgrade" lemah.
```

```text
● jquery@1.12.4  (terbaru yang terpasang: 3.7.1)
      dipakai oleh: node_modules/legacy-ui  (~1.12.4)
      ✅ BISA di-konsolidasi ke 3.7.1 — semua dependents dari versi lama ini
         menerima versi baru tsb menurut range-nya.
         Artinya: klaim "tidak bisa di-upgrade karena konflik" TIDAK terbukti
         (hanya soal mau/enggan, bukan konflik semver).
```

---

## Honest limitations

`lockcheck` proves what the **semver ranges recorded in the lockfile** allow. It
cannot see:

- The **latest actual version** on the npm registry. For the "version gap too far"
  claim, complement with:
  ```bash
  npm view <name> versions     # actual latest & how far the gap really is
  npm view <name> engines      # does the new version require a newer Node?
  npm outdated                 # gaps in your own project
  ```
- **Runtime / API breakage** — a version that satisfies every range can still break
  behavior. `lockcheck` is a *first-pass filter* to force a real technical answer,
  not a substitute for `npm install <name>@<newver>` and running your test suite.

A loose range (`^1.2.3`) that resolves to a very old version is a sign the
lockfile hasn't been refreshed — not a technical conflict.

---

## Requirements

- Go `>= 1.22` to build (`go build -o lockcheck .`). The compiled binary has no
  runtime dependencies.
- Alternatively, the reference implementation `lockcheck.mjs` needs Node.js
  `>= 18` and nothing else.

---

## Roadmap

- [ ] Query npm registry (`--online`) to compare installed vs latest actual versions
- [ ] Export graph as PNG/SVG file (currently HTML/SVG-in-HTML only)
- [ ] `--json` full-machine-readable report with exit codes for CI gates
- [ ] Support for `npm-shrinkwrap.json` and `pnpm-lock.yaml`

---

## License

MIT
