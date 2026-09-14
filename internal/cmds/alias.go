package cmds

import (
	"fmt"
	"os"
	"strings"

	"github.com/samuelbanapour/hopcli/internal/core"
	"github.com/samuelbanapour/hopcli/internal/ui"
)

func init() {
	register(&Command{
		Name:  "alias",
		Group: "History",
		Usage: "hop alias <package> [version]",
		Short: "Hot-swap one tool to a version you've had installed before",
		Long: `Switch a single tool's active version without touching anything else.

Every version hop has ever installed for you stays in the content-addressed
store, so switching back to one is a symlink swap: instant, no download, and
every other package in the active set is left exactly as it is.

With no version, lists what this package has been at before, so you can see
what is available to switch to. This only reaches versions hop has actually
installed on this machine — the recipe index tracks the current release
only, so a version that has never been installed here has nothing to swap to.`,
		Run: runAlias,
	})
}

func runAlias(a *App, args []string) error {
	if len(args) == 0 || len(args) > 2 {
		return usagef("alias <package> [version]")
	}
	name := args[0]

	cur, err := a.Current()
	if err != nil {
		return err
	}
	if cur == nil {
		return fmt.Errorf("nothing is installed")
	}
	active, ok := cur.Find(name)
	if !ok {
		return fmt.Errorf("%s is not installed (see `hop list`)", name)
	}

	versions, err := core.AliasVersions(a.Layout, name)
	if err != nil {
		return err
	}

	if len(args) == 1 {
		return listAliasVersions(a, name, active, versions)
	}
	return switchAlias(a, name, args[1], active, versions)
}

func listAliasVersions(a *App, name string, active *core.Installed, versions []core.Installed) error {
	if a.JSON {
		type row struct {
			Version string `json:"version"`
			Active  bool   `json:"active"`
			InStore bool   `json:"in_store"`
		}
		out := make([]row, 0, len(versions))
		for _, v := range versions {
			out = append(out, row{v.Version, v.Version == active.Version, storePathExists(v.StorePath)})
		}
		return a.emitJSON(out)
	}

	t := ui.NewTable("", "VERSION", "STATUS")
	for _, v := range versions {
		mark := " "
		status := ui.Grey("in store")
		if !storePathExists(v.StorePath) {
			status = ui.Grey("gc'd — `hop install` to fetch it again")
		}
		if v.Version == active.Version {
			mark = ui.Green("●")
			status = ui.Green("active")
		}
		t.Row("  "+mark, ui.Bold(v.Version), status)
	}
	t.Render()
	ui.Blank()
	ui.Line("%s", ui.Grey("switch with:  ")+ui.Cyan(fmt.Sprintf("hop alias %s <version>", name)))
	return nil
}

func switchAlias(a *App, name, version string, active *core.Installed, versions []core.Installed) error {
	if version == active.Version {
		ui.Info("%s %s is already active", name, version)
		return nil
	}

	var target *core.Installed
	for i := range versions {
		if versions[i].Version == version {
			target = &versions[i]
			break
		}
	}
	if target == nil {
		if len(versions) == 0 {
			return fmt.Errorf("hop has never installed %s at any version but %s", name, active.Version)
		}
		avail := make([]string, 0, len(versions))
		for _, v := range versions {
			avail = append(avail, v.Version)
		}
		return fmt.Errorf("%s has no version %s to switch to (available: %s)", name, version, strings.Join(avail, ", "))
	}
	if !storePathExists(target.StorePath) {
		return fmt.Errorf("%s %s was reclaimed by `hop gc` and can no longer be switched to; only what `hop install %s` fetches now remains available", name, version, name)
	}

	if a.DryRun {
		ui.Info("dry run: would switch %s from %s to %s", name, active.Version, version)
		return nil
	}

	guard, err := a.lock()
	if err != nil {
		return err
	}
	defer guard.Release()

	g, err := core.BuildAliasGeneration(a.Layout, *target)
	if err != nil {
		return err
	}
	if issues := core.Verify(a.Layout, g); len(issues) > 0 {
		return fmt.Errorf("%s %s is incomplete on disk and cannot be switched to", name, version)
	}
	if err := core.Activate(a.Layout, g.ID); err != nil {
		return err
	}

	if a.JSON {
		return a.emitJSON(map[string]any{
			"name": name, "from": active.Version, "to": version, "generation": g.ID,
		})
	}
	ui.Ok("switched %s %s %s %s", ui.Pkg(name), ui.Grey(active.Version), ui.Arrow(), ui.Bold(version))
	return nil
}

func storePathExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}
