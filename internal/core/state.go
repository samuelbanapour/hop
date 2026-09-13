package core

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Installed records one package inside a generation. Everything needed to
// reconstruct the generation lives here, so a manifest is self-contained and
// hop never has to consult the index to describe what is installed.
type Installed struct {
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	StorePath string    `json:"store_path"`
	Kind      Kind      `json:"kind,omitempty"`
	SHA256    string    `json:"sha256,omitempty"`
	SHA512    string    `json:"sha512,omitempty"`
	SHA1      string    `json:"sha1,omitempty"`
	Deps      []string  `json:"deps,omitempty"`
	Bins      []BinLink `json:"bins,omitempty"`
	Mans      []string  `json:"mans,omitempty"`
	Platform  Platform  `json:"platform,omitempty"`
	Source    string    `json:"source,omitempty"`
	Size      int64     `json:"size,omitempty"`
	Explicit  bool      `json:"explicit"`
	At        time.Time `json:"installed_at"`
}

// Generation is an immutable snapshot of a complete installed set. Installing,
// removing and upgrading all work by building a new generation and flipping
// one symlink, which is why every operation is atomic and reversible.
type Generation struct {
	ID         int         `json:"id"`
	Parent     int         `json:"parent"`
	Created    time.Time   `json:"created"`
	Command    string      `json:"command,omitempty"`
	HopVersion string      `json:"hop_version,omitempty"`
	Packages   []Installed `json:"packages"`
}

// Find returns the installed record for name.
// ImagePath returns the absolute path to a KindImage package's stored file —
// the exact bytes hop verified, never extracted, ready to hand to qemu,
// docker import, or whatever else expects them. Empty for anything else.
func (i *Installed) ImagePath() string {
	if i == nil || i.Kind != KindImage || i.StorePath == "" || i.Source == "" {
		return ""
	}
	name := baseName(strings.SplitN(i.Source, "?", 2)[0])
	if name == "" {
		return ""
	}
	return i.StorePath + "/" + name
}

func (g *Generation) Find(name string) (*Installed, bool) {
	if g == nil {
		return nil, false
	}
	for i := range g.Packages {
		if strings.EqualFold(g.Packages[i].Name, name) {
			return &g.Packages[i], true
		}
	}
	return nil, false
}

