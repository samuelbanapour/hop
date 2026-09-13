package core

import (
	"fmt"
	"sort"
	"strings"
)

// Action is what a plan step will do to one package.
type Action int

// Plan step actions.
const (
	ActionInstall Action = iota
	ActionUpgrade
	ActionDowngrade
	ActionReinstall
	ActionRemove
	ActionPromote // already installed as a dependency; mark it explicit
)

// String renders an action as the verb shown in plan output.
func (a Action) String() string {
	switch a {
	case ActionInstall:
		return "install"
	case ActionUpgrade:
		return "upgrade"
	case ActionDowngrade:
		return "downgrade"
	case ActionReinstall:
		return "reinstall"
	case ActionRemove:
		return "remove"
	case ActionPromote:
		return "keep"
	default:
		return "?"
	}
}

// Mutates reports whether the action needs a download and extraction.
func (a Action) Mutates() bool {
	switch a {
	case ActionInstall, ActionUpgrade, ActionDowngrade, ActionReinstall:
		return true
	}
	return false
}

// Step is one package's worth of work in a plan.
type Step struct {
	Action   Action
	Name     string
	Version  string // target version ("" for removals)
	From     string // currently installed version, if any
	Recipe   *Recipe
	Artifact *Artifact
	Platform Platform
	Explicit bool
	Reason   string // why this step is in the plan
	Rosetta  bool   // selected artifact is an emulated fallback
}

// Plan is an ordered, reviewable description of a transaction. Every mutating
// command produces one first, which is what makes `--dry-run` trustworthy:
// the same plan that prints is the plan that runs.
type Plan struct {
	Steps    []Step
	Warnings []string

	// Final is the complete package set the resulting generation will contain.
	Final []Installed
}

// Empty reports whether the plan would change nothing.
func (p *Plan) Empty() bool {
	for _, s := range p.Steps {
		if s.Action != ActionPromote {
			return false
		}
	}
	return len(p.Steps) == 0
}

// Downloads lists the steps that need a network fetch.
func (p *Plan) Downloads() []Step {
	var out []Step
	for _, s := range p.Steps {
		if s.Action.Mutates() {
			out = append(out, s)
		}
	}
	return out
}

// DownloadBytes estimates the transfer, using recorded artifact sizes.
func (p *Plan) DownloadBytes() int64 {
	var n int64
	for _, s := range p.Downloads() {
		if s.Artifact != nil {
			n += s.Artifact.Size
		}
	}
	return n
}

// Counts summarises the plan by action, for the one-line confirmation.
func (p *Plan) Counts() map[Action]int {
	m := map[Action]int{}
	for _, s := range p.Steps {
		m[s.Action]++
	}
	return m
}

// ------------------------------------------------------------------ errors ----

// UnknownPackageError carries suggestions so the message can fix the typo.
type UnknownPackageError struct {
	Name    string
	Suggest []string
}

func (e *UnknownPackageError) Error() string {
	return fmt.Sprintf("no package named %q", e.Name)
}

// UnsupportedPlatformError means the recipe exists but has nothing to install
// for this machine.
type UnsupportedPlatformError struct {
	Name      string
	Version   string
	Platform  Platform
	Available []string
}

func (e *UnsupportedPlatformError) Error() string {
	if len(e.Available) == 0 {
		return fmt.Sprintf("%s %s has no prebuilt artifacts at all", e.Name, e.Version)
	}
	return fmt.Sprintf("%s %s has no artifact for %s (available: %s)",
		e.Name, e.Version, e.Platform, strings.Join(e.Available, ", "))
}

// CycleError reports a dependency cycle in the index.
type CycleError struct{ Path []string }

func (e *CycleError) Error() string {
	return "dependency cycle: " + strings.Join(e.Path, " → ")
}

// ---------------------------------------------------------------- resolving ----

// Resolver turns requests into plans against an index and the current state.
type Resolver struct {
	Index    *Index
	Current  *Generation
	Platform Platform
}

// NewResolver builds a resolver. A nil generation means nothing is installed.
func NewResolver(ix *Index, cur *Generation, plat Platform) *Resolver {
	return &Resolver{Index: ix, Current: cur, Platform: plat}
}

// request is a package to install, with why it is wanted.
type request struct {
	name     string
	explicit bool
	reason   string
}

// selection is a recipe chosen for installation.
type selection struct {
	recipe   *Recipe
	artifact *Artifact
	plat     Platform
	explicit bool
	reason   string
}

