package cmds

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/samuelbanapour/hopcli/internal/core"
	"github.com/samuelbanapour/hopcli/internal/ui"
)

// welcomeMarker's presence means the banner has already run once. It lives
// in the cache, not the store, because it is throwaway: losing it just means
// the banner shows one extra time, which is harmless.
func welcomeMarker(l *core.Layout) string {
	return filepath.Join(l.Cache(), "welcomed")
}

// maybeShowWelcome prints hop's first-run banner exactly once per prefix.
// Skipped for JSON/quiet output so scripts and CI never see it.
func maybeShowWelcome(a *App) {
	if a.JSON || a.Quiet {
		return
	}
	marker := welcomeMarker(a.Layout)
	if _, err := os.Stat(marker); err == nil {
		return
	}
	printWelcomeBanner()
	_ = os.WriteFile(marker, []byte("1\n"), 0o644)
}

func printWelcomeBanner() {
	c := ui.Cyan
	b := ui.Bold
	g := ui.Grey

	ui.Blank()
	ui.Line("%s", c(`   __`))
	ui.Line("%s", c(`  / /_  ___  ____`)+"    "+b("hop")+" "+g(core.Version))
	ui.Line("%s", c(` / __ \/ _ \/ __ \`)+"   "+g("a fast, atomic package manager"))
	ui.Line("%s", c(`/ / / / (_) / /_/ /`))
	ui.Line("%s", c(`/_/ /_/\___/ .___/`)+"   "+g("welcome — this is your first run"))
	ui.Line("%s", c(`          /_/`))
	ui.Blank()
	tips := [][2]string{
		{"hop install <pkg>", "install something"},
		{"hop rollback", "undo the last change — instant, no download"},
		{"hop gc", "reclaim disk (aliases: prune, clean)"},
		{"hop alias <pkg>", "hot-swap a tool to a version you've had before"},
		{"hop --help", "see everything else"},
	}
	width := 0
	for _, t := range tips {
		if len(t[0]) > width {
			width = len(t[0])
		}
	}
	for _, t := range tips {
		ui.Line("  %s  %s", c(fmt.Sprintf("%-*s", width, t[0])), g(t[1]))
	}
	ui.Blank()
}
