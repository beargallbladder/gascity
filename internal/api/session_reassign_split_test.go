package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
)

// TestSplitNamedSessionWakeReassignsWorkInEveryWorkStore is write-audit B7: when
// the API replaces a continuity-ineligible named session, the open work still
// assigned to the retired session must move to its successor wherever that work
// lives. The reassign used to list work through the SESSIONS store, which on a
// split city is the binding: only binding-resident graph steps moved, and every
// task in the city and rig work stores kept the retired session id. Named
// session work is not pool-routed, so nothing else rescues it.
func TestSplitNamedSessionWakeReassignsWorkInEveryWorkStore(t *testing.T) {
	fs := newSessionFakeState(t)
	work := splittest.NewWorkStore(t, "gc")
	rig := splittest.NewWorkStore(t, "mr")
	binding, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = binding.(interface{ CloseStore() error }).CloseStore() })
	fs.cityBeadStore = work
	fs.stores = map[string]beads.Store{"myrig": rig}
	fs.sessionsBeadStore = binding
	fs.graphBeadStore = binding
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	fs.cfg.Agents[0].Dir = ""
	fs.cfg.NamedSessions[0].Dir = ""
	spec, ok, err := srv.findNamedSessionSpecForTarget(binding, "worker")
	if err != nil || !ok {
		t.Fatalf("findNamedSessionSpecForTarget(worker) = %v, %v", ok, err)
	}
	historicalID, err := srv.materializeNamedSession(binding, spec)
	if err != nil {
		t.Fatalf("materializeNamedSession(worker): %v", err)
	}
	if err := binding.SetMetadataBatch(historicalID, map[string]string{"state": "archived", "continuity_eligible": "false"}); err != nil {
		t.Fatal(err)
	}
	cityTask := mustCreateAssigned(t, work, "city task", historicalID)
	rigTask := mustCreateAssigned(t, rig, "rig task", historicalID)
	graphStep := mustCreateAssigned(t, binding, "graph step", historicalID)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/session/worker/wake"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("wake = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding the wake response %s: %v", rec.Body.String(), err)
	}
	freshID := resp.ID
	if freshID == "" || freshID == historicalID {
		t.Fatalf("fresh id = %q, want a new canonical bead distinct from %s", freshID, historicalID)
	}
	for _, c := range []struct {
		store beads.Store
		id    string
		where string
	}{
		{work, cityTask.ID, "city work store"},
		{rig, rigTask.ID, "rig work store"},
		{binding, graphStep.ID, "graph binding"},
	} {
		got, err := c.store.Get(c.id)
		if err != nil {
			t.Fatalf("Get(%s): %v", c.id, err)
		}
		if got.Assignee != freshID {
			t.Errorf("%s %s assignee = %q, want the successor %s: the retired session's work was not reassigned there", c.where, c.id, got.Assignee, freshID)
		}
	}
}

func mustCreateAssigned(t *testing.T, store beads.Store, title, assignee string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: title, Type: "task", Assignee: assignee})
	if err != nil {
		t.Fatalf("Create(%s): %v", title, err)
	}
	return b
}
