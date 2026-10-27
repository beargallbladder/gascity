package bdflags

import (
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The pinned bd command inventories: bd's own cobra command tree — every
// command, alias and flag, hidden ones included, with each flag's type,
// default and NoOptDefVal — dumped from the bd source of each build gc bd's
// split-city allowlist is pinned to (inventorygen/generate.sh). They are the
// ground truth gc resolves a gc bd argv against: it builds a mirror cobra tree
// from the inventory of the bd it is about to run (InventoryFor) and lets the
// same cobra and pflag versions bd uses (v1.10.2 / v1.0.10) resolve the command
// and parse its flags, so the command gc judges is the command bd runs.

//go:embed inventory/*.json
var inventoryFS embed.FS

// InventoryFlag is one flag as bd's cobra tree declares it.
type InventoryFlag struct {
	Name        string `json:"name"`
	Shorthand   string `json:"shorthand,omitempty"`
	Type        string `json:"type"`
	Default     string `json:"default"`
	NoOptDefVal string `json:"no_opt_def_val,omitempty"`
	Hidden      bool   `json:"hidden,omitempty"`
}

// TakesValue reports whether the flag consumes a value when spelled without
// "=" (pflag: no NoOptDefVal).
func (f InventoryFlag) TakesValue() bool { return f.NoOptDefVal == "" }

// InventoryCommand is one command of bd's cobra tree.
type InventoryCommand struct {
	Path               string          `json:"path"`
	Aliases            []string        `json:"aliases,omitempty"`
	Hidden             bool            `json:"hidden,omitempty"`
	Runnable           bool            `json:"runnable"`
	DisableFlagParsing bool            `json:"disable_flag_parsing,omitempty"`
	Local              []InventoryFlag `json:"local,omitempty"`
	Persistent         []InventoryFlag `json:"persistent,omitempty"`
}

// Inventory is one pinned bd build's command tree.
type Inventory struct {
	// Name is the inventory file's base name ("bd-1.3.2").
	Name     string             `json:"-"`
	Version  string             `json:"version"`
	Build    string             `json:"build"`
	Commands []InventoryCommand `json:"commands"`

	byPath map[string]*InventoryCommand
}

// inventoryMatch says which `bd version` output an inventory answers for.
type inventoryMatch struct {
	version    string
	enterprise bool
	// commit, when set, must appear in the version line (an Enterprise build
	// keeps its version string across commits that change its flags).
	commit string
}

// inventoryMatches pins each inventory file to the bd builds it describes.
var inventoryMatches = map[string]inventoryMatch{
	"bd-1.3.2":                         {version: "1.3.2"},
	"bd-1.3.0-enterprise-77612097cec0": {version: "1.3.0", enterprise: true, commit: "77612097cec0"},
}

var (
	inventoriesOnce sync.Once
	inventories     []*Inventory
	inventoriesErr  error
)

// Inventories returns every pinned inventory, sorted by name.
func Inventories() []*Inventory {
	inventoriesOnce.Do(func() {
		entries, err := inventoryFS.ReadDir("inventory")
		if err != nil {
			inventoriesErr = err
			return
		}
		for _, e := range entries {
			data, err := inventoryFS.ReadFile(path.Join("inventory", e.Name()))
			if err != nil {
				inventoriesErr = err
				return
			}
			inv := &Inventory{}
			if err := json.Unmarshal(data, inv); err != nil {
				inventoriesErr = fmt.Errorf("%s: %w", e.Name(), err)
				return
			}
			inv.Name = strings.TrimSuffix(e.Name(), ".json")
			inv.byPath = map[string]*InventoryCommand{}
			for i := range inv.Commands {
				inv.byPath[inv.Commands[i].Path] = &inv.Commands[i]
			}
			inventories = append(inventories, inv)
		}
		sort.Slice(inventories, func(i, j int) bool { return inventories[i].Name < inventories[j].Name })
	})
	if inventoriesErr != nil {
		panic("bdflags: pinned bd inventories: " + inventoriesErr.Error())
	}
	return inventories
}

// bdVersionLineRE matches `bd version <version> (<build>)`.
var bdVersionLineRE = regexp.MustCompile(`(?m)^bd version (\S+)(?: \((.*)\))?`)

// InventoryFor returns the pinned inventory for the bd whose `bd version`
// output is versionOutput, or an error naming the builds gc is pinned to. A bd
// newer (or older, or differently built) than every pinned inventory has
// commands and flags gc has not vetted, so a split city refuses gc bd there.
func InventoryFor(versionOutput string) (*Inventory, error) {
	m := bdVersionLineRE.FindStringSubmatch(versionOutput)
	if m == nil {
		return nil, fmt.Errorf("cannot read bd's version from %q", strings.TrimSpace(versionOutput))
	}
	version, build := m[1], m[2]
	enterprise := strings.Contains(build, "Enterprise")
	for _, inv := range Inventories() {
		want, ok := inventoryMatches[inv.Name]
		if !ok || want.version != version || want.enterprise != enterprise {
			continue
		}
		if want.commit != "" && !strings.Contains(build, want.commit) {
			continue
		}
		return inv, nil
	}
	var pinned []string
	for _, inv := range Inventories() {
		pinned = append(pinned, inv.Name)
	}
	return nil, fmt.Errorf("bd %s (%s) is not a build gc's split-city allowlist is pinned to (%s)", version, build, strings.Join(pinned, ", "))
}

// Command returns the command at path, or nil.
func (inv *Inventory) Command(p string) *InventoryCommand { return inv.byPath[p] }

// Paths returns every command path, sorted ("" is the root).
func (inv *Inventory) Paths() []string {
	out := make([]string, 0, len(inv.Commands))
	for _, c := range inv.Commands {
		out = append(out, c.Path)
	}
	return out
}

// Aliases returns alias path → command path ("mol fart" → "mol bond").
func (inv *Inventory) Aliases() map[string]string {
	out := map[string]string{}
	for _, c := range inv.Commands {
		parent, _ := splitParent(c.Path)
		for _, a := range c.Aliases {
			out[strings.TrimSpace(parent+" "+a)] = c.Path
		}
	}
	return out
}

// Subcommands returns the direct subcommand names of path.
func (inv *Inventory) Subcommands(p string) []string {
	var out []string
	for _, c := range inv.Commands {
		if c.Path == "" {
			continue
		}
		parent, leaf := splitParent(c.Path)
		if parent == p {
			out = append(out, leaf)
		}
	}
	sort.Strings(out)
	return out
}

// Flags returns the flags cobra merges for path: its local flags, its own
// persistent flags, and every ancestor's persistent flags.
func (inv *Inventory) Flags(p string) []InventoryFlag {
	seen := map[string]bool{}
	var out []InventoryFlag
	add := func(fs []InventoryFlag) {
		for _, f := range fs {
			if !seen[f.Name] {
				seen[f.Name] = true
				out = append(out, f)
			}
		}
	}
	for cur := p; ; {
		c := inv.byPath[cur]
		if c != nil {
			if cur == p {
				add(c.Local)
			}
			add(c.Persistent)
		}
		if cur == "" {
			break
		}
		cur, _ = splitParent(cur)
	}
	return out
}

// FlagSpecs returns Flags(path) as FlagSpecs for ParseArgs.
func (inv *Inventory) FlagSpecs(p string) []FlagSpec {
	var out []FlagSpec
	for _, f := range inv.Flags(p) {
		short := ""
		if f.Shorthand != "" {
			short = "-" + f.Shorthand
		}
		out = append(out, FlagSpec{Long: "--" + f.Name, Short: short, TakesValue: f.TakesValue()})
	}
	return out
}

func splitParent(p string) (parent, leaf string) {
	if i := strings.LastIndex(p, " "); i >= 0 {
		return p[:i], p[i+1:]
	}
	if p == "" {
		return "", ""
	}
	return "", p
}

// Resolved is a gc bd argv resolved and parsed exactly as bd resolves and
// parses it.
type Resolved struct {
	// Path is the command bd runs ("" for the root).
	Path string
	// Flags are the flags the argv set, by long name ("--type"); a slice
	// flag carries its elements as pflag split them.
	Flags map[string][]string
	// Positionals are the arguments left after flags.
	Positionals []string
	// Help reports that the argv asks for help, which bd prints instead of
	// running the command.
	Help bool
	// DisableFlagParsing reports a command whose flags bd does not parse.
	DisableFlagParsing bool
}

// Resolve resolves argv against the mirror of this inventory's cobra tree with
// the cobra and pflag versions bd itself uses: cobra's Find (its own stripFlags
// pre-scan, which can disagree with pflag about which token is the
// subcommand), then pflag's parse of the found command's flags.
//
// The mirror is built fresh for each call: cobra and pflag keep parse state
// (Changed, values) on the flag objects, so a shared tree would leak one
// argv's flags into the next.
func (inv *Inventory) Resolve(argv []string) (Resolved, error) {
	root, byCmd := inv.buildCobra()
	cmd, rest, err := root.Find(argv)
	if err != nil {
		return Resolved{}, err
	}
	res := Resolved{Path: byCmd[cmd], DisableFlagParsing: cmd.DisableFlagParsing, Flags: map[string][]string{}}
	if cmd.DisableFlagParsing {
		res.Positionals = rest
		return res, nil
	}
	fs := cmd.Flags()
	// Find has already merged the persistent flags into fs.
	if err := fs.Parse(rest); err != nil {
		return Resolved{}, err
	}
	fs.Visit(func(f *pflag.Flag) {
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			res.Flags["--"+f.Name] = append([]string(nil), sv.GetSlice()...)
			return
		}
		res.Flags["--"+f.Name] = []string{f.Value.String()}
	})
	if v, ok := res.Flags["--help"]; ok && len(v) == 1 && v[0] == "true" {
		res.Help = true
	}
	res.Positionals = fs.Args()
	return res, nil
}

