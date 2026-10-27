package main

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

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
//  1. The per-city cache (.gc/cache/bd-inventory.json, locked across
//     processes), keyed by the binary's resolved path, device, inode, size,
//     mtime and ctime and by the embedded inventory set: a hit answers
//     without reading the binary.
//  2. The city's vouch, [storage] gc_bd_inventory = "<id>@sha256:<hex>",
//     which `gc doctor --fix` writes after a structural walk matched: a binary
//     with exactly that sha256 is admitted as that inventory without a walk,
//     and a gc_bd.vouch_used event says so. The vouch is a convenience, not a
//     security boundary; the doctor re-walks vouched binaries.
//  3. A cached verdict for the same sha256 under another path.
//  4. A structural walk of the binary's --help tree (bdflags.WalkHelp) in a
//     fixed minimal environment, matched against every pinned inventory, plus
//     a build-info check (bd's cmd/bd, the inventory's cobra and pflag). Only
//     a conclusive verdict is cached; a failed or empty walk is an error,
//     remembered for a minute.
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
	// Vouched reports that the city's gc_bd_inventory vouch, not a walk,
	// admitted the binary. The doctor re-walks vouched binaries.
	Vouched bool `json:"vouched,omitempty"`
	// Source is how the verdict was reached: vouched, cache, walk.
	Source string `json:"-"`
}

// bdBinaryID identifies one bd binary file for the verdict cache. Every
// usual way of replacing a binary changes at least one field: cp, install
// and rsync rewrite it (mtime and ctime), cp -p, install -p, rsync -t and tar
// restore the mtime but not the ctime, and a rename into place changes the
// inode.
type bdBinaryID struct {
	Path  string `json:"path"`
	Dev   uint64 `json:"dev"`
	Ino   uint64 `json:"ino"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime_unix_nano"`
	CTime int64  `json:"ctime_unix_nano"`
}

// statBdBinary resolves bdPath's symlinks and identifies the file.
func statBdBinary(bdPath string) (bdBinaryID, error) {
	abs, err := filepath.EvalSymlinks(bdPath)
	if err != nil {
		return bdBinaryID{}, fmt.Errorf("resolving bd binary %s: %w", bdPath, err)
	}
	if abs, err = filepath.Abs(abs); err != nil {
		return bdBinaryID{}, err
	}
	var st syscall.Stat_t
	if err := syscall.Stat(abs, &st); err != nil {
		return bdBinaryID{}, fmt.Errorf("reading bd binary %s: %w", abs, err)
	}
	return bdBinaryID{
		Path:  abs,
		Dev:   uint64(st.Dev), //nolint:unconvert // Dev's type varies by platform
		Ino:   st.Ino,
		Size:  st.Size,
		MTime: st.Mtim.Nano(),
		CTime: st.Ctim.Nano(),
	}, nil
}

// bdTrustCacheEntry is one cached verdict, or one remembered walk failure.
type bdTrustCacheEntry struct {
	bdBinaryID
	// InventorySet is the embedded inventory set the verdict was judged
	// against (bdflags.InventorySetFingerprint); an entry from another set
	// is never used.
	InventorySet string    `json:"inventory_set"`
	CheckedAt    time.Time `json:"checked_at"`
	// WalkError, with ErrorUntil, remembers a walk that failed or was
	// inconclusive, so a broken binary is not re-walked on every call. It is
	// never a verdict: the caller gets the error, not a refusal.
	WalkError  string    `json:"walk_error,omitempty"`
	ErrorUntil time.Time `json:"error_until,omitempty"`
	bdTrustVerdict
}

type bdTrustCache struct {
	Entries []bdTrustCacheEntry `json:"entries"`
}

// bdWalkErrorTTL is how long a failed walk is remembered.
var bdWalkErrorTTL = time.Minute

// bdWalkDeadline bounds a whole structural walk; bdWalkCommandTimeout bounds
// one `bd <path> --help`.
var (
	bdWalkDeadline       = 2 * time.Minute
	bdWalkCommandTimeout = 20 * time.Second
)

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

