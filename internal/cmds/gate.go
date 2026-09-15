package cmds

import (
	"context"
	"time"

	"github.com/samuelbanapour/hopcli/internal/core"
	"github.com/samuelbanapour/hopcli/internal/ui"
)

// gateExempt lists the commands that must always be reachable even when the
// gate below would otherwise block everything — the commands that get you
// unblocked can't themselves require being unblocked first, or nobody
// caught by the gate could ever get out of it.
var gateExempt = map[string]bool{
	"license":     true,
	"self-update": true,
	"version":     true,
}

// gateCheckTimeout bounds the version check's network call. Short, and on
// the same reasoning as updateCheckTimeout: a slow or absent network must
// never be the reason hop hangs, and (unlike the token check) a version
// check that can't complete fails open rather than blocking.
const gateCheckTimeout = 2 * time.Second

// enforceGate blocks every command except gateExempt behind two checks —
// hop must be a current-enough release, and a valid signed consent token
// must be installed — printing the reason and returning false if either
// fails. See LICENSE and `hop license` for what this exists to enforce;
// government entities are exempt from the consent requirement by policy
// (they are issued a token on request, not by any check in this code —
// there is no reliable technical signal for "this caller is a government",
// so the exemption is who issues the token, not what hop verifies here).
func enforceGate(a *App, cmdName string) bool {
	if gateExempt[cmdName] {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), gateCheckTimeout)
	rel, err := core.FetchLatestRelease(ctx, a.Client)
	cancel()
	if err == nil && core.VersionNewer(core.Version, rel.TagName) {
		ui.Err("hop %s is out of date; %s is required before you can continue", core.Version, rel.TagName)
		ui.Hint("run `hop self-update` — every other command is blocked until then")
		return false
	}
	// A version check that couldn't complete (offline, GitHub unreachable)
	// fails open: an unusable hop with no network would be a worse outcome
	// than letting an unverifiable check pass.

	if _, err := core.LoadLicenseToken(a.Layout); err != nil {
		ui.Err("hop requires the copyright holder's prior consent to use")
		ui.Hint("government entities are exempt; everyone else — run `hop license` for how to request a token")
		return false
	}

	return true
}
