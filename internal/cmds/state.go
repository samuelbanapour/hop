package cmds

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/samuelbanapour/hopcli/internal/core"
	"github.com/samuelbanapour/hopcli/internal/ui"
)

func init() {
	register(&Command{
		Name:    "generations",
		Aliases: []string{"gens", "history"},
		Group:   "History",
		Usage:   "hop generations",
		Short:   "List every generation of the installed set",
		Long: `Show the history of your installed set.

Each install, upgrade or removal creates a numbered generation. Old
generations remain on disk, so any of them can be restored instantly with
` + "`hop rollback <n>`" + `.`,
		Run: runGenerations,
	})

	register(&Command{
		Name:    "rollback",
		Aliases: []string{"undo"},
		Group:   "History",
		Usage:   "hop rollback [generation]",
		Short:   "Switch back to a previous generation",
		Long: `Restore a previous generation of the installed set.

With no argument, rollback goes to the generation before the active one.
Because every version is still in the store, this is a single symlink swap:
it takes microseconds and downloads nothing, however large the change.

Rolling back is itself just an activation, so ` + "`hop rollback`" + ` again
returns you to where you were.`,
		Run: runRollback,
	})

	register(&Command{
		Name:  "gc",
		Group: "Maintenance",
		Usage: "hop gc [flags]",
		Short: "Reclaim disk from unreferenced store paths",
		Long: `Delete store paths and cached downloads that no generation needs.

By default every generation is kept, so only genuinely unreachable data is
removed. Use --keep N to also discard all but the newest N generations, which
is what actually frees space after a long history of upgrades.

The active generation is never deleted.`,
		Flags: []Flag{
			{Long: "keep", Kind: 'i', Arg: "N", Help: "keep only the newest N generations"},
		},
		Run: runGC,
	})

	register(&Command{
		Name:  "doctor",
		Group: "Maintenance",
		Usage: "hop doctor",
		Short: "Check the installation for problems",
		Run:   runDoctor,
	})

	register(&Command{
		Name:  "verify",
		Group: "Maintenance",
		Usage: "hop verify",
		Short: "Check that installed packages are intact on disk",
		Run:   runVerify,
	})

	register(&Command{
		Name:  "update",
		Group: "Maintenance",
		Usage: "hop update",
		Short: "Refresh the recipe index",
		Long: `Refresh the recipe index from HOP_INDEX_URL.

This is one conditional HTTP request. There is no repository to clone and no
thousands of files to churn through, so it usually finishes in well under a
second and most often returns "not modified".

With no HOP_INDEX_URL set, hop uses the index built into the binary and this
command has nothing to do.`,
		Run: runUpdate,
	})
}

func runGenerations(a *App, args []string) error {
	if len(args) > 0 {
		return usagef("generations takes no arguments (did you mean `hop rollback %s`?)", args[0])
	}
	gens, err := core.Generations(a.Layout)
	if err != nil {
		return err
	}
	if len(gens) == 0 {
		if a.JSON {
			return a.emitJSON([]any{})
		}
		ui.Info("no generations yet")
		ui.Hint("the first `hop install` creates generation 1")
		return nil
	}
	cur := core.CurrentID(a.Layout)

	if a.JSON {
		type row struct {
			ID       int      `json:"id"`
			Active   bool     `json:"active"`
			Created  string   `json:"created"`
			Command  string   `json:"command,omitempty"`
			Packages int      `json:"packages"`
			Size     int64    `json:"size"`
			Names    []string `json:"names"`
		}
		out := make([]row, 0, len(gens))
		for _, g := range gens {
			out = append(out, row{g.ID, g.ID == cur, g.Created.Format("2006-01-02T15:04:05Z07:00"),
				g.Command, len(g.Packages), g.Size(), g.Names()})
		}
		return a.emitJSON(out)
	}

	// Newest first: that is what people scan for when rolling back.
	sort.Slice(gens, func(i, j int) bool { return gens[i].ID > gens[j].ID })

	t := ui.NewTable("", "GEN", "WHEN", "PACKAGES", "COMMAND").RightAlign(1, 3)
	for _, g := range gens {
		mark := " "
		if g.ID == cur {
			mark = ui.Green("●")
		}
		cmd := g.Command
		cmd = strings.TrimPrefix(cmd, "hop ")
		t.Row("  "+mark, ui.Bold(strconv.Itoa(g.ID)), ui.Grey(ui.Ago(g.Created)),
			strconv.Itoa(len(g.Packages)), ui.Grey(ui.Truncate(cmd, 40)))
	}
	t.Render()
	ui.Blank()
	ui.Line("%s  %s", ui.Count(len(gens), "generation", "generations"), ui.Grey("· ● is active"))
	if len(gens) > 1 {
		prev := 0
		for _, g := range gens {
			if g.ID < cur && g.ID > prev {
				prev = g.ID
			}
		}
		if prev > 0 {
			ui.Line("%s", ui.Grey("go back with:  ")+ui.Cyan("hop rollback"))
		}
	}
	return nil
}

