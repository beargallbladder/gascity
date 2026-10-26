package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
//     batch, create-form, sql writes, tracker syncs. TestGcBdRefusesAClassFlipOnASplitCity;
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
// is the argv to forward (import --skip-infra rewrites it); stdin, when
// non-nil, replaces os.Stdin because the guard had to read it to judge it.
type bdClassGuard struct {
	refusal string
	args    []string
	stdin   io.Reader
	notice  string
	cleanup func()
}

// bdClassGuardVerb is one verb's arm of the guard.
type bdClassGuardVerb func(in bdClassGuardInput) bdClassGuard

// bdClassGuardInput is everything an arm may consult.
type bdClassGuardInput struct {
	verb      string
	verbArgs  []string
	bdArgs    []string
	holds     []coordclass.Class // nil on a single-store city
	scopeRoot string
	stdin     io.Reader
	// read returns the bead id as the work store holds it now; ok=false when
	// it cannot be read (absent, ephemeral, projection lag, bd unavailable).
	read func(id string) (beads.Bead, bool)
}

// bdClassGuardVerbs are the gc bd verbs the guard judges. Every other bd verb
// is either refused elsewhere, mints rather than rewrites, or cannot write a
// field coordclass.Classify reads; TestGcBdEveryVerbIsGuardedOrCannotFlipClass
// holds a row for every verb bd has, and fails when one is added without one.
var bdClassGuardVerbs = map[string]bdClassGuardVerb{
	"update":      bdClassGuardUpdate,
	"label":       bdClassGuardLabel,
	"tag":         bdClassGuardTag,
	"set-state":   bdClassGuardSetState,
	"import":      bdClassGuardImport,
	"batch":       bdClassGuardBatch,
	"create-form": bdClassGuardRefuseOnSplit("create-form asks for the bead's fields interactively, so gc cannot classify the bead before bd writes it"),
	"sql":         bdClassGuardSQL,
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
func bdClassFlipGuard(cfg *config.City, scopeRoot string, bdArgs []string, stdin io.Reader, read func(string) (beads.Bead, bool)) bdClassGuard {
	pass := bdClassGuard{args: bdArgs}
	verb, verbArgs, resolved := bdRelocatedClassVerb(bdArgs)
	if !resolved {
		// An undecidable verb sits behind a root flag in neither bdflags
		// manifest, which bd rejects before writing; see
		// bdRelocatedClassCreateRefusal for why this premise is pinned.
		return pass
	}
	arm, guarded := bdClassGuardVerbs[verb]
	if !guarded {
		return pass
	}
	return arm(bdClassGuardInput{
		verb: verb, verbArgs: verbArgs, bdArgs: bdArgs,
		holds: workStoreClassHolds(cfg), scopeRoot: scopeRoot, stdin: stdin, read: read,
	})
}

// refuse is the shared refusal of an arm.
func (in bdClassGuardInput) refuse(format string, args ...any) bdClassGuard {
	return bdClassGuard{refusal: fmt.Sprintf(format, args...)}
}

func (in bdClassGuardInput) pass() bdClassGuard { return bdClassGuard{args: in.bdArgs} }

// checkIDs judges a mutation against every addressed bead. A bead that cannot
// be read is judged from the mutation alone (applied to an empty work bead):
// an add of a class marker is still refused, and bd reports the missing bead
// itself.
func (in bdClassGuardInput) checkIDs(who string, ids []string, m coordclass.Mutation) bdClassGuard {
	if in.holds == nil || !m.TouchesClass() {
		return in.pass()
	}
	if len(ids) == 0 {
		// bd's id-less update writes "the last touched issue", which gc
		// cannot read back to judge.
		return in.refuse("gc bd %s: refused: this city keeps infrastructure beads in a separate store, and a write of a bead's type, labels or metadata is judged against the bead it lands on; name the bead explicitly (bd's id-less form writes the last touched issue, which gc cannot read). Nothing was written.", who)
	}
	for _, id := range ids {
		before, ok := in.read(id)
		if !ok {
			before = beads.Bead{ID: id}
		}
		if flip := coordclass.CheckFlip(before, m.Apply(before), in.holds); flip != nil {
			return in.refuse("%s", classFlipRefusalText(fmt.Sprintf("gc bd %s %s", who, id), flip))
		}
	}
	return in.pass()
}

// bdClassGuardUpdate judges `gc bd update`.
func bdClassGuardUpdate(in bdClassGuardInput) bdClassGuard {
	if in.holds == nil {
		return in.pass()
	}
	ids, m, problem := bdUpdateMutation(in.verbArgs)
	if problem != "" {
		return in.refuse("gc bd update: refused: %s, so gc cannot tell whether this write changes a bead's storage class on this split city. Nothing was written.", problem)
	}
	return in.checkIDs("update", ids, m)
}

// bdUpdateMutation reads the class-relevant flags of a `bd update` argv (the
// argv after the verb) into a mutation, and returns the positional ids.
// problem is non-empty when the argv cannot be read exactly.
func bdUpdateMutation(args []string) (ids []string, m coordclass.Mutation, problem string) {
	values := bdflags.ValueFlags("update")
	bools := bdflags.BoolFlags("update")
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
			return nil, m, fmt.Sprintf("flag %q is not one gc knows for bd update", arg)
		}
		if !stated {
			if i+1 >= len(args) {
				return nil, m, fmt.Sprintf("flag %q has no value", name)
			}
			i++
			value = args[i]
		}
		switch name {
		case "--type", "-t":
			v := strings.TrimSpace(value)
			m.Type = &v
		case "--add-label":
			m.AddLabels = append(m.AddLabels, splitBdLabelList(value)...)
		case "--remove-label":
			m.RemoveLabels = append(m.RemoveLabels, splitBdLabelList(value)...)
		case "--set-labels":
			m.ReplaceLabels = true
			m.SetLabels = append(m.SetLabels, splitBdLabelList(value)...)
		case "--set-metadata":
			key, v, ok := strings.Cut(value, "=")
			if !ok || strings.TrimSpace(key) == "" {
				return nil, m, fmt.Sprintf("--set-metadata %q is not key=value", value)
			}
			if m.SetMetadata == nil {
				m.SetMetadata = map[string]string{}
			}
			m.SetMetadata[key] = v
		case "--unset-metadata":
			m.UnsetMetadata = append(m.UnsetMetadata, value)
		case "--metadata":
			// bd merges the object into the bead's metadata. @file and
			// malformed bodies were already refused by
			// bdRigQualifiedMetadataRefusal.
			var fields beads.StringMap
			if err := json.Unmarshal([]byte(value), &fields); err != nil {
				return nil, m, fmt.Sprintf("--metadata is not an inline JSON object (%v)", err)
			}
			if m.SetMetadata == nil {
				m.SetMetadata = map[string]string{}
			}
			for k, v := range fields {
				m.SetMetadata[k] = v
			}
		}
	}
	return ids, m, ""
}

