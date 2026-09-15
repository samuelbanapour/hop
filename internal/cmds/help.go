package cmds

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/samuelbanapour/hopcli/internal/core"
	"github.com/samuelbanapour/hopcli/internal/ui"
)

func init() {
	register(&Command{
		Name:    "version",
		Aliases: []string{"--version"},
		Group:   "Shell",
		Usage:   "hop version",
		Short:   "Show hop's version",
		Run: func(a *App, args []string) error {
			if a.JSON {
				return a.emitJSON(map[string]string{
					"version": core.Version, "revision": core.Revision,
					"platform": string(core.CurrentPlatform()), "go": runtime.Version(),
					"root": a.Layout.Root,
				})
			}
			printVersion(a.Verbose)
			return nil
		},
	})

	register(&Command{
		Name:  "shellenv",
		Group: "Shell",
		Usage: `eval "$(hop shellenv)"`,
		Short: "Print the shell commands that put hop on PATH",
		Long: `Print shell commands that add hop to your environment.

Add this to your shell profile:

    eval "$(hop shellenv)"

It prepends <root>/current/bin to PATH. Because that path goes through the
"current" symlink, it never changes as you install, upgrade or roll back —
your shell config is written once and stays correct.`,
		Run: runShellenv,
	})

	register(&Command{
		Name:  "completions",
		Group: "Shell",
		Usage: "hop completions <bash|zsh|fish>",
		Short: "Print a shell completion script",
		Run:   runCompletions,
	})
}

func printVersion(verbose bool) {
	ui.Line("%s %s", ui.Bold("hop"), core.Version)
	if verbose {
		ui.KV(
			[2]string{"revision", core.Revision},
			[2]string{"platform", string(core.CurrentPlatform()) + " (" + core.CurrentPlatform().Pretty() + ")"},
			[2]string{"go", runtime.Version()},
		)
	}
}

func runShellenv(a *App, args []string) error {
	shell := filepath.Base(os.Getenv("SHELL"))
	if len(args) == 1 {
		shell = args[0]
	} else if len(args) > 1 {
		return usagef("shellenv takes at most one shell name")
	}

	bin := a.Layout.CurrentBin()
	man := filepath.Join(a.Layout.Current(), "share", "man")

	// hop's own binary is not necessarily anywhere Layout manages — the
	// install script drops it at <root>/bin, a plain sibling of
	// store/profiles/current that nothing else here ever creates or
	// writes to, specifically so it can be found and put on PATH here
	// too. Without this, shellenv only ever exposed *installed packages*
	// (current/bin): someone who installed via the script needed hop's
	// full path just to run this command in the first place, and this
	// would never fix that for next time — confirmed for real, it just
	// silently left `hop` itself unreachable by name forever.
	pathDirs := []string{bin}
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		if selfDir := filepath.Dir(exe); selfDir != bin {
			pathDirs = append(pathDirs, selfDir)
		}
	}

	// Output goes to stdout unadorned: it is meant to be eval'd, so a stray
	// decoration would end up executed.
	switch shell {
	case "fish":
		fmt.Printf("set -gx HOP_ROOT %s;\n", shellQuote(a.Layout.Root))
		fmt.Printf("fish_add_path -gP %s;\n", strings.Join(shellQuoteAll(pathDirs), " "))
		fmt.Printf("set -q MANPATH; or set -gx MANPATH '';\n")
		fmt.Printf("set -gx MANPATH %s $MANPATH;\n", shellQuote(man))
	case "csh", "tcsh":
		fmt.Printf("setenv HOP_ROOT %s;\n", shellQuote(a.Layout.Root))
		fmt.Printf("setenv PATH %s:$PATH;\n", strings.Join(shellQuoteAll(pathDirs), ":"))
	default: // bash, zsh, sh, ksh
		fmt.Printf("export HOP_ROOT=%s;\n", shellQuote(a.Layout.Root))
		fmt.Printf("export PATH=%s:$PATH;\n", strings.Join(shellQuoteAll(pathDirs), ":"))
		fmt.Printf("export MANPATH=%s:${MANPATH:-};\n", shellQuote(man))
	}
	return nil
}

// shellQuote single-quotes a path safely for POSIX shells.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellQuoteAll applies shellQuote to every element.
func shellQuoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = shellQuote(s)
	}
	return out
}

func runCompletions(a *App, args []string) error {
	if len(args) != 1 {
		return usagef("completions needs a shell: bash, zsh or fish")
	}

	var names []string
	for _, c := range registry {
		names = append(names, c.Name)
	}
	list := strings.Join(names, " ")

	switch args[0] {
	case "bash":
		fmt.Printf(bashCompletion, list)
	case "zsh":
		fmt.Printf(zshCompletion, zshCommandDescriptions())
	case "fish":
		fmt.Print(fishCompletion())
	default:
		return usagef("unsupported shell %q (try bash, zsh or fish)", args[0])
	}
	return nil
}

