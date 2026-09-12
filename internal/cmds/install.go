package cmds

import (
	"fmt"
	"strings"

	"github.com/samuelbanapour/hop/internal/core"
	"github.com/samuelbanapour/hop/internal/ui"
)

func init() {
	register(&Command{
		Name:    "install",
		Aliases: []string{"i", "add-pkg"},
		Group:   "Packages",
		Usage:   "hop install [flags] <package>...",
		Short:   "Install packages",
		Long: `Install one or more packages and everything they depend on.

Every install is a transaction. hop downloads and verifies every artifact
first, extracts them into the content-addressed store, and only then flips a
single symlink to make the new set live. If anything fails, nothing changes:
your environment is left exactly as it was.

Downloads run in parallel, and an artifact already in the store is reused
without touching the network.`,
		Flags: []Flag{
			{Long: "force", Short: "f", Kind: 'b', Help: "reinstall even if already at this version"},
		},
		Run: runInstall,
	})

	register(&Command{
		Name:    "remove",
		Aliases: []string{"rm", "uninstall"},
		Group:   "Packages",
		Usage:   "hop remove [flags] <package>...",
		Short:   "Remove packages and their orphaned dependencies",
		Long: `Remove packages from the active set.

Dependencies that nothing else needs are removed too, so uninstalling does not
leave the orphans behind that other package managers accumulate. Use
--keep-orphans to remove only what you named.

Removal is bookkeeping only: the package stays in the store until you run
` + "`hop gc`" + `, which is why ` + "`hop rollback`" + ` can bring it straight back.`,
		Flags: []Flag{
			{Long: "keep-orphans", Kind: 'b', Help: "do not remove dependencies that are no longer needed"},
		},
		Run: runRemove,
	})

	register(&Command{
		Name:    "upgrade",
		Aliases: []string{"up"},
		Group:   "Packages",
		Usage:   "hop upgrade [flags] [package]...",
		Short:   "Upgrade packages to the newest indexed version",
		Long: `Upgrade the named packages, or everything if you name none.

Like install, an upgrade is a single transaction across every package: either
they all move, or none do. The previous versions stay in the store, so
` + "`hop rollback`" + ` undoes the whole upgrade instantly.`,
		Run: runUpgrade,
	})

	register(&Command{
		Name:  "reinstall",
		Group: "Packages",
		Usage: "hop reinstall <package>...",
		Short: "Reinstall packages at their current indexed version",
		Run: func(a *App, args []string) error {
			a.Force = true
			return runInstall(a, args)
		},
	})
}

func runInstall(a *App, args []string) error {
	if len(args) == 0 {
		return usagef("install what? name at least one package")
	}

	ix, err := a.Index_()
	if err != nil {
		return err
	}
	cur, err := a.Current()
	if err != nil {
		return err
	}

	plan, err := core.NewResolver(ix, cur, core.CurrentPlatform()).PlanInstall(args, a.Force)
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
		ui.Ok("%s already installed and up to date", ui.List(quoteAll(args), "and"))
		ui.Hint("use `hop install --force` to reinstall")
		return nil
	}

	renderPlan(plan, "hop will")
	if a.DryRun {
		ui.Info("dry run: nothing was changed")
		return nil
	}
	if !a.confirm("Proceed?") {
		ui.Info("cancelled")
		return nil
	}

	return execute(a, plan, cur, fmt.Sprintf("Downloading %s", ui.Count(len(plan.Downloads()), "package", "packages")))
}

func runRemove(a *App, args []string) error {
	if len(args) == 0 {
		return usagef("remove what? name at least one package")
	}

	ix, err := a.Index_()
	if err != nil {
		return err
	}
	cur, err := a.Current()
	if err != nil {
		return err
	}
	if cur == nil {
		return fmt.Errorf("nothing is installed")
	}

	plan, err := core.NewResolver(ix, cur, core.CurrentPlatform()).PlanRemove(args, a.KeepOrphans)
	if err != nil {
		return err
	}
	if a.JSON && a.DryRun {
		return a.emitJSON(toPlanJSON(plan, true))
	}
	if plan.Empty() {
		ui.Info("nothing to remove")
		return nil
	}

	renderPlan(plan, "hop will")
	if a.DryRun {
		ui.Info("dry run: nothing was changed")
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

	res, err := core.ApplyRemoveOnly(a.Layout, plan, a.CommandLine)
	if err != nil {
		return err
	}
	if a.JSON {
		return a.emitJSON(toResultJSON(res))
	}

	renderResult(a, res, res.Generation)
	ui.Hint("the files are still in the store; `hop gc` reclaims them, `hop rollback` restores them")
	return nil
}

func runUpgrade(a *App, args []string) error {
	ix, err := a.Index_()
	if err != nil {
		return err
	}
	cur, err := a.Current()
	if err != nil {
		return err
	}
	if cur == nil || len(cur.Packages) == 0 {
		return fmt.Errorf("nothing is installed")
	}

	plan, err := core.NewResolver(ix, cur, core.CurrentPlatform()).PlanUpgrade(args)
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
		ui.Ok("everything is up to date")
		if ix.IsBuiltin() {
			ui.Hint("this is the index built into hop; set HOP_INDEX_URL and run `hop update` for newer recipes")
		}
		return nil
	}

	renderPlan(plan, "hop will")
	if a.DryRun {
		ui.Info("dry run: nothing was changed")
		return nil
	}
	if !a.confirm("Proceed?") {
		ui.Info("cancelled")
		return nil
	}

	return execute(a, plan, cur, fmt.Sprintf("Upgrading %s", ui.Count(len(plan.Downloads()), "package", "packages")))
}

// execute runs a plan that needs downloads, rendering live progress.
func execute(a *App, plan *core.Plan, cur *core.Generation, title string) error {
	guard, err := a.lock()
	if err != nil {
		return err
	}
	defer guard.Release()

	// Re-read the current generation under the lock: another hop may have
	// committed between planning and here.
	fresh, err := a.Current()
	if err != nil {
		return err
	}
	if generationChanged(cur, fresh) {
		ui.Warn("the installed set changed while hop was waiting for the lock; re-planning")
		return fmt.Errorf("state changed under us; run the command again")
	}

	ui.Blank()
	ui.Step("%s", title)
	ui.Blank()

	prog := ui.NewProgress()
	res, err := core.Apply(a.Ctx, a.Layout, cur, plan, core.ApplyOptions{
		Jobs:    a.Jobs,
		Sink:    sink{prog},
		Client:  a.Client,
		Command: a.CommandLine,
	})
	prog.Stop()
	if err != nil {
		return err
	}

	if a.JSON {
		return a.emitJSON(toResultJSON(res))
	}
	renderResult(a, res, res.Generation)
	return nil
}

// generationChanged reports whether the active generation moved.
func generationChanged(before, after *core.Generation) bool {
	switch {
	case before == nil && after == nil:
		return false
	case before == nil || after == nil:
		return true
	default:
		return before.ID != after.ID
	}
}

// splitNameVersion accepts "name@version" so users can pin inline.
func splitNameVersion(s string) (name, version string) {
	if i := strings.LastIndexByte(s, '@'); i > 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}
