package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
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

// On a split city `gc bd` is an ALLOWLIST. bd writes whatever it is told into
// the work store, and a bead that lands there with an infrastructure class is
// stranded: every later routed gc command refuses until the fleet is stopped
// and the row repaired. A denylist of the verbs that can do that kept growing
// with every review, so the owner decided the other way round: gc bd forwards
// only the (verb, subverb) pairs in bdSplitAllowlist, each with a vetted flag
// set, parsed exactly as bd's pflag parses them (bdflags.ParseArgs). Anything
// else — an unknown verb, an alias that is not explicitly mapped, a subverb, a
// flag, or a positional shape the guard cannot judge — is refused with a
// pointer to the gc-native command. The verbs that can change a bead's class
// carry a judge built on the one class-must-not-flip guard
// (coordclass.CheckFlip).
//
// Single-store cities never reach any of this: storeClassHolds answers nil
// there and gc bd forwards argv unchanged.
//
// TestGcBdSplitAllowlistDecidesEveryCommand holds every command path and alias
// in the pinned bd inventory (internal/bdflags/commands_gen.go) to a decision
// here: allowlisted, or refused with a stated alternative. The docs list the
// allowlist in docs/runbooks/split-storage-classes.md ("gc bd on split
// cities"), which TestGcBdSplitAllowlistIsDocumented keeps in step.

// bdSplitJudge decides an allowlisted command that can change a bead's class.
type bdSplitJudge func(in bdClassGuardInput) bdClassGuard

// bdSplitCommand is one vetted command path.
type bdSplitCommand struct {
	// only, when non-nil, is the complete set of the command's own flags
	// (long names) that are vetted; every other flag of it is refused. nil
	// vets every flag bd declares for the command except those in exclude.
	only    []string
	exclude []string
	// globals are global flags this command additionally vets beyond
	// bdSplitVettedGlobals (init's --db, which names the database it creates).
	globals []string
	// judge, when set, decides the command; nil means it writes no field
	// coordclass.Classify reads.
	judge bdSplitJudge
	// why says what makes the command safe to forward.
	why string
}

const (
	bdSplitAltSling   = "gc sling <target> <formula> or gc formula cook <formula>, which put every bead in the store that holds its class"
	bdSplitAltRestore = "the split-city restore path in examples/bd/dolt/docs/journal-corruption-recovery.md (restore into a scratch scope, bd export, then gc bd import <file> --skip-infra)"
	bdSplitAltRequest = "if a workflow needs it, request an addition to the allowlist (docs/runbooks/split-storage-classes.md, \"gc bd on split cities\"); meanwhile run bd directly in the scope after checking what it writes"
	bdSplitAltBulk    = "stop the city and run bd directly in the scope; the next gc start re-runs the containment check over the work store"
)

