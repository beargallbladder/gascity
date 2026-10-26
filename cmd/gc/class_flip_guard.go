package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// This file is cmd/gc's side of the one class-must-not-flip guard
// (coordclass.CheckFlip). On a split city the work stores hold only work beads
// and the class binding only its infrastructure classes. A write that changes
// a bead's class IN PLACE strands it in a store its class is never read from,
// and one stranded row in the city work store makes every later routed gc
// command refuse until the fleet is stopped and the row repaired. Every door
// that lets a caller write a bead's type, labels or metadata states that write
// as a coordclass.Mutation and asks CheckFlip. The doors, each with its own
// table-driven test:
//
//   - gc bd (doBd → bdClassFlipGuard): update, label, tag, set-state, import,
//     batch, sql, mol pour/wisp/bond and cook --persist (formula steps), backup
//     restore, and the verbs gc cannot judge (create-form, serve, tracker
//     syncs). TestGcBdRefusesAClassFlipOnASplitCity and the
//     class_flip_guard_review_test.go tables;
//     TestGcBdEveryVerbIsGuardedOrCannotFlipClass holds a row for every bd verb.
//   - gc bd's by-id door, binding side (serveBdByIDResolved):
//     TestGcBdByIDUpdateRefusesTurningABindingResidentIntoWork.
//   - gc formula cook --meta (cookMetaClassFlip):
//     TestFormulaCookMetaRefusesAClassFlipOnASplitCity.
//   - gc beads metadata-cas (beadsMetadataCASClassFlipRefusal):
//     TestBeadsMetadataCASRefusesAClassFlipOnASplitCity.
//   - the bead API's PATCH/POST update (internal/api refuseClassFlip):
//     TestBeadAPIUpdateRefusesAClassFlipOnASplitCity.
//
// The answer to "which classes does this store hold" is pure configuration,
// the same input relocatedBeadClasses and the create refusal read, so it is the
// same before and after a migration has physically moved the beads. A city
// that relocates nothing answers nil, and nil disarms the guard: single-store
// cities see no behavior change.

// storeClassHolds returns the classes the store behind binding holds on this
// city, or nil when the city relocates nothing (its one store holds every
// class). config.StorageWorkBinding names the work stores — the city's and
// every rig's — which hold work plus any class no binding serves.
func storeClassHolds(cfg *config.City, binding string) []coordclass.Class {
	if cfg == nil || cfg.Storage == nil {
		return nil
	}
	storage := cfg.EffectiveStorage()
	relocated := false
	var holds []coordclass.Class
	if binding == config.StorageWorkBinding {
		holds = append(holds, coordclass.ClassWork)
	}
	for _, class := range infraMigrationClasses {
		served := strings.TrimSpace(storage.Classes.BindingFor(class))
		if served == "" {
			served = config.StorageWorkBinding
		}
		if served != config.StorageWorkBinding {
			relocated = true
		}
		if served == binding {
			holds = append(holds, coordclassFor(string(class)))
		}
	}
	if !relocated {
		return nil
	}
	return holds
}

// workStoreClassHolds is storeClassHolds for a work store.
func workStoreClassHolds(cfg *config.City) []coordclass.Class {
	return storeClassHolds(cfg, config.StorageWorkBinding)
}

// routesClassHolds is storeClassHolds for a door that already holds the
// opened routes and the store it is about to write: a binding holds the
// classes routed to it, any other store (the city or a rig work store) holds
// work plus every class the routes leave on work. Routes that relocate
// nothing — a single-store city — answer nil.
func routesClassHolds(routes *storageRoutes, store beads.Store) []coordclass.Class {
	if routes == nil || len(routes.stores) == 0 {
		return nil
	}
	var holds []coordclass.Class
	for _, class := range coordclass.Classes() {
		if served, ok := routes.stores[class]; ok && served == store {
			holds = append(holds, class)
		}
	}
	if len(holds) > 0 {
		return holds
	}
	holds = []coordclass.Class{coordclass.ClassWork}
	for _, class := range coordclass.Classes() {
		if _, relocated := routes.stores[class]; class.IsInfrastructure() && !relocated {
			holds = append(holds, class)
		}
	}
	return holds
}

// classFlipRefusalText renders a refused flip for a CLI door. who names the
// write ("gc bd update demo-1"), and the text ends with the command that does
// what the caller was after.
func classFlipRefusalText(who string, flip *coordclass.Flip) string {
	return fmt.Sprintf("%s: refused: %s. %s", who, flip.Error(), flip.Guidance())
}

// bdClassGuard is what the gc bd class-must-not-flip guard decided for one
// invocation. A non-empty refusal stops the passthrough before bd runs. args
// is the argv to forward (gc's own flags, such as import --skip-infra, are
// consumed); stdin, when non-nil, replaces os.Stdin because the guard had to
// read it to judge it.
type bdClassGuard struct {
	refusal string
	args    []string
	stdin   io.Reader
	notice  string
	cleanup func()
}

// bdReadResult is what a pre-read of the bead a write lands on found.
type bdReadResult int

const (
	// bdReadFound: the bead was read.
	bdReadFound bdReadResult = iota
	// bdReadNotFound: the store answered that the bead is absent.
	bdReadNotFound
	// bdReadFailed: the store could not answer (bd unavailable, a decode
	// error, a substring collision). Nothing is known about the bead.
	bdReadFailed
)

// bdClassGuardDeps are the guard's views of the world, injected so the guard
// itself runs nothing.
type bdClassGuardDeps struct {
	// stdin is what bd would read; the guard reads it for import - and batch.
	stdin io.Reader
	// read returns the bead id as the work store holds it now.
	read func(id string) (beads.Bead, bdReadResult)
	// runBd runs bd (in the target scope, with the passthrough's environment)
	// for a READ the guard needs — `bd cook <formula>`, which prints the
	// resolved formula and writes nothing — and returns its stdout.
	runBd func(argv []string) ([]byte, error)
}

// bdClassGuardVerb is one verb's arm of the guard.
type bdClassGuardVerb func(in bdClassGuardInput) bdClassGuard

