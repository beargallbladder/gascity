//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// guardCase is one argv through bdClassFlipGuard on a split city.
type guardCase struct {
	name   string
	argv   []string
	before beads.Bead
	read   bdReadResult
	refuse bool
	want   string // a fragment the refusal must carry, when refused
}

func runGuardCases(t *testing.T, cases []guardCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reads := 0
			deps := bdClassGuardDeps{
				read: func(string) (beads.Bead, bdReadResult) {
					reads++
					return c.before, c.read
				},
				runBd: stubCookRunner,
			}
			g := bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), c.argv, deps)
			if c.refuse != (g.refusal != "") {
				t.Fatalf("gc bd %s: refused=%v, want %v; refusal=%q", strings.Join(c.argv, " "), g.refusal != "", c.refuse, g.refusal)
			}
			if c.refuse && !strings.Contains(g.refusal, c.want) {
				t.Errorf("refusal does not carry %q: %q", c.want, g.refusal)
			}
			// The same argv on a single-store city is never refused.
			if single := bdClassFlipGuard(&config.City{}, t.TempDir(), c.argv, deps); single.refusal != "" {
				t.Errorf("single-store city refused: %q", single.refusal)
			}
		})
	}
}

var guardTask = beads.Bead{ID: "demo-1", Type: "task"}

// TestGcBdClassGuardParsesLabelsAsBdDoes is review finding 1. bd parses
// update's --add-label/-l/--remove-label/--set-labels and create's --labels as
// ONE CSV record, so a quoted label is the bare label; reading them as a plain
// comma split let `"wisp"` through. The label VERBS split on plain commas, so
// there a quoted label really is a different label.
func TestGcBdClassGuardParsesLabelsAsBdDoes(t *testing.T) {
	runGuardCases(t, []guardCase{
		{"quoted wisp", []string{"update", "demo-1", "--add-label", `"wisp"`}, guardTask, bdReadFound, true, "graph-class"},
		{"quoted second label", []string{"update", "demo-1", "--add-label", `triage,"gc:session"`}, guardTask, bdReadFound, true, "sessions-class"},
		{"quoted set-labels", []string{"update", "demo-1", "--set-labels", `"gc:nudge"`}, guardTask, bdReadFound, true, "nudges-class"},
		{"enterprise -l", []string{"update", "demo-1", "-l", "gc:session"}, guardTask, bdReadFound, true, "sessions-class"},
		{"unparseable label list", []string{"update", "demo-1", "--add-label", `a"b`}, guardTask, bdReadFound, true, "not a label list bd can parse"},
		{"quoted ordinary label", []string{"update", "demo-1", "--add-label", `"triage"`}, guardTask, bdReadFound, false, ""},
		{"label verb keeps the quotes", []string{"label", "add", "demo-1", `"wisp"`}, guardTask, bdReadFound, false, ""},
	})
}

// TestGcBdCreateGuardParsesLabelsAsBdDoes is finding 1 on the pre-existing
// create guard: `--labels '"gc:session"'` used to classify as work.
func TestGcBdCreateGuardParsesLabelsAsBdDoes(t *testing.T) {
	cfg := splitGuardTestCity()
	for _, argv := range [][]string{
		{"create", "--title", "x", "--labels", `"gc:session"`},
		{"create", "--title", "x", "-l", `triage,"wisp"`},
		{"new", "--title", "x", "--label", `"gc:nudge"`},
		{"create", "--title", "x", "--labels", `a"b`},
	} {
		if msg, refused := bdRelocatedClassCreateRefusal(cfg, t.TempDir(), argv); !refused {
			t.Errorf("gc bd %s was not refused", strings.Join(argv, " "))
		} else if msg == "" {
			t.Errorf("gc bd %s: empty refusal", strings.Join(argv, " "))
		}
	}
	if _, refused := bdRelocatedClassCreateRefusal(cfg, t.TempDir(), []string{"create", "--title", "x", "--labels", `"triage"`}); refused {
		t.Error("a quoted work label was refused")
	}
}

