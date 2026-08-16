package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const USAGE = `
lockcheck — validate the claim "a dependency can't be upgraded due to a conflict"

USAGE:
  lockcheck <package-lock.json> [options]

OPTIONS:
  --target <name>            Deep analysis of one dependency:
                             who uses it, with which version ranges,
                             and whether those ranges really block an upgrade.
  --upgrade <name>@<version> Simulate an upgrade: is the new version accepted
                             by ALL dependents per their ranges (semver)?
  --json                     Output as JSON (for scripting / diffing across versions).
  --no-tree                  Skip the multi-version (dedup) analysis for --target.
  --html <file>              Save an interactive dependency graph as a standalone HTML
                             file (no external libraries needed). Works with or without
                             --target / --upgrade.

EXAMPLES:
  lockcheck package-lock.json --target axios
  lockcheck package-lock.json --upgrade axios@1.7.0
  lockcheck package-lock.json --target axios --json
  lockcheck package-lock.json --target jquery --html graph.html
  lockcheck package-lock.json --upgrade jquery@3.7.1 --html graph.html
`

/* ------------------------- SEMVER MINIMAL ------------------------- */

type Version struct {
	Major int
	Minor int
	Patch int
	Pre   string
}

var versionRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

func parseVersion(v string) *Version {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return nil
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	pre := ""
	if len(m) > 4 {
		pre = m[4]
	}
	return &Version{Major: major, Minor: minor, Patch: patch, Pre: pre}
}

func cmpVersion(a, b *Version) int {
	if a.Major != b.Major {
		return a.Major - b.Major
	}
	if a.Minor != b.Minor {
		return a.Minor - b.Minor
	}
	if a.Patch != b.Patch {
		return a.Patch - b.Patch
	}
	if a.Pre == b.Pre {
		return 0
	}
	if a.Pre == "" {
		return 1
	}
	if b.Pre == "" {
		return -1
	}
	if a.Pre < b.Pre {
		return -1
	}
	return 1
}

func ltVersion(a, b *Version) bool  { return cmpVersion(a, b) < 0 }
func lteVersion(a, b *Version) bool { return cmpVersion(a, b) <= 0 }
func gtVersion(a, b *Version) bool  { return cmpVersion(a, b) > 0 }
func gteVersion(a, b *Version) bool { return cmpVersion(a, b) >= 0 }
func eqVersion(a, b *Version) bool  { return cmpVersion(a, b) == 0 }

type Comparator struct {
	Op  string
	Raw string
}

var opRe = regexp.MustCompile(`^(\^|~|>=|<=|>|<|=)?(.*)$`)

func parseRange(rangeStr string) [][]Comparator {
	var result [][]Comparator
	for _, alt := range strings.Split(rangeStr, "||") {
		alt = strings.TrimSpace(alt)
		if alt == "" {
			continue
		}
		var comps []Comparator
		for _, c := range strings.Fields(alt) {
			m := opRe.FindStringSubmatch(c)
			op := "="
			raw := ""
			if m != nil {
				if m[1] != "" {
					op = m[1]
				}
				raw = m[2]
			}
			comps = append(comps, Comparator{Op: op, Raw: raw})
		}
		result = append(result, comps)
	}
	return result
}

