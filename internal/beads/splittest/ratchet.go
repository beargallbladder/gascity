package splittest

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Verdicts a baseline entry may carry. Every known site is adjudicated, so a
// reviewer reading the baseline sees what each one is, not only that it exists.
const (
	// VerdictMisroute is a production read or write that reaches the wrong
	// store on a split city: a routing defect with a fix owed (S3).
	VerdictMisroute = "misroute"
	// VerdictKnownViolation is a placement the owner has ruled against but
	// current code still relies on (rig-store graph beads, S4): tracked, not
	// allowed.
	VerdictKnownViolation = "known-violation"
	// VerdictRelic is a read that only finds the row because it was written
	// before the split (a pre-cutover copy in a scope store).
	VerdictRelic = "relic"
	// VerdictFixture is a test-fixture artifact: the site is only reached
	// because a fixture shaped the store in a way production never does.
	VerdictFixture = "fixture"
	// VerdictTriage is a site no one has adjudicated yet. The ratchet FAILS on
	// it: a baseline row is a ruling, not a placeholder.
	VerdictTriage = "triage"
)

var knownVerdicts = map[string]bool{
	VerdictMisroute:       true,
	VerdictKnownViolation: true,
	VerdictRelic:          true,
	VerdictFixture:        true,
	VerdictTriage:         true,
}

// baselineColumns is the baseline's tab-separated layout.
const baselineColumns = "file\tfunc\top\tview\tclass\tcaller\tmax_lines\ttests\tverdict\tnote"

// BaselineEntry is one known violation key in a package's checked-in
// baseline.
type BaselineEntry struct {
	Key SiteKey
	// MaxLines caps the distinct lines in Key.File's Key.Func the gate may
	// see violations at: a second offending line in a known function is a new
	// site, even under the same key.
	MaxLines int
	// Tests is the number of distinct tests that reached the key in the run
	// the baseline was taken from.
	Tests int
	// Verdict is one of the Verdict* constants.
	Verdict string
	// Note says why, and which fix owns it.
	Note string
}

