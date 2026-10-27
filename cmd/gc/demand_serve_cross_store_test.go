package main

import (
	"context"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/readywork"
)

// TestControllerDemandAgreesWithServeOnCrossStoreBlockers is the demand half of
// the owner decision: pool demand applies the SAME cross-store rule as
// `gc ready`, so the rows the controller counts as demand and the rows a seat
// is served are the same rows. It runs on a split city with a real bd/Dolt work
// store and rig and a real SQLite binding, over the three edge shapes that
// differ by backend: an external:<id> ref, a cross-rig blocker and a dangling
// literal.
func TestControllerDemandAgreesWithServeOnCrossStoreBlockers(t *testing.T) {
	cityPath := t.TempDir()
	work := splittest.NewNativeWorkStore(t, "gc")
	rig := splittest.NewNativeWorkStore(t, "rg")
	binding := newSplitEnvClassLeaf(t)
	routes := splitEnvRoutes(binding)
	registerResidencyRoutes(cityPath, routes, func() beads.Store { return work })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", Prefix: "gc"},
		Rigs:      []config.Rig{{Name: "rg", Path: "rigs/rg", Prefix: "rg"}},
		Storage:   splitEnvStorageConfig(),
	}
	rigStores := map[string]beads.Store{"rg": rig}

	last := 4
	step, err := binding.Create(beads.Bead{Title: "graph step", Type: "task", Priority: &last})
	if err != nil {
		t.Fatal(err)
	}
	rigBlocker, err := rig.Create(beads.Bead{Title: "rig blocker", Type: "task", Priority: &last})
	if err != nil {
		t.Fatal(err)
	}
	newRow := func(title, target string) beads.Bead {
		t.Helper()
		row, err := work.Create(beads.Bead{Title: title, Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		if target != "" {
			if err := work.DepAdd(row.ID, target, "blocks"); err != nil {
				t.Fatalf("DepAdd(%s -> %s): %v", row.ID, target, err)
			}
		}
		return row
	}
	extRow := newRow("external id dep", "external:"+step.ID)
	rigRow := newRow("cross-rig dep", rigBlocker.ID)
	danglingRow := newRow("dangling dep", "discovered-from:gc-404")
	capRow := newRow("capability dep", "external:rg:some-capability")
	clean := newRow("clean", "")

	legs, err := readyLegsForTopology(residencyTopologyForCity(cityPath, cfg, work, rigStores))
	if err != nil {
		t.Fatal(err)
	}
	if !readywork.IsSplit(legs) {
		t.Fatal("fixture: the legs do not read as a split city")
	}
	agree := func(phase string) (demand []string) {
		t.Helper()
		cache := newReadyDemandCache().withCrossStoreGate(cityPath, cfg, work, rigStores, nil)
		for _, leg := range legs {
			rows, err := cache.controllerDemandReady(leg.Store, leg.Label)
			if err != nil {
				t.Fatalf("%s: demand ready on %s: %v", phase, leg.Label, err)
			}
			for _, b := range rows {
				demand = append(demand, b.ID)
			}
		}
		served, err := readywork.ReadContext(context.Background(), legs, readywork.Query{})
		if err != nil {
			t.Fatal(err)
		}
		var serve []string
		for _, r := range served {
			serve = append(serve, r.ID)
		}
		slices.Sort(demand)
		slices.Sort(serve)
		demand = slices.Compact(demand)
		if !slices.Equal(demand, serve) {
			t.Fatalf("%s: demand counts %v, serve answers %v: the controller would spawn for work no seat is served, or starve work a seat would be", phase, demand, serve)
		}
		return demand
	}

	before := agree("blockers open")
	for _, id := range []string{extRow.ID, rigRow.ID, danglingRow.ID} {
		if slices.Contains(before, id) {
			t.Errorf("blockers open: %s is counted and served while its blocker holds it", id)
		}
	}
	for _, id := range []string{capRow.ID, clean.ID} {
		if !slices.Contains(before, id) {
			t.Errorf("blockers open: %s is neither counted nor served; nothing holds it", id)
		}
	}
	if err := binding.Close(step.ID); err != nil {
		t.Fatal(err)
	}
	if err := rig.Close(rigBlocker.ID); err != nil {
		t.Fatal(err)
	}
	after := agree("blockers closed")
	for _, id := range []string{extRow.ID, rigRow.ID} {
		if !slices.Contains(after, id) {
			t.Errorf("blockers closed: %s is still held", id)
		}
	}
	if slices.Contains(after, danglingRow.ID) {
		t.Errorf("blockers closed: %s, whose blocker no store holds, is counted and served", danglingRow.ID)
	}
}