// bdSplitAllowlist is every command path gc bd forwards on a split city.
var bdSplitAllowlist = map[string]bdSplitCommand{
	// Reads.
	"show":               {why: "read"},
	"list":               {why: "read (the relocated-class read guard runs first)"},
	"ready":              {why: "read (refused earlier by the relocated-class read guard unless overridden)"},
	"search":             {why: "read"},
	"query":              {why: "read (the relocated-class read guard runs first)"},
	"count":              {why: "read"},
	"children":           {why: "read"},
	"blocked":            {why: "read"},
	"stale":              {why: "read"},
	"status":             {why: "read"},
	"statuses":           {why: "read"},
	"types":              {why: "read"},
	"where":              {why: "read"},
	"info":               {why: "read"},
	"context":            {why: "read"},
	"version":            {why: "read"},
	"help":               {why: "help text"},
	"human":              {why: "help text"},
	"prime":              {why: "prints workflow context"},
	"history":            {why: "read"},
	"graph":              {why: "read"},
	"export":             {why: "writes JSONL out of the ledger"},
	"state":              {why: "read"},
	"comments":           {why: "lists comments"},
	"comments list":      {why: "lists comments"},
	"state list":         {why: "read"},
	"label list":         {why: "read"},
	"label list-all":     {why: "read"},
	"dep list":           {why: "read"},
	"dep tree":           {why: "read"},
	"dep cycles":         {why: "read"},
	"mol current":        {why: "read (binding residents are refused by the by-id door)"},
	"mol show":           {why: "read"},
	"mol progress":       {why: "read"},
	"formula list":       {why: "read"},
	"formula show":       {why: "read"},
	"gate list":          {why: "read"},
	"config get":         {why: "read"},
	"config list":        {why: "read"},
	"config show":        {why: "read"},
	"backup status":      {why: "read"},
	"dolt status":        {why: "read"},
	"dolt show":          {why: "read"},
	"release-if-current": {why: "gc's own verb: a conditional assignee release, parsed and served by gc"},

	// Writes that touch no field Classify reads.
	"close":        {why: "status and close reason"},
	"reopen":       {why: "status"},
	"delete":       {why: "removes beads; a deleted bead has no class to strand"},
	"assign":       {why: "assignee"},
	"priority":     {why: "priority"},
	"note":         {why: "appends notes"},
	"comment":      {why: "adds a comment row"},
	"comments add": {why: "adds a comment row"},
	"heartbeat":    {why: "pushes the claim lease forward"},
	"unclaim":      {why: "status and assignee"},
	"reclaim":      {why: "returns stale in_progress beads to ready"},
	"defer":        {why: "status and defer_until"},
	"undefer":      {why: "status and defer_until"},
	"link":         {why: "a dependency edge (its -t is the edge type)"},
	"dep add":      {why: "a dependency edge (its -t is the edge type)"},
	"dep remove":   {why: "a dependency edge"},
	"dep relate":   {why: "a relates-to edge"},
	"dep unrelate": {why: "a relates-to edge"},
	"promote":      {why: "moves a wisp row to the durable table with its type, labels and metadata unchanged"},
	"duplicate":    {why: "closes the bead and records an edge"},
	"supersede":    {why: "closes the bead and records an edge"},
	"purge":        {why: "deletes closed ephemeral beads (the core reaper)"},
	"prune":        {why: "deletes old closed beads (the core reaper)"},
	"gate create":  {why: "mints a type=gate bead (work class); its --type is the gate's await type, not the bead type"},
	"gate resolve": {why: "closes a gate"},
	"gate check":   {why: "resolves and escalates gates (the core gate-sweep order)"},
	"mol burn":     {why: "closes or deletes a molecule (gastown patrols); binding residents are refused by the by-id door"},
	"backup sync":  {why: "pushes the store to its backup; writes nothing into the store"},
	"backup init":  {why: "configures the backup destination (the dolt pack's backup order)"},
	"dolt start":   {why: "starts the scope's server"},
	"dolt stop":    {why: "stops the scope's server"},
	"dolt push":    {why: "pushes to the remote; writes nothing into the store"},
	"dolt commit":  {why: "commits the working set already written"},
	"config set":   {why: "configuration, not bead rows"},
	"init": {
		only:    []string{"--prefix", "--non-interactive", "--skip-hooks", "--skip-agents", "--init-if-missing", "--recreate-missing"},
		globals: []string{"--db"},
		why:     "creates an empty store (the journal-corruption runbook); --from-jsonl and the remote/reset flags are not vetted",
	},

	// Writes judged by the class-must-not-flip guard.
	"create":          {judge: judgeBdCreate, why: "judged: the bead's type, labels and metadata are classified"},
	"q":               {judge: judgeBdCreate, why: "judged: the bead's type and labels are classified"},
	"update":          {judge: judgeBdUpdate, why: "judged: type/label/metadata writes are classified against the bead they land on"},
	"label add":       {judge: judgeBdLabelAddRemove, why: "judged against the bead"},
	"label remove":    {judge: judgeBdLabelAddRemove, why: "judged against the bead"},
	"label propagate": {judge: judgeBdLabelPropagate, why: "judged: a class-marker label is refused"},
	"tag":             {judge: judgeBdTag, why: "judged against the bead"},
	"set-state":       {judge: judgeBdSetState, why: "judged against the bead"},
	"import":          {judge: judgeBdImport, why: "judged: rows of a class the store does not hold are refused or left out (--skip-infra)"},
	"batch":           {judge: judgeBdBatch, why: "judged: every create line is classified; update lines may set only class-free keys"},
	"sql":             {judge: judgeBdSQL, why: "judged: a single plain read statement only"},
	"mol wisp": {
		only:  []string{"--var", "--root-only"},
		judge: judgeBdMolWisp,
		why:   "gastown patrols; root-only only: the one bead it creates is judged in process",
	},
	"mol wisp create": {
		only:  []string{"--var", "--root-only"},
		judge: judgeBdMolWisp,
		why:   "as mol wisp",
	},
	"cook": {judge: judgeBdCook, why: "prints a resolved formula; --persist (which writes a proto) is refused"},
}

// bdSplitAllowedAliases are the aliases gc bd forwards on a split city, each
// mapped to the allowlisted path bd runs it as. Every other alias in the
// inventory is refused (bdSplitRefusedAliases).
var bdSplitAllowedAliases = map[string]string{
	"new":    "create",
	"done":   "close",
	"view":   "show",
	"hb":     "heartbeat",
	"stats":  "status",
	"dep rm": "dep remove",
}

// bdSplitRefusedAliases are the aliases refused on a split city, and why.
var bdSplitRefusedAliases = map[string]string{
	"protomolecule":      "an alias of mol; use mol",
	"mol fart":           "an alias of mol bond, which is refused",
	"find-dups":          "an alias of find-duplicates, which is not allowlisted",
	"formula primitives": "an alias of formula schema, which is not allowlisted",
	"backup add":         "an alias of backup init; use backup init",
	"backup rm":          "an alias of backup remove, which is not allowlisted",
}

// bdSplitRefusal is the decision for a command path that is not allowlisted.
type bdSplitRefusal struct {
	why string
	alt string
}

