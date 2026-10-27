package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
)

// The wisp GC's WORK arm, on its own lane.
//
// On a city that relocates the graph class, a v1 POURED molecule is work class
// and lives on the work ledgers (the city work store and every rig store), not
// in the binding the graph arm sweeps. This arm closes abandoned ones and
// purges expired closed ones there.
//
// Every read here lands on a work ledger, which on maintainer-city is bd: one
// subprocess per read, about 0.75 s each. So the arm runs off the controller
// tick, on a paced background lane the tick only wakes, and each pass spends
// one budget shared across all legs (wispGCWorkArmWalkBudget walks and
// wispGCWorkArmWallBudget of wall clock), resuming where the last pass stopped
// (a rotating leg and root cursor). Outcomes that leave a root alone (a live
// subtree, a held stepless root, a refused or failed purge) are remembered by
// root id and updated_at, so a backlog of busy roots cannot take every walk of
// every pass.

// wispGCWorkArmWalkBudget is how many roots one work-arm pass walks, across
// every leg. Package var so tests can change it.
var wispGCWorkArmWalkBudget = 5

// wispGCWorkArmWallBudget is the wall clock one work-arm pass may spend
// walking, across every leg. Package var so tests can change it.
var wispGCWorkArmWallBudget = 5 * time.Second

// workMoleculeGC is the work arm's state, owned by its lane: the memo of
// outcomes and log lines, and the rotation cursors.
type workMoleculeGC struct {
	mu    sync.Mutex
	notes wispGCNotes
	// legCursor is the leg position the next pass starts at.
	legCursor int
	// rootCursor is, per leg position and list, the last root id walked.
	rootCursor map[string]string
}

func newWorkMoleculeGC() *workMoleculeGC {
	return &workMoleculeGC{rootCursor: make(map[string]string)}
}

const (
	workArmCloseMemo = "work-close"
	workArmPurgeMemo = "work-purge"
)