func runRollback(a *App, args []string) error {
	if len(args) > 1 {
		return usagef("rollback takes at most one generation number")
	}
	gens, err := core.Generations(a.Layout)
	if err != nil {
		return err
	}
	if len(gens) == 0 {
		return fmt.Errorf("there are no generations to roll back to")
	}
	cur := core.CurrentID(a.Layout)

	target := 0
	if len(args) == 1 {
		n, err := strconv.Atoi(args[0])
		if err != nil {
			return usagef("%q is not a generation number; see `hop generations`", args[0])
		}
		target = n
	} else {
		// Default: the newest generation older than the active one.
		for _, g := range gens {
			if g.ID < cur && g.ID > target {
				target = g.ID
			}
		}
		if target == 0 {
			return fmt.Errorf("generation %d is the oldest; there is nothing earlier to roll back to", cur)
		}
	}

	if target == cur {
		ui.Info("generation %d is already active", target)
		return nil
	}

	var want *core.Generation
	for _, g := range gens {
		if g.ID == target {
			want = g
		}
	}
	if want == nil {
		available := make([]string, 0, len(gens))
		for _, g := range gens {
			available = append(available, strconv.Itoa(g.ID))
		}
		return fmt.Errorf("no generation %d (available: %s)", target, strings.Join(available, ", "))
	}

	// Rolling back to a generation whose store paths were collected would
	// produce dangling links; refuse rather than break the environment.
	if issues := core.Verify(a.Layout, want); len(issues) > 0 {
		ui.Err("generation %d is no longer complete on disk:", target)
		for _, is := range issues[:minInt(len(issues), 6)] {
			ui.Line("    %s  %s", ui.Pkg(is.Name), is.Problem)
		}
		ui.Blank()
		ui.Hint("a previous `hop gc --keep` removed data this generation needed")
		ui.Hint("reinstall instead: `hop install %s`", strings.Join(want.Names(), " "))
		return fmt.Errorf("refusing to activate an incomplete generation")
	}

	if a.DryRun {
		ui.Info("dry run: would activate generation %d (%s)", target, ui.Count(len(want.Packages), "package", "packages"))
		return nil
	}

	guard, err := a.lock()
	if err != nil {
		return err
	}
	defer guard.Release()

	if err := core.Activate(a.Layout, target); err != nil {
		return err
	}

	if a.JSON {
		return a.emitJSON(map[string]any{
			"from": cur, "to": target, "packages": want.Names(),
		})
	}

	ui.Ok("rolled back to generation %d %s", target, ui.Grey("(from "+strconv.Itoa(cur)+")"))
	ui.Blank()
	diffGenerations(a, cur, target)
	return nil
}

// diffGenerations prints what changed between two generations, so the user can
// see what a rollback actually did to their tools.
func diffGenerations(a *App, fromID, toID int) {
	from, err1 := core.LoadGeneration(a.Layout, fromID)
	to, err2 := core.LoadGeneration(a.Layout, toID)
	if err1 != nil || err2 != nil {
		return
	}

	fromMap := map[string]string{}
	for _, p := range from.Packages {
		fromMap[p.Name] = p.Version
	}
	toMap := map[string]string{}
	for _, p := range to.Packages {
		toMap[p.Name] = p.Version
	}

	var lines []string
	for name, v := range toMap {
		old, had := fromMap[name]
		switch {
		case !had:
			lines = append(lines, fmt.Sprintf("  %s %s %s", ui.Green("+"), ui.Pkg(name), ui.Grey(v)))
		case old != v:
			lines = append(lines, fmt.Sprintf("  %s %s %s %s %s", ui.Cyan("~"), ui.Pkg(name), ui.Grey(old), ui.Arrow(), ui.Bold(v)))
		}
	}
	for name, v := range fromMap {
		if _, still := toMap[name]; !still {
			lines = append(lines, fmt.Sprintf("  %s %s %s", ui.Red("−"), ui.Pkg(name), ui.Grey(v)))
		}
	}
	sort.Strings(lines)
	for _, l := range lines {
		ui.Line("%s", l)
	}
	if len(lines) == 0 {
		ui.Line("  %s", ui.Grey("the two generations contain the same packages"))
	}
}

