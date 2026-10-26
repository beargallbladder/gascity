package coordclass

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func strp(s string) *string { return &s }

// workHolds is what a work store holds on a city that serves every
// infrastructure class from a binding; infraHolds is what that binding holds.
var (
	workHolds  = []Class{ClassWork}
	infraHolds = []Class{ClassGraph, ClassMessaging, ClassSessions, ClassOrders, ClassNudges}
)

// TestCheckFlipRefusesAWriteThatLeavesTheStoresClasses is the guard's table:
// every way the audit flipped a work bead in place, the reverse flip inside a
// binding, and the writes that must stay allowed.
func TestCheckFlipRefusesAWriteThatLeavesTheStoresClasses(t *testing.T) {
	task := beads.Bead{ID: "demo-1", Type: "task"}
	graphStep := beads.Bead{ID: "gcg-1", Type: "task", Metadata: map[string]string{"gc.root_bead_id": "gcg-0"}}
	syntheticConvoy := beads.Bead{ID: "demo-2", Type: "convoy", Metadata: map[string]string{"gc.synthetic": "true", "gc.root_bead_id": "gcg-0"}}
	stranded := beads.Bead{ID: "demo-3", Type: "task", Labels: []string{"gc:nudge"}}

	cases := []struct {
		name   string
		before beads.Bead
		m      Mutation
		holds  []Class
		want   Class // the refused After class; ClassWork with refuse=false means allowed
		refuse bool
	}{
		{"metadata gc.root_bead_id", task, Mutation{SetMetadata: map[string]string{"gc.root_bead_id": "gcg-1"}}, workHolds, ClassGraph, true},
		{"metadata gc.kind=wisp", task, Mutation{SetMetadata: map[string]string{"gc.kind": "wisp"}}, workHolds, ClassGraph, true},
		{"metadata gc.kind=workflow", task, Mutation{SetMetadata: map[string]string{"gc.kind": "workflow"}}, workHolds, ClassGraph, true},
		{"metadata gc.formula_contract=graph.v2", task, Mutation{SetMetadata: map[string]string{"gc.formula_contract": "graph.v2"}}, workHolds, ClassGraph, true},
		{"label wisp", task, Mutation{AddLabels: []string{"wisp"}}, workHolds, ClassGraph, true},
		{"label gc:wisp", task, Mutation{AddLabels: []string{"gc:wisp"}}, workHolds, ClassGraph, true},
		{"label gc:session", task, Mutation{AddLabels: []string{"gc:session"}}, workHolds, ClassSessions, true},
		{"label gc:wait", task, Mutation{AddLabels: []string{"gc:wait"}}, workHolds, ClassSessions, true},
		{"label gc:rt", task, Mutation{AddLabels: []string{"gc:rt"}}, workHolds, ClassSessions, true},
		{"label gc:fact", task, Mutation{AddLabels: []string{"gc:fact"}}, workHolds, ClassSessions, true},
		{"label gc:op", task, Mutation{AddLabels: []string{"gc:op"}}, workHolds, ClassSessions, true},
		{"label gc:nudge", task, Mutation{AddLabels: []string{"gc:nudge"}}, workHolds, ClassNudges, true},
		{"label order-tracking", task, Mutation{AddLabels: []string{"order-tracking"}}, workHolds, ClassOrders, true},
		{"label gc:extmsg-binding", task, Mutation{AddLabels: []string{"gc:extmsg-binding"}}, workHolds, ClassMessaging, true},
		{"set-labels to a session", task, Mutation{SetLabels: []string{"gc:session"}, ReplaceLabels: true}, workHolds, ClassSessions, true},
		{"type message", task, Mutation{Type: strp("message")}, workHolds, ClassMessaging, true},
		{"type session", task, Mutation{Type: strp("session")}, workHolds, ClassSessions, true},
		{"type wisp", task, Mutation{Type: strp("wisp")}, workHolds, ClassGraph, true},
		{"type convergence", task, Mutation{Type: strp("convergence")}, workHolds, ClassGraph, true},
		{"type op", task, Mutation{Type: strp("op")}, workHolds, ClassSessions, true},
		// A removal flips too: a synthetic convoy is work only while it is synthetic.
		{"unset gc.synthetic on a graph-marked convoy", syntheticConvoy, Mutation{UnsetMetadata: []string{"gc.synthetic"}}, workHolds, ClassGraph, true},
		{"blank gc.root_bead_id is not a graph marker", task, Mutation{SetMetadata: map[string]string{"gc.root_bead_id": "  "}}, workHolds, ClassWork, false},

		// The reverse, inside a binding.
		{"binding: clear the only graph marker", graphStep, Mutation{UnsetMetadata: []string{"gc.root_bead_id"}}, infraHolds, ClassWork, true},
		{"binding: blank the only graph marker", graphStep, Mutation{SetMetadata: map[string]string{"gc.root_bead_id": ""}}, infraHolds, ClassWork, true},
		{"binding: graph to sessions stays in the binding", graphStep, Mutation{AddLabels: []string{"gc:session"}}, infraHolds, ClassSessions, false},
		{"binding: ordinary metadata", graphStep, Mutation{SetMetadata: map[string]string{"gc.outcome": "pass"}}, infraHolds, ClassGraph, false},

		// Allowed in the work store.
		{"ordinary label", task, Mutation{AddLabels: []string{"triage"}}, workHolds, ClassWork, false},
		{"ordinary metadata", task, Mutation{SetMetadata: map[string]string{"gc.outcome": "pass"}}, workHolds, ClassWork, false},
		{"type bug", task, Mutation{Type: strp("bug")}, workHolds, ClassWork, false},
		{"convoy type stays work", task, Mutation{Type: strp("convoy")}, workHolds, ClassWork, false},
		// A stranded bead may be written without moving class, and may be
		// repaired back into the class its store holds.
		{"stranded: unrelated write", stranded, Mutation{SetMetadata: map[string]string{"note": "x"}}, workHolds, ClassNudges, false},
		{"stranded: repair to work", stranded, Mutation{RemoveLabels: []string{"gc:nudge"}}, workHolds, ClassWork, false},

		// A store that holds every class — a single-store city — never refuses.
		{"single store: nil holds", task, Mutation{AddLabels: []string{"gc:session"}}, nil, ClassSessions, false},
		{"single store: every class", task, Mutation{AddLabels: []string{"wisp"}}, Classes(), ClassGraph, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			after := c.m.Apply(c.before)
			if got := Classify(after); got != c.want {
				t.Fatalf("Classify(after) = %s, want %s (after=%+v)", got, c.want, after)
			}
			flip := CheckFlip(c.before, after, c.holds)
			if c.refuse != (flip != nil) {
				t.Fatalf("CheckFlip refused=%v, want %v (flip=%v)", flip != nil, c.refuse, flip)
			}
			if flip == nil {
				return
			}
			if flip.Before != Classify(c.before) || flip.After != c.want {
				t.Errorf("flip = %s -> %s, want %s -> %s", flip.Before, flip.After, Classify(c.before), c.want)
			}
			msg := flip.Error()
			for _, want := range []string{flip.Before.String(), flip.After.String()} {
				if !strings.Contains(msg, want) {
					t.Errorf("flip message %q does not name class %q", msg, want)
				}
			}
			if cmd := MintCommand(flip.After); cmd == "" || !strings.Contains(flip.Guidance(), cmd) {
				t.Errorf("guidance %q does not name the command %q that makes a %s bead", flip.Guidance(), cmd, flip.After)
			}
		})
	}
}

