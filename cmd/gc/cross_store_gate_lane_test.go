package main

import (
	"bytes"
	"context"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/dispatch"
)

// TestCrossStoreGateLaneReleasesADrainItemOnceItsWorkBlockerCloses drives the
// controller's tick phase over a split city: a real SQLite binding holding a
// drain item behind a cross-store gate, and a strict work store holding the
// gate's blocker. The item is not ready while the blocker is open, the lane
// keeps the gate, and once the blocker closes in the work store the lane
// releases the gate and the item becomes ready in the binding's own Ready.
func TestCrossStoreGateLaneReleasesADrainItemOnceItsWorkBlockerCloses(t *testing.T) {
	binding := newSplitEnvClassLeaf(t)
	work := splittest.NewWorkStore(t, "gc")
	rig := splittest.NewWorkStore(t, "mr")
	var stdout bytes.Buffer
	cr := &CityRuntime{
		cityName:            "city",
		standaloneCityStore: work,
		standaloneRigStores: map[string]beads.Store{"myrig": rig},
		storageRoutes:       splitEnvRoutes(binding),
		logPrefix:           "gc",
		stdout:              &stdout,
		stderr:              io.Discard,
	}

	blocker, err := rig.Create(beads.Bead{Title: "work blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	control, err := binding.Create(beads.Bead{Title: "drain", Type: "task", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gcg-root"}})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := binding.Create(beads.Bead{
		Title:  "waits for " + blocker.ID,
		Type:   "gate",
		Labels: []string{dispatch.CrossStoreGateLabel},
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:          "gcg-root",
			beadmeta.CrossStoreGateOwnerMetadataKey: control.ID,
			beadmeta.CrossStoreBlockerMetadataKey:   blocker.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err := binding.Create(beads.Bead{Title: "drain item step", Type: "task", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gcg-item"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := binding.DepAdd(item.ID, gate.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	itemReady := func() bool {
		t.Helper()
		rows, err := binding.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(rows, func(b beads.Bead) bool { return b.ID == item.ID })
	}
	pass := func() {
		t.Helper()
		cr.crossStoreGates = crossStoreGateLane{} // each pass is due
		if cr.tickReleaseCrossStoreGates(&tickPass{ctx: context.Background()}) {
			t.Fatal("the gate phase stopped the tick")
		}
	}

	pass()
	if itemReady() {
		t.Fatalf("drain item %s is ready while its gate's blocker %s is open in the rig store", item.ID, blocker.ID)
	}
	if err := rig.Close(blocker.ID); err != nil {
		t.Fatal(err)
	}
	pass()
	if got, err := binding.Get(gate.ID); err != nil || got.Status != "closed" {
		t.Fatalf("gate %s = %+v, %v after its blocker closed; want it released", gate.ID, got, err)
	}
	if !itemReady() {
		t.Fatalf("drain item %s is not ready after its gate was released", item.ID)
	}
}

// TestCrossStoreGateLaneReadsNothingOnASingleStoreCity is the compatibility
// control: a city that relocates nothing never mints a gate, so the phase must
// not read a store at all.
func TestCrossStoreGateLaneReadsNothingOnASingleStoreCity(t *testing.T) {
	work := &countingRouteStore{Store: beads.NewMemStore()}
	cr := &CityRuntime{cityName: "city", standaloneCityStore: work, logPrefix: "gc", stdout: io.Discard, stderr: io.Discard}
	if cr.tickReleaseCrossStoreGates(&tickPass{ctx: context.Background()}) {
		t.Fatal("the gate phase stopped the tick")
	}
	if work.reads() != 0 {
		t.Fatalf("the gate phase issued %d read(s) on a single-store city, want 0", work.reads())
	}
}

// TestCrossStoreGateLaneIsCadenced pins the bound on how often a pass reads
// the work legs: at most once per crossStoreGateInterval.
func TestCrossStoreGateLaneIsCadenced(t *testing.T) {
	var lane crossStoreGateLane
	now := time.Now()
	if !lane.due(now) {
		t.Fatal("the first pass is not due")
	}
	if lane.due(now.Add(crossStoreGateInterval / 2)) {
		t.Fatal("a second pass inside the interval is due")
	}
	if !lane.due(now.Add(crossStoreGateInterval)) {
		t.Fatal("a pass one interval later is not due")
	}
}
