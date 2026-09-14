package cmds

import (
	"fmt"
	"sort"
	"strings"

	"github.com/samuelbanapour/hopcli/internal/core"
	"github.com/samuelbanapour/hopcli/internal/ui"
)

// verbColour gives each action a consistent colour across every command, so
// "remove" always reads red and "install" always reads green.
func verbColour(a core.Action) func(string) string {
	switch a {
	case core.ActionInstall:
		return ui.Green
	case core.ActionUpgrade:
		return ui.Cyan
	case core.ActionDowngrade:
		return ui.Yellow
	case core.ActionReinstall:
		return ui.Blue
	case core.ActionRemove:
		return ui.Red
	default:
		return ui.Grey
	}
}

// renderPlan prints the transaction hop is about to perform. Every mutating
// command shows this first: the plan that prints is exactly the plan that
// runs, which is what makes --dry-run worth trusting.
func renderPlan(p *core.Plan, title string) {
	if p.Empty() {
		return
	}

	ui.Step("%s", title)
	ui.Blank()

	t := ui.NewTable()
	for _, s := range p.Steps {
		verb := verbColour(s.Action)(s.Action.String())

		version := s.Version
		switch s.Action {
		case core.ActionUpgrade, core.ActionDowngrade:
			version = fmt.Sprintf("%s %s %s", ui.Grey(s.From), ui.Arrow(), ui.Bold(s.Version))
		case core.ActionRemove:
			version = ui.Grey(s.From)
		default:
			version = ui.Bold(s.Version)
		}

		size := ""
		if s.Artifact != nil && s.Artifact.Size > 0 {
			size = ui.Grey(ui.Bytes(s.Artifact.Size))
		}

		note := ""
		switch {
		case s.Rosetta && s.Recipe != nil && s.Recipe.Kind == core.KindImage:
			note = ui.Yellow("x86_64 build") // a static image is never "run under Rosetta"
		case s.Rosetta:
			note = ui.Yellow("via Rosetta")
		case s.Reason == "requested", s.Reason == "":
		case strings.HasPrefix(s.Reason, "dependency of "):
			note = ui.Grey("dep of " + strings.TrimPrefix(s.Reason, "dependency of "))
		default:
			note = ui.Grey(s.Reason)
		}

		t.Row("  "+verb, ui.Pkg(s.Name), version, size, note)
	}
	t.Render()
	ui.Blank()

	// One-line summary: what changes, and what it costs.
	counts := p.Counts()
	var parts []string
	for _, a := range []core.Action{core.ActionInstall, core.ActionUpgrade, core.ActionDowngrade, core.ActionReinstall, core.ActionRemove} {
		if n := counts[a]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d to %s", n, a))
		}
	}
	summary := strings.Join(parts, ", ")
	if b := p.DownloadBytes(); b > 0 {
		summary += ui.Grey(fmt.Sprintf("  ·  %s to download", ui.Bytes(b)))
	}
	ui.Line("  %s", summary)

	for _, w := range p.Warnings {
		ui.Warn("%s", w)
	}
	if len(p.Warnings) > 0 {
		ui.Blank()
	}
}