func runGC(a *App, args []string) error {
	if len(args) > 0 {
		return usagef("gc takes no arguments")
	}
	plan, err := core.PlanGC(a.Layout, a.Keep)
	if err != nil {
		return err
	}

	if a.JSON {
		return a.emitJSON(map[string]any{
			"store_paths": plan.StorePaths, "generations": plan.Generations,
			"downloads": len(plan.Downloads), "bytes": plan.Bytes, "dry_run": a.DryRun,
		})
	}

	if plan.Empty() {
		ui.Ok("nothing to reclaim")
		stats, _ := core.Stats(a.Layout)
		if stats != nil {
			ui.Line("%s", ui.Grey(fmt.Sprintf("store holds %s across %s",
				ui.Bytes(stats.Bytes), ui.Count(stats.Paths, "path", "paths"))))
		}
		if a.Keep == 0 {
			gens, _ := core.Generations(a.Layout)
			if len(gens) > 1 {
				ui.Hint("every generation is still kept; `hop gc --keep 3` discards older history too")
			}
		}
		return nil
	}

	ui.Step("hop will reclaim %s", ui.Bytes(plan.Bytes))
	ui.Blank()
	if len(plan.Generations) > 0 {
		ids := make([]string, 0, len(plan.Generations))
		for _, n := range plan.Generations {
			ids = append(ids, strconv.Itoa(n))
		}
		ui.Line("  %s %s", ui.Red("drop"), fmt.Sprintf("%s %s",
			ui.Count(len(plan.Generations), "generation", "generations"), ui.Grey("("+strings.Join(ids, ", ")+")")))
	}
	for _, p := range plan.StorePaths {
		ui.Line("  %s %s", ui.Red("delete"), ui.Grey(filepath.Base(p)))
	}
	if n := len(plan.Downloads); n > 0 {
		ui.Line("  %s %s", ui.Red("delete"), ui.Grey(ui.Count(n, "cached download", "cached downloads")))
	}
	ui.Blank()

	if len(plan.Generations) > 0 {
		ui.Warn("dropping generations makes those rollbacks unavailable for good")
		ui.Blank()
	}

	if a.DryRun {
		ui.Info("dry run: nothing was deleted")
		return nil
	}
	if !a.confirm("Proceed?") {
		ui.Info("cancelled")
		return nil
	}

	guard, err := a.lock()
	if err != nil {
		return err
	}
	defer guard.Release()

	freed, err := core.RunGC(a.Layout, plan)
	if err != nil {
		ui.Warn("some items could not be removed: %v", err)
	}
	ui.Ok("reclaimed %s", ui.Bytes(freed))
	return nil
}

