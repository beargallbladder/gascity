package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

// The bd trust policy for split cities: gc bd's allowlist is pinned to bd
// COMMAND TREES, not to version strings. Version strings cannot be trusted —
// every Enterprise build says 1.3.0, and OSS main says 1.3.2 with a tree that
// differs from the v1.3.2 tag's — so a binary is accepted only when its
// command tree equals the visible projection of a pinned inventory
// (bdflags.MatchHelpTree), and that inventory's decisions then apply.
//
// How a binary's verdict is reached, cheapest first:
//
//  1. The per-city cache (.gc/cache/bd-inventory.json), keyed by the binary's
//     resolved path, size and mtime: a hit answers without reading the binary.
//  2. The city's vouch, [storage] gc_bd_inventory = "<id>@sha256:<hex>",
//     which `gc doctor --fix` writes after a structural walk matched: a binary
//     with exactly that sha256 is trusted as that inventory without a walk.
//  3. A cached verdict for the same sha256 under another path or mtime.
//  4. A structural walk of the binary's --help tree (bdflags.WalkHelp),
//     matched against every pinned inventory. The verdict, accepted or
//     refused, is cached by sha256.
//
// Hidden commands and flags are not in --help, so a walk can only prove the
// visible tree; hidden elements come from the vetted inventory, dumped from
// bd's own source. A bd `__command-tree --json` would close that gap; the
// proposal is in the PR that introduced this file.

// bdTrustVerdict is what gc concluded about one bd binary.
type bdTrustVerdict struct {
	// Inventory is the matched pinned inventory's id, "" when none matched.
	Inventory string   `json:"inventory,omitempty"`
	Diffs     []string `json:"diffs,omitempty"`
	SHA256    string   `json:"sha256"`
	// Source is how the verdict was reached: vouched, cache, walk.
	Source string `json:"-"`
}

// bdTrustCacheEntry is one cached verdict.
type bdTrustCacheEntry struct {
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	ModTime   int64     `json:"mtime_unix_nano"`
	CheckedAt time.Time `json:"checked_at"`
	bdTrustVerdict
}

type bdTrustCache struct {
	Entries []bdTrustCacheEntry `json:"entries"`
}

var bdTrustCacheMu sync.Mutex

func bdTrustCachePath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", "cache", "bd-inventory.json")
}

func loadBdTrustCache(cityPath string) bdTrustCache {
	var c bdTrustCache
	data, err := os.ReadFile(bdTrustCachePath(cityPath))
	if err == nil {
		_ = json.Unmarshal(data, &c)
	}
	return c
}

func saveBdTrustCache(cityPath string, c bdTrustCache) {
	sort.Slice(c.Entries, func(i, j int) bool { return c.Entries[i].CheckedAt.After(c.Entries[j].CheckedAt) })
	if len(c.Entries) > 32 {
		c.Entries = c.Entries[:32]
	}
	data, err := json.MarshalIndent(c, "", " ")
	if err != nil {
		return
	}
	path := bdTrustCachePath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = fsys.WriteFileIfChangedAtomic(fsys.OSFS{}, path, data, 0o644)
}

// bdHelpRunner runs `bd <path...> --help` for a structural walk.
type bdHelpRunner func(path []string) (string, error)

