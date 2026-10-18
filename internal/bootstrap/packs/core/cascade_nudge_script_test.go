package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cascadeNudgeScriptPath is the on-disk copy of the cascade-nudge order's
// script. The test runs it, so it needs a real path.
const cascadeNudgeScriptPath = "assets/scripts/cascade-nudge-on-blocker-close.sh"

// cascadeNudgeEnvKeys are the settings that would point the script at a live
// session's city, state, or trace file. The test supplies its own state dir.
var cascadeNudgeEnvKeys = map[string]struct{}{
	"GC_CITY":                    {},
	"GC_CITY_RUNTIME_DIR":        {},
	"GC_PACK_STATE_DIR":          {},
	"GC_BD_TRACE_JSON":           {},
	"GC_CASCADE_NUDGE_LOOKBACK":  {},
	"GC_CASCADE_NUDGE_RETENTION": {},
}

func runCascadeNudge(t *testing.T, binDir, stateDir string) (string, error) {
	t.Helper()
	return runPackScript(t, cascadeNudgeScriptPath, binDir, cascadeNudgeEnvKeys,
		[]string{"GC_PACK_STATE_DIR=" + stateDir})
}

func countNudges(t *testing.T, logPath string) (int, string) {
	t.Helper()
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(logged), "\n") {
		if strings.HasPrefix(line, "gc session nudge ") {
			n++
		}
	}
	return n, string(logged)
}

// TestCascadeNudgeAbsorbsDuplicateCloseEvents pins the order's half of the
// at-least-once bead.closed contract. One close can reach the bus twice, from
// the process that closed the bead and from the controller's cache when its
// read saw the close first. When the copies land on either side of an order
// evaluation the order runs once per copy, and the second run's lookback
// window holds both. The dependent's assignee must still be nudged once.
func TestCascadeNudgeAbsorbsDuplicateCloseEvents(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; cascade-nudge-on-blocker-close.sh requires it")
	}
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	binDir, logPath := fakeGCBin(t, `case "$1" in
events) cat '`+eventsPath+`' ;;
bd) case "$*" in
    *direction=down*) printf '%s\n' '[{"id":"ga-blk","status":"closed"}]' ;;
    *) printf '%s\n' '[{"id":"ga-dep","status":"open","assignee":"worker-1"}]' ;;
    esac ;;
esac
exit 0
`)
	stateDir := filepath.Join(dir, "state")
	copies := []string{
		`{"seq":7,"type":"bead.closed","actor":"gc","subject":"ga-blk","payload":{"bead":{"id":"ga-blk","status":"closed"}}}`,
		`{"seq":8,"type":"bead.closed","actor":"cache-reconcile","subject":"ga-blk","payload":{"bead":{"id":"ga-blk","status":"closed"}}}`,
	}

	var events strings.Builder
	for i, line := range copies {
		events.WriteString(line + "\n")
		if err := os.WriteFile(eventsPath, []byte(events.String()), 0o644); err != nil {
			t.Fatalf("write events: %v", err)
		}
		if out, err := runCascadeNudge(t, binDir, stateDir); err != nil {
			t.Fatalf("run %d: cascade-nudge-on-blocker-close.sh failed: %v\n%s", i+1, err, out)
		}
		if got, logged := countNudges(t, logPath); got != 1 {
			t.Fatalf("after run %d: nudges = %d, want 1 for one close delivered twice; gc calls:\n%s", i+1, got, logged)
		}
	}
	_, logged := countNudges(t, logPath)
	if !strings.Contains(logged, "gc session nudge worker-1 blocker ga-blk closed") {
		t.Fatalf("the nudge must reach the dependent's assignee and name the blocker; gc calls:\n%s", logged)
	}
}

