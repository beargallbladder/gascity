package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// The wisp GC on a split city (R7 and the work-class molecule roots).
//
// The GC runs over the graph-class store. Two of its reads belong to the work
// side on a split city:
//
//   - the abandoned-root sweep's live-holder guard: the holders of an attached
//     root are WORK beads carrying molecule_id / workflow_id, so listing them
//     in the binding always answered "no live holder" and an attached
//     root-only wisp became closable one TTL after pour while its source was
//     live (R7);
//   - work-class molecule roots: a v1 poured molecule is work class and lives
//     on the work ledger, so a GC that enumerated roots in the binding alone
//     never closed an abandoned one nor collected a closed one.
//
// Both topologies; the split leg's binding is a real SQLite store.

func wispGCLegsFor(e splitEnv) wispGCLegs {
	if !e.split {
		return nil
	}
	return workLegsFromCensus(e.cityPath, e.cfg, cityWorkLegOf(beads.WorkStore{Store: e.work}), nil)
}

func runSplitWispGC(t *testing.T, e splitEnv, now time.Time) {
	t.Helper()
	withCloseAbandonedEnforced(t, func() {
		withCloseAbandonedTTL(t, 5*time.Minute, func() {
			wg := newWispGC(5*time.Minute, time.Hour, 0)
			if _, err := wg.runGC(wispGCLegsFor(e), beads.GraphStore{Store: e.graphStore()}, beads.SessionStore{}, beads.MailStore{}, now); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
	})
}

// R7: a live work source keeps its attached root-only wisp open.
func TestWispGCAbandonedRootHolderGuardSeesAWorkSource(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		graph := e.graphStore()
		root, err := graph.Create(beads.Bead{Title: "attached root-only wisp", Type: "molecule", Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWisp,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.work.Create(beads.Bead{Title: "live source", Type: "task", Metadata: map[string]string{
			beadmeta.MoleculeIDMetadataKey: root.ID,
		}}); err != nil {
			t.Fatal(err)
		}
		runSplitWispGC(t, e, time.Now().Add(30*time.Minute))
		got, err := graph.Get(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "open" {
			t.Fatalf("attached wisp %s status=%q with a live work source, want open", root.ID, got.Status)
		}
	})
}

// The far side: once the work source is terminal the stepless root reaps.
func TestWispGCAbandonedRootClosesWhenItsWorkSourceIsTerminal(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		graph := e.graphStore()
		root, err := graph.Create(beads.Bead{Title: "attached root-only wisp", Type: "molecule", Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWisp,
		}})
		if err != nil {
			t.Fatal(err)
		}
		src, err := e.work.Create(beads.Bead{Title: "done source", Type: "task", Metadata: map[string]string{
			beadmeta.MoleculeIDMetadataKey: root.ID,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.work.Close(src.ID); err != nil {
			t.Fatal(err)
		}
		runSplitWispGC(t, e, time.Now().Add(30*time.Minute))
		got, err := graph.Get(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" {
			t.Fatalf("attached wisp %s status=%q with a terminal source, want closed", root.ID, got.Status)
		}
	})
}

// A v1 poured molecule on the work ledger whose steps are all terminal is an
// abandoned root the sweep closes.
func TestWispGCClosesAnAbandonedWorkClassMoleculeRoot(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		root, steps, owner := pourV1Molecule(t, e, "")
		if owner != e.work {
			t.Fatalf("v1 poured molecule went to %T, want the work store", owner)
		}
		for _, s := range steps {
			if err := e.work.Close(s.ID); err != nil {
				t.Fatal(err)
			}
		}
		runSplitWispGC(t, e, time.Now().Add(30*time.Minute))
		got, err := e.work.Get(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" {
			t.Fatalf("abandoned work-class molecule root %s status=%q, want closed", root.ID, got.Status)
		}
	})
}

// A closed v1 poured molecule on the work ledger past the TTL is collected.
//
// The purge deletes through the store's orphan-preserving batch delete, which
// the production work stores (bd, Dolt) have and the kit's strict in-memory
// leaf does not, so this row reads its work leg from a store that has it.
func TestWispGCPurgesAnExpiredWorkClassMoleculeRoot(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		work := newBatchDeleteWorkStore()
		graph := e.graphStore()
		var legs wispGCLegs
		if e.split {
			legs = workLegsFromCensus(e.cityPath, e.cfg, cityWorkLegOf(beads.WorkStore{Store: work}), nil)
		} else {
			graph = work
		}
		root, err := work.Create(beads.Bead{Title: "v1 poured", Type: "molecule"})
		if err != nil {
			t.Fatal(err)
		}
		step, err := work.Create(beads.Bead{Title: "step", Type: "step", ParentID: root.ID})
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{step.ID, root.ID} {
			if err := work.Close(id); err != nil {
				t.Fatal(err)
			}
		}
		wg := newWispGC(5*time.Minute, time.Hour, 0)
		if _, err := wg.runGC(legs, beads.GraphStore{Store: graph}, beads.SessionStore{}, beads.MailStore{}, time.Now().Add(2*time.Hour)); err != nil {
			t.Fatalf("runGC: %v", err)
		}
		if _, err := work.Get(root.ID); err == nil {
			t.Fatalf("closed work-class molecule root %s survived a GC past its TTL", root.ID)
		}
	})
}

// batchDeleteWorkStore is a work ledger with the orphan-preserving batch delete
// the production work stores have.
type batchDeleteWorkStore struct{ *beads.MemStore }

func newBatchDeleteWorkStore() batchDeleteWorkStore {
	return batchDeleteWorkStore{MemStore: beads.NewMemStore()}
}

func (s batchDeleteWorkStore) DeleteBatch(ids []string) error {
	for _, id := range ids {
		if err := s.Delete(id); err != nil {
			return err
		}
	}
	return nil
}

// The work arm never touches a plain work bead.
func TestWispGCLeavesPlainWorkAlone(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		task, err := e.work.Create(beads.Bead{Title: "plain task", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.work.Close(task.ID); err != nil {
			t.Fatal(err)
		}
		runSplitWispGC(t, e, time.Now().Add(2*time.Hour))
		if _, err := e.work.Get(task.ID); err != nil {
			t.Fatalf("plain closed task %s was collected by the wisp GC: %v", task.ID, err)
		}
	})
}