// Names lists installed package names, sorted.
func (g *Generation) Names() []string {
	if g == nil {
		return nil
	}
	out := make([]string, 0, len(g.Packages))
	for _, p := range g.Packages {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

// Size sums the store footprint of this generation.
func (g *Generation) Size() int64 {
	var n int64
	if g == nil {
		return 0
	}
	for _, p := range g.Packages {
		n += p.Size
	}
	return n
}

// Clone returns a deep-enough copy to mutate while planning a new generation.
func (g *Generation) Clone() []Installed {
	if g == nil {
		return nil
	}
	out := make([]Installed, len(g.Packages))
	copy(out, g.Packages)
	return out
}

// ------------------------------------------------------------ generations ----

// CurrentID returns the active generation number, or 0 when hop has never
// activated anything.
func CurrentID(l *Layout) int {
	target, err := os.Readlink(l.Current())
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(filepath.Base(target))
	if err != nil {
		return 0
	}
	return n
}

// Current loads the active generation. A nil generation with a nil error means
// "nothing installed yet", which every caller must handle.
func Current(l *Layout) (*Generation, error) {
	id := CurrentID(l)
	if id == 0 {
		return nil, nil
	}
	return LoadGeneration(l, id)
}

// LoadGeneration reads generation n's manifest.
func LoadGeneration(l *Layout, n int) (*Generation, error) {
	var g Generation
	if err := readJSON(l.ProfileManifest(n), &g); err != nil {
		return nil, fmt.Errorf("reading generation %d: %w", n, err)
	}
	return &g, nil
}

// Generations lists every generation on disk, oldest first.
func Generations(l *Layout) ([]*Generation, error) {
	ents, err := os.ReadDir(l.Profiles())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Generation
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		g, err := LoadGeneration(l, n)
		if err != nil {
			continue // a half-built generation is simply not listed
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// nextID picks the next generation number, always above every existing one so
// a rollback followed by an install never reuses an id.
func nextID(l *Layout) int {
	gens, _ := Generations(l)
	maxID := 0
	for _, g := range gens {
		if g.ID > maxID {
			maxID = g.ID
		}
	}
	return maxID + 1
}

// Commit writes a new generation containing pkgs and returns it, without
// activating it. Splitting commit from activation keeps the window in which
// the user's environment is inconsistent down to a single rename.
func Commit(l *Layout, pkgs []Installed, command string) (*Generation, error) {
	id := nextID(l)
	g := &Generation{
		ID:         id,
		Parent:     CurrentID(l),
		Created:    time.Now(),
		Command:    command,
		HopVersion: Version,
		Packages:   pkgs,
	}
	sort.Slice(g.Packages, func(i, j int) bool { return g.Packages[i].Name < g.Packages[j].Name })

	dir := l.Profile(id)
	if err := removeTree(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		return nil, err
	}

	if err := linkFarm(l, g); err != nil {
		_ = removeTree(dir)
		return nil, err
	}
	if err := writeJSONAtomic(l.ProfileManifest(id), g, 0o644); err != nil {
		_ = removeTree(dir)
		return nil, err
	}
	return g, nil
}

// Conflict reports two packages claiming the same command name.
type Conflict struct {
	Bin    string
	Winner string
	Loser  string
}

var lastConflicts []Conflict

// Conflicts returns the command-name collisions from the last linkFarm call.
func Conflicts() []Conflict { return lastConflicts }

// linkFarm materialises generation g's bin/ and share/man/ symlinks.
// Explicitly requested packages win any name collision, since that is what
// the user actually asked to be on PATH.
func linkFarm(l *Layout, g *Generation) error {
	binDir := filepath.Join(l.Profile(g.ID), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}

	order := make([]Installed, len(g.Packages))
	copy(order, g.Packages)
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].Explicit != order[j].Explicit {
			return order[i].Explicit // explicit first
		}
		return order[i].Name < order[j].Name
	})

	lastConflicts = nil
	owner := map[string]string{}

	for _, p := range order {
		for _, b := range p.Bins {
			name := b.Name
			if name == "" {
				name = baseName(b.Path)
			}
			if name == "" {
				continue
			}
			if prev, taken := owner[name]; taken {
				if prev != p.Name {
					lastConflicts = append(lastConflicts, Conflict{Bin: name, Winner: prev, Loser: p.Name})
				}
				continue
			}
			target := b.Path
			if !filepath.IsAbs(target) {
				target = filepath.Join(p.StorePath, b.Path)
			}
			link := filepath.Join(binDir, name)
			_ = os.Remove(link)
			if err := os.Symlink(target, link); err != nil {
				return fmt.Errorf("linking %s: %w", name, err)
			}
			owner[name] = p.Name
		}

		for _, m := range p.Mans {
			section := manSection(m)
			manDir := filepath.Join(l.Profile(g.ID), "share", "man", "man"+section)
			if err := os.MkdirAll(manDir, 0o755); err != nil {
				continue
			}
			link := filepath.Join(manDir, baseName(m))
			_ = os.Remove(link)
			target := m
			if !filepath.IsAbs(target) {
				target = filepath.Join(p.StorePath, m)
			}
			_ = os.Symlink(target, link)
		}
	}
	return nil
}

func manSection(path string) string {
	b := baseName(path)
	if i := strings.LastIndexByte(b, '.'); i >= 0 && i < len(b)-1 {
		s := b[i+1:]
		if len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
			return s
		}
	}
	return "1"
}

// Activate points <root>/current at generation n. This single rename is the
// entire switchover: every command on PATH changes together, or not at all.
func Activate(l *Layout, n int) error {
	if !exists(l.ProfileManifest(n)) {
		return fmt.Errorf("generation %d does not exist", n)
	}
	// A relative target keeps the whole root relocatable.
	return replaceSymlink(filepath.Join("profiles", strconv.Itoa(n)), l.Current())
}

// DropGeneration deletes a generation's profile directory. The store is left
// alone; `hop gc` reclaims it once nothing references the paths.
func DropGeneration(l *Layout, n int) error {
	if n == CurrentID(l) {
		return fmt.Errorf("refusing to delete the active generation %d", n)
	}
	return removeTree(l.Profile(n))
}

// ------------------------------------------------------------------- query ----

