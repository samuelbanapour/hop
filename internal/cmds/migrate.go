package cmds

import (
	"encoding/json"
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
		Name:    "migrate",
		Aliases: []string{"from-brew"},
		Group:   "Projects",
		Usage:   "hop migrate [flags]",
		Short:   "Take over the packages you installed with Homebrew",
		Long: `Find what Homebrew has installed and reinstall it under hop.

hop reads Homebrew's Cellar and install receipts directly, so brew does not
have to be running and nothing about your Homebrew installation is touched.
Only the formulae you asked for are considered: brew's own internal library
dependencies are left out, because hop resolves its own.

Nothing is removed from Homebrew. Once you are happy, hop prints the exact
` + "`brew uninstall`" + ` commands to run yourself — it will not run them for
you, because that is your call, not a package manager's.

Casks (GUI applications) are counted but not migrated automatically — hop
can install and manage GUI apps (see ` + "`hop install`" + `), but migrate
does not yet cross Homebrew's cask names over to hop's own recipes.`,
		Flags: []Flag{
			{Long: "all", Kind: 'b', Help: "include brew's auto-installed dependencies too"},
			{Long: "write-hopfile", Kind: 'b', Help: "also record the migrated set in a hopfile"},
			{Long: "prefix", Kind: 's', Arg: "DIR", Help: "Homebrew prefix (default: auto-detect)"},
		},
		Run: runMigrate,
	})
}

// migrateRow pairs a Homebrew formula with the hop recipe that can replace it.
type migrateRow struct {
	f       brewFormula
	recipe  *core.Recipe
	already bool
}

// brewFormula is one thing Homebrew has installed.
type brewFormula struct {
	Name      string
	Version   string
	OnRequest bool // the user asked for it, rather than brew pulling it in
	Path      string
}

// brewPrefixes are the standard Homebrew locations: Apple Silicon, Intel, and
// the usual Linuxbrew path.
var brewPrefixes = []string{
	"/opt/homebrew",
	"/usr/local",
	"/home/linuxbrew/.linuxbrew",
}

// findBrewPrefix locates a Homebrew installation. HOMEBREW_PREFIX wins, then
// the standard locations; a directory only counts if it has a Cellar.
func findBrewPrefix(override string) (string, bool) {
	candidates := brewPrefixes
	if override != "" {
		candidates = []string{override}
	} else if env := os.Getenv("HOMEBREW_PREFIX"); env != "" {
		candidates = append([]string{env}, candidates...)
	}
	for _, p := range candidates {
		if fi, err := os.Stat(filepath.Join(p, "Cellar")); err == nil && fi.IsDir() {
			return p, true
		}
	}
	return "", false
}

// readBrewCellar lists installed formulae by walking the Cellar. This reads
// only Homebrew's own on-disk layout, so it works whether or not brew itself
// is functional, and never shells out.
func readBrewCellar(prefix string) ([]brewFormula, error) {
	cellar := filepath.Join(prefix, "Cellar")
	entries, err := os.ReadDir(cellar)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", cellar, err)
	}

	var out []brewFormula
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		// <Cellar>/<name>/<version>/ — take the newest version present.
		versions, err := os.ReadDir(filepath.Join(cellar, e.Name()))
		if err != nil {
			continue
		}
		best := ""
		for _, v := range versions {
			if !v.IsDir() || strings.HasPrefix(v.Name(), ".") {
				continue
			}
			if best == "" || core.CompareVersions(v.Name(), best) > 0 {
				best = v.Name()
			}
		}
		if best == "" {
			continue
		}
		kegPath := filepath.Join(cellar, e.Name(), best)
		out = append(out, brewFormula{
			Name:      e.Name(),
			Version:   best,
			OnRequest: installedOnRequest(kegPath),
			Path:      kegPath,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// installedOnRequest reads Homebrew's install receipt to tell a package the
// user asked for from one brew pulled in as a dependency. A missing or
// unreadable receipt is treated as "on request", since that is the safer
// assumption: better to offer a package than to silently drop it.
func installedOnRequest(kegPath string) bool {
	b, err := os.ReadFile(filepath.Join(kegPath, "INSTALL_RECEIPT.json"))
	if err != nil {
		return true
	}
	var receipt struct {
		InstalledOnRequest *bool `json:"installed_on_request"`
	}
	if err := json.Unmarshal(b, &receipt); err != nil || receipt.InstalledOnRequest == nil {
		return true
	}
	return *receipt.InstalledOnRequest
}

// countBrewCasks reports how many GUI applications brew manages, so the
// report can say plainly that hop is not taking those over.
func countBrewCasks(prefix string) int {
	entries, err := os.ReadDir(filepath.Join(prefix, "Caskroom"))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			n++
		}
	}
	return n
}