// bdClassGuardInput is everything an arm may consult.
type bdClassGuardInput struct {
	verb      string
	verbArgs  []string
	bdArgs    []string
	globals   []string           // the root flags in front of the verb
	holds     []coordclass.Class // nil on a single-store city
	scopeRoot string
	bdClassGuardDeps
}

// bdClassGuardVerbs are the gc bd verbs the guard judges. Every other bd verb
// is either refused elsewhere or cannot write a field coordclass.Classify
// reads; TestGcBdEveryVerbIsGuardedOrCannotFlipClass holds a row for every
// verb bd has, and fails when one is added without one.
var bdClassGuardVerbs = map[string]bdClassGuardVerb{
	"update":      bdClassGuardUpdate,
	"label":       bdClassGuardLabel,
	"tag":         bdClassGuardTag,
	"set-state":   bdClassGuardSetState,
	"import":      bdClassGuardImport,
	"batch":       bdClassGuardBatch,
	"sql":         bdClassGuardSQL,
	"mol":         bdClassGuardMol,
	"cook":        bdClassGuardCook,
	"backup":      bdClassGuardBackup,
	"create-form": bdClassGuardRefuseOnSplit("create-form asks for the bead's fields interactively, so gc cannot classify the bead before bd writes it"),
	"serve":       bdClassGuardRefuseOnSplit("bd serve exposes bd's write API to any HTTP client, and none of those writes passes through gc, so gc can judge none of them"),
	"jira":        bdClassGuardRefuseOnSplit(bdClassGuardIntegrationReason),
	"linear":      bdClassGuardRefuseOnSplit(bdClassGuardIntegrationReason),
	"github":      bdClassGuardRefuseOnSplit(bdClassGuardIntegrationReason),
	"gitlab":      bdClassGuardRefuseOnSplit(bdClassGuardIntegrationReason),
	"ado":         bdClassGuardRefuseOnSplit(bdClassGuardIntegrationReason),
	"notion":      bdClassGuardRefuseOnSplit(bdClassGuardIntegrationReason),
}

const bdClassGuardIntegrationReason = "an external-tracker sync writes the labels and types the tracker holds onto work-store beads, and gc cannot classify them before bd writes them"

// bdClassFlipGuard runs the class-must-not-flip guard over a gc bd invocation
// that is about to reach bd in the work scope (the by-id door has already
// served or refused every binding resident).
func bdClassFlipGuard(cfg *config.City, scopeRoot string, bdArgs []string, deps bdClassGuardDeps) bdClassGuard {
	pass := bdClassGuard{args: bdArgs}
	verb, verbArgs, resolved := bdRelocatedClassVerb(bdArgs)
	if !resolved {
		// An undecidable verb sits behind a root flag in neither bdflags
		// manifest, which bd rejects before writing; see
		// bdRelocatedClassCreateRefusal for why this premise is pinned.
		return pass
	}
	if canonical, aliased := bdflags.TopLevelAliases()[verb]; aliased {
		// bd runs an alias as its verb (`bd protomolecule pour` is `bd mol
		// pour`), so the guard judges it as that verb.
		verb = canonical
	}
	arm, guarded := bdClassGuardVerbs[verb]
	if !guarded {
		return pass
	}
	return arm(bdClassGuardInput{
		verb: verb, verbArgs: verbArgs, bdArgs: bdArgs,
		globals: bdArgs[:len(bdArgs)-len(verbArgs)-1],
		holds:   workStoreClassHolds(cfg), scopeRoot: scopeRoot, bdClassGuardDeps: deps,
	})
}

// refuse is the shared refusal of an arm.
func (in bdClassGuardInput) refuse(format string, args ...any) bdClassGuard {
	return bdClassGuard{refusal: fmt.Sprintf(format, args...)}
}

func (in bdClassGuardInput) pass() bdClassGuard { return bdClassGuard{args: in.bdArgs} }

// mutationRemoves reports whether m takes something away: a removed or
// replaced label, an unset key, or a key set to empty. A removal can unmask a
// class marker (a synthetic convoy carrying gc.root_bead_id is work only while
// it stays synthetic), so it can only be judged against the real bead.
func mutationRemoves(m coordclass.Mutation) bool {
	if m.ReplaceLabels || len(m.RemoveLabels) > 0 || len(m.UnsetMetadata) > 0 {
		return true
	}
	for _, v := range m.SetMetadata {
		if strings.TrimSpace(v) == "" {
			return true
		}
	}
	return false
}

// checkIDs judges a write against every addressed bead. variants are the
// readings of the one write a bd build might make (see bdUpdateMutation's
// null handling); the write is refused if ANY of them flips the class.
//
// A bead the store answers is absent is judged from the write alone (applied
// to an empty work bead): an add of a class marker is still refused, and bd
// reports the missing bead itself. A bead the store could NOT read is judged
// the same way unless the write removes something, which cannot be judged
// without the bead and is refused.
func (in bdClassGuardInput) checkIDs(who string, ids []string, variants ...coordclass.Mutation) bdClassGuard {
	touches, removes := false, false
	for _, m := range variants {
		touches = touches || m.TouchesClass()
		removes = removes || mutationRemoves(m)
	}
	if in.holds == nil || !touches {
		return in.pass()
	}
	if len(ids) == 0 {
		// bd's id-less update writes "the last touched issue", which gc
		// cannot read back to judge.
		return in.refuse("gc bd %s: refused: this city keeps infrastructure beads in a separate store, and a write of a bead's type, labels or metadata is judged against the bead it lands on; name the bead explicitly (bd's id-less form writes the last touched issue, which gc cannot read). Nothing was written.", who)
	}
	for _, id := range ids {
		before, result := in.read(id)
		switch result {
		case bdReadFound:
		case bdReadFailed:
			if removes {
				return in.refuse("gc bd %s %s: refused: gc could not read the bead to check that this write leaves its storage class alone, and a write that removes a label or metadata key can only be judged against the bead itself. Retry once the store answers. Nothing was written.", who, id)
			}
			before = beads.Bead{ID: id}
		default:
			before = beads.Bead{ID: id}
		}
		for _, m := range variants {
			if flip := coordclass.CheckFlip(before, m.Apply(before), in.holds); flip != nil {
				return in.refuse("%s", classFlipRefusalText(fmt.Sprintf("gc bd %s %s", who, id), flip))
			}
		}
	}
	return in.pass()
}