// TestGcBdClassGuardSQLRefusesHiddenWrites is finding 2.
func TestGcBdClassGuardSQLRefusesHiddenWrites(t *testing.T) {
	runGuardCases(t, []guardCase{
		{"comment hides a separator", []string{"sql", "/* ; SELECT 1 */ UPDATE issues SET issue_type='message'"}, beads.Bead{}, bdReadFound, true, "comment"},
		{"executable comment", []string{"sql", "/*!40000 UPDATE issues SET issue_type='message' */"}, beads.Bead{}, bdReadFound, true, "comment"},
		{"line comment", []string{"sql", "SELECT 1 -- x\nUPDATE issues SET issue_type='message'"}, beads.Bead{}, bdReadFound, true, "comment"},
		{"hash comment", []string{"sql", "SELECT 1 # x"}, beads.Bead{}, bdReadFound, true, "comment"},
		{"two statements", []string{"sql", "select 1; update issues set issue_type='message'"}, beads.Bead{}, bdReadFound, true, "more than one statement"},
		{"dolt procedure as a function", []string{"sql", "SELECT DOLT_MERGE('pre-split')"}, beads.Bead{}, bdReadFound, true, "Dolt procedure"},
		{"explain wraps a write", []string{"sql", "EXPLAIN UPDATE issues SET issue_type='message'"}, beads.Bead{}, bdReadFound, true, "not a plain SELECT"},
		{"unknown flag", []string{"sql", "--weird", "select 1"}, beads.Bead{}, bdReadFound, true, "--weird"},
		{"--csv read", []string{"sql", "--csv", "select id from issues"}, beads.Bead{}, bdReadFound, false, ""},
		{"--json read, trailing semicolon", []string{"sql", "--json", "SELECT COUNT(*) FROM issues;"}, beads.Bead{}, bdReadFound, false, ""},
		{"dolt system table read", []string{"sql", "select * from dolt_log"}, beads.Bead{}, bdReadFound, false, ""},
		{"core pack derived-table read", []string{"sql", "--csv", "SELECT * FROM (\nWITH c AS (SELECT id FROM issues) SELECT COUNT(*) FROM c\n) gc_scope_read"}, beads.Bead{}, bdReadFound, false, ""},
	})
}

// TestGcBdClassGuardModelsMetadataNull is finding 3: bd 1.3.2 stores
// --set-metadata k=null as JSON null (reads back absent), the Enterprise build
// as the string "null". Clearing gc.synthetic on a synthetic convoy that
// carries gc.root_bead_id unmasks the graph marker under the first reading.
func TestGcBdClassGuardModelsMetadataNull(t *testing.T) {
	convoy := beads.Bead{ID: "demo-2", Type: "convoy", Metadata: map[string]string{"gc.synthetic": "true", "gc.root_bead_id": "gcg-1"}}
	runGuardCases(t, []guardCase{
		{"null unmasks the graph marker", []string{"update", "demo-2", "--set-metadata", "gc.synthetic=null"}, convoy, bdReadFound, true, "graph-class"},
		{"empty unmasks it too", []string{"update", "demo-2", "--set-metadata", "gc.synthetic="}, convoy, bdReadFound, true, "graph-class"},
		{"null of an absent marker", []string{"update", "demo-1", "--set-metadata", "gc.kind=null"}, guardTask, bdReadFound, false, ""},
	})
}