// expand resolves the transitive closure of the requested names, in
// dependency-first order, detecting cycles and missing packages.
func (r *Resolver) expand(reqs []request) ([]selection, error) {
	const (
		white = 0 // unvisited
		grey  = 1 // on the current DFS path
		black = 2 // finished
	)
	colour := map[string]int{}
	chosen := map[string]*selection{}
	var order []string
	var path []string

	var visit func(req request) error
	visit = func(req request) error {
		key := strings.ToLower(req.name)

		switch colour[key] {
		case grey:
			return &CycleError{Path: append(append([]string{}, path...), req.name)}
		case black:
			// Already selected; an explicit request upgrades its status so the
			// package survives a later `hop remove` of whatever pulled it in.
			if sel := chosen[key]; sel != nil && req.explicit && !sel.explicit {
				sel.explicit, sel.reason = true, req.reason
			}
			return nil
		}

		rec, ok := r.Index.Lookup(req.name)
		if !ok {
			return &UnknownPackageError{Name: req.name, Suggest: r.Index.Suggest(req.name, 3)}
		}
		art, plat, ok := rec.Artifact(r.Platform)
		if !ok {
			return &UnsupportedPlatformError{
				Name: rec.Name, Version: rec.Version,
				Platform: r.Platform, Available: rec.Platforms(),
			}
		}

		colour[key] = grey
		path = append(path, rec.Name)
		for _, dep := range rec.Deps {
			if err := visit(request{name: dep, explicit: false, reason: "dependency of " + rec.Name}); err != nil {
				return err
			}
		}
		path = path[:len(path)-1]
		colour[key] = black

		chosen[key] = &selection{
			recipe: rec, artifact: art, plat: plat,
			explicit: req.explicit, reason: req.reason,
		}
		order = append(order, key) // dependencies appended before their parent
		return nil
	}

	for _, req := range reqs {
		if err := visit(req); err != nil {
			return nil, err
		}
	}

	out := make([]selection, 0, len(order))
	for _, k := range order {
		out = append(out, *chosen[k])
	}
	return out, nil
}

// PlanInstall builds a plan that adds names (and their dependencies) to the
// current set. force turns a no-op into an explicit reinstall.
func (r *Resolver) PlanInstall(names []string, force bool) (*Plan, error) {
	reqs := make([]request, 0, len(names))
	for _, n := range names {
		reqs = append(reqs, request{name: n, explicit: true, reason: "requested"})
	}
	sels, err := r.expand(reqs)
	if err != nil {
		return nil, err
	}

	plan := &Plan{}
	final := r.Current.Clone()

	upsert := func(inst Installed) {
		for i := range final {
			if strings.EqualFold(final[i].Name, inst.Name) {
				// Preserve an existing explicit flag: being pulled in as a
				// dependency must not demote a package the user chose.
				inst.Explicit = inst.Explicit || final[i].Explicit
				final[i] = inst
				return
			}
		}
		final = append(final, inst)
	}

	for _, s := range sels {
		cur, installed := r.Current.Find(s.recipe.Name)

		action := ActionInstall
		from := ""
		switch {
		case !installed:
			action = ActionInstall
		case force:
			action, from = ActionReinstall, cur.Version
		case cur.Version == s.recipe.Version:
			// Same version: nothing to download. Still worth a step if this
			// promotes a dependency to an explicit install.
			if s.explicit && !cur.Explicit {
				plan.Steps = append(plan.Steps, Step{
					Action: ActionPromote, Name: s.recipe.Name, Version: cur.Version,
					From: cur.Version, Recipe: s.recipe, Platform: s.plat,
					Explicit: true, Reason: "already installed as a dependency",
				})
				promoted := *cur
				promoted.Explicit = true
				upsert(promoted)
			}
			continue
		case VersionNewer(cur.Version, s.recipe.Version):
			action, from = ActionUpgrade, cur.Version
		default:
			action, from = ActionDowngrade, cur.Version
		}

		plan.Steps = append(plan.Steps, Step{
			Action: action, Name: s.recipe.Name, Version: s.recipe.Version, From: from,
			Recipe: s.recipe, Artifact: s.artifact, Platform: s.plat,
			Explicit: s.explicit, Reason: s.reason,
			Rosetta: r.Platform.IsRosettaFallback(s.plat),
		})
		upsert(Installed{
			Name: s.recipe.Name, Version: s.recipe.Version, Kind: s.recipe.Kind,
			SHA256: s.artifact.SHA256, SHA512: s.artifact.SHA512,
			Platform: s.plat, Source: s.artifact.URL, Deps: s.recipe.Deps,
			Explicit: s.explicit,
		})
		if r.Platform.IsRosettaFallback(s.plat) {
			if s.recipe.Kind == KindImage {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf(
					"%s has no arm64 build; using the x86_64 one — that's fine for a static image, unlike a CLI tool",
					s.recipe.Name))
			} else {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf(
					"%s has no native %s build; installing the x86_64 build to run under Rosetta 2",
					s.recipe.Name, r.Platform))
			}
		}
		if s.artifact.SHA256 == "" && s.artifact.SHA512 == "" {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"%s has no pinned checksum; hop will record the digest it observes (trust on first use)",
				s.recipe.Name))
		}
	}

	plan.Final = final
	return plan, nil
}

