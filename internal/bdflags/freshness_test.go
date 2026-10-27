//go:build integration

package bdflags

import (
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// flagNameRE matches the flag-declaration prefix of a cobra --help flag
// line, e.g. "  -a, --assignee string   Assignee" or
// "      --claim                Atomically claim...". It is anchored to
// the start of the line so mentions of "--flag" inside another flag's
// description text (which always follow on the same line, never at the
// start of one) are not mistaken for a declaration.
var flagNameRE = regexp.MustCompile(`(?m)^\s*(?:-([A-Za-z0-9]), )?--([A-Za-z0-9][A-Za-z0-9-]*)`)

// parseHelpFlagNames extracts every long (--flag) and short (-f) flag name
// declared in a bd --help transcript. The "Flags:" and "Global Flags:"
// sections are both plain flag-declaration lines and are matched the same
// way, so the result already includes global flags alongside the
// subcommand's own.
func parseHelpFlagNames(help string) map[string]bool {
	names := make(map[string]bool)
	for _, line := range strings.Split(help, "\n") {
		m := flagNameRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if m[1] != "" {
			names["-"+m[1]] = true
		}
		names["--"+m[2]] = true
	}
	return names
}

// TestBdFlagManifestCurrent guards against the bd CLI growing a flag that
// this package's hardcoded manifest doesn't know about. It shells the real
// installed bd binary's --help output per known subcommand and fails
// loudly — fail-closed, the same posture as bdMutationWriteIDs in
// cmd/gc/cmd_bd.go — if the live CLI declares a flag the manifest is
// MISSING.
//
// It deliberately does not fail on the reverse (the manifest listing flags
// the installed bd lacks). The manifest is intentionally the newest-known
// superset of bd's flags — see its dated-provenance comment — and its two
// consumers, the `gc lint` bd-flag check (scan.go) and the cmd_bd
// write-mutation ID guard, only misbehave when a real flag is missing from
// the manifest, never when the manifest is ahead of the installed bd. The
// bd binary is version-pinned independently of the manifest's provenance:
// CI installs the stable bd release via BD_VERSION while the manifest
// tracks the newer bd the fleet runs, so a manifest ahead of the installed
// bd is expected and benign. That skew is reported, not failed.
//
// If bd is not in PATH, the test is skipped with a clear message rather
// than failing, since manifest currency can't be checked without a bd
// binary to check it against.
func TestBdFlagManifestCurrent(t *testing.T) {
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		t.Skip("bd not found in PATH; skipping flag-manifest freshness check")
	}

	for _, sub := range Subcommands() {
		t.Run(sub, func(t *testing.T) {
			args := append(strings.Fields(sub), "--help")
			out, _ := exec.Command(bdPath, args...).CombinedOutput()
			live := parseHelpFlagNames(string(out))
			if len(live) == 0 {
				t.Fatalf("parsed zero flags from `bd %s --help`; output format may have changed:\n%s", sub, out)
			}

			manifest := mergeFlagSets(ValueFlags(sub), BoolFlags(sub))

			var missingFromManifest, aheadOfInstalled []string
			for f := range live {
				if !manifest[f] {
					missingFromManifest = append(missingFromManifest, f)
				}
			}
			for f := range manifest {
				if !live[f] {
					aheadOfInstalled = append(aheadOfInstalled, f)
				}
			}
			sort.Strings(missingFromManifest)
			sort.Strings(aheadOfInstalled)

			// Benign direction: the manifest lists flags this bd binary does
			// not have. Expected whenever the installed bd is older than the
			// manifest's provenance version (e.g. CI's pinned stable bd). A
			// superset allowlist is safe for both consumers, so report it
			// without failing.
			if len(aheadOfInstalled) > 0 {
				t.Logf("bd %s: manifest lists %d flag(s) absent from this bd's --help: %v (manifest is ahead of the installed bd; benign for a superset allowlist)",
					sub, len(aheadOfInstalled), aheadOfInstalled)
			}

			// Dangerous direction: the live bd declares a flag the manifest
			// does not know. gc lint would then false-positive on valid
			// templates and the write-mutation ID guard could misparse the
			// bead ID. Fail closed and require the manifest be regenerated.
			if len(missingFromManifest) > 0 {
				t.Errorf("bd %s flag manifest is missing flag(s) present in `bd %s --help`: %v\nThe manifest must be a superset of the installed bd's real flags. Update internal/bdflags/bdflags.go with a fresh dated-provenance comment.",
					sub, sub, missingFromManifest)
			}
		})
	}
}

// helpVerbRE matches one command line in a `bd --help` command group: two
// spaces, the verb, then its description.
var helpVerbRE = regexp.MustCompile(`^  ([a-zA-Z][a-zA-Z0-9-]*)\s+\S`)