// bdClassGuardUpdate judges `gc bd update`.
func bdClassGuardUpdate(in bdClassGuardInput) bdClassGuard {
	if in.holds == nil {
		return in.pass()
	}
	ids, variants, problem := bdUpdateMutation(in.verbArgs)
	if problem != "" {
		return in.refuse("gc bd update: refused: %s, so gc cannot tell whether this write changes a bead's storage class on this split city. Nothing was written.", problem)
	}
	return in.checkIDs("update", ids, variants...)
}

// bdUpdateMutation reads the class-relevant flags of a `bd update` argv (the
// argv after the verb) into the write, and returns the positional ids. problem
// is non-empty when the argv cannot be read exactly.
//
// The write comes back as one or two variants. `--set-metadata k=null` is
// stored differently by the bd builds the fleet runs: bd 1.3.2 stores JSON
// null, which reads back as absent, and the 1.3.0 Enterprise build stores the
// string "null". The second variant reads every such key as removed, so a
// null can unmask no class marker unseen.
func bdUpdateMutation(args []string) (ids []string, variants []coordclass.Mutation, problem string) {
	values := bdflags.ValueFlags("update")
	bools := bdflags.BoolFlags("update")
	var m coordclass.Mutation
	var nullKeys []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			ids = append(ids, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			ids = append(ids, arg)
			continue
		}
		name, value, stated := bdRelocatedClassCreateFlagValue(arg)
		if !stated && bools[name] {
			continue
		}
		if !values[name] {
			if stated && bools[name] {
				continue
			}
			return nil, nil, fmt.Sprintf("flag %q is not one gc knows for bd update", arg)
		}
		if !stated {
			if i+1 >= len(args) {
				return nil, nil, fmt.Sprintf("flag %q has no value", name)
			}
			i++
			value = args[i]
		}
		switch name {
		case "--type", "-t":
			v := strings.TrimSpace(value)
			m.Type = &v
		case "--add-label", "-l", "--remove-label", "--set-labels":
			labels, err := bdLabelFlagValues(value)
			if err != nil {
				return nil, nil, fmt.Sprintf("%s %q is not a label list bd can parse (%v)", name, value, err)
			}
			switch name {
			case "--remove-label":
				m.RemoveLabels = append(m.RemoveLabels, labels...)
			case "--set-labels":
				m.ReplaceLabels = true
				m.SetLabels = append(m.SetLabels, labels...)
			default:
				m.AddLabels = append(m.AddLabels, labels...)
			}
		case "--set-metadata":
			key, v, ok := strings.Cut(value, "=")
			if !ok || strings.TrimSpace(key) == "" {
				return nil, nil, fmt.Sprintf("--set-metadata %q is not key=value", value)
			}
			if m.SetMetadata == nil {
				m.SetMetadata = map[string]string{}
			}
			m.SetMetadata[key] = v
			if v == "null" {
				nullKeys = append(nullKeys, key)
			}
		case "--unset-metadata":
			m.UnsetMetadata = append(m.UnsetMetadata, value)
		case "--metadata":
			// bd merges the object into the bead's metadata; a JSON null
			// reads back as empty, which StringMap already decodes it to.
			// @file and malformed bodies were already refused by
			// bdRigQualifiedMetadataRefusal.
			var fields beads.StringMap
			if err := json.Unmarshal([]byte(value), &fields); err != nil {
				return nil, nil, fmt.Sprintf("--metadata is not an inline JSON object (%v)", err)
			}
			if m.SetMetadata == nil {
				m.SetMetadata = map[string]string{}
			}
			for k, v := range fields {
				m.SetMetadata[k] = v
			}
		}
	}
	variants = []coordclass.Mutation{m}
	if len(nullKeys) > 0 {
		absent := m
		absent.SetMetadata = maps.Clone(m.SetMetadata)
		absent.UnsetMetadata = slices.Clone(m.UnsetMetadata)
		for _, k := range nullKeys {
			delete(absent.SetMetadata, k)
			absent.UnsetMetadata = append(absent.UnsetMetadata, k)
		}
		variants = append(variants, absent)
	}
	return ids, variants, ""
}