// PlanRemove builds a plan that removes names. Dependencies that nothing else
// needs are swept too, which is the leak Homebrew leaves behind.
func (r *Resolver) PlanRemove(names []string, keepOrphans bool) (*Plan, error) {
	if r.Current == nil || len(r.Current.Packages) == 0 {
		return nil, fmt.Errorf("nothing is installed")
	}

	remove := map[string]bool{}
	for _, n := range names {
		inst, ok := r.Current.Find(n)
		if !ok {
			sug := r.Index.Suggest(n, 3)
			// Prefer suggesting something actually installed.
			var installedSug []string
			for _, s := range sug {
				if _, ok := r.Current.Find(s); ok {
					installedSug = append(installedSug, s)
				}
			}
			if len(installedSug) > 0 {
				sug = installedSug
			}
			return nil, &UnknownPackageError{Name: n, Suggest: sug}
		}
		remove[strings.ToLower(inst.Name)] = true
	}

	// Survivors: every explicit package not being removed.
	var final []Installed
	for _, p := range r.Current.Packages {
		if !remove[strings.ToLower(p.Name)] {
			final = append(final, p)
		}
	}

	if !keepOrphans {
		// Mark everything reachable from a surviving explicit package.
		byName := map[string]*Installed{}
		for i := range final {
			byName[strings.ToLower(final[i].Name)] = &final[i]
		}
		live := map[string]bool{}
		var mark func(string)
		mark = func(name string) {
			key := strings.ToLower(name)
			if live[key] {
				return
			}
			live[key] = true
			if p, ok := byName[key]; ok {
				for _, d := range p.Deps {
					mark(d)
				}
			} else if rec, ok := r.Index.Lookup(name); ok {
				for _, d := range rec.Deps {
					mark(d)
				}
			}
		}
		for i := range final {
			if final[i].Explicit {
				mark(final[i].Name)
			}
		}
		var kept []Installed
		for _, p := range final {
			if live[strings.ToLower(p.Name)] {
				kept = append(kept, p)
				continue
			}
			remove[strings.ToLower(p.Name)] = true // newly orphaned
		}
		final = kept
	}

	plan := &Plan{Final: final}
	for _, p := range r.Current.Packages {
		if !remove[strings.ToLower(p.Name)] {
			continue
		}
		reason := "requested"
		explicitlyNamed := false
		for _, n := range names {
			if strings.EqualFold(n, p.Name) {
				explicitlyNamed = true
			}
		}
		if !explicitlyNamed {
			reason = "no longer needed"
		}
		plan.Steps = append(plan.Steps, Step{
			Action: ActionRemove, Name: p.Name, From: p.Version,
			Version: p.Version, Explicit: p.Explicit, Reason: reason,
		})
	}
	sort.SliceStable(plan.Steps, func(i, j int) bool {
		if plan.Steps[i].Reason != plan.Steps[j].Reason {
			return plan.Steps[i].Reason == "requested"
		}
		return plan.Steps[i].Name < plan.Steps[j].Name
	})
	return plan, nil
}

// PlanUpgrade builds a plan advancing installed packages to index versions.
// With no names, every installed package is considered.
func (r *Resolver) PlanUpgrade(names []string) (*Plan, error) {
	if r.Current == nil || len(r.Current.Packages) == 0 {
		return nil, fmt.Errorf("nothing is installed")
	}

	var targets []Installed
	if len(names) == 0 {
		targets = r.Current.Clone()
	} else {
		for _, n := range names {
			inst, ok := r.Current.Find(n)
			if !ok {
				return nil, &UnknownPackageError{Name: n, Suggest: r.Current.Names()}
			}
			targets = append(targets, *inst)
		}
	}

	// Anything upgradeable is re-requested, which also pulls in any newly
	// added dependency of the newer version.
	var reqs []request
	for _, t := range targets {
		rec, ok := r.Index.Lookup(t.Name)
		if !ok {
			continue // dropped from the index; leave it alone
		}
		if !VersionNewer(t.Version, rec.Version) {
			continue
		}
		reqs = append(reqs, request{name: t.Name, explicit: t.Explicit, reason: "upgrade"})
	}
	if len(reqs) == 0 {
		return &Plan{Final: r.Current.Clone()}, nil
	}

	sels, err := r.expand(reqs)
	if err != nil {
		return nil, err
	}

	plan := &Plan{}
	final := r.Current.Clone()
	upsert := func(inst Installed) {
		for i := range final {
			if strings.EqualFold(final[i].Name, inst.Name) {
				inst.Explicit = inst.Explicit || final[i].Explicit
				final[i] = inst
				return
			}
		}
		final = append(final, inst)
	}

	for _, s := range sels {
		cur, installed := r.Current.Find(s.recipe.Name)
		if installed && cur.Version == s.recipe.Version {
			continue
		}
		action, from := ActionInstall, ""
		if installed {
			action, from = ActionUpgrade, cur.Version
			if !VersionNewer(cur.Version, s.recipe.Version) {
				action = ActionDowngrade
			}
		}
		plan.Steps = append(plan.Steps, Step{
			Action: action, Name: s.recipe.Name, Version: s.recipe.Version, From: from,
			Recipe: s.recipe, Artifact: s.artifact, Platform: s.plat,
			Explicit: s.explicit, Reason: s.reason,
			Rosetta: r.Platform.IsRosettaFallback(s.plat),
		})
		upsert(Installed{
			Name: s.recipe.Name, Version: s.recipe.Version, Kind: s.recipe.Kind,
			SHA256: s.artifact.SHA256, SHA512: s.artifact.SHA512,
			Platform: s.plat, Source: s.artifact.URL, Deps: s.recipe.Deps,
			Explicit: s.explicit,
		})
	}
	plan.Final = final
	return plan, nil
}