// buildCobra builds the mirror tree. Every command is runnable (a no-op) so
// Find never needs bd's Run functions, and Args is left nil as in bd, so
// Find's legacyArgs check behaves as bd's does.
func (inv *Inventory) buildCobra() (*cobra.Command, map[*cobra.Command]string) {
	byCmd := map[*cobra.Command]string{}
	byPath := map[string]*cobra.Command{}
	for _, ic := range inv.Commands {
		_, leaf := splitParent(ic.Path)
		c := &cobra.Command{
			Use:                leaf,
			Aliases:            ic.Aliases,
			Hidden:             ic.Hidden,
			DisableFlagParsing: ic.DisableFlagParsing,
			SilenceErrors:      true,
			SilenceUsage:       true,
		}
		if ic.Runnable {
			c.Run = func(*cobra.Command, []string) {}
		}
		if ic.Path == "" {
			c.Use = "bd"
		}
		for _, f := range ic.Local {
			addMirrorFlag(c.Flags(), f)
		}
		for _, f := range ic.Persistent {
			addMirrorFlag(c.PersistentFlags(), f)
		}
		byPath[ic.Path] = c
		byCmd[c] = ic.Path
	}
	// Parents before children: paths sort with every prefix first.
	paths := inv.Paths()
	sort.Strings(paths)
	for _, p := range paths {
		if p == "" {
			continue
		}
		parent, _ := splitParent(p)
		byPath[parent].AddCommand(byPath[p])
	}
	return byPath[""], byCmd
}

// addMirrorFlag declares f on fs with pflag's own value type, so Set parses
// exactly as bd's flag does (a stringSlice reads one CSV record, a bool
// rejects anything strconv.ParseBool does, an int rejects non-numbers).
func addMirrorFlag(fs *pflag.FlagSet, f InventoryFlag) {
	switch f.Type {
	case "bool":
		fs.BoolP(f.Name, f.Shorthand, f.Default == "true", "")
	case "int":
		fs.IntP(f.Name, f.Shorthand, 0, "")
	case "int64":
		fs.Int64P(f.Name, f.Shorthand, 0, "")
	case "float64":
		fs.Float64P(f.Name, f.Shorthand, 0, "")
	case "duration":
		fs.DurationP(f.Name, f.Shorthand, 0, "")
	case "stringSlice":
		fs.StringSliceP(f.Name, f.Shorthand, nil, "")
	case "stringArray":
		fs.StringArrayP(f.Name, f.Shorthand, nil, "")
	default:
		fs.StringP(f.Name, f.Shorthand, "", "")
	}
	pf := fs.Lookup(f.Name)
	pf.NoOptDefVal = f.NoOptDefVal
	pf.Hidden = f.Hidden
}
