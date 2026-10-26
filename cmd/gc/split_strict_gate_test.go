package main

import (
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/rogpeppe/go-internal/testscript"
)

// The split-strict test gate for cmd/gc.
//
// With GC_TEST_SPLIT_STRICT=1 (//cmd/gc:gc_split_strict_test) this binary
// installs splitStrictSeam, so every city whose routes relocate nothing looks
// split to the code under test: the controller's and runtime's work accessors
// hand out the gate's WORK view of the physical store and resolveClassStore's
// identity branch its CLASS view (split_strict_seam.go). A read that reaches a
// wrong-class row through the wrong view is charged to its production call
// site; TestMain's splitStrictTestingM runs the ratchet over the sites the run
// reached, and a site the baseline does not list fails the run. Fixtures that
// already build a physical split (splitEnv, newSplitStores) keep their own
// stores: their relocated classes never reach the identity branch.

// splitStrictBaseline is this package's checked-in list of known violation
// sites, relative to the package directory the test binary runs in.
const splitStrictBaseline = "testdata/split_strict_baseline.tsv"

func init() {
	if !splittest.GateEnabled() {
		return
	}
	splitStrictSeam = splitStrictGate{}
	// Pass-through wrappers: a violation is charged to whoever called INTO
	// them, not to the wrapper.
	splittest.TreatFilesAsPlumbing(
		"cmd/gc/bead_policy_store.go",
		"cmd/gc/class_store_emit.go",
		"cmd/gc/split_strict_seam.go",
	)
}

// splitStrictGate hands out the splittest projection's views.
type splitStrictGate struct{}

func (splitStrictGate) workView(store beads.Store) beads.Store {
	return splittest.ProjectionOf(store).WorkView()
}

func (splitStrictGate) classView(store beads.Store) beads.Store {
	return splittest.ProjectionOf(store).ClassView()
}

// splitStrictTestingM runs the ratchet after the package's tests (a no-op
// without GC_TEST_SPLIT_STRICT=1).
type splitStrictTestingM struct{ m testscript.TestingM }

func (m splitStrictTestingM) Run() int {
	return splittest.GateMain(m.m.Run(), splitStrictBaseline, os.Stderr)
}

func TestSplitStrictBaselineParses(t *testing.T) {
	f, err := os.Open(splitStrictBaseline)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only
	if _, err := splittest.ParseBaseline(f); err != nil {
		t.Fatalf("%s: %v", splitStrictBaseline, err)
	}
}

func TestSplitStrictSeamIsInstalledOnlyUnderTheGate(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{Workspace: config.Workspace{Name: "c"}}
	cs := &controllerState{cityBeadStore: store, cityName: "c", cfg: cfg}
	if !splittest.GateEnabled() {
		if splitStrictSeam != nil {
			t.Fatal("the seam is installed although the gate is off")
		}
		if cs.CityBeadStore() != beads.Store(store) || cs.SessionsBeadStore().Store != beads.Store(store) {
			t.Fatal("accessors wrap the store although the gate is off")
		}
		return
	}
	p := splittest.ProjectionOf(store)
	if cs.CityBeadStore() != p.WorkView() {
		t.Fatal("CityBeadStore does not hand out the gate's work view")
	}
	if cs.SessionsBeadStore().Store != p.ClassView() || cs.GraphBeadStore().Store != p.ClassView() {
		t.Fatal("the class accessors do not hand out the gate's class view")
	}
}