// Completions call back into hop for package names, so they are always in
// step with the index rather than baked in at generation time.
const bashCompletion = `# hop completions for bash. Add to your profile:
#   eval "$(hop completions bash)"
_hop() {
  local cur prev cmds
  cur="${COMP_WORDS[COMP_CWORD]}"
  prev="${COMP_WORDS[COMP_CWORD-1]}"
  cmds="%s"

  if [ "$COMP_CWORD" -eq 1 ]; then
    COMPREPLY=( $(compgen -W "$cmds" -- "$cur") )
    return
  fi

  case "${COMP_WORDS[1]}" in
    install|i|info|show|search|s|deps|add)
      COMPREPLY=( $(compgen -W "$(hop __complete-packages 2>/dev/null)" -- "$cur") ) ;;
    remove|rm|uninstall|upgrade|up|reinstall)
      COMPREPLY=( $(compgen -W "$(hop __complete-installed 2>/dev/null)" -- "$cur") ) ;;
    rollback|undo)
      COMPREPLY=( $(compgen -W "$(hop __complete-generations 2>/dev/null)" -- "$cur") ) ;;
    completions)
      COMPREPLY=( $(compgen -W "bash zsh fish" -- "$cur") ) ;;
    *)
      COMPREPLY=( $(compgen -W "--help --dry-run --yes --json --quiet --verbose" -- "$cur") ) ;;
  esac
}
complete -F _hop hop
`

const zshCompletion = `# hop completions for zsh. Add to your profile:
#   eval "$(hop completions zsh)"
_hop() {
  local -a commands
  commands=(
%s
  )

  _arguments -C \
    '1:command:->cmds' \
    '*::arg:->args' \
    '--help[show help]' \
    '--dry-run[show what would happen]' \
    '--yes[do not prompt]' \
    '--json[machine-readable output]'

  case "$state" in
    cmds) _describe -t commands 'hop command' commands ;;
    args)
      case "$words[1]" in
        install|i|info|show|search|s|deps|add)
          local -a pkgs; pkgs=(${(f)"$(hop __complete-packages 2>/dev/null)"})
          _describe -t packages 'package' pkgs ;;
        remove|rm|uninstall|upgrade|up|reinstall)
          local -a pkgs; pkgs=(${(f)"$(hop __complete-installed 2>/dev/null)"})
          _describe -t packages 'installed package' pkgs ;;
        rollback|undo)
          local -a gens; gens=(${(f)"$(hop __complete-generations 2>/dev/null)"})
          _describe -t generations 'generation' gens ;;
        completions) _values 'shell' bash zsh fish ;;
      esac ;;
  esac
}
compdef _hop hop
`

func zshCommandDescriptions() string {
	var b strings.Builder
	for _, c := range registry {
		if strings.HasPrefix(c.Name, "__") {
			continue
		}
		b.WriteString(fmt.Sprintf("    '%s:%s'\n", c.Name, strings.ReplaceAll(c.Short, "'", "")))
	}
	return strings.TrimRight(b.String(), "\n")
}

func fishCompletion() string {
	var b strings.Builder
	b.WriteString("# hop completions for fish. Add to your config:\n")
	b.WriteString("#   hop completions fish > ~/.config/fish/completions/hop.fish\n")
	b.WriteString("complete -c hop -f\n")
	for _, c := range registry {
		if strings.HasPrefix(c.Name, "__") {
			continue
		}
		b.WriteString(fmt.Sprintf("complete -c hop -n '__fish_use_subcommand' -a %s -d %q\n",
			c.Name, c.Short))
	}
	b.WriteString("complete -c hop -n '__fish_seen_subcommand_from install info search deps add' " +
		"-a '(hop __complete-packages)'\n")
	b.WriteString("complete -c hop -n '__fish_seen_subcommand_from remove rm uninstall upgrade reinstall' " +
		"-a '(hop __complete-installed)'\n")
	b.WriteString("complete -c hop -n '__fish_seen_subcommand_from rollback undo' " +
		"-a '(hop __complete-generations)'\n")
	return b.String()
}

// ------------------------------------------------- completion data commands ----

