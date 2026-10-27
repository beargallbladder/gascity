package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
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

// Must-fix 1: count the backing calls one wisp GC pass makes against a rig
// leg served through the production CachingStore. On maintainer-city every
// backing call is a bd subprocess (~0.75 s), and the pass runs inline in the
// controller tick.
func TestWispGCWorkArmBackingCallsPerPass(t *testing.T) {
	e := newSplitEnvWithRig(t, true)
	backing := beads.NewMemStore()
	backing.IDPrefix = "ra"
	rec := beadstest.NewOpRecordingStore(backing)
	mcShapedRigFixture(t, rec)
	cache := beads.NewCachingStore(rec, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	legs := workLegsFromCensus(e.cityPath, e.cfg, cityWorkLegOf(beads.WorkStore{Store: e.work}), map[string]beads.Store{e.rigName: cache})

	prevOut := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	pass := func(now time.Time) int {
		rec.Reset()
		withCloseAbandonedTTL(t, 5*time.Minute, func() {
			if _, err := wg.runGC(legs, beads.GraphStore{Store: e.graphStore()}, beads.SessionStore{}, beads.MailStore{}, now); err != nil {
				t.Logf("runGC: %v", err)
			}
		})
		if testing.Verbose() {
			counts := map[string]int{}
			for _, op := range rec.Ops() {
				counts[op.Kind+"/"+op.Method]++
			}
			t.Logf("backing ops by method: %v", counts)
		}
		return len(rec.Ops())
	}
	now := time.Now().Add(48 * time.Hour)
	dry1 := pass(now)
	dry2 := pass(now)
	t.Logf("dry-run backing calls per pass: %d, %d", dry1, dry2)
	if dry1 > 8 || dry2 > 8 {
		t.Fatalf("a dry-run pass made %d / %d backing calls; it must only list (a handful), never walk closures", dry1, dry2)
	}

	var enforced1, enforced2 int
	withWorkMoleculesEnforced(t, func() {
		enforced1 = pass(now)
		enforced2 = pass(now)
	})
	t.Logf("enforced backing calls per pass: %d, %d (walk cap %d)", enforced1, enforced2, wispGCWorkArmWalkCap)
	if enforced2 >= enforced1 {
		t.Fatalf("the second enforced pass made %d backing calls, the first %d: remembered refusals must not be re-walked", enforced2, enforced1)
	}
}
