package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

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
	resolve func(cityPath string, cfg *config.City, bdPath string, opts bdTrustOpts) (bdTrustVerdict, error)
}

func newBdInventoryCheck(cityPath string, cfg *config.City) *bdInventoryCheck {
	return &bdInventoryCheck{cityPath: cityPath, cfg: cfg, resolve: resolveBdInventory}
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
	var bad, warn []string
	if c.cfg.Storage != nil {
		if v := strings.TrimSpace(c.cfg.Storage.GCBdInventory); v != "" {
			if _, _, ok := config.ParseGCBdInventory(v); !ok {
				warn = append(warn, fmt.Sprintf("[storage] gc_bd_inventory %q is not \"<inventory-id>@sha256:<64 lowercase hex>\"; it is ignored (gc doctor --fix rewrites it)", v))
			}
		}
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
		v, err := c.resolve(c.cityPath, c.cfg, bdPath, bdTrustOpts{})
		if err == nil && v.Vouched {
			// The vouch admitted it without a walk; the doctor walks it.
			walked, werr := c.resolve(c.cityPath, c.cfg, bdPath, bdTrustOpts{forceWalk: true})
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
		r.FixHint = "install a pinned bd build (see internal/bdflags/inventory), or have the gc maintainers inventory this build; `gc doctor --fix` vouches for the city's bd only when its tree matches"
		return r
	}
	if len(warn) > 0 {
		r.Status = doctor.StatusWarning
		r.Message = "every scope's bd matches a pinned gc bd inventory, but the city's vouch is malformed"
		r.Details = warn
		r.FixHint = "`gc doctor --fix` rewrites the vouch after walking the city's bd"
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

// Fix walks the city scope's bd for real and, on a match, vouches for that
// exact binary in city.toml.
func (c *bdInventoryCheck) Fix(_ *doctor.CheckContext) error {
	if c.cfg == nil || workStoreClassHolds(c.cfg) == nil {
		return nil
	}
	bdPath, err := resolveBdBinaryForScope(c.cityPath, bdCityScopeTarget(c.cityPath, c.cfg).ScopeRoot)
	if err != nil {
		return err
	}
	v, err := c.resolve(c.cityPath, c.cfg, bdPath, bdTrustOpts{forceWalk: true})
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

// writeGCBdInventoryVouch sets [storage] gc_bd_inventory in city.toml. It
// edits the text (replacing an existing key in the [storage] table, adding
// one under the table's header, or adding a [storage] table ahead of the
// first [storage.*] table, else at the end), then decodes the result and
// writes it only if the vouch reads back and nothing else in the document
// changed. Any other shape (a dotted or inline storage key, a header inside
// a multi-line string, a file that does not parse) is refused: the edit is
// the operator's.
func writeGCBdInventoryVouch(path, value string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	refuse := func(why string) error {
		return fmt.Errorf("not writing the vouch to %s: %s; add gc_bd_inventory = %q under [storage] by hand", path, why, value)
	}
	var before map[string]any
	if _, err := toml.Decode(string(data), &before); err != nil {
		return refuse(fmt.Sprintf("it does not parse (%v)", err))
	}
	candidate, headed := vouchedCityTOML(string(data), value)
	var after map[string]any
	if _, err := toml.Decode(candidate, &after); err != nil {
		return refuse(fmt.Sprintf("the edit would not parse (%v)", err))
	}
	expected := before
	if expected == nil {
		expected = map[string]any{}
	}
	storage, ok := expected["storage"].(map[string]any)
	if !ok {
		if _, present := expected["storage"]; present {
			return refuse("its storage key is not a table")
		}
		storage = map[string]any{}
		expected["storage"] = storage
	}
	if !headed {
		// storage keys set by dotted keys or an inline table: a [storage]
		// header after them redefines the table, which TOML forbids even
		// where a lenient decoder accepts it.
		for _, v := range storage {
			if _, sub := v.(map[string]any); !sub {
				return refuse("its [storage] keys are set by dotted keys or an inline table, not a [storage] header")
			}
		}
	}
	storage["gc_bd_inventory"] = value
	if !reflect.DeepEqual(expected, after) {
		return refuse("the edit would change more than [storage] gc_bd_inventory")
	}
	return fsys.WriteFileIfChangedAtomic(fsys.OSFS{}, path, []byte(candidate), 0o644)
}

// tomlHeaderRE matches a standard table header line, comments allowed.
var tomlHeaderRE = regexp.MustCompile(`^\s*\[\s*([^\[\]#]+?)\s*\]\s*(#.*)?$`)

// vouchedCityTOML is writeGCBdInventoryVouch's text edit; the caller
// verifies it. headed reports that the document has a [storage] header.
func vouchedCityTOML(doc, value string) (candidate string, headed bool) {
	line := fmt.Sprintf("gc_bd_inventory = %q", value)
	lines := strings.Split(doc, "\n")
	inStorage, storageAt, firstSub := false, -1, -1
	inMultiline := ""
	for i, l := range lines {
		if inMultiline != "" {
			if strings.Count(l, inMultiline)%2 == 1 {
				inMultiline = ""
			}
			continue
		}
		t := strings.TrimSpace(l)
		if m := tomlHeaderRE.FindStringSubmatch(l); m != nil && !strings.HasPrefix(t, "[[") {
			name := strings.Join(strings.Fields(m[1]), "")
			inStorage = name == "storage"
			if inStorage && storageAt < 0 {
				storageAt = i
			}
			if strings.HasPrefix(name, "storage.") && firstSub < 0 {
				firstSub = i
			}
			continue
		}
		if inStorage {
			if key, _, ok := strings.Cut(t, "="); ok && strings.TrimSpace(key) == "gc_bd_inventory" {
				lines[i] = line
				return strings.Join(lines, "\n"), true
			}
		}
		for _, q := range []string{`"""`, `'''`} {
			if strings.Count(l, q)%2 == 1 {
				inMultiline = q
				break
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
	return strings.Join(lines, "\n"), storageAt >= 0
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
