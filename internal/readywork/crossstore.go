package readywork

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fanout"
)

// Cross-store blockers: one rule, whatever each backend does with them.
//
// # The rule
//
// A blocking edge (blocks, waits-for, conditional-blocks) may name a blocker
// that the dependent's own store does not hold. On a split city that is a
// blocker in the other store — a work bead held up by a graph step, or the
// reverse — and the owner rule for it is: it blocks until it is closed, on
// every store that holds it. A blocker no store holds at all is unresolvable,
// and an unresolvable blocker blocks too; it never fails the read.
//
// # Why the reader enforces it rather than trusting the stores
//
// The backends answer the same edge in opposite ways. SQLite and the in-memory
// stores read the absent target as a status that is never "closed", so the
// dependent is stranded forever: closing the blocker in its own store cannot
// release it. bd/Dolt (beads v1.3.2, issueops/dependencies.go) classifies a
// cross-prefix target as external and never marks the dependent blocked, so it
// is served at once. Neither is the rule, and which one a city gets depends on
// which backend holds the dependent. So the reader re-judges every blocking
// edge of every row it is about to serve, through the legs it federates:
//
//   - The premature half: a row in a store's ready set whose blocker is held
//     open elsewhere, or held nowhere, is withheld (gateReadyRows).
//   - The stranded half: a row a store withholds only because a blocker is
//     absent from it is listed through beads.AbsentBlockedReader and offered
//     to the same gate, which serves it once every blocker is closed wherever
//     it is held (readAbsentBlocked).
//   - The in-progress tier reports each blocker with the status of the store
//     that actually holds it, and an unresolvable one as BlockerUnresolved
//     with a reason, so the resumed holder sees its bead as gated
//     (readLegBlockers).
//
// A one-leg federation has no other store, so none of this runs there: a
// single-store city keeps its store's own answer, byte for byte.

// BlockerUnresolved is the blocked_by status of a blocker no federated store
// holds. It is not "closed", so every consumer that reads blocked_by treats the
// row as gated (cmd/gc isDepBlockedHookCandidate), which is the owner rule.
const BlockerUnresolved = "unresolved"

// blockerVerdict is what the federation knows about one blocker id, judged
// from the row's own leg first and the other legs only when that leg does not
// hold it.
type blockerVerdict struct {
	// status is the blocker's status as the holding store reports it, or
	// BlockerUnresolved when no store holds it.
	status string
	// satisfied reports whether the blocker releases its dependents: closed,
	// and not closed blocked (beads.DependencySatisfied), in every store that
	// holds it.
	satisfied bool
	// reason says where the verdict came from when it did not come from the
	// row's own leg. Empty for a blocker the row's own leg holds, which is the
	// verdict the store itself applied.
	reason string
	// err is a store read that failed. A failed read is not an absence: it
	// fails the answer, as every leg failure does (fail loud).
	err error
}

// verdictOf judges one held copy of a blocker.
func verdictOf(b beads.Bead) blockerVerdict {
	return blockerVerdict{
		status:    b.Status,
		satisfied: beads.DependencySatisfied(b.Status, beads.ReadinessWorkOutcome(b.Metadata)),
	}
}

// resolveBlockers judges every id in targets for a row served by legs[own]:
// through that leg first, and, for an id it does not hold, through every other
// leg. The reads run concurrently, at most LegFanOut at a time, and an id whose
// turn comes after ctx has ended carries ctx's error.
func resolveBlockers(ctx context.Context, legs []Leg, own int, targets []string) []blockerVerdict {
	out := make([]blockerVerdict, len(targets))
	live := beads.HandlesFor(legs[own].Store).Live
	var absent []int
	fanout.Bounded(len(targets), LegFanOut, func(i int) {
		if err := ctx.Err(); err != nil {
			out[i] = blockerVerdict{err: err}
			return
		}
		b, err := live.Get(targets[i])
		switch {
		case err == nil:
			out[i] = verdictOf(b)
		case errors.Is(err, beads.ErrNotFound):
			out[i] = blockerVerdict{status: BlockerUnresolved}
		default:
			out[i] = blockerVerdict{err: fmt.Errorf("%s store: %w", legs[own].Label, err)}
		}
	})
	for i := range targets {
		if out[i].err == nil && out[i].status == BlockerUnresolved {
			absent = append(absent, i)
		}
	}
	if len(absent) == 0 || len(legs) < 2 {
		for _, i := range absent {
			out[i].reason = fmt.Sprintf("blocker %s is held by no store this city federates", targets[i])
		}
		return out
	}
	fanout.Bounded(len(absent), LegFanOut, func(j int) {
		i := absent[j]
		out[i] = resolveElsewhere(ctx, legs, own, targets[i])
	})
	return out
}

