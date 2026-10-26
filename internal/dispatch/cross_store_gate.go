package dispatch

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
)

// Cross-store gates: how a drain item waits for a blocker held in another
// store without writing an edge that store cannot resolve.
//
// # The problem
//
// A drained member's out-of-convoy `blocks` dependency names a WORK bead, and
// the item workflow the drain builds for that member lives in the graph
// binding. A dependency row can only name an id its own store holds: written
// anyway, SQLite reads the absent target as never closed and strands the item
// forever, while bd/Dolt reads it as external and never blocks at all. So the
// drain used to drop the edge, stamp gc.drain_unprojected_blockers on the item
// root, and let the item run at once — ahead of the work its member was
// waiting for (read-audit R3). On a single-store city the edge exists and the
// item waits.
//
// # The design: a local gate that mirrors the remote blocker
//
// For each open blocker the item's store cannot hold, the drain mints a GATE in
// the item's own store — a bead of type "gate" (never ready work on any
// backend) carrying the blocker's id — and the item workflow depends on the
// gate instead of on the blocker. That edge is an ordinary same-store edge, so
// every reader of readiness agrees on it with no change of its own: the store's
// own Ready, every cache over it, gc ready, the hook, the control dispatcher and
// pool demand all see the item blocked while the gate is open. No dependency row
// ever names an id its store does not hold.
//
// The gate is released by ReleaseCrossStoreGates, which the controller runs on
// its tick: a gate closes once its blocker is closed (and not closed blocked,
// beads.DependencySatisfied) in every store that holds it. A blocker no store
// holds keeps its gate open — an unresolvable blocker blocks, as it does on a
// single-store city — and the gate says so in gc.cross_store_gate_state so an
// operator can see why the item waits (and close the gate by hand to override).
// A gate whose drain control has closed is released too, so a drain that
// failed or was canceled leaves no open gate behind.
//
// One gate serves every item of one drain that waits on the same blocker, keyed
// by (owner control, blocker). It carries the control's gc.root_bead_id, which
// makes it graph class (so it is created in, and stays in, the binding) and a
// member of the drain's own workflow, so the workflow's teardown removes it.

// CrossStoreGateLabel marks a gate bead that mirrors a blocker held in another
// store.
const CrossStoreGateLabel = "gc:cross-store-gate"

// crossStoreGateType is the gate bead's type. "gate" is excluded from Ready by
// every backend and by beads.IsReadyExcludedType, so the gate is never offered
// to a worker as work.
const crossStoreGateType = "gate"

// Gate states recorded in gc.cross_store_gate_state.
const (
	crossStoreGateStateUnresolved = "unresolved"
)

// crossStoreGateRoot is the workflow root a drain's gates belong to: the
// control's own root, or the control itself when it names none.
func crossStoreGateRoot(control beads.Bead) string {
	if root := strings.TrimSpace(control.Metadata[beadmeta.RootBeadIDMetadataKey]); root != "" {
		return root
	}
	return control.ID
}

// ensureCrossStoreGate returns the open gate in store that holds control's work
// back until blockerID closes, minting it on first use. It returns "" when a
// gate for this blocker was already released: the blocker closed, so it no
// longer constrains the item.
func ensureCrossStoreGate(store beads.Store, control beads.Bead, blockerID string) (string, error) {
	existing, err := store.List(beads.ListQuery{
		Label: CrossStoreGateLabel,
		Metadata: map[string]string{
			beadmeta.CrossStoreGateOwnerMetadataKey: control.ID,
			beadmeta.CrossStoreBlockerMetadataKey:   blockerID,
		},
		IncludeClosed: true,
		Live:          true,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		return "", fmt.Errorf("looking up the cross-store gate for blocker %s: %w", blockerID, err)
	}
	sort.Slice(existing, func(i, j int) bool { return existing[i].ID < existing[j].ID })
	released := false
	for _, gate := range existing {
		if gate.Status != "closed" {
			return gate.ID, nil
		}
		released = true
	}
	if released {
		return "", nil
	}
	gate, err := store.Create(beads.Bead{
		Title:  fmt.Sprintf("Waits for %s, held in another store", blockerID),
		Type:   crossStoreGateType,
		Labels: []string{CrossStoreGateLabel},
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:          crossStoreGateRoot(control),
			beadmeta.CrossStoreGateOwnerMetadataKey: control.ID,
			beadmeta.CrossStoreBlockerMetadataKey:   blockerID,
		},
	})
	if err != nil {
		return "", fmt.Errorf("creating the cross-store gate for blocker %s: %w", blockerID, err)
	}
	return gate.ID, nil
}

