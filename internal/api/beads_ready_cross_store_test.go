package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
)

// TestBeadsReadyAppliesTheCrossStoreRuleOnASplitCity: GET /beads/ready (the
// dashboard's ready read) applies the same rule as `gc ready` on a split city.
// A bd work row whose blocker is a graph step held open in the binding is not
// served (bd alone would serve it), and is served once the step closes.
func TestBeadsReadyAppliesTheCrossStoreRuleOnASplitCity(t *testing.T) {
	work := splittest.NewNativeWorkStore(t, "gc")
	binding, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = binding.(interface{ CloseStore() error }).CloseStore() })
	last := 4
	step, err := binding.Create(beads.Bead{Title: "graph step", Type: "task", Priority: &last})
	if err != nil {
		t.Fatal(err)
	}
	row, err := work.Create(beads.Bead{Title: "held work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := work.DepAdd(row.ID, step.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	fs := newFakeState(t)
	fs.cityBeadStore = work
	fs.stores = map[string]beads.Store{}
	fs.graphBeadStore = binding
	h := newTestCityHandler(t, fs)

	ready := func() []string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/beads/ready"), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /beads/ready = %d: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Items []beads.Bead `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, b := range body.Items {
			ids = append(ids, b.ID)
		}
		return ids
	}
	if got := ready(); slices.Contains(got, row.ID) {
		t.Fatalf("/beads/ready = %v served %s while its graph blocker %s is open", got, row.ID, step.ID)
	}
	if err := binding.Close(step.ID); err != nil {
		t.Fatal(err)
	}
	if got := ready(); !slices.Contains(got, row.ID) {
		t.Fatalf("/beads/ready = %v withholds %s after its blocker closed", got, row.ID)
	}
}
