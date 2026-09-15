package cmds

import (
	"bufio"
	"os"
	"strings"
	"time"

	"github.com/samuelbanapour/hopcli/internal/core"
	"github.com/samuelbanapour/hopcli/internal/ui"
)

func init() {
	register(&Command{
		Name:  "license",
		Group: "General",
		Usage: "hop license [accept|install <token>|status [request-id]]",
		Short: "Show, self-request, or install your consent token",
		Long: `hop requires the copyright holder's prior consent to use, with government
entities exempt from that requirement — see LICENSE in the source repository
for the exact terms.

Run with no arguments for an overview.

    hop license accept

walks through the self-service flow: read the terms, give your name and
email, and hop records that acceptance and emails you a confirmation link.
Clicking it gives you a one-time code — enter that at the service's /redeem
page to get your token. That page is the only place the token is ever
shown, and only once, so copy it when you see it.

    hop license install <token>

installs a token (self-service or one issued to you directly) to
<root>/license.token, where every future command picks it up automatically.
HOP_LICENSE_TOKEN in the environment works too, and is checked first —
handy for CI or containers without writing a file.

    hop license status [request-id]

with no argument, shows the locally installed token. With a request-id from
` + "`hop license accept`" + `, checks whether that request is still pending,
has been email-verified, or has already been redeemed — never the token
itself, which this command can't see any more than anyone else can.`,
		Run: runLicense,
	})
}

func runLicense(a *App, args []string) error {
	if len(args) == 0 {
		printLicenseInfo(a)
		return nil
	}

	switch args[0] {
	case "accept":
		if len(args) != 1 {
			return usagef("license accept takes no arguments")
		}
		return runLicenseAccept(a)

	case "status":
		if len(args) == 2 {
			return runLicenseRemoteStatus(a, args[1])
		}
		if len(args) != 1 {
			return usagef("license status takes at most one request-id")
		}
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
		return usagef("unknown license subcommand %q; try `accept`, `install <token>`, or `status`", args[0])
	}
}

func runLicenseRemoteStatus(a *App, requestID string) error {
	st, err := core.CheckLicenseStatus(a.Ctx, a.Client, requestID)
	if err != nil {
		return err
	}
	switch st.Status {
	case "redeemed":
		ui.Ok("this request's token has already been redeemed")
		ui.Hint("if that wasn't you: contact the copyright holder to revoke and reissue")
	case "verified":
		ui.Info("email confirmed — go to the service's /redeem page with your one-time code to get your token")
	default:
		ui.Info("still waiting on email confirmation")
	}
	return nil
}

// runLicenseAccept walks a person through the self-service consent flow:
// read the terms, give identifying info, and hand off to email + the
// service's /redeem webpage for the one-time token disclosure — this
// command never sees or installs the token itself, on purpose.
func runLicenseAccept(a *App) error {
	if core.LicenseServiceURL() == "" {
		ui.Err("no self-service license service is configured")
		ui.Hint("set HOP_LICENSE_SERVICE_URL, or ask the copyright holder to issue you a token directly")
		return nil
	}
	if !ui.Interactive() {
		ui.Err("`hop license accept` needs a real terminal to show terms and collect your agreement")
		return nil
	}

	terms, err := core.FetchLicenseTerms(a.Ctx, a.Client)
	if err != nil {
		return err
	}

	ui.Blank()
	ui.Line("%s", ui.Bold("hop's terms (version "+terms.Version+")"))
	ui.Blank()
	for _, line := range strings.Split(strings.TrimRight(terms.Text, "\n"), "\n") {
		ui.Line("  %s", line)
	}
	ui.Blank()

	in := bufio.NewReader(os.Stdin)
	name, ok := readLine(in, "your name: ")
	if !ok || strings.TrimSpace(name) == "" {
		ui.Info("cancelled")
		return nil
	}
	email, ok := readLine(in, "your email: ")
	if !ok || strings.TrimSpace(email) == "" {
		ui.Info("cancelled")
		return nil
	}
	agree, ok := readLine(in, `type "I agree" to accept the terms above: `)
	if !ok || !strings.EqualFold(strings.TrimSpace(agree), "I agree") {
		ui.Info("not accepted; cancelled")
		return nil
	}

	res, err := core.SubmitLicenseAcceptance(a.Ctx, a.Client, strings.TrimSpace(name), strings.TrimSpace(email))
	if err != nil {
		return err
	}

	ui.Blank()
	ui.Ok("recorded — check %s for a confirmation email", ui.Bold(strings.TrimSpace(email)))
	if res.IsGov {
		ui.Hint("%s was recognized as a government domain", strings.TrimSpace(email))
	}
	ui.Line("  1. click the link in that email")
	ui.Line("  2. it shows a one-time code and a link to the /redeem page")
	ui.Line("  3. enter the code there — it shows your token exactly once, so copy it")
	ui.Line("  4. run %s", ui.Cyan("hop license install <token>"))
	ui.Blank()
	ui.Hint("check progress any time with `hop license status %s`", res.RequestID)
	return nil
}

func printLicenseInfo(a *App) {
	ui.Blank()
	ui.Line("%s", ui.Bold("hop requires prior consent to use"))
	ui.Blank()
	ui.Line("  Government entities are exempt from this requirement — everyone else")
	ui.Line("  needs a signed consent token before hop will run. See LICENSE in the")
	ui.Line("  source repository for the exact terms.")
	ui.Blank()
	if core.LicenseServiceURL() != "" {
		ui.Line("  %s", ui.Bold("To request one yourself:"))
		ui.Line("    %s", ui.Cyan("hop license accept"))
		ui.Blank()
	}
	ui.Line("  %s", ui.Bold("Or, if you already have a token:"))
	ui.Line("    %s", ui.Cyan("hop license install <token>"))
	ui.Blank()
	if _, err := core.LoadLicenseToken(a.Layout); err == nil {
		ui.Ok("a valid token is already installed — run `hop license status` for details")
	}
}
