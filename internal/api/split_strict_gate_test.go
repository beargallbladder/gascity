package api

import (
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
)

// The split-strict test gate for internal/api.
//
// With GC_TEST_SPLIT_STRICT=1 (//internal/api:api_split_strict_test) every
// fakeState work accessor (CityBeadStore, BeadStore, BeadStores) hands out the
// gate's WORK view of the physical store and every class accessor left on the
// city store (Sessions, Graph, Nudges, Orders, and the default mail provider)
// its CLASS view, so the package's tests run against a projection split: a
// handler that reads a session through the work store sees nothing, as it would
// on a split city, and the read is charged to its production call site. See
// internal/beads/splittest/projection.go.
//
// TestMain runs the ratchet over the sites the run reached: a site the baseline
// below does not list fails the run. Regenerate the baseline from a full run
// with scripts/split-strict-baseline.

// splitStrictBaseline is this package's checked-in list of known violation
// sites, relative to the package directory the test binary runs in.
const splitStrictBaseline = "testdata/split_strict_baseline.tsv"

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

func TestFakeStateHandsOutGateViewsOnlyUnderTheGate(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = fs.stores["myrig"]
	if splittest.GateEnabled() {
		p := splittest.ProjectionOf(fs.cityBeadStore)
		if fs.CityBeadStore() != p.WorkView() || fs.BeadStore("myrig") != p.WorkView() {
			t.Fatal("work accessors do not hand out the gate's work view")
		}
		if fs.SessionsBeadStore().Store != p.ClassView() || fs.GraphBeadStore().Store != p.ClassView() {
			t.Fatal("class accessors do not hand out the gate's class view")
		}
	} else if fs.CityBeadStore() != fs.cityBeadStore || fs.SessionsBeadStore().Store != fs.cityBeadStore {
		t.Fatal("accessors wrap the store although the gate is off")
	}
	relocated := beads.NewMemStore()
	fs.graphBeadStore = relocated
	if fs.GraphBeadStore().Store != beads.Store(relocated) {
		t.Fatal("a store relocated to its own binding was wrapped")
	}
}