// Owner reports which installed package provides a command name.
func Owner(l *Layout, bin string) (*Installed, bool) {
	g, err := Current(l)
	if err != nil || g == nil {
		return nil, false
	}
	for i := range g.Packages {
		for _, b := range g.Packages[i].Bins {
			if b.Name == bin || baseName(b.Path) == bin {
				return &g.Packages[i], true
			}
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------- gc ----

// GCPlan describes what a sweep would reclaim, so `hop gc --dry-run` can show
// the user the bill before charging it.
type GCPlan struct {
	StorePaths  []string
	Generations []int
	Downloads   []string
	Bytes       int64
}

// Empty reports whether there is nothing to reclaim.
func (p *GCPlan) Empty() bool {
	return len(p.StorePaths) == 0 && len(p.Generations) == 0 && len(p.Downloads) == 0
}

// PlanGC computes a mark-and-sweep over the store. keep limits how many
// generations to retain (0 = all); the active generation is always retained.
// Anything still reachable from a retained generation is never collected,
// which is what makes rollback safe to rely on.
func PlanGC(l *Layout, keep int) (*GCPlan, error) {
	gens, err := Generations(l)
	if err != nil {
		return nil, err
	}
	cur := CurrentID(l)

	// Decide which generations survive.
	doomed := map[int]bool{}
	if keep > 0 && len(gens) > keep {
		// Retain the newest `keep`, always including the active one.
		sort.Slice(gens, func(i, j int) bool { return gens[i].ID > gens[j].ID })
		kept := 0
		for _, g := range gens {
			if g.ID == cur {
				continue // counted separately; never dropped
			}
			if kept < keep-1 {
				kept++
				continue
			}
			doomed[g.ID] = true
		}
		sort.Slice(gens, func(i, j int) bool { return gens[i].ID < gens[j].ID })
	}

	// Mark: every store path reachable from a surviving generation.
	live := map[string]bool{}
	plan := &GCPlan{}
	for _, g := range gens {
		if doomed[g.ID] {
			plan.Generations = append(plan.Generations, g.ID)
			continue
		}
		for _, p := range g.Packages {
			live[filepath.Clean(p.StorePath)] = true
		}
	}
	sort.Ints(plan.Generations)

	// Sweep the store.
	ents, err := os.ReadDir(l.Store())
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(l.Store(), e.Name())
		if strings.HasPrefix(e.Name(), ".stage-") {
			// An abandoned staging directory from an interrupted run.
			plan.StorePaths = append(plan.StorePaths, p)
			if n, err := dirSize(p); err == nil {
				plan.Bytes += n
			}
			continue
		}
		if live[filepath.Clean(p)] {
			continue
		}
		plan.StorePaths = append(plan.StorePaths, p)
		if n, err := dirSize(p); err == nil {
			plan.Bytes += n
		}
	}

	// Sweep cached downloads: they are pure cache, always safe to drop.
	dents, err := os.ReadDir(l.Downloads())
	if err == nil {
		for _, e := range dents {
			if e.IsDir() {
				continue
			}
			p := filepath.Join(l.Downloads(), e.Name())
			plan.Downloads = append(plan.Downloads, p)
			if fi, err := e.Info(); err == nil {
				plan.Bytes += fi.Size()
			}
		}
	}

	sort.Strings(plan.StorePaths)
	sort.Strings(plan.Downloads)
	return plan, nil
}

// RunGC executes a plan, returning the bytes actually reclaimed.
func RunGC(l *Layout, plan *GCPlan) (int64, error) {
	var freed int64
	var firstErr error

	for _, n := range plan.Generations {
		if err := DropGeneration(l, n); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, p := range plan.StorePaths {
		sz, _ := dirSize(p)
		if err := removeTree(p); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		freed += sz
	}
	for _, p := range plan.Downloads {
		var sz int64
		if fi, err := os.Stat(p); err == nil {
			sz = fi.Size()
		}
		if err := os.Remove(p); err == nil {
			freed += sz
		}
	}
	return freed, firstErr
}

// StoreStats summarises store occupancy for `hop doctor`.
type StoreStats struct {
	Paths      int
	Bytes      int64
	CacheBytes int64
	Free       int64
	Gens       int
}

// Stats gathers store statistics.
func Stats(l *Layout) (*StoreStats, error) {
	s := &StoreStats{}
	ents, err := os.ReadDir(l.Store())
	if err == nil {
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			s.Paths++
			if n, err := dirSize(filepath.Join(l.Store(), e.Name())); err == nil {
				s.Bytes += n
			}
		}
	}
	if n, err := dirSize(l.Cache()); err == nil {
		s.CacheBytes = n
	}
	if n, err := freeSpace(l.Root); err == nil {
		s.Free = n
	}
	gens, _ := Generations(l)
	s.Gens = len(gens)
	return s, nil
}