// bdSplitRefusals decides every command path that is not allowlisted. A key is
// a path ("backup restore") or a whole verb ("jira"), which decides every path
// under it that is neither allowlisted nor decided more specifically.
var bdSplitRefusals = map[string]bdSplitRefusal{
	"mol pour":          {"it instantiates every step of a formula or proto, and bd copies each step's type, labels and metadata onto the beads it mints", bdSplitAltSling},
	"mol bond":          {"it instantiates a formula or proto into an existing molecule", bdSplitAltSling},
	"mol":               {"molecule maintenance outside the patrol verbs is not vetted", bdSplitAltSling},
	"mol wisp gc":       {"bulk wisp cleanup is not vetted", bdSplitAltRequest},
	"mol wisp list":     {"not vetted", bdSplitAltRequest},
	"backup restore":    {"it replaces the work store with the backup, and a backup taken before the split brings back every infrastructure row of that time; gc cannot verify a backup's age against the split", bdSplitAltRestore},
	"backup remove":     {"not vetted", bdSplitAltRequest},
	"backup":            {"not vetted", bdSplitAltRequest},
	"dolt pull":         {"it merges another revision into the work store wholesale, which gc cannot judge row by row", bdSplitAltBulk},
	"dolt":              {"server and remote administration beyond start/stop/status/show/push/commit is not vetted", bdSplitAltBulk},
	"vc":                {"a merge brings another revision into the work store wholesale", bdSplitAltBulk},
	"federation":        {"a federation sync merges a peer's rows into the work store wholesale", bdSplitAltBulk},
	"sync":              {"it pulls and merges another revision into the work store wholesale", bdSplitAltBulk},
	"branch":            {"switching or creating branches changes which revision the work store serves", bdSplitAltBulk},
	"conflicts":         {"resolving merge conflicts writes whichever side wins, unjudged", bdSplitAltBulk},
	"restore":           {"not vetted", bdSplitAltRequest},
	"migrate":           {"schema and backend migrations are not vetted", bdSplitAltBulk},
	"migrate-personal":  {"not vetted", bdSplitAltRequest},
	"admin":             {"database administration (reset, cleanup) is not vetted", bdSplitAltBulk},
	"compact":           {"not vetted", bdSplitAltBulk},
	"flatten":           {"not vetted", bdSplitAltBulk},
	"gc":                {"not vetted", bdSplitAltBulk},
	"rename":            {"not vetted", bdSplitAltRequest},
	"rename-prefix":     {"not vetted", bdSplitAltBulk},
	"recompute-blocked": {"not vetted", bdSplitAltRequest},
	"bootstrap":         {"not vetted", bdSplitAltBulk},
	"doctor":            {"its repairs write the store unjudged", bdSplitAltRequest},
	"serve":             {"bd serve exposes bd's write API to any HTTP client, and none of those writes passes through gc", "the gc API (gc dashboard / the supervisor API), which routes writes by class"},
	"connect":           {"it attaches the workspace to a bd serve gc does not route", bdSplitAltRequest},
	"create-form":       {"it asks for the bead's fields interactively, so gc cannot classify the bead before bd writes it", "gc bd create with flags"},
	"edit":              {"it edits through $EDITOR, which gc cannot judge", "gc bd update with flags"},
	"todo":              {"not vetted", "gc bd create / gc bd close"},
	"epic":              {"not vetted", bdSplitAltRequest},
	"swarm":             {"it creates a swarm molecule from an epic", bdSplitAltSling},
	"merge-slot":        {"not vetted", bdSplitAltRequest},
	"gate":              {"gate administration beyond create/resolve/check/list is not vetted", bdSplitAltRequest},
	"ship":              {"not vetted", bdSplitAltRequest},
	"provenance":        {"not vetted", bdSplitAltRequest},
	"events":            {"not vetted", bdSplitAltRequest},
	"audit":             {"not vetted", bdSplitAltRequest},
	"kv":                {"not vetted", bdSplitAltRequest},
	"config":            {"configuration changes beyond get/set/list/show are not vetted", bdSplitAltRequest},
	"remember":          {"not vetted", bdSplitAltRequest},
	"forget":            {"not vetted", bdSplitAltRequest},
	"memories":          {"not vetted", bdSplitAltRequest},
	"recall":            {"not vetted", bdSplitAltRequest},
	"formula":           {"not vetted", bdSplitAltRequest},
	"label":             {"not vetted", bdSplitAltRequest},
	"dep":               {"not vetted", bdSplitAltRequest},
	"graph check":       {"not vetted", bdSplitAltRequest},
	"human dismiss":     {"not vetted", bdSplitAltRequest},
	"human respond":     {"not vetted", bdSplitAltRequest},
	"human list":        {"not vetted", bdSplitAltRequest},
	"human stats":       {"not vetted", bdSplitAltRequest},
	"jira":              {"an external-tracker sync writes the tracker's labels and types onto work-store beads unjudged", bdSplitAltRequest},
	"linear":            {"an external-tracker sync writes the tracker's labels and types onto work-store beads unjudged", bdSplitAltRequest},
	"github":            {"an external-tracker sync writes the tracker's labels and types onto work-store beads unjudged", bdSplitAltRequest},
	"gitlab":            {"an external-tracker sync writes the tracker's labels and types onto work-store beads unjudged", bdSplitAltRequest},
	"ado":               {"an external-tracker sync writes the tracker's labels and types onto work-store beads unjudged", bdSplitAltRequest},
	"notion":            {"an external-tracker sync writes the tracker's labels and types onto work-store beads unjudged", bdSplitAltRequest},
	"mail":              {"it delegates to an external mail program", "gc mail"},
	"find-duplicates":   {"not vetted", bdSplitAltRequest},
	"duplicates":        {"it can merge duplicates", bdSplitAltRequest},
	"lint":              {"not vetted", bdSplitAltRequest},
	"orphans":           {"not vetted", bdSplitAltRequest},
	"preflight":         {"not vetted", bdSplitAltRequest},
	"diff":              {"not vetted", bdSplitAltRequest},
	"schema":            {"not vetted", bdSplitAltRequest},
	"hooks":             {"not vetted", bdSplitAltRequest},
	"setup":             {"not vetted", bdSplitAltRequest},
	"onboard":           {"not vetted", bdSplitAltRequest},
	"quickstart":        {"not vetted", bdSplitAltRequest},
	"init-safety":       {"not vetted", bdSplitAltRequest},
	"completion":        {"not vetted", bdSplitAltRequest},
	"metrics":           {"not vetted", bdSplitAltRequest},
	"upgrade":           {"not vetted", bdSplitAltRequest},
	"worktree":          {"not vetted", bdSplitAltRequest},
	"repo":              {"not vetted", bdSplitAltRequest},
	"rules":             {"not vetted", bdSplitAltRequest},
	"ping":              {"not vetted", bdSplitAltRequest},
	"companion":         {"not vetted", bdSplitAltRequest},
	"getToken":          {"not vetted", bdSplitAltRequest},
	"login":             {"not vetted", bdSplitAltRequest},
	"logout":            {"not vetted", bdSplitAltRequest},
	"profile":           {"not vetted", bdSplitAltRequest},
	"whoami":            {"not vetted", bdSplitAltRequest},
}