// lockBdTrustCache takes the city's cross-process verdict-cache lock, so a
// cold cache costs one walk however many gc processes ask at once: the rest
// wait and then read the first one's verdict.
func lockBdTrustCache(cityPath string) (func(), error) {
	path := bdTrustCachePath(cityPath) + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(bdWalkDeadline + 30*time.Second)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("another gc process is still checking the bd binary (%s held)", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// bdHelpRunner runs `bd <path...> --help` for a structural walk.
type bdHelpRunner func(ctx context.Context, path []string) (string, error)

// bdTrustOpts selects how much of the cache and vouch a resolution may use.
type bdTrustOpts struct {
	// forceWalk ignores the cache and the vouch: the doctor's fix and its
	// re-check of a vouched binary walk for real.
	forceWalk bool
}

// bdBuildInfoCheck verifies that the binary is a Go build of bd's cmd/bd
// with the cobra and pflag the matched inventory was dumped with (a mise or
// asdf shim, a wrapper script, or a pflag bump fails). Tests whose fake bd is
// a shell script replace it.
var bdBuildInfoCheck = func(path string, inv *bdflags.Inventory) error {
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s is not a Go build of bd (%w); point PATH at the real bd binary, not a shim or wrapper", path, err)
	}
	return checkBdBuildInfo(bi, inv)
}

// checkBdBuildInfo is bdBuildInfoCheck's rule on decoded build info.
func checkBdBuildInfo(bi *debug.BuildInfo, inv *bdflags.Inventory) error {
	if !strings.HasSuffix(bi.Path, "/cmd/bd") {
		return fmt.Errorf("its main package is %q, not bd's cmd/bd", bi.Path)
	}
	want := map[string]string{
		"github.com/spf13/cobra": inv.Provenance.CobraVersion,
		"github.com/spf13/pflag": inv.Provenance.PflagVersion,
	}
	for _, dep := range bi.Deps {
		m := dep
		if dep.Replace != nil {
			m = dep.Replace
		}
		if v, ok := want[dep.Path]; ok {
			if m.Version != v {
				return fmt.Errorf("it was built with %s %s; inventory %s was dumped with %s, and gc resolves argv with that version", dep.Path, m.Version, inv.Name, v)
			}
			delete(want, dep.Path)
		}
	}
	for path := range want {
		return fmt.Errorf("its build info lists no %s", path)
	}
	return nil
}

// resolveBdInventory returns the verdict for the bd binary at bdPath, walking
// it (when it must) with a fixed minimal environment.
//
// Only a conclusive verdict is cached: a walk whose root lists subcommands,
// matched or not. A walk that fails, times out or parses to an empty root is
// an error the caller reports (and remembered for bdWalkErrorTTL), never a
// cached refusal, so one caller's broken environment cannot disable gc bd for
// the city.
func resolveBdInventory(cityPath string, cfg *config.City, bdPath string, opts bdTrustOpts) (bdTrustVerdict, error) {
	id, err := statBdBinary(bdPath)
	if err != nil {
		return bdTrustVerdict{}, err
	}
	set := bdflags.InventorySetFingerprint()
	bdTrustCacheMu.Lock()
	defer bdTrustCacheMu.Unlock()
	unlock, err := lockBdTrustCache(cityPath)
	if err != nil {
		return bdTrustVerdict{}, err
	}
	defer unlock()
	cache := loadBdTrustCache(cityPath)
	now := time.Now()
	same := func(e bdTrustCacheEntry) bool { return e.bdBinaryID == id && e.InventorySet == set }
	if !opts.forceWalk {
		for _, e := range cache.Entries {
			if !same(e) {
				continue
			}
			if e.WalkError != "" {
				if now.Before(e.ErrorUntil) {
					return bdTrustVerdict{}, fmt.Errorf("checking %s's command tree failed %s ago: %s", id.Path, now.Sub(e.CheckedAt).Round(time.Second), e.WalkError)
				}
				continue
			}
			if e.Inventory != "" && bdflags.InventoryByName(e.Inventory) == nil {
				continue
			}
			v := e.bdTrustVerdict
			v.Source = "cache"
			return v, nil
		}
	}
	record := func(e bdTrustCacheEntry) {
		e.bdBinaryID, e.InventorySet, e.CheckedAt = id, set, time.Now().UTC()
		kept := make([]bdTrustCacheEntry, 0, len(cache.Entries)+1)
		for _, old := range cache.Entries {
			if old.bdBinaryID != id {
				kept = append(kept, old)
			}
		}
		kept = append(kept, e)
		cache.Entries = kept
		saveBdTrustCache(cityPath, cache)
	}
	sum, err := fileSHA256(id.Path)
	if err != nil {
		return bdTrustVerdict{}, err
	}
	if !opts.forceWalk {
		if cfg != nil && cfg.Storage != nil {
			if invID, vouched, ok := config.ParseGCBdInventory(cfg.Storage.GCBdInventory); ok && vouched == sum {
				if inv := bdflags.InventoryByName(invID); inv != nil {
					v := bdTrustVerdict{Inventory: invID, SHA256: sum, Vouched: true}
					if err := bdBuildInfoCheck(id.Path, inv); err != nil {
						v = bdTrustVerdict{SHA256: sum, Diffs: []string{"build info: " + err.Error()}}
					}
					record(bdTrustCacheEntry{bdTrustVerdict: v})
					if v.Vouched {
						recordBdVouchUsed(cityPath, id.Path, sum, invID)
					}
					v.Source = "vouched"
					return v, nil
				}
			}
		}
		for _, e := range cache.Entries {
			if e.InventorySet == set && e.WalkError == "" && e.SHA256 == sum && (e.Inventory == "" || bdflags.InventoryByName(e.Inventory) != nil) {
				record(bdTrustCacheEntry{bdTrustVerdict: e.bdTrustVerdict})
				v := e.bdTrustVerdict
				v.Source = "cache"
				return v, nil
			}
		}
	}
	tree, walkErr := walkBdHelpTree(id.Path)
	if walkErr == nil && len(tree[""].Subcommands) == 0 {
		walkErr = errors.New("bd's root help listed no commands, so the walk proves nothing")
	}
	if walkErr != nil {
		record(bdTrustCacheEntry{WalkError: walkErr.Error(), ErrorUntil: time.Now().Add(bdWalkErrorTTL)})
		return bdTrustVerdict{}, fmt.Errorf("checking %s's command tree: %w", id.Path, walkErr)
	}
	v := bdTrustVerdict{SHA256: sum}
	if inv, diffs := bdflags.MatchHelpTree(tree); inv != nil {
		if err := bdBuildInfoCheck(id.Path, inv); err != nil {
			v.Diffs = []string{"build info: " + err.Error()}
		} else {
			v.Inventory = inv.Name
		}
	} else {
		for _, name := range bdSortedDiffKeys(diffs) {
			for i, d := range diffs[name] {
				if i >= 3 {
					break
				}
				v.Diffs = append(v.Diffs, bdTruncateUTF8(name+": "+bdFirstLine(d), 240))
			}
		}
	}
	record(bdTrustCacheEntry{bdTrustVerdict: v})
	v.Source = "walk"
	return v, nil
}

// walkBdHelpTree walks the binary's --help tree under one total deadline.
func walkBdHelpTree(bdPath string) (bdflags.HelpTree, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bdWalkDeadline)
	defer cancel()
	home, err := os.MkdirTemp("", "gc-bd-walk-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home) //nolint:errcheck // best-effort cleanup
	help := bdHelpRunnerFor(bdPath, home)
	return bdflags.WalkHelp(func(path []string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("the walk exceeded %s", bdWalkDeadline)
		}
		return help(ctx, path)
	}, bdflags.SkipUnparsedHelp, 8)
}

