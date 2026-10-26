package splittest

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// StrictEnv turns the split-strict test gate on ("1") for a package whose
	// store seams consult GateEnabled.
	StrictEnv = "GC_TEST_SPLIT_STRICT"
	// StrictPanicEnv makes every gate violation panic at the offending call
	// ("1"), for bisecting one site with a full stack.
	StrictPanicEnv = "GC_TEST_SPLIT_STRICT_PANIC"
	// StrictReportEnv names a file the gate writes its observed site table to
	// when the test binary exits. Under Bazel the table also goes to
	// TEST_UNDECLARED_OUTPUTS_DIR (bazel-testlogs/.../test.outputs).
	StrictReportEnv = "GC_TEST_SPLIT_STRICT_REPORT"

	// observedReportName is the site table's file name in the undeclared
	// outputs directory; scripts/split-strict-baseline merges the shards'.
	observedReportName = "split-strict-observed.tsv"
)

// gateSink is the process-wide violation sink the gate's views record into.
var gateSink = newViolationSink()

// GateEnabled reports whether GC_TEST_SPLIT_STRICT=1 asked for the gate.
func GateEnabled() bool {
	return os.Getenv(StrictEnv) == "1"
}

// GateWorkView returns the gate's work view of store when the gate is on, and
// store unchanged otherwise. A nil store stays nil.
func GateWorkView(store beads.Store) beads.Store {
	if store == nil || !GateEnabled() {
		return store
	}
	return ProjectionOf(store).WorkView()
}

// GateClassView returns the gate's class view of store when the gate is on,
// and store unchanged otherwise. A nil store stays nil.
func GateClassView(store beads.Store) beads.Store {
	if store == nil || !GateEnabled() {
		return store
	}
	return ProjectionOf(store).ClassView()
}

// GateSites returns every call site the gate has charged so far.
func GateSites() []SiteSummary {
	return gateSink.summaries()
}

// GateMain finishes a test binary's run under the gate: it writes the observed
// site table, compares it with the package's checked-in baseline, and returns
// the exit code the binary should use. Call it from TestMain with m.Run's code
// when GateEnabled; it returns code unchanged when the gate is off.
//
// The comparison is the ratchet. A site the baseline does not list is NEW, and
// a new site fails the run whatever the tests did, with a block naming it. A
// baseline entry the run did not reach is not an error (a shard runs a subset
// of the tests); scripts/split-strict-baseline regenerates the file from a full
// run's merged tables.
func GateMain(code int, baselinePath string, out io.Writer) int {
	if !GateEnabled() {
		return code
	}
	observed := GateSites()
	table := FormatSiteTable(observed)
	for _, path := range observedReportPaths() {
		if err := os.WriteFile(path, []byte(table), 0o644); err != nil {
			fmt.Fprintf(out, "split-strict: writing the observed site table %s: %v\n", path, err) //nolint:errcheck // best-effort test log
		}
	}
	baseline, err := readBaselineFile(baselinePath)
	if err != nil {
		fmt.Fprintf(out, "split-strict: %v\n", err) //nolint:errcheck // best-effort test log
		return 1
	}
	result := CompareToBaseline(observed, baseline)
	if n := gateSink.testOnlyCount(); n > 0 {
		fmt.Fprintf(out, "split-strict: %d violation(s) came from test code reading through a view directly (fixture artifacts, not sites).\n", n) //nolint:errcheck // best-effort test log
	}
	fmt.Fprint(out, result.Report(baselinePath)) //nolint:errcheck // best-effort test log
	if len(result.New) > 0 {
		return 1
	}
	return code
}

// observedReportPaths lists where the observed site table goes.
func observedReportPaths() []string {
	var paths []string
	if path := strings.TrimSpace(os.Getenv(StrictReportEnv)); path != "" {
		paths = append(paths, path)
	}
	if dir := strings.TrimSpace(os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR")); dir != "" {
		paths = append(paths, filepath.Join(dir, observedReportName))
	}
	return paths
}

func readBaselineFile(path string) ([]BaselineEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading the split-strict baseline: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	entries, err := ParseBaseline(f)
	if err != nil {
		return nil, fmt.Errorf("parsing the split-strict baseline %s: %w", path, err)
	}
	return entries, nil
}
