package dispatch

import (
	"crypto/sha256"
	"encoding/hex"
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
// drain used to drop the edge and let the item run at once — ahead of the work
// its member was waiting for (read-audit R3). On a single-store city the edge
// exists and the item waits.
//
// # The design: a local gate that mirrors the remote blocker
//
// For each open blocker the item's store cannot hold, the drain mints a GATE in
// the item's own store — a bead of type "gate" (never ready work on any
// backend) carrying the blocker's id — and the item workflow depends on the
// gate instead of on the blocker. That edge is an ordinary same-store edge, so
// every reader of readiness agrees on it with no change of its own. No
// dependency row ever names an id its store does not hold.
//
// Gates exist only on a split city, and only in the store the drain's item
// workflow lives in — the class binding (a SQLite or workspace engine). A drain
// on a city with no binding names no member stores, never classifies a blocker
// as unprojectable, and so never mints one. A gate must never land in a bd
// work store: bd would read the gate's own edges by its prefix rules, not ours.
//
// # Release
//
// ReleaseCrossStoreGates, run by the controller OFF its tick: a gate closes
// once its blocker is closed (and not closed blocked, beads.DependencySatisfied)
// in every store that holds it, and once its drain control is closed or gone,
// so a drain that failed or was canceled leaves no open gate behind. A blocker
// no store holds keeps its gate open — an unresolvable blocker blocks, as it
// does on a single-store city — and the gate records it in
// gc.cross_store_gate_state=unresolved, which the caller reports (an event and
// a doctor finding) and an operator can override by closing the gate. A blocker
// a store that could not be read MAY hold keeps its gate exactly as it is.
//
// # Identity
//
// One gate serves every item of one drain that waits on the same blocker. Its
// id is derived from (control, blocker) under the store's own mint prefix, so
// two processes that race to mint it collide on the store's duplicate-id check
// and both end up with the one gate. A store that does not honor a pinned id
// mints its own, and the gate is then found by its metadata.

// CrossStoreGateLabel marks a gate bead that mirrors a blocker held in another
// store.
const CrossStoreGateLabel = "gc:cross-store-gate"

// crossStoreGateType is the gate bead's type. "gate" is excluded from Ready by
// every backend and by beads.IsReadyExcludedType, so the gate is never offered
// to a worker as work.
const crossStoreGateType = "gate"

// Gate states recorded in gc.cross_store_gate_state.
const (
	// CrossStoreGateStateUnresolved: no store holds the gate's blocker.
	CrossStoreGateStateUnresolved = "unresolved"
	// CrossStoreGateStateReleased: the gate was released (its close_reason
	// says why).
	CrossStoreGateStateReleased = "released"
)

// crossStoreGateRoot is the workflow root a drain's gates belong to: the
// control's own root, or the control itself when it names none.
func crossStoreGateRoot(control beads.Bead) string {
	if root := strings.TrimSpace(control.Metadata[beadmeta.RootBeadIDMetadataKey]); root != "" {
		return root
	}
	return control.ID
}

// crossStoreGateID is the deterministic id of control's gate for blockerID in
// store, or "" when store declares no mint prefix to pin it under.
func crossStoreGateID(store beads.Store, control beads.Bead, blockerID string) string {
	prefixed, ok := store.(interface{ IDPrefix() string })
	if !ok {
		return ""
	}
	prefix := strings.Trim(strings.TrimSpace(prefixed.IDPrefix()), "-")
	if prefix == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(control.ID + "\x00" + blockerID))
	return prefix + "-xsg-" + hex.EncodeToString(sum[:6])
}

// gateAnswer reads an existing gate: its id when open, "" when it was released.
func gateAnswer(gate beads.Bead) string {
	if gate.Status == "closed" {
		return ""
	}
	return gate.ID
}

