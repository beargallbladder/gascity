package splittest

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleBaseline = `# comment
internal/api/residency_by_id.go:82	api.(*Server).resolveBeadOwner	3	misroute	S3: by-id through the work leg

cmd/gc/sling.go:10	main.attach	1	known-violation	S4: rig-store graph write
`

func TestParseBaseline(t *testing.T) {
	entries, err := ParseBaseline(strings.NewReader(sampleBaseline))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	want := BaselineEntry{File: "internal/api/residency_by_id.go", Line: 82, Func: "api.(*Server).resolveBeadOwner", Count: 3, Verdict: VerdictMisroute, Note: "S3: by-id through the work leg"}
	if entries[0] != want {
		t.Fatalf("entry = %+v, want %+v", entries[0], want)
	}
}

func TestParseBaselineRejectsMalformedEntries(t *testing.T) {
	for name, text := range map[string]string{
		"fields":    "a.go:1\tf\t1\tmisroute\n",
		"site":      "a.go\tf\t1\tmisroute\tnote\n",
		"count":     "a.go:1\tf\tx\tmisroute\tnote\n",
		"verdict":   "a.go:1\tf\t1\tok\tnote\n",
		"note":      "a.go:1\tf\t1\tmisroute\t \n",
		"duplicate": "a.go:1\tf\t1\tmisroute\tn\na.go:9\tf\t1\tmisroute\tn\n",
	} {
		if _, err := ParseBaseline(strings.NewReader(text)); err == nil {
			t.Errorf("%s: ParseBaseline accepted %q", name, text)
		}
	}
}

func TestCompareToBaselineFailsOnlyOnNewSites(t *testing.T) {
	baseline, err := ParseBaseline(strings.NewReader(sampleBaseline))
	if err != nil {
		t.Fatal(err)
	}
	observed := []SiteSummary{
		{Site: Site{File: "internal/api/residency_by_id.go", Line: 90, Func: "api.(*Server).resolveBeadOwner"}, Tests: 5},
		{Site: Site{File: "internal/api/new.go", Line: 7, Func: "api.fresh"}, Tests: 1, Ops: []string{"get"}, Classes: []string{"graph"}, Views: []string{"work"}, SampleTest: "TestX"},
	}
	result := CompareToBaseline(observed, baseline)
	if len(result.New) != 1 || result.New[0].Site.Func != "api.fresh" {
		t.Fatalf("New = %+v, want api.fresh only", result.New)
	}
	if len(result.Moved) != 1 || len(result.Grown) != 1 {
		t.Fatalf("Moved = %+v, Grown = %+v; want the moved, grown known site in both", result.Moved, result.Grown)
	}
	report := result.Report("testdata/b.tsv")
	for _, want := range []string{"SPLIT-STRICT RATCHET FAIL", "NEW internal/api/new.go:7 api.fresh", "TestX", "beads.FederatedRead"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}

	clean := CompareToBaseline(observed[:1], baseline)
	if len(clean.New) != 0 || !strings.Contains(clean.Report("b"), "RATCHET PASS") {
		t.Fatalf("a known-only run failed the ratchet: %+v", clean)
	}
}

func TestGateMainFailsTheRunOnANewSiteAndWritesTheTable(t *testing.T) {
	t.Setenv(StrictEnv, "1")
	dir := t.TempDir()
	report := filepath.Join(dir, "observed.tsv")
	t.Setenv(StrictReportEnv, report)
	t.Setenv("TEST_UNDECLARED_OUTPUTS_DIR", "")
	baselinePath := filepath.Join(dir, "baseline.tsv")
	if err := os.WriteFile(baselinePath, []byte(sampleBaseline), 0o644); err != nil {
		t.Fatal(err)
	}
	gateSink.reset()
	t.Cleanup(gateSink.reset)

	var out bytes.Buffer
	if code := GateMain(0, baselinePath, &out); code != 0 {
		t.Fatalf("GateMain with no sites = %d, want 0:\n%s", code, out.String())
	}
	gateSink.record(SplitViolation{View: "work", Op: "get", Class: "graph", ID: "gcg-1", Site: Site{File: "internal/api/new.go", Line: 3, Func: "api.fresh"}, Test: "TestY"})
	out.Reset()
	if code := GateMain(0, baselinePath, &out); code != 1 {
		t.Fatalf("GateMain with a new site = %d, want 1:\n%s", code, out.String())
	}
	table, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(table), "internal/api/new.go:3\tapi.fresh\t1\t1\tget\tgraph\twork\tTestY\t") {
		t.Fatalf("observed table:\n%s", table)
	}
	out.Reset()
	if code := GateMain(3, baselinePath, &out); code != 1 {
		t.Fatalf("GateMain kept code %d over a new site", code)
	}
}

func TestGateMainIsANoOpWhenTheGateIsOff(t *testing.T) {
	t.Setenv(StrictEnv, "")
	var out bytes.Buffer
	if code := GateMain(7, "/nonexistent", &out); code != 7 || out.Len() != 0 {
		t.Fatalf("GateMain off = %d, %q", code, out.String())
	}
}

func TestGateMainFailsOnAMissingBaseline(t *testing.T) {
	t.Setenv(StrictEnv, "1")
	t.Setenv(StrictReportEnv, "")
	t.Setenv("TEST_UNDECLARED_OUTPUTS_DIR", "")
	var out bytes.Buffer
	if code := GateMain(0, filepath.Join(t.TempDir(), "none.tsv"), &out); code != 1 {
		t.Fatalf("GateMain with no baseline = %d, want 1", code)
	}
}