// bdLabelFlagValues parses a label FLAG value — update's --add-label, -l,
// --remove-label and --set-labels, and create's --labels/-l/--label — exactly
// as bd does: they are cobra StringSlice flags, which read the value as ONE
// CSV record (so `"wisp"` is the label wisp, quotes stripped), and bd then
// trims each label and drops empty ones. A value bd's own parse rejects is an
// error; the caller refuses rather than guesses.
func bdLabelFlagValues(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	record, err := csv.NewReader(strings.NewReader(value)).Read()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range record {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

// bdLabelVerbList splits the label argument of `bd label add|remove|propagate`
// the way bd's label verbs do (splitLabelArg): on plain commas, trimming each,
// with no CSV quoting — so `"wisp"` there is a label spelled with quotes, not
// wisp.
func bdLabelVerbList(value string) []string {
	var out []string
	for _, l := range strings.Split(value, ",") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// bdPositionals returns the non-flag tokens of argv, failing on a flag that is
// neither a global flag nor one of the verb's own (extraValues take a value,
// extraBools do not).
func bdPositionals(args []string, extraValues []string, extraBools ...string) ([]string, string) {
	values := bdflags.GlobalValueFlags()
	for _, f := range extraValues {
		values[f] = true
	}
	bools := bdflags.GlobalBoolFlags()
	bools["-h"], bools["--help"] = true, true
	for _, f := range extraBools {
		bools[f] = true
	}
	var out []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			out = append(out, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			out = append(out, arg)
			continue
		}
		name, _, stated := strings.Cut(arg, "=")
		switch {
		case bools[name] && !stated:
		case values[name] && stated:
		case values[name] && i+1 < len(args):
			i++
		default:
			return nil, fmt.Sprintf("flag %q is not one gc knows here", arg)
		}
	}
	return out, ""
}

// bdFlagValues returns every value given to the named value flags in args.
func bdFlagValues(args []string, names ...string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		name, value, stated := bdRelocatedClassCreateFlagValue(args[i])
		if !slices.Contains(names, name) {
			continue
		}
		if !stated {
			if i+1 >= len(args) {
				break
			}
			i++
			value = args[i]
		}
		out = append(out, value)
	}
	return out
}

// bdClassGuardLabel judges `gc bd label add|remove|propagate`.
func bdClassGuardLabel(in bdClassGuardInput) bdClassGuard {
	if in.holds == nil {
		return in.pass()
	}
	pos, problem := bdPositionals(in.verbArgs, nil)
	if problem != "" {
		return in.refuse("gc bd label: refused: %s, so gc cannot tell whether this write changes a bead's storage class on this split city. Nothing was written.", problem)
	}
	if len(pos) == 0 {
		return in.pass()
	}
	sub, rest := pos[0], pos[1:]
	switch sub {
	case "list", "list-all":
		return in.pass()
	case "add", "remove":
		if len(rest) < 2 {
			return in.pass() // bd rejects it before writing
		}
		labels := bdLabelVerbList(rest[len(rest)-1])
		m := coordclass.Mutation{AddLabels: labels}
		if sub == "remove" {
			m = coordclass.Mutation{RemoveLabels: labels}
		}
		return in.checkIDs("label "+sub, rest[:len(rest)-1], m)
	case "propagate":
		if len(rest) != 2 {
			return in.pass()
		}
		// The label lands on the parent's children, which are not named
		// here; judge it on a work bead, which is what a work store's
		// children are. A class-marker label is refused whatever they are.
		empty := beads.Bead{}
		if flip := coordclass.CheckFlip(empty, coordclass.Mutation{AddLabels: bdLabelVerbList(rest[1])}.Apply(empty), in.holds); flip != nil {
			return in.refuse("%s", classFlipRefusalText("gc bd label propagate "+rest[0], flip))
		}
		return in.pass()
	default:
		return in.refuse("gc bd label %s: refused: gc does not know this label subcommand, so it cannot tell whether it changes a bead's storage class on this split city. Nothing was written.", sub)
	}
}

// bdClassGuardTag judges `gc bd tag <id> <label>` (bd's --add-label shorthand,
// which takes its one label verbatim, trimmed).
func bdClassGuardTag(in bdClassGuardInput) bdClassGuard {
	if in.holds == nil {
		return in.pass()
	}
	pos, problem := bdPositionals(in.verbArgs, nil)
	if problem != "" {
		return in.refuse("gc bd tag: refused: %s. Nothing was written.", problem)
	}
	if len(pos) != 2 {
		return in.pass()
	}
	return in.checkIDs("tag", pos[:1], coordclass.Mutation{AddLabels: []string{strings.TrimSpace(pos[1])}})
}

// bdClassGuardSetState judges `gc bd set-state <id> <dimension>=<value>`, which
// swaps the bead's first <dimension>:* label for <dimension>:<value>.
func bdClassGuardSetState(in bdClassGuardInput) bdClassGuard {
	if in.holds == nil {
		return in.pass()
	}
	pos, problem := bdPositionals(in.verbArgs, []string{"--reason"})
	if problem != "" {
		return in.refuse("gc bd set-state: refused: %s. Nothing was written.", problem)
	}
	if len(pos) != 2 {
		return in.pass()
	}
	id := pos[0]
	dimension, value, ok := strings.Cut(pos[1], "=")
	if !ok {
		return in.pass()
	}
	before, read := in.read(id)
	switch read {
	case bdReadFound:
	case bdReadFailed:
		// The swap removes whatever <dimension>:* label the bead carries,
		// which can only be judged against the bead.
		return in.refuse("gc bd set-state %s: refused: gc could not read the bead to check that swapping its %s label leaves its storage class alone. Retry once the store answers. Nothing was written.", id, dimension)
	default:
		before = beads.Bead{ID: id}
	}
	m := coordclass.Mutation{AddLabels: []string{dimension + ":" + value}}
	for _, l := range before.Labels {
		if strings.HasPrefix(l, dimension+":") {
			m.RemoveLabels = []string{l}
			break
		}
	}
	if flip := coordclass.CheckFlip(before, m.Apply(before), in.holds); flip != nil {
		return in.refuse("%s", classFlipRefusalText("gc bd set-state "+id, flip))
	}
	return in.pass()
}

// bdClassGuardRefuseOnSplit refuses a verb whose writes gc cannot classify,
// on a split city only.
func bdClassGuardRefuseOnSplit(reason string) bdClassGuardVerb {
	return func(in bdClassGuardInput) bdClassGuard {
		if in.holds == nil {
			return in.pass()
		}
		return in.refuse("gc bd %s: refused on this split city: %s, and a bead that lands in the work store with an infrastructure class is stranded there (one stranded bead makes every later gc command refuse until it is repaired). Run bd directly in %s if you have checked what it will write. Nothing was written.", in.verb, reason, in.scopeRoot)
	}
}

// bdClassGuardSQL refuses anything but a single plain read statement through
// `gc bd sql` on a split city: raw DML can set issue_type or labels to anything
// and bypasses every bd write path.
//
// It is deliberately simple rather than clever. A query carrying a comment
// (/*, --, #) is refused outright: MySQL executable comments (/*! ... */) run
// their contents, and a comment can hide a statement separator from any
// splitter. More than one statement is refused. What is left must start with a
// read keyword, an EXPLAIN/DESCRIBE must not wrap a write, and no Dolt
// procedure may be called as a function (SELECT DOLT_MERGE(...) writes). The
// core pack's own reads (scope_bd.sh) are plain single SELECTs and pass.
func bdClassGuardSQL(in bdClassGuardInput) bdClassGuard {
	if in.holds == nil {
		return in.pass()
	}
	pos, problem := bdPositionals(in.verbArgs, nil, "--csv")
	if problem != "" {
		return in.refuse("gc bd sql: refused on this split city: %s, so gc cannot find the query it would run. Nothing was written.", problem)
	}
	for _, query := range pos {
		if why := sqlReadOnlyProblem(query); why != "" {
			return in.refuse("gc bd sql: refused on this split city: %s. Only a single plain read statement (SELECT, SHOW, DESCRIBE, EXPLAIN SELECT), with no comments, passes through gc bd sql here: a raw write can give a work-store bead an infrastructure type or label, which strands it. Use the gc bd verbs, which gc judges, or run bd directly in %s if you have checked what it will write. Nothing was written.", why, in.scopeRoot)
		}
	}
	return in.pass()
}

// sqlDoltCallRE matches a Dolt procedure called as a function.
var sqlDoltCallRE = regexp.MustCompile(`(?i)\bdolt_[a-z_]+\s*\(`)

// sqlReadOnlyProblem returns why query is not a single plain read, or "".
func sqlReadOnlyProblem(query string) string {
	q := strings.TrimSpace(query)
	q = strings.TrimSpace(strings.TrimSuffix(q, ";"))
	switch {
	case q == "":
		return "the query is empty"
	case strings.Contains(q, "/*"), strings.Contains(q, "--"), strings.Contains(q, "#"):
		return "the query carries a comment (/*, -- or #)"
	case strings.Contains(q, ";"):
		return "the query holds more than one statement"
	case sqlDoltCallRE.MatchString(q):
		return "the query calls a Dolt procedure"
	}
	words := strings.FieldsFunc(q, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '('
	})
	switch strings.ToUpper(words[0]) {
	case "SELECT", "SHOW":
		return ""
	case "DESCRIBE", "DESC", "EXPLAIN":
		if len(words) > 1 {
			switch strings.ToUpper(words[1]) {
			case "UPDATE", "INSERT", "DELETE", "REPLACE", "ANALYZE", "WITH", "CALL", "TABLE":
				return "the EXPLAIN/DESCRIBE wraps a statement that is not a plain SELECT"
			}
		}
		return ""
	default:
		return fmt.Sprintf("%q is not a read statement", words[0])
	}
}