// Outdated lists installed packages with a newer version in the index.
type Outdated struct {
	Name    string
	Have    string
	Want    string
	Pinned  bool
	Missing bool // no longer present in the index
}

// Outdated reports which installed packages have newer versions available.
func (r *Resolver) Outdated() []Outdated {
	var out []Outdated
	if r.Current == nil {
		return nil
	}
	for _, p := range r.Current.Packages {
		rec, ok := r.Index.Lookup(p.Name)
		if !ok {
			out = append(out, Outdated{Name: p.Name, Have: p.Version, Missing: true})
			continue
		}
		if VersionNewer(p.Version, rec.Version) {
			out = append(out, Outdated{Name: p.Name, Have: p.Version, Want: rec.Version})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// PlanSync builds a plan that makes the installed set match a hopfile exactly:
// missing packages are installed, version drift is corrected, and anything not
// in the file is removed. This is the reproducible-environment command.
func (r *Resolver) PlanSync(want map[string]string, prune bool) (*Plan, error) {
	// Validate constraints before touching anything.
	names := make([]string, 0, len(want))
	for n, c := range want {
		if !ValidConstraint(c) {
			return nil, fmt.Errorf("package %q has an unparseable version constraint %q", n, c)
		}
		names = append(names, n)
	}
	sort.Strings(names)

	var reqs []request
	for _, n := range names {
		reqs = append(reqs, request{name: n, explicit: true, reason: "hopfile"})
	}
	sels, err := r.expand(reqs)
	if err != nil {
		return nil, err
	}

	plan := &Plan{}
	var final []Installed

	wanted := map[string]bool{}
	for _, s := range sels {
		wanted[strings.ToLower(s.recipe.Name)] = true

		if c, ok := want[s.recipe.Name]; ok && !VersionSatisfies(c, s.recipe.Version) {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"hopfile asks for %s %s but the index only offers %s",
				s.recipe.Name, c, s.recipe.Version))
		}

		cur, installed := r.Current.Find(s.recipe.Name)
		if installed && cur.Version == s.recipe.Version {
			keep := *cur
			keep.Explicit = keep.Explicit || s.explicit
			final = append(final, keep)
			continue
		}
		action, from := ActionInstall, ""
		if installed {
			from = cur.Version
			action = ActionUpgrade
			if !VersionNewer(cur.Version, s.recipe.Version) {
				action = ActionDowngrade
			}
		}
		plan.Steps = append(plan.Steps, Step{
			Action: action, Name: s.recipe.Name, Version: s.recipe.Version, From: from,
			Recipe: s.recipe, Artifact: s.artifact, Platform: s.plat,
			Explicit: s.explicit, Reason: s.reason,
			Rosetta: r.Platform.IsRosettaFallback(s.plat),
		})
		final = append(final, Installed{
			Name: s.recipe.Name, Version: s.recipe.Version, Kind: s.recipe.Kind,
			SHA256: s.artifact.SHA256, SHA512: s.artifact.SHA512,
			Platform: s.plat, Source: s.artifact.URL, Deps: s.recipe.Deps,
			Explicit: s.explicit,
		})
	}

	if prune && r.Current != nil {
		for _, p := range r.Current.Packages {
			if wanted[strings.ToLower(p.Name)] {
				continue
			}
			plan.Steps = append(plan.Steps, Step{
				Action: ActionRemove, Name: p.Name, From: p.Version, Version: p.Version,
				Reason: "not in hopfile",
			})
		}
	} else if r.Current != nil {
		// Keep unmanaged packages as they are.
		for _, p := range r.Current.Packages {
			if !wanted[strings.ToLower(p.Name)] {
				final = append(final, p)
			}
		}
	}

	plan.Final = final
	return plan, nil
}