// pass runs one work-arm pass over legs (every leg but the graph store),
// purging closed roots past ttl. It returns how many closures it purged.
//
// Dry-run (GC_WISP_GC_WORK_MOLECULES unset) only LISTS and counts: no subtree
// or closure is walked, and one summary line is logged.
func (w *workMoleculeGC) pass(legs wispGCLegs, graph beads.Store, ttl time.Duration, now time.Time) (int, error) {
	if legs == nil || legs.unusable() != nil || ttl <= 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.rootCursor == nil {
		w.rootCursor = make(map[string]string)
	}
	var stores []beads.Store
	legs.each(func(work beads.Store) {
		if work != nil && !sameBeadStore(work, graph) {
			stores = append(stores, work)
		}
	})
	if len(stores) == 0 {
		return 0, nil
	}
	enforce := workMoleculesEnforced()
	cutoff := now.Add(-ttl)
	budget := &wispGCBudget{walks: wispGCWorkArmWalkBudget, deadline: time.Now().Add(wispGCWorkArmWallBudget)}

	purged, idle, expired, walked := 0, 0, 0, 0
	presentOpen, presentClosed := map[string]bool{}, map[string]bool{}
	var errs error
	start := w.legCursor % len(stores)
	next := start
	for i := 0; i < len(stores); i++ {
		pos := (start + i) % len(stores)
		work := stores[pos]
		open, err := enumerateWorkClassMoleculeRoots(work, "open", "in_progress")
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("listing open work-class molecule roots: %w", err))
		}
		closed, cerr := enumerateWorkClassMoleculeRoots(work, "closed")
		if cerr != nil {
			errs = errors.Join(errs, fmt.Errorf("listing closed work-class molecule roots: %w", cerr))
		}
		for _, r := range open {
			presentOpen[r.ID] = true
		}
		for _, r := range closed {
			presentClosed[r.ID] = true
		}
		if !enforce {
			idle += countIdleAbandonedCandidates(open, now)
			for _, root := range closed {
				if workArmPurgeAge(root).Before(cutoff) {
					expired++
				}
			}
			continue
		}
		if budget.exhausted() {
			continue
		}
		closeKey, purgeKey := fmt.Sprintf("%d/close", pos), fmt.Sprintf("%d/purge", pos)
		if err == nil {
			holders := wispGCHolders{graph: graph, own: work}
			closeErr := closeAbandonedRootCandidatesMode(work, rotateAfter(open, w.rootCursor[closeKey]), holders, now, abandonedCloseMode{
				enforce:    closeAbandonedEnforced(),
				dryRunHint: fmt.Sprintf("set %s=1 to enforce", closeAbandonedEnv),
				label:      "work-class molecule root",
				budget:     budget,
				notes:      &w.notes,
				memo:       &w.notes,
				memoArm:    workArmCloseMemo,
				onWalk:     func(id string) { w.rootCursor[closeKey] = id; walked++ },
			})
			errs = errors.Join(errs, closeErr)
		}
		if cerr == nil {
			n, perr := purgeExpiredBeadClosuresWith(work, rotateAfter(closed, w.rootCursor[purgeKey]), closurePurgeOpts{
				cutoff:       cutoff,
				batchCap:     wispGCClosurePurgeBatchCap,
				budget:       budget,
				age:          workArmPurgeAge,
				arm:          workArmPurgeMemo,
				notes:        &w.notes,
				now:          now,
				memoFailures: true,
				prefetch:     true,
				onWalk:       func(id string) { w.rootCursor[purgeKey] = id; walked++ },
			})
			purged += n
			errs = errors.Join(errs, perr)
		}
		if budget.exhausted() {
			// Resume on this leg next pass, where its cursors stopped.
			next = pos
		} else {
			next = (pos + 1) % len(stores)
		}
	}
	w.legCursor = next
	w.notes.pruneRefusals(workArmCloseMemo, presentOpen)
	w.notes.pruneRefusals(workArmPurgeMemo, presentClosed)
	w.notes.endPass()
	if !enforce && (idle > 0 || expired > 0) {
		log.Printf("wisp gc: work arm (dry-run; set %s=1 to enforce): %d open work-class molecule root(s) idle past %s, %d closed root(s) past retention; subtrees and closures not inspected",
			workMoleculesEnv, idle, wispGCCloseAbandonedTTL, expired)
	}
	if enforce && (walked > 0 || purged > 0) {
		log.Printf("wisp gc: work arm walked %d root(s) across %d leg(s), purged %d closure(s)", walked, len(stores), purged)
	}
	return purged, errs
}