// TestBdTopLevelVerbsCurrent guards the verb inventory the same way
// TestBdFlagManifestCurrent guards the flag manifests: it fails when the
// installed bd lists a top-level verb TopLevelVerbs is missing. The
// class-must-not-flip guard's coverage test in cmd/gc requires a row for every
// verb in the inventory, so a new bd verb reaches that test through here.
// The reverse (the inventory lists a verb this bd lacks) is reported, not
// failed: the inventory tracks the newest bd the fleet runs.
func TestBdTopLevelVerbsCurrent(t *testing.T) {
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		t.Skip("bd not found in PATH; skipping verb-inventory freshness check")
	}
	out, _ := exec.Command(bdPath, "--help").CombinedOutput()
	known := map[string]bool{}
	for _, v := range TopLevelVerbs() {
		known[v] = true
	}
	live := map[string]bool{}
	inGroup := false
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "Flags:"):
			inGroup = false
		case strings.HasSuffix(line, ":") && !strings.HasPrefix(line, " "):
			inGroup = line != "Usage:"
		case inGroup:
			if m := helpVerbRE.FindStringSubmatch(line); m != nil {
				live[m[1]] = true
			}
		}
	}
	if len(live) == 0 {
		t.Fatalf("parsed zero verbs from `bd --help`; output format may have changed:\n%s", out)
	}
	var missing, ahead []string
	for v := range live {
		if !known[v] {
			missing = append(missing, v)
		}
	}
	for v := range known {
		if !live[v] {
			ahead = append(ahead, v)
		}
	}
	sort.Strings(missing)
	sort.Strings(ahead)
	if len(ahead) > 0 {
		t.Logf("TopLevelVerbs lists %d verb(s) absent from this bd's --help (hidden, or newer than the installed bd): %v", len(ahead), ahead)
	}
	if len(missing) > 0 {
		t.Errorf("`bd --help` lists verb(s) TopLevelVerbs is missing: %v\nAdd them to internal/bdflags/verbs.go, then give each a row in cmd/gc's TestGcBdEveryVerbIsGuardedOrCannotFlipClass.", missing)
	}
}

// TestBdCommandInventoryCurrent ties the installed bd to its pinned
// inventory. `bd version` must name a pinned build (InventoryFor), and the
// installed binary's visible command tree (`bd <path> --help`, recursively)
// must match that inventory: every path, alias and flag present, and each
// flag's shape (takes a value, or not) the same. Hidden commands and flags are
// not in --help; they come from bd's own cobra tree (inventorygen/generate.sh),
// which is the inventory's source of truth. CI's integration lane runs this
// against the pinned bd 1.3.2; run it with the Enterprise build on PATH to
// check that inventory too.
func TestBdCommandInventoryCurrent(t *testing.T) {
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		t.Skip("bd not found in PATH; skipping command-inventory freshness check")
	}
	versionOut, _ := exec.Command(bdPath, "version").CombinedOutput()
	inv, err := InventoryFor(string(versionOut))
	if err != nil {
		t.Fatalf("the installed bd has no pinned inventory: %v\nRegenerate internal/bdflags/inventory with inventorygen/generate.sh for this build.", err)
	}
	aliases := inv.Aliases()
	var problems []string
	var walk func(path []string)
	walk = func(path []string) {
		out, _ := exec.Command(bdPath, append(append([]string(nil), path...), "--help")...).CombinedOutput()
		section := ""
		key := strings.Join(path, " ")
		parent := strings.Join(path[:max(0, len(path)-1)], " ")
		flags := map[string]InventoryFlag{}
		for _, f := range inv.Flags(key) {
			flags["--"+f.Name] = f
		}
		var subs []string
		for _, line := range strings.Split(string(out), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(line, " ") && strings.HasSuffix(trimmed, ":") && trimmed != "" {
				section = strings.TrimSuffix(trimmed, ":")
				continue
			}
			switch {
			case section == "Aliases" && trimmed != "" && len(path) > 0:
				for _, a := range strings.Split(trimmed, ",") {
					a = strings.TrimSpace(a)
					if a == "" || a == path[len(path)-1] {
						continue
					}
					full := strings.TrimSpace(parent + " " + a)
					if aliases[full] != key {
						problems = append(problems, "alias "+full+" → "+key+" missing")
					}
				}
				section = ""
			case (section == "Flags" || section == "Global Flags") && len(path) > 0:
				m := flagShapeRE.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				long := "--" + m[2]
				f, ok := flags[long]
				if !ok {
					problems = append(problems, "flag "+key+" "+long+" missing")
					continue
				}
				takesValue := strings.TrimSpace(m[3]) != "" && !strings.Contains(m[3], "[=")
				if takesValue != f.TakesValue() {
					problems = append(problems, fmt.Sprintf("flag %s %s: help says takes-value=%v, inventory says %v (type %s, NoOptDefVal %q)", key, long, takesValue, f.TakesValue(), f.Type, f.NoOptDefVal))
				}
			case section == "Available Commands" || (len(path) == 0 && section != "" && section != "Flags" && section != "Global Flags" && section != "Usage" && section != "Examples"):
				if m := helpVerbRE.FindStringSubmatch(line); m != nil {
					subs = append(subs, m[1])
				}
			}
		}
		for _, sub := range subs {
			child := strings.TrimSpace(key + " " + sub)
			if inv.Command(child) == nil {
				problems = append(problems, "path "+child+" missing")
				continue
			}
			if len(path) < 3 {
				walk(append(append([]string(nil), path...), sub))
			}
		}
	}
	walk(nil)
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("the installed bd (%s) disagrees with inventory %s in %d place(s):\n  %s\nRegenerate it with inventorygen/generate.sh, then decide each new path, alias and flag in cmd/gc's split-city allowlist.", strings.TrimSpace(string(versionOut)), inv.Name, len(problems), strings.Join(problems, "\n  "))
	}
}

// flagShapeRE splits a cobra help flag line into shorthand, name and the
// value-type token (empty for a bool).
var flagShapeRE = regexp.MustCompile(`^\s+(?:-([A-Za-z0-9]), )?--([A-Za-z0-9][A-Za-z0-9-]*)(?: (\S[^ ]*(?: [^ ]+)*?))?(?:\s{2,}|$)`)