// cascadeNudgeOnce runs the script once over one close of ga-blk whose
// dependent ga-dep, assigned to worker-1, has the blockers down answers for
// (a JSON array, or a shell command's exit for a failed read), and returns
// the nudges it sent.
func cascadeNudgeOnce(t *testing.T, down string) (int, string) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; cascade-nudge-on-blocker-close.sh requires it")
	}
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	event := `{"seq":7,"type":"bead.closed","actor":"gc","subject":"ga-blk","payload":{"bead":{"id":"ga-blk","status":"closed"}}}`
	if err := os.WriteFile(eventsPath, []byte(event+"\n"), 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}
	binDir, logPath := fakeGCBin(t, `case "$1" in
events) cat '`+eventsPath+`' ;;
bd) case "$*" in
    *direction=down*) `+down+` ;;
    *) printf '%s\n' '[{"id":"ga-dep","status":"open","assignee":"worker-1"}]' ;;
    esac ;;
esac
exit 0
`)
	if out, err := runCascadeNudge(t, binDir, filepath.Join(dir, "state")); err != nil {
		t.Fatalf("cascade-nudge-on-blocker-close.sh failed: %v\n%s", err, out)
	}
	return countNudges(t, logPath)
}

// A dependent another open blocker still holds is not nudged: its assignee
// has nothing to do yet, and a nudge would wake an asleep seat parked on it
// for nothing (ga-ylpbsy.16, RCA R4). The seat wakes on its own once the
// dependent is ready.
func TestCascadeNudgeSkipsADependentStillBlocked(t *testing.T) {
	for _, status := range []string{"open", "in_progress", "blocked", "deferred", "hooked"} {
		t.Run(status, func(t *testing.T) {
			down := `printf '%s\n' '[{"id":"ga-blk","status":"closed"},{"id":"ga-other","status":"` + status + `"}]'`
			if got, logged := cascadeNudgeOnce(t, down); got != 0 {
				t.Fatalf("nudges = %d, want 0 while ga-other is %s; gc calls:\n%s", got, status, logged)
			}
		})
	}
}

// The close of a dependent's last open blocker nudges its assignee once.
func TestCascadeNudgeNudgesOnTheLastBlockersClose(t *testing.T) {
	for name, down := range map[string]string{
		"every blocker closed":                      `printf '%s\n' '[{"id":"ga-blk","status":"closed"},{"id":"ga-other","status":"closed"}]'`,
		"the other blocker pinned (bd: satisfied)":  `printf '%s\n' '[{"id":"ga-blk","status":"closed"},{"id":"ga-other","status":"pinned"}]'`,
		"the closing blocker read before its close": `printf '%s\n' '[{"id":"ga-blk","status":"open"}]'`,
		"no blocker listed":                         `printf '%s\n' '[]'`,
		// A read that fails proves nothing: the nudge goes out, as before.
		"a failed blocker read":       `exit 1`,
		"an unparseable blocker read": `printf 'not json\n'`,
	} {
		t.Run(name, func(t *testing.T) {
			got, logged := cascadeNudgeOnce(t, down)
			if got != 1 || !strings.Contains(logged, "gc session nudge worker-1 blocker ga-blk closed") {
				t.Fatalf("nudges = %d, want one to worker-1; gc calls:\n%s", got, logged)
			}
		})
	}
}

// A still-blocked pair is recorded: the lookback's later runs see the same
// close again and must not read the dependent's blockers again.
func TestCascadeNudgeRecordsAStillBlockedPair(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; cascade-nudge-on-blocker-close.sh requires it")
	}
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	event := `{"seq":7,"type":"bead.closed","actor":"gc","subject":"ga-blk","payload":{"bead":{"id":"ga-blk","status":"closed"}}}`
	if err := os.WriteFile(eventsPath, []byte(event+"\n"), 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}
	binDir, logPath := fakeGCBin(t, `case "$1" in
events) cat '`+eventsPath+`' ;;
bd) case "$*" in
    *direction=down*) printf '%s\n' '[{"id":"ga-blk","status":"closed"},{"id":"ga-other","status":"open"}]' ;;
    *) printf '%s\n' '[{"id":"ga-dep","status":"open","assignee":"worker-1"}]' ;;
    esac ;;
esac
exit 0
`)
	stateDir := filepath.Join(dir, "state")
	for i := range 2 {
		if out, err := runCascadeNudge(t, binDir, stateDir); err != nil {
			t.Fatalf("run %d: cascade-nudge-on-blocker-close.sh failed: %v\n%s", i+1, err, out)
		}
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(logged), "--direction=down"); got != 1 {
		t.Fatalf("blocker reads = %d, want 1 across two runs; gc calls:\n%s", got, logged)
	}
	if n, _ := countNudges(t, logPath); n != 0 {
		t.Fatalf("nudges = %d, want 0", n)
	}
}