// resolveElsewhere judges a blocker the row's own leg does not hold, across the
// other legs in leg order. Every copy must release its dependents: a blocker
// two stores hold (a migrated relic and its uncleared original) blocks until
// both are closed.
func resolveElsewhere(ctx context.Context, legs []Leg, own int, id string) blockerVerdict {
	var holders []string
	verdict := blockerVerdict{status: "closed", satisfied: true}
	for i, leg := range legs {
		if i == own || leg.Store == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return blockerVerdict{err: err}
		}
		b, err := beads.HandlesFor(leg.Store).Live.Get(id)
		if errors.Is(err, beads.ErrNotFound) {
			continue
		}
		if err != nil {
			return blockerVerdict{err: fmt.Errorf("%s store: %w", leg.Label, err)}
		}
		holders = append(holders, leg.Label)
		held := verdictOf(b)
		if !held.satisfied && verdict.satisfied {
			verdict = held
			verdict.reason = fmt.Sprintf("blocker %s is held open in the %s store", id, leg.Label)
		}
	}
	if len(holders) == 0 {
		return blockerVerdict{
			status: BlockerUnresolved,
			reason: fmt.Sprintf("blocker %s is held by no store this city federates", id),
		}
	}
	if verdict.satisfied {
		verdict.reason = fmt.Sprintf("blocker %s is closed in the %s store", id, strings.Join(holders, ", "))
	}
	return verdict
}

// readAbsentBlocked lists, from every leg that can say, the rows its own Ready
// withholds only because a blocking edge names an id it does not hold, and
// merges them into the candidate set as rows that leg served. They are
// candidates, not answers: gateReadyRows serves one only once every blocker it
// has is closed wherever it is held.
//
// It reads nothing on a one-leg federation, where an absent blocker has no
// other store to be held in and the store's own verdict stands.
func readAbsentBlocked(ctx context.Context, legs []Leg, q beads.ReadyQuery, items []beads.Bead, owners map[string]int) ([]beads.Bead, error) {
	if len(legs) < 2 {
		return items, nil
	}
	results := readLegs(ctx, legs, func(_ context.Context, leg Leg) legResult {
		reader, ok := beads.ReadyBlockedByAbsentFor(leg.Store)
		if !ok {
			return legResult{}
		}
		rows, err := reader.ReadyBlockedByAbsent(q)
		return legResult{rows: rows, err: err}
	})
	if err := firstLegError(legs, results); err != nil {
		return nil, err
	}
	for i, res := range results {
		for _, b := range res.rows {
			if _, seen := owners[b.ID]; seen {
				continue
			}
			owners[b.ID] = i
			items = append(items, b)
		}
	}
	return items, nil
}

// gateReadyRows returns the rows of items whose every blocking edge is
// satisfied under the cross-store rule, in their order, and how many it
// withheld.
//
// Each row's edges are read from the leg that served it; each blocker is judged
// there first and in the other legs only when that leg does not hold it
// (resolveBlockers). A failed read fails the answer: serving a row whose
// blocker could not be read is the premature dispatch this gate exists to stop.
func gateReadyRows(ctx context.Context, legs []Leg, owners map[string]int, items []beads.Bead) ([]beads.Bead, int, error) {
	if len(legs) < 2 || len(items) == 0 {
		return items, 0, nil
	}
	byLeg := make(map[int][]string)
	var legOrder []int
	for _, item := range items {
		idx, ok := owners[item.ID]
		if !ok || legs[idx].Store == nil {
			continue
		}
		if _, seen := byLeg[idx]; !seen {
			legOrder = append(legOrder, idx)
		}
		byLeg[idx] = append(byLeg[idx], item.ID)
	}
	blocked := make(map[string]bool)
	errs := make([]error, len(legOrder))
	blockedPerLeg := make([][]string, len(legOrder))
	fanout.Bounded(len(legOrder), LegFanOut, func(i int) {
		blockedPerLeg[i], errs[i] = blockedRowsOnLeg(ctx, legs, legOrder[i], byLeg[legOrder[i]])
	})
	for i, err := range errs {
		if err != nil {
			return nil, 0, err
		}
		for _, id := range blockedPerLeg[i] {
			blocked[id] = true
		}
	}
	if len(blocked) == 0 {
		return items, 0, nil
	}
	kept := make([]beads.Bead, 0, len(items)-len(blocked))
	for _, item := range items {
		if blocked[item.ID] {
			continue
		}
		kept = append(kept, item)
	}
	return kept, len(items) - len(kept), nil
}

