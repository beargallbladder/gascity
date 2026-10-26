package splittest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// newTestProjection builds a projection over a fresh MemStore with its own
// sink, so a test inspects only what it caused.
func newTestProjection(t *testing.T) (*Projection, *beads.MemStore, *violationSink) {
	t.Helper()
	leaf := beads.NewMemStore()
	sink := newViolationSink()
	p := projectionOf(leaf, sink)
	t.Cleanup(func() {
		projectionsMu.Lock()
		delete(projections, any(leaf))
		projectionsMu.Unlock()
	})
	return p, leaf, sink
}

func seed(t *testing.T, s beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := s.Create(b)
	if err != nil {
		t.Fatalf("seeding %+v: %v", b, err)
	}
	return created
}

func sessionBead(title string) beads.Bead {
	return beads.Bead{Title: title, Type: "session", Labels: []string{"gc:session"}}
}

func TestProjectionMintsInfrastructureBeadsUnderTheBindingPrefix(t *testing.T) {
	p, leaf, _ := newTestProjection(t)
	work := seed(t, leaf, beads.Bead{Title: "task"})
	session := seed(t, leaf, sessionBead("s"))
	if !strings.HasPrefix(work.ID, "gc-") {
		t.Fatalf("work bead id = %q, want gc-", work.ID)
	}
	if !strings.HasPrefix(session.ID, "gcg-") {
		t.Fatalf("session bead id = %q, want gcg- (the binding's prefix)", session.ID)
	}
	if p.Leaf() != beads.Store(leaf) {
		t.Fatal("Leaf() is not the physical store")
	}
}

func TestProjectionViewsAreMemoizedAndUnwrap(t *testing.T) {
	p, leaf, _ := newTestProjection(t)
	if ProjectionOf(leaf) != p {
		t.Fatal("ProjectionOf(leaf) built a second projection")
	}
	if ProjectionOf(p.WorkView()) != p || ProjectionOf(p.ClassView()) != p {
		t.Fatal("a view did not unwrap to its own projection")
	}
	if p.WorkView() == p.ClassView() {
		t.Fatal("work and class views are the same value; code branching on graph == city would not take its split path")
	}
}

func TestProjectionHidesWrongClassRowsByID(t *testing.T) {
	p, leaf, sink := newTestProjection(t)
	session := seed(t, leaf, sessionBead("s"))
	task := seed(t, leaf, beads.Bead{Title: "task"})

	if _, err := p.WorkView().Get(session.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("work view Get(session) error = %v, want ErrNotFound", err)
	}
	if _, err := p.ClassView().Get(task.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("class view Get(task) error = %v, want ErrNotFound", err)
	}
	if got, err := p.ClassView().Get(session.ID); err != nil || got.ID != session.ID {
		t.Fatalf("class view Get(session) = %+v, %v", got, err)
	}
	if got, err := p.WorkView().Get(task.ID); err != nil || got.ID != task.ID {
		t.Fatalf("work view Get(task) = %+v, %v", got, err)
	}
	// Both wrong-class reads came straight from this test, so they are
	// fixture reads, not production sites.
	if sites := sink.summaries(); len(sites) != 0 {
		t.Fatalf("test-only reads produced sites: %+v", sites)
	}
	if got := sink.testOnlyCount(); got != 2 {
		t.Fatalf("test-only violations = %d, want 2", got)
	}
}

func TestProjectionChargesAProductionCaller(t *testing.T) {
	p, leaf, sink := newTestProjection(t)
	session := seed(t, leaf, sessionBead("s"))

	// storeref.Resolve probes each store it is given; it is production code,
	// so a wrong-class hit through the work view is charged to it.
	if _, err := storeref.Resolve(session.ID, []beads.Store{p.WorkView()}); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Resolve through the work view error = %v, want ErrNotFound", err)
	}
	sites := sink.summaries()
	if len(sites) != 1 {
		t.Fatalf("sites = %+v, want one", sites)
	}
	site := sites[0]
	if site.Site.File != "internal/storeref/storeref.go" || site.Site.Func != "storeref.Resolve" || site.Site.Line <= 0 {
		t.Fatalf("site = %+v, want internal/storeref/storeref.go storeref.Resolve", site.Site)
	}
	if site.SampleTest != "TestProjectionChargesAProductionCaller" || site.Tests != 1 {
		t.Fatalf("attribution = %q (%d tests), want this test", site.SampleTest, site.Tests)
	}
	if strings.Join(site.Ops, ",") != "get" || strings.Join(site.Classes, ",") != "sessions" || strings.Join(site.Views, ",") != "work" {
		t.Fatalf("site detail = ops %v classes %v views %v", site.Ops, site.Classes, site.Views)
	}
}

func TestProjectionDoesNotFlagAFederatedRead(t *testing.T) {
	p, leaf, sink := newTestProjection(t)
	session := seed(t, leaf, sessionBead("s"))
	err := beads.FederatedRead(func() error {
		_, err := storeref.Resolve(session.ID, []beads.Store{p.WorkView()})
		return err
	})
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("federated Resolve error = %v, want ErrNotFound (still hidden)", err)
	}
	if sites := sink.summaries(); len(sites) != 0 {
		t.Fatalf("a federated read was flagged: %+v", sites)
	}
	if got := sink.testOnlyCount(); got != 0 {
		t.Fatalf("a federated read was counted: %d", got)
	}
}

