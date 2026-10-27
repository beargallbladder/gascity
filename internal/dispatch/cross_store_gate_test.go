package dispatch

import (
	"slices"
	"sync"
	"testing"

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
			ids[i], errs[i] = ensureCrossStoreGate(graph, control, "gc-7")
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
			gateID, err := ensureCrossStoreGate(graph, control, "gc-7")
			if err != nil || gateID == "" {
				t.Fatalf("ensureCrossStoreGate = %q, %v", gateID, err)
			}
			if err := work.Close("gc-7"); err != nil {
				t.Fatal(err)
			}
			if report, err := ReleaseCrossStoreGates(graph, storesResolver(work)); err != nil || len(report.Released) != 1 {
				t.Fatalf("release = %+v, %v", report, err)
			}
			again, err := ensureCrossStoreGate(graph, control, "gc-7")
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
			gateID, err := ensureCrossStoreGate(graph, control, "gc-404")
			if err != nil || gateID == "" {
				t.Fatalf("ensureCrossStoreGate = %q, %v", gateID, err)
			}
			if err := graph.Close(gateID); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				again, err := ensureCrossStoreGate(graph, control, "gc-404")
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
	gateID, err := ensureCrossStoreGate(graph, control, "gc-7")
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
			a, err := ensureCrossStoreGate(graph, control, "gc-7")
			if err != nil {
				t.Fatal(err)
			}
			b, err := ensureCrossStoreGate(graph, control, "gc-8")
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
			gateID, err := ensureCrossStoreGate(graph, control, "gc-7")
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
		if _, err := ensureCrossStoreGate(graph, control, "gc-7"); err != nil {
			t.Fatal(err)
		}
		if _, err := ensureCrossStoreGate(graph, control, "external:gc-8"); err != nil {
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
	gateID, err := ensureCrossStoreGate(graph, newGateControl(t, graph), "mr-1")
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
	gateID, err := ensureCrossStoreGate(graph, control, "gc-7")
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
	gateID, err := ensureCrossStoreGate(graph, newGateControl(t, graph), "gc-7")
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