// TestGcBdClassGuardJudgesAnUnreadableBead is findings 5 (M24) and 6: a bead
// the store could not read is still judged, never skipped; one it answers is
// absent is judged as an empty work bead; and a removal against a bead that
// could not be read is refused, because only the bead can say what it unmasks.
func TestGcBdClassGuardJudgesAnUnreadableBead(t *testing.T) {
	runGuardCases(t, []guardCase{
		{"absent bead, marker add", []string{"update", "demo-9", "--add-label", "wisp"}, beads.Bead{}, bdReadNotFound, true, "graph-class"},
		{"failed read, marker add", []string{"update", "demo-9", "--add-label", "gc:session"}, beads.Bead{}, bdReadFailed, true, "sessions-class"},
		{"failed read, marker removal", []string{"update", "demo-9", "--remove-label", "gc:session"}, beads.Bead{}, bdReadFailed, true, "could not read"},
		{"failed read, marker unset", []string{"update", "demo-9", "--unset-metadata", "gc.synthetic"}, beads.Bead{}, bdReadFailed, true, "could not read"},
		{"failed read, set-labels", []string{"update", "demo-9", "--set-labels", "triage"}, beads.Bead{}, bdReadFailed, true, "could not read"},
		{"failed read, set-state", []string{"set-state", "demo-9", "patrol=muted"}, beads.Bead{}, bdReadFailed, true, "could not read"},
		{"absent bead, removal", []string{"update", "demo-9", "--remove-label", "gc:session"}, beads.Bead{}, bdReadNotFound, false, ""},
		{"failed read, ordinary write", []string{"update", "demo-9", "--add-label", "triage", "--set-metadata", "gc.outcome="}, beads.Bead{}, bdReadFailed, false, ""},
	})
}

// TestGcBdClassGuardKnowsTheFleetsFlags is finding 7: an ordinary update
// spelled with the Enterprise build's flags must not be refused as unknown.
func TestGcBdClassGuardKnowsTheFleetsFlags(t *testing.T) {
	runGuardCases(t, []guardCase{
		{"--if-revision", []string{"update", "demo-1", "--if-revision", "7", "--add-label", "triage"}, guardTask, bdReadFound, false, ""},
		{"--clear-notes", []string{"update", "demo-1", "--clear-notes", "--add-label", "triage"}, guardTask, bdReadFound, false, ""},
		{"--profile in front", []string{"--profile", "team", "update", "demo-1", "--add-label", "wisp"}, guardTask, bdReadFound, true, "graph-class"},
		{"--server-url in front", []string{"--server-url", "http://x", "update", "demo-1", "--add-label", "triage"}, guardTask, bdReadFound, false, ""},
	})
}

// TestGcBdClassGuardServeAndBackup is finding 8.
func TestGcBdClassGuardServeAndBackup(t *testing.T) {
	runGuardCases(t, []guardCase{
		{"serve", []string{"serve"}, beads.Bead{}, bdReadFound, true, "write API"},
		{"backup restore", []string{"backup", "restore", "/b", "--force"}, beads.Bead{}, bdReadFound, true, bdBackupPostdatesSplitFlag},
		{"backup restore, attested", []string{"backup", "restore", "/b", bdBackupPostdatesSplitFlag}, beads.Bead{}, bdReadFound, false, ""},
		{"backup status", []string{"backup", "status", "--json"}, beads.Bead{}, bdReadFound, false, ""},
	})
	g := bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), []string{"backup", "restore", "/b", bdBackupPostdatesSplitFlag}, bdClassGuardDeps{})
	if strings.Contains(strings.Join(g.args, " "), bdBackupPostdatesSplitFlag) || g.notice == "" {
		t.Fatalf("attested restore forwarded %v (notice %q); want gc's flag consumed and a notice", g.args, g.notice)
	}
}

