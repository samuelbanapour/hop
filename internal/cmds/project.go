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
		Name:  "init",
		Group: "Projects",
		Usage: "hop init [flags]",
		Short: "Create a hopfile.toml in this directory",
		Long: `Create a hopfile: a declarative list of the tools a project needs.

Commit it alongside your code and anyone can reproduce your toolchain with a
single ` + "`hop sync`" + `. With --from-installed the file starts out
describing whatever you already have.`,
		Flags: []Flag{
			{Long: "from-installed", Kind: 'b', Help: "seed the file from currently installed packages"},
			{Long: "force", Short: "f", Kind: 'b', Help: "overwrite an existing hopfile"},
		},
		Run: runInit,
	})

	register(&Command{
		Name:  "add",
		Group: "Projects",
		Usage: "hop add <package>...",
		Short: "Install packages and record them in the hopfile",
		Long: `Install packages and add them to this project's hopfile.

Exactly like ` + "`hop install`" + `, except the hopfile is updated so the
choice is recorded for everyone else working on the project.`,
		Flags: []Flag{
			{Long: "force", Short: "f", Kind: 'b', Help: "reinstall even if already at this version"},
		},
		Run: runAdd,
	})

	register(&Command{
		Name:  "sync",
		Group: "Projects",
		Usage: "hop sync [flags]",
		Short: "Make the installed set match the hopfile exactly",
		Long: `Install, upgrade and remove packages until this machine matches the
hopfile.

With a hop.lock present, sync installs the exact versions it pins, so two
machines end up byte-identical. Use --locked in CI to fail rather than
silently re-resolve when the lockfile is stale.`,
		Flags: []Flag{
			{Long: "prune", Kind: 'b', Help: "remove packages not in the hopfile (default from the file)"},
			{Long: "locked", Kind: 'b', Help: "require an up-to-date hop.lock; do not re-resolve"},
		},
		Run: runSync,
	})

	register(&Command{
		Name:  "lock",
		Group: "Projects",
		Usage: "hop lock",
		Short: "Write hop.lock pinning the resolved versions",
		Run:   runLock,
	})
}

// findOrExplainHopfile locates the hopfile, with a useful error if there is none.
func findOrExplainHopfile(a *App) (*core.Hopfile, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	path, ok := core.FindHopfile(cwd)
	if !ok {
		return nil, fmt.Errorf("no %s in this directory or any parent (create one with `hop init`)", core.HopfileName)
	}
	return core.LoadHopfile(path)
}