// bdSplitRefusedGlobals are bd's global flags that point bd at a store other
// than the one gc resolved and judged; they are refused on a split city.
var bdSplitRefusedGlobals = map[string]bool{
	"--db": true, "--database": true, "--profile": true, "--server-url": true, "--global": true,
}

// bdSplitRefusalFor returns the decision for a path that is not allowlisted:
// its own, else its verb's.
func bdSplitRefusalFor(path string) (bdSplitRefusal, bool) {
	if r, ok := bdSplitRefusals[path]; ok {
		return r, true
	}
	verb, _, _ := strings.Cut(path, " ")
	r, ok := bdSplitRefusals[verb]
	return r, ok
}

// bdSplitFlagSpecs returns the vetted flag specs for an allowlisted command:
// the vetted globals, the command's vetted flags, and gc's own extras.
func bdSplitFlagSpecs(path string, cmd bdSplitCommand) []bdflags.FlagSpec {
	var specs []bdflags.FlagSpec
	for _, g := range bdflags.GlobalFlagSpecs() {
		if bdSplitRefusedGlobals[g.Long] && !slices.Contains(cmd.globals, g.Long) {
			continue
		}
		specs = append(specs, g)
	}
	for _, f := range bdflags.CommandFlagSpecs(path) {
		if cmd.only != nil && !slices.Contains(cmd.only, f.Long) {
			continue
		}
		if slices.Contains(cmd.exclude, f.Long) {
			continue
		}
		specs = append(specs, f)
	}
	return specs
}

// bdSplitResolution is a gc bd argv resolved against the allowlist.
type bdSplitResolution struct {
	path    string
	cmd     bdSplitCommand
	args    bdflags.ParsedArgs
	refusal string
}