// TestGcBdClassGuardClassifiesFormulaSteps is finding 9: bd copies each
// formula step's type, labels and metadata onto the beads pour/wisp/bond and
// cook --persist create, so every resolved step is classified first.
func TestGcBdClassGuardClassifiesFormulaSteps(t *testing.T) {
	runGuardCases(t, []guardCase{
		{"pour", []string{"mol", "pour", "infra-formula", "--var", "x=1"}, beads.Bead{}, bdReadNotFound, true, `step "graph"`},
		{"wisp", []string{"mol", "wisp", "infra-formula", "--root-only"}, beads.Bead{}, bdReadNotFound, true, "graph-class"},
		{"wisp create", []string{"mol", "wisp", "create", "infra-formula"}, beads.Bead{}, bdReadNotFound, true, "graph-class"},
		{"bond", []string{"mol", "bond", "work-formula", "infra-formula"}, beads.Bead{}, bdReadNotFound, true, "graph-class"},
		{"pour --attach", []string{"mol", "pour", "work-formula", "--attach", "infra-formula"}, beads.Bead{}, bdReadNotFound, true, "graph-class"},
		{"cook --persist", []string{"cook", "infra-formula", "--persist"}, beads.Bead{}, bdReadNotFound, true, "graph-class"},
		{"unresolvable", []string{"mol", "pour", "nothing-here"}, beads.Bead{}, bdReadNotFound, true, "could not resolve"},
		{"existing work proto", []string{"mol", "pour", "demo-proto"}, beads.Bead{ID: "demo-proto", Type: "molecule"}, bdReadFound, false, ""},
		{"existing graph proto", []string{"mol", "pour", "demo-proto"}, beads.Bead{ID: "demo-proto", Type: "molecule", Labels: []string{"gc:wisp"}}, bdReadFound, true, "graph-class"},
		{"work formula", []string{"mol", "wisp", "work-formula", "--root-only"}, beads.Bead{}, bdReadNotFound, false, ""},
		{"dry run", []string{"mol", "pour", "infra-formula", "--dry-run"}, beads.Bead{}, bdReadNotFound, false, ""},
		{"cook without --persist", []string{"cook", "infra-formula"}, beads.Bead{}, bdReadNotFound, false, ""},
		{"wisp list", []string{"mol", "wisp", "list"}, beads.Bead{}, bdReadNotFound, false, ""},
	})
}

// bdFormulaFakeBd is the parity fake bd, plus a `cook` that prints a resolved
// formula whose one step carries gc.root_bead_id and labels=["gc:session"].
const bdFormulaFakeBd = `#!/bin/sh
{
  printf 'argv=%s\n' "$*"
  printf -- '--\n'
} >> "${BD_PARITY_LOG}.$$"
case "$1" in
  cook) echo '{"formula":"mol-strand","version":1,"steps":[{"id":"bad","title":"b","metadata":{"gc.root_bead_id":"gcg-1"},"labels":["gc:session"]}]}' ;;
  show) echo '[]'; exit 3 ;;
esac
exit 0
`

// TestGcBdMolPourRefusesAnInfraShapedFormulaEndToEnd drives finding 9 through
// doBd with a fake bd: the guard asks bd to cook the formula, sees the step
// that would be minted as an infrastructure bead, and bd never pours.
func TestGcBdMolPourRefusesAnInfraShapedFormulaEndToEnd(t *testing.T) {
	setup := func(t *testing.T, c bdParityCity) []string {
		if err := os.WriteFile(filepath.Join(c.bin, "bd"), []byte(bdFormulaFakeBd), 0o755); err != nil {
			t.Fatal(err)
		}
		return []string{"mol", "pour", "mol-strand"}
	}
	res := runBdParityArm(t, false, bdParityOpts{storage: bdSQLRefusalSplitStorage}, setup)
	if res.code != 1 || !strings.Contains(res.stderr, `step "bad"`) {
		t.Fatalf("exit %d, stderr=%q; want the formula-step refusal", res.code, res.stderr)
	}
	if !strings.Contains(res.bd, "argv=cook mol-strand") {
		t.Errorf("the guard did not resolve the formula through bd cook; bd log:\n%s", res.bd)
	}
	if strings.Contains(res.bd, "argv=mol pour") {
		t.Fatalf("bd poured despite the refusal; bd log:\n%s", res.bd)
	}
	res = runBdParityArm(t, false, bdParityOpts{}, setup)
	if res.code != 0 || !strings.Contains(res.bd, "argv=mol pour mol-strand") || strings.Contains(res.bd, "argv=cook") {
		t.Fatalf("single-store: exit %d; want the pour forwarded untouched; bd log:\n%s", res.code, res.bd)
	}
}

