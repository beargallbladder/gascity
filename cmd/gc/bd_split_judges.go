package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// The judges of the split-city allowlist's mutating commands. Each reads the
// argv as bd parsed it (bdSplitArgs: slice flags already split as pflag splits
// them) and decides with the class-must-not-flip guard (coordclass.CheckFlip)
// and the cross-store edge rule (checkEdgeEndpoints).

// normalizeBdLabels trims labels and drops empty ones, as bd's
// utils.NormalizeLabels does after pflag split them.
func normalizeBdLabels(values []string) []string {
	var out []string
	for _, l := range values {
		if l = strings.TrimSpace(l); l != "" && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// checkIDs judges a write against every addressed bead. variants are the
// readings of the one write a bd build might make (see bdUpdateMutation); the
// write is refused if ANY of them flips the class. A write that touches no
// field Classify reads passes without a read. Otherwise each id must resolve
// to exactly one bead — the one bd would write — and that bead is judged; an
// id that is absent, resolves to a different bead, or cannot be read is
// refused, because the guard cannot know what the write would do.
func (in bdClassGuardInput) checkIDs(ids []string, variants ...coordclass.Mutation) bdClassGuard {
	touches := false
	for _, m := range variants {
		touches = touches || m.TouchesClass()
	}
	if !touches {
		return in.pass()
	}
	if len(ids) == 0 {
		return in.refuse("gc bd %s: refused on this split city: a write of a bead's type, labels or metadata is judged against the bead it lands on; name the bead explicitly (bd's id-less form writes the last touched issue, which gc cannot read). Nothing was written.", in.path)
	}
	for _, id := range ids {
		before, result := in.read(id)
		if refusal := in.readRefusal(id, result); refusal != "" {
			return in.refuse("%s", refusal)
		}
		for _, m := range variants {
			if flip := coordclass.CheckFlip(before, m.Apply(before), in.holds); flip != nil {
				return in.refuse("%s", classFlipRefusalText(fmt.Sprintf("gc bd %s %s", in.path, id), flip))
			}
		}
	}
	return in.pass()
}

// readRefusal is the refusal for a pre-read that did not find exactly the
// bead id names, or "".
func (in bdClassGuardInput) readRefusal(id string, result bdReadResult) string {
	switch result {
	case bdReadFound:
		return ""
	case bdReadCollision:
		return fmt.Sprintf("gc bd %s %s: refused on this split city: bd resolves %q to a different bead (a short or partial id), so the write would land on a bead gc did not judge. Name the bead by its full id. Nothing was written.", in.path, id, id)
	case bdReadNotFound:
		return fmt.Sprintf("gc bd %s %s: refused on this split city: no bead %q in this store, so gc cannot judge what the write would do. Nothing was written.", in.path, id, id)
	default:
		return fmt.Sprintf("gc bd %s %s: refused on this split city: gc could not read the bead to check the write. Retry once the store answers. Nothing was written.", in.path, id)
	}
}

// checkEdgeEndpoints enforces the cross-store edge rule: every endpoint of an
// edge must be a bead of this work store — found here, exactly — and one no
// class binding holds (a migrated relic keeps a work-shaped id but lives in the
// binding). bd accepts an edge to an id it cannot resolve, and a dep row
// naming a bead in another store is one no Ready computation clears.
func (in bdClassGuardInput) checkEdgeEndpoints(ids ...string) string {
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		_, result := in.read(id)
		if result != bdReadFound {
			return fmt.Sprintf("gc bd %s: refused on this split city: an edge endpoint must be a bead of this work store, and %s. A dependency edge across stores is never written. Nothing was written.", in.path, strings.TrimPrefix(in.readRefusal(id, result), fmt.Sprintf("gc bd %s %s: refused on this split city: ", in.path, id)))
		}
		if in.held == nil {
			return fmt.Sprintf("gc bd %s: refused on this split city: gc cannot check whether %s lives in the class binding. Nothing was written.", in.path, id)
		}
		held, err := in.held(id)
		if err != nil {
			return fmt.Sprintf("gc bd %s: refused on this split city: gc could not check whether %s lives in the class binding (%v). Nothing was written.", in.path, id, err)
		}
		if held {
			return fmt.Sprintf("gc bd %s: refused on this split city: %s lives in the class binding, and a dependency edge across stores is never written. Nothing was written.", in.path, id)
		}
	}
	return ""
}

// bdDepsEndpoints returns the ids a create --deps value names: each element is
// "<id>" or "<type>:<id>".
func bdDepsEndpoints(values []string) []string {
	var out []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if _, id, typed := strings.Cut(v, ":"); typed {
			v = id
		}
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// bdMetadataObject decodes a --metadata value as bd reads it back: an inline
// JSON object, or @file (read relative to the scope root, where bd runs).
func (in bdClassGuardInput) bdMetadataObject(value string) (map[string]string, string) {
	body := []byte(value)
	if path, isFile := strings.CutPrefix(strings.TrimSpace(value), "@"); isFile {
		if !filepath.IsAbs(path) {
			path = filepath.Join(in.scopeRoot, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Sprintf("--metadata %s cannot be read to judge it (%v)", value, err)
		}
		body = data
	}
	var fields beads.StringMap
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Sprintf("--metadata is not a JSON object (%v)", err)
	}
	return fields, ""
}

// judgeBdCreate classifies the bead `create` / `q` would mint and checks the
// edges it would write.
func judgeBdCreate(in bdClassGuardInput) bdClassGuard {
	if len(in.args.Values("--file")) > 0 {
		return in.refuse("gc bd %s --file: refused on this split city: bd's multi-issue markdown is not classified by gc. Create the beads one at a time with flags. Nothing was written.", in.path)
	}
	if id, ok := in.args.Last("--id"); ok && bdIDIsClassReserved(id) { // residency:allow — refuses minting a NEW bead inside a reserved namespace; resolves no bead
		return in.refuse("gc bd %s --id %s: refused on this split city: that id is inside a class binding's reserved namespace, so the bead would answer to a store that never holds it. Nothing was written.", in.path, id)
	}
	var shape beads.Bead
	if t, ok := in.args.Last("--type"); ok {
		shape.Type = strings.TrimSpace(t)
	}
	shape.Labels = normalizeBdLabels(append(slices.Clone(in.args.Values("--labels")), in.args.Values("--label")...))
	if v, ok := in.args.Last("--metadata"); ok {
		fields, problem := in.bdMetadataObject(v)
		if problem != "" {
			return in.refuse("gc bd %s: refused: %s. Nothing was written.", in.path, problem)
		}
		shape.Metadata = map[string]string{}
		maps.Copy(shape.Metadata, fields)
	}
	if plan, ok := in.args.Last("--graph"); ok {
		if class, parsed := bdRelocatedClassGraphPlanClass(in.scopeRoot, plan); parsed && !slices.Contains(in.holds, class) {
			return in.refuse("gc bd %s --graph: refused on this split city: the plan is %s class, which this store does not hold. Mint it with `%s`. Nothing was written.", in.path, class, coordclass.MintCommand(class))
		}
	}
	if flip := coordclass.CheckFlip(beads.Bead{}, shape, in.holds); flip != nil {
		return in.refuse("%s", classFlipRefusalText("gc bd "+in.path, flip))
	}
	var endpoints []string
	endpoints = append(endpoints, bdDepsEndpoints(in.args.Values("--deps"))...)
	endpoints = append(endpoints, in.args.Values("--waits-for")...)
	endpoints = append(endpoints, in.args.Values("--parent")...)
	if refusal := in.checkEdgeEndpoints(endpoints...); refusal != "" {
		return in.refuse("%s", refusal)
	}
	return in.pass()
}

// judgeBdUpdate judges `gc bd update`: its class-field writes against the
// bead, and a reparent as an edge.
func judgeBdUpdate(in bdClassGuardInput) bdClassGuard {
	variants, problem := in.bdUpdateMutation()
	if problem != "" {
		return in.refuse("gc bd update: refused on this split city: %s. Nothing was written.", problem)
	}
	if parent, ok := in.args.Last("--parent"); ok && strings.TrimSpace(parent) != "" {
		if refusal := in.checkEdgeEndpoints(append(slices.Clone(in.args.positionals), parent)...); refusal != "" {
			return in.refuse("%s", refusal)
		}
	}
	return in.checkIDs(in.args.positionals, variants...)
}

// bdUpdateMutation states a parsed `bd update` as the write it makes, in bd's
// order (labels: replace, add, remove; metadata: merge/set, then unset — see
// coordclass.Mutation.Apply). `--set-metadata k=null` is stored differently by
// the bd builds the fleet runs (bd 1.3.2: JSON null, read back absent; 1.3.0
// Enterprise: the string "null"), so it comes back as two variants and the
// write must pass both.
func (in bdClassGuardInput) bdUpdateMutation() ([]coordclass.Mutation, string) {
	var m coordclass.Mutation
	if t, ok := in.args.Last("--type"); ok {
		v := strings.TrimSpace(t)
		m.Type = &v
	}
	m.AddLabels = normalizeBdLabels(in.args.Values("--add-label"))
	m.RemoveLabels = normalizeBdLabels(in.args.Values("--remove-label"))
	if set, ok := in.args.flags["--set-labels"]; ok {
		m.ReplaceLabels = true
		m.SetLabels = normalizeBdLabels(set)
	}
	var nullKeys []string
	if v, ok := in.args.Last("--metadata"); ok {
		fields, problem := in.bdMetadataObject(v)
		if problem != "" {
			return nil, problem
		}
		m.SetMetadata = map[string]string{}
		maps.Copy(m.SetMetadata, fields)
	}
	for _, kv := range in.args.Values("--set-metadata") {
		key, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Sprintf("--set-metadata %q is not key=value", kv)
		}
		if m.SetMetadata == nil {
			m.SetMetadata = map[string]string{}
		}
		m.SetMetadata[key] = v
		if v == "null" {
			nullKeys = append(nullKeys, key)
		}
	}
	m.UnsetMetadata = slices.Clone(in.args.Values("--unset-metadata"))
	variants := []coordclass.Mutation{m}
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
	return variants, ""
}

// judgeBdEdgeVerb applies the cross-store edge rule to the commands whose job
// is an edge: dep add (positionals, --blocked-by, --depends-on; --file
// refused), dep relate, link, duplicate --of, supersede --with, and gate
// create --blocks.
func judgeBdEdgeVerb(in bdClassGuardInput) bdClassGuard {
	if in.path == "dep add" && len(in.args.Values("--file")) > 0 {
		return in.refuse("gc bd dep add --file: refused on this split city: a dependency file's edges are not judged one by one here; add each edge with gc bd dep add <id> <id>. Nothing was written.")
	}
	var endpoints []string
	switch in.path {
	case "gate create":
		endpoints = in.args.Values("--blocks")
	default:
		endpoints = slices.Clone(in.args.positionals)
		for _, f := range []string{"--blocked-by", "--depends-on", "--of", "--with"} {
			endpoints = append(endpoints, in.args.Values(f)...)
		}
	}
	if refusal := in.checkEdgeEndpoints(endpoints...); refusal != "" {
		return in.refuse("%s", refusal)
	}
	return in.pass()
}

// judgeBdConfigSet admits only the keys in bdSplitConfigSetKeys.
func judgeBdConfigSet(in bdClassGuardInput) bdClassGuard {
	if len(in.args.positionals) < 1 {
		return in.refuse("gc bd config set: refused on this split city: expected <key> <value>. Nothing was written.")
	}
	key := in.args.positionals[0]
	if _, ok := bdSplitConfigSetKeys[key]; !ok {
		return in.refuse("gc bd config set %s: refused on this split city: only %s may be set through gc bd here; other keys (storage-class.*, routing.*, dolt.*, backup.*, sync.*, federation.*, ...) can redirect writes or change what create does. Nothing was written.", key, strings.Join(slices.Sorted(maps.Keys(bdSplitConfigSetKeys)), ", "))
	}
	return in.pass()
}

// judgeBdLabelAddRemove judges `label add|remove <id...> <label[,label]>`, which
// split their label argument on plain commas (no CSV quoting).
func judgeBdLabelAddRemove(in bdClassGuardInput) bdClassGuard {
	pos := in.args.positionals
	if len(pos) < 2 {
		return in.refuse("gc bd %s: refused on this split city: expected <id...> <label>. Nothing was written.", in.path)
	}
	labels := bdLabelVerbList(pos[len(pos)-1])
	m := coordclass.Mutation{AddLabels: labels}
	if in.path == "label remove" {
		m = coordclass.Mutation{RemoveLabels: labels}
	}
	return in.checkIDs(pos[:len(pos)-1], m)
}

// judgeBdLabelPropagate refuses a class-marker label pushed to a parent's
// children, whoever they are.
func judgeBdLabelPropagate(in bdClassGuardInput) bdClassGuard {
	pos := in.args.positionals
	if len(pos) != 2 {
		return in.refuse("gc bd label propagate: refused on this split city: expected <parent-id> <label>. Nothing was written.")
	}
	empty := beads.Bead{}
	if flip := coordclass.CheckFlip(empty, coordclass.Mutation{AddLabels: bdLabelVerbList(pos[1])}.Apply(empty), in.holds); flip != nil {
		return in.refuse("%s", classFlipRefusalText("gc bd label propagate "+pos[0], flip))
	}
	return in.pass()
}

// judgeBdTag judges `tag <id> <label>`, which takes its one label trimmed.
func judgeBdTag(in bdClassGuardInput) bdClassGuard {
	pos := in.args.positionals
	if len(pos) != 2 {
		return in.refuse("gc bd tag: refused on this split city: expected <id> <label>. Nothing was written.")
	}
	return in.checkIDs(pos[:1], coordclass.Mutation{AddLabels: []string{strings.TrimSpace(pos[1])}})
}

// judgeBdSetState judges `set-state <id> <dimension>=<value>`, which swaps the
// bead's first <dimension>:* label for <dimension>:<value>.
func judgeBdSetState(in bdClassGuardInput) bdClassGuard {
	pos := in.args.positionals
	if len(pos) != 2 || !strings.Contains(pos[1], "=") {
		return in.refuse("gc bd set-state: refused on this split city: expected <id> <dimension>=<value>. Nothing was written.")
	}
	id := pos[0]
	dimension, value, _ := strings.Cut(pos[1], "=")
	before, result := in.read(id)
	if refusal := in.readRefusal(id, result); refusal != "" {
		return in.refuse("%s", refusal)
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

// judgeBdSQL passes a single plain read statement only (sqlReadOnlyProblem).
func judgeBdSQL(in bdClassGuardInput) bdClassGuard {
	if len(in.args.positionals) != 1 {
		return in.refuse("gc bd sql: refused on this split city: expected exactly one query. Nothing was written.")
	}
	if why := sqlReadOnlyProblem(in.args.positionals[0]); why != "" {
		return in.refuse("gc bd sql: refused on this split city: %s. Only a single plain read statement (SELECT, SHOW, DESCRIBE, EXPLAIN SELECT), with no comments and no Dolt procedures, passes through gc bd sql here: a raw write can give a work-store bead an infrastructure type or label, which strands it. Use the gc bd verbs, which gc judges, or run bd directly in %s if you have checked what it will write. Nothing was written.", why, in.scopeRoot)
	}
	return in.pass()
}

// judgeBdMolWisp admits the one wisp shape a workflow depends on — the gastown
// patrols' `gc bd mol wisp <formula> --root-only [--var k=v]...` — and judges,
// in process, every bead it can create, before anything is written.
//
// With --root-only bd creates exactly ONE bead, the molecule root. bd resolves
// the name in this order (wisp.go): a formula (the root is then type molecule
// with no labels or metadata: bd cookFormulaToSubgraph); else a legacy proto by
// id, partial ids included; else, for a mol-* name, the first template bead
// whose title contains the name. In the proto cases bd clones that bead's type,
// labels and metadata. The guard cannot know whether bd's formula lookup will
// succeed, so it judges every candidate: the formula root, the bead the name
// resolves to (a collision is a refusal), and every template bead a mol-* name
// could match. Without --root-only bd instantiates every step, children and
// expansion templates included, and that shape is refused toward gc's own
// sling/cook paths. TestGcBdMolWispRootOnlyMintsOneWorkBead (an integration
// test against a real bd) pins the root shape.
func judgeBdMolWisp(in bdClassGuardInput) bdClassGuard {
	if !in.args.Bool("--root-only") {
		return in.refuse("gc bd %s: refused on this split city: without --root-only it instantiates every step of the formula, and bd copies each step's type, labels and metadata onto the beads it mints in the work store. Instead: %s. Nothing was written.", in.path, bdSplitAltSling)
	}
	if len(in.args.positionals) != 1 {
		return in.refuse("gc bd %s: refused on this split city: expected exactly one formula or proto. Nothing was written.", in.path)
	}
	target := in.args.positionals[0]
	candidates := []beads.Bead{{Type: "molecule"}}
	proto, result := in.read(target)
	switch result {
	case bdReadNotFound:
	case bdReadFound:
		candidates = append(candidates, proto)
	default:
		return in.refuse("%s", in.readRefusal(target, result))
	}
	if strings.HasPrefix(target, "mol-") {
		if in.templates == nil {
			return in.refuse("gc bd %s %s: refused on this split city: gc cannot list the template beads a mol-* name could resolve to. Nothing was written.", in.path, target)
		}
		templates, err := in.templates()
		if err != nil {
			return in.refuse("gc bd %s %s: refused on this split city: gc could not list the template beads a mol-* name could resolve to (%v). Nothing was written.", in.path, target, err)
		}
		for _, t := range templates {
			if strings.Contains(t.Title, target) || t.ID == target {
				candidates = append(candidates, t)
			}
		}
	}
	for _, c := range candidates {
		root := beads.Bead{ID: c.ID, Type: c.Type, Labels: c.Labels, Metadata: c.Metadata}
		if flip := coordclass.CheckFlip(beads.Bead{}, root, in.holds); flip != nil {
			return in.refuse("%s", classFlipRefusalText(fmt.Sprintf("gc bd %s %s (the proto %s it can clone)", in.path, target, c.ID), flip))
		}
	}
	return in.pass()
}

// judgeBdCook passes `cook` that only prints; --persist writes a proto into the
// work store and is refused.
func judgeBdCook(in bdClassGuardInput) bdClassGuard {
	if in.args.Bool("--persist") {
		return in.refuse("gc bd cook --persist: refused on this split city: it writes the formula's steps into the work store as a proto, with each step's type, labels and metadata. Instead: %s. Nothing was written.", bdSplitAltSling)
	}
	return in.pass()
}

// judgeBdBatch judges `gc bd batch`, whose script (stdin or --file) can mint a
// bead of any type with `create <type> ...` and write edges with `dep add`.
func judgeBdBatch(in bdClassGuardInput) bdClassGuard {
	if len(in.args.positionals) != 0 {
		return in.refuse("gc bd batch: refused on this split city: batch takes no positional arguments. Nothing was written.")
	}
	var script []byte
	var err error
	out := in.pass()
	if path, ok := in.args.Last("--file"); ok {
		if !filepath.IsAbs(path) {
			path = filepath.Join(in.scopeRoot, path)
		}
		script, err = os.ReadFile(path)
	} else {
		script, err = io.ReadAll(in.stdin)
		out.stdin = bytes.NewReader(script)
	}
	if err != nil {
		return in.refuse("gc bd batch: refused on this split city: gc must read the batch script to judge it, and could not: %v. Nothing was written.", err)
	}
	if problem := batchScriptClassProblem(script, in.holds); problem != "" {
		return in.refuse("gc bd batch: refused on this split city: %s. Nothing was written.", problem)
	}
	scanner := bufio.NewScanner(bytes.NewReader(script))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		tokens := tokenizeBdBatchLine(strings.TrimSpace(scanner.Text()))
		if len(tokens) >= 4 && tokens[0] == "dep" && tokens[1] == "add" {
			if refusal := in.checkEdgeEndpoints(tokens[2], tokens[3]); refusal != "" {
				return in.refuse("%s", refusal)
			}
		}
	}
	return out
}

// judgeBdImport judges `gc bd import`, the documented restore path for a JSONL
// archive. A row whose class this store does not hold — every infrastructure
// row of an archive taken before the split — would be stranded by bd's
// upsert. Those rows are not routed to the binding: it already holds the live
// copies (gc storage migrate preserved ids), and replaying an archive over them
// would roll sessions and running graphs back. The import is refused and names
// --skip-infra (gc's own flag), which imports a copy holding only the rows this
// store holds and counts the rest. A dry run writes nothing, so it is reported.
// A kept row's dependency must name a kept row of the file or a bead of this
// work store no binding holds (the cross-store edge rule).
func judgeBdImport(in bdClassGuardInput) bdClassGuard {
	args, skipInfra := stripSkipInfraFlag(in.bdArgs)
	sources := append(slices.Clone(in.args.positionals), in.args.Values("--input")...)
	switch len(sources) {
	case 0:
		return in.refuse("gc bd import: refused on this split city: with no file bd imports its configured import path, which gc does not resolve; name the file (or - for stdin). Nothing was written.")
	case 1:
	default:
		return in.refuse("gc bd import: refused on this split city: more than one import source is named. Nothing was written.")
	}
	source := sources[0]
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
		return in.refuse("gc bd import: refused on this split city: gc must read %s to check which classes it holds, and could not: %v. Nothing was written.", source, err)
	}
	split, problem := splitBdImportRows(data, in.holds)
	if problem != "" {
		return in.refuse("gc bd import: refused on this split city: %s. Nothing was written.", problem)
	}
	dryRun := in.args.Bool("--dry-run")
	if split.skipped > 0 && !skipInfra && !dryRun {
		return in.refuse("gc bd import: refused on this split city: %s holds %d row(s) of a class this store does not hold (%s). bd import would strand every one in the work store, and every later gc command would refuse until they were repaired. They are not routed to the class store either: it already holds the live copies, and replaying an archive over them would roll sessions and running graphs back. Re-run with %s to import only the %d row(s) this store holds; the rest are left out and counted. Nothing was written.",
			source, split.skipped, formatClassCounts(split.byClass), bdImportSkipInfraFlag, len(split.keep))
	}
	if !dryRun {
		if refusal := in.checkImportEdges(split); refusal != "" {
			return in.refuse("%s", refusal)
		}
	}
	out := bdClassGuard{args: args}
	if split.skipped == 0 {
		if source == "-" {
			out.stdin = bytes.NewReader(data)
		}
		return out
	}
	if dryRun && !skipInfra {
		if source == "-" {
			out.stdin = bytes.NewReader(data)
		}
		out.notice = fmt.Sprintf("gc bd import: dry run: %s holds %d row(s) of a class this store does not hold (%s); on this split city the real import is refused unless re-run with %s, which imports the %d other row(s) only\n",
			source, split.skipped, formatClassCounts(split.byClass), bdImportSkipInfraFlag, len(split.keep))
		return out
	}
	filtered := bytes.Join(split.keep, []byte("\n"))
	if len(filtered) > 0 {
		filtered = append(filtered, '\n')
	}
	out.notice = fmt.Sprintf("gc bd import: %s: importing %d row(s); left out %d row(s) of a class this store does not hold (%s), which this split city keeps in its class store\n",
		source, len(split.keep), split.skipped, formatClassCounts(split.byClass))
	if source == "-" {
		out.stdin = bytes.NewReader(filtered)
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
		return in.refuse("gc bd import: refused: could not write the filtered copy of %s: %v. Nothing was written.", source, err)
	}
	redirected, ok := bdImportRedirect(args, source, tmp.Name())
	if !ok {
		_ = os.Remove(tmp.Name())
		return in.refuse("gc bd import: refused on this split city: gc could not point bd at the filtered copy of %s (spell the source as a plain argument or --input <file>). Nothing was written.", source)
	}
	out.args = redirected
	name := tmp.Name()
	out.cleanup = func() { _ = os.Remove(name) }
	return out
}

// checkImportEdges applies the cross-store edge rule to the kept rows'
// dependencies: an endpoint must be a kept row of the file, or a bead of this
// work store no binding holds.
func (in bdClassGuardInput) checkImportEdges(split bdImportSplit) string {
	inFile := map[string]bool{}
	for _, id := range split.keptIDs {
		inFile[id] = true
	}
	checked := map[string]bool{}
	for _, dep := range split.keptDeps {
		if inFile[dep] || checked[dep] {
			continue
		}
		checked[dep] = true
		if refusal := in.checkEdgeEndpoints(dep); refusal != "" {
			return refusal
		}
	}
	return ""
}

// bdImportRedirect points the one token of argv that names source (the
// positional, `-i x`, `-ix`, `-i=x`, `--input x`, `--input=x`) at path,
// re-reading argv with import's flags so a flag value that happens to equal
// source is never rewritten.
func bdImportRedirect(argv []string, source, path string) ([]string, bool) {
	out := slices.Clone(argv)
	verbSeen := false
	for i := 0; i < len(out); i++ {
		tok := out[i]
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			if !verbSeen {
				verbSeen = tok == "import"
				continue
			}
			if tok == source {
				out[i] = path
				return out, true
			}
			continue
		}
		name, _, inline := strings.Cut(tok, "=")
		switch {
		case name == "--input" || name == "-i":
			switch {
			case inline:
				out[i] = name + "=" + path
			case i+1 < len(out):
				out[i+1] = path
			default:
				return nil, false
			}
			return out, true
		case strings.HasPrefix(tok, "-i") && !strings.HasPrefix(tok, "--"):
			out[i] = "-i" + path
			return out, true
		}
		// Import's other flags are bools or globals; a global value flag
		// consumes the next token.
		if !inline && bdImportGlobalTakesValue(name) {
			i++
		}
	}
	return nil, false
}

// bdImportGlobalTakesValue reports whether a global flag consumes the next
// token.
func bdImportGlobalTakesValue(name string) bool {
	for _, g := range bdflags.GlobalFlagSpecs() {
		if g.Long == name || (g.Short != "" && g.Short == name) {
			return g.TakesValue
		}
	}
	return false
}
