package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/doctor"
)

// Doctor checks for the split-city cross-store blocker rule (internal/readywork
// crossstore.go, internal/dispatch cross_store_gate.go). Both report nothing on
// a city with no class binding: there every store keeps its own ready answer.

// doctorScope is one store a cross-store check reads, by its operator label.
type doctorScope struct {
	label string
	store beads.Store
}

// splitCityDoctorScopes returns the stores a split city's cross-store checks
// read — the city work store, every non-suspended rig store and the class
// binding — or nil when the city relocates nothing. skipped names the scopes
// whose store would not open.
func splitCityDoctorScopes(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) (scopes []doctorScope, binding beads.Store, skipped []string) {
	graph, relocated := cliStorageRoutes(cityPath).storeFor(coordclass.ClassGraph) // residency:allow — a doctor scan of every store a split city's blockers may live in
	if !relocated || graph == nil || newStore == nil {
		return nil, nil, nil
	}
	paths := []struct{ label, path string }{{"city", cityPath}}
	if cfg != nil {
		for _, rig := range cfg.Rigs {
			if rig.Suspended || strings.TrimSpace(rig.Path) == "" {
				continue
			}
			paths = append(paths, struct{ label, path string }{"rig " + rig.Name, rig.Path})
		}
	}
	for _, p := range paths {
		store, err := newStore(p.path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: opening bead store: %v", p.label, err))
			continue
		}
		scopes = append(scopes, doctorScope{label: p.label, store: store})
	}
	scopes = append(scopes, doctorScope{label: "graph binding", store: graph})
	return scopes, graph, skipped
}

// crossStoreGatesCheck reports the open cross-store gates whose blocker no
// store holds (gc.cross_store_gate_state=unresolved). Each holds a drain's
// items forever: an unresolvable blocker blocks. The operator decides — close
// the gate to release the items, or restore the blocker.
type crossStoreGatesCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newCrossStoreGatesCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *crossStoreGatesCheck {
	return &crossStoreGatesCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *crossStoreGatesCheck) Name() string         { return "cross-store-gates" }
func (c *crossStoreGatesCheck) CanFix() bool         { return false }
func (c *crossStoreGatesCheck) WarmupEligible() bool { return false }
func (c *crossStoreGatesCheck) Fix(*doctor.CheckContext) error {
	return nil
}

func (c *crossStoreGatesCheck) Run(*doctor.CheckContext) *doctor.CheckResult {
	graph, relocated := cliStorageRoutes(c.cityPath).storeFor(coordclass.ClassGraph) // residency:allow — the gates live in the binding, by construction
	if !relocated || graph == nil {
		return okCheck(c.Name(), "no class binding; drains need no cross-store gates")
	}
	gates, err := graph.List(beads.ListQuery{
		Label:    dispatch.CrossStoreGateLabel,
		Status:   "open",
		Metadata: map[string]string{beadmeta.CrossStoreGateStateMetadataKey: dispatch.CrossStoreGateStateUnresolved},
		Live:     true,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return warnCheck(c.Name(), "could not list cross-store gates", "fix the class binding, then rerun gc doctor", []string{err.Error()})
	}
	if len(gates) == 0 {
		return okCheck(c.Name(), "no cross-store gate waits on a blocker no store holds")
	}
	details := make([]string, 0, len(gates))
	for _, gate := range gates {
		details = append(details, fmt.Sprintf("gate %s (drain %s) waits on %s, which no store holds", gate.ID, gate.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey], gate.Metadata[beadmeta.CrossStoreBlockerMetadataKey]))
	}
	sort.Strings(details)
	return warnCheck(c.Name(),
		fmt.Sprintf("%d cross-store gate(s) hold drain items on a blocker no store holds", len(gates)),
		"restore the blocker, or close the gate (gc bd close <gate>) to release its drain items",
		details)
}

// danglingBlockingEdgesCheck reports open and in-progress rows whose blocking
// edge names a bead no store holds. On a split city gc ready holds such a row
// forever (an unresolvable blocker blocks), where bd on a single-store city
// would have served it. --fix removes the dangling edge from the row's own
// store; it never writes an edge, so it never writes a cross-store one.
type danglingBlockingEdgesCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newDanglingBlockingEdgesCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *danglingBlockingEdgesCheck {
	return &danglingBlockingEdgesCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *danglingBlockingEdgesCheck) Name() string         { return "dangling-blocking-edges" }
func (c *danglingBlockingEdgesCheck) CanFix() bool         { return true }
func (c *danglingBlockingEdgesCheck) WarmupEligible() bool { return false }

// danglingEdge is one blocking edge no store resolves.
type danglingEdge struct {
	label  string
	store  beads.Store
	rowID  string
	target string // the edge's DependsOnID as written
}

