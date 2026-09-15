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
		Name:  "uninstall-self",
		Group: "Maintenance",
		Usage: "hop uninstall-self",
		Short: "Remove hop itself: every package, the whole store, and hop's own binary",
		Long: `Completely remove hop from this machine.

Every generation, every store path, the download cache, and finally hop's
own binary — all of it. This is the one command in hop that isn't
reversible: there is no ` + "`hop rollback`" + ` afterward, because there is
no hop left to run it.

A GUI app gets one courtesy step first: its symlink in ~/Applications is
removed, so nothing is left pointing at a store that's about to disappear.

hop never touches your shell configuration, so this doesn't either — if you
added ` + "`eval \"$(hop shellenv)\"`" + ` to ~/.zshrc or similar, this
prints the line to remove yourself, the same way ` + "`hop migrate`" + `
prints ` + "`brew uninstall`" + ` commands rather than running them.`,
		Run: runUninstallSelf,
	})
}

func runUninstallSelf(a *App, args []string) error {
	if len(args) > 0 {
		return usagef("uninstall-self takes no arguments")
	}

	stats, _ := core.Stats(a.Layout)
	gens, _ := core.Generations(a.Layout)
	cur, _ := a.Current()

	var appNames []string
	if cur != nil {
		for _, p := range cur.Packages {
			if p.Kind == core.KindApp && p.App != "" {
				appNames = append(appNames, p.App)
			}
		}
		sort.Strings(appNames)
	}

	self := ""
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			self = resolved
		} else {
			self = exe
		}
	}

	if a.JSON {
		out := map[string]any{
			"root":        a.Layout.Root,
			"generations": len(gens),
			"apps":        appNames,
			"binary":      self,
			"dry_run":     a.DryRun,
		}
		if stats != nil {
			out["bytes"] = stats.Bytes + stats.CacheBytes
		}
		return a.emitJSON(out)
	}

	ui.Warn("this removes hop entirely — every package, every generation, the whole store:")
	ui.Blank()
	ui.Line("  %s  %s", ui.Red("delete"), ui.Grey(a.Layout.Root))
	if stats != nil {
		ui.Line("    %s", ui.Grey(fmt.Sprintf("%s across %s, plus the cache",
			ui.Bytes(stats.Bytes+stats.CacheBytes), ui.Count(stats.Paths, "package", "packages"))))
	}
	if len(gens) > 0 {
		ui.Line("    %s", ui.Grey(fmt.Sprintf(
			"%s of history goes with it — there is no hop left afterward to roll back to",
			ui.Count(len(gens), "generation", "generations"))))
	}
	if len(appNames) > 0 {
		ui.Line("  %s  %s", ui.Red("unlink"), ui.Grey(strings.Join(appNames, ", ")+" from ~/Applications"))
	}
	if self != "" {
		ui.Line("  %s  %s", ui.Red("delete"), ui.Grey(self))
	}
	ui.Blank()

	if a.DryRun {
		ui.Info("dry run: nothing was changed")
		return nil
	}
	if !a.confirm("Uninstall hop completely?") {
		ui.Info("cancelled")
		return nil
	}

	guard, err := a.lock()
	if err != nil {
		return err
	}
	defer guard.Release()

	if cur != nil && len(appNames) > 0 {
		if err := core.UnlinkApps(a.Layout, cur); err != nil {
			ui.Warn("could not unlink every app from ~/Applications: %v", err)
		}
	}

	if err := os.RemoveAll(a.Layout.Root); err != nil {
		return fmt.Errorf("removing %s: %w", a.Layout.Root, err)
	}
	ui.Ok("removed %s", a.Layout.Root)

	if self != "" {
		switch err := os.Remove(self); {
		case err == nil:
			ui.Ok("removed %s", self)
		case os.IsNotExist(err):
			// The binary lived inside Root (a HOP_ROOT/bin install, say)
			// and the RemoveAll above already took it — not a failure,
			// just means there is nothing left to do here.
		default:
			ui.Warn("could not remove the hop binary itself: %v", err)
			ui.Hint("delete it yourself: rm %s", self)
		}
	}

	ui.Blank()
	ui.Hint(`remove this from your shell config if you added it: eval "$(hop shellenv)"`)
	return nil
}