// blockedRowsOnLeg returns the ids, all served by legs[own], that a blocking
// edge holds back under the cross-store rule.
func blockedRowsOnLeg(ctx context.Context, legs []Leg, own int, ids []string) ([]string, error) {
	leg := legs[own]
	deps, err := legDeps(ctx, leg, ids)
	if err != nil {
		return nil, err
	}
	var targets []string
	index := make(map[string]int)
	for _, id := range ids {
		for _, dep := range deps[id] {
			if !beads.IsReadyBlockingDependencyType(dep.Type) || dep.DependsOnID == id {
				continue
			}
			if _, seen := index[dep.DependsOnID]; seen {
				continue
			}
			index[dep.DependsOnID] = len(targets)
			targets = append(targets, dep.DependsOnID)
		}
	}
	if len(targets) == 0 {
		return nil, nil
	}
	verdicts := resolveBlockers(ctx, legs, own, targets)
	var blocked []string
	for _, id := range ids {
		for _, dep := range deps[id] {
			if !beads.IsReadyBlockingDependencyType(dep.Type) || dep.DependsOnID == id {
				continue
			}
			v := verdicts[index[dep.DependsOnID]]
			if v.err != nil {
				return nil, fmt.Errorf("resolving blocker %q of %q: %w", dep.DependsOnID, id, v.err)
			}
			if !v.satisfied {
				blocked = append(blocked, id)
				break
			}
		}
	}
	return blocked, nil
}

// legDeps reads the down edges of ids from leg: one batched read where the
// store has one, otherwise one live DepList per row, at most LegFanOut at once.
// Edges are written once, at creation or by an explicit dep add, so a batch a
// cache answers from memory is as good as a live one; the blocker STATUSES the
// verdict turns on are always read live (resolveBlockers).
func legDeps(ctx context.Context, leg Leg, ids []string) (map[string][]beads.Dep, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if batch, ok := beads.DepListBatchFor(leg.Store); ok {
		deps, err := batch.DepListBatch(ids)
		if err == nil {
			return deps, nil
		}
		if !errors.Is(err, beads.ErrDepListBatchUnsupported) {
			return nil, fmt.Errorf("%s store: listing dependencies: %w", leg.Label, err)
		}
	}
	live := beads.HandlesFor(leg.Store).Live
	lists := make([][]beads.Dep, len(ids))
	errs := make([]error, len(ids))
	fanout.Bounded(len(ids), LegFanOut, func(i int) {
		if errs[i] = ctx.Err(); errs[i] != nil {
			return
		}
		lists[i], errs[i] = live.DepList(ids[i], "down")
	})
	out := make(map[string][]beads.Dep, len(ids))
	for i, id := range ids {
		if errs[i] != nil {
			return nil, fmt.Errorf("%s store: listing dependencies of %q: %w", leg.Label, id, errs[i])
		}
		out[id] = lists[i]
	}
	return out, nil
}

// gateTopN applies gateReadyRows to the ordered answer until it holds limit
// rows or runs out, reading edges only for the rows it needs: the crash- and
// claim-tier queries ask for one row, and gating the whole ready set to serve
// one would pay for every row the bound then discards.
func gateTopN(ctx context.Context, legs []Leg, owners map[string]int, items []beads.Bead, limit int) ([]beads.Bead, int, error) {
	if limit <= 0 {
		return gateReadyRows(ctx, legs, owners, items)
	}
	var kept []beads.Bead
	withheld := 0
	chunk := limit + boundedSlack
	for start := 0; start < len(items) && len(kept) < limit; start += chunk {
		end := min(start+chunk, len(items))
		rows, dropped, err := gateReadyRows(ctx, legs, owners, items[start:end])
		if err != nil {
			return nil, 0, err
		}
		withheld += dropped
		kept = append(kept, rows...)
	}
	if len(kept) > limit {
		kept = kept[:limit]
	}
	return slices.Clip(kept), withheld, nil
}