// ensureCrossStoreGate returns the open gate in store that holds control's work
// back until blockerID closes, minting it on first use. It returns "" when the
// gate was already released: the blocker closed, so it no longer constrains
// the item, and a released gate is never reopened or replaced.
func ensureCrossStoreGate(store beads.Store, control beads.Bead, blockerID string) (string, error) {
	pinned := crossStoreGateID(store, control, blockerID)
	if pinned != "" {
		if gate, err := beads.HandlesFor(store).Live.Get(pinned); err == nil {
			return gateAnswer(gate), nil
		} else if !errors.Is(err, beads.ErrNotFound) {
			return "", fmt.Errorf("reading cross-store gate %s: %w", pinned, err)
		}
	}
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
	if len(existing) > 0 {
		sort.Slice(existing, func(i, j int) bool { return existing[i].ID < existing[j].ID })
		for _, gate := range existing {
			if gate.Status != "closed" {
				return gate.ID, nil
			}
		}
		return "", nil
	}
	gate, err := store.Create(beads.Bead{
		ID:     pinned,
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
		if pinned != "" {
			// A racing minter won the pinned id: use its gate.
			if raced, getErr := beads.HandlesFor(store).Live.Get(pinned); getErr == nil {
				return gateAnswer(raced), nil
			}
		}
		return "", fmt.Errorf("creating the cross-store gate for blocker %s: %w", blockerID, err)
	}
	return gate.ID, nil
}

// OpenCrossStoreGatesNaming returns the open cross-store gates in store that
// wait on the bead blockerID. A caller about to delete a work bead asks it
// first: deleting a bead an open gate names makes that gate's blocker
// unresolvable, which holds its drain items forever (the delete guard's half
// of the contract; the guard lives with the delete, not here).
func OpenCrossStoreGatesNaming(store beads.Store, blockerID string) ([]beads.Bead, error) {
	if store == nil || strings.TrimSpace(blockerID) == "" {
		return nil, nil
	}
	return store.List(beads.ListQuery{
		Label:    CrossStoreGateLabel,
		Status:   "open",
		Metadata: map[string]string{beadmeta.CrossStoreBlockerMetadataKey: strings.TrimSpace(blockerID)},
		Live:     true,
		TierMode: beads.TierBoth,
	})
}

// CrossStoreBlockerResolver reads every blocker in ids from the stores other
// than the gates' own, in ONE batched read per store: copies holds every copy
// found, by id. undecided names the ids a store the resolver could not read,
// or chose not to (a suspended or quiescent one), MAY hold: their gates are
// left exactly as they are. An error fails the whole pass.
type CrossStoreBlockerResolver func(ids []string) (copies map[string][]beads.Bead, undecided map[string]bool, err error)

// CrossStoreGateReport is one release pass.
type CrossStoreGateReport struct {
	// Open is how many open gates the pass judged.
	Open int
	// Released are the gates the pass closed, by id.
	Released []string
	// Unresolved are the open gates whose blocker no store holds, by id.
	Unresolved []string
	// NewlyUnresolved are the gates this pass marked unresolved (a subset of
	// Unresolved): the transition a caller reports once.
	NewlyUnresolved []beads.Bead
}

// ReleaseCrossStoreGates closes every open cross-store gate in store whose
// blocker is satisfied in every store that holds it, and every gate whose
// drain control is closed or gone. The blockers of all open gates are read in
// one resolver call, deduplicated by blocker.
//
// A pass with no open gate reads nothing but the gate listing. A write that
// fails leaves its gate as it is and is returned with the others; the next
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
	var open []beads.Bead
	blockerSet := make(map[string]bool)
	for _, gate := range gates {
		if gate.Status == "closed" {
			continue
		}
		open = append(open, gate)
		if id := gateBlocker(gate); id != "" {
			blockerSet[id] = true
		}
	}
	report.Open = len(open)
	if len(open) == 0 {
		return report, nil
	}
	controls, err := gateControls(store, open)
	if err != nil {
		return report, err
	}
	blockers := make([]string, 0, len(blockerSet))
	for id := range blockerSet {
		blockers = append(blockers, id)
	}
	sort.Strings(blockers)
	copies, undecided, err := resolve(blockers)
	if err != nil {
		return report, fmt.Errorf("resolving %d cross-store blocker(s): %w", len(blockers), err)
	}
	var errs []error
	for _, gate := range open {
		release, reason, unresolved := judgeCrossStoreGate(gate, controls, copies, undecided)
		switch {
		case unresolved:
			report.Unresolved = append(report.Unresolved, gate.ID)
			if gate.Metadata[beadmeta.CrossStoreGateStateMetadataKey] == CrossStoreGateStateUnresolved {
				continue
			}
			if err := store.SetMetadata(gate.ID, beadmeta.CrossStoreGateStateMetadataKey, CrossStoreGateStateUnresolved); err != nil {
				errs = append(errs, fmt.Errorf("gate %s: recording an unresolved blocker: %w", gate.ID, err))
				continue
			}
			report.NewlyUnresolved = append(report.NewlyUnresolved, gate)
		case release:
			if err := store.SetMetadataBatch(gate.ID, map[string]string{
				beadmeta.CrossStoreGateStateMetadataKey: CrossStoreGateStateReleased,
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
	}
	return report, errors.Join(errs...)
}

// gateBlocker is the bead a gate waits on, external: marker stripped; "" for a
// gate that names no bead.
func gateBlocker(gate beads.Bead) string {
	target, capability := beads.ExternalBlockerTarget(gate.Metadata[beadmeta.CrossStoreBlockerMetadataKey])
	if capability {
		return ""
	}
	return target
}

// gateControlState is what a pass knows about a gate's drain control.
type gateControlState int

const (
	gateControlLive gateControlState = iota
	gateControlDone
)

// gateControls reads the drain control of every gate, one read per distinct
// control in the gates' own store (the binding, which is local).
func gateControls(store beads.Store, gates []beads.Bead) (map[string]gateControlState, error) {
	out := make(map[string]gateControlState)
	var ids []string
	for _, gate := range gates {
		owner := strings.TrimSpace(gate.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey])
		if owner == "" {
			continue
		}
		if _, seen := out[owner]; seen {
			continue
		}
		out[owner] = gateControlDone
		ids = append(ids, owner)
	}
	found, err := beads.GetMany(store, ids)
	if err != nil {
		return nil, fmt.Errorf("reading the drain controls of cross-store gates: %w", err)
	}
	for _, id := range ids {
		if control, ok := found[id]; ok && !convoycore.IsTerminalStatus(control.Status) {
			out[id] = gateControlLive
		}
	}
	return out, nil
}

// judgeCrossStoreGate decides one open gate: release it (and why), report its
// blocker unresolved, or leave it as it is.
func judgeCrossStoreGate(gate beads.Bead, controls map[string]gateControlState, copies map[string][]beads.Bead, undecided map[string]bool) (release bool, reason string, unresolved bool) {
	if owner := strings.TrimSpace(gate.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey]); owner != "" {
		if controls[owner] == gateControlDone {
			return true, fmt.Sprintf("drain control %s is closed or gone", owner), false
		}
	}
	blockerID := gateBlocker(gate)
	if blockerID == "" {
		// A gate that names no bead guards nothing.
		return true, "the gate names no blocker", false
	}
	if undecided[blockerID] {
		return false, "", false
	}
	held := copies[blockerID]
	if len(held) == 0 {
		return false, "", true
	}
	for _, b := range held {
		if !beads.DependencySatisfied(b.Status, beads.ReadinessWorkOutcome(b.Metadata)) {
			return false, "", false
		}
	}
	return true, fmt.Sprintf("blocker %s closed", blockerID), false
}
