package readywork

// Cross-store blockers on real split fixtures: a real SQLite binding (the store
// a split city's graph class is served from), a real bd/Dolt work store (the
// linked beads library over embedded Dolt), and the in-memory leaf the migration
// clear leaves kept edges in. The two real backends answer a blocker they do not
// hold in OPPOSITE ways, and each test pins the owner rule over both: a blocker
// held in the other store blocks until it is closed there; one no store holds
// blocks its row and never fails the read.

import (
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
)

// sqliteBinding opens the class binding a split city serves its graph class
// from: a real SQLite store minting gcg-.
func sqliteBinding(t *testing.T) beads.Store {
	t.Helper()
	store, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("opening the SQLite binding: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := store.(interface{ CloseStore() error }); ok {
			_ = closer.CloseStore()
		}
	})
	return store
}

func splitLegs(work, binding beads.Store) []Leg {
	return []Leg{
		testLeg("work", work),
		{Label: "graph", Store: binding, ClassBinding: true, ReservedPrefixes: []string{"gcg"}},
	}
}

func closeBead(t *testing.T, store beads.Store, id string) {
	t.Helper()
	if err := store.Close(id); err != nil {
		t.Fatalf("closing %s: %v", id, err)
	}
}

func readyIDs(t *testing.T, legs []Leg, q Query) []string {
	t.Helper()
	rows, err := Read(legs, q)
	if err != nil {
		t.Fatalf("Read(%+v): %v", q, err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

func setInProgress(t *testing.T, store beads.Store, id, assignee string) {
	t.Helper()
	status := "in_progress"
	if err := store.Update(id, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("marking %s in progress: %v", id, err)
	}
}

// TestReadInProgressUnresolvableBlockerBlocksItsRowNotTheTier is R8: one work
// row whose blocking edge names an id no store holds (an edge the migration
// clear kept into an id since purged from the binding) used to fail the WHOLE
// crash-recovery tier, so a seat could not resume any of its work. Now that row
// is served as gated, with a reason, and every clean row is served beside it.
func TestReadInProgressUnresolvableBlockerBlocksItsRowNotTheTier(t *testing.T) {
	work := beads.NewMemStore() // a raw leaf: the edge was kept by the clear, not added through a strict door
	mine := mustCreate(t, work, beads.Bead{Title: "mine", Type: "task"})
	other := mustCreate(t, work, beads.Bead{Title: "other", Type: "task"})
	setInProgress(t, work, mine.ID, "worker-1")
	setInProgress(t, work, other.ID, "worker-1")
	if err := work.DepAdd(other.ID, "gcg-7", "blocks"); err != nil {
		t.Fatal(err)
	}
	legs := splitLegs(work, sqliteBinding(t))

	for _, limit := range []int{0, 1, 2} {
		rows, err := Read(legs, Query{Status: StatusInProgress, Assignee: "worker-1", Limit: limit})
		if err != nil {
			t.Fatalf("limit=%d: the in-progress tier failed outright because %s has a blocker no store holds: %v", limit, other.ID, err)
		}
		want := 2
		if limit == 1 {
			want = 1
		}
		if len(rows) != want {
			t.Fatalf("limit=%d: served %d rows, want %d: %+v", limit, len(rows), want, rows)
		}
		for _, row := range rows {
			switch row.ID {
			case mine.ID:
				if len(row.BlockedBy) != 0 {
					t.Errorf("limit=%d: clean row %s carries blockers %+v", limit, mine.ID, row.BlockedBy)
				}
			case other.ID:
				if len(row.BlockedBy) != 1 {
					t.Fatalf("limit=%d: %s blocked_by = %+v, want the one unresolvable blocker", limit, other.ID, row.BlockedBy)
				}
				got := row.BlockedBy[0]
				if got.ID != "gcg-7" || got.Status != BlockerUnresolved || !strings.Contains(got.Reason, "no store") {
					t.Errorf("limit=%d: blocker = %+v, want gcg-7 %q with a reason naming that no store holds it", limit, got, BlockerUnresolved)
				}
			}
		}
	}
}

// TestReadInProgressCrossStoreBlockerReportsTheStatusItsHomeStoreHolds pins
// the resumed holder's view of a blocker in the other store: the status the
// binding holds, not a failure and not an absence.
func TestReadInProgressCrossStoreBlockerReportsTheStatusItsHomeStoreHolds(t *testing.T) {
	work := beads.NewMemStore()
	binding := sqliteBinding(t)
	step := mustCreate(t, binding, beads.Bead{Title: "graph step", Type: "task"})
	row := mustCreate(t, work, beads.Bead{Title: "work", Type: "task"})
	setInProgress(t, work, row.ID, "worker-1")
	if err := work.DepAdd(row.ID, step.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	legs := splitLegs(work, binding)

	blockerOf := func() ReadyBeadBlocker {
		t.Helper()
		rows, err := Read(legs, Query{Status: StatusInProgress, Assignee: "worker-1"})
		if err != nil || len(rows) != 1 || len(rows[0].BlockedBy) != 1 {
			t.Fatalf("Read = %+v, %v; want %s with its one blocker", rows, err, row.ID)
		}
		return rows[0].BlockedBy[0]
	}
	if got := blockerOf(); got.ID != step.ID || got.Status != "open" || !strings.Contains(got.Reason, "graph") {
		t.Fatalf("open cross-store blocker = %+v, want %s open, with a reason naming the graph store", got, step.ID)
	}
	closeBead(t, binding, step.ID)
	if got := blockerOf(); got.Status != "closed" {
		t.Fatalf("closed cross-store blocker = %+v, want status closed", got)
	}
}

// TestReadServesAStrandedSQLiteRowOnceItsWorkBlockerCloses is the stranded half
// of R9 on the real SQLite binding: a graph row whose blocker lives in the work
// store is withheld by the binding's own Ready FOREVER (the LEFT JOIN reads the
// absent blocker as never closed). gc's reader serves it the moment the blocker
// closes in the work store, and not before.
func TestReadServesAStrandedSQLiteRowOnceItsWorkBlockerCloses(t *testing.T) {
	work := splittest.NewWorkStore(t, "gc")
	binding := sqliteBinding(t)
	blocker := mustCreate(t, work, beads.Bead{Title: "work blocker", Type: "task"})
	item := mustCreate(t, binding, beads.Bead{Title: "graph item", Type: "task"})
	if err := binding.DepAdd(item.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("writing the cross-store edge into the binding: %v", err)
	}
	legs := splitLegs(work, binding)

	if got := readyIDs(t, legs, Query{}); slices.Contains(got, item.ID) {
		t.Fatalf("ready = %v served %s while its work blocker %s is open", got, item.ID, blocker.ID)
	}
	closeBead(t, work, blocker.ID)
	own, err := binding.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(own, func(b beads.Bead) bool { return b.ID == item.ID }) {
		t.Fatalf("fixture: the SQLite binding's own Ready served %s; it is meant to strand it behind the absent blocker", item.ID)
	}
	for _, q := range []Query{{}, {Limit: 1}, {Limit: 5, Sort: SortOldest}} {
		if got := readyIDs(t, legs, q); !slices.Contains(got, item.ID) {
			t.Errorf("ready(%+v) = %v; %s stays stranded although its blocker %s closed in the work store", q, got, item.ID, blocker.ID)
		}
	}
}

// TestReadWithholdsAPrematureBdRowUntilItsGraphBlockerCloses is the premature
// half of R9 on a real bd/Dolt work store: bd classifies a blocker it does not
// hold as external and serves the dependent at once. gc's reader withholds it
// until the blocker closes in the binding, and withholds one whose blocker no
// store holds at all.
func TestReadWithholdsAPrematureBdRowUntilItsGraphBlockerCloses(t *testing.T) {
	work := splittest.NewNativeWorkStore(t, "gc")
	binding := sqliteBinding(t)
	// The graph step is ready work of its own; ranking it last keeps it out
	// of the bounded answers below, which are about the work rows.
	first, last := 0, 4
	step := mustCreate(t, binding, beads.Bead{Title: "graph step", Type: "task", Priority: &last})
	gated := mustCreate(t, work, beads.Bead{Title: "gated", Type: "task", Priority: &first})
	dangling := mustCreate(t, work, beads.Bead{Title: "dangling", Type: "task", Priority: &first})
	clean := mustCreate(t, work, beads.Bead{Title: "clean", Type: "task", Priority: &first})
	// bd's dependency door accepts a target it does not hold (only the bd CLI
	// store refuses a cross-prefix edge), which is how a kept or create-time
	// edge reaches a real work ledger.
	if err := work.DepAdd(gated.ID, step.ID, "blocks"); err != nil {
		t.Fatalf("writing the cross-store edge into the bd store: %v", err)
	}
	if err := work.DepAdd(dangling.ID, "gcg-404", "blocks"); err != nil {
		t.Fatalf("writing the dangling edge into the bd store: %v", err)
	}

	own, err := work.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	var ownIDs []string
	for _, b := range own {
		ownIDs = append(ownIDs, b.ID)
	}
	if !slices.Contains(ownIDs, gated.ID) {
		t.Skipf("fixture: this bd serves no external-blocker row as ready (own ready = %v); the premature case does not arise", ownIDs)
	}

	legs := splitLegs(work, binding)
	for _, q := range []Query{{}, {Limit: 1}, {Limit: 3, Sort: SortOldest}} {
		got := readyIDs(t, legs, q)
		if slices.Contains(got, gated.ID) {
			t.Errorf("ready(%+v) = %v served %s while its graph blocker %s is open", q, got, gated.ID, step.ID)
		}
		if slices.Contains(got, dangling.ID) {
			t.Errorf("ready(%+v) = %v served %s, whose blocker no store holds", q, got, dangling.ID)
		}
		if !slices.Contains(got, clean.ID) {
			t.Errorf("ready(%+v) = %v dropped the clean row %s", q, got, clean.ID)
		}
	}
	closeBead(t, binding, step.ID)
	if got := readyIDs(t, legs, Query{}); !slices.Contains(got, gated.ID) {
		t.Errorf("ready = %v; %s is still withheld after its graph blocker %s closed", got, gated.ID, step.ID)
	}
}

// TestReadBoundedAnswerRefillsPastAWithheldRow pins the bound: when the gate
// withholds the rows a bounded read cut at, the answer is still the top N of
// what may be served, not a short one.
func TestReadBoundedAnswerRefillsPastAWithheldRow(t *testing.T) {
	work := beads.NewMemStore()
	binding := sqliteBinding(t)
	high, low, last := 0, 3, 4
	step := mustCreate(t, binding, beads.Bead{Title: "graph step", Type: "task", Priority: &last})
	for i := 0; i < boundedSlack+3; i++ {
		b := mustCreate(t, work, beads.Bead{Title: "gated", Type: "task", Priority: &high})
		if err := work.DepAdd(b.ID, step.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	// The MemStore leaf is the stranding backend, so its own Ready withholds
	// these; make them premature instead, as bd would, by serving them through
	// a leg that ignores the edge.
	served := mustCreate(t, work, beads.Bead{Title: "servable", Type: "task", Priority: &low})
	legs := []Leg{testLeg("work", externalBlockerStore{work}), {Label: "graph", Store: binding, ClassBinding: true, ReservedPrefixes: []string{"gcg"}}}
	got := readyIDs(t, legs, Query{Limit: 1})
	if !slices.Equal(got, []string{served.ID}) {
		t.Fatalf("ready(limit=1) = %v, want [%s]: every higher-priority row is gated by the open graph step %s", got, served.ID, step.ID)
	}
}

// externalBlockerStore serves a MemStore's ready set the way bd/Dolt does: a
// blocker the store does not hold never blocks.
type externalBlockerStore struct{ *beads.MemStore }

func (s externalBlockerStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	q := beads.ReadyQuery{}
	if len(query) > 0 {
		q = query[0]
	}
	all, err := s.List(beads.ListQuery{Status: "open", TierMode: q.TierMode})
	if err != nil {
		return nil, err
	}
	var out []beads.Bead
	for _, b := range all {
		blocked := false
		deps, err := s.DepList(b.ID, "down")
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			if !beads.IsReadyBlockingDependencyType(d.Type) {
				continue
			}
			target, err := s.Get(d.DependsOnID)
			if err != nil {
				continue // external: not held here, so not blocking
			}
			if target.Status != "closed" {
				blocked = true
			}
		}
		if !blocked && beads.IsReadyCandidateForTier(b, time.Now().UTC(), q.TierMode) {
			out = append(out, b)
		}
	}
	return out, nil
}

// ReadyOrdered cuts Ready in the canonical order, so the reader's bounded read
// is really bounded and the gate can withhold every row it cut at.
func (s externalBlockerStore) ReadyOrdered(query beads.ReadyQuery, _ beads.ReadyOrder) ([]beads.Bead, bool, error) {
	rows, err := s.Ready(query)
	if err != nil {
		return nil, false, err
	}
	prio := func(b beads.Bead) int {
		if b.Priority == nil {
			return 2
		}
		return *b.Priority
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if prio(rows[i]) != prio(rows[j]) {
			return prio(rows[i]) < prio(rows[j])
		}
		if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].CreatedAt.Before(rows[j].CreatedAt)
		}
		return rows[i].ID < rows[j].ID
	})
	if query.Limit > 0 && len(rows) > query.Limit {
		rows = rows[:query.Limit]
	}
	return rows, true, nil
}

// ReadyBlockedByAbsent is withheld: this store models bd, which strands nothing.
func (s externalBlockerStore) ReadyBlockedByAbsent(beads.ReadyQuery) ([]beads.Bead, error) {
	return nil, nil
}