// resolveBdInventory returns the verdict for the bd binary at bdPath.
// forceWalk skips the cache and the vouch (the doctor's fix walks for real).
func resolveBdInventory(cityPath string, cfg *config.City, bdPath string, help bdHelpRunner, forceWalk bool) (bdTrustVerdict, error) {
	abs, err := filepath.EvalSymlinks(bdPath)
	if err != nil {
		return bdTrustVerdict{}, fmt.Errorf("resolving bd binary %s: %w", bdPath, err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return bdTrustVerdict{}, fmt.Errorf("reading bd binary %s: %w", abs, err)
	}
	bdTrustCacheMu.Lock()
	defer bdTrustCacheMu.Unlock()
	cache := loadBdTrustCache(cityPath)
	key := func(e bdTrustCacheEntry) bool {
		return e.Path == abs && e.Size == st.Size() && e.ModTime == st.ModTime().UnixNano()
	}
	if !forceWalk {
		for _, e := range cache.Entries {
			if key(e) {
				v := e.bdTrustVerdict
				v.Source = "cache"
				return v, nil
			}
		}
	}
	sum, err := fileSHA256(abs)
	if err != nil {
		return bdTrustVerdict{}, err
	}
	record := func(v bdTrustVerdict) bdTrustVerdict {
		kept := cache.Entries[:0]
		for _, e := range cache.Entries {
			if !key(e) {
				kept = append(kept, e)
			}
		}
		kept = append(kept, bdTrustCacheEntry{Path: abs, Size: st.Size(), ModTime: st.ModTime().UnixNano(), CheckedAt: time.Now().UTC(), bdTrustVerdict: v})
		cache.Entries = kept
		saveBdTrustCache(cityPath, cache)
		return v
	}
	if !forceWalk {
		if cfg != nil && cfg.Storage != nil {
			if id, vouched, ok := config.ParseGCBdInventory(cfg.Storage.GCBdInventory); ok && vouched == sum && bdflags.InventoryByName(id) != nil {
				v := record(bdTrustVerdict{Inventory: id, SHA256: sum})
				v.Source = "vouched"
				return v, nil
			}
		}
		for _, e := range cache.Entries {
			if e.SHA256 == sum {
				v := record(e.bdTrustVerdict)
				v.Source = "cache"
				return v, nil
			}
		}
	}
	tree, err := bdflags.WalkHelp(help, bdflags.SkipUnparsedHelp, 8)
	if err != nil {
		return bdTrustVerdict{}, fmt.Errorf("walking %s's command tree: %w", abs, err)
	}
	v := bdTrustVerdict{SHA256: sum}
	if inv, diffs := bdflags.MatchHelpTree(tree); inv != nil {
		v.Inventory = inv.Name
	} else {
		for _, name := range bdSortedDiffKeys(diffs) {
			for i, d := range diffs[name] {
				if i >= 3 {
					break
				}
				v.Diffs = append(v.Diffs, name+": "+bdFirstLine(d))
			}
		}
	}
	v = record(v)
	v.Source = "walk"
	return v, nil
}

func bdSortedDiffKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func bdFirstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// bdHelpRunnerFor runs the bd binary at bdPath for a structural walk, in dir,
// with env (BD_NAME removed: it renames the root command in help text).
func bdHelpRunnerFor(bdPath, dir string, env []string) bdHelpRunner {
	var clean []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "BD_NAME=") {
			clean = append(clean, kv)
		}
	}
	return func(path []string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bdPath, append(append([]string(nil), path...), "--help")...)
		cmd.Dir = dir
		cmd.Env = clean
		cmd.Stdin = nil
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
}

// errBdUninventoried is wrapped by the error a split city returns for a bd
// whose command tree matches no pinned inventory.
var errBdUninventoried = errors.New("bd has no pinned gc bd inventory")

// bdSplitInventoryResolver is the resolver doBd uses; tests whose fake bd has
// no real command tree pin it to an inventory (pinTestBdInventory).
var bdSplitInventoryResolver = bdInventoryForScope

// bdInventoryForScope returns the pinned inventory the scope's bd binary
// matches, or an error wrapping errBdUninventoried that says what differed.
func bdInventoryForScope(cityPath string, cfg *config.City, target execStoreTarget) (*bdflags.Inventory, error) {
	bdPath, err := resolveBdBinaryForScope(cityPath, target.ScopeRoot)
	if err != nil {
		return nil, err
	}
	env, err := bdCommandEnv(cityPath, cfg, target)
	if err != nil {
		return nil, err
	}
	v, err := resolveBdInventory(cityPath, cfg, bdPath, bdHelpRunnerFor(bdPath, target.ScopeRoot, workQueryEnvForDir(env, target.ScopeRoot)), false)
	if err != nil {
		return nil, err
	}
	if v.Inventory == "" {
		return nil, fmt.Errorf("%w: %s (sha256 %s) matches none (%s)", errBdUninventoried, bdPath, v.SHA256[:12], strings.Join(v.Diffs, "; "))
	}
	return bdflags.InventoryByName(v.Inventory), nil
}

// bdSplitRefusalEvent is the gc_bd.refused event: one per split-city gc bd
// refusal, so a refusal swallowed by a script's `|| true` (wisp-compact.sh,
// the deacon's mol burn) still shows up in the city's event log, and so the
// allowlist's burn-in can see which commands callers actually need.
const bdSplitRefusalEvent = "gc_bd.refused"

// bdSplitRefusalPayload is the gc_bd.refused event's payload.
type bdSplitRefusalPayload struct {
	Kind    string `json:"kind"`
	Path    string `json:"path,omitempty"`
	Verb    string `json:"verb,omitempty"`
	Scope   string `json:"scope"`
	Caller  string `json:"caller,omitempty"`
	Session string `json:"session,omitempty"`
	Reason  string `json:"reason"`
}