func runDoctor(a *App, args []string) error {
	if len(args) > 0 {
		return usagef("doctor takes no arguments")
	}

	type check struct {
		Name   string `json:"name"`
		Status string `json:"status"` // ok, warn, fail
		Detail string `json:"detail"`
		Fix    string `json:"fix,omitempty"`
	}
	var checks []check
	add := func(name, status, detail, fix string) {
		checks = append(checks, check{name, status, detail, fix})
	}

	// --- prefix and permissions ---
	if fi, err := os.Stat(a.Layout.Root); err != nil {
		add("prefix", "fail", "cannot stat "+a.Layout.Root+": "+err.Error(), "")
	} else if !fi.IsDir() {
		add("prefix", "fail", a.Layout.Root+" is not a directory", "")
	} else {
		probe := filepath.Join(a.Layout.Root, ".hop-write-probe")
		if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
			add("prefix", "fail", a.Layout.Root+" is not writable", "check ownership of "+a.Layout.Root)
		} else {
			os.Remove(probe)
			add("prefix", "ok", a.Layout.Root, "")
		}
	}

	// --- PATH ---
	if onPath(a.Layout) {
		add("PATH", "ok", a.Layout.CurrentBin()+" is on PATH", "")
	} else {
		add("PATH", "warn", a.Layout.CurrentBin()+" is not on PATH",
			`add: eval "$(hop shellenv)"`)
	}

	// --- active generation ---
	gen, err := a.Current()
	switch {
	case err != nil:
		add("generation", "fail", "cannot read the active generation: "+err.Error(), "")
	case gen == nil:
		add("generation", "ok", "nothing installed yet", "")
	default:
		add("generation", "ok", fmt.Sprintf("generation %d with %s", gen.ID,
			ui.Count(len(gen.Packages), "package", "packages")), "")
	}

	// --- integrity ---
	if gen != nil {
		if issues := core.Verify(a.Layout, gen); len(issues) > 0 {
			add("integrity", "fail", fmt.Sprintf("%s in the active generation",
				ui.Count(len(issues), "problem", "problems")), "run `hop verify` for detail")
		} else {
			add("integrity", "ok", "every installed package is intact", "")
		}
	}

	// --- index ---
	ix, ierr := a.Index_()
	switch {
	case ierr != nil:
		add("index", "fail", ierr.Error(), "")
	case ix.IsBuiltin():
		add("index", "ok", fmt.Sprintf("built-in index, %s", ui.Count(ix.Len(), "recipe", "recipes")),
			"set HOP_INDEX_URL for a live index")
	default:
		add("index", "ok", fmt.Sprintf("%s, %s, fetched %s",
			ix.Source, ui.Count(ix.Len(), "recipe", "recipes"), ui.Ago(ix.Age())), "")
	}

	// --- disk ---
	stats, _ := core.Stats(a.Layout)
	if stats != nil {
		status, fix := "ok", ""
		// A package manager on a nearly-full disk fails in confusing ways, so
		// say so plainly before the user hits ENOSPC mid-install.
		switch {
		case stats.Free > 0 && stats.Free < 200<<20:
			status, fix = "fail", "free space before installing anything"
		case stats.Free > 0 && stats.Free < 2<<30:
			status, fix = "warn", "consider `hop gc`"
		}
		add("disk", status, fmt.Sprintf("%s free · store %s · cache %s",
			ui.Bytes(stats.Free), ui.Bytes(stats.Bytes), ui.Bytes(stats.CacheBytes)), fix)
	}

	// --- symlink support, which exFAT-style volumes lack ---
	probe := filepath.Join(a.Layout.Root, ".hop-symlink-probe")
	os.Remove(probe)
	if err := os.Symlink(a.Layout.Store(), probe); err != nil {
		add("symlinks", "fail", "this filesystem cannot create symlinks: "+err.Error(),
			"move HOP_ROOT to a filesystem that supports symlinks")
	} else {
		os.Remove(probe)
		add("symlinks", "ok", "supported", "")
	}

	// --- shadowed commands ---
	if gen != nil {
		var shadowed []string
		for _, p := range gen.Packages {
			for _, b := range p.Bins {
				if other, err := lookPathOutsideHop(a, b.Name); err == nil {
					if onPath(a.Layout) && !earlierOnPath(a, a.Layout.CurrentBin(), filepath.Dir(other)) {
						shadowed = append(shadowed, b.Name+" ("+other+")")
					}
				}
			}
		}
		sort.Strings(shadowed)
		if len(shadowed) > 0 {
			add("shadowing", "warn",
				fmt.Sprintf("%s also exist earlier on PATH: %s",
					ui.Count(len(shadowed), "command", "commands"),
					strings.Join(shadowed[:minInt(len(shadowed), 4)], ", ")),
				"put hop's bin directory earlier in PATH")
		} else {
			add("shadowing", "ok", "no installed command is shadowed", "")
		}
	}

	if a.JSON {
		return a.emitJSON(checks)
	}

	ui.Blank()
	ui.Line("%s %s", ui.Bold("hop"), ui.Grey(core.Version+" · "+string(core.CurrentPlatform())))
	ui.Blank()

	fails, warns := 0, 0
	for _, c := range checks {
		var mark string
		switch c.Status {
		case "ok":
			mark = ui.Green("✓")
		case "warn":
			mark = ui.Yellow("!")
			warns++
		default:
			mark = ui.Red("✗")
			fails++
		}
		ui.Line("  %s %-12s %s", mark, c.Name, c.Detail)
		if c.Fix != "" && c.Status != "ok" {
			ui.Line("    %s %s", ui.Grey("→"), ui.Cyan(c.Fix))
		}
	}
	ui.Blank()
	switch {
	case fails > 0:
		ui.Err("%s need attention", ui.Count(fails, "check", "checks"))
		return fmt.Errorf("doctor found %d problem(s)", fails)
	case warns > 0:
		ui.Warn("%s worth looking at, but hop works", ui.Count(warns, "check", "checks"))
	default:
		ui.Ok("everything looks healthy")
	}
	return nil
}