// collect reads, per store, the open and in-progress rows (two lists), their
// edges (one batched read) and then every distinct target from every store
// (one batched read per store).
func (c *danglingBlockingEdgesCheck) collect() (edges []danglingEdge, skipped []string) {
	scopes, _, skipped := splitCityDoctorScopes(c.cfg, c.cityPath, c.newStore)
	if len(scopes) == 0 {
		return nil, skipped
	}
	type candidate struct {
		scope  doctorScope
		rowID  string
		raw    string
		target string
	}
	var candidates []candidate
	targetSet := make(map[string]bool)
	for _, sc := range scopes {
		var rows []beads.Bead
		for _, status := range []string{"open", "in_progress"} {
			got, err := sc.store.List(beads.ListQuery{Status: status, AllowScan: true, Live: true, TierMode: beads.TierBoth})
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("%s skipped: listing %s rows: %v", sc.label, status, err))
				rows = nil
				break
			}
			rows = append(rows, got...)
		}
		if len(rows) == 0 {
			continue
		}
		ids := make([]string, 0, len(rows))
		for _, b := range rows {
			ids = append(ids, b.ID)
		}
		deps, err := doctorDepsFor(sc.store, ids)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing dependencies: %v", sc.label, err))
			continue
		}
		for _, id := range ids {
			for _, dep := range deps[id] {
				if !beads.IsReadyBlockingDependencyType(dep.Type) {
					continue
				}
				target, capability := beads.ExternalBlockerTarget(dep.DependsOnID)
				if capability || target == "" {
					continue
				}
				candidates = append(candidates, candidate{scope: sc, rowID: id, raw: dep.DependsOnID, target: target})
				targetSet[target] = true
			}
		}
	}
	if len(candidates) == 0 {
		return nil, skipped
	}
	targets := make([]string, 0, len(targetSet))
	for t := range targetSet {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	held := make(map[string]bool, len(targets))
	for _, sc := range scopes {
		found, err := beads.GetMany(sc.store, targets)
		if err != nil {
			// A store that cannot be read may hold any target: report nothing
			// rather than call a held blocker dangling.
			return nil, append(skipped, fmt.Sprintf("%s: reading blockers: %v (no edge reported dangling while a store is unreadable)", sc.label, err))
		}
		for id := range found {
			held[id] = true
		}
	}
	for _, cand := range candidates {
		if held[cand.target] {
			continue
		}
		edges = append(edges, danglingEdge{label: cand.scope.label, store: cand.scope.store, rowID: cand.rowID, target: cand.raw})
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].rowID != edges[j].rowID {
			return edges[i].rowID < edges[j].rowID
		}
		return edges[i].target < edges[j].target
	})
	return edges, skipped
}

// doctorDepsFor reads ids' down edges in one batched read where the store has
// one, otherwise one DepList per row.
func doctorDepsFor(store beads.Store, ids []string) (map[string][]beads.Dep, error) {
	if batch, ok := beads.DepListBatchFor(store); ok {
		if deps, err := batch.DepListBatch(ids); err == nil {
			return deps, nil
		}
	}
	out := make(map[string][]beads.Dep, len(ids))
	for _, id := range ids {
		deps, err := store.DepList(id, "down")
		if err != nil {
			return nil, err
		}
		out[id] = deps
	}
	return out, nil
}

func (c *danglingBlockingEdgesCheck) Run(*doctor.CheckContext) *doctor.CheckResult {
	edges, skipped := c.collect()
	if len(edges) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no open row is held by a blocking edge no store resolves")
	}
	details := make([]string, 0, len(edges)+len(skipped))
	for _, e := range edges {
		details = append(details, fmt.Sprintf("%s row %s blocks on %s, which no store holds", e.label, e.rowID, e.target))
	}
	details = append(details, skipped...)
	if len(edges) == 0 {
		return warnCheck(c.Name(), fmt.Sprintf("dangling-edge scan skipped %d scope(s)", len(skipped)), "fix bead store access, then rerun gc doctor", details)
	}
	return warnCheck(c.Name(),
		fmt.Sprintf("%d blocking edge(s) name a bead no store holds; gc ready holds their rows forever on a split city", len(edges)),
		"run gc doctor --fix to remove the dangling edges (no edge is written)",
		details)
}

func (c *danglingBlockingEdgesCheck) Fix(*doctor.CheckContext) error {
	edges, skipped := c.collect()
	for _, e := range edges {
		if err := e.store.DepRemove(e.rowID, e.target); err != nil {
			return fmt.Errorf("%s row %s: removing dangling edge to %s: %w", e.label, e.rowID, e.target, err)
		}
	}
	if len(skipped) > 0 {
		return fmt.Errorf("dangling-blocking-edges skipped %d scope(s): %s", len(skipped), strings.Join(skipped, "; "))
	}
	return nil
}
