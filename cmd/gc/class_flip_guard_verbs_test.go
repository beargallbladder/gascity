package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// splitGuardTestCity is a whole-split city: every infrastructure class on one
// binding, work on the work stores.
func splitGuardTestCity() *config.City {
	return &config.City{Storage: &config.StorageConfig{
		Classes: config.StorageClasses{
			Work: config.StorageWorkBinding, Graph: "infra", Sessions: "infra",
			Messaging: "infra", Orders: "infra", Nudges: "infra",
		},
		Bindings: map[string]config.StorageBindingConfig{"infra": {Provider: config.StorageProviderSQLiteBeads}},
	}}
}

// bdVerbClassVerdict is why a gc bd verb cannot strand a bead by flipping its
// class on a split city.
type bdVerbClassVerdict int

const (
	// verdictGuarded: the class guard (bdClassGuardVerbs) judges it.
	verdictGuarded bdVerbClassVerdict = iota
	// verdictCreateGuard: a create from argv, judged by
	// bdRelocatedClassCreateRefusal before anything is written.
	verdictCreateGuard
	// verdictNoClassField: it writes existing beads, but only fields
	// coordclass.Classify does not read (status, assignee, priority, text,
	// edges, ids, the ephemeral bit).
	verdictNoClassField
	// verdictMints: it mints NEW beads whose shape bd fixes itself (a gate,
	// a swarm molecule, a todo task) and copies from no formula; it never
	// rewrites an existing bead's type, labels or metadata, and bd never
	// stamps a classifier marker (no gc:* label, no gc.* key, no wisp type or
	// label). The verbs that copy a formula's step shapes (mol, cook
	// --persist) are guarded instead.
	verdictMints
	// verdictReadOnly: it reads beads and writes none.
	verdictReadOnly
	// verdictNoBeadRows: it touches no bead row (config, kv, memories,
	// server, setup, help, auth, storage maintenance that only deletes or
	// compacts).
	verdictNoBeadRows
	// verdictReplication: it moves whole revisions of the SAME ledger
	// (branches, remotes, federation) — the split city's own, post-split
	// history; it applies no per-bead type, label or metadata write. A backup
	// restore can bring back a PRE-split revision, so backup is guarded.
	verdictReplication
)

// bdVerbClassRow is one verb's row: its verdict, and why. A guarded row also
// names an argv the guard must refuse on a split city (flip) and one it must
// pass (keep), plus the stdin they read and a file they name, if any.
type bdVerbClassRow struct {
	verdict    bdVerbClassVerdict
	why        string
	flip, keep []string
	stdin      string
	file       string
}

