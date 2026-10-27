package main

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// splitGuardTestCity is a whole-split city: every infrastructure class on one
// binding, work on the work stores.
func splitGuardTestCity() *config.City {
	return &config.City{Storage: &config.StorageConfig{
		Classes: config.StorageClasses{
			Work: config.StorageWorkBinding, Graph: "infra", Sessions: "infra",
			Messaging: "infra", Orders: "infra", Nudges: "infra",
		},
		Bindings: map[string]config.StorageBindingConfig{"infra": {Provider: config.StorageProviderSQLiteBeads}},
	}}
}

// TestGcBdSplitAllowlistDecidesEveryCommand is the allowlist's coverage test
// over the whole pinned bd inventory (internal/bdflags/commands_gen.go): every
// command path is either allowlisted with its vetted flag set or refused with a
// stated alternative, and every alias is either mapped onto an allowlisted path
// or refused. A command or alias bd adds reaches the inventory through
// TestBdCommandInventoryCurrent (integration) and then fails here until it has
// a decision. The decisions are checked by behavior, through
// resolveBdSplitCommand, not just by table membership.
func TestGcBdSplitAllowlistDecidesEveryCommand(t *testing.T) {
	inventory := map[string]bool{"release-if-current": true}
	for _, p := range bdflags.CommandPaths() {
		inventory[p] = true
	}
	for path := range bdSplitAllowlist {
		if !inventory[path] {
			t.Errorf("allowlisted %q is not a command path in the inventory", path)
		}
	}
	for key := range bdSplitRefusals {
		if !inventory[key] {
			t.Errorf("refusal %q is not a command path in the inventory", key)
		}
		if _, allowed := bdSplitAllowlist[key]; allowed {
			t.Errorf("%q is both allowlisted and refused", key)
		}
	}
	paths := slices.Sorted(maps.Keys(inventory))
	for _, path := range paths {
		cmd, allowed := bdSplitAllowlist[path]
		if !allowed {
			r, decided := bdSplitRefusalFor(path)
			if !decided {
				t.Errorf("command %q has no split-city decision: allowlist it (with its flag set) or add a refusal with an alternative", path)
				continue
			}
			if strings.TrimSpace(r.why) == "" || strings.TrimSpace(r.alt) == "" {
				t.Errorf("refusal of %q states no reason or no alternative", path)
			}
			res := resolveBdSplitCommand(strings.Fields(path), "/scope")
			if res.refusal == "" || !strings.Contains(res.refusal, path) {
				t.Errorf("refused %q resolved without a refusal naming it: %+v", path, res)
			}
			continue
		}
		if strings.TrimSpace(cmd.why) == "" {
			t.Errorf("allowlisted %q states no reason", path)
		}
		// Every vetted flag of the command is a flag bd declares for it.
		declared := map[string]bool{}
		for _, f := range bdflags.CommandFlagSpecs(path) {
			declared[f.Long] = true
		}
		for _, f := range append(slices.Clone(cmd.only), cmd.exclude...) {
			if !declared[f] {
				t.Errorf("allowlisted %q vets %s, which bd does not declare for it", path, f)
			}
		}
		if path == "release-if-current" {
			continue
		}
		res := resolveBdSplitCommand(strings.Fields(path), "/scope")
		if res.refusal != "" || res.path != path {
			t.Errorf("allowlisted %q did not resolve to itself: path=%q refusal=%q", path, res.path, res.refusal)
		}
		// Each vetted flag parses; each unvetted one of the command is refused.
		for _, f := range bdflags.CommandFlagSpecs(path) {
			argv := append(strings.Fields(path), f.Long)
			if f.TakesValue {
				argv = append(argv, "v")
			}
			vetted := (cmd.only == nil || slices.Contains(cmd.only, f.Long)) && !slices.Contains(cmd.exclude, f.Long)
			got := resolveBdSplitCommand(argv, "/scope")
			if vetted && got.refusal != "" {
				t.Errorf("vetted flag %s of %q was refused: %s", f.Long, path, got.refusal)
			}
			if !vetted && got.refusal == "" {
				t.Errorf("unvetted flag %s of %q was accepted", f.Long, path)
			}
		}
	}
	for alias, target := range bdflags.CommandAliases() {
		mapped, allowed := bdSplitAllowedAliases[alias]
		_, refused := bdSplitRefusedAliases[alias]
		switch {
		case allowed && refused:
			t.Errorf("alias %q is both allowed and refused", alias)
		case allowed:
			if mapped != target {
				t.Errorf("alias %q maps to %q, but bd runs it as %q", alias, mapped, target)
			}
			if _, ok := bdSplitAllowlist[mapped]; !ok {
				t.Errorf("alias %q maps to %q, which is not allowlisted", alias, mapped)
			}
			if res := resolveBdSplitCommand(strings.Fields(alias), "/scope"); res.refusal != "" || res.path != target {
				t.Errorf("allowed alias %q resolved to %q (%q)", alias, res.path, res.refusal)
			}
		case refused:
			if res := resolveBdSplitCommand(strings.Fields(alias), "/scope"); res.refusal == "" {
				t.Errorf("refused alias %q resolved to %q", alias, res.path)
			}
		default:
			t.Errorf("alias %q (of %q) has no split-city decision", alias, target)
		}
	}
	for alias := range bdSplitAllowedAliases {
		if _, ok := bdflags.CommandAliases()[alias]; !ok {
			t.Errorf("allowed alias %q is not in the inventory", alias)
		}
	}
	for alias := range bdSplitRefusedAliases {
		if _, ok := bdflags.CommandAliases()[alias]; !ok {
			t.Errorf("refused alias %q is not in the inventory", alias)
		}
	}
}