// earlierOnPath reports whether a comes before b in PATH.
func earlierOnPath(a *App, first, second string) bool {
	first, second = filepath.Clean(first), filepath.Clean(second)
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		switch filepath.Clean(dir) {
		case first:
			return true
		case second:
			return false
		}
	}
	return false
}

func runVerify(a *App, args []string) error {
	if len(args) > 0 {
		return usagef("verify takes no arguments")
	}
	gen, err := a.Current()
	if err != nil {
		return err
	}
	if gen == nil {
		ui.Info("nothing installed yet")
		return nil
	}

	sp := ui.StartSpinner("Verifying %s", ui.Count(len(gen.Packages), "package", "packages"))
	issues := core.Verify(a.Layout, gen)
	sp.Stop()

	if a.JSON {
		return a.emitJSON(map[string]any{
			"generation": gen.ID, "packages": len(gen.Packages), "issues": issues,
		})
	}

	if len(issues) == 0 {
		ui.Ok("all %s in generation %d are intact", ui.Count(len(gen.Packages), "package", "packages"), gen.ID)
		return nil
	}

	ui.Err("%s", ui.Count(len(issues), "problem", "problems"))
	ui.Blank()
	for _, is := range issues {
		ui.Line("  %s %s  %s", ui.Red("✗"), ui.Pkg(is.Name), is.Problem)
	}
	ui.Blank()
	var names []string
	seen := map[string]bool{}
	for _, is := range issues {
		if !seen[is.Name] {
			names = append(names, is.Name)
			seen[is.Name] = true
		}
	}
	ui.Hint("repair with `hop install --force %s`", strings.Join(names, " "))
	return fmt.Errorf("%d integrity problem(s)", len(issues))
}

func runUpdate(a *App, args []string) error {
	if len(args) > 0 {
		return usagef("update takes no arguments")
	}
	url := core.IndexURL()
	if url == "" {
		ix, err := a.Index_()
		if err != nil {
			return err
		}
		if a.JSON {
			return a.emitJSON(map[string]any{"source": "builtin", "recipes": ix.Len(), "changed": false})
		}
		ui.Info("using the index built into hop (%s)", ui.Count(ix.Len(), "recipe", "recipes"))
		ui.Blank()
		ui.Line("  There is nothing to update: hop ships its index inside the binary,")
		ui.Line("  so a fresh machine can install packages with no network at all.")
		ui.Blank()
		ui.Line("  To follow a live index instead, set:")
		ui.Line("    %s", ui.Cyan("export HOP_INDEX_URL=https://example.com/hop-index.json"))
		return nil
	}

	sp := ui.StartSpinner("Fetching index")
	res, err := core.UpdateIndex(a.Layout, a.Client)
	if err != nil {
		sp.Stop()
		return err
	}

	if a.JSON {
		sp.Stop()
		return a.emitJSON(map[string]any{
			"source": res.Source, "recipes": res.Recipes,
			"changed": res.Changed, "not_modified": res.NotModif,
		})
	}

	if res.NotModif {
		sp.Succeed("index is already current (%s)", ui.Count(res.Recipes, "recipe", "recipes"))
		return nil
	}
	sp.Succeed("index updated: %s", ui.Count(res.Recipes, "recipe", "recipes"))

	// Tell the user immediately whether the refresh actually gained anything.
	a.index = nil
	if gen, err := a.Current(); err == nil && gen != nil {
		if ix, err := a.Index_(); err == nil {
			if old := core.NewResolver(ix, gen, core.CurrentPlatform()).Outdated(); len(old) > 0 {
				ui.Blank()
				ui.Line("%s can be upgraded:", ui.Count(len(old), "package", "packages"))
				for _, o := range old[:minInt(len(old), 5)] {
					if o.Missing {
						continue
					}
					ui.Line("  %s %s %s %s", ui.Pkg(o.Name), ui.Grey(o.Have), ui.Arrow(), ui.Bold(o.Want))
				}
				ui.Blank()
				ui.Line("%s", ui.Grey("run:  ")+ui.Cyan("hop upgrade"))
			}
		}
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
