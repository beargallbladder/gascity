package dispatch

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// sqliteGraphStore opens a real SQLite binding minting gcg-.
func sqliteGraphStore(t *testing.T) beads.Store {
	t.Helper()
	store, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("opening the SQLite graph binding: %v", err)
	}
	t.Cleanup(func() { _ = store.(interface{ CloseStore() error }).CloseStore() })
	return store
}

func newGateControl(t *testing.T, graph beads.Store) beads.Bead {
	t.Helper()
	control, err := graph.Create(beads.Bead{Title: "drain", Type: "task", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gcg-root"}})
	if err != nil {
		t.Fatal(err)
	}
	return control
}

// TestEnsureCrossStoreGateIsIdempotentUnderARace: two minters racing for the
// same (control, blocker) end up with one gate, on a real SQLite binding whose
// pinned-id duplicate check is what makes the create conditional.
func TestEnsureCrossStoreGateIsIdempotentUnderARace(t *testing.T) {
	graph := sqliteGraphStore(t)
	control := newGateControl(t, graph)
	const racers = 8
	ids := make([]string, racers)
	errs := make([]error, racers)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range racers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			ids[i], errs[i] = ensureCrossStoreGate(graph, control, "gc-7", true)
		}()
	}
	start.Done()
	done.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
		if ids[i] == "" || ids[i] != ids[0] {
			t.Fatalf("racers got gates %v, want one shared gate", ids)
		}
	}
	if gates := crossStoreGatesIn(t, graph); len(gates) != 1 {
		t.Fatalf("gates = %+v, want exactly one", gates)
	}
}

// gateStores are the two gate-identity paths: a store that pins the derived id
// (SQLite) and one that mints its own and is found by metadata (MemStore).
func gateStores(t *testing.T) map[string]beads.Store {
	return map[string]beads.Store{
		"sqlite (pinned id)":    sqliteGraphStore(t),
		"memstore (minted ids)": beads.NewMemStoreFrom(1000, nil, nil),
	}
}

// TestEnsureCrossStoreGateReopensAGateTheControllerReleased: the drain asks for
// a gate only when it has just found the blocker open, so a gate the release
// pass closed (its blocker had closed) is REOPENED — the blocker opened again —
// and the item is held again, as a single-store city holds it behind the
// reopened blocker. No second gate is minted.
func TestEnsureCrossStoreGateReopensAGateTheControllerReleased(t *testing.T) {
	for name, graph := range gateStores(t) {
		t.Run(name, func(t *testing.T) {
			work := beads.NewMemStore()
			work.HonorExplicitIDs = true
			if _, err := work.Create(beads.Bead{ID: "gc-7", Title: "blocker", Type: "task"}); err != nil {
				t.Fatal(err)
			}
			control := newGateControl(t, graph)
			gateID, err := ensureCrossStoreGate(graph, control, "gc-7", true)
			if err != nil || gateID == "" {
				t.Fatalf("ensureCrossStoreGate = %q, %v", gateID, err)
			}
			if err := work.Close("gc-7"); err != nil {
				t.Fatal(err)
			}
			if report, err := ReleaseCrossStoreGates(graph, storesResolver(work)); err != nil || len(report.Released) != 1 {
				t.Fatalf("release = %+v, %v", report, err)
			}
			again, err := ensureCrossStoreGate(graph, control, "gc-7", true)
			if err != nil || again != gateID {
				t.Fatalf("after release ensureCrossStoreGate = %q, %v; want the released gate %s reopened", again, err, gateID)
			}
			got := mustGetBead(t, graph, gateID)
			if got.Status == "closed" || got.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != "" {
				t.Fatalf("gate = %+v; want it open with its released marker cleared", got)
			}
			if gates := crossStoreGatesIn(t, graph); len(gates) != 1 {
				t.Fatalf("gates = %+v, want the one gate", gates)
			}
		})
	}
}