// bdTruncateUTF8 cuts s to at most n bytes on a rune boundary.
func bdTruncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
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

// bdHelpRunnerFor runs the bd binary at bdPath for a structural walk, in
// home, with a fixed minimal environment (PATH, HOME and TMPDIR only). The
// caller's environment never reaches the walk: CLICOLOR_FORCE, BD_NAME, a
// BEADS_DIR or a scope's config can all change what --help prints. Each run
// is its own process group, killed whole on timeout.
func bdHelpRunnerFor(bdPath, home string) bdHelpRunner {
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + home, "TMPDIR=" + os.TempDir()}
	return func(ctx context.Context, path []string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, bdWalkCommandTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, bdPath, append(append([]string(nil), path...), "--help")...)
		cmd.Dir = home
		cmd.Env = env
		cmd.Stdin = nil
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return nil
			}
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		cmd.WaitDelay = 2 * time.Second
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
	v, err := resolveBdInventory(cityPath, cfg, bdPath, bdTrustOpts{})
	if err != nil {
		return nil, err
	}
	inv := bdflags.InventoryByName(v.Inventory)
	if v.Inventory == "" || inv == nil {
		return nil, fmt.Errorf("%w: %s (sha256 %s) matches none (%s)", errBdUninventoried, bdPath, shortSHA(v.SHA256), strings.Join(v.Diffs, "; "))
	}
	return inv, nil
}