// resolveBdSplitCommand resolves bdArgs to an allowlisted command, parsing the
// whole argv as bd's pflag would, or returns the refusal.
func resolveBdSplitCommand(bdArgs []string, scopeRoot string) bdSplitResolution {
	refuse := func(format string, a ...any) bdSplitResolution {
		return bdSplitResolution{refusal: fmt.Sprintf(format, a...)}
	}
	globals := bdflags.GlobalFlagSpecs()
	// Leading global flags, then the verb.
	i := 0
	for i < len(bdArgs) && strings.HasPrefix(bdArgs[i], "-") && bdArgs[i] != "-" {
		if bdArgs[i] == "--" {
			return refuse("gc bd: refused on this split city: `--` before the verb leaves the verb undecidable. Nothing was written.")
		}
		width, err := bdflags.FlagTokenWidth(globals, bdArgs[i:])
		if err != nil {
			return refuse("gc bd: refused on this split city: %v before the verb; gc forwards only vetted commands and flags here (%s). Nothing was written.", err, bdSplitAltRequest)
		}
		i += width
	}
	leading := bdArgs[:i]
	if i == len(bdArgs) {
		// No verb: bd prints its help or version.
		if _, err := bdflags.ParseArgs(bdSplitGlobalsOnly(), leading); err != nil {
			return refuse("gc bd: refused on this split city: %v. Nothing was written.", err)
		}
		return bdSplitResolution{path: ""}
	}
	verb := bdArgs[i]
	path := verb
	rest := bdArgs[i+1:]
	if target, aliased := bdflags.CommandAliases()[path]; aliased {
		mapped, ok := bdSplitAllowedAliases[path]
		if !ok {
			return refuse("gc bd %s: refused on this split city: %s is %s. gc forwards only explicitly vetted aliases here. Nothing was written.", path, path, bdSplitRefusedAliases[path])
		}
		_ = target
		path = mapped
	}
	if !bdflags.IsCommandPath(path) && path != "release-if-current" {
		return refuse("gc bd %s: refused on this split city: %q is not a bd command gc knows, and gc forwards only vetted commands here (%s). Nothing was written.", verb, verb, bdSplitAltRequest)
	}
	// Descend into subcommands: the subverb must follow its parent directly.
	for len(bdflags.SubcommandsOf(path)) > 0 && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		candidate := path + " " + rest[0]
		if mapped, aliased := bdSplitAllowedAliases[candidate]; aliased {
			path, rest = mapped, rest[1:]
			continue
		}
		if why, aliased := bdSplitRefusedAliases[candidate]; aliased {
			return refuse("gc bd %s: refused on this split city: %s is %s. gc forwards only explicitly vetted aliases here. Nothing was written.", candidate, rest[0], why)
		}
		if !bdflags.IsCommandPath(candidate) {
			break
		}
		path, rest = candidate, rest[1:]
	}
	cmd, allowed := bdSplitAllowlist[path]
	if !allowed {
		r, decided := bdSplitRefusalFor(path)
		if !decided {
			r = bdSplitRefusal{"it has no decision", bdSplitAltRequest}
		}
		return refuse("gc bd %s: refused on this split city: %s. Instead: %s. Nothing was written.", path, r.why, strings.ReplaceAll(r.alt, "the scope", scopeRoot))
	}
	if subs := bdflags.SubcommandsOf(path); len(subs) > 0 {
		// A subcommand name anywhere in the remaining positionals means bd may
		// run that subcommand (cobra finds it past interleaved flags); the
		// guard would be judging a different command.
		parsed, err := bdflags.ParseArgs(bdSplitFlagSpecs(path, cmd), rest)
		if err == nil {
			for _, p := range parsed.Positionals {
				if slices.Contains(subs, p) {
					return refuse("gc bd %s: refused on this split city: %q names a subcommand of %s; put it directly after %s, with no flag in between. Nothing was written.", path, p, path, path)
				}
			}
		}
	}
	args := append(slices.Clone(leading), rest...)
	if path == "import" {
		args, _ = stripSkipInfraFlag(args)
	}
	parsed, err := bdflags.ParseArgs(bdSplitFlagSpecs(path, cmd), args)
	if err != nil {
		return refuse("gc bd %s: refused on this split city: %v — gc forwards only vetted flags for %s here (%s). Nothing was written.", path, err, path, bdSplitAltRequest)
	}
	return bdSplitResolution{path: path, cmd: cmd, args: parsed}
}

// bdSplitGlobalsOnly are the vetted global flags.
func bdSplitGlobalsOnly() []bdflags.FlagSpec {
	var specs []bdflags.FlagSpec
	for _, g := range bdflags.GlobalFlagSpecs() {
		if !bdSplitRefusedGlobals[g.Long] {
			specs = append(specs, g)
		}
	}
	return specs
}

// bdReadResult is what a pre-read of the bead a write lands on found.
type bdReadResult int

const (
	// bdReadFound: exactly the bead the id names was read.
	bdReadFound bdReadResult = iota
	// bdReadNotFound: the store answered that no bead has this id.
	bdReadNotFound
	// bdReadCollision: bd resolved the id (a short or partial one) to a
	// DIFFERENT bead. bd's write would land on that bead, so it is a
	// refusal, never a not-found.
	bdReadCollision
	// bdReadFailed: the store could not answer.
	bdReadFailed
)

// bdClassGuardDeps are the guard's views of the world, injected so the guard
// itself runs nothing.
type bdClassGuardDeps struct {
	// stdin is what bd would read; the guard reads it for import - and batch.
	stdin io.Reader
	// read returns the bead id resolves to, collision-aware.
	read func(id string) (beads.Bead, bdReadResult)
}

// bdClassGuardInput is everything a judge may consult.
type bdClassGuardInput struct {
	path      string
	args      bdflags.ParsedArgs
	bdArgs    []string
	holds     []coordclass.Class
	scopeRoot string
	bdClassGuardDeps
}

func (in bdClassGuardInput) refuse(format string, a ...any) bdClassGuard {
	return bdClassGuard{refusal: fmt.Sprintf(format, a...)}
}

func (in bdClassGuardInput) pass() bdClassGuard { return bdClassGuard{args: in.bdArgs} }