// renderResult prints what a completed transaction did.
func renderResult(a *App, res *core.ApplyResult, gen *core.Generation) {
	ui.Blank()
	ui.Step("Generation %d activated", gen.ID)
	ui.Blank()

	if len(res.Added) > 0 || len(res.Changed) > 0 {
		t := ui.NewTable()
		for _, p := range append(append([]core.Installed{}, res.Added...), res.Changed...) {
			note := ""
			switch p.Kind {
			case core.KindImage:
				note = ui.Yellow("image") + ui.Grey(" · "+ui.Bytes(p.Size))
			case core.KindApp:
				note = ui.Yellow("app") + ui.Grey(" · ~/Applications/"+p.App)
			default:
				cmds := make([]string, 0, len(p.Bins))
				for _, b := range p.Bins {
					cmds = append(cmds, b.Name)
				}
				sort.Strings(cmds)
				note = ui.Grey(strings.Join(cmds, ", "))
			}
			t.Row("  "+ui.Green("✓"), ui.PkgVer(p.Name, p.Version), note)
		}
		t.Render()
	}
	for _, name := range res.Removed {
		ui.Line("  %s %s", ui.Red("−"), ui.Pkg(name))
	}
	ui.Blank()

	// Timing and transfer, the numbers that justify the parallel downloader.
	var bits []string
	if n := len(res.Added); n > 0 {
		bits = append(bits, fmt.Sprintf("installed %d", n))
	}
	if n := len(res.Changed); n > 0 {
		bits = append(bits, fmt.Sprintf("changed %d", n))
	}
	if n := len(res.Removed); n > 0 {
		bits = append(bits, fmt.Sprintf("removed %d", n))
	}
	line := strings.Join(bits, ", ")
	if line == "" {
		line = "no changes"
	}
	line += fmt.Sprintf(" in %s", ui.Duration(res.Duration))
	if res.Bytes > 0 {
		line += ui.Grey(fmt.Sprintf("  ·  %s downloaded", ui.Bytes(res.Bytes)))
	}
	if res.Reused > 0 {
		line += ui.Grey(fmt.Sprintf("  ·  %s from cache", ui.Count(res.Reused, "artifact", "artifacts")))
	}
	ui.Ok("%s", line)

	// Digests recorded on first use deserve an explicit note: the user should
	// know which packages were not checksum-verified in advance.
	if len(res.Recorded) > 0 {
		ui.Blank()
		names := make([]string, 0, len(res.Recorded))
		for n := range res.Recorded {
			names = append(names, n)
		}
		sort.Strings(names)
		ui.Warn("recorded a checksum on first use for %s", ui.List(names, "and"))
		ui.Hint("future installs of these versions are verified against it")
	}

	for _, c := range res.Conflicts {
		ui.Warn("%s is provided by both %s and %s; %s wins on PATH",
			ui.Bold(c.Bin), c.Winner, c.Loser, c.Winner)
	}

	if len(res.Caveats) > 0 {
		names := make([]string, 0, len(res.Caveats))
		for n := range res.Caveats {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			ui.Blank()
			ui.Step("Caveats for %s", n)
			ui.Line("%s", ui.Indent(res.Caveats[n], 4))
		}
	}

	// Only nag about PATH if something added actually needs to be on it —
	// an image-only install has nothing to run.
	addedACommand := false
	for _, p := range res.Added {
		if len(p.Bins) > 0 {
			addedACommand = true
			break
		}
	}
	if addedACommand {
		warnPathIfNeeded(a)
	}
}

// planJSON is the --json shape of a plan, kept stable for scripts.
type planJSON struct {
	DryRun    bool           `json:"dry_run"`
	Steps     []planStepJSON `json:"steps"`
	Downloads int            `json:"downloads"`
	Bytes     int64          `json:"download_bytes"`
	Warnings  []string       `json:"warnings,omitempty"`
}

type planStepJSON struct {
	Action   string `json:"action"`
	Name     string `json:"name"`
	Version  string `json:"version,omitempty"`
	From     string `json:"from,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Platform string `json:"platform,omitempty"`
}

func toPlanJSON(p *core.Plan, dryRun bool) planJSON {
	out := planJSON{DryRun: dryRun, Warnings: p.Warnings, Bytes: p.DownloadBytes()}
	for _, s := range p.Steps {
		j := planStepJSON{
			Action: s.Action.String(), Name: s.Name, Version: s.Version,
			From: s.From, Reason: s.Reason, Platform: string(s.Platform),
		}
		if s.Artifact != nil {
			j.Size = s.Artifact.Size
		}
		out.Steps = append(out.Steps, j)
	}
	out.Downloads = len(p.Downloads())
	return out
}

// resultJSON is the --json shape of a completed transaction.
type resultJSON struct {
	Generation int               `json:"generation"`
	Added      []string          `json:"added,omitempty"`
	Changed    []string          `json:"changed,omitempty"`
	Removed    []string          `json:"removed,omitempty"`
	Bytes      int64             `json:"downloaded_bytes"`
	Cached     int               `json:"cached_artifacts"`
	DurationMS int64             `json:"duration_ms"`
	Recorded   map[string]string `json:"recorded_checksums,omitempty"`
}

func toResultJSON(res *core.ApplyResult) resultJSON {
	j := resultJSON{
		Generation: res.Generation.ID,
		Removed:    res.Removed,
		Bytes:      res.Bytes,
		Cached:     res.Reused,
		DurationMS: res.Duration.Milliseconds(),
	}
	if len(res.Recorded) > 0 {
		j.Recorded = res.Recorded
	}
	for _, p := range res.Added {
		j.Added = append(j.Added, p.Name+"@"+p.Version)
	}
	for _, p := range res.Changed {
		j.Changed = append(j.Changed, p.Name+"@"+p.Version)
	}
	return j
}