// bdClassGuardMol judges `gc bd mol pour|wisp|bond`, which instantiate a
// formula or proto: bd copies each step's type, labels and metadata onto the
// beads it creates, verbatim, so a formula whose step carries gc.root_bead_id
// or gc:session mints an infrastructure-class bead in the work store. Each
// formula is resolved with `bd cook <formula>` — bd's own resolution
// (extends, composition, search paths), which prints the resolved formula and
// writes nothing — and every step is classified. An argument that is not a
// formula must be an existing bead (a persisted proto or molecule) that the
// work store may hold; anything else is refused.
func bdClassGuardMol(in bdClassGuardInput) bdClassGuard {
	if in.holds == nil {
		return in.pass()
	}
	values := []string{"--var", "--assignee", "--attach", "--attach-type", "--type", "--as", "--ref", "--age", "--exclude-type", "--for", "--limit", "--range"}
	bools := []string{"--dry-run", "--root-only", "--ephemeral", "--pour", "--all", "--closed", "--force", "-f", "--gated"}
	pos, problem := bdPositionals(in.verbArgs, values, bools...)
	if problem != "" {
		return in.refuse("gc bd mol: refused on this split city: %s, so gc cannot find the formula it would instantiate. Nothing was written.", problem)
	}
	if len(pos) == 0 || slices.Contains(in.verbArgs, "--dry-run") {
		return in.pass()
	}
	sub, targets := pos[0], pos[1:]
	switch sub {
	case "pour", "bond":
	case "wisp":
		if len(targets) > 0 {
			switch targets[0] {
			case "list", "gc":
				return in.pass()
			case "create":
				targets = targets[1:]
			}
		}
	default:
		return in.pass()
	}
	targets = append(targets, bdFlagValues(in.verbArgs, "--attach")...)
	for _, target := range targets {
		if refusal := in.formulaClassRefusal("mol "+sub, target, nil); refusal != "" {
			return in.refuse("%s", refusal)
		}
	}
	return in.pass()
}

// bdClassGuardCook judges `gc bd cook --persist`, which writes the resolved
// formula into the work store as a proto. Without --persist cook only prints.
func bdClassGuardCook(in bdClassGuardInput) bdClassGuard {
	if in.holds == nil || !slices.Contains(in.verbArgs, "--persist") || slices.Contains(in.verbArgs, "--dry-run") {
		return in.pass()
	}
	pos, problem := bdPositionals(in.verbArgs, []string{"--search-path", "--prefix", "--var", "--mode"}, "--persist", "--force", "--dry-run")
	if problem != "" {
		return in.refuse("gc bd cook: refused on this split city: %s, so gc cannot find the formula it would persist. Nothing was written.", problem)
	}
	searchPaths := bdFlagValues(in.verbArgs, "--search-path")
	for _, target := range pos {
		if refusal := in.formulaClassRefusal("cook --persist", target, searchPaths); refusal != "" {
			return in.refuse("%s", refusal)
		}
	}
	return in.pass()
}

// formulaClassRefusal classifies every step of the formula target resolves to
// and returns the refusal for the first one this store does not hold, or "".
func (in bdClassGuardInput) formulaClassRefusal(who, target string, searchPaths []string) string {
	argv := append(slices.Clone(in.globals), "cook", target)
	for _, p := range searchPaths {
		argv = append(argv, "--search-path", p)
	}
	var steps []beads.Bead
	resolved := false
	if in.runBd != nil {
		if out, err := in.runBd(argv); err == nil {
			steps, resolved = formulaStepShapes(out)
		}
	}
	if !resolved {
		bead, result := in.read(target)
		if result != bdReadFound {
			return fmt.Sprintf("gc bd %s %s: refused on this split city: gc could not resolve %q as a formula (bd cook) or as a bead in this store, so it cannot check that the beads it would create belong in the work store. Nothing was written.", who, target, target)
		}
		steps = []beads.Bead{bead}
	}
	for _, step := range steps {
		if class := coordclass.Classify(step); !slices.Contains(in.holds, class) {
			return fmt.Sprintf("gc bd %s %s: refused on this split city: step %q would be minted as a %s-class bead (type %q, labels %v) in the work store, where this city never reads that class, and one stranded bead makes every later gc command refuse until it is repaired. Instantiate it with `%s`, which writes the store that holds that class. Nothing was written.",
				who, target, step.ID, class, step.Type, step.Labels, coordclass.MintCommand(class))
		}
	}
	return ""
}