// bdVouchUsedEvent is recorded whenever the city's gc_bd_inventory vouch,
// not a walk, admits a bd binary. The vouch is the owner's convenience, not a
// security boundary (anyone who can write city.toml can write it), so its
// use is visible.
const bdVouchUsedEvent = "gc_bd.vouch_used"

func recordBdVouchUsed(cityPath, bdPath, sum, inventory string) {
	payload, err := json.Marshal(map[string]string{"binary": bdPath, "sha256": sum, "inventory": inventory})
	if err != nil {
		return
	}
	rec := openCityRecorderAt(cityPath, io.Discard)
	rec.Record(events.Event{Type: bdVouchUsedEvent, Ts: time.Now().UTC(), Actor: eventActor(), Subject: bdPath, Message: "gc bd admitted " + bdPath + " on the city's vouch for " + inventory, Payload: payload})
	if closer, ok := rec.(io.Closer); ok {
		_ = closer.Close()
	}
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
func recordBdSplitRefusal(cityPath, scopeRoot string, bdArgs []string, g bdClassGuard) {
	verb, _, _ := bdRelocatedClassVerb(bdArgs)
	reason := bdTruncateUTF8(g.refusal, 400)
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
	binding, relocated, err := cliSoleClassBinding(cityPath) // residency:allow — a reverse dependency lookup in the sole class binding for the delete guard; resolves no bead owner
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
		gates, err := bdOpenCrossStoreGatesNaming(binding.Store, id)
		if err != nil {
			return nil, err
		}
		out[id] = append(out[id], gates...)
	}
	return out, nil
}

// bdCrossStoreBlockerKey is the S3b drain gate's metadata key naming the
// work bead it waits on. TODO(#8360): use beadmeta.CrossStoreBlockerMetadataKey
// and its "open gates naming id X" helper once #8360 lands; this is the local
// equivalent.
const bdCrossStoreBlockerKey = "gc.cross_store_blocker"

// bdOpenCrossStoreGatesNaming lists the open beads in store whose
// gc.cross_store_blocker names id.
func bdOpenCrossStoreGatesNaming(store beads.Store, id string) ([]string, error) {
	gates, err := store.List(beads.ListQuery{
		Metadata: map[string]string{bdCrossStoreBlockerKey: id},
		Status:   "open",
		Live:     true,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return nil, fmt.Errorf("listing the binding's open gates waiting on %s: %w", id, err)
	}
	var out []string
	for _, g := range gates {
		if g.Status != "closed" && strings.TrimSpace(g.Metadata[bdCrossStoreBlockerKey]) == id {
			out = append(out, g.ID)
		}
	}
	return out, nil
}
