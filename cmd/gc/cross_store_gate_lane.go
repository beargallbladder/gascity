package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
)

// The cross-store gate lane: the controller half of how a drain item waits for
// a blocker held in another store (internal/dispatch cross_store_gate.go).
//
// A drain on a split city cannot write an edge from its graph-resident item
// workflow to a work-resident blocker, so it holds the item behind a GATE in the
// graph binding that mirrors the blocker. This lane closes a gate once its
// blocker is closed in every work store that holds it (and closes the gates of
// a drain control that is closed or gone), which makes the item ready through
// an ordinary same-store edge every readiness reader already honors.
//
// # Off the tick
//
// The lane reads work stores, and the tick never reads the work ledger
// (ga-l7jdg; TestSteadyTickLedgerRoundTripBudgetIsZero). So it runs on its own
// goroutine, the shape of the route-recovery backstop poller, once per
// crossStoreGateInterval.
//
// # Cost
//
// A city that relocates nothing never mints a gate, and the loop does not even
// start reading: it checks the routes and returns. On a split city a pass reads
// the open gates from the binding (one local query) and, only when there are
// some, every distinct blocker in ONE batched read per work store. A store that
// is suspended or quiescent is not read, and neither is any store while the
// city is quiescent.
//
// # A store the pass could not read
//
// A blocker a store the pass did not read may hold is UNDECIDED: its gate stays
// exactly as it is — still holding its item, not released and not marked
// unresolved — until a pass can read every store that may hold it.

// crossStoreGateInterval bounds how often the lane runs on a split city. It is
// the latency between a work blocker closing and its drain item becoming ready.
const crossStoreGateInterval = 30 * time.Second

// runCrossStoreGateLoop polls the gate lane off the tick until ctx ends.
func (cr *CityRuntime) runCrossStoreGateLoop(ctx context.Context) {
	ticker := time.NewTicker(crossStoreGateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cr.safeTick(func() { cr.releaseCrossStoreGates() }, "cross-store-gates")
	}
}

// releaseCrossStoreGates runs one release pass and reports it.
func (cr *CityRuntime) releaseCrossStoreGates() dispatch.CrossStoreGateReport {
	graph := cr.graphBeadStore().Store
	city := cr.cityBeadStore()
	if graph == nil || city == nil || graph == city {
		// Nothing relocated: no drain ever needed a gate.
		return dispatch.CrossStoreGateReport{}
	}
	if cr.cs != nil && cr.cs.storesQuiescent() {
		// A quiescent city's stores are not touched; the gates wait for it.
		return dispatch.CrossStoreGateReport{}
	}
	cfg := cr.serviceConfigSnapshot()
	report, err := dispatch.ReleaseCrossStoreGates(graph, crossStoreBlockerResolver(graph, city, cfg, cr.rigBeadStores(), buildSuspendedRigPathsForCity(cfg, cr.cityPath))) // residency:allow — the work legs a gate's blocker may live in; see crossStoreBlockerResolver
	if len(report.Released) > 0 {
		fmt.Fprintf(cr.stdout, "Cross-store gates: released %d (%s); their drain items may run\n", len(report.Released), strings.Join(report.Released, " ")) //nolint:errcheck // best-effort stdout
	}
	for _, gate := range report.NewlyUnresolved {
		cr.recordUnresolvedCrossStoreGate(gate)
	}
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: cross-store gates: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
	}
	return report
}

// recordUnresolvedCrossStoreGate makes a gate whose blocker no store holds
// visible: once, on the transition, as a control.stalled event on its drain
// control (error_class "pending": the drain is open and an operator can heal
// it), and on stderr. `gc doctor` reports every such gate for as long as it is
// open (cross-store-gates).
func (cr *CityRuntime) recordUnresolvedCrossStoreGate(gate beads.Bead) {
	blocker := gate.Metadata[beadmeta.CrossStoreBlockerMetadataKey]
	control := gate.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey]
	msg := fmt.Sprintf("drain items wait on cross-store gate %s, whose blocker %s is held by no store this city federates; close the gate to release them", gate.ID, blocker)
	fmt.Fprintf(cr.stderr, "%s: cross-store gates: %s\n", cr.logPrefix, msg) //nolint:errcheck // best-effort stderr
	if cr.rec == nil {
		return
	}
	cr.rec.Record(events.Event{
		Type:    events.ControlStalled,
		Actor:   "gc",
		Subject: control,
		Payload: events.ControlStalledPayloadJSON(events.ControlStalledPayload{
			BeadID:     control,
			Kind:       "drain",
			RootBeadID: gate.Metadata[beadmeta.RootBeadIDMetadataKey],
			ErrorClass: "pending",
			FirstSeen:  time.Now().UTC().Format(time.RFC3339),
			Error:      msg,
		}),
	})
}

// crossStoreBlockerResolver reads every gate blocker from every work store a
// blocker may live in — the city store, then each serving rig store — in ONE
// batched read per store, and returns every copy it finds. The binding the
// gates live in is skipped: a blocker it held would have been an ordinary edge,
// never a gate.
//
// A blocker in a SUSPENDED rig's namespace is undecided (the rig's store is
// not read), and every blocker is undecided once any store's read fails: that
// store might hold an open copy of any of them.
func crossStoreBlockerResolver(binding, city beads.Store, cfg *config.City, rigs map[string]beads.Store, suspended map[string]bool) dispatch.CrossStoreBlockerResolver {
	serving := servingRigStores(cfg, rigs, suspended)
	// A configured rig servingRigStores dropped is suspended (or has no store
	// this runtime holds): its store is not read, so its namespace is undecided.
	var suspendedPrefixes []string
	if cfg != nil {
		for _, rig := range cfg.Rigs {
			if _, live := serving[rig.Name]; live {
				continue
			}
			if prefix := strings.ToLower(strings.Trim(rig.EffectivePrefix(), "-")); prefix != "" {
				suspendedPrefixes = append(suspendedPrefixes, prefix+"-")
			}
		}
	}
	legs := []beads.Store{city} // residency:allow — a gate releases only once its blocker is closed in EVERY work store that holds it, so every copy is read, not the first owner
	names := make([]string, 0, len(serving))
	for name := range serving {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		leg := serving[name]
		if leg == nil || leg == binding || leg == city {
			continue
		}
		dup := false
		for _, have := range legs {
			if have == leg {
				dup = true
			}
		}
		if !dup {
			legs = append(legs, leg)
		}
	}
	return func(ids []string) (map[string][]beads.Bead, map[string]bool, error) {
		copies := make(map[string][]beads.Bead, len(ids))
		undecided := make(map[string]bool)
		for _, id := range ids {
			for _, prefix := range suspendedPrefixes {
				if strings.ToLower(beads.BdIDPrefix(id)) == prefix {
					undecided[id] = true
				}
			}
		}
		for _, leg := range legs {
			if leg == nil || leg == binding {
				continue
			}
			found, err := beads.GetMany(leg, ids)
			if err != nil {
				for _, id := range ids {
					undecided[id] = true
				}
				continue
			}
			for _, id := range ids {
				if b, ok := found[id]; ok {
					copies[id] = append(copies[id], b)
				}
			}
		}
		return copies, undecided, nil
	}
}