// TestGcBdImportDryRunReportsAndSkipInfraNamesDependents covers two review
// nits: an import --dry-run writes nothing, so it is reported rather than
// refused; and --skip-infra names the kept work rows whose dependencies point
// at a left-out infrastructure id.
func TestGcBdImportDryRunReportsAndSkipInfraNamesDependents(t *testing.T) {
	archive := `{"id":"demo-1","title":"w","issue_type":"task","dependencies":[{"issue_id":"demo-1","depends_on_id":"demo-3","type":"blocks"}]}` + "\n" +
		`{"id":"demo-2","title":"w2","issue_type":"task"}` + "\n" +
		`{"id":"demo-3","title":"s","issue_type":"session"}` + "\n"
	scope := t.TempDir()
	if err := os.WriteFile(filepath.Join(scope, "a.jsonl"), []byte(archive), 0o644); err != nil {
		t.Fatal(err)
	}
	read := func(string) (beads.Bead, bdReadResult) { return beads.Bead{}, bdReadNotFound }
	g := bdClassFlipGuard(splitGuardTestCity(), scope, []string{"import", "a.jsonl", "--dry-run"}, bdClassGuardDeps{read: read})
	if g.refusal != "" || !strings.Contains(g.notice, "dry run") || strings.Join(g.args, " ") != "import a.jsonl --dry-run" {
		t.Fatalf("dry run: refusal=%q notice=%q args=%v", g.refusal, g.notice, g.args)
	}
	g = bdClassFlipGuard(splitGuardTestCity(), scope, []string{"import", "a.jsonl", "--skip-infra"}, bdClassGuardDeps{read: read})
	if g.cleanup != nil {
		defer g.cleanup()
	}
	if g.refusal != "" || !strings.Contains(g.notice, "1 kept work row(s) carry a dependency") || !strings.Contains(g.notice, "demo-1") {
		t.Fatalf("skip-infra: refusal=%q notice=%q", g.refusal, g.notice)
	}
}

// casGetCountingStore counts Gets and can fail them.
type casGetCountingStore struct {
	beads.Store
	gets   int
	getErr error
}

func (s *casGetCountingStore) Get(id string) (beads.Bead, error) {
	s.gets++
	if s.getErr != nil {
		return beads.Bead{}, s.getErr
	}
	return s.Store.Get(id)
}

// TestBeadsMetadataCASClassGuardReadsOnlyWhenItMatters covers the
// metadata-cas nit and finding 6 there: a key no Classify arm reads is never
// pre-read, and clearing a class key on a bead the store could not read is
// refused.
func TestBeadsMetadataCASClassGuardReadsOnlyWhenItMatters(t *testing.T) {
	holds := workStoreClassHolds(splitGuardTestCity())
	store := &casGetCountingStore{Store: beads.NewMemStore()}
	if refusal := beadsMetadataCASClassFlipRefusal(store, beadsMetadataCASRequest{beadID: "demo-1", key: "review_sha", next: "x"}, holds); refusal != "" || store.gets != 0 {
		t.Fatalf("ordinary key: refusal=%q gets=%d; want no refusal and no read", refusal, store.gets)
	}
	store.getErr = errors.New("bd unavailable")
	if refusal := beadsMetadataCASClassFlipRefusal(store, beadsMetadataCASRequest{beadID: "demo-1", key: "gc.synthetic", next: ""}, holds); !strings.Contains(refusal, "could not read") {
		t.Fatalf("clearing a class key on an unreadable bead: refusal=%q", refusal)
	}
	if refusal := beadsMetadataCASClassFlipRefusal(store, beadsMetadataCASRequest{beadID: "demo-1", key: "gc.root_bead_id", next: "gcg-1"}, holds); !strings.Contains(refusal, "graph-class") {
		t.Fatalf("setting a graph marker on an unreadable bead: refusal=%q", refusal)
	}
}

// stubCookRunner answers `bd cook <name>` with testFormulaJSON, and fails the
// way bd does (non-zero, nothing printed) for a name that is not a formula.
func stubCookRunner(argv []string) ([]byte, error) {
	out := testFormulaJSON(argv)
	if out == "" {
		return nil, errors.New("bd cook: formula not found")
	}
	return []byte(out), nil
}