// TestEnsureCrossStoreGateKeepsAnOperatorOverride: a gate an operator closed
// (no released marker) is final. The drain records the override explicitly
// the first time it sees it, and never reopens or replaces the gate.
func TestEnsureCrossStoreGateKeepsAnOperatorOverride(t *testing.T) {
	for name, graph := range gateStores(t) {
		t.Run(name, func(t *testing.T) {
			control := newGateControl(t, graph)
			gateID, err := ensureCrossStoreGate(graph, control, "gc-404", true)
			if err != nil || gateID == "" {
				t.Fatalf("ensureCrossStoreGate = %q, %v", gateID, err)
			}
			if err := graph.Close(gateID); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				again, err := ensureCrossStoreGate(graph, control, "gc-404", true)
				if err != nil || again != "" {
					t.Fatalf("pass %d after the operator closed the gate: %q, %v; want no gate", i, again, err)
				}
			}
			got := mustGetBead(t, graph, gateID)
			if got.Status != "closed" || got.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != CrossStoreGateStateOverridden {
				t.Fatalf("gate = %+v; want it closed and recorded as overridden", got)
			}
			if gates := crossStoreGatesIn(t, graph); len(gates) != 1 {
				t.Fatalf("gates = %+v, want only the overridden one", gates)
			}
			// The release pass does not reopen an override either.
			if report, err := ReleaseCrossStoreGates(graph, storesResolver()); err != nil || len(report.Reopened) != 0 {
				t.Fatalf("release = %+v, %v; want no reopen", report, err)
			}
		})
	}
}