// CrossStoreBlockerResolver returns every copy of id held by a store other
// than the gate's own: none when no store holds it. An error is a read that
// failed, never an absence.
type CrossStoreBlockerResolver func(id string) ([]beads.Bead, error)

// CrossStoreGateReport is one release pass.
type CrossStoreGateReport struct {
	// Open is how many open gates the pass judged.
	Open int
	// Released are the gates the pass closed, by id.
	Released []string
	// Unresolved are the gates whose blocker no store holds, by id.
	Unresolved []string
}

// ReleaseCrossStoreGates closes every open cross-store gate in store whose
// blocker is closed in every store that holds it, and every gate whose drain
// control has closed. resolve looks the blocker up in the other stores.
//
// A pass with no open gate reads nothing but the gate listing, so a city that
// never drains across stores pays one local query per pass. A gate whose read
// fails is left as it is and the error is returned with the others; the next
// pass tries again.
func ReleaseCrossStoreGates(store beads.Store, resolve CrossStoreBlockerResolver) (CrossStoreGateReport, error) {
	var report CrossStoreGateReport
	if store == nil || resolve == nil {
		return report, nil
	}
	gates, err := store.List(beads.ListQuery{
		Label:    CrossStoreGateLabel,
		Status:   "open",
		Live:     true,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return report, fmt.Errorf("listing open cross-store gates: %w", err)
	}
	sort.Slice(gates, func(i, j int) bool { return gates[i].ID < gates[j].ID })
	var errs []error
	for _, gate := range gates {
		if gate.Status == "closed" {
			continue
		}
		report.Open++
		release, reason, unresolved, err := judgeCrossStoreGate(store, gate, resolve)
		if err != nil {
			errs = append(errs, fmt.Errorf("gate %s: %w", gate.ID, err))
			continue
		}
		if unresolved {
			report.Unresolved = append(report.Unresolved, gate.ID)
			if gate.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != crossStoreGateStateUnresolved {
				if err := store.SetMetadata(gate.ID, beadmeta.CrossStoreGateStateMetadataKey, crossStoreGateStateUnresolved); err != nil {
					errs = append(errs, fmt.Errorf("gate %s: recording an unresolved blocker: %w", gate.ID, err))
				}
			}
			continue
		}
		if !release {
			continue
		}
		if err := store.SetMetadataBatch(gate.ID, map[string]string{
			beadmeta.CrossStoreGateStateMetadataKey: "released",
			"close_reason":                          reason,
		}); err != nil {
			errs = append(errs, fmt.Errorf("gate %s: recording its release: %w", gate.ID, err))
			continue
		}
		if err := store.Close(gate.ID); err != nil {
			errs = append(errs, fmt.Errorf("gate %s: closing: %w", gate.ID, err))
			continue
		}
		report.Released = append(report.Released, gate.ID)
	}
	return report, errors.Join(errs...)
}

// judgeCrossStoreGate decides one open gate: release it (and why), leave it
// open, or report its blocker unresolved.
func judgeCrossStoreGate(store beads.Store, gate beads.Bead, resolve CrossStoreBlockerResolver) (release bool, reason string, unresolved bool, err error) {
	if owner := strings.TrimSpace(gate.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey]); owner != "" {
		control, err := store.Get(owner)
		switch {
		case err == nil && convoycore.IsTerminalStatus(control.Status):
			return true, fmt.Sprintf("drain control %s closed", owner), false, nil
		case err != nil && !errors.Is(err, beads.ErrNotFound):
			return false, "", false, fmt.Errorf("reading drain control %s: %w", owner, err)
		}
	}
	blockerID := strings.TrimSpace(gate.Metadata[beadmeta.CrossStoreBlockerMetadataKey])
	if blockerID == "" {
		// A gate that names nothing guards nothing.
		return true, "the gate names no blocker", false, nil
	}
	copies, err := resolve(blockerID)
	if err != nil {
		return false, "", false, fmt.Errorf("resolving blocker %s: %w", blockerID, err)
	}
	if len(copies) == 0 {
		return false, "", true, nil
	}
	for _, b := range copies {
		if !beads.DependencySatisfied(b.Status, beads.ReadinessWorkOutcome(b.Metadata)) {
			return false, "", false, nil
		}
	}
	return true, fmt.Sprintf("blocker %s closed", blockerID), false, nil
}
