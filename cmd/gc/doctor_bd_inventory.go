package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
)

// bdInventoryCheck is the doctor check behind the split-city bd trust policy
// (bd_inventory_trust.go): every bd-backed scope's bd binary must have a
// command tree that matches a pinned gc bd inventory, or gc bd refuses every
// command there — refusals a script's `|| true` would otherwise hide. It is
// warm-up eligible, so `gc start` warns about it too.
//
// It has no fix: the remedy for a bd that matches nothing is a pinned bd
// build. `gc storage vouch-bd` records a vouch for a binary that does match
// (so gc bd skips the walk), and this check re-walks every vouched binary.
type bdInventoryCheck struct {
	cityPath string
	cfg      *config.City
	// resolve is resolveBdInventory, injectable for tests.
	resolve func(cityPath, bdPath string, opts bdTrustOpts) (bdTrustVerdict, error)
}

func newBdInventoryCheck(cityPath string, cfg *config.City) *bdInventoryCheck {
	return &bdInventoryCheck{cityPath: cityPath, cfg: cfg, resolve: resolveBdInventory}
}

func (c *bdInventoryCheck) Name() string                     { return "bd-command-inventory" }
func (c *bdInventoryCheck) CanFix() bool                     { return false }
func (c *bdInventoryCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (c *bdInventoryCheck) WarmupEligible() bool             { return true }

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
	var bad, warn []string
	if _, err := loadBdVouches(c.cityPath); err != nil {
		warn = append(warn, fmt.Sprintf("the vouch file is ignored: %v (gc storage vouch-bd rewrites it)", err))
	}
	// One check per resolved binary: a city whose rigs share one bd walks it
	// once.
	seen := map[string]bool{}
	for _, target := range c.scopes() {
		if !providerUsesBdStoreContract(rawBeadsProviderForScope(target.ScopeRoot, c.cityPath)) {
			continue
		}
		bdPath, err := resolveBdBinaryForScope(c.cityPath, target.ScopeRoot)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", target.ScopeRoot, err))
			continue
		}
		key := bdPath
		if abs, err := filepath.EvalSymlinks(bdPath); err == nil {
			key = abs
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		v, err := c.resolve(c.cityPath, bdPath, bdTrustOpts{})
		if err == nil && v.Vouched {
			// The vouch admitted it without a walk; the doctor walks it.
			walked, werr := c.resolve(c.cityPath, bdPath, bdTrustOpts{forceWalk: true})
			switch {
			case werr != nil:
				bad = append(bad, fmt.Sprintf("%s: %s is vouched for as %s, and re-walking it failed: %v", target.ScopeRoot, bdPath, v.Inventory, werr))
				continue
			case walked.Inventory != v.Inventory:
				bad = append(bad, fmt.Sprintf("%s: %s is vouched for as %s, but its command tree matches %s: %s", target.ScopeRoot, bdPath, v.Inventory, orNone(walked.Inventory), strings.Join(walked.Diffs, "; ")))
				continue
			}
		}
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
		bad = append(bad, warn...)
		r.Details = bad
		r.FixHint = "install a pinned bd build (see internal/bdflags/inventory), or have the gc maintainers inventory this build"
		return r
	}
	if len(warn) > 0 {
		r.Status = doctor.StatusWarning
		r.Message = "every scope's bd matches a pinned gc bd inventory, but the vouch file is unreadable"
		r.Details = warn
		r.FixHint = "`gc storage vouch-bd` walks the city's bd and rewrites the vouch file"
		return r
	}
	r.Status = doctor.StatusOK
	r.Message = "every scope's bd matches a pinned gc bd inventory"
	return r
}

func orNone(s string) string {
	if s == "" {
		return "no pinned inventory"
	}
	return s
}

func shortSHA(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
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

// warnBdInventoryAtControllerStart records the gc_bd.uninventoried event
// when the city's bd matches no pinned inventory. It runs in the background
// and never blocks or fails a start (an uncached walk takes seconds); a
// single-store city pays nothing. The doctor check and gc start's warm-up
// print the same finding.
func warnBdInventoryAtControllerStart(cityPath string, cfg *config.City, rec events.Recorder) {
	if cfg == nil || workStoreClassHolds(cfg) == nil || rec == nil {
		return
	}
	go func() {
		defer func() { _ = recover() }() // best-effort: never take the controller down
		if ev := bdInventoryStartFinding(newBdInventoryCheck(cityPath, cfg)); ev != nil {
			rec.Record(*ev)
		}
	}()
}