// TestGcBdSingleStoreForwardsEveryCommandUnchanged pins the single-store
// contract over the whole inventory: no command, alias or flag is refused or
// rewritten (beyond gc's own --skip-infra on import).
func TestGcBdSingleStoreForwardsEveryCommandUnchanged(t *testing.T) {
	cfgs := map[string]*config.City{"no storage": {}, "all on work": {Storage: &config.StorageConfig{Classes: config.StorageClasses{
		Work: config.StorageWorkBinding, Graph: config.StorageWorkBinding, Sessions: config.StorageWorkBinding,
		Messaging: config.StorageWorkBinding, Orders: config.StorageWorkBinding, Nudges: config.StorageWorkBinding,
	}}}}
	argvs := [][]string{{"mol", "fart", "a", "b"}, {"backup", "restore", "/b", "--force=true"}, {"--db", "x", "update", "a", "--type", "message"}, {"sql", "/* */ update issues set issue_type='message'"}}
	for _, p := range bdflags.CommandPaths() {
		argvs = append(argvs, append(strings.Fields(p), "--some-unknown-flag"))
	}
	for alias := range bdflags.CommandAliases() {
		argvs = append(argvs, strings.Fields(alias))
	}
	for name, cfg := range cfgs {
		for _, argv := range argvs {
			g := bdClassFlipGuard(cfg, t.TempDir(), argv, bdClassGuardDeps{read: func(string) (beads.Bead, bdReadResult) {
				t.Fatalf("%s: a single-store city read a bead for %v", name, argv)
				return beads.Bead{}, bdReadFailed
			}})
			if g.refusal != "" || strings.Join(g.args, "\x00") != strings.Join(argv, "\x00") {
				t.Fatalf("%s: gc bd %s was changed: refusal=%q args=%v", name, strings.Join(argv, " "), g.refusal, g.args)
			}
		}
	}
}

// TestGcBdSplitAllowlistIsDocumented keeps the "gc bd on split cities" docs
// section in step with the allowlist.
func TestGcBdSplitAllowlistIsDocumented(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "runbooks", "split-storage-classes.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(doc), "## gc bd on split cities")
	if !ok {
		t.Fatal(`docs/runbooks/split-storage-classes.md has no "## gc bd on split cities" section`)
	}
	if next := strings.Index(section, "\n## "); next >= 0 {
		section = section[:next]
	}
	for _, path := range bdSplitAllowlistPaths() {
		if !strings.Contains(section, "`"+path+"`") {
			t.Errorf("the docs section does not list allowlisted `%s`", path)
		}
	}
	for alias := range bdSplitAllowedAliases {
		if !strings.Contains(section, "`"+alias+"`") {
			t.Errorf("the docs section does not list allowed alias `%s`", alias)
		}
	}
}