func runInit(a *App, args []string) error {
	if len(args) > 0 {
		return usagef("init takes no arguments")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	path := filepath.Join(cwd, core.HopfileName)

	if _, err := os.Stat(path); err == nil && !a.Force {
		ui.Err("%s already exists", core.HopfileName)
		ui.Hint("use --force to overwrite it, or edit it directly")
		return fmt.Errorf("refusing to overwrite %s", path)
	}

	pkgs := map[string]string{}
	if a.boolFlag("from-installed") {
		gen, err := a.Current()
		if err != nil {
			return err
		}
		if gen == nil || len(gen.Packages) == 0 {
			ui.Warn("nothing is installed, so the hopfile will start out empty")
		}
		for _, p := range gen.Clone() {
			if p.Explicit {
				pkgs[p.Name] = p.Version
			}
		}
	}

	if a.DryRun {
		ui.Info("dry run: would write %s with %s", path, ui.Count(len(pkgs), "package", "packages"))
		return nil
	}
	if err := core.WriteHopfile(path, pkgs); err != nil {
		return err
	}

	ui.Ok("wrote %s", ui.Bold(core.HopfileName))
	if len(pkgs) > 0 {
		names := make([]string, 0, len(pkgs))
		for n := range pkgs {
			names = append(names, n)
		}
		sort.Strings(names)
		ui.Blank()
		for _, n := range names {
			ui.Line("  %s %s", ui.Pkg(n), ui.Grey(pkgs[n]))
		}
	}
	ui.Blank()
	ui.Line("%s", ui.Grey("commit it, then teammates run:  ")+ui.Cyan("hop sync"))
	return nil
}

func runAdd(a *App, args []string) error {
	if len(args) == 0 {
		return usagef("add what? name at least one package")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	path, ok := core.FindHopfile(cwd)
	if !ok {
		// Adding to a project without a hopfile should just create one.
		path = filepath.Join(cwd, core.HopfileName)
		if !a.DryRun {
			if err := core.WriteHopfile(path, nil); err != nil {
				return err
			}
			ui.Info("created %s", core.HopfileName)
		}
	}

	// Install first: recording a package that failed to install would be a lie.
	if err := runInstall(a, args); err != nil {
		return err
	}
	if a.DryRun {
		ui.Info("dry run: would record %s in %s", ui.List(args, "and"), filepath.Base(path))
		return nil
	}

	ix, err := a.Index_()
	if err != nil {
		return err
	}
	var recorded []string
	for _, raw := range args {
		name, version := splitNameVersion(raw)
		r, ok := ix.Lookup(name)
		if !ok {
			continue
		}
		constraint := "*"
		if version != "" {
			constraint = version
		}
		if err := core.UpsertPackage(path, r.Name, constraint); err != nil {
			return fmt.Errorf("updating %s: %w", filepath.Base(path), err)
		}
		recorded = append(recorded, r.Name)
	}
	if len(recorded) > 0 {
		ui.Blank()
		ui.Ok("recorded %s in %s", ui.List(quoteAll(recorded), "and"), ui.Bold(filepath.Base(path)))
	}
	return nil
}

func runSync(a *App, args []string) error {
	if len(args) > 0 {
		return usagef("sync takes no arguments")
	}
	h, err := findOrExplainHopfile(a)
	if err != nil {
		return err
	}
	ix, err := a.Index_()
	if err != nil {
		return err
	}
	cur, err := a.Current()
	if err != nil {
		return err
	}

	want := h.Packages
	lockPath := core.LockfilePathFor(h.Path)
	lf, lerr := core.LoadLockfile(lockPath)

	switch {
	case a.Locked:
		if lerr != nil {
			return fmt.Errorf("--locked needs %s, but it could not be read: %w", core.LockfileName, lerr)
		}
		if lf.Stale(h) {
			return fmt.Errorf("%s is out of date with %s; run `hop lock` and commit the result",
				core.LockfileName, core.HopfileName)
		}
		want = lf.AsConstraints()
		ui.Info("using pinned versions from %s", core.LockfileName)
	case lerr == nil && !lf.Stale(h):
		// A current lockfile is the whole point of having one: honour it.
		want = lf.AsConstraints()
		ui.Debug("using %s", lockPath)
	case lerr == nil:
		ui.Warn("%s is out of date with %s; resolving fresh versions", core.LockfileName, core.HopfileName)
		ui.Hint("run `hop lock` afterwards to re-pin")
	}

	prune := h.Prune
	if a.NoPrune {
		prune = false
	}
	if a.Jobs == defaultJobs() && h.Jobs > 0 {
		a.Jobs = h.Jobs
	}

	plan, err := core.NewResolver(ix, cur, core.CurrentPlatform()).PlanSync(want, prune)
	if err != nil {
		return err
	}

	if a.JSON && a.DryRun {
		return a.emitJSON(toPlanJSON(plan, true))
	}
	if plan.Empty() {
		if a.JSON {
			return a.emitJSON(toPlanJSON(plan, a.DryRun))
		}
		ui.Ok("already in sync with %s (%s)", filepath.Base(h.Path),
			ui.Count(len(h.Packages), "package", "packages"))
		return nil
	}

	renderPlan(plan, fmt.Sprintf("Syncing with %s", filepath.Base(h.Path)))
	if a.DryRun {
		ui.Info("dry run: nothing was changed")
		return nil
	}
	if !a.confirm("Proceed?") {
		ui.Info("cancelled")
		return nil
	}

	// A sync mixing downloads and removals is still one transaction.
	if len(plan.Downloads()) == 0 {
		guard, err := a.lock()
		if err != nil {
			return err
		}
		defer guard.Release()
		res, err := core.ApplyRemoveOnly(a.Layout, plan, a.CommandLine)
		if err != nil {
			return err
		}
		if a.JSON {
			return a.emitJSON(toResultJSON(res))
		}
		renderResult(a, res, res.Generation)
	} else if err := execute(a, plan, cur, fmt.Sprintf("Syncing %s", ui.Count(len(plan.Downloads()), "package", "packages"))); err != nil {
		return err
	}

	// Keep the lockfile honest after a successful sync.
	if gen, err := a.Current(); err == nil && gen != nil {
		lf := core.BuildLockfile(gen.Packages, core.CurrentPlatform(), h.Path)
		if err := lf.Write(lockPath); err == nil && !a.JSON {
			ui.Line("%s", ui.Grey("updated "+core.LockfileName))
		}
	}
	return nil
}

func runLock(a *App, args []string) error {
	if len(args) > 0 {
		return usagef("lock takes no arguments")
	}
	h, err := findOrExplainHopfile(a)
	if err != nil {
		return err
	}
	ix, err := a.Index_()
	if err != nil {
		return err
	}

	// Resolve the hopfile without installing anything: a lockfile records what
	// sync *would* do, so it can be written and reviewed before any download.
	plan, err := core.NewResolver(ix, nil, core.CurrentPlatform()).PlanSync(h.Packages, false)
	if err != nil {
		return err
	}

	lockPath := core.LockfilePathFor(h.Path)
	lf := core.BuildLockfile(plan.Final, core.CurrentPlatform(), h.Path)

	if a.JSON {
		return a.emitJSON(lf)
	}
	if a.DryRun {
		ui.Info("dry run: would pin %s in %s", ui.Count(len(lf.Packages), "package", "packages"), core.LockfileName)
		for _, p := range lf.Packages {
			ui.Line("  %s %s", ui.Pkg(p.Name), ui.Grey(p.Version))
		}
		return nil
	}

	if err := lf.Write(lockPath); err != nil {
		return err
	}
	ui.Ok("pinned %s in %s", ui.Count(len(lf.Packages), "package", "packages"), ui.Bold(core.LockfileName))

	unpinned := 0
	for _, p := range lf.Packages {
		if p.SHA256 == "" {
			unpinned++
		}
	}
	if unpinned > 0 {
		ui.Warn("%s have no checksum in the index and cannot be pinned byte-exactly", ui.Count(unpinned, "package", "packages"))
	}
	ui.Blank()
	ui.Line("%s", ui.Grey("commit it; then `hop sync --locked` reproduces it exactly"))
	return nil
}

// boolFlag reads a command-scoped boolean that App has no dedicated field for.
func (a *App) boolFlag(name string) bool { return a.extraBools[name] }

// stringsEqualFold reports set equality, ignoring case and order.
func stringsEqualFold(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string{}, a...)
	y := append([]string{}, b...)
	for i := range x {
		x[i] = strings.ToLower(x[i])
	}
	for i := range y {
		y[i] = strings.ToLower(y[i])
	}
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
