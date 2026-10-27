package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

// bdInventoryCheck is the doctor check behind the split-city bd trust policy
// (bd_inventory_trust.go): every bd-backed scope's bd binary must have a
// command tree that matches a pinned gc bd inventory, or gc bd refuses every
// command there — refusals a script's `|| true` would otherwise hide. It is
// warm-up eligible, so `gc start` warns about it too.
//
// The fix vouches for the city scope's binary: it walks that binary's command
// tree for real (no cache, no existing vouch), and only when the tree matches
// a pinned inventory writes [storage] gc_bd_inventory = "<id>@sha256:<hex>" to
// city.toml. A mismatch is refused and reported; nothing is written.
type bdInventoryCheck struct {
	cityPath string
	cfg      *config.City
	// resolve is resolveBdInventory, injectable for tests.
	resolve func(cityPath string, cfg *config.City, target execStoreTarget, force bool) (bdTrustVerdict, string, error)
}

func newBdInventoryCheck(cityPath string, cfg *config.City) *bdInventoryCheck {
	return &bdInventoryCheck{cityPath: cityPath, cfg: cfg, resolve: resolveScopeBdInventory}
}

// resolveScopeBdInventory resolves the verdict for target's bd binary and
// returns the binary's path.
func resolveScopeBdInventory(cityPath string, cfg *config.City, target execStoreTarget, force bool) (bdTrustVerdict, string, error) {
	bdPath, err := resolveBdBinaryForScope(cityPath, target.ScopeRoot)
	if err != nil {
		return bdTrustVerdict{}, "", err
	}
	env, err := bdCommandEnv(cityPath, cfg, target)
	if err != nil {
		return bdTrustVerdict{}, bdPath, err
	}
	v, err := resolveBdInventory(cityPath, cfg, bdPath, bdHelpRunnerFor(bdPath, target.ScopeRoot, workQueryEnvForDir(env, target.ScopeRoot)), force)
	return v, bdPath, err
}

func (c *bdInventoryCheck) Name() string         { return "bd-command-inventory" }
func (c *bdInventoryCheck) CanFix() bool         { return true }
func (c *bdInventoryCheck) WarmupEligible() bool { return true }

// scopes returns the city scope and every bound rig scope.
func (c *bdInventoryCheck) scopes() []execStoreTarget {
	out := []execStoreTarget{bdCityScopeTarget(c.cityPath, c.cfg)}
	for _, rig := range c.cfg.Rigs {
		if strings.TrimSpace(rig.Path) != "" {
			out = append(out, bdRigScopeTarget(c.cityPath, rig))
		}
	}
	return out
}

func (c *bdInventoryCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	if c.cfg == nil || workStoreClassHolds(c.cfg) == nil {
		r.Status = doctor.StatusOK
		r.Message = "single-store city: gc bd forwards every command, whatever the bd build"
		return r
	}
	var bad []string
	seen := map[string]bool{}
	for _, target := range c.scopes() {
		if !providerUsesBdStoreContract(rawBeadsProviderForScope(target.ScopeRoot, c.cityPath)) {
			continue
		}
		v, bdPath, err := c.resolve(c.cityPath, c.cfg, target, false)
		key := bdPath + "|" + v.SHA256
		if err == nil && seen[key] {
			continue
		}
		seen[key] = true
		switch {
		case err != nil:
			bad = append(bad, fmt.Sprintf("%s: %v", target.ScopeRoot, err))
		case v.Inventory == "":
			bad = append(bad, fmt.Sprintf("%s: %s (sha256 %s) matches no pinned gc bd inventory: %s", target.ScopeRoot, bdPath, shortSHA(v.SHA256), strings.Join(v.Diffs, "; ")))
		}
	}
	if len(bad) > 0 {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("%d scope(s) run a bd whose command tree matches no pinned gc bd inventory, so gc bd refuses every command there", len(bad))
		r.Details = bad
		r.FixHint = "install a pinned bd build (see internal/bdflags/inventory), or have the gc maintainers inventory this build; `gc doctor --fix` vouches for the city's bd only when its tree matches"
		return r
	}
	r.Status = doctor.StatusOK
	r.Message = "every scope's bd matches a pinned gc bd inventory"
	return r
}

