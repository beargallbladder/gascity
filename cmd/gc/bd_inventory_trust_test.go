//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// renderBdHelp renders one HelpView the way cobra's default help template
// lays it out, for a fake bd that serves a tree's help.
func renderBdHelp(path string, v bdflags.HelpView) string {
	name := "bd"
	if path != "" {
		fields := strings.Fields(path)
		name = fields[len(fields)-1]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Some command.\n\nUsage:\n  bd %s [flags]\n", path)
	if len(v.Aliases) > 0 {
		fmt.Fprintf(&b, "\nAliases:\n  %s, %s\n", name, strings.Join(v.Aliases, ", "))
	}
	if len(v.Subcommands) > 0 {
		b.WriteString("\nAvailable Commands:\n")
		for _, s := range v.Subcommands {
			fmt.Fprintf(&b, "  %-12s does a thing\n", s)
		}
	}
	if v.Flags != "" {
		fmt.Fprintf(&b, "\nFlags:\n%s\n", v.Flags)
	}
	if v.GlobalFlags != "" {
		fmt.Fprintf(&b, "\nGlobal Flags:\n%s\n", v.GlobalFlags)
	}
	b.WriteString("\nUse \"bd [command] --help\" for more information about a command.\n")
	return b.String()
}

// writeFakeTreeBd writes a fake bd at binPath that serves tree's help from
// files in helpDir and logs every call to logPath.
func writeFakeTreeBd(t *testing.T, binPath, helpDir, logPath string, tree bdflags.HelpTree) {
	t.Helper()
	if err := os.MkdirAll(helpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, v := range tree {
		key := "_root"
		if path != "" {
			key = strings.ReplaceAll(path, " ", "_")
		}
		if err := os.WriteFile(filepath.Join(helpDir, key+".txt"), []byte(renderBdHelp(path, v)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
key=""
for a in "$@"; do
  [ "$a" = "--help" ] && continue
  key="${key}${key:+_}$a"
done
[ -z "$key" ] && key=_root
cat %q/"$key".txt
`, logPath, helpDir)
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "\n")
}

// withoutFlag returns tree with one flag line removed from one command: a
// binary whose tree differs from the pinned one in one visible flag.
func withoutFlag(tree bdflags.HelpTree, path, long string) bdflags.HelpTree {
	out := bdflags.HelpTree{}
	for p, v := range tree {
		if p == path {
			var kept []string
			for _, l := range strings.Split(v.Flags, "\n") {
				if !strings.Contains(l, long+" ") {
					kept = append(kept, l)
				}
			}
			v.Flags = strings.Join(kept, "\n")
		}
		out[p] = v
	}
	return out
}

// TestBdTrustMatchesTheCommandTreeNotTheVersion is the owner's trust policy
// with maintainer-city's config shape: city.toml's [workspace.env] PATH pins a
// bd that is NOT the pinned build's file — a different path and binary — but
// has the pinned Enterprise tree, and it is accepted. A binary with OSS main's
// kind of difference (one visible flag changed) is refused with the
// difference named. The verdict is cached per binary and recomputed when the
// binary changes.
func TestBdTrustMatchesTheCommandTreeNotTheVersion(t *testing.T) {
	c := newBdParityCity(t, bdParityOpts{storage: bdSQLRefusalSplitStorage})
	cfg, err := loadBdPassthroughConfig(c.city, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	enterprise := bdflags.InventoryByName("bd-1.3.0-enterprise-77612097cec0")
	logPath := filepath.Join(c.scratch, "walk.log")
	bin := filepath.Join(c.bin, "bd")
	writeFakeTreeBd(t, bin, filepath.Join(c.scratch, "help"), logPath, enterprise.HelpTree())
	target := bdCityScopeTarget(c.city, cfg)

	resolvedBin, err := resolveBdBinaryForScope(c.city, target.ScopeRoot)
	if err != nil || resolvedBin != bin {
		t.Fatalf("the [workspace.env] PATH pin resolved %q (%v), want %q", resolvedBin, err, bin)
	}
	v, _, err := resolveScopeBdInventory(c.city, cfg, target, false)
	if err != nil || v.Inventory != enterprise.Name || v.Source != "walk" {
		t.Fatalf("verdict = %+v, %v; want %s by walk", v, err, enterprise.Name)
	}
	walks := countLines(t, logPath)
	if walks < 100 {
		t.Fatalf("the walk ran bd %d times; the tree looks unwalked", walks)
	}

	// Cached: no bd call at all.
	v, _, err = resolveScopeBdInventory(c.city, cfg, target, false)
	if err != nil || v.Inventory != enterprise.Name || v.Source != "cache" || countLines(t, logPath) != walks {
		t.Fatalf("second verdict = %+v, %v (bd calls %d → %d); want a cache hit", v, err, walks, countLines(t, logPath))
	}

	// The binary changes (OSS main's kind of drift): the cache key misses and
	// the new tree is refused.
	time.Sleep(10 * time.Millisecond)
	writeFakeTreeBd(t, bin, filepath.Join(c.scratch, "help2"), logPath, withoutFlag(enterprise.HelpTree(), "update", "--claim"))
	v, _, err = resolveScopeBdInventory(c.city, cfg, target, false)
	if err != nil || v.Inventory != "" || v.Source != "walk" || len(v.Diffs) == 0 {
		t.Fatalf("changed binary verdict = %+v, %v; want a refusal by walk", v, err)
	}
	if !slices.ContainsFunc(v.Diffs, func(d string) bool { return strings.Contains(d, `"update" flags differ`) }) {
		t.Errorf("the refusal does not name the difference: %v", v.Diffs)
	}
	if _, err := bdInventoryForScope(c.city, cfg, target); err == nil || !strings.Contains(err.Error(), "matches none") {
		t.Fatalf("bdInventoryForScope on a refused tree: %v", err)
	}
}

// TestBdTrustVouchAndShaCache: a city's vouch for a binary's sha256 is taken
// without a walk; the same bytes under another path reuse the cached verdict;
// a vouch for different bytes is ignored.
func TestBdTrustVouchAndShaCache(t *testing.T) {
	c := newBdParityCity(t, bdParityOpts{storage: bdSQLRefusalSplitStorage})
	oss := bdflags.InventoryByName("bd-1.3.2")
	logPath := filepath.Join(c.scratch, "walk.log")
	bin := filepath.Join(c.bin, "bd")
	writeFakeTreeBd(t, bin, filepath.Join(c.scratch, "help"), logPath, oss.HelpTree())
	sum, err := fileSHA256(bin)
	if err != nil {
		t.Fatal(err)
	}
	run := bdHelpRunnerFor(bin, c.city, os.Environ())
	cfg := &config.City{Storage: &config.StorageConfig{GCBdInventory: oss.Name + "@sha256:" + sum}}
	v, err := resolveBdInventory(c.city, cfg, bin, run, false)
	if err != nil || v.Inventory != oss.Name || v.Source != "vouched" || countLines(t, logPath) != 0 {
		t.Fatalf("vouched verdict = %+v, %v (bd calls %d); want vouched, no walk", v, err, countLines(t, logPath))
	}
	// A copy at another path: same sha256, cached verdict, no walk.
	copyPath := filepath.Join(c.scratch, "bd-copy")
	data, _ := os.ReadFile(bin)
	if err := os.WriteFile(copyPath, data, 0o755); err != nil {
		t.Fatal(err)
	}
	v, err = resolveBdInventory(c.city, &config.City{}, copyPath, bdHelpRunnerFor(copyPath, c.city, os.Environ()), false)
	if err != nil || v.Inventory != oss.Name || v.Source != "cache" || countLines(t, logPath) != 0 {
		t.Fatalf("same-bytes verdict = %+v, %v (bd calls %d); want a sha cache hit", v, err, countLines(t, logPath))
	}
	// A vouch for other bytes is not this binary's.
	other := &config.City{Storage: &config.StorageConfig{GCBdInventory: oss.Name + "@sha256:" + strings.Repeat("0", 64)}}
	if err := os.WriteFile(bdTrustCachePath(c.city), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err = resolveBdInventory(c.city, other, bin, run, false)
	if err != nil || v.Source != "walk" || v.Inventory != oss.Name {
		t.Fatalf("mismatched vouch verdict = %+v, %v; want a walk", v, err)
	}
}

// TestBdInventoryDoctorCheckAndFix: the check reports a scope whose bd has no
// inventory; the fix vouches only after a real walk matched.
func TestBdInventoryDoctorCheckAndFix(t *testing.T) {
	c := newBdParityCity(t, bdParityOpts{storage: bdSQLRefusalSplitStorage})
	cfg, err := loadBdPassthroughConfig(c.city, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	enterprise := bdflags.InventoryByName("bd-1.3.0-enterprise-77612097cec0")
	bin := filepath.Join(c.bin, "bd")
	logPath := filepath.Join(c.scratch, "walk.log")

	writeFakeTreeBd(t, bin, filepath.Join(c.scratch, "bad"), logPath, withoutFlag(enterprise.HelpTree(), "close", "--reason"))
	check := newBdInventoryCheck(c.city, cfg)
	if r := check.Run(&doctor.CheckContext{CityPath: c.city}); r.Status != doctor.StatusError || !strings.Contains(strings.Join(r.Details, "\n"), "matches no pinned") {
		t.Fatalf("un-inventoried bd: %+v", r)
	}
	if err := check.Fix(&doctor.CheckContext{CityPath: c.city}); err == nil || !strings.Contains(err.Error(), "refusing to vouch") {
		t.Fatalf("Fix on a mismatched tree: %v", err)
	}
	// A starting controller records the finding as a gc_bd.uninventoried
	// event (review must-fix 3: loud, not just a doctor line).
	ev := bdInventoryStartFinding(check)
	if ev == nil || ev.Type != bdUninventoriedEvent || !strings.Contains(string(ev.Payload), "matches no pinned") {
		t.Fatalf("controller-start finding = %+v", ev)
	}
	if bdInventoryStartFinding(newBdInventoryCheck(c.city, &config.City{})) != nil {
		t.Fatal("a single-store city produced a controller-start finding")
	}
	if data, _ := os.ReadFile(filepath.Join(c.city, "city.toml")); strings.Contains(string(data), "gc_bd_inventory") {
		t.Fatal("Fix wrote a vouch for a mismatched tree")
	}

	time.Sleep(10 * time.Millisecond)
	writeFakeTreeBd(t, bin, filepath.Join(c.scratch, "good"), logPath, enterprise.HelpTree())
	if err := check.Fix(&doctor.CheckContext{CityPath: c.city}); err != nil {
		t.Fatalf("Fix on a matching tree: %v", err)
	}
	reloaded, err := loadBdPassthroughConfig(c.city, os.Stderr)
	if err != nil {
		t.Fatalf("city.toml after the fix does not load: %v", err)
	}
	sum, _ := fileSHA256(bin)
	if reloaded.Storage == nil || reloaded.Storage.GCBdInventory != enterprise.Name+"@sha256:"+sum {
		t.Fatalf("vouch = %+v, want %s@sha256:%s", reloaded.Storage, enterprise.Name, sum)
	}
	if r := newBdInventoryCheck(c.city, reloaded).Run(&doctor.CheckContext{CityPath: c.city}); r.Status != doctor.StatusOK {
		t.Fatalf("after the fix: %+v", r)
	}
	if ev := bdInventoryStartFinding(newBdInventoryCheck(c.city, reloaded)); ev != nil {
		t.Fatalf("a matching bd produced a controller-start finding: %+v", ev)
	}
	if !newBdInventoryCheck(c.city, reloaded).WarmupEligible() {
		t.Fatal("the check must run in gc start's warm-up, which is the controller-start warning")
	}
	if r := newBdInventoryCheck(c.city, &config.City{}).Run(&doctor.CheckContext{CityPath: c.city}); r.Status != doctor.StatusOK {
		t.Fatalf("a single-store city: %+v", r)
	}
}

// TestWriteGCBdInventoryVouchShapes covers where the vouch lands in city.toml.
func TestWriteGCBdInventoryVouchShapes(t *testing.T) {
	sum := strings.Repeat("a", 64)
	for name, c := range map[string]struct{ in, want string }{
		"storage table":     {"[storage]\nfoo = 1\n", "[storage]\ngc_bd_inventory = \"x@sha256:" + sum + "\"\nfoo = 1\n"},
		"replace":           {"[storage]\ngc_bd_inventory = \"old\"\n", "[storage]\ngc_bd_inventory = \"x@sha256:" + sum + "\"\n"},
		"subtables only":    {"[workspace]\nname = \"d\"\n\n[storage.classes]\nwork = \"work\"\n", "[workspace]\nname = \"d\"\n\n[storage]\ngc_bd_inventory = \"x@sha256:" + sum + "\"\n\n[storage.classes]\nwork = \"work\"\n"},
		"no storage at all": {"[workspace]\nname = \"d\"\n", "[workspace]\nname = \"d\"\n\n\n[storage]\ngc_bd_inventory = \"x@sha256:" + sum + "\"\n"},
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "city.toml")
			if err := os.WriteFile(p, []byte(c.in), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := writeGCBdInventoryVouch(p, "x@sha256:"+sum); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(p)
			if string(got) != c.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}

// TestGcBdRefusalRecordsAnEvent is review must-fix 3: a split-city refusal
// writes a gc_bd.refused event, so a script's `|| true` cannot hide it.
func TestGcBdRefusalRecordsAnEvent(t *testing.T) {
	c := newBdParityCity(t, bdParityOpts{storage: bdSQLRefusalSplitStorage})
	t.Setenv("GC_AGENT", "deacon")
	var stdout, stderr strings.Builder
	if code := doBd([]string{"serve"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d; stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(c.city, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("no events log: %v", err)
	}
	var found bool
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, `"type":"gc_bd.refused"`) && strings.Contains(line, `"caller":"deacon"`) && strings.Contains(line, `"verb":"serve"`) && strings.Contains(line, `"kind":"allowlist"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no gc_bd.refused event for the refusal:\n%s", data)
	}
}
