package bdflags

import (
	"reflect"
	"strings"
	"testing"
)

var parseSpecs = []FlagSpec{
	{"--type", "-t", true},
	{"--assignee", "-a", true},
	{"--dry-run", "", false},
	{"--quiet", "-q", false},
	{"--verbose", "-v", false},
	{"--force", "-f", false},
	{"--persist", "", false},
	{"--prefix", "", true},
}

// TestParseArgsMatchesPflag pins the parser against pflag's rules, including
// the shapes the review reproduced: a value flag consumes the next token even
// when it spells a flag, and a bool takes =true/=false.
func TestParseArgsMatchesPflag(t *testing.T) {
	for _, c := range []struct {
		name  string
		args  []string
		flags []ParsedFlag
		pos   []string
		err   string
	}{
		{"value consumes a flag-shaped token", []string{"--assignee", "--dry-run", "x"}, []ParsedFlag{{"--assignee", "--dry-run", false}}, []string{"x"}, ""},
		{"prefix consumes --dry-run", []string{"f", "--prefix", "--dry-run", "--persist"}, []ParsedFlag{{"--prefix", "--dry-run", false}, {"--persist", "true", true}}, []string{"f"}, ""},
		{"bool =true", []string{"--persist=true"}, []ParsedFlag{{"--persist", "true", true}}, nil, ""},
		{"bool =false", []string{"--force=false", "id"}, []ParsedFlag{{"--force", "false", true}}, []string{"id"}, ""},
		{"bool =1 normalizes", []string{"--force=1"}, []ParsedFlag{{"--force", "true", true}}, nil, ""},
		{"bad bool", []string{"--force=maybe"}, nil, nil, "invalid argument"},
		{"inline value", []string{"--type=message"}, []ParsedFlag{{"--type", "message", false}}, nil, ""},
		{"attached shorthand", []string{"-tmessage"}, []ParsedFlag{{"--type", "message", false}}, nil, ""},
		{"shorthand =", []string{"-t=message"}, []ParsedFlag{{"--type", "message", false}}, nil, ""},
		{"separated shorthand", []string{"-t", "message"}, []ParsedFlag{{"--type", "message", false}}, nil, ""},
		{"combined bools then value", []string{"-qvtmessage"}, []ParsedFlag{{"--quiet", "true", true}, {"--verbose", "true", true}, {"--type", "message", false}}, nil, ""},
		{"combined bools then separated value", []string{"-qt", "message"}, []ParsedFlag{{"--quiet", "true", true}, {"--type", "message", false}}, nil, ""},
		{"short bool =false", []string{"-f=false"}, []ParsedFlag{{"--force", "false", true}}, nil, ""},
		{"interspersed", []string{"a", "-q", "b"}, []ParsedFlag{{"--quiet", "true", true}}, []string{"a", "b"}, ""},
		{"double dash", []string{"a", "--", "--type", "-q"}, nil, []string{"a", "--type", "-q"}, ""},
		{"lone dash is positional", []string{"-"}, nil, []string{"-"}, ""},
		{"unknown long", []string{"--nope"}, nil, nil, "unknown flag"},
		{"unknown short", []string{"-z"}, nil, nil, "unknown shorthand"},
		{"unknown in a cluster", []string{"-qz"}, nil, nil, "unknown shorthand"},
		{"missing value", []string{"--type"}, nil, nil, "needs an argument"},
		{"bad syntax", []string{"---type"}, nil, nil, "bad flag syntax"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseArgs(parseSpecs, c.args)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Flags, c.flags) || !reflect.DeepEqual(got.Positionals, c.pos) {
				t.Fatalf("got flags=%v pos=%v, want flags=%v pos=%v", got.Flags, got.Positionals, c.flags, c.pos)
			}
		})
	}
	p, _ := ParseArgs(parseSpecs, []string{"--persist", "--persist=false"})
	if v, set := p.BoolValue("--persist"); v || !set {
		t.Fatalf("last bool occurrence must win: %v %v", v, set)
	}
}

// TestCommandInventoryIsConsistent ties the generated command inventory to the
// hand-pinned top-level verb list and alias map.
func TestCommandInventoryIsConsistent(t *testing.T) {
	verbs := map[string]bool{}
	for _, v := range TopLevelVerbs() {
		verbs[v] = true
	}
	hidden := map[string]bool{"codex-hook": true, "cursor-hook": true, "db-proxy-child": true, "send-metrics": true}
	top := map[string]bool{}
	for _, p := range CommandPaths() {
		v, _, _ := strings.Cut(p, " ")
		top[v] = true
		if !verbs[v] {
			t.Errorf("inventory path %q has a top-level verb TopLevelVerbs lacks", p)
		}
		if !IsCommandPath(p) {
			t.Errorf("path %q has no flag entry", p)
		}
	}
	for v := range verbs {
		if !top[v] && !hidden[v] {
			t.Errorf("TopLevelVerbs has %q, which the command inventory lacks", v)
		}
	}
	for alias, target := range CommandAliases() {
		if !IsCommandPath(target) {
			t.Errorf("alias %q names unknown path %q", alias, target)
		}
		if !strings.Contains(alias, " ") && TopLevelAliases()[alias] != target {
			t.Errorf("top-level alias %q → %q disagrees with TopLevelAliases (%q)", alias, target, TopLevelAliases()[alias])
		}
	}
	for alias, target := range TopLevelAliases() {
		if CommandAliases()[alias] != target {
			t.Errorf("TopLevelAliases %q → %q is missing from CommandAliases", alias, target)
		}
	}
}
