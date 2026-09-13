package cmds

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/samuelbanapour/hopcli/internal/core"
	"github.com/samuelbanapour/hopcli/internal/ui"
)

func init() {
	register(&Command{
		Name:    "list",
		Aliases: []string{"ls"},
		Group:   "Inspect",
		Usage:   "hop list [flags]",
		Short:   "List installed packages",
		Flags: []Flag{
			{Long: "explicit", Short: "e", Kind: 'b', Help: "only packages you asked for, not dependencies"},
		},
		Run: runList,
	})

	register(&Command{
		Name:    "search",
		Aliases: []string{"s", "find"},
		Group:   "Inspect",
		Usage:   "hop search [flags] [query]",
		Short:   "Search the recipe index",
		Long: `Search package names, keywords and descriptions.

Results are ranked: exact name matches first, then prefixes, then keywords and
descriptions. A query with a typo still finds its target.`,
		Flags: []Flag{
			{Long: "limit", Kind: 'i', Arg: "N", Help: "maximum results (default 20, 0 for all)"},
		},
		Run: runSearch,
	})

	register(&Command{
		Name:    "info",
		Aliases: []string{"show"},
		Group:   "Inspect",
		Usage:   "hop info <package>",
		Short:   "Show everything known about a package",
		Run:     runInfo,
	})

	register(&Command{
		Name:  "which",
		Group: "Inspect",
		Usage: "hop which <command>",
		Short: "Show which package provides a command",
		Run:   runWhich,
	})

	register(&Command{
		Name:  "outdated",
		Group: "Inspect",
		Usage: "hop outdated",
		Short: "List installed packages with newer versions available",
		Run:   runOutdated,
	})

	register(&Command{
		Name:  "deps",
		Group: "Inspect",
		Usage: "hop deps <package>",
		Short: "Show a package's dependency tree",
		Flags: []Flag{
			{Long: "all", Kind: 'b', Help: "include packages that depend on it too"},
		},
		Run: runDeps,
	})
}

func runList(a *App, args []string) error {
	if len(args) > 0 {
		return usagef("list takes no arguments (did you mean `hop info %s`?)", args[0])
	}
	gen, err := a.Current()
	if err != nil {
		return err
	}
	if gen == nil || len(gen.Packages) == 0 {
		if a.JSON {
			return a.emitJSON([]any{})
		}
		ui.Info("nothing installed yet")
		ui.Hint("try `hop search ripgrep`, then `hop install ripgrep`")
		return nil
	}

	pkgs := gen.Packages
	if a.Explicit {
		var filtered []core.Installed
		for _, p := range pkgs {
			if p.Explicit {
				filtered = append(filtered, p)
			}
		}
		pkgs = filtered
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].Name < pkgs[j].Name })

	if a.JSON {
		type row struct {
			Name     string `json:"name"`
			Version  string `json:"version"`
			Kind     string `json:"kind,omitempty"`
			Explicit bool   `json:"explicit"`
			Size     int64  `json:"size"`
			Platform string `json:"platform"`
			Store    string `json:"store_path"`
			Image    string `json:"image_path,omitempty"`
		}
		out := make([]row, 0, len(pkgs))
		for _, p := range pkgs {
			out = append(out, row{p.Name, p.Version, string(p.Kind), p.Explicit, p.Size, string(p.Platform), p.StorePath, p.ImagePath()})
		}
		return a.emitJSON(out)
	}

	t := ui.NewTable("PACKAGE", "VERSION", "SIZE", "").RightAlign(2)
	var total int64
	for _, p := range pkgs {
		total += p.Size
		note := ""
		if !p.Explicit {
			note = ui.Grey("dependency")
		}
		t.Row(ui.Pkg(p.Name), p.Version, ui.Bytes(p.Size), note)
	}
	t.Render()
	ui.Blank()
	ui.Line("%s  %s  %s",
		ui.Count(len(pkgs), "package", "packages"),
		ui.Grey("·"),
		ui.Grey(ui.Bytes(total)+" on disk"))
	ui.Line("%s", ui.Grey(fmt.Sprintf("generation %d  ·  %s", gen.ID, ui.Ago(gen.Created))))
	return nil
}

