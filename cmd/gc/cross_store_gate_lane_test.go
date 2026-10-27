package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
)

// gateLaneCity is a split city for the gate lane: a real SQLite binding, a
// strict work store, and the rigs given, each a counting strict store.
type gateLaneCity struct {
	cr      *CityRuntime
	binding beads.Store
	work    *countingRouteStore
	rigs    map[string]*countingRouteStore
	rec     *events.Fake
}

func newGateLaneCity(t *testing.T, rigNames ...string) *gateLaneCity {
	t.Helper()
	binding := newSplitEnvClassLeaf(t)
	work := &countingRouteStore{Store: splittest.NewWorkStore(t, "gc")}
	cfg := &config.City{Workspace: config.Workspace{Name: "city", Prefix: "gc"}}
	rigStores := map[string]beads.Store{}
	rigs := map[string]*countingRouteStore{}
	for i, name := range rigNames {
		prefix := "r" + strconv.Itoa(i)
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: name, Path: "rigs/" + name, Prefix: prefix})
		store := &countingRouteStore{Store: splittest.NewWorkStore(t, prefix)}
		rigs[name] = store
		rigStores[name] = store
	}
	rec := events.NewFake()
	cr := &CityRuntime{
		cityName:            "city",
		cityPath:            t.TempDir(),
		cfg:                 cfg,
		standaloneCityStore: work,
		standaloneRigStores: rigStores,
		storageRoutes:       splitEnvRoutes(binding),
		rec:                 rec,
		logPrefix:           "gc",
		stdout:              io.Discard,
		stderr:              io.Discard,
	}
	return &gateLaneCity{cr: cr, binding: binding, work: work, rigs: rigs, rec: rec}
}