// TestMutationApplyDoesNotAliasTheBefore pins that computing the after shape
// never rewrites the caller's bead: the before has to stay the before.
func TestMutationApplyDoesNotAliasTheBefore(t *testing.T) {
	before := beads.Bead{Type: "task", Labels: []string{"a", "b"}, Metadata: map[string]string{"k": "v"}}
	m := Mutation{
		Type:          strp("bug"),
		AddLabels:     []string{"c"},
		RemoveLabels:  []string{"a"},
		SetMetadata:   map[string]string{"k2": "v2"},
		UnsetMetadata: []string{"k"},
	}
	after := m.Apply(before)
	if before.Type != "task" || len(before.Labels) != 2 || before.Labels[0] != "a" || before.Metadata["k"] != "v" || len(before.Metadata) != 1 {
		t.Fatalf("Apply mutated the before: %+v", before)
	}
	if after.Type != "bug" || strings.Join(after.Labels, ",") != "b,c" || after.Metadata["k2"] != "v2" || after.Metadata["k"] != "" {
		t.Fatalf("after = %+v", after)
	}
	if !m.TouchesClass() || (Mutation{}).TouchesClass() {
		t.Fatalf("TouchesClass: %v for a type write, %v for an empty one", m.TouchesClass(), (Mutation{}).TouchesClass())
	}
}

