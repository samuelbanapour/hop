package core

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// AliasVersions returns every version of name that hop has ever installed on
// this machine, newest first, drawn from generation history rather than the
// recipe index. This is what makes `hop alias` instant: the store already
// has the bytes, so there is nothing to fetch.
func AliasVersions(l *Layout, name string) ([]Installed, error) {
	gens, err := Generations(l)
	if err != nil {
		return nil, err
	}
	byVersion := map[string]Installed{}
	for _, g := range gens {
		if inst, ok := g.Find(name); ok {
			byVersion[inst.Version] = *inst
		}
	}
	out := make([]Installed, 0, len(byVersion))
	for _, inst := range byVersion {
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool { return CompareVersions(out[i].Version, out[j].Version) > 0 })
	return out, nil
}

// BuildAliasGeneration commits a new generation identical to the active one
// except that target's package is swapped in. It does not activate the
// result — callers should Verify it first, exactly like rollback does,
// since the swapped-to version's store path could have been reclaimed by gc.
func BuildAliasGeneration(l *Layout, target Installed) (*Generation, error) {
	cur, err := Current(l)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, fmt.Errorf("nothing is installed")
	}
	if _, ok := cur.Find(target.Name); !ok {
		return nil, fmt.Errorf("%s is not installed", target.Name)
	}

	pkgs := cur.Clone()
	for i := range pkgs {
		if strings.EqualFold(pkgs[i].Name, target.Name) {
			explicit := pkgs[i].Explicit
			pkgs[i] = target
			pkgs[i].Explicit = explicit
			pkgs[i].At = time.Now()
		}
	}
	return Commit(l, pkgs, fmt.Sprintf("hop alias %s %s", target.Name, target.Version))
}