// TestStoreClassHoldsIsNilOnASingleStoreCity pins the single-store contract
// every CLI door relies on: no [storage], or one that keeps every class on the
// work binding, holds nil, and nil disarms coordclass.CheckFlip.
func TestStoreClassHoldsIsNilOnASingleStoreCity(t *testing.T) {
	allWork := &config.City{Storage: &config.StorageConfig{Classes: config.StorageClasses{
		Work: config.StorageWorkBinding, Graph: config.StorageWorkBinding, Sessions: config.StorageWorkBinding,
		Messaging: config.StorageWorkBinding, Orders: config.StorageWorkBinding, Nudges: config.StorageWorkBinding,
	}}}
	for name, cfg := range map[string]*config.City{"nil": nil, "no storage": {}, "all on work": allWork} {
		if got := workStoreClassHolds(cfg); got != nil {
			t.Errorf("%s: work store holds %v, want nil", name, got)
		}
	}
	if got := workStoreClassHolds(splitGuardTestCity()); len(got) != 1 || got[0] != coordclass.ClassWork {
		t.Errorf("split: work store holds %v, want [work]", got)
	}
	if got := storeClassHolds(splitGuardTestCity(), "infra"); len(got) != 5 {
		t.Errorf("split: binding holds %v, want the five infrastructure classes", got)
	}
}

// TestGcBdImportSkipInfraWritesAWorkOnlyCopy checks what --skip-infra hands
// bd, line by line, for a file and for stdin.
func TestGcBdImportSkipInfraWritesAWorkOnlyCopy(t *testing.T) {
	archive := `{"_type":"memory","key":"k","value":"v"}` + "\n" +
		`{"id":"demo-1","title":"w","issue_type":"task","labels":["triage"]}` + "\n" +
		`{"id":"demo-2","title":"m","issue_type":"message"}` + "\n" +
		`{"id":"demo-3","title":"wisp","issue_type":"task","metadata":{"gc.kind":"wisp"}}` + "\n"
	scope := t.TempDir()
	if err := os.WriteFile(filepath.Join(scope, "a.jsonl"), []byte(archive), 0o644); err != nil {
		t.Fatal(err)
	}
	read := func(string) (beads.Bead, bdReadResult) { return beads.Bead{}, bdReadNotFound }

	for _, argv := range [][]string{{"import", "a.jsonl", "--skip-infra"}, {"import", "--skip-infra", "-i", "a.jsonl"}, {"import", "--input=a.jsonl", "--skip-infra"}} {
		g := bdClassFlipGuard(splitGuardTestCity(), scope, argv, bdClassGuardDeps{read: read})
		if g.refusal != "" {
			t.Fatalf("%v refused: %s", argv, g.refusal)
		}
		if slicesContainsString(g.args, "--skip-infra") || slicesContainsString(g.args, "a.jsonl") || slicesContainsString(g.args, "--input=a.jsonl") {
			t.Fatalf("%v forwarded %v; want the work-only copy and no gc flag", argv, g.args)
		}
		var copyPath string
		for _, a := range g.args {
			if i := strings.Index(a, "gc-bd-import-work-"); i >= 0 {
				copyPath = strings.TrimPrefix(a, "--input=")
			}
		}
		data, err := os.ReadFile(copyPath)
		if err != nil {
			t.Fatalf("%v: reading the work-only copy %q: %v", argv, copyPath, err)
		}
		got := string(data)
		if !strings.Contains(got, `"_type":"memory"`) || !strings.Contains(got, `"demo-1"`) || strings.Contains(got, `"demo-2"`) || strings.Contains(got, `"demo-3"`) {
			t.Fatalf("%v: work-only copy = %q", argv, got)
		}
		g.cleanup()
		if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
			t.Errorf("%v: the work-only copy outlived the command", argv)
		}
	}

	g := bdClassFlipGuard(splitGuardTestCity(), scope, []string{"import", "-", "--skip-infra"}, bdClassGuardDeps{stdin: bytes.NewBufferString(archive), read: read})
	if g.refusal != "" || g.stdin == nil {
		t.Fatalf("stdin import: refusal=%q stdin=%v", g.refusal, g.stdin)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(g.stdin); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, `"demo-1"`) || strings.Contains(got, `"demo-2"`) || strings.Contains(got, `"demo-3"`) {
		t.Fatalf("stdin import handed bd %q", got)
	}
	if strings.Join(g.args, " ") != "import -" {
		t.Fatalf("stdin import forwarded %v, want [import -]", g.args)
	}
}

func slicesContainsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