// splitBdLabelList splits a label flag value the way bd's cobra StringSlice
// and its label verbs do: on commas, trimming blanks.
func splitBdLabelList(value string) []string {
	var out []string
	for _, l := range strings.Split(value, ",") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// bdPositionals returns the non-flag tokens of argv, failing on a flag that is
// not a known bool flag (these verbs take no value flags of their own).
func bdPositionals(args []string, extraValueFlags ...string) ([]string, string) {
	values := bdflags.GlobalValueFlags()
	for _, f := range extraValueFlags {
		values[f] = true
	}
	bools := bdflags.GlobalBoolFlags()
	bools["-h"], bools["--help"] = true, true
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
		case bools[name]:
		case values[name] && stated:
		case values[name] && i+1 < len(args):
			i++
		default:
			return nil, fmt.Sprintf("flag %q is not one gc knows here", arg)
		}
	}
	return out, ""
}

// bdClassGuardLabel judges `gc bd label add|remove|propagate`.
func bdClassGuardLabel(in bdClassGuardInput) bdClassGuard {
	if in.holds == nil {
		return in.pass()
	}
	pos, problem := bdPositionals(in.verbArgs)
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
		labels := splitBdLabelList(rest[len(rest)-1])
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
		if flip := coordclass.CheckFlip(empty, coordclass.Mutation{AddLabels: splitBdLabelList(rest[1])}.Apply(empty), in.holds); flip != nil {
			return in.refuse("%s", classFlipRefusalText("gc bd label propagate "+rest[0], flip))
		}
		return in.pass()
	default:
		return in.refuse("gc bd label %s: refused: gc does not know this label subcommand, so it cannot tell whether it changes a bead's storage class on this split city. Nothing was written.", sub)
	}
}

// bdClassGuardTag judges `gc bd tag <id> <label>` (bd's --add-label shorthand).
func bdClassGuardTag(in bdClassGuardInput) bdClassGuard {
	if in.holds == nil {
		return in.pass()
	}
	pos, problem := bdPositionals(in.verbArgs)
	if problem != "" {
		return in.refuse("gc bd tag: refused: %s. Nothing was written.", problem)
	}
	if len(pos) != 2 {
		return in.pass()
	}
	return in.checkIDs("tag", pos[:1], coordclass.Mutation{AddLabels: splitBdLabelList(pos[1])})
}

