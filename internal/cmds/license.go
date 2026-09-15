package cmds

import (
	"time"

	"github.com/samuelbanapour/hopcli/internal/core"
	"github.com/samuelbanapour/hopcli/internal/ui"
)

func init() {
	register(&Command{
		Name:  "license",
		Group: "General",
		Usage: "hop license [install <token>|status]",
		Short: "Show or install your consent token",
		Long: `hop requires the copyright holder's prior consent to use, with government
entities exempt from that requirement — see LICENSE in the source repository
for the exact terms.

Run with no arguments for how to request a token. Once you have one:

    hop license install <token>

installs it to <root>/license.token, where every future command picks it up
automatically. HOP_LICENSE_TOKEN in the environment works too, and is
checked first — handy for CI or containers without writing a file.`,
		Run: runLicense,
	})
}

func runLicense(a *App, args []string) error {
	if len(args) == 0 {
		printLicenseInfo(a)
		return nil
	}

	switch args[0] {
	case "status":
		tok, err := core.LoadLicenseToken(a.Layout)
		if err != nil {
			ui.Warn("no valid consent token: %v", err)
			ui.Hint("run `hop license` for how to request one")
			return nil
		}
		ui.Ok("consent token valid")
		t := ui.NewTable()
		t.Row("  subject", tok.Subject)
		if tok.Gov {
			t.Row("  granted under", "government exemption")
		}
		t.Row("  issued", time.Unix(tok.IssuedAt, 0).Format("2006-01-02"))
		if tok.ExpiresAt == 0 {
			t.Row("  expires", "never")
		} else {
			t.Row("  expires", time.Unix(tok.ExpiresAt, 0).Format("2006-01-02"))
		}
		t.Render()
		return nil

	case "install":
		if len(args) != 2 {
			return usagef("license install takes exactly one token")
		}
		tok, err := core.InstallLicenseToken(a.Layout, args[1])
		if err != nil {
			return err
		}
		ui.Ok("consent token installed for %s", ui.Bold(tok.Subject))
		return nil

	default:
		return usagef("unknown license subcommand %q; try `install <token>` or `status`", args[0])
	}
}

func printLicenseInfo(a *App) {
	ui.Blank()
	ui.Line("%s", ui.Bold("hop requires prior consent to use"))
	ui.Blank()
	ui.Line("  Government entities are exempt from this requirement — everyone else")
	ui.Line("  needs a signed consent token before hop will run. See LICENSE in the")
	ui.Line("  source repository for the exact terms.")
	ui.Blank()
	ui.Line("  %s", ui.Bold("To request one:"))
	ui.Line("  contact the copyright holder and describe your intended use.")
	ui.Blank()
	ui.Line("  %s", ui.Bold("Once you have a token:"))
	ui.Line("    %s", ui.Cyan("hop license install <token>"))
	ui.Blank()
	if _, err := core.LoadLicenseToken(a.Layout); err == nil {
		ui.Ok("a valid token is already installed — run `hop license status` for details")
	}
}