// ParseBaseline reads a baseline: tab-separated rows in baselineColumns order;
// blank lines and lines starting with '#' are skipped.
func ParseBaseline(r io.Reader) ([]BaselineEntry, error) {
	var entries []BaselineEntry
	seen := map[SiteKey]int{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 10 {
			return nil, fmt.Errorf("line %d: want 10 tab-separated fields (%s), got %d", lineNo, strings.ReplaceAll(baselineColumns, "\t", ", "), len(f))
		}
		for i := 0; i < 6; i++ {
			if strings.TrimSpace(f[i]) == "" {
				return nil, fmt.Errorf("line %d: empty key field %d", lineNo, i+1)
			}
		}
		maxLines, err := strconv.Atoi(f[6])
		if err != nil || maxLines < 1 {
			return nil, fmt.Errorf("line %d: max_lines %q is not a positive integer", lineNo, f[6])
		}
		tests, err := strconv.Atoi(f[7])
		if err != nil || tests < 0 {
			return nil, fmt.Errorf("line %d: tests %q is not a non-negative integer", lineNo, f[7])
		}
		verdict := strings.TrimSpace(f[8])
		if !knownVerdicts[verdict] {
			return nil, fmt.Errorf("line %d: verdict %q is not one of misroute, known-violation, relic, fixture, triage", lineNo, verdict)
		}
		if strings.TrimSpace(f[9]) == "" {
			return nil, fmt.Errorf("line %d: empty note; every known site says why it is there", lineNo)
		}
		key := SiteKey{File: f[0], Func: f[1], Op: f[2], View: f[3], Class: f[4], Caller: f[5]}
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("line %d: key %s repeats line %d", lineNo, key, prev)
		}
		seen[key] = lineNo
		entries = append(entries, BaselineEntry{Key: key, MaxLines: maxLines, Tests: tests, Verdict: verdict, Note: strings.TrimSpace(f[9])})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// LineCapExcess is a known function the run saw offending at more distinct
// lines than the baseline allows.
type LineCapExcess struct {
	File, Func string
	Lines      []int
	Cap        int
}

// RatchetResult compares one run's observed sites with the baseline.
type RatchetResult struct {
	// Observed is every key the run reached.
	Observed []SiteSummary
	// New are observed keys the baseline does not list.
	New []SiteSummary
	// Triage are observed keys the baseline lists as unadjudicated.
	Triage []SiteSummary
	// LineCap are known functions seen at more lines than the baseline caps.
	LineCap []LineCapExcess
	// Grown are known keys more tests hit than the baseline counted
	// (informational: counts are per full run, a shard sees fewer).
	Grown []SiteSummary
	// BaselineSize is the number of baseline entries.
	BaselineSize int
}

// Failed reports whether the ratchet fails the run.
func (r RatchetResult) Failed() bool {
	return len(r.New) > 0 || len(r.Triage) > 0 || len(r.LineCap) > 0
}

// CompareToBaseline runs the ratchet.
func CompareToBaseline(observed []SiteSummary, baseline []BaselineEntry) RatchetResult {
	known := make(map[SiteKey]BaselineEntry, len(baseline))
	caps := map[[2]string]int{}
	for _, entry := range baseline {
		known[entry.Key] = entry
		fn := [2]string{entry.Key.File, entry.Key.Func}
		if entry.MaxLines > caps[fn] {
			caps[fn] = entry.MaxLines
		}
	}
	result := RatchetResult{Observed: observed, BaselineSize: len(baseline)}
	linesByFunc := map[[2]string]map[int]bool{}
	for _, site := range observed {
		entry, ok := known[site.Key]
		if !ok {
			result.New = append(result.New, site)
			continue
		}
		if entry.Verdict == VerdictTriage {
			result.Triage = append(result.Triage, site)
		}
		if site.Tests > entry.Tests {
			result.Grown = append(result.Grown, site)
		}
		fn := [2]string{site.Key.File, site.Key.Func}
		if linesByFunc[fn] == nil {
			linesByFunc[fn] = map[int]bool{}
		}
		for _, l := range site.Lines {
			linesByFunc[fn][l] = true
		}
	}
	for fn, lines := range linesByFunc {
		if len(lines) > caps[fn] {
			sorted := make([]int, 0, len(lines))
			for l := range lines {
				sorted = append(sorted, l)
			}
			sort.Ints(sorted)
			result.LineCap = append(result.LineCap, LineCapExcess{File: fn[0], Func: fn[1], Lines: sorted, Cap: caps[fn]})
		}
	}
	sort.Slice(result.LineCap, func(i, j int) bool {
		return result.LineCap[i].File+result.LineCap[i].Func < result.LineCap[j].File+result.LineCap[j].Func
	})
	return result
}

// Report renders the result for the test log.
func (r RatchetResult) Report(baselinePath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "split-strict: %d violation key(s) observed; the baseline %s lists %d.\n", len(r.Observed), baselinePath, r.BaselineSize)
	for _, site := range r.Grown {
		fmt.Fprintf(&b, "split-strict: known key reached by more tests than the baseline counts: %s (%d tests)\n", site.Key, site.Tests)
	}
	if !r.Failed() {
		return b.String()
	}
	for _, site := range r.New {
		fmt.Fprintf(&b, "  NEW %s:%s %s: %s of %s rows through the %s view, from %s; %d test(s), e.g. %s\n",
			site.Key.File, joinInts(site.Lines), site.Key.Func, site.Key.Op, site.Key.Class, site.Key.View,
			site.Key.Caller, site.Tests, site.SampleTest)
	}
	for _, site := range r.Triage {
		fmt.Fprintf(&b, "  TRIAGE %s: the baseline lists it unadjudicated; give it a verdict\n", site.Key)
	}
	for _, x := range r.LineCap {
		fmt.Fprintf(&b, "  LINES %s %s: offending at lines %s, the baseline caps it at %d\n", x.File, x.Func, joinInts(x.Lines), x.Cap)
	}
	b.WriteString("Each NEW key reads or writes a row of one class through the store of another: on a split city that read comes back empty. " +
		"Route it to the store that owns the class; if it is a deliberate federation leg or residence probe, wrap the READ in beads.FederatedRead " +
		"(a reviewed marker); rerun one test with GC_TEST_SPLIT_STRICT_PANIC=1 for the full stack.\n")
	return b.String()
}

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// observedColumns is the observed site table's layout.
const observedColumns = "file\tfunc\top\tview\tclass\tcaller\tlines\ttests\tevents\tsample_test\tsample_via"

// FormatSiteTable renders observed keys as the tab-separated table the gate
// writes per run (observedColumns).
func FormatSiteTable(sites []SiteSummary) string {
	var b strings.Builder
	b.WriteString("# " + observedColumns + "\n")
	for _, s := range sites {
		k := s.Key
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\n", k.File, k.Func, k.Op, k.View, k.Class, k.Caller,
			joinInts(s.Lines), s.Tests, s.Events, s.SampleTest, s.SampleVia)
	}
	return b.String()
}