// bdVerbClassRows has a row for every top-level bd verb (bdflags.TopLevelVerbs)
// and every verb gc serves itself. TestGcBdEveryVerbIsGuardedOrCannotFlipClass
// fails when bd grows a verb without a row here.
var bdVerbClassRows = map[string]bdVerbClassRow{
	// Guarded.
	"update":      {verdict: verdictGuarded, flip: []string{"update", "demo-1", "--add-label", "gc:session"}, keep: []string{"update", "demo-1", "--add-label", "triage"}},
	"label":       {verdict: verdictGuarded, flip: []string{"label", "add", "demo-1", "wisp"}, keep: []string{"label", "add", "demo-1", "triage"}},
	"tag":         {verdict: verdictGuarded, flip: []string{"tag", "demo-1", "gc:nudge"}, keep: []string{"tag", "demo-1", "triage"}},
	"set-state":   {verdict: verdictGuarded, flip: []string{"set-state", "demo-1", "gc=wait"}, keep: []string{"set-state", "demo-1", "patrol=muted"}},
	"import":      {verdict: verdictGuarded, flip: []string{"import", "-"}, keep: []string{"import", "work.jsonl"}, stdin: `{"id":"demo-2","issue_type":"message"}` + "\n", file: `{"id":"demo-3","issue_type":"task"}` + "\n"},
	"batch":       {verdict: verdictGuarded, flip: []string{"batch"}, keep: []string{"batch", "-f", "work.jsonl"}, stdin: "create session 2 s\n", file: "close demo-1 done\n"},
	"create-form": {verdict: verdictGuarded, flip: []string{"create-form"}},
	"sql":         {verdict: verdictGuarded, flip: []string{"sql", "update issues set issue_type='message'"}, keep: []string{"sql", "select id from issues"}},
	"mol":         {verdict: verdictGuarded, flip: []string{"mol", "pour", "infra-formula"}, keep: []string{"mol", "wisp", "work-formula", "--root-only"}},
	"cook":        {verdict: verdictGuarded, flip: []string{"cook", "infra-formula", "--persist"}, keep: []string{"cook", "infra-formula"}},
	"serve":       {verdict: verdictGuarded, flip: []string{"serve"}},
	"backup":      {verdict: verdictGuarded, flip: []string{"backup", "restore", "/b"}, keep: []string{"backup", "restore", "/b", "--backup-postdates-split"}},
	"jira":        {verdict: verdictGuarded, flip: []string{"jira", "sync"}},
	"linear":      {verdict: verdictGuarded, flip: []string{"linear", "sync"}},
	"github":      {verdict: verdictGuarded, flip: []string{"github", "sync"}},
	"gitlab":      {verdict: verdictGuarded, flip: []string{"gitlab", "sync"}},
	"ado":         {verdict: verdictGuarded, flip: []string{"ado", "sync"}},
	"notion":      {verdict: verdictGuarded, flip: []string{"notion", "sync"}},

	// The create guard (new is create's alias).
	"create": {verdict: verdictCreateGuard, why: "bdRelocatedClassCreateRefusal classifies the argv/--graph shape and refuses -f before anything is written"},
	"q":      {verdict: verdictCreateGuard, why: "quick capture is create's mint through -t/-l; bdRelocatedClassCreateRefusal judges it"},

	// Writes, but no field Classify reads.
	"assign":             {verdict: verdictNoClassField, why: "sets the assignee"},
	"close":              {verdict: verdictNoClassField, why: "status and close reason; binding residents are served by the by-id door"},
	"comment":            {verdict: verdictNoClassField, why: "adds a comment row"},
	"comments":           {verdict: verdictNoClassField, why: "lists or adds comment rows"},
	"defer":              {verdict: verdictNoClassField, why: "status and defer_until"},
	"undefer":            {verdict: verdictNoClassField, why: "status and defer_until"},
	"delete":             {verdict: verdictNoClassField, why: "removes beads; a deleted bead has no class to flip"},
	"dep":                {verdict: verdictNoClassField, why: "dependency edges only (its -t is the edge type, not the bead type)"},
	"link":               {verdict: verdictNoClassField, why: "dep add shorthand: an edge"},
	"duplicate":          {verdict: verdictNoClassField, why: "closes the bead and records an edge"},
	"duplicates":         {verdict: verdictNoClassField, why: "finds duplicates and optionally merges them: closes and edges"},
	"supersede":          {verdict: verdictNoClassField, why: "closes the bead and records an edge"},
	"edit":               {verdict: verdictNoClassField, why: "edits title, description, design, notes or acceptance in $EDITOR"},
	"heartbeat":          {verdict: verdictNoClassField, why: "pushes the claim lease forward"},
	"note":               {verdict: verdictNoClassField, why: "appends notes"},
	"priority":           {verdict: verdictNoClassField, why: "sets the priority"},
	"promote":            {verdict: verdictNoClassField, why: "moves a wisp row to the permanent table with its labels, type and metadata unchanged; binding residents are refused by the by-id door"},
	"reclaim":            {verdict: verdictNoClassField, why: "returns stale in_progress beads to ready: status and assignee"},
	"unclaim":            {verdict: verdictNoClassField, why: "status and assignee"},
	"reopen":             {verdict: verdictNoClassField, why: "status; binding residents are served by the by-id door"},
	"rename":             {verdict: verdictNoClassField, why: "changes the id; Classify reads no id"},
	"rename-prefix":      {verdict: verdictNoClassField, why: "changes every id's prefix; Classify reads no id"},
	"restore":            {verdict: verdictNoClassField, why: "restores compacted description/design/notes/acceptance"},
	"ship":               {verdict: verdictNoClassField, why: "adds a provides:<capability> label, a family no Classify arm matches"},
	"recompute-blocked":  {verdict: verdictNoClassField, why: "recomputes the is_blocked flag"},
	"migrate-personal":   {verdict: verdictNoClassField, why: "moves personal issues out to a planning repo (source_repo)"},
	"release-if-current": {verdict: verdictNoClassField, why: "gc's own verb: releases a claim (assignee/status) through the store"},

	// Mints new beads; never rewrites an existing bead's class fields.
	"gate":       {verdict: verdictMints, why: "creates gate beads (type gate, work class), registers waiters, resolves (closes)"},
	"swarm":      {verdict: verdictMints, why: "creates a swarm molecule for an epic; status/list/validate read"},
	"merge-slot": {verdict: verdictMints, why: "creates the merge-slot gate bead and acquires/releases it (holder fields)"},
	"todo":       {verdict: verdictMints, why: "add mints a task, done closes it, list reads"},
	"epic":       {verdict: verdictMints, why: "close-eligible closes epics; status reads"},

	// Read-only.
	"blocked":         {verdict: verdictReadOnly, why: "lists blocked beads"},
	"children":        {verdict: verdictReadOnly, why: "lists children"},
	"count":           {verdict: verdictReadOnly, why: "counts beads"},
	"diff":            {verdict: verdictReadOnly, why: "diffs revisions"},
	"export":          {verdict: verdictReadOnly, why: "writes JSONL out of the ledger"},
	"find-duplicates": {verdict: verdictReadOnly, why: "reports similar beads"},
	"graph":           {verdict: verdictReadOnly, why: "renders the dependency graph"},
	"history":         {verdict: verdictReadOnly, why: "shows a bead's history"},
	"info":            {verdict: verdictReadOnly, why: "database information"},
	"lint":            {verdict: verdictReadOnly, why: "checks template sections"},
	"list":            {verdict: verdictReadOnly, why: "lists beads (its --type/--label are filters)"},
	"orphans":         {verdict: verdictReadOnly, why: "reports orphaned beads"},
	"query":           {verdict: verdictReadOnly, why: "query language over beads"},
	"ready":           {verdict: verdictReadOnly, why: "the ready frontier"},
	"search":          {verdict: verdictReadOnly, why: "text search"},
	"show":            {verdict: verdictReadOnly, why: "shows beads"},
	"stale":           {verdict: verdictReadOnly, why: "lists stale beads"},
	"state":           {verdict: verdictReadOnly, why: "reads a state dimension"},
	"status":          {verdict: verdictReadOnly, why: "database overview"},
	"statuses":        {verdict: verdictReadOnly, why: "lists valid statuses"},
	"types":           {verdict: verdictReadOnly, why: "lists valid types"},
	"where":           {verdict: verdictReadOnly, why: "shows the active beads location"},
	"context":         {verdict: verdictReadOnly, why: "shows backend identity"},
	"preflight":       {verdict: verdictReadOnly, why: "PR readiness checklist"},
	"schema":          {verdict: verdictReadOnly, why: "prints the JSON schema"},

	// No bead rows.
	"admin":          {verdict: verdictNoBeadRows, why: "database maintenance: cleanup/compact delete or squash, reset destroys"},
	"audit":          {verdict: verdictNoBeadRows, why: "appends to the interactions.jsonl sidecar"},
	"bootstrap":      {verdict: verdictNoBeadRows, why: "database setup for a fresh clone"},
	"compact":        {verdict: verdictNoBeadRows, why: "squashes Dolt history"},
	"completion":     {verdict: verdictNoBeadRows, why: "prints a shell completion script"},
	"config":         {verdict: verdictNoBeadRows, why: "configuration settings"},
	"doctor":         {verdict: verdictNoBeadRows, why: "installation health checks and repairs"},
	"events":         {verdict: verdictNoBeadRows, why: "reads and prunes the events journal"},
	"flatten":        {verdict: verdictNoBeadRows, why: "squashes all Dolt history"},
	"forget":         {verdict: verdictNoBeadRows, why: "removes a memory"},
	"formula":        {verdict: verdictNoBeadRows, why: "lists, shows and converts formula files"},
	"gc":             {verdict: verdictNoBeadRows, why: "decays old closed beads, compacts and garbage-collects Dolt"},
	"help":           {verdict: verdictNoBeadRows, why: "help text"},
	"hooks":          {verdict: verdictNoBeadRows, why: "git hooks"},
	"human":          {verdict: verdictNoBeadRows, why: "help text"},
	"init":           {verdict: verdictNoBeadRows, why: "initializes a database"},
	"init-safety":    {verdict: verdictNoBeadRows, why: "help text"},
	"kv":             {verdict: verdictNoBeadRows, why: "the key-value table"},
	"mail":           {verdict: verdictNoBeadRows, why: "delegates to the configured mail program"},
	"memories":       {verdict: verdictNoBeadRows, why: "lists memories"},
	"metrics":        {verdict: verdictNoBeadRows, why: "usage-metrics settings"},
	"migrate":        {verdict: verdictNoBeadRows, why: "schema and backend migrations; migrate issues rewrites source_repo"},
	"onboard":        {verdict: verdictNoBeadRows, why: "help text"},
	"ping":           {verdict: verdictNoBeadRows, why: "connectivity check"},
	"prime":          {verdict: verdictNoBeadRows, why: "prints workflow context"},
	"provenance":     {verdict: verdictNoBeadRows, why: "the append-only provenance log, not bead fields"},
	"prune":          {verdict: verdictNoBeadRows, why: "deletes old closed beads"},
	"purge":          {verdict: verdictNoBeadRows, why: "deletes closed ephemeral beads"},
	"quickstart":     {verdict: verdictNoBeadRows, why: "help text"},
	"recall":         {verdict: verdictNoBeadRows, why: "reads a memory"},
	"remember":       {verdict: verdictNoBeadRows, why: "stores a memory"},
	"repo":           {verdict: verdictNoBeadRows, why: "multi-repo configuration"},
	"rules":          {verdict: verdictNoBeadRows, why: "audits Claude rules files"},
	"setup":          {verdict: verdictNoBeadRows, why: "editor integration setup"},
	"upgrade":        {verdict: verdictNoBeadRows, why: "bd version upgrades"},
	"version":        {verdict: verdictNoBeadRows, why: "prints the version"},
	"worktree":       {verdict: verdictNoBeadRows, why: "git worktrees"},
	"codex-hook":     {verdict: verdictNoBeadRows, why: "hidden editor hook"},
	"cursor-hook":    {verdict: verdictNoBeadRows, why: "hidden editor hook"},
	"db-proxy-child": {verdict: verdictNoBeadRows, why: "hidden database proxy process"},
	"send-metrics":   {verdict: verdictNoBeadRows, why: "hidden metrics sender"},
	"companion":      {verdict: verdictNoBeadRows, why: "enterprise Companion installer"},
	"connect":        {verdict: verdictNoBeadRows, why: "attaches the workspace to a bd serve"},
	"getToken":       {verdict: verdictNoBeadRows, why: "enterprise credential minting"},
	"login":          {verdict: verdictNoBeadRows, why: "enterprise login"},
	"logout":         {verdict: verdictNoBeadRows, why: "enterprise logout"},
	"profile":        {verdict: verdictNoBeadRows, why: "named storage profiles"},
	"whoami":         {verdict: verdictNoBeadRows, why: "enterprise identity"},

	// Replication of the same ledger.
	"branch":     {verdict: verdictReplication, why: "Dolt branches"},
	"conflicts":  {verdict: verdictReplication, why: "resolves merge conflicts between revisions"},
	"dolt":       {verdict: verdictReplication, why: "Dolt server, remotes, push and pull"},
	"federation": {verdict: verdictReplication, why: "peer-to-peer sync of the same workspace"},
	"sync":       {verdict: verdictReplication, why: "pull, merge, push"},
	"vc":         {verdict: verdictReplication, why: "Dolt commit/merge/status"},
}