func runSearch(a *App, args []string) error {
	ix, err := a.Index_()
	if err != nil {
		return err
	}
	query := strings.Join(args, " ")
	matches := ix.Search(query)

	limit := a.Limit
	if limit == 0 && !a.JSON {
		limit = 20
	}
	truncated := 0
	if limit > 0 && len(matches) > limit {
		truncated = len(matches) - limit
		matches = matches[:limit]
	}

	gen, _ := a.Current()

	if a.JSON {
		type row struct {
			Name        string   `json:"name"`
			Version     string   `json:"version"`
			Description string   `json:"description,omitempty"`
			Installed   bool     `json:"installed"`
			Platforms   []string `json:"platforms"`
		}
		out := make([]row, 0, len(matches))
		for _, m := range matches {
			_, inst := gen.Find(m.Recipe.Name)
			out = append(out, row{m.Recipe.Name, m.Recipe.Version, m.Recipe.Description, inst, m.Recipe.Platforms()})
		}
		return a.emitJSON(out)
	}

	if len(matches) == 0 {
		ui.Info("no packages match %q", query)
		if sug := ix.Suggest(query, 3); len(sug) > 0 {
			ui.Hint("did you mean %s?", ui.List(quoteAll(sug), "or"))
		}
		ui.Hint("the index has %s; `hop search` with no query lists them all", ui.Count(ix.Len(), "package", "packages"))
		return nil
	}

	plat := core.CurrentPlatform()
	descWidth := ui.Width() - 40
	if descWidth < 20 {
		descWidth = 20
	}

	t := ui.NewTable("", "PACKAGE", "VERSION", "DESCRIPTION")
	for _, m := range matches {
		r := m.Recipe
		mark := " "
		if _, inst := gen.Find(r.Name); inst {
			mark = ui.Green("✓")
		} else if _, _, ok := r.Artifact(plat); !ok {
			mark = ui.Grey("·") // exists, but not for this machine
		}
		t.Row("  "+mark, ui.Pkg(r.Name), ui.Grey(r.Version),
			ui.Truncate(ui.TrimZeroWidth(r.Description), descWidth))
	}
	t.Render()
	ui.Blank()

	line := ui.Count(t.Len(), "match", "matches")
	if truncated > 0 {
		line += ui.Grey(fmt.Sprintf("  ·  %d more, use --limit 0 to see them", truncated))
	}
	ui.Line("%s", line)
	ui.Line("%s", ui.Grey("✓ installed   · no build for "+string(plat)))
	return nil
}

func runInfo(a *App, args []string) error {
	if len(args) != 1 {
		return usagef("info needs exactly one package name")
	}
	ix, err := a.Index_()
	if err != nil {
		return err
	}
	name, _ := splitNameVersion(args[0])
	r, ok := ix.Lookup(name)
	if !ok {
		return &core.UnknownPackageError{Name: name, Suggest: ix.Suggest(name, 3)}
	}

	gen, _ := a.Current()
	inst, installed := gen.Find(r.Name)
	plat := core.CurrentPlatform()
	art, artPlat, haveArt := r.Artifact(plat)

	if a.JSON {
		return a.emitJSON(map[string]any{
			"name": r.Name, "version": r.Version, "description": r.Description,
			"homepage": r.Homepage, "license": r.License, "keywords": r.Keywords,
			"aliases": r.Aliases, "deps": r.Deps, "platforms": r.Platforms(),
			"installed": installed, "installed_version": versionOf(inst),
			"artifact": art, "artifact_platform": string(artPlat),
		})
	}

	ui.Blank()
	ui.Line("%s %s", ui.Pkg(r.Name), ui.Bold(r.Version))
	if r.Description != "" {
		ui.Line("%s", ui.Indent(r.Description, 0))
	}
	ui.Blank()

	pairs := [][2]string{
		{"Homepage", r.Homepage},
		{"License", r.License},
	}
	if len(r.Aliases) > 0 {
		pairs = append(pairs, [2]string{"Also known as", strings.Join(r.Aliases, ", ")})
	}
	if len(r.Deps) > 0 {
		pairs = append(pairs, [2]string{"Depends on", strings.Join(r.Deps, ", ")})
	}
	if r.Kind == core.KindImage {
		pairs = append(pairs, [2]string{"Kind", ui.Yellow("image") + ui.Grey(" — verified and stored, never extracted or put on PATH")})
	} else if cmds := r.BinNames(plat); len(cmds) > 0 {
		pairs = append(pairs, [2]string{"Commands", strings.Join(cmds, ", ")})
	}
	pairs = append(pairs, [2]string{"Platforms", strings.Join(r.Platforms(), ", ")})

	switch {
	case installed:
		status := ui.Green("installed " + inst.Version)
		if core.VersionNewer(inst.Version, r.Version) {
			status = ui.Yellow(fmt.Sprintf("installed %s, %s available", inst.Version, r.Version))
		}
		pairs = append(pairs, [2]string{"Status", status})
		if img := inst.ImagePath(); img != "" {
			pairs = append(pairs, [2]string{"Image file", ui.Grey(img)})
		} else {
			pairs = append(pairs, [2]string{"Store path", ui.Grey(inst.StorePath)})
		}
		pairs = append(pairs, [2]string{"Installed", ui.Ago(inst.At)})
	case !haveArt:
		pairs = append(pairs, [2]string{"Status", ui.Red("no build for " + plat.Pretty())})
	default:
		pairs = append(pairs, [2]string{"Status", ui.Grey("not installed")})
	}

	if haveArt {
		if art.Size > 0 {
			pairs = append(pairs, [2]string{"Download", ui.Bytes(art.Size)})
		}
		switch {
		case art.SHA256 != "":
			pairs = append(pairs, [2]string{"SHA-256", ui.Grey(art.SHA256)})
		case art.SHA512 != "":
			pairs = append(pairs, [2]string{"SHA-512", ui.Grey(art.SHA512)})
		default:
			pairs = append(pairs, [2]string{"Checksum", ui.Yellow("not pinned (trust on first use)")})
		}
		pairs = append(pairs, [2]string{"Artifact", ui.Grey(art.URL)})
		if plat.IsRosettaFallback(artPlat) {
			if r.Kind == core.KindImage {
				pairs = append(pairs, [2]string{"Note", ui.Yellow("x86_64 image; that's fine for a static image, unlike a CLI tool")})
			} else {
				pairs = append(pairs, [2]string{"Note", ui.Yellow("x86_64 build; runs under Rosetta 2")})
			}
		}
	}
	ui.KV(pairs...)

	if r.Caveats != "" {
		ui.Blank()
		ui.Step("Caveats")
		ui.Line("%s", ui.Indent(r.Caveats, 4))
	}
	ui.Blank()
	if !installed && haveArt {
		ui.Line("%s", ui.Grey("install with:  ")+ui.Cyan("hop install "+r.Name))
	}
	return nil
}