// bdClassFlipGuard is gc bd's split-city gate: the allowlist, then the judge
// of the allowlisted command. It runs after the by-id door, which has already
// served or refused every binding resident. A single-store city passes
// everything through unchanged (gc's own --skip-infra on import is dropped).
func bdClassFlipGuard(cfg *config.City, scopeRoot string, bdArgs []string, deps bdClassGuardDeps) bdClassGuard {
	holds := workStoreClassHolds(cfg)
	if holds == nil {
		if verb, _, ok := bdRelocatedClassVerb(bdArgs); ok && verb == "import" {
			args, _ := stripSkipInfraFlag(bdArgs)
			return bdClassGuard{args: args}
		}
		return bdClassGuard{args: bdArgs}
	}
	res := resolveBdSplitCommand(bdArgs, scopeRoot)
	if res.refusal != "" {
		return bdClassGuard{refusal: res.refusal}
	}
	if res.cmd.judge == nil {
		return bdClassGuard{args: bdArgs}
	}
	return res.cmd.judge(bdClassGuardInput{
		path: res.path, args: res.args, bdArgs: bdArgs, holds: holds, scopeRoot: scopeRoot, bdClassGuardDeps: deps,
	})
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
		return fmt.Sprintf("gc bd %s %s: refused on this split city: no bead %q in this store, so gc cannot judge what a type, label or metadata write would do. Nothing was written.", in.path, id, id)
	default:
		return fmt.Sprintf("gc bd %s %s: refused on this split city: gc could not read the bead to check that this write leaves its storage class alone. Retry once the store answers. Nothing was written.", in.path, id)
	}
}

// judgeBdCreate classifies the bead `create` / `q` would mint.
func judgeBdCreate(in bdClassGuardInput) bdClassGuard {
	if len(in.args.Values("--file")) > 0 {
		return in.refuse("gc bd %s --file: refused on this split city: bd's multi-issue markdown is not classified by gc. Create the beads one at a time with flags. Nothing was written.", in.path)
	}
	var shape beads.Bead
	for _, f := range in.args.Flags {
		switch f.Long {
		case "--type":
			shape.Type = strings.TrimSpace(f.Value)
		case "--labels", "--label":
			labels, err := bdLabelFlagValues(f.Value)
			if err != nil {
				return in.refuse("gc bd %s: refused: %s %q is not a label list bd can parse (%v). Nothing was written.", in.path, f.Long, f.Value, err)
			}
			shape.Labels = append(shape.Labels, labels...)
		case "--metadata":
			fields, problem := bdMetadataObject(f.Value)
			if problem != "" {
				return in.refuse("gc bd %s: refused: %s. Nothing was written.", in.path, problem)
			}
			if shape.Metadata == nil {
				shape.Metadata = map[string]string{}
			}
			maps.Copy(shape.Metadata, fields)
		case "--graph":
			if class, parsed := bdRelocatedClassGraphPlanClass(in.scopeRoot, f.Value); parsed && !slices.Contains(in.holds, class) {
				return in.refuse("gc bd %s --graph: refused on this split city: the plan is %s class, which this store does not hold. Mint it with `%s`. Nothing was written.", in.path, class, coordclass.MintCommand(class))
			}
		}
	}
	if flip := coordclass.CheckFlip(beads.Bead{}, shape, in.holds); flip != nil {
		return in.refuse("%s", classFlipRefusalText("gc bd "+in.path, flip))
	}
	return in.pass()
}

// bdMetadataObject decodes an inline --metadata object as bd reads it back.
func bdMetadataObject(value string) (map[string]string, string) {
	if strings.HasPrefix(strings.TrimSpace(value), "@") {
		return nil, "--metadata @file cannot be judged before the write"
	}
	var fields beads.StringMap
	if err := json.Unmarshal([]byte(value), &fields); err != nil {
		return nil, fmt.Sprintf("--metadata is not an inline JSON object (%v)", err)
	}
	return fields, ""
}

// judgeBdUpdate judges `gc bd update`.
func judgeBdUpdate(in bdClassGuardInput) bdClassGuard {
	variants, problem := bdUpdateMutation(in.args)
	if problem != "" {
		return in.refuse("gc bd update: refused on this split city: %s. Nothing was written.", problem)
	}
	return in.checkIDs(in.args.Positionals, variants...)
}