// recordBdSplitRefusal appends the gc_bd.refused event to the city's event
// log. Best-effort: a log that cannot be opened loses the event, never the
// refusal.
func recordBdSplitRefusal(cityPath, scopeRoot string, bdArgs []string, g bdClassGuard, stderr io.Writer) {
	verb, _, _ := bdRelocatedClassVerb(bdArgs)
	reason := g.refusal
	if len(reason) > 400 {
		reason = reason[:400]
	}
	payload, err := json.Marshal(bdSplitRefusalPayload{
		Kind: g.kind, Path: g.path, Verb: verb, Scope: scopeRoot,
		Caller: strings.TrimSpace(os.Getenv("GC_AGENT")), Session: strings.TrimSpace(os.Getenv("GC_SESSION_NAME")),
		Reason: reason,
	})
	if err != nil {
		return
	}
	rec := openCityRecorderAt(cityPath, io.Discard)
	rec.Record(events.Event{
		Type:    bdSplitRefusalEvent,
		Ts:      time.Now().UTC(),
		Actor:   eventActor(),
		Subject: g.path,
		Message: bdFirstLine(g.refusal),
		Payload: payload,
	})
	if closer, ok := rec.(io.Closer); ok {
		_ = closer.Close()
	}
	_ = stderr
}

// bdHeldByBinding reports which ids a class binding holds, building the
// residency topology once for the whole set (a refused city's per-id relic
// proof is the one case that needs a per-id topology).
func bdHeldByBinding(cityPath string, ids []string) (map[string]bool, error) {
	base := residencyTopologyForCity(cityPath, nil, newUnprobedWorkResidual(), nil)
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		topo := base
		if base.Refused != nil {
			topo = byIDResidencyTopology(cityPath, nil, newUnprobedWorkResidual(), nil, id)
		}
		_, owned, err := byIDBindingOwnerForTopology(topo, id)
		if err != nil {
			return nil, err
		}
		out[id] = owned
	}
	return out, nil
}

// bdEndpointScope returns the work scope an edge endpoint lives in, by its id
// prefix: the city's or a bound rig's (longest prefix wins), else fallback.
func bdEndpointScope(cityPath string, cfg *config.City, id, fallback string) string {
	best, bestLen := fallback, 0
	consider := func(prefix, root string) {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" || root == "" {
			return
		}
		if strings.HasPrefix(id, prefix+"-") && len(prefix) > bestLen {
			best, bestLen = root, len(prefix)
		}
	}
	if cfg != nil {
		consider(config.EffectiveHQPrefix(cfg), cityPath)
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Path) != "" {
				consider(rig.EffectivePrefix(), resolveStoreScopeRoot(cityPath, rig.Path))
			}
		}
	}
	return best
}

// bdReadEdgeEndpoints reads edge endpoints across the city's work stores,
// grouped by the store each id's prefix names, batched where the store can.
func bdReadEdgeEndpoints(cityPath string, cfg *config.City, scopeRoot string, ids []string) map[string]bdReadResult {
	byScope := map[string][]string{}
	for _, id := range ids {
		s := bdEndpointScope(cityPath, cfg, id, scopeRoot)
		byScope[s] = append(byScope[s], id)
	}
	out := make(map[string]bdReadResult, len(ids))
	for scope, group := range byScope {
		store := bdWriteGuardStore(scope, cityPath, cfg)
		pending := group
		if getter, ok := store.(beads.ExactBatchGetter); ok && len(group) > 1 {
			if found, unresolved, err := getter.GetExactBatch(group); err == nil {
				for id := range found {
					out[id] = bdReadFound
				}
				pending = unresolved
			}
		}
		for _, id := range pending {
			_, err := store.Get(id)
			switch {
			case err == nil:
				out[id] = bdReadFound
			case errors.Is(err, beads.ErrIDCollision):
				out[id] = bdReadCollision
			case errors.Is(err, beads.ErrNotFound):
				out[id] = bdReadNotFound
			default:
				out[id] = bdReadFailed
			}
		}
	}
	return out
}

// bdBindingDependents returns, per work id, the class-binding beads with a
// dependency on it.
func bdBindingDependents(cityPath string, ids []string) (map[string][]string, error) {
	binding, relocated, err := cliSoleClassBinding(cityPath)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	if !relocated {
		return out, nil
	}
	for _, id := range ids {
		deps, err := binding.Store.DepList(id, "up")
		if err != nil {
			return nil, fmt.Errorf("listing the binding's dependents of %s: %w", id, err)
		}
		for _, d := range deps {
			if d.DependsOnID == id && d.IssueID != "" {
				out[id] = append(out[id], d.IssueID)
			}
		}
	}
	return out, nil
}
