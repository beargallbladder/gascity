package main

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/dispatch"
)

// The cross-store gate lane: the controller half of how a drain item waits for
// a blocker held in another store (internal/dispatch cross_store_gate.go).
//
// A drain on a split city cannot write an edge from its graph-resident item
// workflow to a work-resident blocker, so it holds the item behind a GATE in the
// graph binding that mirrors the blocker. This lane closes a gate once its
// blocker is closed in every work store that holds it (and closes the gates of
// a drain control that has closed), which makes the item ready through an
// ordinary same-store edge that every readiness reader already honors.
//
// # Cost
//
// A city that relocates nothing never mints a gate and this lane does nothing:
// it returns before reading anything. On a split city a pass reads the open
// gates from the binding (one local query) and, only when there are some, each
// gate's blocker from the work legs. Passes run at most once per
// crossStoreGateInterval, so a remote work ledger sees a handful of reads a
// minute while a drain is waiting, and none at all otherwise.

// crossStoreGateInterval bounds how often the lane runs on a split city. It is
// the latency between a work blocker closing and its drain item becoming ready.
const crossStoreGateInterval = 30 * time.Second

// crossStoreGateLane holds the lane's cadence.
type crossStoreGateLane struct {
	mu      sync.Mutex
	lastRun time.Time
}

// due reports whether a pass should run at now, and records it when it should.
func (l *crossStoreGateLane) due(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.lastRun.IsZero() && now.Sub(l.lastRun) < crossStoreGateInterval {
		return false
	}
	l.lastRun = now
	return true
}

// tickReleaseCrossStoreGates runs one release pass when it is due.
func (cr *CityRuntime) tickReleaseCrossStoreGates(p *tickPass) bool {
	graph := cr.graphBeadStore().Store
	city := cr.cityBeadStore()
	if graph == nil || city == nil || graph == city {
		// Nothing relocated: no drain ever needed a gate.
		return false
	}
	if !cr.crossStoreGates.due(time.Now()) {
		return false
	}
	phaseStart := time.Now()
	report, err := dispatch.ReleaseCrossStoreGates(graph, crossStoreBlockerResolver(graph, city, cr.rigBeadStores()))
	fields := map[string]any{
		"open":       report.Open,
		"released":   len(report.Released),
		"unresolved": len(report.Unresolved),
	}
	p.recordPhase(TraceSiteControllerTickPhase, "release_cross_store_gates", phaseStart, fields)
	if len(report.Released) > 0 {
		fmt.Fprintf(cr.stdout, "Cross-store gates: released %d (%s); their drain items may run\n", len(report.Released), strings.Join(report.Released, " ")) //nolint:errcheck // best-effort stdout
	}
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: cross-store gates: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
	}
	return p.ctx.Err() != nil
}

// crossStoreBlockerResolver looks a gate's blocker up in every work store —
// the city store first, then each rig store — and returns every copy it finds.
// The binding the gates live in is skipped: a blocker it held would have been
// an ordinary edge, never a gate.
func crossStoreBlockerResolver(binding, city beads.Store, rigs map[string]beads.Store) dispatch.CrossStoreBlockerResolver {
	legs := []beads.Store{city}
	names := make([]string, 0, len(rigs))
	for name := range rigs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if leg := rigs[name]; leg != nil && leg != binding && !slices.Contains(legs, leg) {
			legs = append(legs, leg)
		}
	}
	return func(id string) ([]beads.Bead, error) {
		var found []beads.Bead
		for _, leg := range legs {
			if leg == nil || leg == binding {
				continue
			}
			b, err := beads.HandlesFor(leg).Live.Get(id)
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			found = append(found, b)
		}
		return found, nil
	}
}
