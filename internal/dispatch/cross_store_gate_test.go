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

// TestEnsureCrossStoreGateNeverReplacesAReleasedGate: once a gate is released
// (its blocker closed), asking again returns no gate and mints none — on the
// pinned-id path and on the metadata path a store without pinned ids takes.
func TestEnsureCrossStoreGateNeverReplacesAReleasedGate(t *testing.T) {
	for name, graph := range map[string]beads.Store{
		"sqlite (pinned id)":    sqliteGraphStore(t),
		"memstore (minted ids)": beads.NewMemStoreFrom(1000, nil, nil),
	} {
		t.Run(name, func(t *testing.T) {
			control := newGateControl(t, graph)
			gateID, err := ensureCrossStoreGate(graph, control, "gc-7")
			if err != nil || gateID == "" {
				t.Fatalf("ensureCrossStoreGate = %q, %v", gateID, err)
			}
			if err := graph.Close(gateID); err != nil {
				t.Fatal(err)
			}
			again, err := ensureCrossStoreGate(graph, control, "gc-7")
			if err != nil || again != "" {
				t.Fatalf("after release ensureCrossStoreGate = %q, %v; want no gate", again, err)
			}
			if gates := crossStoreGatesIn(t, graph); len(gates) != 1 {
				t.Fatalf("gates = %+v, want only the released one", gates)
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