// gate mints a drain control and its gate on blocker in the binding, plus an
// item step held behind the gate, and returns the gate and the item.
func (c *gateLaneCity) gate(t *testing.T, blocker string) (gate, item beads.Bead) {
	t.Helper()
	control, err := c.binding.Create(beads.Bead{Title: "drain", Type: "task", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gcg-root"}})
	if err != nil {
		t.Fatal(err)
	}
	gate, err = c.binding.Create(beads.Bead{
		Title:  "waits for " + blocker,
		Type:   "gate",
		Labels: []string{dispatch.CrossStoreGateLabel},
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:          "gcg-root",
			beadmeta.CrossStoreGateOwnerMetadataKey: control.ID,
			beadmeta.CrossStoreBlockerMetadataKey:   blocker,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err = c.binding.Create(beads.Bead{Title: "drain item step", Type: "task", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gcg-item"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.binding.DepAdd(item.ID, gate.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	return gate, item
}

func (c *gateLaneCity) ready(t *testing.T, id string) bool {
	t.Helper()
	rows, err := c.binding.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(rows, func(b beads.Bead) bool { return b.ID == id })
}

// TestCrossStoreGateLaneReleasesADrainItemOnceItsWorkBlockerCloses drives the
// off-tick pass over a split city: the item is held while its blocker is open
// in a rig store, and once the blocker closes the pass releases the gate and
// the item becomes ready in the binding's own Ready.
func TestCrossStoreGateLaneReleasesADrainItemOnceItsWorkBlockerCloses(t *testing.T) {
	c := newGateLaneCity(t, "myrig")
	blocker, err := c.rigs["myrig"].Create(beads.Bead{Title: "work blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	gate, item := c.gate(t, blocker.ID)

	c.cr.releaseCrossStoreGates()
	if c.ready(t, item.ID) {
		t.Fatalf("drain item %s is ready while its gate's blocker %s is open", item.ID, blocker.ID)
	}
	if err := c.rigs["myrig"].Close(blocker.ID); err != nil {
		t.Fatal(err)
	}
	report := c.cr.releaseCrossStoreGates()
	if !slices.Equal(report.Released, []string{gate.ID}) {
		t.Fatalf("report = %+v, want gate %s released", report, gate.ID)
	}
	if !c.ready(t, item.ID) {
		t.Fatalf("drain item %s is not ready after its gate was released", item.ID)
	}
}

// TestCrossStoreGateLaneReadsEachStoreOncePerPass bounds the pass's cost: ten
// gates over four rigs cost ONE batched read per work store, not one read per
// gate per store (the review measured 50 serial round trips, ~4.5 min at
// maintainer-city's 5.4 s ledger query).
func TestCrossStoreGateLaneReadsEachStoreOncePerPass(t *testing.T) {
	c := newGateLaneCity(t, "a", "b", "c", "d")
	for i := 0; i < 10; i++ {
		blocker, err := c.rigs["a"].Create(beads.Bead{Title: "blocker", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		c.gate(t, blocker.ID)
	}
	c.cr.releaseCrossStoreGates()
	stores := map[string]*countingRouteStore{"city": c.work}
	for name, store := range c.rigs {
		stores[name] = store
	}
	for name, store := range stores {
		if got := store.reads(); got > 1 {
			t.Errorf("store %s was read %d time(s) in one pass, want at most 1 batched read", name, got)
		}
	}
}

// TestCrossStoreGateLaneKeepsAGateWhileAStoreIsUnreadable: a store whose read
// fails might hold an open copy of any blocker, so no gate is released and none
// is marked unresolved until the store can be read.
func TestCrossStoreGateLaneKeepsAGateWhileAStoreIsUnreadable(t *testing.T) {
	c := newGateLaneCity(t, "myrig")
	blocker, err := c.work.Create(beads.Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.work.Close(blocker.ID); err != nil {
		t.Fatal(err)
	}
	gate, _ := c.gate(t, blocker.ID)
	ghost, _ := c.gate(t, "gc-404")
	c.cr.standaloneRigStores["myrig"] = unreadableGateLeg{Store: c.rigs["myrig"]}

	report := c.cr.releaseCrossStoreGates()
	if len(report.Released) != 0 || len(report.Unresolved) != 0 {
		t.Fatalf("report = %+v; want both gates kept while the rig store is unreadable", report)
	}
	for _, id := range []string{gate.ID, ghost.ID} {
		got, err := c.binding.Get(id)
		if err != nil || got.Status == "closed" || got.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != "" {
			t.Fatalf("gate %s = %+v, %v; want it open and unmarked", id, got, err)
		}
	}
}

type unreadableGateLeg struct{ beads.Store }

func (unreadableGateLeg) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("store unavailable")
}

func (unreadableGateLeg) Get(string) (beads.Bead, error) {
	return beads.Bead{}, errors.New("store unavailable")
}

// TestCrossStoreGateLaneLeavesASuspendedRigsBlockersUndecided: a suspended
// rig's store is not read, so a blocker in its namespace keeps its gate, not
// marked unresolved.
func TestCrossStoreGateLaneLeavesASuspendedRigsBlockersUndecided(t *testing.T) {
	c := newGateLaneCity(t, "myrig")
	c.cr.cfg.Rigs[0].Suspended = true
	gate, _ := c.gate(t, "r0-1")
	report := c.cr.releaseCrossStoreGates()
	if len(report.Released)+len(report.Unresolved) != 0 {
		t.Fatalf("report = %+v; want the suspended rig's blocker undecided", report)
	}
	if c.rigs["myrig"].reads() != 0 {
		t.Fatalf("the suspended rig's store was read %d time(s)", c.rigs["myrig"].reads())
	}
	if got, _ := c.binding.Get(gate.ID); got.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != "" {
		t.Fatalf("gate %s was marked %q", gate.ID, got.Metadata[beadmeta.CrossStoreGateStateMetadataKey])
	}
}

// TestCrossStoreGateLaneReportsAnUnresolvedGateOnce: a gate whose blocker no
// store holds is marked unresolved and announced ONCE, as control.stalled on
// its drain control.
func TestCrossStoreGateLaneReportsAnUnresolvedGateOnce(t *testing.T) {
	c := newGateLaneCity(t)
	var stderr bytes.Buffer
	c.cr.stderr = &stderr
	gate, _ := c.gate(t, "gc-404")
	c.cr.releaseCrossStoreGates()
	c.cr.releaseCrossStoreGates()
	var stalled []events.Event
	for _, evt := range c.rec.Events {
		if evt.Type == events.ControlStalled {
			stalled = append(stalled, evt)
		}
	}
	if len(stalled) != 1 {
		t.Fatalf("control.stalled events = %d, want exactly 1 across two passes", len(stalled))
	}
	var payload events.ControlStalledPayload
	if err := json.Unmarshal(stalled[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.BeadID != gate.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey] {
		t.Fatalf("payload = %+v, want the gate's drain control", payload)
	}
	if payload.ErrorClass != "pending" {
		t.Fatalf("error_class = %q, want pending (the drain is open and healable)", payload.ErrorClass)
	}
	if got, _ := c.binding.Get(gate.ID); got.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != dispatch.CrossStoreGateStateUnresolved {
		t.Fatalf("gate state = %q, want unresolved", got.Metadata[beadmeta.CrossStoreGateStateMetadataKey])
	}
}

// TestCrossStoreGateLaneReadsNothingOnASingleStoreCity is the compatibility
// control: a city that relocates nothing never mints a gate, so the pass must
// not read a store at all.
func TestCrossStoreGateLaneReadsNothingOnASingleStoreCity(t *testing.T) {
	work := &countingRouteStore{Store: beads.NewMemStore()}
	cr := &CityRuntime{cityName: "city", standaloneCityStore: work, logPrefix: "gc", stdout: io.Discard, stderr: io.Discard}
	cr.releaseCrossStoreGates()
	if work.reads() != 0 {
		t.Fatalf("the gate pass issued %d read(s) on a single-store city, want 0", work.reads())
	}
}