func init() {
	register(&Command{
		Name:  "__complete-packages",
		Usage: "hop __complete-packages",
		Short: "",
		Run: func(a *App, args []string) error {
			ix, err := a.Index_()
			if err != nil {
				return nil // completion must never print an error
			}
			for _, r := range ix.Recipes {
				fmt.Println(r.Name)
			}
			return nil
		},
	})
	register(&Command{
		Name:  "__complete-installed",
		Usage: "hop __complete-installed",
		Short: "",
		Run: func(a *App, args []string) error {
			gen, err := a.Current()
			if err != nil || gen == nil {
				return nil
			}
			for _, n := range gen.Names() {
				fmt.Println(n)
			}
			return nil
		},
	})
	register(&Command{
		Name:  "__complete-generations",
		Usage: "hop __complete-generations",
		Short: "",
		Run: func(a *App, args []string) error {
			gens, err := core.Generations(a.Layout)
			if err != nil {
				return nil
			}
			for i := len(gens) - 1; i >= 0; i-- {
				fmt.Println(gens[i].ID)
			}
			return nil
		},
	})
}

// -------------------------------------------------------------------- help ----

// groupOrder is the order help sections appear in.
var groupOrder = []string{"Packages", "Inspect", "Projects", "History", "Maintenance", "Shell"}

func printRootHelp() {
	ui.Blank()
	ui.Line("%s  %s", ui.Bold(ui.Cyan("hop")), ui.Grey("a fast, atomic package manager for the terminal"))
	ui.Blank()
	ui.Line("%s", ui.Bold("USAGE"))
	ui.Line("  hop <command> [flags] [arguments]")
	ui.Blank()

	byGroup := map[string][]*Command{}
	for _, c := range registry {
		if strings.HasPrefix(c.Name, "__") || c.Short == "" {
			continue
		}
		g := c.Group
		if g == "" {
			g = "Other"
		}
		byGroup[g] = append(byGroup[g], c)
	}

	for _, g := range groupOrder {
		cmds := byGroup[g]
		if len(cmds) == 0 {
			continue
		}
		ui.Line("%s", ui.Bold(strings.ToUpper(g)))
		t := ui.NewTable()
		for _, c := range cmds {
			t.Row("  "+ui.Cyan(c.Name), ui.Grey(c.Short))
		}
		t.Render()
		ui.Blank()
	}

	ui.Line("%s", ui.Bold("GLOBAL FLAGS"))
	t := ui.NewTable()
	for _, f := range globalFlags {
		t.Row("  "+flagLabel(f), ui.Grey(f.Help))
	}
	t.Render()
	ui.Blank()

	ui.Line("%s", ui.Bold("EXAMPLES"))
	for _, ex := range [][2]string{
		{"hop install ripgrep fd bat", "install three tools in parallel"},
		{"hop upgrade --dry-run", "see what an upgrade would change"},
		{"hop rollback", "undo the last change instantly"},
		{"hop migrate", "take over what Homebrew installed"},
		{"hop sync", "match this machine to hopfile.toml"},
	} {
		ui.Line("  %s", ui.Cyan(ex[0]))
		ui.Line("      %s", ui.Grey(ex[1]))
	}
	ui.Blank()
	ui.Line("%s", ui.Grey("run `hop help <command>` for detail on any command"))
	ui.Blank()
}

func flagLabel(f Flag) string {
	s := "--" + f.Long
	if f.Short != "" {
		s = "-" + f.Short + ", " + s
	} else {
		s = "    " + s
	}
	if f.Kind != 'b' {
		arg := f.Arg
		if arg == "" {
			arg = "VALUE"
		}
		s += " " + arg
	}
	return s
}

func printCommandHelp(c *Command) {
	ui.Blank()
	ui.Line("%s  %s", ui.Bold(ui.Cyan("hop "+c.Name)), ui.Grey(c.Short))
	ui.Blank()
	ui.Line("%s", ui.Bold("USAGE"))
	ui.Line("  %s", c.Usage)
	ui.Blank()

	if c.Long != "" {
		ui.Line("%s", ui.Indent(c.Long, 2))
		ui.Blank()
	}

	if len(c.Aliases) > 0 {
		ui.Line("%s", ui.Bold("ALIASES"))
		ui.Line("  %s", strings.Join(c.Aliases, ", "))
		ui.Blank()
	}

	if len(c.Flags) > 0 {
		ui.Line("%s", ui.Bold("FLAGS"))
		t := ui.NewTable()
		for _, f := range c.Flags {
			t.Row("  "+flagLabel(f), ui.Grey(f.Help))
		}
		t.Render()
		ui.Blank()
	}

	ui.Line("%s", ui.Bold("GLOBAL FLAGS"))
	t := ui.NewTable()
	for _, f := range globalFlags {
		t.Row("  "+flagLabel(f), ui.Grey(f.Help))
	}
	t.Render()
	ui.Blank()
}