// TestGcBdEveryVerbIsGuardedOrCannotFlipClass is the gc bd verb coverage test.
// The passthrough forwards ANY bd verb, so every verb bd has must have a row:
// guarded (and then the guard must refuse its flip on a split city and pass it
// on a single-store city), or a stated reason it cannot change an existing
// bead's class. A verb bd adds reaches bdflags.TopLevelVerbs through
// TestBdTopLevelVerbsCurrent and then fails here until it has a row.
func TestGcBdEveryVerbIsGuardedOrCannotFlipClass(t *testing.T) {
	inventory := map[string]bool{}
	for _, v := range bdflags.TopLevelVerbs() {
		inventory[v] = true
	}
	gcServed := map[string]bool{"release-if-current": true}
	var missing []string
	for v := range inventory {
		if _, ok := bdVerbClassRows[v]; !ok {
			missing = append(missing, v)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("gc bd verb(s) with no class-guard row: %v. Add a row to bdVerbClassRows: guard the verb in bdClassGuardVerbs, or state why it cannot change a bead's class.", missing)
	}
	for v := range bdVerbClassRows {
		if !inventory[v] && !gcServed[v] {
			t.Errorf("row %q names no bd verb in bdflags.TopLevelVerbs", v)
		}
	}
	aliasOf := map[string][]string{}
	for alias, verb := range bdflags.TopLevelAliases() {
		aliasOf[verb] = append(aliasOf[verb], alias)
	}

	// The guarded rows and the guard agree, both ways.
	for v, row := range bdVerbClassRows {
		_, guarded := bdClassGuardVerbs[v]
		if guarded != (row.verdict == verdictGuarded) {
			t.Errorf("verb %q: row says guarded=%v, bdClassGuardVerbs says %v", v, row.verdict == verdictGuarded, guarded)
		}
		if row.verdict != verdictGuarded && strings.TrimSpace(row.why) == "" {
			t.Errorf("verb %q is not guarded and states no reason it cannot flip a class", v)
		}
	}
	for v := range bdClassGuardVerbs {
		if _, ok := bdVerbClassRows[v]; !ok {
			t.Errorf("bdClassGuardVerbs guards %q, which has no row", v)
		}
	}
	// The create guard's verbs are exactly the create-guard rows (plus the
	// alias).
	for v := range bdRelocatedClassCreateVerbs {
		canonical := v
		if target, ok := bdflags.TopLevelAliases()[v]; ok {
			canonical = target
		}
		if bdVerbClassRows[canonical].verdict != verdictCreateGuard {
			t.Errorf("create verb %q (%q) is not a create-guard row", v, canonical)
		}
	}

	// A verb said to write no class field has no class flag in its pinned
	// manifest, where one exists.
	classFlags := []string{"--add-label", "--remove-label", "--set-labels", "--set-metadata", "--unset-metadata", "--metadata", "--labels", "-l", "--label"}
	for v, row := range bdVerbClassRows {
		if row.verdict != verdictNoClassField || !bdflags.Known(v) {
			continue
		}
		manifest := bdflags.ValueFlags(v)
		for _, f := range classFlags {
			if manifest[f] {
				t.Errorf("verb %q is filed as writing no class field, but its manifest has %s", v, f)
			}
		}
		if manifest["--type"] && v != "dep" {
			t.Errorf("verb %q is filed as writing no class field, but its manifest has --type", v)
		}
	}

	// Each guarded row's flip is refused on a split city and forwarded on a
	// single-store one; its keep is forwarded on both.
	work := beads.Bead{ID: "demo-1", Type: "task"}
	read := func(id string) (beads.Bead, bdReadResult) {
		if strings.HasSuffix(id, "-formula") {
			return beads.Bead{}, bdReadNotFound
		}
		return work, bdReadFound
	}
	runBd := stubCookRunner
	for v, row := range bdVerbClassRows {
		if row.verdict != verdictGuarded {
			continue
		}
		t.Run(v, func(t *testing.T) {
			scope := t.TempDir()
			if row.file != "" {
				if err := os.WriteFile(filepath.Join(scope, "work.jsonl"), []byte(row.file), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			run := func(cfg *config.City, argv []string) bdClassGuard {
				g := bdClassFlipGuard(cfg, scope, argv, bdClassGuardDeps{stdin: bytes.NewBufferString(row.stdin), read: read, runBd: runBd})
				if g.cleanup != nil {
					t.Cleanup(g.cleanup)
				}
				return g
			}
			if g := run(splitGuardTestCity(), row.flip); g.refusal == "" {
				t.Errorf("split city: gc bd %s was not refused", strings.Join(row.flip, " "))
			}
			// An alias bd runs as this verb is refused the same way.
			for _, alias := range aliasOf[v] {
				aliased := append([]string{alias}, row.flip[1:]...)
				if g := run(splitGuardTestCity(), aliased); g.refusal == "" {
					t.Errorf("split city: gc bd %s (alias of %s) was not refused", strings.Join(aliased, " "), v)
				}
			}
			if g := run(&config.City{}, row.flip); g.refusal != "" {
				t.Errorf("single-store city: gc bd %s was refused: %s", strings.Join(row.flip, " "), g.refusal)
			}
			if row.keep == nil {
				return
			}
			for name, cfg := range map[string]*config.City{"split": splitGuardTestCity(), "single-store": {}} {
				if g := run(cfg, row.keep); g.refusal != "" {
					t.Errorf("%s city: gc bd %s was refused: %s", name, strings.Join(row.keep, " "), g.refusal)
				}
			}
		})
	}
}

// TestStoreClassHoldsIsNilOnASingleStoreCity pins the single-store contract
// every CLI door relies on: no [storage], or one that keeps every class on the
// work binding, holds nil, and nil disarms coordclass.CheckFlip.
func TestStoreClassHoldsIsNilOnASingleStoreCity(t *testing.T) {
	allWork := &config.City{Storage: &config.StorageConfig{Classes: config.StorageClasses{
		Work: config.StorageWorkBinding, Graph: config.StorageWorkBinding, Sessions: config.StorageWorkBinding,
		Messaging: config.StorageWorkBinding, Orders: config.StorageWorkBinding, Nudges: config.StorageWorkBinding,
	}}}
	for name, cfg := range map[string]*config.City{"nil": nil, "no storage": {}, "all on work": allWork} {
		if got := workStoreClassHolds(cfg); got != nil {
			t.Errorf("%s: work store holds %v, want nil", name, got)
		}
	}
	if got := workStoreClassHolds(splitGuardTestCity()); len(got) != 1 || got[0] != coordclass.ClassWork {
		t.Errorf("split: work store holds %v, want [work]", got)
	}
	if got := storeClassHolds(splitGuardTestCity(), "infra"); len(got) != 5 {
		t.Errorf("split: binding holds %v, want the five infrastructure classes", got)
	}
}

// TestGcBdImportSkipInfraWritesAWorkOnlyCopy checks what --skip-infra hands
// bd, line by line, for a file and for stdin.
func TestGcBdImportSkipInfraWritesAWorkOnlyCopy(t *testing.T) {
	archive := `{"_type":"memory","key":"k","value":"v"}` + "\n" +
		`{"id":"demo-1","title":"w","issue_type":"task","labels":["triage"]}` + "\n" +
		`{"id":"demo-2","title":"m","issue_type":"message"}` + "\n" +
		`{"id":"demo-3","title":"wisp","issue_type":"task","metadata":{"gc.kind":"wisp"}}` + "\n"
	scope := t.TempDir()
	if err := os.WriteFile(filepath.Join(scope, "a.jsonl"), []byte(archive), 0o644); err != nil {
		t.Fatal(err)
	}
	read := func(string) (beads.Bead, bdReadResult) { return beads.Bead{}, bdReadNotFound }

	for _, argv := range [][]string{{"import", "a.jsonl", "--skip-infra"}, {"import", "--skip-infra", "-i", "a.jsonl"}, {"import", "--input=a.jsonl", "--skip-infra"}} {
		g := bdClassFlipGuard(splitGuardTestCity(), scope, argv, bdClassGuardDeps{read: read})
		if g.refusal != "" {
			t.Fatalf("%v refused: %s", argv, g.refusal)
		}
		if slicesContainsString(g.args, "--skip-infra") || slicesContainsString(g.args, "a.jsonl") || slicesContainsString(g.args, "--input=a.jsonl") {
			t.Fatalf("%v forwarded %v; want the work-only copy and no gc flag", argv, g.args)
		}
		var copyPath string
		for _, a := range g.args {
			if i := strings.Index(a, "gc-bd-import-work-"); i >= 0 {
				copyPath = strings.TrimPrefix(a, "--input=")
			}
		}
		data, err := os.ReadFile(copyPath)
		if err != nil {
			t.Fatalf("%v: reading the work-only copy %q: %v", argv, copyPath, err)
		}
		got := string(data)
		if !strings.Contains(got, `"_type":"memory"`) || !strings.Contains(got, `"demo-1"`) || strings.Contains(got, `"demo-2"`) || strings.Contains(got, `"demo-3"`) {
			t.Fatalf("%v: work-only copy = %q", argv, got)
		}
		g.cleanup()
		if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
			t.Errorf("%v: the work-only copy outlived the command", argv)
		}
	}

	g := bdClassFlipGuard(splitGuardTestCity(), scope, []string{"import", "-", "--skip-infra"}, bdClassGuardDeps{stdin: bytes.NewBufferString(archive), read: read})
	if g.refusal != "" || g.stdin == nil {
		t.Fatalf("stdin import: refusal=%q stdin=%v", g.refusal, g.stdin)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(g.stdin); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, `"demo-1"`) || strings.Contains(got, `"demo-2"`) || strings.Contains(got, `"demo-3"`) {
		t.Fatalf("stdin import handed bd %q", got)
	}
	if strings.Join(g.args, " ") != "import -" {
		t.Fatalf("stdin import forwarded %v, want [import -]", g.args)
	}
}

func slicesContainsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// testFormulaJSON is what a stubbed `bd cook <name>` prints: a resolved
// formula. "infra-formula" carries a step bd would mint as a graph-class bead
// (gc.root_bead_id) and a child it would mint as a sessions-class bead;
// "work-formula" is the shape every shipped formula has. Anything else is not
// a formula, and the stub prints nothing.
func testFormulaJSON(argv []string) string {
	name := ""
	for i, a := range argv {
		if a == "cook" && i+1 < len(argv) {
			name = argv[i+1]
		}
	}
	switch name {
	case "infra-formula":
		return `{"formula":"infra-formula","version":1,"steps":[{"id":"plain","title":"p"},{"id":"graph","title":"g","metadata":{"gc.root_bead_id":"gcg-1"},"children":[{"id":"sess","title":"s","labels":["gc:session"]}]}]}`
	case "work-formula":
		return `{"formula":"work-formula","version":1,"steps":[{"id":"a","title":"a","type":"task","labels":["triage"],"metadata":{"gc.outcome":"pass"}},{"id":"b","title":"b","gate":{"type":"gh:pr","id":"g1"}}]}`
	}
	return ""
}
