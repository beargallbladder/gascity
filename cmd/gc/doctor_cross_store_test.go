package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/doctor"
)

// TestDanglingBlockingEdgesCheckFindsAndRemovesOnlyDanglingEdges: on a split
// city the check reports the open and in-progress rows whose blocking edge
// names a bead no store holds — the maintainer-city shape is a blocks edge to a
// malformed "discovered-from:<id>" literal — and --fix removes exactly those
// edges. A cross-store edge whose target a store holds, a capability ref and a
// closed row are left alone, and nothing is written but the removals.
func TestDanglingBlockingEdgesCheckFindsAndRemovesOnlyDanglingEdges(t *testing.T) {
	cityPath := t.TempDir()
	binding := sessionSplitRoutes(t, cityPath)
	work := beads.NewMemStore()
	step, err := binding.Create(beads.Bead{Title: "graph step", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(title, status string, targets ...string) beads.Bead {
		t.Helper()
		b, err := work.Create(beads.Bead{Title: title, Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range targets {
			if err := work.DepAdd(b.ID, target, "blocks"); err != nil {
				t.Fatal(err)
			}
		}
		if status != "" && status != "open" {
			s := status
			if err := work.Update(b.ID, beads.UpdateOpts{Status: &s}); err != nil {
				t.Fatal(err)
			}
		}
		return b
	}
	malformed := mk("malformed literal", "open", "discovered-from:gc-404")
	gone := mk("external to nothing", "in_progress", "external:gc-405")
	mk("cross-store, held", "open", step.ID)
	mk("capability", "open", "external:rig:some-capability")
	mk("closed row", "closed", "discovered-from:gc-406")

	check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return work, nil })
	edges, skipped := check.collect()
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v", skipped)
	}
	var got []string
	for _, e := range edges {
		got = append(got, e.rowID+"->"+e.target)
	}
	want := []string{malformed.ID + "->discovered-from:gc-404", gone.ID + "->external:gc-405"}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("dangling edges = %v, want %v", got, want)
	}
	if res := check.Run(nil); res.Status != doctor.StatusWarning || !strings.Contains(res.Message, "2 blocking edge") {
		t.Fatalf("Run = %+v, want a warning naming 2 edges", res)
	}
	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if edges, _ := check.collect(); len(edges) != 0 {
		t.Fatalf("after --fix dangling edges = %+v, want none", edges)
	}
	if res := check.Run(nil); res.Status != doctor.StatusOK {
		t.Fatalf("Run after --fix = %+v, want OK", res)
	}
	deps, err := work.DepList(malformed.ID, "down")
	if err != nil || len(deps) != 0 {
		t.Fatalf("deps of %s after --fix = %+v, %v; want the dangling edge removed and nothing written", malformed.ID, deps, err)
	}
}

// TestDanglingBlockingEdgesCheckIsQuietOnASingleStoreCity: a city with no
// class binding keeps bd's answer, so the check reads nothing.
func TestDanglingBlockingEdgesCheckIsQuietOnASingleStoreCity(t *testing.T) {
	cityPath := t.TempDir()
	seedCLIStorageRoutes(t, cityPath, nil)
	opened := 0
	check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) {
		opened++
		return beads.NewMemStore(), nil
	})
	if res := check.Run(nil); res.Status != doctor.StatusOK || opened != 0 {
		t.Fatalf("Run = %+v with %d store open(s), want OK and none", res, opened)
	}
}

// TestCrossStoreGatesCheckReportsUnresolvedGates: an open gate whose blocker no
// store holds is a doctor finding for as long as it stays open.
func TestCrossStoreGatesCheckReportsUnresolvedGates(t *testing.T) {
	cityPath := t.TempDir()
	binding := sessionSplitRoutes(t, cityPath)
	check := newCrossStoreGatesCheck(nil, cityPath, nil)
	if res := check.Run(nil); res.Status != doctor.StatusOK {
		t.Fatalf("Run with no gates = %+v, want OK", res)
	}
	gate, err := binding.Create(beads.Bead{
		Title:  "waits for gc-404",
		Type:   "gate",
		Labels: []string{dispatch.CrossStoreGateLabel},
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:          "gcg-root",
			beadmeta.CrossStoreGateOwnerMetadataKey: "gcg-drain",
			beadmeta.CrossStoreBlockerMetadataKey:   "gc-404",
			beadmeta.CrossStoreGateStateMetadataKey: dispatch.CrossStoreGateStateUnresolved,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := check.Run(nil)
	if res.Status != doctor.StatusWarning || len(res.Details) != 1 || !strings.Contains(res.Details[0], gate.ID) {
		t.Fatalf("Run = %+v, want a warning naming gate %s", res, gate.ID)
	}
}
