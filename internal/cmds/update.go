package cmds

import (
	"context"
	"runtime"
	"time"

	"github.com/samuelbanapour/hopcli/internal/core"
	"github.com/samuelbanapour/hopcli/internal/ui"
)

func init() {
	register(&Command{
		Name:    "self-update",
		Aliases: []string{"self-upgrade"},
		Group:   "Maintenance",
		Usage:   "hop self-update [flags]",
		Short:   "Update hop itself to the latest release",
		Long: `Check GitHub for hop's latest published release and, if it is newer than
the version currently running, download it, verify its checksum against
the release's own SHA256SUMS, and replace the running binary in place.

This only ever updates hop itself — it is not ` + "`hop upgrade`" + `, which
moves installed packages to their newest indexed version instead. Neither
command touches what the other manages.

The replacement is atomic: the new binary is written alongside the old one
and renamed over it, so an interrupted self-update never leaves a
half-written executable in place, and anything already running keeps
working off the old file until it exits.`,
		Flags: []Flag{
			{Long: "check", Kind: 'b', Help: "only report whether a newer version exists; do not install it"},
		},
		Run: runSelfUpdate,
	})
}

// runSelfUpdate is the explicit, user-invoked counterpart to
// maybeCheckForUpdate below: unlike the background check, it always hits
// the network (no once-a-day throttle) and, unless --check was given,
// actually installs whatever it finds.
func runSelfUpdate(a *App, args []string) error {
	if len(args) > 0 {
		return usagef("self-update takes no arguments")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return usagef("hop only publishes builds for darwin and linux")
	}

	sp := ui.StartSpinner("Checking for a newer release")
	rel, err := core.FetchLatestRelease(a.Ctx, a.Client)
	if err != nil {
		sp.Stop()
		return err
	}

	current := core.Version
	latest := rel.TagName
	if !core.VersionNewer(current, latest) {
		sp.Stop()
		if a.JSON {
			return a.emitJSON(map[string]any{"current": current, "latest": latest, "up_to_date": true})
		}
		ui.Ok("hop %s is already the latest version", current)
		return nil
	}

	if a.boolFlag("check") {
		sp.Stop()
		if a.JSON {
			return a.emitJSON(map[string]any{"current": current, "latest": latest, "up_to_date": false})
		}
		ui.Info("hop %s is available (you have %s)", ui.Bold(latest), current)
		ui.Hint("run `hop self-update` to install it")
		return nil
	}

	sp.Update("Downloading hop %s", latest)
	body, assetName, err := core.DownloadUpdate(a.Ctx, a.Client, rel)
	if err != nil {
		sp.Stop()
		return err
	}

	sp.Update("Verifying")
	bin, mode, err := core.ExtractBinary(body)
	if err != nil {
		sp.Stop()
		return err
	}

	if a.DryRun {
		sp.Stop()
		ui.Info("dry run: would replace the running binary with %s (%s)", assetName, ui.Bytes(int64(len(bin))))
		return nil
	}

	sp.Update("Installing")
	path, err := core.ReplaceSelf(bin, mode)
	if err != nil {
		sp.Stop()
		return err
	}
	sp.Stop()

	if a.JSON {
		return a.emitJSON(map[string]any{
			"current": current, "latest": latest, "up_to_date": false,
			"installed": true, "path": path,
		})
	}
	ui.Ok("updated hop %s %s %s", current, ui.Arrow(), ui.Bold(latest))
	ui.Hint("release notes: %s", rel.HTMLURL)
	return nil
}

// updateCheckInterval is how often an ordinary command's background check
// is allowed to actually hit the network — often enough that a real update
// surfaces within a day, rarely enough that no command pays for it more
// than once in that window.
const updateCheckInterval = 24 * time.Hour

// updateCheckTimeout bounds the background goroutine started below. It is
// deliberately short: this must never be the reason an unrelated command
// feels slow, and a check that doesn't finish in time is retried on the
// next invocation anyway, once updateCheckInterval has passed.
const updateCheckTimeout = 1500 * time.Millisecond

// maybeCheckForUpdate starts a background check for a newer hop release, if
// one hasn't run in the last updateCheckInterval, and returns a function
// that — called after the real command has finished its own work — prints
// a one-line notice if that check (a) finished in time and (b) found
// something newer. It never blocks the command itself: the network request
// runs concurrently with everything runCmd does, and the wait below only
// ever costs whatever time is left over.
//
// Skipped entirely for JSON output (scripts don't want stray text) and for
// the self-update command itself (which already does its own, unthrottled
// check) — checking both there would just be two network calls for one
// answer.
func maybeCheckForUpdate(a *App, cmdName string) func() {
	noop := func() {}
	if a.JSON || a.Quiet || cmdName == "self-update" {
		return noop
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return noop
	}

	st := core.ReadUpdateCheckState(a.Layout)
	if st.LastChecked != "" {
		if t, err := time.Parse(time.RFC3339, st.LastChecked); err == nil && time.Since(t) < updateCheckInterval {
			if st.LatestVersion != "" && core.VersionNewer(core.Version, st.LatestVersion) {
				latest := st.LatestVersion
				return func() {
					ui.Blank()
					ui.Hint("hop %s is available (you have %s) — run `hop self-update`", ui.Bold(latest), core.Version)
				}
			}
			return noop
		}
	}

	result := make(chan string, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
		defer cancel()
		rel, err := core.FetchLatestRelease(ctx, a.Client)
		latest := ""
		if err == nil {
			latest = rel.TagName
			core.WriteUpdateCheckState(a.Layout, core.UpdateCheckState{
				LastChecked: time.Now().Format(time.RFC3339), LatestVersion: latest,
			})
		}
		result <- latest
	}()

	return func() {
		select {
		case latest := <-result:
			if latest != "" && core.VersionNewer(core.Version, latest) {
				ui.Blank()
				ui.Hint("hop %s is available (you have %s) — run `hop self-update`", ui.Bold(latest), core.Version)
			}
		case <-time.After(updateCheckTimeout):
			// Slower than we're willing to wait for; the state file (if the
			// goroutine does eventually finish and write it) still saves the
			// next command from re-checking before updateCheckInterval is up.
		}
	}
}