func TestProjectionListsHideWrongClassRowsAndFlagOnlySelectiveQueries(t *testing.T) {
	p, leaf, sink := newTestProjection(t)
	seed(t, leaf, sessionBead("s"))
	seed(t, leaf, beads.Bead{Title: "task"})

	broad, err := p.WorkView().List(beads.ListQuery{AllowScan: true})
	if err != nil || len(broad) != 1 || broad[0].Type != "task" {
		t.Fatalf("broad List through the work view = %+v, %v; want the task only", broad, err)
	}
	if open, err := p.WorkView().ListOpen(); err != nil || len(open) != 1 {
		t.Fatalf("ListOpen = %+v, %v", open, err)
	}
	if got := sink.testOnlyCount(); got != 0 {
		t.Fatalf("broad scans were flagged %d times; they merely filter unrelated rows", got)
	}

	labeled, err := p.WorkView().ListByLabel("gc:session", 0)
	if err != nil || len(labeled) != 0 {
		t.Fatalf("ListByLabel(gc:session) through the work view = %+v, %v; want none", labeled, err)
	}
	typed, err := p.WorkView().List(beads.ListQuery{Type: "session"})
	if err != nil || len(typed) != 0 {
		t.Fatalf("List(type=session) through the work view = %+v, %v; want none", typed, err)
	}
	if got := sink.testOnlyCount(); got != 2 {
		t.Fatalf("selective reads flagged %d times, want 2", got)
	}
}

func TestProjectionRefusesWritesToWrongClassRows(t *testing.T) {
	p, leaf, _ := newTestProjection(t)
	session := seed(t, leaf, sessionBead("s"))
	task := seed(t, leaf, beads.Bead{Title: "task"})
	work := p.WorkView()

	title := "renamed"
	checks := map[string]error{
		"update":       work.Update(session.ID, beads.UpdateOpts{Title: &title}),
		"close":        work.Close(session.ID),
		"set-metadata": work.SetMetadata(session.ID, "k", "v"),
		"dep-add":      work.DepAdd(task.ID, session.ID, "blocks"),
		"delete":       work.Delete(session.ID),
	}
	for op, err := range checks {
		if !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("%s of a session row through the work view: error = %v, want ErrNotFound", op, err)
		}
	}
	got, err := leaf.Get(session.ID)
	if err != nil || got.Status != "open" || got.Title != "s" || got.Metadata["k"] != "" {
		t.Fatalf("the refused writes changed the row: %+v, %v", got, err)
	}
	if deps, _ := work.DepList(session.ID, "down"); len(deps) != 0 {
		t.Fatalf("DepList of a session row through the work view = %+v, want none", deps)
	}
}

func TestProjectionPinsAMisroutedCreateToTheViewThatWroteIt(t *testing.T) {
	p, _, sink := newTestProjection(t)
	misrouted, err := p.WorkView().Create(sessionBead("misrouted"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ClassView().Get(misrouted.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("class view sees a session the work view wrote: %v", err)
	}
	if _, err := p.WorkView().Get(misrouted.ID); err != nil {
		t.Fatalf("work view lost the row it wrote: %v", err)
	}
	if got := sink.testOnlyCount(); got != 2 {
		t.Fatalf("flags = %d, want 2 (the create, and the class view's miss)", got)
	}
}

func TestProjectionForwardsCapabilitiesAndRefusesUnfilterableCounts(t *testing.T) {
	p, _, _ := newTestProjection(t)
	work := p.WorkView()
	if _, ok := work.(beads.StorageCreateStore); ok {
		t.Fatal("a view over a MemStore claims CreateWithStorage the leaf does not have")
	}
	if _, ok := beads.ConditionalWriterFor(work); !ok {
		t.Fatal("conditional writes were stripped")
	}
	if counter, ok := work.(beads.Counter); !ok {
		t.Fatal("view is not a Counter")
	} else if _, err := counter.Count(context.Background(), beads.ListQuery{AllowScan: true}); !errors.Is(err, beads.ErrCountUnsupported) {
		t.Fatalf("Count error = %v, want ErrCountUnsupported so callers list", err)
	}
	if rr, ok := work.(beads.ContextReadyReader); !ok {
		t.Fatal("view is not a ContextReadyReader")
	} else if _, err := rr.ReadyContext(context.Background()); err != nil {
		t.Fatalf("ReadyContext: %v", err)
	}
}

func TestGateViewsAreIdentityWhenTheGateIsOff(t *testing.T) {
	t.Setenv(StrictEnv, "")
	leaf := beads.NewMemStore()
	if GateWorkView(leaf) != beads.Store(leaf) || GateClassView(leaf) != beads.Store(leaf) {
		t.Fatal("the gate wrapped a store while off")
	}
	if GateWorkView(nil) != nil {
		t.Fatal("a nil store did not stay nil")
	}
}

func TestGateViewsProjectWhenTheGateIsOn(t *testing.T) {
	t.Setenv(StrictEnv, "1")
	leaf := beads.NewMemStore()
	t.Cleanup(func() {
		projectionsMu.Lock()
		delete(projections, any(leaf))
		projectionsMu.Unlock()
	})
	if GateWorkView(leaf) != ProjectionOf(leaf).WorkView() || GateClassView(leaf) != ProjectionOf(leaf).ClassView() {
		t.Fatal("the gate did not hand out the projection's views")
	}
}