// bdUpdateMutation states a parsed `bd update` as the write it makes, in bd's
// order (labels: replace, add, remove; metadata: merge/set, then unset — see
// coordclass.Mutation.Apply). `--set-metadata k=null` is stored differently by
// the bd builds the fleet runs (bd 1.3.2: JSON null, read back absent; 1.3.0
// Enterprise: the string "null"), so it comes back as two variants and the
// write must pass both.
func bdUpdateMutation(args bdflags.ParsedArgs) ([]coordclass.Mutation, string) {
	var m coordclass.Mutation
	var nullKeys []string
	for _, f := range args.Flags {
		switch f.Long {
		case "--type":
			v := strings.TrimSpace(f.Value)
			m.Type = &v
		case "--add-label", "--remove-label", "--set-labels":
			labels, err := bdLabelFlagValues(f.Value)
			if err != nil {
				return nil, fmt.Sprintf("%s %q is not a label list bd can parse (%v)", f.Long, f.Value, err)
			}
			switch f.Long {
			case "--remove-label":
				m.RemoveLabels = append(m.RemoveLabels, labels...)
			case "--set-labels":
				m.ReplaceLabels = true
				m.SetLabels = append(m.SetLabels, labels...)
			default:
				m.AddLabels = append(m.AddLabels, labels...)
			}
		case "--set-metadata":
			key, v, ok := strings.Cut(f.Value, "=")
			if !ok || strings.TrimSpace(key) == "" {
				return nil, fmt.Sprintf("--set-metadata %q is not key=value", f.Value)
			}
			if m.SetMetadata == nil {
				m.SetMetadata = map[string]string{}
			}
			m.SetMetadata[key] = v
			if v == "null" {
				nullKeys = append(nullKeys, key)
			}
		case "--unset-metadata":
			m.UnsetMetadata = append(m.UnsetMetadata, f.Value)
		case "--metadata":
			fields, problem := bdMetadataObject(f.Value)
			if problem != "" {
				return nil, problem
			}
			if m.SetMetadata == nil {
				m.SetMetadata = map[string]string{}
			}
			maps.Copy(m.SetMetadata, fields)
		}
	}
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

// judgeBdLabelAddRemove judges `label add|remove <id...> <label[,label]>`, which
// split their label argument on plain commas (no CSV quoting).
func judgeBdLabelAddRemove(in bdClassGuardInput) bdClassGuard {
	pos := in.args.Positionals
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
	pos := in.args.Positionals
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
	pos := in.args.Positionals
	if len(pos) != 2 {
		return in.refuse("gc bd tag: refused on this split city: expected <id> <label>. Nothing was written.")
	}
	return in.checkIDs(pos[:1], coordclass.Mutation{AddLabels: []string{strings.TrimSpace(pos[1])}})
}

// judgeBdSetState judges `set-state <id> <dimension>=<value>`, which swaps the
// bead's first <dimension>:* label for <dimension>:<value>.
func judgeBdSetState(in bdClassGuardInput) bdClassGuard {
	pos := in.args.Positionals
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
	if len(in.args.Positionals) != 1 {
		return in.refuse("gc bd sql: refused on this split city: expected exactly one query. Nothing was written.")
	}
	if why := sqlReadOnlyProblem(in.args.Positionals[0]); why != "" {
		return in.refuse("gc bd sql: refused on this split city: %s. Only a single plain read statement (SELECT, SHOW, DESCRIBE, EXPLAIN SELECT), with no comments and no Dolt procedures, passes through gc bd sql here: a raw write can give a work-store bead an infrastructure type or label, which strands it. Use the gc bd verbs, which gc judges, or run bd directly in %s if you have checked what it will write. Nothing was written.", why, in.scopeRoot)
	}
	return in.pass()
}

// judgeBdMolWisp admits the one wisp shape a workflow depends on — the gastown
// patrols' `gc bd mol wisp <formula> --root-only [--var k=v]...` — and judges,
// in process, every bead it creates, before anything is written.
//
// With --root-only bd creates exactly ONE bead, the molecule root, and its shape
// does not come from the formula's steps: for a formula, bd mints the root as
// type molecule carrying only the "template" label and no metadata (bd
// cookFormulaToSubgraph); for a legacy proto id, bd clones that proto bead's
// type, labels and metadata. So the guard resolves the target the way bd does
// — a bead with that id is the proto; otherwise it is a formula — and judges
// the one root that results. Without --root-only bd instantiates every step,
// children and expansion templates included, from content gc would have to
// re-derive; that shape has no workflow depending on it and is refused toward
// gc's own sling/cook paths. TestGcBdMolWispRootOnlyMintsOneWorkBead (an
// integration test against a real bd) pins the root shape.
func judgeBdMolWisp(in bdClassGuardInput) bdClassGuard {
	if rootOnly, _ := in.args.BoolValue("--root-only"); !rootOnly {
		return in.refuse("gc bd %s: refused on this split city: without --root-only it instantiates every step of the formula, and bd copies each step's type, labels and metadata onto the beads it mints in the work store. Instead: %s. Nothing was written.", in.path, bdSplitAltSling)
	}
	if len(in.args.Positionals) != 1 {
		return in.refuse("gc bd %s: refused on this split city: expected exactly one formula or proto. Nothing was written.", in.path)
	}
	target := in.args.Positionals[0]
	root := beads.Bead{Type: "molecule", Labels: []string{"template"}}
	proto, result := in.read(target)
	switch result {
	case bdReadNotFound:
		// A formula: the root shape above.
	case bdReadFound:
		root = beads.Bead{ID: proto.ID, Type: proto.Type, Labels: proto.Labels, Metadata: proto.Metadata}
	default:
		return in.refuse("%s", in.readRefusal(target, result))
	}
	if flip := coordclass.CheckFlip(beads.Bead{}, root, in.holds); flip != nil {
		return in.refuse("%s", classFlipRefusalText(fmt.Sprintf("gc bd %s %s (the proto it clones)", in.path, target), flip))
	}
	return in.pass()
}

// judgeBdCook passes `cook` that only prints; --persist writes a proto into the
// work store and is refused.
func judgeBdCook(in bdClassGuardInput) bdClassGuard {
	if persist, _ := in.args.BoolValue("--persist"); persist {
		return in.refuse("gc bd cook --persist: refused on this split city: it writes the formula's steps into the work store as a proto, with each step's type, labels and metadata. Instead: %s. Nothing was written.", bdSplitAltSling)
	}
	return in.pass()
}

// judgeBdBatch judges `gc bd batch`, whose script (stdin or --file) can mint a
// bead of any type with `create <type> ...`.
func judgeBdBatch(in bdClassGuardInput) bdClassGuard {
	if len(in.args.Positionals) != 0 {
		return in.refuse("gc bd batch: refused on this split city: batch takes no positional arguments. Nothing was written.")
	}
	files := in.args.Values("--file")
	var script []byte
	var err error
	out := in.pass()
	if len(files) > 0 {
		path := files[len(files)-1]
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

// judgeBdImport judges `gc bd import`, the documented restore path for a JSONL
// archive. A row whose class this store does not hold — every infrastructure
// row of an archive taken before the split — would be stranded by bd's
// upsert. Those rows are not routed to the binding: it already holds the live
// copies (gc storage migrate preserved ids), and replaying an archive over them
// would roll sessions and running graphs back. The import is refused and names
// --skip-infra (gc's own flag), which imports a copy holding only the rows this
// store holds and counts the rest. A dry run writes nothing, so it is reported.
func judgeBdImport(in bdClassGuardInput) bdClassGuard {
	args, skipInfra := stripSkipInfraFlag(in.bdArgs)
	inputs := in.args.Values("--input")
	sources := append(slices.Clone(in.args.Positionals), inputs...)
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
	out := bdClassGuard{args: args}
	if split.skipped == 0 {
		if source == "-" {
			out.stdin = bytes.NewReader(data)
		}
		return out
	}
	if dryRun, _ := in.args.BoolValue("--dry-run"); dryRun && !skipInfra {
		if source == "-" {
			out.stdin = bytes.NewReader(data)
		}
		out.notice = fmt.Sprintf("gc bd import: dry run: %s holds %d row(s) of a class this store does not hold (%s); on this split city the real import is refused unless re-run with %s, which imports the %d other row(s) only\n",
			source, split.skipped, formatClassCounts(split.byClass), bdImportSkipInfraFlag, len(split.keep))
		return out
	}
	if !skipInfra {
		return in.refuse("gc bd import: refused on this split city: %s holds %d row(s) of a class this store does not hold (%s). bd import would strand every one in the work store, and every later gc command would refuse until they were repaired. They are not routed to the class store either: it already holds the live copies, and replaying an archive over them would roll sessions and running graphs back. Re-run with %s to import only the %d row(s) this store holds; the rest are left out and counted. Nothing was written.",
			source, split.skipped, formatClassCounts(split.byClass), bdImportSkipInfraFlag, len(split.keep))
	}
	filtered := bytes.Join(split.keep, []byte("\n"))
	if len(filtered) > 0 {
		filtered = append(filtered, '\n')
	}
	notice := fmt.Sprintf("gc bd import: %s: importing %d row(s); left out %d row(s) of a class this store does not hold (%s), which this split city keeps in its class store\n",
		source, len(split.keep), split.skipped, formatClassCounts(split.byClass))
	if len(split.dependents) > 0 {
		shown := split.dependents
		if len(shown) > 10 {
			shown = shown[:10]
		}
		notice += fmt.Sprintf("gc bd import: %d kept row(s) carry a dependency on a left-out id (%s); those edges point at beads the class store holds under the same ids, so check them after the import\n",
			len(split.dependents), strings.Join(shown, ", "))
	}
	out.notice = notice
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

// bdImportRedirect points the one token of argv that names source (the
// positional, `-i x`, `-ix`, `-i=x`, `--input x`, `--input=x`) at path. It
// finds the token by re-parsing argv token by token with import's flags, so a
// flag value that happens to equal source is never rewritten.
func bdImportRedirect(argv []string, source, path string) ([]string, bool) {
	out := slices.Clone(argv)
	specs := bdSplitFlagSpecs("import", bdSplitCommand{})
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
		width, err := bdflags.FlagTokenWidth(specs, out[i:])
		if err != nil {
			return nil, false
		}
		name, _, _ := strings.Cut(tok, "=")
		switch {
		case name == "--input" || name == "-i":
			if width == 2 {
				out[i+1] = path
			} else {
				out[i] = name + "=" + path
			}
			return out, true
		case strings.HasPrefix(tok, "-i") && !strings.HasPrefix(tok, "--"):
			out[i] = "-i" + path
			return out, true
		}
		i += width - 1
	}
	return nil, false
}

// bdSplitAllowlistPaths returns the allowlisted paths, sorted (for the docs
// test and the coverage report).
func bdSplitAllowlistPaths() []string {
	paths := slices.Collect(maps.Keys(bdSplitAllowlist))
	sort.Strings(paths)
	return paths
}
