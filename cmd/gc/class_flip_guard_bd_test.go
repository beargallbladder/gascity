//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// bdParityWrites returns the bd invocations a parity arm recorded other than
// the reads the passthrough's own guards and the split city's residency
// checks make (show, list, query), one argv per entry.
func bdParityWrites(log string) []string {
	var writes []string
	for _, line := range strings.Split(log, "\n") {
		argv, ok := strings.CutPrefix(line, "argv=")
		if !ok {
			continue
		}
		switch verb, _ := bdRelocatedClassVerbOf(strings.Fields(argv)); verb {
		case "show", "list", "query", "version":
			continue
		}
		writes = append(writes, argv)
	}
	return writes
}

// bdRelocatedClassVerbOf is bdRelocatedClassVerb without the resolved flag.
func bdRelocatedClassVerbOf(argv []string) (string, []string) {
	verb, rest, _ := bdRelocatedClassVerb(argv)
	return verb, rest
}

// TestGcBdRefusesAClassFlipOnASplitCity is the gc bd entry point's table: the
// audit's R4 argv (zz_audit_bd_flip_test.go), inverted. On a split city every
// argv that would turn the work-store bead into an infrastructure-class bead
// exits 1 with a class refusal and bd never runs a write. On a single-store
// city every one of them reaches bd exactly as before.
func TestGcBdRefusesAClassFlipOnASplitCity(t *testing.T) {
	split := bdParityOpts{storage: bdSQLRefusalSplitStorage}
	plain := bdParityOpts{}
	args := func(a ...string) func(*testing.T, bdParityCity) []string {
		return func(*testing.T, bdParityCity) []string { return a }
	}
	withFile := func(name, body string, a ...string) func(*testing.T, bdParityCity) []string {
		return func(t *testing.T, c bdParityCity) []string {
			if err := os.WriteFile(filepath.Join(c.city, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			return a
		}
	}
	flips := []struct {
		name  string
		setup func(*testing.T, bdParityCity) []string
		want  string // a fragment the refusal must carry
	}{
		{"update --add-label wisp", args("update", "demo-abc", "--add-label", "wisp"), "graph-class"},
		{"update --add-label gc:session", args("update", "demo-abc", "--add-label", "gc:session"), "sessions-class"},
		{"update --add-label a,gc:wait", args("update", "demo-abc", "--add-label", "triage,gc:wait"), "sessions-class"},
		{"update --add-label=gc:rt", args("update", "demo-abc", "--add-label=gc:rt"), "sessions-class"},
		{"update --add-label gc:fact", args("update", "demo-abc", "--add-label", "gc:fact"), "sessions-class"},
		{"update --add-label gc:nudge", args("update", "demo-abc", "--add-label", "gc:nudge"), "nudges-class"},
		{"update --add-label order-tracking", args("update", "demo-abc", "--add-label", "order-tracking"), "orders-class"},
		{"update --add-label gc:extmsg-binding", args("update", "demo-abc", "--add-label", "gc:extmsg-binding"), "messaging-class"},
		{"update --set-labels gc:nudge", args("update", "demo-abc", "--set-labels", "gc:nudge"), "nudges-class"},
		{"update --set-metadata gc.root_bead_id", args("update", "demo-abc", "--set-metadata", "gc.root_bead_id=gcg-1"), "graph-class"},
		{"update --set-metadata gc.kind=workflow", args("update", "demo-abc", "--set-metadata", "gc.kind=workflow"), "graph-class"},
		{"update --set-metadata gc.kind=wisp", args("update", "demo-abc", "--set-metadata", "gc.kind=wisp"), "graph-class"},
		{"update --set-metadata gc.formula_contract", args("update", "demo-abc", "--set-metadata", "gc.formula_contract=graph.v2"), "graph-class"},
		{"update --metadata json", args("update", "demo-abc", "--metadata", `{"gc.kind":"wisp"}`), "graph-class"},
		{"update --type message", args("update", "demo-abc", "--type", "message"), "messaging-class"},
		{"update --type=session", args("update", "demo-abc", "--type=session"), "sessions-class"},
		// The attached shorthand: on a single-store city the exact-ID guard
		// already refuses it as an unrecognized flag (unchanged).
		{"update -twisp", args("update", "demo-abc", "-twisp"), "graph-class"},
		{"update -t convergence", args("update", "demo-abc", "-t", "convergence"), "graph-class"},
		{"update --type op", args("update", "demo-abc", "--type", "op"), "sessions-class"},
		{"update behind a root flag", args("--json", "update", "demo-abc", "--add-label", "wisp"), "graph-class"},
		{"update with no id", args("update", "--add-label", "wisp"), "name the bead explicitly"},
		{"label add", args("label", "add", "demo-abc", "gc:nudge"), "nudges-class"},
		{"label add list", args("label", "add", "demo-abc", "triage,gc:session"), "sessions-class"},
		{"label propagate", args("label", "propagate", "demo-abc", "wisp"), "graph-class"},
		{"label unknown subcommand", args("label", "set", "demo-abc", "x"), "gc bd label: refused on this split city"},
		// The fake bd answers `show` for ANY id with demo-abc, so a short id
		// resolves to a different bead: a collision, refused.
		{"short-id collision", args("update", "demo-ab", "--add-label", "triage,wisp"), "different bead"},
		{"mol fart", args("mol", "fart", "a", "b"), "alias of mol bond"},
		{"mol pour", args("mol", "pour", "mol-strand"), "gc sling"},
		{"mol wisp without --root-only", args("mol", "wisp", "mol-strand"), "without --root-only"},
		{"cook --persist=true", args("cook", "mol-strand", "--persist=true"), "--persist"},
		{"backup restore --force=true", args("backup", "restore", "/b", "--force=true"), "journal-corruption-recovery.md"},
		{"tag", args("tag", "demo-abc", "order-tracking"), "orders-class"},
		{"set-state gc=session", args("set-state", "demo-abc", "gc=session"), "sessions-class"},
		{"import a pre-split archive", withFile("archive.jsonl",
			`{"id":"demo-1","title":"w","issue_type":"task"}`+"\n"+`{"id":"demo-2","title":"s","issue_type":"session","labels":["gc:session"]}`+"\n",
			"import", "archive.jsonl"), "--skip-infra"},
		{"import with no file", args("import"), "name the file"},
		{"batch create message", withFile("ops.txt", "close demo-abc done\ncreate message 2 hello there\n", "batch", "-f", "ops.txt"), "messaging-class"},
		{"batch update an unknown key", withFile("ops.txt", "update demo-abc type=message\n", "batch", "--file=ops.txt"), "does not know"},
		{"create-form", args("create-form"), "interactively"},
		{"sql write", args("sql", "update issues set issue_type = 'message' where id = 'demo-abc'"), "single plain read statement"},
		{"sql write behind a select", args("sql", "select 1; delete from labels"), "single plain read statement"},
		{"github sync", args("github", "sync"), "external-tracker"},
	}
	for _, c := range flips {
		t.Run("split/"+c.name, func(t *testing.T) {
			res := runBdParityArm(t, false, split, c.setup)
			if res.code != 1 {
				t.Fatalf("exit %d, want 1; stderr=%q", res.code, res.stderr)
			}
			if !strings.Contains(res.stderr, c.want) {
				t.Errorf("refusal does not carry %q: %q", c.want, res.stderr)
			}
			if !strings.Contains(res.stderr, "Nothing was written") {
				t.Errorf("refusal does not say nothing was written: %q", res.stderr)
			}
			if w := bdParityWrites(res.bd); len(w) > 0 {
				t.Fatalf("bd ran %v despite the refusal", w)
			}
		})
		t.Run("single-store/"+c.name, func(t *testing.T) {
			res := runBdParityArm(t, false, plain, c.setup)
			if strings.Contains(res.stderr, "Nothing was written") {
				t.Fatalf("single-store city refused: %q", res.stderr)
			}
			if strings.Contains(res.stderr, "cannot safely verify bead IDs") || strings.Contains(res.stderr, "substring collision") {
				return // the pre-existing exact-ID guard, on both kinds of city
			}
			if w := bdParityWrites(res.bd); len(w) != 1 {
				t.Fatalf("single-store city: bd ran %v, want exactly the forwarded command; stderr=%q", w, res.stderr)
			}
		})
	}

	// Writes that leave the bead's class alone still reach bd on a split city.
	allowed := []struct {
		name  string
		setup func(*testing.T, bdParityCity) []string
	}{
		{"update an ordinary label", args("update", "demo-abc", "--add-label", "triage")},
		{"update ordinary metadata", args("update", "demo-abc", "--set-metadata", "gc.outcome=pass")},
		{"update status", args("update", "demo-abc", "--status", "in_progress")},
		{"update --type bug", args("update", "demo-abc", "--type", "bug")},
		{"label add ordinary", args("label", "add", "demo-abc", "triage")},
		{"label remove", args("label", "remove", "demo-abc", "triage")},
		{"label list", args("label", "list", "demo-abc")},
		{"tag ordinary", args("tag", "demo-abc", "needs-review")},
		{"set-state patrol=muted", args("set-state", "demo-abc", "patrol=muted")},
		{"sql read", args("sql", "select id from issues where status = 'open'")},
		{"batch close and update", withFile("ops.txt", "# maintenance\nclose demo-abc stale\nupdate demo-abc status=open priority=2\n", "batch", "-f", "ops.txt")},
		{"import a work-only archive", withFile("archive.jsonl", `{"id":"demo-1","title":"w","issue_type":"task","labels":["triage"]}`+"\n", "import", "archive.jsonl")},
		{"close", args("close", "demo-abc")},
	}
	for _, c := range allowed {
		t.Run("split/allowed/"+c.name, func(t *testing.T) {
			res := runBdParityArm(t, false, split, c.setup)
			if strings.Contains(res.stderr, "Nothing was written") {
				t.Fatalf("refused a write that leaves the class alone: %q", res.stderr)
			}
			if w := bdParityWrites(res.bd); len(w) != 1 {
				t.Fatalf("bd ran %v, want exactly the forwarded command; stderr=%q", w, res.stderr)
			}
		})
	}
}

// TestGcBdImportSkipInfraImportsOnlyTheWorkRows pins the recovery runbook on a
// split city end to end: --skip-infra hands bd a work-only copy of the archive
// (never the original path) and reports what it left out; the flag itself
// never reaches bd, on either kind of city.
func TestGcBdImportSkipInfraImportsOnlyTheWorkRows(t *testing.T) {
	archive := `{"id":"demo-1","title":"w","issue_type":"task"}` + "\n" +
		`{"id":"demo-2","title":"step","issue_type":"task","metadata":{"gc.root_bead_id":"demo-0"}}` + "\n" +
		`{"id":"demo-3","title":"s","issue_type":"session","labels":["gc:session"]}` + "\n"
	setup := func(t *testing.T, c bdParityCity) []string {
		if err := os.WriteFile(filepath.Join(c.city, "archive.jsonl"), []byte(archive), 0o644); err != nil {
			t.Fatal(err)
		}
		return []string{"import", "archive.jsonl", "--skip-infra"}
	}
	res := runBdParityArm(t, false, bdParityOpts{storage: bdSQLRefusalSplitStorage}, setup)
	if res.code != 0 {
		t.Fatalf("exit %d; stderr=%q", res.code, res.stderr)
	}
	w := bdParityWrites(res.bd)
	if len(w) != 1 || strings.Contains(w[0], "archive.jsonl") || strings.Contains(w[0], "--skip-infra") || !strings.Contains(w[0], "gc-bd-import-work-") {
		t.Fatalf("bd ran %v, want one import of the work-only copy", w)
	}
	for _, want := range []string{"importing 1 row(s)", "left out 2 row(s)", "1 graph", "1 sessions"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("notice does not carry %q: %q", want, res.stderr)
		}
	}

	res = runBdParityArm(t, false, bdParityOpts{}, setup)
	w = bdParityWrites(res.bd)
	if res.code != 0 || len(w) != 1 || !strings.Contains(w[0], "import archive.jsonl") || strings.Contains(w[0], "--skip-infra") {
		t.Fatalf("single-store: exit %d, bd ran %v; want the original import without the gc flag; stderr=%q", res.code, w, res.stderr)
	}
}

// TestGcBdByIDUpdateRefusesTurningABindingResidentIntoWork is the reverse
// direction, at the by-id door: on a split city `gc bd update` of a bead the
// class binding holds is served in process from the binding, and a write that
// would leave it a work-class bead (its only graph marker removed or blanked)
// strands it in a store work is never read from. It is refused; a write that
// keeps the class is still served.
func TestGcBdByIDUpdateRefusesTurningABindingResidentIntoWork(t *testing.T) {
	split := bdParityOpts{storage: bdSQLRefusalSplitStorage}
	seedStep := func(t *testing.T, c bdParityCity) string {
		t.Helper()
		binding, relocated, err := cliSoleClassBinding(c.city)
		if err != nil || !relocated {
			t.Fatalf("resolving the class binding: relocated=%v err=%v", relocated, err)
		}
		step, err := binding.Store.Create(beads.Bead{Title: "step", Type: "task", Metadata: map[string]string{"gc.root_bead_id": "gcg-root"}})
		if err != nil {
			t.Fatal(err)
		}
		if coordclass.Classify(step) != coordclass.ClassGraph {
			t.Fatalf("seeded step is %s class", coordclass.Classify(step))
		}
		return step.ID
	}
	for _, c := range []struct {
		name   string
		args   func(id string) []string
		refuse bool
	}{
		{"unset the only graph marker", func(id string) []string { return []string{"update", id, "--unset-metadata", "gc.root_bead_id"} }, true},
		{"blank the only graph marker", func(id string) []string { return []string{"update", id, "--set-metadata", "gc.root_bead_id="} }, true},
		{"ordinary metadata", func(id string) []string { return []string{"update", id, "--set-metadata", "gc.outcome=pass"} }, false},
		{"graph to sessions stays in the binding", func(id string) []string { return []string{"update", id, "--add-label", "gc:wait"} }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := runBdParityArm(t, false, split, func(t *testing.T, city bdParityCity) []string {
				return c.args(seedStep(t, city))
			})
			if w := bdParityWrites(res.bd); len(w) > 0 {
				t.Fatalf("a binding resident's update reached bd: %v", w)
			}
			if !c.refuse {
				if res.code != 0 {
					t.Fatalf("exit %d; stderr=%q", res.code, res.stderr)
				}
				return
			}
			if res.code != 1 {
				t.Fatalf("exit %d, want 1; stdout=%q stderr=%q", res.code, res.stdout, res.stderr)
			}
			for _, want := range []string{"graph-class bead into a work-class bead", coordclass.MintCommand(coordclass.ClassWork), "Nothing was written"} {
				if !strings.Contains(res.stderr, want) {
					t.Errorf("refusal does not carry %q: %q", want, res.stderr)
				}
			}
		})
	}
}
