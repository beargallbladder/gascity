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
	// store on a split city: a routing defect with a fix owed.
	VerdictMisroute = "misroute"
	// VerdictKnownViolation is a placement the owner has ruled against but
	// current code still relies on (rig-store graph beads): tracked, not
	// allowed.
	VerdictKnownViolation = "known-violation"
	// VerdictFixture is a test-fixture artifact: the site is only reached
	// because a fixture shaped the store in a way production never does.
	VerdictFixture = "fixture"
	// VerdictTriage is a site no one has adjudicated yet.
	VerdictTriage = "triage"
)

var knownVerdicts = map[string]bool{
	VerdictMisroute:       true,
	VerdictKnownViolation: true,
	VerdictFixture:        true,
	VerdictTriage:         true,
}

// BaselineEntry is one known violation site in a package's checked-in
// baseline.
type BaselineEntry struct {
	// File and Line are the site; Func its function. The ratchet matches on
	// File and Func, so a site whose line moved is the same site.
	File string
	Line int
	Func string
	// Count is the number of distinct tests that hit the site in the run the
	// baseline was taken from.
	Count int
	// Verdict is one of the Verdict* constants.
	Verdict string
	// Note says why, and which fix owns it.
	Note string
}

// ParseBaseline reads a baseline: tab-separated site, func, count, verdict,
// note, one entry per line; blank lines and lines starting with '#' are
// skipped.
func ParseBaseline(r io.Reader) ([]BaselineEntry, error) {
	var entries []BaselineEntry
	seen := map[string]int{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 5 {
			return nil, fmt.Errorf("line %d: want 5 tab-separated fields (site, func, count, verdict, note), got %d", lineNo, len(fields))
		}
		file, siteLine, err := splitSite(fields[0])
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		count, err := strconv.Atoi(fields[2])
		if err != nil || count < 0 {
			return nil, fmt.Errorf("line %d: count %q is not a non-negative integer", lineNo, fields[2])
		}
		verdict := strings.TrimSpace(fields[3])
		if !knownVerdicts[verdict] {
			return nil, fmt.Errorf("line %d: verdict %q is not one of misroute, known-violation, fixture, triage", lineNo, verdict)
		}
		fn := strings.TrimSpace(fields[1])
		if fn == "" {
			return nil, fmt.Errorf("line %d: empty func", lineNo)
		}
		if strings.TrimSpace(fields[4]) == "" {
			return nil, fmt.Errorf("line %d: empty note; every known site says why it is there", lineNo)
		}
		key := file + "\x00" + fn
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("line %d: %s %s repeats line %d (the ratchet matches on file and func)", lineNo, file, fn, prev)
		}
		seen[key] = lineNo
		entries = append(entries, BaselineEntry{File: file, Line: siteLine, Func: fn, Count: count, Verdict: verdict, Note: strings.TrimSpace(fields[4])})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// splitSite parses "file:line".
func splitSite(site string) (string, int, error) {
	colon := strings.LastIndex(site, ":")
	if colon <= 0 {
		return "", 0, fmt.Errorf("site %q is not file:line", site)
	}
	line, err := strconv.Atoi(site[colon+1:])
	if err != nil || line <= 0 {
		return "", 0, fmt.Errorf("site %q has no line number", site)
	}
	return site[:colon], line, nil
}

// RatchetResult compares one run's observed sites with the baseline.
type RatchetResult struct {
	// Observed is every site the run reached.
	Observed []SiteSummary
	// New are observed sites the baseline does not list: the ratchet's
	// failures.
	New []SiteSummary
	// Moved are known sites observed at a different line than the baseline
	// records (informational: an edit above them shifted them).
	Moved []SiteSummary
	// Grown are known sites more tests hit than the baseline counted
	// (informational: counts are per full run, a shard sees fewer).
	Grown []SiteSummary
	// BaselineSize is the number of baseline entries.
	BaselineSize int
}

// CompareToBaseline runs the ratchet: an observed site is known when the
// baseline lists the same file and function.
func CompareToBaseline(observed []SiteSummary, baseline []BaselineEntry) RatchetResult {
	known := make(map[string]BaselineEntry, len(baseline))
	for _, entry := range baseline {
		known[entry.File+"\x00"+entry.Func] = entry
	}
	result := RatchetResult{Observed: observed, BaselineSize: len(baseline)}
	for _, site := range observed {
		entry, ok := known[site.Site.File+"\x00"+site.Site.Func]
		if !ok {
			result.New = append(result.New, site)
			continue
		}
		if entry.Line != site.Site.Line {
			result.Moved = append(result.Moved, site)
		}
		if site.Tests > entry.Count {
			result.Grown = append(result.Grown, site)
		}
	}
	return result
}

// Report renders the result for the test log.
func (r RatchetResult) Report(baselinePath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "split-strict: %d violation site(s) observed; the baseline %s lists %d.\n", len(r.Observed), baselinePath, r.BaselineSize)
	for _, site := range r.Moved {
		fmt.Fprintf(&b, "split-strict: known site moved: %s %s\n", site.Site, site.Site.Func)
	}
	for _, site := range r.Grown {
		fmt.Fprintf(&b, "split-strict: known site reached by more tests than the baseline counts: %s %s (%d tests)\n", site.Site, site.Site.Func, site.Tests)
	}
	if len(r.New) == 0 {
		b.WriteString("split-strict: RATCHET PASS: no new violation site.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "SPLIT-STRICT RATCHET FAIL: %d NEW violation site(s), not in %s:\n", len(r.New), baselinePath)
	for _, site := range r.New {
		fmt.Fprintf(&b, "  NEW %s %s: %s of %s rows through the %s view; %d test(s), e.g. %s\n",
			site.Site, site.Site.Func, strings.Join(site.Ops, ","), strings.Join(site.Classes, ","),
			strings.Join(site.Views, ","), site.Tests, site.SampleTest)
	}
	b.WriteString("Each NEW site reads or writes a row of one class through the store of another: on a split city that read comes back empty. " +
		"Route it to the store that owns the class; if it is a deliberate federation leg or residence probe, wrap the read in beads.FederatedRead " +
		"(a reviewed marker); rerun one test with GC_TEST_SPLIT_STRICT_PANIC=1 for the full stack.\n")
	return b.String()
}

// FormatSiteTable renders observed sites as the tab-separated table the gate
// writes per run: site, func, tests, events, ops, classes, views, sample test.
func FormatSiteTable(sites []SiteSummary) string {
	sorted := append([]SiteSummary(nil), sites...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Site.File != sorted[j].Site.File {
			return sorted[i].Site.File < sorted[j].Site.File
		}
		return sorted[i].Site.Line < sorted[j].Site.Line
	})
	var b strings.Builder
	b.WriteString("# site\tfunc\ttests\tevents\tops\tclasses\tviews\tsample_test\tsample_via\n")
	for _, s := range sorted {
		fmt.Fprintf(&b, "%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n", s.Site, s.Site.Func, s.Tests, s.Events,
			strings.Join(s.Ops, ","), strings.Join(s.Classes, ","), strings.Join(s.Views, ","), s.SampleTest, s.SampleVia)
	}
	return b.String()
}
