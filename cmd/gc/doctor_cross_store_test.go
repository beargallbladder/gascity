package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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

// TestDanglingBlockingEdgesFixSparesBlockersInUnreadRigs: --fix acts only on
// targets proven absent from EVERY store. A blocker in the namespace of a
// suspended rig, or of a rig whose store will not open, is undecided — even a
// hyphenated rig prefix ("gc-ops") is matched whole — and while any store was
// skipped --fix touches nothing at all.
func TestDanglingBlockingEdgesFixSparesBlockersInUnreadRigs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		prefix    string
		suspended bool
		openFails bool
	}{
		{name: "suspended rig", prefix: "rg", suspended: true},
		{name: "rig store fails to open", prefix: "rg", openFails: true},
		{name: "suspended rig with a hyphenated prefix", prefix: "gc-ops", suspended: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			sessionSplitRoutes(t, cityPath)
			work := beads.NewMemStore()
			rig := beads.NewMemStore()
			rig.HonorExplicitIDs = true
			rb, err := rig.Create(beads.Bead{ID: tc.prefix + "-77", Title: "open rig blocker", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			row, err := work.Create(beads.Bead{Title: "row", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			if err := work.DepAdd(row.ID, rb.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
			cfg := &config.City{Rigs: []config.Rig{{Name: "rg", Path: "rigs/rg", Prefix: tc.prefix, Suspended: tc.suspended}}}
			check := newDanglingBlockingEdgesCheck(cfg, cityPath, func(p string) (beads.Store, error) {
				if p == "rigs/rg" {
					if tc.openFails {
						return nil, errors.New("dolt server restarting")
					}
					return rig, nil
				}
				return work, nil
			})
			if edges, _ := check.collect(); len(edges) != 0 {
				t.Fatalf("dangling edges = %+v; %s is in an unread rig's namespace and is undecided", edges, rb.ID)
			}
			ferr := check.Fix(nil)
			if tc.openFails && ferr == nil {
				t.Fatal("Fix succeeded although a rig store could not be read")
			}
			if deps, _ := work.DepList(row.ID, "down"); len(deps) != 1 {
				t.Fatalf("deps of %s after --fix = %+v; the edge to %s, held by an unread rig, was touched", row.ID, deps, rb.ID)
			}
		})
	}
}

// TestDanglingBlockingEdgesFixRewritesAMalformedDiscoveredFromLiteral is the
// maintainer-city shape (ga-26jnu, ga-0wmnm, ga-dsazz): a blocks edge to the
// literal "discovered-from:<id>" whose <id> the row's own store holds. --fix
// rewrites it as the real discovered-from edge in that store; nothing blocks
// afterwards. A literal whose <id> only another store holds is left for the
// operator, because rewriting it would write a cross-store edge.
func TestDanglingBlockingEdgesFixRewritesAMalformedDiscoveredFromLiteral(t *testing.T) {
	cityPath := t.TempDir()
	binding := sessionSplitRoutes(t, cityPath)
	work := beads.NewMemStore()
	origin, err := work.Create(beads.Bead{Title: "where it was discovered", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := binding.Create(beads.Bead{Title: "graph-side origin", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	row, err := work.Create(beads.Bead{Title: "discovered work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	cross, err := work.Create(beads.Bead{Title: "discovered from the graph", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := work.DepAdd(row.ID, "discovered-from:"+origin.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	if err := work.DepAdd(cross.ID, "discovered-from:"+elsewhere.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return work, nil })
	edges, skipped := check.collect()
	if len(skipped) != 0 || len(edges) != 2 {
		t.Fatalf("edges = %+v, skipped = %v; want the two malformed literals", edges, skipped)
	}
	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	deps, err := work.DepList(row.ID, "down")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != origin.ID || deps[0].Type != "discovered-from" {
		t.Fatalf("deps of %s after --fix = %+v; want one discovered-from edge to %s and no blocks edge", row.ID, deps, origin.ID)
	}
	crossDeps, err := work.DepList(cross.ID, "down")
	if err != nil {
		t.Fatal(err)
	}
	if len(crossDeps) != 1 || crossDeps[0].DependsOnID != "discovered-from:"+elsewhere.ID {
		t.Fatalf("deps of %s after --fix = %+v; want the literal left for the operator (its id lives in another store)", cross.ID, crossDeps)
	}
}