// formulaStepShapes reads the resolved formula `bd cook` prints and returns
// the shape of every step bd would turn into a bead: each object in it that
// carries an "id" (steps, their children, template and advice steps alike),
// with its type, labels and metadata. ok=false when the output is not a
// resolved formula.
func formulaStepShapes(out []byte) ([]beads.Bead, bool) {
	start := bytes.IndexByte(out, '{')
	if start < 0 {
		return nil, false
	}
	var root map[string]any
	if err := json.NewDecoder(bytes.NewReader(out[start:])).Decode(&root); err != nil {
		return nil, false
	}
	if _, isFormula := root["formula"]; !isFormula {
		return nil, false
	}
	var shapes []beads.Bead
	var walk func(v any, top bool)
	walk = func(v any, top bool) {
		switch t := v.(type) {
		case map[string]any:
			if id, ok := t["id"].(string); ok && !top {
				shape := beads.Bead{ID: id}
				shape.Type, _ = t["type"].(string)
				if labels, ok := t["labels"].([]any); ok {
					for _, l := range labels {
						if s, ok := l.(string); ok {
							shape.Labels = append(shape.Labels, strings.TrimSpace(s))
						}
					}
				}
				if meta, ok := t["metadata"].(map[string]any); ok {
					shape.Metadata = map[string]string{}
					for k, mv := range meta {
						if s, ok := mv.(string); ok {
							shape.Metadata[k] = s
						} else if mv != nil {
							text, _ := json.Marshal(mv)
							shape.Metadata[k] = string(text)
						}
					}
				}
				shapes = append(shapes, shape)
			}
			for _, child := range t {
				walk(child, false)
			}
		case []any:
			for _, child := range t {
				walk(child, false)
			}
		}
	}
	walk(root, true)
	return shapes, true
}

// bdBackupPostdatesSplitFlag is gc's own flag on `gc bd backup restore`: the
// operator's attestation that the backup was taken AFTER this city split its
// storage. gc consumes it; bd never sees it.
const bdBackupPostdatesSplitFlag = "--backup-postdates-split"

// bdClassGuardBackup judges `gc bd backup restore`. A restore replaces the work
// store with the backup, so a backup taken before the split brings every
// infrastructure row of that time back into the work store at once — the
// import hazard, wholesale, and one gc cannot inspect (the backup is a Dolt
// database, not rows). It is refused on a split city unless the operator
// attests, with --backup-postdates-split, that the backup was taken after the
// split; the boot containment check still reports anything stranded.
func bdClassGuardBackup(in bdClassGuardInput) bdClassGuard {
	args, attested := stripGCFlag(in.bdArgs, bdBackupPostdatesSplitFlag)
	pos, _ := bdPositionals(in.verbArgs, nil, "--force", bdBackupPostdatesSplitFlag)
	if in.holds == nil || len(pos) == 0 || pos[0] != "restore" {
		return bdClassGuard{args: args}
	}
	if !attested {
		return in.refuse("gc bd backup restore: refused on this split city: a restore replaces the work store with the backup, and a backup taken before the split carries the infrastructure rows of that time, which would all be stranded in the work store at once (every later gc command refuses until they are repaired). gc cannot look inside the backup to tell. If this backup was taken after the split, re-run with %s. If it predates the split, restore it elsewhere, export it, and use `gc bd import <file> --skip-infra` instead (see examples/bd/dolt/docs/journal-corruption-recovery.md). Nothing was written.", bdBackupPostdatesSplitFlag)
	}
	return bdClassGuard{args: args, notice: "gc bd backup restore: restoring a backup attested to postdate the split; the next boot's containment check reports any infrastructure row it brings back into the work store\n"}
}