// bdClassGuardSetState judges `gc bd set-state <id> <dimension>=<value>`, which
// swaps the bead's <dimension>:* label for <dimension>:<value>.
func bdClassGuardSetState(in bdClassGuardInput) bdClassGuard {
	if in.holds == nil {
		return in.pass()
	}
	pos, problem := bdPositionals(in.verbArgs, "--reason")
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
	if !read {
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
		return in.refuse("gc bd %s: refused on this split city: %s, and a bead that lands in the work store with an infrastructure class is stranded there (every later gc command refuses until it is repaired). Run bd directly in %s if you have checked what it will write. Nothing was written.", in.verb, reason, in.scopeRoot)
	}
}

// bdClassGuardSQL refuses a write statement through `gc bd sql` on a split
// city: raw DML can set issue_type or labels to anything and bypasses every bd
// write path. Read statements pass (the relocated-class read guard has already
// judged them).
func bdClassGuardSQL(in bdClassGuardInput) bdClassGuard {
	if in.holds == nil {
		return in.pass()
	}
	pos, problem := bdPositionals(in.verbArgs)
	if problem != "" {
		return in.pass() // bd rejects an unknown flag before running anything
	}
	for _, query := range pos {
		if !sqlIsReadOnly(query) {
			return in.refuse("gc bd sql: refused on this split city: only read statements (SELECT, SHOW, DESCRIBE, EXPLAIN) pass through gc bd sql here. A raw write can give a work-store bead an infrastructure type or label, which strands it (every later gc command refuses until it is repaired). Use the gc bd verbs, which gc judges, or run bd directly in %s if you have checked what it will write. Nothing was written.", in.scopeRoot)
		}
	}
	return in.pass()
}

// sqlIsReadOnly reports whether every statement in query starts with a read
// keyword. It fails closed: anything it cannot read as a read is a write.
func sqlIsReadOnly(query string) bool {
	statements := 0
	for _, stmt := range strings.Split(query, ";") {
		stmt = stripLeadingSQLComments(stmt)
		if stmt == "" {
			continue
		}
		statements++
		words := strings.FieldsFunc(stmt, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '('
		})
		if len(words) == 0 {
			return false
		}
		switch strings.ToUpper(words[0]) {
		case "SELECT", "SHOW", "DESCRIBE", "DESC", "EXPLAIN":
		default:
			return false
		}
	}
	return statements > 0
}

// stripLeadingSQLComments trims whitespace and leading -- and /* */ comments.
func stripLeadingSQLComments(s string) string {
	for {
		s = strings.TrimSpace(s)
		switch {
		case strings.HasPrefix(s, "--"), strings.HasPrefix(s, "#"):
			nl := strings.IndexByte(s, '\n')
			if nl < 0 {
				return ""
			}
			s = s[nl+1:]
		case strings.HasPrefix(s, "/*"):
			end := strings.Index(s, "*/")
			if end < 0 {
				return ""
			}
			s = s[end+2:]
		default:
			return s
		}
	}
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
	work, skipped, byClass, problem := splitBdImportRows(data)
	if problem != "" {
		return in.refuse("gc bd import: refused on this split city: %s. Nothing was written.", problem)
	}
	out := bdClassGuard{args: args}
	if skipped == 0 {
		if source == "-" {
			out.stdin = bytes.NewReader(data)
		}
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
	out := make([]string, 0, len(bdArgs))
	found := false
	for _, a := range bdArgs {
		if a == bdImportSkipInfraFlag {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
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

// splitBdImportRows splits a bd export JSONL into the lines to keep (work
// rows and non-issue records such as memories) and counts the
// infrastructure-class issue rows by class.
func splitBdImportRows(data []byte) (keep [][]byte, skipped int, byClass map[coordclass.Class]int, problem string) {
	byClass = map[coordclass.Class]int{}
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
			Kind     string           `json:"_type"`
			Type     string           `json:"issue_type"`
			Labels   []string         `json:"labels"`
			Metadata *beads.StringMap `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, 0, nil, fmt.Sprintf("line %d is not a JSON object gc can classify (%v)", line, err)
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
			continue
		}
		keep = append(keep, keepLine)
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, nil, fmt.Sprintf("reading the file: %v", err)
	}
	return keep, skipped, byClass, ""
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