// TestTouchesClassIsExact pins TouchesClass against Classify. Over every
// before shape and every single-field write drawn from a pool that holds every
// classifier label and key and as many that are not: a write TouchesClass calls
// inert never changes the class, and every classifier label and key is called
// live (so a new Classify arm that reads a new label or key, and is not taught
// to TouchesClass, fails the first half for the shape that exposes it).
func TestTouchesClassIsExact(t *testing.T) {
	labels := []string{
		"wisp", "gc:wisp", "order-tracking", "gc:session", "gc:wait", "gc:op", "gc:fact", "gc:rt",
		"gc:nudge", "gc:nudge-queue", "gc:extmsg-binding", "gc:extmsg-transcript",
		"triage", "session:gc-7", "gc:rt-conv:gc-7", "gc:op-conv:gc-7", "order-run:x", "provides:cap", "gc:other", "Wisp",
	}
	keys := []string{
		"gc.kind", "gc.formula_contract", "gc.root_bead_id", "gc.synthetic", "gc.synthetic_kind",
		"gc.outcome", "gc.output_json", "session_id", "gc.routed_to", "molecule_id", "gc.step_id",
	}
	values := []string{"", "wisp", "workflow", "graph.v2", "gcg-1", "true", "x"}
	types := []string{"task", "convoy", "molecule", "message", "session", "op", "convergence", "gate", "wisp"}

	var befores []beads.Bead
	for _, typ := range types {
		befores = append(befores, beads.Bead{Type: typ})
		for _, l := range labels {
			befores = append(befores, beads.Bead{Type: typ, Labels: []string{l}})
		}
		for _, k := range keys {
			for _, v := range values {
				befores = append(befores, beads.Bead{Type: typ, Metadata: map[string]string{k: v}})
				befores = append(befores, beads.Bead{Type: typ, Labels: []string{"order-tracking"}, Metadata: map[string]string{k: v, "gc.root_bead_id": "gcg-1"}})
				befores = append(befores, beads.Bead{Type: typ, Metadata: map[string]string{k: v, "gc.synthetic": "true", "gc.root_bead_id": "gcg-1"}})
			}
		}
	}
	var writes []Mutation
	for _, l := range labels {
		writes = append(writes, Mutation{AddLabels: []string{l}}, Mutation{RemoveLabels: []string{l}})
	}
	for _, k := range keys {
		writes = append(writes, Mutation{UnsetMetadata: []string{k}})
		for _, v := range values {
			writes = append(writes, Mutation{SetMetadata: map[string]string{k: v}})
		}
	}
	for _, m := range writes {
		if m.TouchesClass() {
			continue
		}
		for _, b := range befores {
			if before, after := Classify(b), Classify(m.Apply(b)); before != after {
				t.Fatalf("TouchesClass calls %+v inert, but it moves %+v from %s to %s", m, b, before, after)
			}
		}
	}
	for _, l := range labels[:12] {
		if !(Mutation{AddLabels: []string{l}}).TouchesClass() {
			t.Errorf("classifier label %q is not live", l)
		}
	}
	for _, k := range keys[:5] {
		if !(Mutation{UnsetMetadata: []string{k}}).TouchesClass() {
			t.Errorf("classifier key %q is not live", k)
		}
	}
}

// TestMintCommandNamesEveryClass keeps a refusal from ever pointing at nothing.
func TestMintCommandNamesEveryClass(t *testing.T) {
	for _, c := range Classes() {
		if MintCommand(c) == "" {
			t.Errorf("MintCommand(%s) is empty", c)
		}
	}
}