// rotateAfter returns roots ordered by id, starting after cursor and wrapping
// around, so a pass that stops early resumes where the last one stopped.
func rotateAfter(roots []beads.Bead, cursor string) []beads.Bead {
	out := append([]beads.Bead(nil), roots...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if cursor == "" {
		return out
	}
	i := sort.Search(len(out), func(i int) bool { return out[i].ID > cursor })
	return append(out[i:], out[:i]...)
}

// workArmPurgeAge is the clock a closed work-class molecule root ages on: its
// last update, which its close stamps, not its creation. A long-lived v1
// molecule closed a minute ago is not past retention because it was poured
// long ago.
func workArmPurgeAge(root beads.Bead) time.Time {
	return beadLastActivity(root)
}

// countIdleAbandonedCandidates counts, from a listing alone, the roots the
// abandoned-root closer would go on to inspect: in scope, not exempt, not
// terminal, idle past the TTL. It reads nothing.
func countIdleAbandonedCandidates(roots []beads.Bead, now time.Time) int {
	cutoff := now.Add(-wispGCCloseAbandonedTTL)
	n := 0
	for _, root := range roots {
		if isAbandonedRootCandidate(root) && !convoycore.IsTerminalStatus(root.Status) && !isGCExempt(root) && beadLastActivity(root).Before(cutoff) {
			n++
		}
	}
	return n
}

// workGCLane runs the work arm off the controller tick.
type workGCLane struct {
	wakeCh chan struct{}
	gc     *workMoleculeGC

	statusMu   sync.Mutex
	lastPassAt time.Time
}

func newWorkGCLane() *workGCLane {
	return &workGCLane{wakeCh: make(chan struct{}, 1), gc: newWorkMoleculeGC()}
}

// workGCLaneOf returns this runtime's lane, creating it on first use.
func (cr *CityRuntime) workGCLaneOf() *workGCLane {
	cr.workGCLaneOnce.Do(func() { cr.workGCLane = newWorkGCLane() })
	return cr.workGCLane
}

// wake asks the lane for a pass. Non-blocking.
func (l *workGCLane) wake() {
	select {
	case l.wakeCh <- struct{}{}:
	default:
	}
}

// lastPassAge is how long ago the last pass finished, and whether one has.
func (l *workGCLane) lastPassAge(now time.Time) (time.Duration, bool) {
	l.statusMu.Lock()
	defer l.statusMu.Unlock()
	if l.lastPassAt.IsZero() {
		return 0, false
	}
	return now.Sub(l.lastPassAt), true
}

// startWorkGCLane starts the lane goroutine and returns a channel closed when it
// exits. The tick wakes it whenever the wisp GC runs; a backstop runs a pass
// one wisp-GC interval after the last one otherwise.
func (cr *CityRuntime) startWorkGCLane(ctx context.Context) <-chan struct{} {
	lane := cr.workGCLaneOf()
	interval := 10 * time.Minute
	if cfg := cr.serviceConfigSnapshot(); cfg != nil {
		if d := cfg.Daemon.WispGCIntervalDuration(); d > 0 {
			interval = d
		}
	}
	return startPacedLane(ctx, interval, 0, lane.wakeCh, func(bool) {
		cr.safeTick(func() { cr.runWorkGCLanePass(ctx, time.Now()) }, "work-gc-lane")
	})
}

// runWorkGCLanePass is one lane pass. It reads the published config snapshot,
// not cr.cfg, because it runs on its own goroutine.
func (cr *CityRuntime) runWorkGCLanePass(ctx context.Context, now time.Time) {
	if ctx.Err() != nil || cr.beadsQuiescent.Load() {
		return
	}
	cfg := cr.serviceConfigSnapshot()
	if cfg == nil {
		return
	}
	ttl := cfg.Daemon.WispTTLDuration()
	if ttl <= 0 || cfg.Daemon.WispGCIntervalDuration() <= 0 {
		return
	}
	if _, relocated := graphClassBinding(cr.storageRoutes); !relocated { // residency:allow — asks only whether the graph class is relocated; the legs come from the census plan (workLegsFromCensus)
		return
	}
	lane := cr.workGCLaneOf()
	graph := resolveGraphStore(cr.storageRoutes, cr.cityBeadStore(), cfg, cr.cityPath, cr.rec)
	legs := workLegsFromCensus(cr.cityPath, cfg, cityWorkLegOf(cr.cityWorkStore()), cr.rigBeadStores()) // residency:allow — the census frame; workLegsFromCensus plans the legs (storeref.Plan)
	purged, err := lane.gc.pass(legs, graph, ttl, now)
	if err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			if line != "" {
				fmt.Fprintf(cr.stderr, "%s: wisp gc (work arm): %s\n", cr.logPrefix, line) //nolint:errcheck // best-effort stderr
			}
		}
	}
	if purged > 0 {
		fmt.Fprintf(cr.stdout, "Bead GC (work arm): purged %d expired molecule closure(s)\n", purged) //nolint:errcheck // best-effort stdout
	}
	lane.statusMu.Lock()
	lane.lastPassAt = time.Now()
	lane.statusMu.Unlock()
}