// TestReleaseCrossStoreGatesReopensAReleasedGateWhoseBlockerReopened: the
// release pass also follows a blocker that reopens after its gate was
// released, while the drain is live; once the drain's control is closed it
// leaves the gate alone.
func TestReleaseCrossStoreGatesReopensAReleasedGateWhoseBlockerReopened(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	work := beads.NewMemStore()
	work.HonorExplicitIDs = true
	if _, err := work.Create(beads.Bead{ID: "gc-7", Title: "blocker", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	control := newGateControl(t, graph)
	gateID, err := ensureCrossStoreGate(graph, control, "gc-7", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := work.Close("gc-7"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReleaseCrossStoreGates(graph, storesResolver(work)); err != nil {
		t.Fatal(err)
	}
	if err := work.Reopen("gc-7"); err != nil {
		t.Fatal(err)
	}
	report, err := ReleaseCrossStoreGates(graph, storesResolver(work))
	if err != nil || !slices.Equal(report.Reopened, []string{gateID}) {
		t.Fatalf("release after the blocker reopened = %+v, %v; want gate %s reopened", report, err, gateID)
	}
	if got := mustGetBead(t, graph, gateID); got.Status == "closed" {
		t.Fatalf("gate %s is still closed", gateID)
	}

	if err := work.Close("gc-7"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReleaseCrossStoreGates(graph, storesResolver(work)); err != nil {
		t.Fatal(err)
	}
	if err := graph.Close(control.ID); err != nil {
		t.Fatal(err)
	}
	if err := work.Reopen("gc-7"); err != nil {
		t.Fatal(err)
	}
	if report, err := ReleaseCrossStoreGates(graph, storesResolver(work)); err != nil || len(report.Reopened) != 0 {
		t.Fatalf("release with the drain closed = %+v, %v; want no reopen", report, err)
	}
}

// TestEnsureCrossStoreGateGivesEachBlockerItsOwnGate: one drain waiting on
// two blockers holds its items behind two gates, on the pinned-id path as on
// the minted one.
func TestEnsureCrossStoreGateGivesEachBlockerItsOwnGate(t *testing.T) {
	for name, graph := range gateStores(t) {
		t.Run(name, func(t *testing.T) {
			control := newGateControl(t, graph)
			a, err := ensureCrossStoreGate(graph, control, "gc-7", true)
			if err != nil {
				t.Fatal(err)
			}
			b, err := ensureCrossStoreGate(graph, control, "gc-8", true)
			if err != nil {
				t.Fatal(err)
			}
			if a == "" || b == "" || a == b {
				t.Fatalf("gates for gc-7 and gc-8 = %q and %q, want two distinct gates", a, b)
			}
		})
	}
}

// TestReleaseCrossStoreGatesRequiresEveryCopyReleased pins the judge's half of
// "closed in every store": a blocker held closed in one store and open in
// another keeps its gate, and a blocker closed with a failed work outcome keeps
// its gate.
func TestReleaseCrossStoreGatesRequiresEveryCopyReleased(t *testing.T) {
	for _, tc := range []struct {
		name   string
		copies func(t *testing.T) []beads.Store
	}{
		{"a second copy is still open", func(t *testing.T) []beads.Store {
			closed := beads.NewMemStore()
			closed.HonorExplicitIDs = true
			open := beads.NewMemStore()
			open.HonorExplicitIDs = true
			if _, err := closed.Create(beads.Bead{ID: "gc-7", Title: "copy", Type: "task"}); err != nil {
				t.Fatal(err)
			}
			if err := closed.Close("gc-7"); err != nil {
				t.Fatal(err)
			}
			if _, err := open.Create(beads.Bead{ID: "gc-7", Title: "copy", Type: "task"}); err != nil {
				t.Fatal(err)
			}
			return []beads.Store{closed, open}
		}},
		{"closed with a failed outcome", func(t *testing.T) []beads.Store {
			work := beads.NewMemStore()
			work.HonorExplicitIDs = true
			if _, err := work.Create(beads.Bead{ID: "gc-7", Title: "failed", Type: "task", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked}}); err != nil {
				t.Fatal(err)
			}
			if err := work.Close("gc-7"); err != nil {
				t.Fatal(err)
			}
			return []beads.Store{work}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := beads.NewMemStoreFrom(1000, nil, nil)
			control := newGateControl(t, graph)
			gateID, err := ensureCrossStoreGate(graph, control, "gc-7", true)
			if err != nil || gateID == "" {
				t.Fatalf("ensureCrossStoreGate = %q, %v", gateID, err)
			}
			report, err := ReleaseCrossStoreGates(graph, storesResolver(tc.copies(t)...))
			if err != nil || len(report.Released) != 0 {
				t.Fatalf("release = %+v, %v; want the gate kept: its blocker has not released everywhere", report, err)
			}
		})
	}
}

// TestReleaseCrossStoreGatesReadsEachBlockerOnceAndStripsExternal: the blockers
// of every open gate are resolved in ONE resolver call, each distinct blocker
// once, with an external: marker stripped.
func TestReleaseCrossStoreGatesReadsEachBlockerOnceAndStripsExternal(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	for i := 0; i < 3; i++ {
		control := newGateControl(t, graph)
		if _, err := ensureCrossStoreGate(graph, control, "gc-7", true); err != nil {
			t.Fatal(err)
		}
		if _, err := ensureCrossStoreGate(graph, control, "external:gc-8", true); err != nil {
			t.Fatal(err)
		}
	}
	var calls [][]string
	report, err := ReleaseCrossStoreGates(graph, func(ids []string) (map[string][]beads.Bead, map[string]bool, error) {
		calls = append(calls, slices.Clone(ids))
		return nil, nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !slices.Equal(calls[0], []string{"gc-7", "gc-8"}) {
		t.Fatalf("resolver calls = %v, want one call naming gc-7 and gc-8 once each", calls)
	}
	if len(report.Unresolved) != 6 || len(report.NewlyUnresolved) != 6 {
		t.Fatalf("report = %+v, want 6 gates newly unresolved", report)
	}
	again, err := ReleaseCrossStoreGates(graph, storesResolver())
	if err != nil || len(again.NewlyUnresolved) != 0 || len(again.Unresolved) != 6 {
		t.Fatalf("second pass = %+v, %v; want the 6 still unresolved and none newly", again, err)
	}
}

// TestReleaseCrossStoreGatesKeepsAGateAnUnreadableStoreMayDecide: a blocker a
// store the resolver could not read may hold is undecided — its gate is neither
// released nor marked unresolved.
func TestReleaseCrossStoreGatesKeepsAGateAnUnreadableStoreMayDecide(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	gateID, err := ensureCrossStoreGate(graph, newGateControl(t, graph), "mr-1", true)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ReleaseCrossStoreGates(graph, func([]string) (map[string][]beads.Bead, map[string]bool, error) {
		return nil, map[string]bool{"mr-1": true}, nil
	})
	if err != nil || len(report.Released)+len(report.Unresolved) != 0 {
		t.Fatalf("release = %+v, %v; want the gate left as it is", report, err)
	}
	if got := mustGetBead(t, graph, gateID); got.Status == "closed" || got.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != "" {
		t.Fatalf("gate = %+v; want it open and unmarked", got)
	}
}

// TestReleaseCrossStoreGatesReleasesAGateWhoseControlIsGone: a deleted drain
// control releases its gates.
func TestReleaseCrossStoreGatesReleasesAGateWhoseControlIsGone(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	control := newGateControl(t, graph)
	gateID, err := ensureCrossStoreGate(graph, control, "gc-7", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Delete(control.ID); err != nil {
		t.Fatal(err)
	}
	report, err := ReleaseCrossStoreGates(graph, storesResolver())
	if err != nil || !slices.Equal(report.Released, []string{gateID}) {
		t.Fatalf("release = %+v, %v; want gate %s released", report, err, gateID)
	}
}

// TestOpenCrossStoreGatesNamingFindsTheGatesADeleteWouldStrand is the helper
// the delete guard calls (S2): it names the open gates waiting on a bead.
func TestOpenCrossStoreGatesNamingFindsTheGatesADeleteWouldStrand(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	gateID, err := ensureCrossStoreGate(graph, newGateControl(t, graph), "gc-7", true)
	if err != nil {
		t.Fatal(err)
	}
	gates, err := OpenCrossStoreGatesNaming(graph, "gc-7")
	if err != nil || len(gates) != 1 || gates[0].ID != gateID {
		t.Fatalf("OpenCrossStoreGatesNaming = %+v, %v; want gate %s", gates, err, gateID)
	}
	if err := graph.Close(gateID); err != nil {
		t.Fatal(err)
	}
	if gates, err := OpenCrossStoreGatesNaming(graph, "gc-7"); err != nil || len(gates) != 0 {
		t.Fatalf("after release = %+v, %v; want none", gates, err)
	}
}

// releasedGate mints a gate on gc-7 under a fresh control and releases it
// through the pass (gc-7 closed in work), returning the control and gate id.
func releasedGate(t *testing.T, graph beads.Store, work *beads.MemStore) (beads.Bead, string) {
	t.Helper()
	if _, err := work.Get("gc-7"); err != nil {
		if _, err := work.Create(beads.Bead{ID: "gc-7", Title: "blocker", Type: "task"}); err != nil {
			t.Fatal(err)
		}
	}
	control := newGateControl(t, graph)
	gateID, err := ensureCrossStoreGate(graph, control, "gc-7", true)
	if err != nil || gateID == "" {
		t.Fatalf("ensureCrossStoreGate = %q, %v", gateID, err)
	}
	if b, _ := work.Get("gc-7"); b.Status != "closed" {
		if err := work.Close("gc-7"); err != nil {
			t.Fatal(err)
		}
	}
	if report, err := ReleaseCrossStoreGates(graph, storesResolver(work)); err != nil || !slices.Contains(report.Released, gateID) {
		t.Fatalf("release = %+v, %v; want %s released", report, err, gateID)
	}
	return control, gateID
}

func honoringMem() *beads.MemStore {
	m := beads.NewMemStore()
	m.HonorExplicitIDs = true
	return m
}

// TestEnsureCrossStoreGateLeavesAReleasedGateWhenTheBlockerIsNotFound: the
// drain reopens a controller-closed gate only when it FOUND the blocker open.
// A blocker its narrower residency frame cannot find (another rig, which the
// release pass reads) leaves the gate released — no release/reopen flap.
func TestEnsureCrossStoreGateLeavesAReleasedGateWhenTheBlockerIsNotFound(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	work := honoringMem()
	control, gateID := releasedGate(t, graph, work)
	got, err := ensureCrossStoreGate(graph, control, "gc-7", false)
	if err != nil || got != "" {
		t.Fatalf("ensureCrossStoreGate(not found) = %q, %v; want no gate (left released)", got, err)
	}
	if g := mustGetBead(t, graph, gateID); g.Status != "closed" || g.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != CrossStoreGateStateReleased {
		t.Fatalf("gate = %+v; want it still closed and released", g)
	}
}

// TestReleaseCrossStoreGatesKeepsAReleasedGateWhileItsBlockerStaysClosed: a
// pass over a released gate whose blocker is still closed reopens nothing.
func TestReleaseCrossStoreGatesKeepsAReleasedGateWhileItsBlockerStaysClosed(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	work := honoringMem()
	_, gateID := releasedGate(t, graph, work)
	for i := 0; i < 2; i++ {
		report, err := ReleaseCrossStoreGates(graph, storesResolver(work))
		if err != nil || len(report.Reopened) != 0 {
			t.Fatalf("pass %d = %+v, %v; want no reopen while gc-7 is closed", i, report, err)
		}
	}
	if g := mustGetBead(t, graph, gateID); g.Status != "closed" {
		t.Fatalf("gate %s reopened", gateID)
	}
}

// TestReleaseCrossStoreGatesDoesNotReopenOnAPurgedOrUnreadableBlocker: a
// released gate reopens only when some store holds an unreleased copy. A
// blocker that is gone (purged after closing) is not one, and neither is a
// blocker that an unreadable store may hold.
func TestReleaseCrossStoreGatesDoesNotReopenOnAPurgedOrUnreadableBlocker(t *testing.T) {
	t.Run("purged", func(t *testing.T) {
		graph := beads.NewMemStoreFrom(1000, nil, nil)
		work := honoringMem()
		_, gateID := releasedGate(t, graph, work)
		if err := work.Delete("gc-7"); err != nil {
			t.Fatal(err)
		}
		report, err := ReleaseCrossStoreGates(graph, storesResolver(work))
		if err != nil || len(report.Reopened) != 0 || mustGetBead(t, graph, gateID).Status != "closed" {
			t.Fatalf("after purge = %+v, %v; want the gate left released", report, err)
		}
	})
	t.Run("unreadable store", func(t *testing.T) {
		graph := beads.NewMemStoreFrom(1000, nil, nil)
		work := honoringMem()
		_, gateID := releasedGate(t, graph, work)
		if err := work.Reopen("gc-7"); err != nil {
			t.Fatal(err)
		}
		open, _ := work.Get("gc-7")
		report, err := ReleaseCrossStoreGates(graph, func([]string) (map[string][]beads.Bead, map[string]bool, error) {
			// The open copy is visible, but another store that may hold a
			// copy could not be read: undecided.
			return map[string][]beads.Bead{"gc-7": {open}}, map[string]bool{"gc-7": true}, nil
		})
		if err != nil || len(report.Reopened) != 0 || mustGetBead(t, graph, gateID).Status != "closed" {
			t.Fatalf("with gc-7 undecided = %+v, %v; want the gate left released", report, err)
		}
	})
}

// failingUpdates fails every Update on one id.
type failingUpdates struct {
	beads.Store
	failID string
}

func (s failingUpdates) Update(id string, opts beads.UpdateOpts) error {
	if id == s.failID {
		return errors.New("injected: write failed")
	}
	return s.Store.Update(id, opts)
}

// TestReopenIsOneWriteSoAFailureLeavesNoHalfState: a reopen that fails leaves
// the gate exactly as it was (closed, released) — never open with its released
// marker, whose later operator close would read as a controller release.
func TestReopenIsOneWriteSoAFailureLeavesNoHalfState(t *testing.T) {
	inner := beads.NewMemStoreFrom(1000, nil, nil)
	work := honoringMem()
	_, gateID := releasedGate(t, inner, work)
	if err := work.Reopen("gc-7"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReleaseCrossStoreGates(failingUpdates{Store: inner, failID: gateID}, storesResolver(work)); err == nil {
		t.Fatal("the reopen's failure was not reported")
	}
	g := mustGetBead(t, inner, gateID)
	if g.Status != "closed" || g.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != CrossStoreGateStateReleased {
		t.Fatalf("gate after a failed reopen = %s/%q; want closed/released, untouched", g.Status, g.Metadata[beadmeta.CrossStoreGateStateMetadataKey])
	}
	// The next pass reopens it, and the marker is REMOVED, not left empty.
	if _, err := ReleaseCrossStoreGates(inner, storesResolver(work)); err != nil {
		t.Fatal(err)
	}
	g = mustGetBead(t, inner, gateID)
	if _, present := g.Metadata[beadmeta.CrossStoreGateStateMetadataKey]; g.Status == "closed" || present {
		t.Fatalf("gate after the reopen = %s with metadata %v; want open with the state key removed", g.Status, g.Metadata)
	}
}

// listCounting counts a gate store's List calls and id-batched reads.
type listCounting struct {
	beads.Store
	lists, idReads int
}

func (s *listCounting) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.lists++
	if len(q.IDs) > 0 {
		s.idReads++
	}
	return s.Store.List(q)
}

// TestReleaseCrossStoreGatesRetiresFinishedDrainsAndThenReadsOnlyItsListings:
// a released gate whose drain is closed is retired once; after that a pass
// with nothing gated reads the two gate listings and nothing else — no
// control, no resolver call.
func TestReleaseCrossStoreGatesRetiresFinishedDrainsAndThenReadsOnlyItsListings(t *testing.T) {
	inner := beads.NewMemStoreFrom(1000, nil, nil)
	work := honoringMem()
	control, gateID := releasedGate(t, inner, work)
	if err := inner.Close(control.ID); err != nil {
		t.Fatal(err)
	}
	report, err := ReleaseCrossStoreGates(inner, storesResolver(work))
	if err != nil || !slices.Equal(report.Retired, []string{gateID}) {
		t.Fatalf("pass = %+v, %v; want gate %s retired", report, err, gateID)
	}
	counting := &listCounting{Store: inner}
	resolved := 0
	report, err = ReleaseCrossStoreGates(counting, func([]string) (map[string][]beads.Bead, map[string]bool, error) {
		resolved++
		return nil, nil, nil
	})
	if err != nil || report.Open != 0 || len(report.Retired) != 0 {
		t.Fatalf("idle pass = %+v, %v", report, err)
	}
	if counting.lists != 2 || counting.idReads != 0 || resolved != 0 {
		t.Fatalf("idle pass read %d list(s), %d id batch(es), %d resolver call(s); want the two gate listings only", counting.lists, counting.idReads, resolved)
	}
}

// racingStore hides the pinned gate from the up-front reads, as if a racing
// minter created it between them and this caller's Create.
type racingStore struct {
	beads.Store
	pinned string
	hidden bool
}

func (s *racingStore) Get(id string) (beads.Bead, error) {
	if id == s.pinned && s.hidden {
		s.hidden = false
		return beads.Bead{}, beads.ErrNotFound
	}
	return s.Store.Get(id)
}

func (s *racingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Label == CrossStoreGateLabel && len(q.Metadata) > 0 {
		return nil, nil
	}
	return s.Store.List(q)
}

// IDPrefix forwards the binding's mint prefix, so the gate id is pinned.
func (s *racingStore) IDPrefix() string { return "gcg" }

// TestEnsureCrossStoreGateSettlesARacingMintersGateLikeAnyOther: a gate a
// racing minter won is settled exactly like one found up front — a released
// one is not handed back as holding when the blocker was not found open.
func TestEnsureCrossStoreGateSettlesARacingMintersGateLikeAnyOther(t *testing.T) {
	graph := sqliteGraphStore(t)
	work := honoringMem()
	control, gateID := releasedGate(t, graph, work)
	race := &racingStore{Store: graph, pinned: gateID, hidden: true}
	got, err := ensureCrossStoreGate(race, control, "gc-7", false)
	if err != nil || got != "" {
		t.Fatalf("racing ensureCrossStoreGate(not found open) = %q, %v; want no gate: the raced gate is released", got, err)
	}
}

// TestEnsureCrossStoreGateTreatsATeardownCloseAsTheControllers: a workflow
// teardown (CloseSubtree) closes the gate with a stamped close_reason and no
// state marker. That is the controller's close, not an operator override: a
// retried drain that finds the blocker open reopens it.
func TestEnsureCrossStoreGateTreatsATeardownCloseAsTheControllers(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	control := newGateControl(t, graph)
	gateID, err := ensureCrossStoreGate(graph, control, "gc-7", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graph.CloseAll([]string{gateID}, map[string]string{"close_reason": "workflow subtree closed"}); err != nil {
		t.Fatal(err)
	}
	if got, err := ensureCrossStoreGate(graph, control, "gc-7", false); err != nil || got != "" {
		t.Fatalf("not found open: %q, %v; want the torn-down gate left closed", got, err)
	}
	if state := mustGetBead(t, graph, gateID).Metadata[beadmeta.CrossStoreGateStateMetadataKey]; state == CrossStoreGateStateOverridden {
		t.Fatal("a teardown close was recorded as an operator override")
	}
	if got, err := ensureCrossStoreGate(graph, control, "gc-7", true); err != nil || got != gateID {
		t.Fatalf("found open: %q, %v; want gate %s reopened", got, err, gateID)
	}
}

// TestReleaseCrossStoreGatesStampsWhenAGateWasFirstFoundUnresolved: the
// first_seen anchor control.stalled reports is the persisted time the gate
// was first found unresolved, and a later pass does not move it.
func TestReleaseCrossStoreGatesStampsWhenAGateWasFirstFoundUnresolved(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	gateID, err := ensureCrossStoreGate(graph, newGateControl(t, graph), "gc-404", true)
	if err != nil {
		t.Fatal(err)
	}
	prev := crossStoreGateNow
	t.Cleanup(func() { crossStoreGateNow = prev })
	first := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	crossStoreGateNow = func() time.Time { return first }
	if _, err := ReleaseCrossStoreGates(graph, storesResolver()); err != nil {
		t.Fatal(err)
	}
	crossStoreGateNow = func() time.Time { return first.Add(time.Hour) }
	if _, err := ReleaseCrossStoreGates(graph, storesResolver()); err != nil {
		t.Fatal(err)
	}
	if got := mustGetBead(t, graph, gateID).Metadata[beadmeta.CrossStoreGateUnresolvedAtMetadataKey]; got != first.Format(time.RFC3339) {
		t.Fatalf("unresolved_at = %q, want the first pass's %q", got, first.Format(time.RFC3339))
	}
}