func versionOf(i *core.Installed) string {
	if i == nil {
		return ""
	}
	return i.Version
}

func runWhich(a *App, args []string) error {
	if len(args) != 1 {
		return usagef("which needs exactly one command name")
	}
	cmd := args[0]

	inst, ok := core.Owner(a.Layout, cmd)
	if !ok {
		if a.JSON {
			return a.emitJSON(map[string]any{"command": cmd, "found": false})
		}
		ui.Info("no installed package provides %s", ui.Bold(cmd))
		// Point at the system copy, if there is one, rather than dead-ending.
		if p, err := lookPathOutsideHop(a, cmd); err == nil {
			ui.Hint("your PATH resolves %s to %s (not managed by hop)", cmd, p)
		}
		// The argument might name an installed image, not a command — those
		// are never on PATH by design, so "which" alone would just confuse.
		if gen, err := a.Current(); err == nil {
			if p, ok := gen.Find(cmd); ok && p.Kind == core.KindImage {
				ui.Hint("%s is installed as an image, not a command; see `hop info %s`", p.Name, p.Name)
				return nil
			}
		}
		ix, ierr := a.Index_()
		if ierr == nil {
			if r, found := ix.Lookup(cmd); found {
				ui.Hint("install it with `hop install %s`", r.Name)
			}
		}
		return nil
	}

	link := filepath.Join(a.Layout.CurrentBin(), cmd)
	target, _ := filepath.EvalSymlinks(link)

	if a.JSON {
		return a.emitJSON(map[string]any{
			"command": cmd, "found": true, "package": inst.Name,
			"version": inst.Version, "link": link, "target": target,
			"store_path": inst.StorePath,
		})
	}

	ui.Line("%s %s", ui.Pkg(inst.Name), ui.Bold(inst.Version))
	ui.KV(
		[2]string{"Command", link},
		[2]string{"Resolves to", ui.Grey(target)},
		[2]string{"Store path", ui.Grey(inst.StorePath)},
	)
	if !onPath(a.Layout) {
		ui.Blank()
		ui.Warn("hop's bin directory is not on your PATH, so this command is not yet runnable")
		ui.Hint("run `eval \"$(hop shellenv)\"`")
	}
	return nil
}