// Fix walks the city scope's bd for real and, on a match, vouches for that
// exact binary in city.toml.
func (c *bdInventoryCheck) Fix(_ *doctor.CheckContext) error {
	if c.cfg == nil || workStoreClassHolds(c.cfg) == nil {
		return nil
	}
	v, bdPath, err := c.resolve(c.cityPath, c.cfg, bdCityScopeTarget(c.cityPath, c.cfg), true)
	if err != nil {
		return err
	}
	if v.Inventory == "" {
		return fmt.Errorf("refusing to vouch for %s: its command tree matches no pinned gc bd inventory (%s)", bdPath, strings.Join(v.Diffs, "; "))
	}
	if bdflags.InventoryByName(v.Inventory) == nil {
		return fmt.Errorf("refusing to vouch for %s: inventory %q is not pinned in this gc", bdPath, v.Inventory)
	}
	return writeGCBdInventoryVouch(filepath.Join(c.cityPath, "city.toml"), v.Inventory+"@sha256:"+v.SHA256)
}

func shortSHA(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}

// writeGCBdInventoryVouch sets [storage] gc_bd_inventory in city.toml,
// replacing an existing value, adding the key under an existing [storage]
// table, or adding a [storage] table ahead of the first [storage.*] table
// (else at the end).
func writeGCBdInventoryVouch(path, value string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("gc_bd_inventory = %q", value)
	lines := strings.Split(string(data), "\n")
	inStorage, storageAt, firstSub := false, -1, -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			inStorage = t == "[storage]"
			if inStorage {
				storageAt = i
			}
			if strings.HasPrefix(t, "[storage.") && firstSub < 0 {
				firstSub = i
			}
			continue
		}
		if inStorage {
			if key, _, ok := strings.Cut(t, "="); ok && strings.TrimSpace(key) == "gc_bd_inventory" {
				lines[i] = line
				return fsys.WriteFileIfChangedAtomic(fsys.OSFS{}, path, []byte(strings.Join(lines, "\n")), 0o644)
			}
		}
	}
	switch {
	case storageAt >= 0:
		lines = append(lines[:storageAt+1], append([]string{line}, lines[storageAt+1:]...)...)
	case firstSub >= 0:
		lines = append(lines[:firstSub], append([]string{"[storage]", line, ""}, lines[firstSub:]...)...)
	default:
		lines = append(lines, "", "[storage]", line, "")
	}
	return fsys.WriteFileIfChangedAtomic(fsys.OSFS{}, path, []byte(strings.Join(lines, "\n")), 0o644)
}

// bdUninventoriedEvent is recorded when a controller starts on a split city
// whose bd (city or rig scope) matches no pinned inventory: every gc bd
// command there will be refused, and a script that swallows the refusal
// would otherwise hide it.
const bdUninventoriedEvent = "gc_bd.uninventoried"

// bdInventoryStartFinding runs the bd-command-inventory check for a starting
// controller and returns the event to record, or nil when every scope's bd
// matches (or the city is not split).
func bdInventoryStartFinding(check *bdInventoryCheck) *events.Event {
	if check.cfg == nil || workStoreClassHolds(check.cfg) == nil {
		return nil
	}
	r := check.Run(&doctor.CheckContext{CityPath: check.cityPath})
	if r.Status == doctor.StatusOK {
		return nil
	}
	payload, err := json.Marshal(struct {
		Details []string `json:"details"`
		FixHint string   `json:"fix_hint,omitempty"`
	}{r.Details, r.FixHint})
	if err != nil {
		payload = nil
	}
	return &events.Event{Type: bdUninventoriedEvent, Ts: time.Now().UTC(), Actor: "gc", Subject: check.Name(), Message: r.Message, Payload: payload}
}

// warnBdInventoryAtControllerStart records the gc_bd.uninventoried event and
// warns on stderr when the city's bd matches no pinned inventory. It runs
// inline, so the event is in the log before the controller serves: the
// verdict is cached per binary (bd_inventory_trust.go), so only the first
// start after a bd change pays for a walk (a few seconds), and a single-store
// city pays nothing.
func warnBdInventoryAtControllerStart(cityPath string, cfg *config.City, rec events.Recorder, stderr io.Writer) {
	if cfg == nil || workStoreClassHolds(cfg) == nil || rec == nil {
		return
	}
	if ev := bdInventoryStartFinding(newBdInventoryCheck(cityPath, cfg)); ev != nil {
		rec.Record(*ev)
		fmt.Fprintf(stderr, "gc: warning: %s; run `gc doctor` for details\n", ev.Message) //nolint:errcheck // best-effort stderr
	}
}
