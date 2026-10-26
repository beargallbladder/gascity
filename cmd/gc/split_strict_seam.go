package main

import "github.com/gastownhall/gascity/internal/beads"

// splitStrictSeam is the split-strict test gate's hook into the city's store
// accessors. It is nil in every production binary, so the helpers below return
// their argument unchanged: only cmd/gc's test binary installs it, and only
// under GC_TEST_SPLIT_STRICT=1 (split_strict_gate_test.go).
//
// With it installed, a city whose routes relocate nothing still looks split to
// the code under test: work accessors (controllerState.CityBeadStore, BeadStore,
// BeadStores, and the CityRuntime's standalone stores) return a view of the one
// physical store that hides infrastructure rows, and resolveClassStore's
// identity branch returns a view that hides work rows. A read that reaches the
// wrong one is charged to its call site (internal/beads/splittest/projection.go).
var splitStrictSeam interface {
	workView(beads.Store) beads.Store
	classView(beads.Store) beads.Store
}

// splitStrictWorkView returns the gate's work view of store, or store.
func splitStrictWorkView(store beads.Store) beads.Store {
	if splitStrictSeam == nil || store == nil {
		return store
	}
	return splitStrictSeam.workView(store)
}

// splitStrictClassView returns the gate's class view of store, or store.
func splitStrictClassView(store beads.Store) beads.Store {
	if splitStrictSeam == nil || store == nil {
		return store
	}
	return splitStrictSeam.classView(store)
}

// splitStrictWorkViews applies splitStrictWorkView to every store of a map,
// returning the map itself when the gate is off.
//
// residency:allow — projects the caller's own snapshot store for store; it
// opens no store, resolves no bead and builds no leg list of its own.
func splitStrictWorkViews(stores map[string]beads.Store) map[string]beads.Store {
	if splitStrictSeam == nil || stores == nil {
		return stores
	}
	out := make(map[string]beads.Store, len(stores))
	for name, store := range stores {
		out[name] = splitStrictWorkView(store)
	}
	return out
}