func runMigrate(a *App, args []string) error {
	if len(args) > 0 {
		return usagef("migrate takes no arguments")
	}

	prefix, ok := findBrewPrefix(a.strFlag("prefix"))
	if !ok {
		ui.Info("no Homebrew installation found")
		ui.Blank()
		ui.Line("  hop looked in:")
		for _, p := range brewPrefixes {
			ui.Line("    %s", ui.Grey(p))
		}
		ui.Blank()
		ui.Hint("point hop at it with `hop migrate --prefix /path/to/homebrew`")
		return nil
	}

	sp := ui.StartSpinner("Reading Homebrew at %s", prefix)
	formulae, err := readBrewCellar(prefix)
	casks := countBrewCasks(prefix)
	sp.Stop()
	if err != nil {
		return err
	}
	if len(formulae) == 0 {
		ui.Info("Homebrew is installed at %s but has no formulae", prefix)
		return nil
	}

	ix, err := a.Index_()
	if err != nil {
		return err
	}
	gen, err := a.Current()
	if err != nil {
		return err
	}

	// Classify every formula.
	var migratable, alreadyHave, unavailable, skippedDeps []migrateRow

	for _, f := range formulae {
		if !f.OnRequest && !a.All {
			skippedDeps = append(skippedDeps, migrateRow{f: f})
			continue
		}
		r, found := ix.Lookup(f.Name)
		if !found {
			unavailable = append(unavailable, migrateRow{f: f})
			continue
		}
		if _, have := gen.Find(r.Name); have {
			alreadyHave = append(alreadyHave, migrateRow{f, r, true})
			continue
		}
		if _, _, ok := r.Artifact(core.CurrentPlatform()); !ok {
			unavailable = append(unavailable, migrateRow{f: f, recipe: r})
			continue
		}
		migratable = append(migratable, migrateRow{f, r, false})
	}

	if a.JSON {
		names := func(rs []migrateRow) []string {
			out := make([]string, 0, len(rs))
			for _, r := range rs {
				out = append(out, r.f.Name)
			}
			return out
		}
		return a.emitJSON(map[string]any{
			"brew_prefix":     prefix,
			"formulae":        len(formulae),
			"casks":           casks,
			"migratable":      names(migratable),
			"already_managed": names(alreadyHave),
			"unavailable":     names(unavailable),
			"brew_deps":       len(skippedDeps),
		})
	}

	// ---- report -------------------------------------------------------------
	ui.Blank()
	ui.Step("Homebrew at %s", prefix)
	ui.Blank()
	ui.Line("  %s installed  %s  %d you asked for yourself",
		ui.Count(len(formulae), "formula", "formulae"), ui.Grey("·"),
		len(formulae)-len(skippedDeps))
	if casks > 0 {
		ui.Line("  %s %s", ui.Count(casks, "cask", "casks"),
			ui.Grey("(GUI apps — hop can install apps too, but migrate doesn't cross Homebrew's cask names over yet; reinstall these with `hop install` if hop has a recipe for them)"))
	}
	ui.Blank()

	if len(migratable) > 0 {
		ui.Line("%s", ui.Bold("hop can take these over"))
		t := ui.NewTable()
		for _, r := range migratable {
			note := ""
			if !strings.EqualFold(r.f.Name, r.recipe.Name) {
				note = ui.Grey("brew calls it " + r.f.Name)
			}
			ver := r.recipe.Version
			if core.VersionNewer(r.f.Version, r.recipe.Version) {
				ver = fmt.Sprintf("%s %s %s", ui.Grey(r.f.Version), ui.Arrow(), ui.Bold(r.recipe.Version))
			} else {
				ver = ui.Bold(ver)
			}
			t.Row("  "+ui.Green("+"), ui.Pkg(r.recipe.Name), ver, note)
		}
		t.Render()
		ui.Blank()
	}

	if len(alreadyHave) > 0 {
		names := make([]string, 0, len(alreadyHave))
		for _, r := range alreadyHave {
			names = append(names, r.recipe.Name)
		}
		ui.Line("%s %s", ui.Green("✓"), ui.Grey(fmt.Sprintf("already managed by hop: %s", strings.Join(names, ", "))))
		ui.Blank()
	}

	if len(unavailable) > 0 {
		var names []string
		for _, r := range unavailable {
			names = append(names, r.f.Name)
		}
		sort.Strings(names)
		ui.Line("%s %s", ui.Bold("no hop recipe yet"),
			ui.Grey(fmt.Sprintf("(%d)", len(names))))
		ui.Line("  %s", ui.Grey(ui.Truncate(strings.Join(names, ", "), ui.Width()-4)))
		ui.Line("  %s", ui.Grey("these stay with brew; hop and brew coexist fine."))
		ui.Blank()
	}

	if len(skippedDeps) > 0 && !a.All {
		ui.Line("%s", ui.Grey(fmt.Sprintf(
			"%s were pulled in by brew as dependencies and are not migrated (--all includes them)",
			ui.Count(len(skippedDeps), "formula", "formulae"))))
		ui.Blank()
	}

	if len(migratable) == 0 {
		if len(alreadyHave) > 0 {
			ui.Ok("everything hop can manage is already managed by hop")
		} else {
			ui.Info("nothing to migrate: hop has no recipe for any of your formulae yet")
			ui.Hint("hop's index covers %s; `hop search` lists them", ui.Count(ix.Len(), "tool", "tools"))
		}
		return nil
	}

	// ---- install ------------------------------------------------------------
	names := make([]string, 0, len(migratable))
	for _, r := range migratable {
		names = append(names, r.recipe.Name)
	}

	cur, err := a.Current()
	if err != nil {
		return err
	}
	plan, err := core.NewResolver(ix, cur, core.CurrentPlatform()).PlanInstall(names, false)
	if err != nil {
		return err
	}

	renderPlan(plan, "hop will")
	if a.DryRun {
		ui.Info("dry run: nothing was changed")
		printBrewCleanup(migratableNames(migratable), prefix)
		return nil
	}
	if !a.confirm("Install these under hop?") {
		ui.Info("cancelled")
		return nil
	}

	if err := execute(a, plan, cur, fmt.Sprintf("Migrating %s", ui.Count(len(plan.Downloads()), "package", "packages"))); err != nil {
		return err
	}

	if a.boolFlag("write-hopfile") {
		cwd, _ := os.Getwd()
		path := filepath.Join(cwd, core.HopfileName)
		pkgs := map[string]string{}
		for _, n := range names {
			pkgs[n] = "*"
		}
		if _, err := os.Stat(path); err == nil {
			for _, n := range names {
				_ = core.UpsertPackage(path, n, "*")
			}
		} else if err := core.WriteHopfile(path, pkgs); err != nil {
			ui.Warn("could not write %s: %v", core.HopfileName, err)
		}
		ui.Line("%s", ui.Grey("recorded the migrated set in "+core.HopfileName))
	}

	printBrewCleanup(migratableNames(migratable), prefix)
	return nil
}

func migratableNames(rows []migrateRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.f.Name)
	}
	sort.Strings(out)
	return out
}

// printBrewCleanup shows the commands to remove the now-duplicated formulae.
// hop deliberately does not run them: removing software the user installed
// with another tool is their decision to make, not hop's.
func printBrewCleanup(brewNames []string, prefix string) {
	if len(brewNames) == 0 {
		return
	}
	ui.Blank()
	ui.Step("Next: retire the Homebrew copies")
	ui.Blank()
	ui.Line("  Both copies are installed right now. Which one wins depends on")
	ui.Line("  PATH order — check with %s.", ui.Cyan("hop doctor"))
	ui.Blank()
	ui.Line("  When you are satisfied hop's versions work, remove brew's:")
	ui.Blank()
	ui.Line("    %s", ui.Cyan("brew uninstall "+strings.Join(brewNames, " ")))
	ui.Blank()
	ui.Line("  %s", ui.Grey("hop will not run that for you — removing software you installed"))
	ui.Line("  %s", ui.Grey("with another tool is your call."))
}
