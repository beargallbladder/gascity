package splittest

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// reviewedFederatedMarkers is the reviewed set of production call sites that
// wrap a store read in beads.FederatedRead or beads.FederatedGet, by file, with
// the number of marked reads in each. A marked read is invisible to the
// split-strict gate, so every entry is a reviewed claim that the read is a
// federation leg or residence probe whose miss another leg covers. Adding,
// moving or removing one changes this list in the same change, where a reviewer
// sees it.
var reviewedFederatedMarkers = map[string]int{
	// The shared by-id leg walk: every leg of a ModeFirstOwner plan is a
	// planned residence probe, and a miss moves the walk to the next leg.
	"internal/storeref/resolve.go": 1,
	// The hook-claim class route's work-first escalation (#8137): each seam
	// runs the work-scope call first and escalates to the binding only on
	// the not-found (or the empty continuation list) that proves the bead is
	// not there.
	"cmd/gc/claim_class_route.go": 5,
	// Molecule autoclose's tracking-convoy lookup probes the work store and
	// the graph store (a convoy minted pre-split can sit on the graph side).
	"cmd/gc/wisp_autoclose.go": 1,
	// gc convoy/workflow control across scopes: the scope probe for a control
	// bead (city, rigs, then the binding), and the per-view legs of workflow
	// delete and the source-workflow walk, which federate the scope stores and
	// the binding.
	"cmd/gc/cmd_convoy_dispatch.go": 5,
	// gc order sweep-tracking: the stale sweep, the retention count and the
	// retention prune run per leg over the scope stores (pre-cutover tracking
	// history) and the orders binding.
	"cmd/gc/order_dispatch.go": 3,
	// internal/api's workflowStores federation (graph binding, city, rigs):
	// workflow root and snapshot probes ask every leg, and only the owner
	// answers. The root_store_ref hint path names one store and stays
	// unmarked.
	"internal/api/convoy_event_stream.go":     2,
	"internal/api/handler_convoy_dispatch.go": 2,
	"internal/api/huma_handlers_convoys.go":   2,
	"internal/api/huma_handlers_runs.go":      1,
	"internal/api/orders_feed.go":             3,
	// The order-history federation (orders binding plus the city and rig
	// scopes kept for pre-cutover history).
	"internal/api/huma_handlers_orders.go": 3,
	// The source-workflow root scan over SourceWorkflowStores (graph binding,
	// city, rigs).
	"internal/sling/sling_core.go": 1,
}

// federatedMarkerCall matches a call of the marker from production code.
var federatedMarkerCall = regexp.MustCompile(`\bbeads\.Federated(Read|Get)\(`)

func TestFederatedReadMarkersAreReviewed(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	got := map[string]int{}
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipImportScanDir(path, root, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/beads/") && !strings.Contains(strings.TrimPrefix(rel, "internal/beads/"), "/") {
			return nil // the marker's own package
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		if n := len(federatedMarkerCall.FindAll(src, -1)); n > 0 {
			got[rel] = n
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scanning for federated-read markers: %v", err)
	}
	if scanned == 0 {
		t.Fatal("the marker guard scanned zero production files; it is not evaluating anything")
	}
	var diffs []string
	for file, n := range got {
		if want := reviewedFederatedMarkers[file]; want != n {
			diffs = append(diffs, file+": "+itoa(n)+" marked read(s), the reviewed list says "+itoa(want))
		}
	}
	for file, want := range reviewedFederatedMarkers {
		if _, ok := got[file]; !ok {
			diffs = append(diffs, file+": no marked read, the reviewed list says "+itoa(want))
		}
	}
	if len(diffs) > 0 {
		sort.Strings(diffs)
		t.Errorf("beads.FederatedRead / FederatedGet call sites differ from reviewedFederatedMarkers:\n  %s\n\n"+
			"A marked read is invisible to the split-strict gate. Mark only a federation leg or residence probe "+
			"whose miss another leg covers, and record it in reviewedFederatedMarkers with the reason, so the "+
			"marker set stays small and reviewed.", strings.Join(diffs, "\n  "))
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
