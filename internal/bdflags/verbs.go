package bdflags

import "sort"

// topLevelVerbs is every top-level bd subcommand: the ones `bd --help` lists
// plus the hidden ones bd registers without listing. Aliases are in
// topLevelAliases, keyed to the verb they run.
//
// It is the inventory `gc bd` forwards from: the passthrough accepts any bd
// verb, so a guard that must judge every verb (the class-must-not-flip guard's
// coverage test in cmd/gc) iterates this list, and a verb bd adds without a
// row there fails that test. TestBdTopLevelVerbsCurrent (integration) fails
// when the installed bd lists a verb this inventory is missing.
//
// Sourced from `bd --help` and the cobra registrations (bd 1.3.2 and bd 1.3.0
// Enterprise, 2026-10-26).
var topLevelVerbs = []string{
	"admin", "ado", "assign", "audit", "backup", "batch", "blocked", "bootstrap",
	"branch", "children", "close", "comment", "comments", "compact", "completion",
	"config", "conflicts", "context", "cook", "count", "create", "create-form",
	"defer", "delete", "dep", "diff", "doctor", "dolt", "duplicate", "duplicates",
	"edit", "epic", "events", "export", "federation", "find-duplicates", "flatten",
	"forget", "formula", "gate", "gc", "github", "gitlab", "graph", "heartbeat",
	"help", "history", "hooks", "human", "import", "info", "init", "init-safety",
	"jira", "kv", "label", "linear", "link", "lint", "list", "mail", "memories",
	"merge-slot", "metrics", "migrate", "migrate-personal", "mol", "note", "notion",
	"onboard", "orphans", "ping", "preflight", "prime", "priority", "promote",
	"provenance", "prune", "purge", "q", "query", "quickstart", "ready", "recall",
	"reclaim", "recompute-blocked", "remember", "rename", "rename-prefix", "reopen",
	"repo", "restore", "rules", "schema", "search", "serve", "set-state", "setup",
	"ship", "show", "sql", "stale", "state", "status", "statuses", "supersede",
	"swarm", "sync", "tag", "todo", "types", "unclaim", "undefer", "update",
	"upgrade", "vc", "version", "where", "worktree",
	// Hidden: registered, not listed by `bd --help`.
	"codex-hook", "cursor-hook", "db-proxy-child", "send-metrics",
	// Enterprise builds only (bd 1.3.0 Enterprise): hosted-beads identity,
	// profiles and the Companion. Listed so the inventory is a superset of
	// every bd the fleet runs.
	"companion", "connect", "getToken", "login", "logout", "profile", "whoami",
}

// topLevelAliases maps each top-level alias bd registers to the verb it runs.
var topLevelAliases = map[string]string{
	"new":           "create",
	"done":          "close",
	"find-dups":     "find-duplicates",
	"hb":            "heartbeat",
	"view":          "show",
	"stats":         "status",
	"protomolecule": "mol",
}

// TopLevelVerbs returns every top-level bd subcommand, sorted. Aliases are
// not included; see TopLevelAliases.
func TopLevelVerbs() []string {
	out := append([]string(nil), topLevelVerbs...)
	sort.Strings(out)
	return out
}

// TopLevelAliases returns a copy of the alias-to-verb map.
func TopLevelAliases() map[string]string {
	out := make(map[string]string, len(topLevelAliases))
	for k, v := range topLevelAliases {
		out[k] = v
	}
	return out
}