// lookPathOutsideHop finds a command on PATH, ignoring hop's own directory,
// so `hop which` can explain what the user is currently running.
func lookPathOutsideHop(a *App, cmd string) (string, error) {
	hopBin := filepath.Clean(a.Layout.CurrentBin())
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || filepath.Clean(dir) == hopBin {
			continue
		}
		p := filepath.Join(dir, cmd)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

func runOutdated(a *App, args []string) error {
	if len(args) > 0 {
		return usagef("outdated takes no arguments")
	}
	ix, err := a.Index_()
	if err != nil {
		return err
	}
	gen, err := a.Current()
	if err != nil {
		return err
	}
	if gen == nil || len(gen.Packages) == 0 {
		if a.JSON {
			return a.emitJSON([]any{})
		}
		ui.Info("nothing installed yet")
		return nil
	}

	old := core.NewResolver(ix, gen, core.CurrentPlatform()).Outdated()

	if a.JSON {
		return a.emitJSON(old)
	}
	if len(old) == 0 {
		ui.Ok("everything is up to date")
		return nil
	}

	t := ui.NewTable("PACKAGE", "INSTALLED", "", "AVAILABLE")
	n := 0
	for _, o := range old {
		if o.Missing {
			t.Row(ui.Pkg(o.Name), o.Have, "", ui.Grey("no longer in the index"))
			continue
		}
		n++
		t.Row(ui.Pkg(o.Name), ui.Grey(o.Have), ui.Arrow(), ui.Bold(o.Want))
	}
	t.Render()
	ui.Blank()
	if n > 0 {
		ui.Line("%s outdated", ui.Count(n, "package", "packages"))
		ui.Line("%s", ui.Grey("upgrade everything with:  ")+ui.Cyan("hop upgrade"))
	}
	return nil
}

func runDeps(a *App, args []string) error {
	if len(args) != 1 {
		return usagef("deps needs exactly one package name")
	}
	ix, err := a.Index_()
	if err != nil {
		return err
	}
	r, ok := ix.Lookup(args[0])
	if !ok {
		return &core.UnknownPackageError{Name: args[0], Suggest: ix.Suggest(args[0], 3)}
	}

	if a.JSON {
		return a.emitJSON(map[string]any{
			"name": r.Name, "deps": r.Deps, "dependents": dependentsOf(ix, r.Name),
		})
	}

	ui.Blank()
	ui.Line("%s %s", ui.Pkg(r.Name), ui.Grey(r.Version))
	if len(r.Deps) == 0 {
		ui.Line("  %s", ui.Grey("no dependencies"))
	} else {
		printDepTree(ix, r, "  ", map[string]bool{}, 0)
	}

	if a.All {
		dep := dependentsOf(ix, r.Name)
		ui.Blank()
		if len(dep) == 0 {
			ui.Line("%s", ui.Grey("nothing in the index depends on it"))
		} else {
			ui.Line("%s", ui.Bold("Depended on by"))
			for _, d := range dep {
				ui.Line("  %s %s", ui.Grey(ui.Arrow()), ui.Pkg(d))
			}
		}
	}
	return nil
}

// printDepTree renders a dependency tree, guarding against cycles and
// unbounded depth so a malformed index cannot hang the command.
func printDepTree(ix *core.Index, r *core.Recipe, indent string, seen map[string]bool, depth int) {
	if depth > 16 {
		ui.Line("%s%s", indent, ui.Yellow("… tree is deeper than 16 levels"))
		return
	}
	for i, d := range r.Deps {
		last := i == len(r.Deps)-1
		branch := "├─ "
		nextIndent := indent + "│  "
		if last {
			branch = "└─ "
			nextIndent = indent + "   "
		}

		dr, ok := ix.Lookup(d)
		if !ok {
			ui.Line("%s%s%s %s", indent, ui.Grey(branch), ui.Pkg(d), ui.Red("(not in the index)"))
			continue
		}
		if seen[strings.ToLower(d)] {
			ui.Line("%s%s%s %s", indent, ui.Grey(branch), ui.Pkg(dr.Name), ui.Grey("(already shown)"))
			continue
		}
		seen[strings.ToLower(d)] = true
		ui.Line("%s%s%s %s", indent, ui.Grey(branch), ui.Pkg(dr.Name), ui.Grey(dr.Version))
		printDepTree(ix, dr, nextIndent, seen, depth+1)
	}
}

func dependentsOf(ix *core.Index, name string) []string {
	var out []string
	for _, r := range ix.Recipes {
		for _, d := range r.Deps {
			if strings.EqualFold(d, name) {
				out = append(out, r.Name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