// stripGCFlag removes every occurrence of a gc-only bool flag from argv.
func stripGCFlag(argv []string, flag string) ([]string, bool) {
	out := make([]string, 0, len(argv))
	found := false
	for _, a := range argv {
		if a == flag {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}

// bdClassGuardBatch judges `gc bd batch`, whose script (stdin or -f) can mint
// a bead of any type with `create <type> ...`. A create of an infrastructure
// type is refused; an update line may only set the keys bd's batch grammar
// documents (none of which is a class field), and any other line is refused
// rather than guessed at.
func bdClassGuardBatch(in bdClassGuardInput) bdClassGuard {
	if in.holds == nil {
		return in.pass()
	}
	var file string
	for i := 0; i < len(in.verbArgs); i++ {
		name, value, stated := bdRelocatedClassCreateFlagValue(in.verbArgs[i])
		if name == "-f" || name == "--file" {
			if !stated && i+1 < len(in.verbArgs) {
				i++
				value = in.verbArgs[i]
			}
			file = value
		} else if (name == "-m" || name == "--message") && !stated {
			i++
		}
	}
	var script []byte
	var err error
	out := in.pass()
	if file != "" {
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(in.scopeRoot, path)
		}
		script, err = os.ReadFile(path)
	} else {
		script, err = io.ReadAll(in.stdin)
		out.stdin = bytes.NewReader(script)
	}
	if err != nil {
		return in.refuse("gc bd batch: refused on this split city: gc must read the batch script to check that it mints no infrastructure bead into the work store, and could not: %v. Nothing was written.", err)
	}
	if problem := batchScriptClassProblem(script, in.holds); problem != "" {
		return in.refuse("gc bd batch: refused on this split city: %s. Nothing was written.", problem)
	}
	return out
}

// batchScriptClassProblem returns why a bd batch script would write a bead
// outside the work class, or "".
func batchScriptClassProblem(script []byte, holds []coordclass.Class) string {
	safeUpdateKeys := map[string]bool{"status": true, "priority": true, "title": true, "assignee": true, "force": true}
	scanner := bufio.NewScanner(bytes.NewReader(script))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		tokens := tokenizeBdBatchLine(text)
		if len(tokens) == 0 {
			continue
		}
		switch tokens[0] {
		case "close", "dep":
		case "update":
			for _, kv := range tokens[min(2, len(tokens)):] {
				key, _, _ := strings.Cut(kv, "=")
				if !safeUpdateKeys[key] {
					return fmt.Sprintf("line %d updates %q, which gc does not know to be a field that leaves a bead's storage class alone", line, key)
				}
			}
		case "create":
			if len(tokens) < 2 {
				continue
			}
			shape := beads.Bead{Type: tokens[1]}
			if flip := coordclass.CheckFlip(beads.Bead{}, shape, holds); flip != nil {
				return fmt.Sprintf("line %d creates a %s-class bead (type %q) in the work store, where this city never reads that class; mint it with `%s`", line, flip.After, tokens[1], coordclass.MintCommand(flip.After))
			}
		default:
			return fmt.Sprintf("line %d is a batch command gc does not know (%q)", line, tokens[0])
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Sprintf("reading the script: %v", err)
	}
	return ""
}

// tokenizeBdBatchLine splits a batch line the way bd does: whitespace
// separated, with double-quoted tokens where \" and \\ escape.
func tokenizeBdBatchLine(line string) []string {
	var tokens []string
	var cur strings.Builder
	inQuote, have := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQuote && c == '\\' && i+1 < len(line) && (line[i+1] == '"' || line[i+1] == '\\'):
			cur.WriteByte(line[i+1])
			i++
		case c == '"':
			inQuote = !inQuote
			have = true
		case !inQuote && (c == ' ' || c == '\t'):
			if have {
				tokens = append(tokens, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	if have {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// bdImportSkipInfraFlag is gc's own flag on `gc bd import`: import only the
// work-class rows of the file and report the rest. gc consumes it; bd never
// sees it.
const bdImportSkipInfraFlag = "--skip-infra"

// bdClassGuardImport judges `gc bd import`, the documented restore path for a
// JSONL archive (examples/bd/dolt/docs/journal-corruption-recovery.md,
// docs/getting-started/troubleshooting.md).
//
// On a split city an archive taken before the split carries infrastructure
// rows — sessions, wisps, molecules, mail — and bd import would upsert every
// one into the work store, stranding them all at once. They are NOT routed to
// the binding: the binding already holds the live copies (gc storage migrate
// preserved ids), and an upsert of a backup row over its live copy would roll
// back sessions and running graphs to the archive's stale state, through a
// store that has no import verb and no transaction spanning the two stores.
// So the import is refused and names --skip-infra, which imports the
// work-class rows only and says how many infrastructure rows it left out.
func bdClassGuardImport(in bdClassGuardInput) bdClassGuard {
	args, skipInfra := stripBdImportSkipInfra(in.bdArgs, in.verb)
	if in.holds == nil {
		// Single-store: the one store holds every class. --skip-infra has
		// nothing to skip; it is accepted and dropped.
		return bdClassGuard{args: args}
	}
	_, verbArgs, _ := bdRelocatedClassVerb(args)
	source, sourceAt, problem := bdImportSource(verbArgs)
	if problem != "" {
		return in.refuse("gc bd import: refused on this split city: %s. Nothing was written.", problem)
	}
	var data []byte
	var err error
	if source == "-" {
		data, err = io.ReadAll(in.stdin)
	} else {
		path := source
		if !filepath.IsAbs(path) {
			path = filepath.Join(in.scopeRoot, path)
		}
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return in.refuse("gc bd import: refused on this split city: gc must read %s to check that it holds no infrastructure rows, and could not: %v. Nothing was written.", source, err)
	}
	split, problem := splitBdImportRows(data)
	if problem != "" {
		return in.refuse("gc bd import: refused on this split city: %s. Nothing was written.", problem)
	}
	work, skipped, byClass := split.keep, split.skipped, split.byClass
	out := bdClassGuard{args: args}
	if skipped == 0 {
		if source == "-" {
			out.stdin = bytes.NewReader(data)
		}
		return out
	}
	if slices.Contains(verbArgs, "--dry-run") && !skipInfra {
		// A dry run writes nothing, so it is reported rather than refused:
		// bd shows what it would import, and gc says what it would refuse.
		if source == "-" {
			out.stdin = bytes.NewReader(data)
		}
		out.notice = fmt.Sprintf("gc bd import: dry run: %s holds %d infrastructure row(s) (%s); on this split city the real import is refused unless re-run with %s, which imports the %d work row(s) only\n",
			source, skipped, formatClassCounts(byClass), bdImportSkipInfraFlag, len(work))
		return out
	}
	if !skipInfra {
		return in.refuse("gc bd import: refused on this split city: %s holds %d infrastructure row(s) (%s). This city keeps those classes in a separate store, so bd import would strand every one in the work store, and every later gc command would refuse until they were repaired. They are not routed to that store either: it already holds the live copies, and replaying an archive over them would roll sessions and running graphs back to the archive's state. Re-run with %s to import only the %d work row(s); the infrastructure rows are left out and counted. Nothing was written.",
			source, skipped, formatClassCounts(byClass), bdImportSkipInfraFlag, len(work))
	}
	filtered := bytes.Join(work, []byte("\n"))
	if len(filtered) > 0 {
		filtered = append(filtered, '\n')
	}
	notice := fmt.Sprintf("gc bd import: %s: importing %d work row(s); left out %d infrastructure row(s) (%s), which this split city keeps in its class store\n",
		source, len(work), skipped, formatClassCounts(byClass))
	if len(split.dependents) > 0 {
		shown := split.dependents
		if len(shown) > 10 {
			shown = shown[:10]
		}
		notice += fmt.Sprintf("gc bd import: %d kept work row(s) carry a dependency on a left-out infrastructure id (%s); those edges point at beads the class store holds under the same ids, so check them after the import\n",
			len(split.dependents), strings.Join(shown, ", "))
	}
	if source == "-" {
		out.stdin = bytes.NewReader(filtered)
		out.notice = notice
		return out
	}
	tmp, err := os.CreateTemp("", "gc-bd-import-work-*.jsonl")
	if err == nil {
		_, err = tmp.Write(filtered)
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		if tmp != nil {
			_ = os.Remove(tmp.Name())
		}
		return in.refuse("gc bd import: refused: could not write the work-only copy of %s: %v. Nothing was written.", source, err)
	}
	rewritten := slices.Clone(args)
	at := len(args) - len(verbArgs) + sourceAt
	rewritten[at] = bdImportRewriteSourceToken(rewritten[at], tmp.Name())
	out.args = rewritten
	out.notice = notice
	name := tmp.Name()
	out.cleanup = func() { _ = os.Remove(name) }
	return out
}

// stripBdImportSkipInfra removes gc's --skip-infra from an import argv.
func stripBdImportSkipInfra(bdArgs []string, verb string) ([]string, bool) {
	if verb != "import" {
		return bdArgs, false
	}
	return stripGCFlag(bdArgs, bdImportSkipInfraFlag)
}

// bdImportSource finds the JSONL an import reads: the positional file, -i /
// --input, or "-" for stdin. sourceAt is the index within verbArgs of the
// token naming it (for a separated -i, the value token).
func bdImportSource(verbArgs []string) (source string, sourceAt int, problem string) {
	values := map[string]bool{"-i": true, "--input": true}
	for k := range bdflags.GlobalValueFlags() {
		values[k] = true
	}
	bools := bdflags.GlobalBoolFlags()
	for _, b := range []string{"--allow-stale", "--dedup", "--dry-run", "-h", "--help"} {
		bools[b] = true
	}
	var found []int
	for i := 0; i < len(verbArgs); i++ {
		arg := verbArgs[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			found = append(found, i)
			continue
		}
		name, value, stated := bdRelocatedClassCreateFlagValue(arg)
		switch {
		case bools[name] && !stated:
		case name == "-i" || name == "--input":
			if stated {
				if value == "" {
					return "", 0, "-i/--input has no value"
				}
				found = append(found, i)
			} else if i+1 < len(verbArgs) {
				i++
				found = append(found, i)
			}
		case values[name]:
			if !stated {
				i++
			}
		default:
			return "", 0, fmt.Sprintf("flag %q is not one gc knows for bd import, so gc cannot find the file it reads", arg)
		}
	}
	switch len(found) {
	case 0:
		return "", 0, "bd import with no file reads the configured import path, which gc does not resolve; name the file (or - for stdin) explicitly"
	case 1:
	default:
		return "", 0, "more than one import source is named"
	}
	offset := found[0]
	tok := verbArgs[offset]
	_, value, stated := bdRelocatedClassCreateFlagValue(tok)
	if strings.HasPrefix(tok, "-") && tok != "-" && stated {
		tok = value
	}
	return tok, offset, ""
}

// bdImportRewriteSourceToken points one argv token at path, keeping an inline
// -i=/--input= spelling.
func bdImportRewriteSourceToken(tok, path string) string {
	if name, _, stated := bdRelocatedClassCreateFlagValue(tok); strings.HasPrefix(tok, "-") && tok != "-" && stated {
		if strings.HasPrefix(tok, "--") {
			return name + "=" + path
		}
		return name + path
	}
	return path
}

// bdImportSplit is a bd export JSONL split by class.
type bdImportSplit struct {
	keep    [][]byte // work rows and non-issue records such as memories
	skipped int      // infrastructure-class issue rows
	byClass map[coordclass.Class]int
	// dependents are the ids of kept work rows with a dependency on a skipped
	// row's id.
	dependents []string
}

// splitBdImportRows splits a bd export JSONL into the lines to keep (work
// rows and non-issue records such as memories) and counts the
// infrastructure-class issue rows by class.
func splitBdImportRows(data []byte) (bdImportSplit, string) {
	byClass := map[coordclass.Class]int{}
	var keep [][]byte
	skipped := 0
	skippedIDs := map[string]bool{}
	type keptDeps struct {
		id   string
		deps []string
	}
	var kept []keptDeps
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var row struct {
			Kind         string           `json:"_type"`
			ID           string           `json:"id"`
			Type         string           `json:"issue_type"`
			Labels       []string         `json:"labels"`
			Metadata     *beads.StringMap `json:"metadata"`
			Dependencies []struct {
				DependsOnID string `json:"depends_on_id"`
			} `json:"dependencies"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return bdImportSplit{}, fmt.Sprintf("line %d is not a JSON object gc can classify (%v)", line, err)
		}
		keepLine := append([]byte(nil), raw...)
		if row.Kind != "" && row.Kind != "issue" {
			keep = append(keep, keepLine) // memories and other non-issue records
			continue
		}
		b := beads.Bead{Type: row.Type, Labels: row.Labels}
		if row.Metadata != nil {
			b.Metadata = map[string]string(*row.Metadata)
		}
		if class := coordclass.Classify(b); class != coordclass.ClassWork {
			skipped++
			byClass[class]++
			skippedIDs[row.ID] = true
			continue
		}
		keep = append(keep, keepLine)
		k := keptDeps{id: row.ID}
		for _, d := range row.Dependencies {
			k.deps = append(k.deps, d.DependsOnID)
		}
		kept = append(kept, k)
	}
	if err := scanner.Err(); err != nil {
		return bdImportSplit{}, fmt.Sprintf("reading the file: %v", err)
	}
	split := bdImportSplit{keep: keep, skipped: skipped, byClass: byClass}
	for _, k := range kept {
		for _, d := range k.deps {
			if skippedIDs[d] {
				split.dependents = append(split.dependents, k.id)
				break
			}
		}
	}
	return split, ""
}

// formatClassCounts renders {graph: 3, sessions: 1} as "3 graph, 1 sessions".
func formatClassCounts(byClass map[coordclass.Class]int) string {
	classes := make([]coordclass.Class, 0, len(byClass))
	for c := range byClass {
		classes = append(classes, c)
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i] < classes[j] })
	parts := make([]string, 0, len(classes))
	for _, c := range classes {
		parts = append(parts, fmt.Sprintf("%d %s", byClass[c], c))
	}
	return strings.Join(parts, ", ")
}
