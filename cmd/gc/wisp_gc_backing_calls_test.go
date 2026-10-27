package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
)

// mcShapedRigFixture seeds store with the shape of maintainer-city's gascity
// rig the review measured: 26 closed v1 poured roots holding 27 children, 2
// open stepless junk roots, a bd proto template, and a closed root whose
// closure holds a reopened step.
func mcShapedRigFixture(t *testing.T, store beads.Store) {
	t.Helper()
	mk := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := store.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	closeID := func(id string) {
		t.Helper()
		if err := store.Close(id); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 26; i++ {
		root := mk(beads.Bead{Title: fmt.Sprintf("mol-poured-%02d", i), Type: "molecule"})
		children := 1
		if i == 0 {
			children = 2
		}
		for c := 0; c < children; c++ {
			closeID(mk(beads.Bead{Title: "step", Type: "step", ParentID: root.ID}).ID)
		}
		closeID(root.ID)
	}
	for i := 0; i < 2; i++ {
		mk(beads.Bead{Title: fmt.Sprintf("junk-%d", i), Type: "molecule"})
	}
	closeID(mk(beads.Bead{Title: "mol-proto", Type: "molecule", Labels: []string{bdTemplateLabel}}).ID)
	live := mk(beads.Bead{Title: "mol-live-step", Type: "molecule"})
	liveStep := mk(beads.Bead{Title: "reopened step", Type: "step", ParentID: live.ID})
	closeID(liveStep.ID)
	closeID(live.ID)
	inProgress := "in_progress"
	if err := store.Update(liveStep.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}
}

// bdLegs builds n MC-shaped rig legs, each a real BdStore (driven by a fake
// bd that counts its invocations — each one a subprocess in production)
// behind the production CachingStore, and the split env's WorkLegs over them.
func bdLegs(t *testing.T, n int) (splitEnv, WorkLegs, *atomic.Int64) {
	t.Helper()
	e := newSplitEnvWithRig(t, true)
	var calls atomic.Int64
	rigs := map[string]beads.Store{}
	e.cfg.Rigs = nil
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("rig-%c", 'A'+i)
		e.cfg.Rigs = append(e.cfg.Rigs, config.Rig{Name: name, Path: filepath.Join(e.cityPath, "rigs", name)})
		fb := beadstest.NewFakeBd()
		bd := beads.NewBdStore("/fake-bd", func(dir, name string, args ...string) ([]byte, error) {
			calls.Add(1)
			return fb.Run(dir, name, args...)
		})
		mcShapedRigFixture(t, bd)
		cache := beads.NewCachingStore(bd, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		rigs[name] = cache
	}
	legs := workLegsFromCensus(e.cityPath, e.cfg, cityWorkLegOf(beads.WorkStore{Store: e.work}), rigs)
	return e, legs, &calls
}

// Must-fix: count the bd invocations (subprocesses on maintainer-city, ~0.75
// s each) one work-arm pass makes, with 1 and 7 MC-shaped legs behind the
// production CachingStore. A dry pass only lists; an enforced pass spends one
// budget shared across every leg.
func TestWispGCWorkArmBackingCallsPerPass(t *testing.T) {
	prevOut := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(prevOut) })
	for _, n := range []int{1, 7} {
		t.Run(fmt.Sprintf("%d-legs", n), func(t *testing.T) {
			e, legs, calls := bdLegs(t, n)
			gc := newWorkMoleculeGC()
			pass := func(now time.Time) int64 {
				calls.Store(0)
				withCloseAbandonedEnforced(t, func() {
					withCloseAbandonedTTL(t, 5*time.Minute, func() {
						if _, err := gc.pass(legs, e.graphStore(), time.Hour, now); err != nil {
							t.Logf("pass: %v", err)
						}
					})
				})
				return calls.Load()
			}
			now := time.Now().Add(48 * time.Hour)
			dry := []int64{pass(now), pass(now)}
			var enforced []int64
			withWorkMoleculesEnforced(t, func() {
				for i := 0; i < 8; i++ {
					enforced = append(enforced, pass(now))
				}
			})
			t.Logf("%d leg(s): dry-run bd calls per pass %v; enforced %v (budget %d walks / %s per pass)", n, dry, enforced, wispGCWorkArmWalkBudget, wispGCWorkArmWallBudget)
			for _, c := range dry {
				if c > int64(4*n) {
					t.Errorf("a dry-run pass made %d bd calls over %d leg(s); it must only list", c, n)
				}
			}
			// One budget per pass across all legs: the enforced cost must not
			// grow with the leg count beyond the listing.
			for _, c := range enforced {
				if c > int64(4*n)+60 {
					t.Errorf("an enforced pass made %d bd calls over %d leg(s): the walk budget is not shared", c, n)
				}
			}
		})
	}
}