func satisfies(version, rangeStr string) bool {
	v := parseVersion(version)
	if v == nil {
		return false
	}
	for _, alt := range parseRange(rangeStr) {
		ok := true
		for _, comp := range alt {
			if !comparatorOk(v, comp) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func comparatorOk(v *Version, comp Comparator) bool {
	if comp.Raw == "" || comp.Raw == "*" || comp.Raw == "x" || comp.Raw == "X" {
		return true
	}
	repl := strings.NewReplacer("x", "0", "X", "0")
	target := parseVersion(repl.Replace(comp.Raw))
	if target == nil {
		return false
	}

	if v.Pre != "" && !strings.Contains(comp.Raw, "-") && comp.Op != "=" {
		return false
	}

	switch comp.Op {
	case "=":
		return eqVersion(v, target)
	case ">":
		return gtVersion(v, target)
	case ">=":
		return gteVersion(v, target)
	case "<":
		return ltVersion(v, target)
	case "<=":
		return lteVersion(v, target)
	case "^":
		if target.Major > 0 {
			return gteVersion(v, target) && v.Major == target.Major
		}
		if target.Minor > 0 {
			return gteVersion(v, target) && v.Major == 0 && v.Minor == target.Minor
		}
		return gteVersion(v, target) && v.Major == 0 && v.Minor == 0 && v.Patch == target.Patch
	case "~":
		return gteVersion(v, target) && v.Major == target.Major && v.Minor == target.Minor
	default:
		return false
	}
}

func sortVersions(keys []string) {
	sort.Slice(keys, func(i, j int) bool {
		a, b := parseVersion(keys[i]), parseVersion(keys[j])
		if a == nil {
			return false
		}
		if b == nil {
			return true
		}
		return cmpVersion(a, b) < 0
	})
}

func sortedVersionKeys(versions map[string][]string) []string {
	keys := make([]string, 0, len(versions))
	for v := range versions {
		keys = append(keys, v)
	}
	sortVersions(keys)
	return keys
}

/* ------------------------- LOCKFILE PARSING ------------------------- */

type PkgEntry struct {
	Path                 string
	Version              string
	Dependencies         map[string]string
	OptionalDependencies map[string]string
	DevDependencies      map[string]string
	PeerDependencies     map[string]string
	Requires             map[string]string
}

type Lockfile struct {
	LockfileVersion int
	Packages        map[string]*PkgEntry
	Root            *PkgEntry
}

type rawPkg struct {
	Version              string            `json:"version"`
	Dependencies         map[string]string `json:"dependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
	Requires             map[string]string `json:"requires"`
}

type rawDep1 struct {
	Version      string              `json:"version"`
	Dependencies map[string]*rawDep1 `json:"dependencies"`
	Requires     map[string]string   `json:"requires"`
	Dev          bool                `json:"dev"`
}

type rawLock struct {
	LockfileVersion int                 `json:"lockfileVersion"`
	Packages        map[string]*rawPkg  `json:"packages"`
	Dependencies    map[string]*rawDep1 `json:"dependencies"`
	DevDependencies map[string]string   `json:"devDependencies"`
}

func parseLockfile(path string) (*Lockfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw rawLock
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	lv := raw.LockfileVersion
	if lv == 0 {
		lv = 1
	}
	lock := &Lockfile{LockfileVersion: lv, Packages: map[string]*PkgEntry{}}

	if raw.Packages != nil {
		for k, v := range raw.Packages {
			if v == nil {
				continue
			}
			lock.Packages[k] = &PkgEntry{
				Path:                 k,
				Version:              v.Version,
				Dependencies:         v.Dependencies,
				OptionalDependencies: v.OptionalDependencies,
				DevDependencies:      v.DevDependencies,
				PeerDependencies:     v.PeerDependencies,
				Requires:             v.Requires,
			}
		}
		lock.Root = lock.Packages[""]
		return lock, nil
	}

	root := &PkgEntry{Path: "", Dependencies: map[string]string{}}
	if raw.DevDependencies != nil {
		root.DevDependencies = raw.DevDependencies
	}
	lock.Packages[""] = root
	var flatten func(node *rawDep1, prefix string)
	flatten = func(node *rawDep1, prefix string) {
		if node.Dependencies == nil {
			return
		}
		for name, meta := range node.Dependencies {
			p := "node_modules/" + name
			if prefix != "" {
				p = prefix + "/node_modules/" + name
			}
			lock.Packages[p] = &PkgEntry{
				Path:     p,
				Version:  meta.Version,
				Requires: meta.Requires,
			}
			if meta.Dependencies != nil {
				flatten(meta, p)
			}
		}
	}
	flatten(&rawDep1{Dependencies: raw.Dependencies}, "")
	return lock, nil
}

func nameFromPath(p string) string {
	seg := strings.Split(p, "/")
	last := seg[len(seg)-1]
	if strings.HasPrefix(last, "@") {
		if len(seg) >= 2 {
			return strings.Join(seg[len(seg)-2:], "/")
		}
	}
	return last
}

func versionOf(entry *PkgEntry) string {
	if entry != nil && entry.Version != "" {
		return entry.Version
	}
	return "?"
}

/* ------------------------- REVERSE GRAPH ------------------------- */

type Dependent struct {
	ByPath    string  `json:"byPath"`
	ByName    string  `json:"byName"`
	Range     *string `json:"range"`
	Kind      string  `json:"kind"`
	ChildPath string  `json:"childPath"`
}

func mergeDeps(entry *PkgEntry) map[string]string {
	deps := map[string]string{}
	for k, v := range entry.Dependencies {
		deps[k] = v
	}
	for k, v := range entry.OptionalDependencies {
		deps[k] = v
	}
	for k, v := range entry.DevDependencies {
		deps[k] = v
	}
	for k, v := range entry.PeerDependencies {
		deps[k] = v
	}
	return deps
}

func buildReverseGraph(lock *Lockfile) map[string][]Dependent {
	rev := make(map[string][]Dependent)

	resolveNearest := func(parentPath, name string) string {
		cur := parentPath
		for {
			candidate := "node_modules/" + name
			if cur != "" {
				candidate = cur + "/node_modules/" + name
			}
			if _, ok := lock.Packages[candidate]; ok {
				return candidate
			}
			if cur == "" {
				return ""
			}
			idx := strings.LastIndex(cur, "/node_modules/")
			if idx == -1 {
				cur = ""
			} else {
				cur = cur[:idx]
			}
		}
	}

	for path, entry := range lock.Packages {
		if entry == nil {
			continue
		}
		var deps map[string]string
		if lock.LockfileVersion == 1 {
			deps = entry.Requires
		} else {
			deps = mergeDeps(entry)
		}
		for name, r := range deps {
			childPath := resolveNearest(path, name)
			if childPath == "" {
				continue
			}
			if _, ok := lock.Packages[childPath]; !ok {
				continue
			}
			byName := nameFromPath(path)
			kind := "dependencies"
			if path == "" {
				byName = "(root)"
				kind = "root"
			}
			rr := r
			rev[name] = append(rev[name], Dependent{
				ByPath:    path,
				ByName:    byName,
				Range:     &rr,
				Kind:      kind,
				ChildPath: childPath,
			})
		}
	}
	return rev
}

func installedVersions(lock *Lockfile, name string) map[string][]string {
	out := map[string][]string{}
	for path, entry := range lock.Packages {
		if path == "" {
			continue
		}
		if nameFromPath(path) == name {
			v := versionOf(entry)
			out[v] = append(out[v], path)
		}
	}
	return out
}

/* ------------------------- ANALYZERS ------------------------- */

type VersionCheck struct {
	Version string `json:"version"`
	OK      bool   `json:"ok"`
}

type SummaryDependent struct {
	From     string         `json:"from"`
	Range    *string        `json:"range"`
	Resolved []VersionCheck `json:"resolved"`
}

type PkgSummary struct {
	Name              string             `json:"name"`
	InstalledVersions []string           `json:"installedVersions"`
	VersionCount      int                `json:"versionCount"`
	Dependents        []SummaryDependent `json:"dependents"`
}

func pkgSummary(lock *Lockfile, name string) PkgSummary {
	versions := installedVersions(lock, name)
	rev := buildReverseGraph(lock)
	dependents := rev[name]
	deps := []SummaryDependent{}
	for _, d := range dependents {
		from := d.ByPath
		if d.ByPath == "" {
			from = "(root)"
		}
		sd := SummaryDependent{From: from, Range: d.Range}
		if d.Range != nil && *d.Range != "" {
			sd.Resolved = checkResolution(*d.Range, versions)
		}
		deps = append(deps, sd)
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].From < deps[j].From })
	return PkgSummary{
		Name:              name,
		InstalledVersions: sortedVersionKeys(versions),
		VersionCount:      len(versions),
		Dependents:        deps,
	}
}

func checkResolution(rangeStr string, versions map[string][]string) []VersionCheck {
	keys := sortedVersionKeys(versions)
	var out []VersionCheck
	for _, v := range keys {
		out = append(out, VersionCheck{Version: v, OK: satisfies(v, rangeStr)})
	}
	return out
}

type UpgradeResult struct {
	Name     string      `json:"name"`
	Proposed string      `json:"proposed"`
	Accepted []Dependent `json:"accepted"`
	Rejected []Dependent `json:"rejected"`
	NoRange  []Dependent `json:"noRange"`
}

func sortDependents(deps []Dependent) {
	sort.Slice(deps, func(i, j int) bool { return deps[i].ByPath < deps[j].ByPath })
}

func simulateUpgrade(lock *Lockfile, name, newVersion string) UpgradeResult {
	dependents := buildReverseGraph(lock)[name]
	res := UpgradeResult{
		Name:     name,
		Proposed: newVersion,
		Accepted: []Dependent{},
		Rejected: []Dependent{},
		NoRange:  []Dependent{},
	}
	for _, d := range dependents {
		if d.Range == nil || satisfies(newVersion, *d.Range) {
			res.Accepted = append(res.Accepted, d)
		} else {
			res.Rejected = append(res.Rejected, d)
		}
		if d.Range == nil {
			res.NoRange = append(res.NoRange, d)
		}
	}
	sortDependents(res.Accepted)
	sortDependents(res.Rejected)
	sortDependents(res.NoRange)
	return res
}

type DepInfo struct {
	From  string  `json:"from"`
	Range *string `json:"range"`
}

type Stale struct {
	Name          string    `json:"name"`
	Version       string    `json:"version"`
	Paths         []string  `json:"paths"`
	NewestMajor   int       `json:"newestMajor"`
	Newest        string    `json:"newest"`
	GapFromNewest int       `json:"gapFromNewest"`
	Dependents    []DepInfo `json:"dependents"`
}

func staleScan(lock *Lockfile) []Stale {
	nameSet := map[string]bool{}
	for path := range lock.Packages {
		if path == "" {
			continue
		}
		nameSet[nameFromPath(path)] = true
	}
	rev := buildReverseGraph(lock)
	stale := []Stale{}

	for name := range nameSet {
		versions := installedVersions(lock, name)
		if len(versions) <= 1 {
			continue
		}
		keys := make([]string, 0, len(versions))
		for v := range versions {
			keys = append(keys, v)
		}
		sortVersions(keys)
		newest := keys[len(keys)-1]
		newestMajor := 0
		if nv := parseVersion(newest); nv != nil {
			newestMajor = nv.Major
		}

		for _, ver := range keys {
			if ver == newest {
				continue
			}
			paths := versions[ver]
			var dependents []DepInfo
			for _, d := range rev[name] {
				for _, p := range paths {
					if d.ChildPath == p {
						from := d.ByPath
						if d.ByPath == "" {
							from = "(root)"
						}
						dependents = append(dependents, DepInfo{From: from, Range: d.Range})
						break
					}
				}
			}
			sort.Slice(dependents, func(i, j int) bool { return dependents[i].From < dependents[j].From })
			stale = append(stale, Stale{
				Name:          name,
				Version:       ver,
				Paths:         paths,
				NewestMajor:   newestMajor,
				Newest:        newest,
				GapFromNewest: len(keys) - 1,
				Dependents:    dependents,
			})
		}
	}

	sort.Slice(stale, func(i, j int) bool {
		if stale[i].Name != stale[j].Name {
			return stale[i].Name < stale[j].Name
		}
		a, b := parseVersion(stale[i].Version), parseVersion(stale[j].Version)
		if a == nil {
			return false
		}
		if b == nil {
			return true
		}
		return cmpVersion(a, b) < 0
	})
	return stale
}

type ConsolidationResult struct {
	CanConsolidate bool    `json:"canConsolidate"`
	To             *string `json:"to"`
}

func canConsolidate(lock *Lockfile, name, oldVersion string) ConsolidationResult {
	versions := installedVersions(lock, name)
	rev := buildReverseGraph(lock)
	var relevant []Dependent
	for _, d := range rev[name] {
		for _, p := range versions[oldVersion] {
			if d.ChildPath == p {
				relevant = append(relevant, d)
				break
			}
		}
	}
	var candidates []string
	for v := range versions {
		if v != oldVersion {
			candidates = append(candidates, v)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := parseVersion(candidates[i]), parseVersion(candidates[j])
		if a == nil {
			return false
		}
		if b == nil {
			return true
		}
		return cmpVersion(a, b) > 0
	})
	for _, cand := range candidates {
		allOk := true
		for _, d := range relevant {
			if d.Range != nil && !satisfies(cand, *d.Range) {
				allOk = false
				break
			}
		}
		if allOk {
			c := cand
			return ConsolidationResult{CanConsolidate: true, To: &c}
		}
	}
	return ConsolidationResult{CanConsolidate: false}
}

type StaleWithConsolidation struct {
	Stale
	Consolidation ConsolidationResult `json:"consolidation"`
}

/* ------------------------- OUTPUT ------------------------- */

func jsonString(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return strings.TrimRight(buf.String(), "\n")
}

func reportTarget(lock *Lockfile, name string, jsonOut bool) string {
	summary := pkgSummary(lock, name)
	stale := []StaleWithConsolidation{}
	for _, s := range staleScan(lock) {
		if s.Name == name {
			stale = append(stale, StaleWithConsolidation{Stale: s, Consolidation: canConsolidate(lock, name, s.Version)})
		}
	}

	if jsonOut {
		return jsonString(struct {
			Target PkgSummary               `json:"target"`
			Stale  []StaleWithConsolidation `json:"stale"`
		}{Target: summary, Stale: stale})
	}

	var L []string
	line := func(s string) { L = append(L, s) }
	bar := strings.Repeat("─", 64)

	line(bar)
	line("TARGET: " + name)
	line("  Installed versions : " + strings.Join(summary.InstalledVersions, ", "))
	line(fmt.Sprintf("  Version count      : %d", summary.VersionCount))
	line(bar)
	line("")
	if len(summary.Dependents) == 0 {
		line("  ❌ Nothing depends on this package (no dependents).")
		line("     So it CANNOT be the blocker of any upgrade.")
	} else {
		line("  Dependents (who uses it, and with what range):")
		line("")
		for _, d := range summary.Dependents {
			r := "(no range)"
			if d.Range != nil {
				r = *d.Range
			}
			var okVersions []string
			for _, x := range d.Resolved {
				if x.OK {
					okVersions = append(okVersions, x.Version)
				}
			}
			sat := strings.Join(okVersions, ", ")
			if sat == "" {
				sat = "(none)"
			}
			line("    • " + d.From)
			line("        range     : " + r)
			line("        satisfied : " + sat)
			line("")
		}
	}

	if len(stale) > 0 {
		line(bar)
		line("  VERSION GAP ANALYSIS (old version vs newest installed):")
		for _, s := range stale {
			line("")
			line(fmt.Sprintf("  ● %s@%s  (newest installed: %s)", s.Name, s.Version, s.Newest))
			for _, d := range s.Dependents {
				r := "(no range)"
				if d.Range != nil {
					r = *d.Range
				}
				line(fmt.Sprintf("      used by: %s  (%s)", d.From, r))
			}
			if s.Consolidation.CanConsolidate {
				line(fmt.Sprintf("      ✅ CAN be consolidated to %s — all dependents", *s.Consolidation.To))
				line("         of this old version accept the new one per their ranges.")
				line("         Meaning: the \"can't upgrade due to conflict\" claim is NOT proven")
				line("         (it's reluctance, not a semver conflict).")
			} else {
				line("      ❌ Consolidation to another version is impossible without changing ranges:")
				line("         at least one dependent's range rejects every other installed version.")
				line("         (A REAL semver conflict — check the dependents' ranges above.)")
			}
		}
	}
	return strings.Join(L, "\n")
}

func reportUpgrade(lock *Lockfile, target string, jsonOut bool) string {
	at := strings.LastIndex(target, "@")
	name := target
	if at > 0 {
		name = target[:at]
	}
	ver := ""
	if at > 0 {
		ver = target[at+1:]
	}
	if ver == "" {
		return "ERROR: --upgrade format must be <name>@<version>. Example: --upgrade axios@1.7.0"
	}
	res := simulateUpgrade(lock, name, ver)
	if jsonOut {
		return jsonString(res)
	}

	var L []string
	line := func(s string) { L = append(L, s) }
	bar := strings.Repeat("─", 64)
	line(bar)
	line(fmt.Sprintf("UPGRADE SIMULATION: %s → %s", name, ver))
	line(bar)
	line("")
	if len(res.Accepted) > 0 {
		line(fmt.Sprintf("  ✅ ACCEPTED by %d dependent(s):", len(res.Accepted)))
		for _, d := range res.Accepted {
			r := "no range"
			if d.Range != nil {
				r = *d.Range
			}
			src := d.ByPath
			if d.ByPath == "" {
				src = "(root)"
			}
			line(fmt.Sprintf("      • %s  (%s)", src, r))
		}
		line("")
	}
	if len(res.Rejected) > 0 {
		line(fmt.Sprintf("  ❌ REJECTED by %d dependent(s) (range does not allow %s):", len(res.Rejected), ver))
		var rootRej, transRej []Dependent
		for _, d := range res.Rejected {
			if d.ByPath == "" {
				rootRej = append(rootRej, d)
			} else {
				transRej = append(transRej, d)
			}
		}
		for _, d := range res.Rejected {
			src := d.ByPath
			if d.ByPath == "" {
				src = "(root / project package.json)"
			}
			r := ""
			if d.Range != nil {
				r = *d.Range
			}
			line(fmt.Sprintf("      • %s  (%s)", src, r))
		}
		line("")
		if len(rootRej) > 0 {
			line("  ⚠️  IMPORTANT: the main rejection comes from ROOT (project package.json).")
			line("     Root ranges are CONTROLLED by the developer — changeable at any time,")
			line("     there is no technical \"conflict\" here. The \"can't upgrade\" claim is weak.")
			line("")
		}
		if len(transRej) > 0 {
			line("  Transitive rejections (from other dependencies):")
			byRange := map[string][]string{}
			var rangeOrder []string
			for _, d := range transRej {
				r := *d.Range
				if _, ok := byRange[r]; !ok {
					rangeOrder = append(rangeOrder, r)
				}
				byRange[r] = append(byRange[r], d.ByName)
			}
			sort.Strings(rangeOrder)
			for _, r := range rangeOrder {
				uniq := map[string]bool{}
				var names []string
				for _, p := range byRange[r] {
					if !uniq[p] {
						uniq[p] = true
						names = append(names, p)
					}
				}
				sort.Strings(names)
				line(fmt.Sprintf("      range \"%s\" declared by: %s", r, strings.Join(names, ", ")))
			}
			line("")
			line("  Next validation step (needs npm registry access):")
			line("     npm view <name> versions  → check the actual latest version & \"version gap\"")
			line("     then: npm install <name>@<latest> --save-exact  → real test")
		}
	} else {
		line(fmt.Sprintf("  ✅ Version %s is ACCEPTED by ALL dependents per their ranges.", ver))
		line("     No semver conflict blocks this upgrade.")
		line("     If the developer claims a \"conflict\", ask them to show the rejecting range.")
	}
	return strings.Join(L, "\n")
}

func reportDefault(lock *Lockfile, jsonOut bool) string {
	stale := staleScan(lock)
	if jsonOut {
		return jsonString(struct {
			Stale []Stale `json:"stale"`
		}{Stale: stale})
	}

	var L []string
	line := func(s string) { L = append(L, s) }
	bar := strings.Repeat("─", 64)
	lvLabel := "lockfile v1"
	if lock.LockfileVersion >= 2 {
		lvLabel = fmt.Sprintf("lockfile v%d", lock.LockfileVersion)
	}
	line(bar)
	line(fmt.Sprintf("SCAN: %s", lvLabel))
	line(fmt.Sprintf("Installed packages: %d", len(lock.Packages)))
	line(fmt.Sprintf("Multi-version packages (dedup opportunity / sign of outdated deps): %d", len(stale)))
	line(bar)
	line("")
	if len(stale) == 0 {
		line("  ✅ No package is installed at multiple versions at once.")
		line("     (No sign of a \"stuck obsolete\" dependency.)")
		return strings.Join(L, "\n")
	}
	for _, s := range stale[:min(60, len(stale))] {
		line(fmt.Sprintf("  ● %s: %s  (another major in tree: %d)", s.Name, s.Version, s.NewestMajor))
		for _, d := range s.Dependents[:min(5, len(s.Dependents))] {
			r := "(no range)"
			if d.Range != nil {
				r = *d.Range
			}
			line(fmt.Sprintf("      used by: %s  (%s)", d.From, r))
		}
		if len(s.Dependents) > 5 {
			line(fmt.Sprintf("      ... and %d more", len(s.Dependents)-5))
		}
		line("")
	}
	if len(stale) > 60 {
		line(fmt.Sprintf("  ... and %d more packages (use --json for full output)", len(stale)-60))
	}
	line("")
	line("  Tip: lockcheck <lock> --target <name>  for per-package details.")
	return strings.Join(L, "\n")
}

/* ------------------------- GRAPH (HTML) ------------------------- */

type DirectDep struct {
	Name  string
	Range *string
}

func directDepsOf(lock *Lockfile, name string) []DirectDep {
	out := map[string]DirectDep{}
	collect := func(entry *PkgEntry) {
		if entry == nil {
			return
		}
		var deps map[string]string
		if lock.LockfileVersion == 1 {
			deps = entry.Requires
		} else {
			deps = map[string]string{}
			for k, v := range entry.Dependencies {
				deps[k] = v
			}
			for k, v := range entry.OptionalDependencies {
				deps[k] = v
			}
			for k, v := range entry.DevDependencies {
				deps[k] = v
			}
		}
		for dn, r := range deps {
			if _, ok := out[dn]; !ok {
				rr := r
				out[dn] = DirectDep{Name: dn, Range: &rr}
			}
		}
	}
	if name == "" {
		collect(lock.Root)
	} else {
		for path, entry := range lock.Packages {
			if path != "" && nameFromPath(path) == name {
				collect(entry)
			}
		}
	}
	var res []DirectDep
	for _, d := range out {
		res = append(res, d)
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Name < res[j].Name })
	return res
}

type GraphNode struct {
	Name     string
	Level    int
	IsTarget bool
	Versions []string
}

type GraphEdge struct {
	From     string
	To       string
	Range    *string
	Accepted *bool
}

type GraphData struct {
	Nodes     []GraphNode
	Edges     []GraphEdge
	Truncated bool
}

func buildGraphData(lock *Lockfile, target, upgrade string) GraphData {
	rev := buildReverseGraph(lock)
	upName := ""
	upVer := ""
	if upgrade != "" {
		at := strings.LastIndex(upgrade, "@")
		if at > 0 {
			upName = upgrade[:at]
			upVer = upgrade[at+1:]
		}
	}
	focus := target
	if focus == "" {
		focus = upName
	}

	nodes := map[string]*GraphNode{}
	var nodeOrder []string
	var edges []GraphEdge
	edgeKey := map[string]bool{}

	addNode := func(name string, level int, isTarget bool) {
		if n, ok := nodes[name]; ok {
			if isTarget {
				n.IsTarget = true
				n.Level = 0
			} else if level < n.Level {
				n.Level = level
			}
			return
		}
		n := &GraphNode{Name: name, Level: level, IsTarget: isTarget, Versions: sortedVersionKeys(installedVersions(lock, name))}
		nodes[name] = n
		nodeOrder = append(nodeOrder, name)
	}
	addEdge := func(from, to string, rangeStr *string, accepted *bool) {
		k := from + "\u2192" + to
		if edgeKey[k] {
			return
		}
		edgeKey[k] = true
		edges = append(edges, GraphEdge{From: from, To: to, Range: rangeStr, Accepted: accepted})
	}

	if focus != "" {
		addNode(focus, 0, true)
		queue := []string{focus}
		level := map[string]int{focus: 0}
		seen := map[string]bool{focus: true}
		for len(queue) > 0 {
			name := queue[0]
			queue = queue[1:]
			lv := level[name]
			deps := rev[name]
			sort.Slice(deps, func(i, j int) bool { return deps[i].ByPath < deps[j].ByPath })
			for _, d := range deps {
				dn := d.ByName
				addNode(dn, lv-1, false)
				var acc *bool
				if upVer != "" {
					r := d.Range != nil && satisfies(upVer, *d.Range)
					acc = &r
				}
				addEdge(dn, name, d.Range, acc)
				if !seen[dn] {
					seen[dn] = true
					level[dn] = lv - 1
					queue = append(queue, dn)
				}
			}
		}
		for _, child := range directDepsOf(lock, focus) {
			addNode(child.Name, 1, false)
			addEdge(focus, child.Name, child.Range, nil)
		}
	} else {
		for _, s := range staleScan(lock) {
			addNode(s.Name, 0, true)
			for _, d := range s.Dependents {
				fromName := nameFromPath(d.From)
				addNode(fromName, 1, false)
				addEdge(fromName, s.Name, d.Range, nil)
			}
		}
		if len(nodes) == 0 {
			addNode("(root)", 0, true)
			for _, d := range directDepsOf(lock, "") {
				addNode(d.Name, 1, false)
				addEdge("(root)", d.Name, d.Range, nil)
			}
		}
	}

	nodeList := make([]GraphNode, 0, len(nodeOrder))
	for _, name := range nodeOrder {
		nodeList = append(nodeList, *nodes[name])
	}
	const MAX = 500
	truncated := len(nodeList) > MAX
	if truncated {
		nodeList = nodeList[:MAX]
	}
	return GraphData{Nodes: nodeList, Edges: edges, Truncated: truncated}
}

func escapeHtml(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	).Replace(s)
}

type GraphMeta struct {
	Title      string
	Subtitle   string
	HasUpgrade bool
	Truncated  bool
}

const graphTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8"/>
<title>__TITLE__</title>
<style>
  body{margin:0;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;background:#0f172a;color:#e2e8f0;}
  header{padding:14px 22px;border-bottom:1px solid #1e293b;display:flex;align-items:center;gap:16px;flex-wrap:wrap;}
  h1{font-size:16px;margin:0;}
  .sub{color:#94a3b8;font-size:12px;}
  #wrap{position:fixed;inset:56px 0 0 0;overflow:hidden;cursor:grab;}
  #wrap.drag{cursor:grabbing;}
  svg{width:100%;height:100%;}
  .edge{fill:none;stroke-width:1.6;stroke:#475569;}
  .edge.ok{stroke:#22c55e;}
  .edge.rej{stroke:#ef4444;stroke-width:2.4;}
  .edge-label{font-size:10px;fill:#94a3b8;}
  .node rect{fill:#1e293b;stroke:#334155;stroke-width:1.5;}
  .node.target rect{fill:#450a0a;stroke:#ef4444;stroke-width:2.5;}
  .node-name{font-size:12px;font-weight:600;fill:#e2e8f0;}
  .node-ver{font-size:10px;fill:#94a3b8;}
  .node:hover rect{stroke:#38bdf8;stroke-width:2.5;}
  #tip{position:fixed;pointer-events:none;background:#0b1220;border:1px solid #334155;padding:8px 10px;border-radius:8px;font-size:12px;display:none;z-index:10;max-width:420px;white-space:pre;}
  #legend{position:fixed;left:14px;bottom:12px;font-size:11px;color:#94a3b8;background:#0b1220;border:1px solid #1e293b;border-radius:8px;padding:8px 12px;}
  #legend b{color:#e2e8f0;}
  .dot{display:inline-block;width:10px;height:10px;border-radius:50%;margin-right:5px;vertical-align:-1px;}
  #search{position:fixed;right:14px;top:14px;}
  #search input{background:#0b1220;border:1px solid #334155;color:#e2e8f0;border-radius:8px;padding:7px 10px;font-size:12px;outline:none;}
  #search input:focus{border-color:#38bdf8;}
  .highlight rect{stroke:#f59e0b !important;stroke-width:3px !important;}
  .controls{color:#64748b;font-size:11px;}
</style>
</head>
<body>
<header>
  <h1>__TITLE__</h1>
  <span class="sub">__SUBTITLE__</span>
  <span class="controls">scroll = zoom &nbsp;·&nbsp; drag = pan</span>
</header>
<div id="search"><input type="text" id="q" placeholder="highlight package..."/></div>
<div id="wrap"><svg id="svg">
  <defs>
    <marker id="arr" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
      <path d="M0,0 L10,5 L0,10 z" fill="#64748b"/>
    </marker>
  </defs>
  <g id="world">__EDGES____NODES__</g>
</svg></div>
<div id="tip"></div>
<div id="legend">
  __LEGEND__
  <div><span class="dot" style="background:#ef4444;border:2px solid #450a0a;"></span>focus package</div>
  <div>arrows point <b>to the dependency</b> (X&rarr;Y = X depends on Y)</div>
  __TRUNCATED__
</div>
<script>
const svg=document.getElementById('svg'),world=document.getElementById('world'),tip=document.getElementById('tip'),wrap=document.getElementById('wrap');
let scale=0.8,tx=0,ty=0;
function apply(){world.setAttribute('transform','translate('+tx+','+ty+') scale('+scale+')');}
apply();
wrap.addEventListener('wheel',e=>{e.preventDefault();const f=e.deltaY>0?0.9:1.1;scale=Math.min(3,Math.max(0.1,scale*f));apply();},{passive:false});
let dragging=false,sx=0,sy=0,stx=0,sty=0;
wrap.addEventListener('mousedown',e=>{dragging=true;wrap.classList.add('drag');sx=e.clientX;sy=e.clientY;stx=tx;sty=ty;});
window.addEventListener('mousemove',e=>{if(dragging){tx=stx+e.clientX-sx;ty=sty+e.clientY-sy;apply();}const t=document.elementFromPoint(e.clientX,e.clientY);const g=t&&t.closest('g.node');if(g){tip.style.display='block';tip.style.left=(e.clientX+14)+'px';tip.style.top=(e.clientY+12)+'px';tip.textContent=g.dataset.name+'\nversion: v'+g.dataset.versions+'\nlevel: '+g.dataset.level;}else{tip.style.display='none';}});
window.addEventListener('mouseup',()=>{dragging=false;wrap.classList.remove('drag');});
const q=document.getElementById('q');
q.addEventListener('input',()=>{const s=q.value.trim().toLowerCase();document.querySelectorAll('g.node').forEach(n=>n.classList.toggle('highlight',s&&n.dataset.name.toLowerCase().includes(s)));});
</script>
</body>
</html>`

func fmtFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func renderGraphHtml(graph GraphData, meta GraphMeta) string {
	nodes := graph.Nodes
	edges := graph.Edges
	const MINW = 230
	const NH = 46
	const VGAP = 18
	const MARGIN = 70
	const COLW = MINW + 90

	levelSet := map[int]bool{}
	for _, n := range nodes {
		levelSet[n.Level] = true
	}
	var levels []int
	for lv := range levelSet {
		levels = append(levels, lv)
	}
	sort.Ints(levels)
	minLevel := levels[0]

	byLevel := map[int][]GraphNode{}
	for _, n := range nodes {
		byLevel[n.Level] = append(byLevel[n.Level], n)
	}
	pos := map[string][2]int{}
	maxCount := 0
	for _, lv := range levels {
		arr := byLevel[lv]
		sort.Slice(arr, func(i, j int) bool { return arr[i].Name < arr[j].Name })
		if len(arr) > maxCount {
			maxCount = len(arr)
		}
		for i, n := range arr {
			pos[n.Name] = [2]int{(lv-minLevel)*COLW + MARGIN, i*(NH+VGAP) + MARGIN}
		}
	}

	var edgeSvg []string
	for _, e := range edges {
		a, okA := pos[e.From]
		b, okB := pos[e.To]
		if !okA || !okB {
			continue
		}
		x1 := a[0] + MINW
		y1 := a[1] + NH/2
		x2 := b[0]
		y2 := b[1] + NH/2
		cx := float64(x1+x2) / 2
		cls := "neut"
		if e.Accepted != nil && *e.Accepted {
			cls = "ok"
		} else if e.Accepted != nil && !*e.Accepted {
			cls = "rej"
		}
		d := fmt.Sprintf("M%d,%d C%s,%d %s,%d %d,%d", x1, y1, fmtFloat(cx), y1, fmtFloat(cx), y2, x2, y2)
		label := ""
		if e.Range != nil && *e.Range != "" {
			label = fmt.Sprintf(`<text x="%s" y="%s" class="edge-label" text-anchor="middle">%s</text>`,
				fmtFloat(cx), fmtFloat(float64(y1+y2)/2-6), escapeHtml(*e.Range))
		}
		rangeTitle := ""
		if e.Range != nil {
			rangeTitle = *e.Range
		}
		edgeSvg = append(edgeSvg, fmt.Sprintf(`<path class="edge %s" d="%s" marker-end="url(#arr)"><title>%s -> %s  [%s]</title></path>%s`,
			cls, d, escapeHtml(e.From), escapeHtml(e.To), escapeHtml(rangeTitle), label))
	}

	var nodeSvg []string
	for _, n := range nodes {
		p, ok := pos[n.Name]
		if !ok {
			continue
		}
		cls := "node"
		if n.IsTarget {
			cls = "node target"
		}
		label := n.Name
		if utf8.RuneCountInString(label) > 26 {
			runes := []rune(label)
			label = string(runes[:25]) + "\u2026"
		}
		ver := strings.Join(n.Versions, ", ")
		nodeSvg = append(nodeSvg, fmt.Sprintf(
			`<g class="%s" transform="translate(%d,%d)" data-name="%s" data-versions="%s" data-level="%d">`+
				`<rect width="%d" height="%d" rx="9" ry="9"/>`+
				`<text x="12" y="20" class="node-name">%s</text>`+
				`<text x="12" y="37" class="node-ver">v%s</text>`+
				"<title>%s\nv%s</title>"+
				`</g>`,
			cls, p[0], p[1], escapeHtml(n.Name), escapeHtml(ver), n.Level,
			MINW, NH, escapeHtml(label), escapeHtml(ver), escapeHtml(n.Name), escapeHtml(ver)))
	}

	legend := ""
	if meta.HasUpgrade {
		legend = `<div><span class="dot" style="background:#22c55e;"></span>accepted range</div><div><span class="dot" style="background:#ef4444;"></span>rejected range</div>`
	}
	truncated := ""
	if meta.Truncated {
		truncated = `<div style="color:#f59e0b;">graph truncated to 500 nodes</div>`
	}

	return strings.NewReplacer(
		"__TITLE__", escapeHtml(meta.Title),
		"__SUBTITLE__", escapeHtml(meta.Subtitle),
		"__EDGES__", strings.Join(edgeSvg, "\n"),
		"__NODES__", strings.Join(nodeSvg, "\n"),
		"__LEGEND__", legend,
		"__TRUNCATED__", truncated,
	).Replace(graphTemplate)
}

/* ------------------------- MAIN ------------------------- */

func hasFlag(argv []string, name string) bool {
	for _, a := range argv {
		if a == name {
			return true
		}
	}
	return false
}

func indexOf(argv []string, name string) int {
	for i, a := range argv {
		if a == name {
			return i
		}
	}
	return -1
}

func main() {
	argv := os.Args[1:]
	jsonOut := hasFlag(argv, "--json")
	noTree := hasFlag(argv, "--no-tree")
	iTarget := indexOf(argv, "--target")
	iUpgrade := indexOf(argv, "--upgrade")
	iHtml := indexOf(argv, "--html")

	lockArg := "package-lock.json"
	for _, a := range argv {
		if !strings.HasPrefix(a, "--") {
			lockArg = a
			break
		}
	}

	target := ""
	upgrade := ""
	htmlOut := ""
	if iTarget != -1 && iTarget+1 < len(argv) {
		target = argv[iTarget+1]
	}
	if iUpgrade != -1 && iUpgrade+1 < len(argv) {
		upgrade = argv[iUpgrade+1]
	}
	if iHtml != -1 && iHtml+1 < len(argv) {
		htmlOut = argv[iHtml+1]
	}

	if hasFlag(argv, "--help") || hasFlag(argv, "-h") {
		fmt.Println(strings.TrimSpace(USAGE))
		return
	}

	lock, err := parseLockfile(lockArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to read %s: %v\n", lockArg, err)
		fmt.Fprintln(os.Stderr, strings.TrimSpace(USAGE))
		os.Exit(1)
	}

	var out string
	if upgrade != "" {
		out = reportUpgrade(lock, upgrade, jsonOut)
	} else if target != "" {
		out = reportTarget(lock, target, jsonOut)
		_ = noTree
	} else {
		out = reportDefault(lock, jsonOut)
	}
	fmt.Println(out)

	if htmlOut != "" {
		focus := target
		if focus == "" && upgrade != "" {
			at := strings.LastIndex(upgrade, "@")
			if at > 0 {
				focus = upgrade[:at]
			}
		}
		upVer := ""
		if upgrade != "" {
			at := strings.LastIndex(upgrade, "@")
			if at > 0 {
				upVer = upgrade[at+1:]
			}
		}
		graph := buildGraphData(lock, target, upgrade)
		title := "Outdated / multi-version packages"
		if focus != "" {
			title = "Dependency graph around " + focus
			if upVer != "" {
				title += " @ " + upVer
			}
		}
		subtitle := fmt.Sprintf("%s · %d packages, %d edges", lockArg, len(graph.Nodes), len(graph.Edges))
		if focus != "" && upVer != "" {
			subtitle += " · green = range accepts upgrade, red = rejects"
		}
		meta := GraphMeta{Title: title, Subtitle: subtitle, HasUpgrade: upVer != "", Truncated: graph.Truncated}
		html := renderGraphHtml(graph, meta)
		if err := os.WriteFile(htmlOut, []byte(html), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "\n❌ Failed to write HTML: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\n✅ Interactive graph saved to: %s\n", htmlOut)
	}
}
