//go:build integration

package main

// The split-city allowlist's one formula-verb admission, against a real bd.
// `gc bd mol wisp <formula> --root-only` is forwarded on a split city because
// the gastown patrols depend on it, and the in-process judge rests on one fact
// about bd: with --root-only bd mints exactly one bead, the molecule root, and
// the formula's step types, labels and metadata do not reach it. Only bd itself
// can show that, so this test seeds a temporary workspace (embedded Dolt), pours
// a formula whose steps are infrastructure-shaped, and classifies what bd wrote.
// It also shows the shape the allowlist refuses: without --root-only the same
// formula mints the infrastructure-class steps.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

func TestGcBdMolWispRootOnlyMintsOneWorkBead(t *testing.T) {
	bd := bazeltest.DataPath(t, "GC_TEST_PINNED_BD_BIN")
	if bd == "" {
		found, err := findPreferredBinary("bd", strings.TrimSpace(os.Getenv("GC_TEST_BD_BIN")))
		if err != nil {
			t.Skip("bd not found (set GC_TEST_BD_BIN or install bd)")
		}
		bd = found
	}
	configureTestDoltIdentityEnv(t)
	ws := t.TempDir()
	run := beads.ExecCommandRunnerWithEnvWithoutAmbientBeads(map[string]string{
		"BD_BIN":             bd,
		"BEADS_DIR":          filepath.Join(ws, ".beads"),
		"BD_NON_INTERACTIVE": "1",
		"BD_BACKUP_ENABLED":  "false",
		"BD_EXPORT_AUTO":     "false",
		"BEADS_ACTOR":        "gc-test",
	})
	must := func(args ...string) []byte {
		t.Helper()
		out, err := run(ws, "bd", args...)
		if err != nil {
			t.Fatalf("bd %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	if out, err := run(ws, "git", "init", "-q", "."); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	must("init", "-p", "tt", "--quiet", "--skip-agents", "--skip-hooks")
	formulas := filepath.Join(ws, ".beads", "formulas")
	if err := os.MkdirAll(formulas, 0o755); err != nil {
		t.Fatal(err)
	}
	formula := `formula = "mol-strand"
version = 1

[vars.label]
default = "triage"

[[steps]]
id = "session"
title = "a label-only infra step"
labels = ["gc:session"]

[[steps]]
id = "graph"
title = "a metadata-only infra step"
[steps.metadata]
"gc.root_bead_id" = "gcg-1"
`
	if err := os.WriteFile(filepath.Join(formulas, "mol-strand.formula.toml"), []byte(formula), 0o644); err != nil {
		t.Fatal(err)
	}

	argv := []string{"mol", "wisp", "mol-strand", "--root-only", "--var", "label=gc:session", "--json"}
	notFound := bdClassGuardDeps{read: func(string) (beads.Bead, bdReadResult) { return beads.Bead{}, bdReadNotFound }}
	if g := bdClassFlipGuard(splitGuardTestCity(), ws, argv, notFound); g.refusal != "" {
		t.Fatalf("the allowlist refused the patrol shape: %s", g.refusal)
	}
	var created struct {
		NewEpicID string            `json:"new_epic_id"`
		IDMapping map[string]string `json:"id_mapping"`
	}
	if err := json.Unmarshal(must(argv...), &created); err != nil || created.NewEpicID == "" {
		t.Fatalf("decoding the wisp result: %v (%+v)", err, created)
	}
	if len(created.IDMapping) > 1 {
		t.Fatalf("a root-only wisp minted %d beads: %v", len(created.IDMapping), created.IDMapping)
	}
	root := realBdShow(t, must, created.NewEpicID)
	if class := coordclass.Classify(root); class != coordclass.ClassWork {
		t.Fatalf("the root-only wisp root %s is %s class (type %q, labels %v, metadata %v)", root.ID, class, root.Type, root.Labels, root.Metadata)
	}

	// Without --root-only, bd mints the infra-shaped steps: why it is refused.
	full := []string{"mol", "wisp", "mol-strand", "--json"}
	if g := bdClassFlipGuard(splitGuardTestCity(), ws, full, notFound); g.refusal == "" {
		t.Fatal("the allowlist forwarded a full wisp")
	}
	if err := json.Unmarshal(must(full...), &created); err != nil {
		t.Fatal(err)
	}
	infra := 0
	for _, id := range created.IDMapping {
		if coordclass.Classify(realBdShow(t, must, id)) != coordclass.ClassWork {
			infra++
		}
	}
	if infra != 2 {
		t.Fatalf("a full wisp minted %d infrastructure-class beads, want the 2 infra steps (mapping %v)", infra, created.IDMapping)
	}
}

// realBdShow reads one bead back from a real bd.
func realBdShow(t *testing.T, must func(args ...string) []byte, id string) beads.Bead {
	t.Helper()
	out := must("show", id, "--json")
	var rows []struct {
		ID       string           `json:"id"`
		Type     string           `json:"issue_type"`
		Labels   []string         `json:"labels"`
		Metadata *beads.StringMap `json:"metadata"`
	}
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) != 1 {
		var row struct {
			ID       string           `json:"id"`
			Type     string           `json:"issue_type"`
			Labels   []string         `json:"labels"`
			Metadata *beads.StringMap `json:"metadata"`
		}
		if err2 := json.Unmarshal(out, &row); err2 != nil {
			t.Fatalf("decoding bd show %s: %v / %v\n%s", id, err, err2, out)
		}
		rows = append(rows[:0], row)
	}
	b := beads.Bead{ID: rows[0].ID, Type: rows[0].Type, Labels: rows[0].Labels}
	if rows[0].Metadata != nil {
		b.Metadata = map[string]string(*rows[0].Metadata)
	}
	return b
}
