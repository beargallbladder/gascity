package splittest

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// SplitViolation is one read or write that reached a wrong-class row through
// the wrong store: on a split city the row would not be there, so the read
// would have come back silently empty (or the write would have landed in a
// store no reader of its class looks at).
type SplitViolation struct {
	// View is the side of the split the caller held: "work" or "class".
	View string
	// Op is the store operation: "get", "list", "children", "ready",
	// "update", "create", "dep-add", ...
	Op string
	// Class is the coordination class of the offending row.
	Class string
	// ID is the offending row's id.
	ID string
	// Site is the production call site the violation is charged to.
	Site Site
	// Via is the production frame that called Site, for triage.
	Via Site
	// Test is the top-level test the call ran under ("" off the test's
	// goroutine).
	Test string
}

// String renders the violation for a failure message.
func (v SplitViolation) String() string {
	test := v.Test
	if test == "" {
		test = "?"
	}
	return fmt.Sprintf("%s %s: %s of %s row %s through the %s store [test %s]", v.Site, v.Site.Func, v.Op, v.Class, v.ID, v.View, test)
}

// SiteSummary aggregates every violation charged to one call site.
type SiteSummary struct {
	Site Site
	// Tests is the number of distinct tests that hit the site. It is the
	// count the ratchet baseline records: deterministic for a given test set,
	// and additive across shards (each test runs in exactly one).
	Tests int
	// Events is the raw number of violations, which depends on polling and
	// retries and is reported for information only.
	Events int
	// Ops, Classes and Views list what was seen at the site, sorted.
	Ops, Classes, Views []string
	// SampleTest names one test that hit the site.
	SampleTest string
	// SampleVia names one caller of the site, for triage.
	SampleVia string
}

// violationSink aggregates violations by call site. The split-strict gate has
// one process-wide sink because its views are handed out by production seams
// that have no *testing.T; attribution to a test comes from the stack.
type violationSink struct {
	mu    sync.Mutex
	sites map[string]*siteAgg
	// testOnly counts violations with no production frame on the stack: a
	// test reading through a view directly. They are fixture artifacts by
	// construction and are not sites.
	testOnly int
}

type siteAgg struct {
	site    Site
	tests   map[string]bool
	events  int
	ops     map[string]bool
	classes map[string]bool
	views   map[string]bool
	sample  string
	via     string
}

func newViolationSink() *violationSink {
	return &violationSink{sites: map[string]*siteAgg{}}
}

// record adds one violation, panicking first at a production site when
// GC_TEST_SPLIT_STRICT_PANIC=1 asks for a stack at the offending call (for
// bisecting one site).
func (s *violationSink) record(v SplitViolation) {
	if v.Site.File != "" && os.Getenv(StrictPanicEnv) == "1" {
		panic("split-strict violation: " + v.String())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.Site.File == "" {
		s.testOnly++
		return
	}
	key := v.Site.File + "\x00" + v.Site.Func + "\x00" + fmt.Sprint(v.Site.Line)
	agg, ok := s.sites[key]
	if !ok {
		agg = &siteAgg{site: v.Site, tests: map[string]bool{}, ops: map[string]bool{}, classes: map[string]bool{}, views: map[string]bool{}}
		s.sites[key] = agg
	}
	test := v.Test
	if test == "" {
		test = "?"
	}
	agg.tests[test] = true
	if agg.sample == "" || (agg.sample == "?" && test != "?") {
		agg.sample = test
	}
	if agg.via == "" && v.Via.File != "" {
		agg.via = v.Via.String() + " " + v.Via.Func
	}
	agg.events++
	agg.ops[v.Op] = true
	agg.classes[v.Class] = true
	agg.views[v.View] = true
}

// summaries returns one summary per site, sorted by file, line and function.
func (s *violationSink) summaries() []SiteSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SiteSummary, 0, len(s.sites))
	for _, agg := range s.sites {
		out = append(out, SiteSummary{
			Site:       agg.site,
			Tests:      len(agg.tests),
			Events:     agg.events,
			Ops:        sortedKeys(agg.ops),
			Classes:    sortedKeys(agg.classes),
			Views:      sortedKeys(agg.views),
			SampleTest: agg.sample,
			SampleVia:  agg.via,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Site, out[j].Site
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Func < b.Func
	})
	return out
}

// testOnlyCount returns the number of violations no production frame owned.
func (s *violationSink) testOnlyCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.testOnly
}

// reset drops everything recorded so far.
func (s *violationSink) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sites = map[string]*siteAgg{}
	s.testOnly = 0
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// violationLog collects one strict leaf's split violations for its test's
// cleanup check, the read-side twin of residenceLog.
type violationLog struct {
	mu         sync.Mutex
	violations []SplitViolation
}

func (l *violationLog) record(v SplitViolation) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.violations = append(l.violations, v)
}

func (l *violationLog) take() []SplitViolation {
	l.mu.Lock()
	defer l.mu.Unlock()
	taken := l.violations
	l.violations = nil
	return taken
}

// unclaimedSplitViolationsMessage renders a strict leaf's cleanup failure, or ""
// when the fixture left nothing behind.
func unclaimedSplitViolationsMessage(store string, violations []SplitViolation) string {
	if len(violations) == 0 {
		return ""
	}
	lines := make([]string, 0, len(violations))
	for _, v := range violations {
		lines = append(lines, "  - "+v.String())
	}
	return fmt.Sprintf("%d split violation(s) on the strict %s store:\n%s\n"+
		"On a split city each class lives in its own store, so these reads would come back empty and these writes would land where no reader of their class looks. "+
		"Route the call to the store that owns the class, wrap a deliberate federation leg or residence probe in beads.FederatedRead, "+
		"or call splittest.TakeSplitViolations if asserting the misroute is the point of this test.",
		len(violations), store, strings.Join(lines, "\n"))
}
