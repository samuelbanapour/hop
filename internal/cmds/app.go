// Package cmds implements hop's command-line surface: flag parsing, command
// dispatch, and the human- and machine-readable rendering of results.
package cmds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/samuelbanapour/hopcli/internal/core"
	"github.com/samuelbanapour/hopcli/internal/ui"
)

// Exit codes. Distinguishing usage errors from failures lets scripts react
// differently to "you typed it wrong" and "the network is down".
const (
	ExitOK       = 0
	ExitError    = 1
	ExitUsage    = 2
	ExitInterupt = 130
)

// App is one invocation's resolved configuration and lazily-loaded state.
type App struct {
	Layout *core.Layout
	Client *http.Client
	Ctx    context.Context

	// Global options.
	Root    string
	Jobs    int
	DryRun  bool
	Yes     bool
	JSON    bool
	Quiet   bool
	Verbose bool
	NoColor bool
	Force   bool
	Help    bool

	// Command-scoped options.
	Keep        int
	All         bool
	Explicit    bool
	KeepOrphans bool
	NoPrune     bool
	Locked      bool
	Limit       int

	// Raw command line, recorded in the generation for `hop history`.
	CommandLine string

	// Every parsed option, so command-scoped flags without a dedicated field
	// are still reachable via boolFlag / intFlag / strFlag.
	extraBools map[string]bool
	extraInts  map[string]int
	extraStrs  map[string]string

	index *core.Index
}

// intFlag reads a command-scoped integer option.
func (a *App) intFlag(name string) int { return a.extraInts[name] }

// strFlag reads a command-scoped string option.
func (a *App) strFlag(name string) string { return a.extraStrs[name] }

// UsageError signals a misuse that should print usage and exit 2.
type UsageError struct{ msg string }

func (e *UsageError) Error() string { return e.msg }

func usagef(format string, a ...any) error {
	return &UsageError{msg: fmt.Sprintf(format, a...)}
}

// Index loads the recipe index on first use, so commands that never consult
// it (rollback, generations, gc) do no parsing work at all.
func (a *App) Index_() (*core.Index, error) {
	if a.index != nil {
		return a.index, nil
	}
	ix, err := core.LoadIndex(a.Layout)
	if err != nil {
		return nil, err
	}
	a.index = ix
	return ix, nil
}

// Current loads the active generation; a nil result means nothing installed.
func (a *App) Current() (*core.Generation, error) { return core.Current(a.Layout) }

// lock takes the root lock for a mutating command.
func (a *App) lock() (*core.Guard, error) { return core.Acquire(a.Layout, 20*time.Second) }

// emitJSON writes v as JSON for --json consumers.
func (a *App) emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// confirm asks for approval unless --yes was given. In --dry-run nothing is
// ever confirmed because nothing will happen.
func (a *App) confirm(prompt string) bool {
	if a.Yes || a.JSON {
		return true
	}
	return ui.Confirm(prompt, true)
}

// sink adapts ui.Progress to the core.Sink interface, keeping the engine free
// of any terminal dependency.
type sink struct{ p *ui.Progress }

func (s sink) Bar(label string, total int64) core.BarHandle { return s.p.Bar(label, total) }
func (s sink) Log(format string, a ...any)                  { s.p.Log(format, a...) }

// ------------------------------------------------------------------ command ----

// Flag describes one option a command accepts.
type Flag struct {
	Long  string
	Short string
	Kind  byte // 'b' bool, 'i' int, 's' string
	Help  string
	Arg   string // placeholder for non-bool flags
}

// Command is one hop subcommand.
type Command struct {
	Name    string
	Aliases []string
	Usage   string
	Short   string
	Long    string
	Group   string
	Flags   []Flag
	Run     func(*App, []string) error
}

// globalFlags apply to every command.
var globalFlags = []Flag{
	{Long: "help", Short: "h", Kind: 'b', Help: "show help for this command"},
	{Long: "yes", Short: "y", Kind: 'b', Help: "assume yes; do not prompt"},
	{Long: "dry-run", Short: "n", Kind: 'b', Help: "show what would happen, change nothing"},
	{Long: "json", Kind: 'b', Help: "emit machine-readable JSON"},
	{Long: "quiet", Short: "q", Kind: 'b', Help: "suppress non-error output"},
	{Long: "verbose", Short: "v", Kind: 'b', Help: "show extra detail"},
	{Long: "no-color", Kind: 'b', Help: "disable colour output"},
	{Long: "jobs", Short: "j", Kind: 'i', Arg: "N", Help: "parallel downloads (default: CPU count, max 8)"},
	{Long: "root", Kind: 's', Arg: "DIR", Help: "hop prefix (default: $HOP_ROOT or ~/.hop)"},
}

// registry is every command, in the order help lists them.
var registry []*Command

func register(c *Command) { registry = append(registry, c) }

// lookup finds a command by name or alias.
func lookup(name string) (*Command, bool) {
	for _, c := range registry {
		if c.Name == name {
			return c, true
		}
		for _, al := range c.Aliases {
			if al == name {
				return c, true
			}
		}
	}
	return nil, false
}

// suggestCommand offers the closest command name for a typo.
func suggestCommand(name string) []string {
	type cand struct {
		n string
		d int
	}
	var cs []cand
	for _, c := range registry {
		for _, n := range append([]string{c.Name}, c.Aliases...) {
			if d := levenshtein(name, n); d <= 3 || strings.HasPrefix(n, name) {
				cs = append(cs, cand{c.Name, d})
				break
			}
		}
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].d < cs[j].d })
	var out []string
	for i, c := range cs {
		if i >= 3 {
			break
		}
		out = append(out, c.n)
	}
	return out
}

func levenshtein(a, b string) int {
	ar, br := []rune(strings.ToLower(a)), []rune(strings.ToLower(b))
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			m := prev[j-1] + cost
			if v := cur[j-1] + 1; v < m {
				m = v
			}
			if v := prev[j] + 1; v < m {
				m = v
			}
			cur[j] = m
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

// ------------------------------------------------------------------- parsing ----

// parsed is the result of splitting a command line.
type parsed struct {
	bools map[string]bool
	ints  map[string]int
	strs  map[string]string
	args  []string
}

// parseFlags splits args into options and positionals, validating every option
// against the allowed set. An unknown flag is a usage error with a suggestion,
// never silently ignored.
func parseFlags(allowed []Flag, args []string) (*parsed, error) {
	byLong := map[string]Flag{}
	byShort := map[string]Flag{}
	for _, f := range allowed {
		byLong[f.Long] = f
		if f.Short != "" {
			byShort[f.Short] = f
		}
	}

	p := &parsed{bools: map[string]bool{}, ints: map[string]int{}, strs: map[string]string{}}

	setValue := func(f Flag, raw string, hadValue bool, next func() (string, bool)) error {
		switch f.Kind {
		case 'b':
			if hadValue {
				switch strings.ToLower(raw) {
				case "true", "yes", "1":
					p.bools[f.Long] = true
				case "false", "no", "0":
					p.bools[f.Long] = false
				default:
					return usagef("--%s does not take a value", f.Long)
				}
				return nil
			}
			p.bools[f.Long] = true
			return nil
		case 'i':
			v := raw
			if !hadValue {
				var ok bool
				v, ok = next()
				if !ok {
					return usagef("--%s needs a number", f.Long)
				}
			}
			n := 0
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
				return usagef("--%s needs a number, got %q", f.Long, v)
			}
			p.ints[f.Long] = n
			return nil
		default:
			v := raw
			if !hadValue {
				var ok bool
				v, ok = next()
				if !ok {
					return usagef("--%s needs a value", f.Long)
				}
			}
			p.strs[f.Long] = v
			return nil
		}
	}

	i := 0
	next := func() (string, bool) {
		if i+1 < len(args) {
			i++
			return args[i], true
		}
		return "", false
	}

	for ; i < len(args); i++ {
		arg := args[i]

		switch {
		case arg == "--":
			p.args = append(p.args, args[i+1:]...)
			return p, nil

		case strings.HasPrefix(arg, "--"):
			body := arg[2:]
			name, val := body, ""
			hadValue := false
			if eq := strings.IndexByte(body, '='); eq >= 0 {
				name, val, hadValue = body[:eq], body[eq+1:], true
			}
			// Accept --no-<flag> for booleans, which reads naturally.
			if f, ok := byLong[name]; ok {
				if err := setValue(f, val, hadValue, next); err != nil {
					return nil, err
				}
				continue
			}
			if strings.HasPrefix(name, "no-") {
				if f, ok := byLong[name[3:]]; ok && f.Kind == 'b' {
					p.bools[f.Long] = false
					continue
				}
			}
			return nil, unknownFlag("--"+name, allowed)

		case len(arg) > 1 && arg[0] == '-':
			// A bundle of short flags, the last of which may take a value.
			body := arg[1:]
			for j := 0; j < len(body); j++ {
				ch := string(body[j])
				f, ok := byShort[ch]
				if !ok {
					return nil, unknownFlag("-"+ch, allowed)
				}
				if f.Kind == 'b' {
					p.bools[f.Long] = true
					continue
				}
				// Remainder of the bundle is the value, else take the next arg.
				rest := body[j+1:]
				rest = strings.TrimPrefix(rest, "=")
				if rest != "" {
					if err := setValue(f, rest, true, next); err != nil {
						return nil, err
					}
				} else if err := setValue(f, "", false, next); err != nil {
					return nil, err
				}
				break
			}

		default:
			p.args = append(p.args, arg)
		}
	}
	return p, nil
}

func unknownFlag(given string, allowed []Flag) error {
	best, bestD := "", 99
	for _, f := range allowed {
		if d := levenshtein(strings.TrimLeft(given, "-"), f.Long); d < bestD {
			best, bestD = "--"+f.Long, d
		}
	}
	if bestD <= 3 && best != "" {
		return usagef("unknown option %s (did you mean %s?)", given, best)
	}
	return usagef("unknown option %s", given)
}

// ------------------------------------------------------------------- running ----

// Main is the process entry point. It returns an exit code rather than
// calling os.Exit, so deferred cleanup always runs.
func Main(argv []string) int {
	// A bare `hop` is a request for help, not an error.
	if len(argv) == 0 {
		ui.Init(false, false, ui.Normal)
		printRootHelp()
		return ExitOK
	}

	// Global help and version work before any command is resolved.
	switch argv[0] {
	case "-h", "--help", "help":
		ui.Init(false, false, ui.Normal)
		if len(argv) > 1 {
			if c, ok := lookup(argv[1]); ok {
				printCommandHelp(c)
				return ExitOK
			}
			ui.Err("no command named %q", argv[1])
			return ExitUsage
		}
		printRootHelp()
		return ExitOK
	case "-V", "--version":
		ui.Init(false, false, ui.Normal)
		printVersion(false)
		return ExitOK
	}

	name := argv[0]
	cmd, ok := lookup(name)
	if !ok {
		ui.Init(false, false, ui.Normal)
		ui.Err("unknown command %q", name)
		if sug := suggestCommand(name); len(sug) > 0 {
			ui.Hint("did you mean %s?", ui.List(quoteAll(sug), "or"))
		}
		ui.Hint("run `hop help` to see every command")
		return ExitUsage
	}

	allowed := append(append([]Flag{}, globalFlags...), cmd.Flags...)
	p, err := parseFlags(allowed, argv[1:])
	if err != nil {
		ui.Init(false, false, ui.Normal)
		ui.Err("%v", err)
		ui.Hint("run `hop %s --help`", cmd.Name)
		return ExitUsage
	}

	app := &App{
		Client:      core.NewHTTPClient(),
		CommandLine: "hop " + strings.Join(argv, " "),
		Root:        p.strs["root"],
		Jobs:        p.ints["jobs"],
		DryRun:      p.bools["dry-run"],
		Yes:         p.bools["yes"],
		JSON:        p.bools["json"],
		Quiet:       p.bools["quiet"],
		Verbose:     p.bools["verbose"],
		NoColor:     p.bools["no-color"],
		Force:       p.bools["force"],
		Help:        p.bools["help"],
		All:         p.bools["all"],
		Explicit:    p.bools["explicit"],
		KeepOrphans: p.bools["keep-orphans"],
		Locked:      p.bools["locked"],
		Keep:        p.ints["keep"],
		Limit:       p.ints["limit"],
		extraBools:  p.bools,
		extraInts:   p.ints,
		extraStrs:   p.strs,
	}
	if v, set := p.bools["prune"]; set && !v {
		app.NoPrune = true
	}

	verbosity := ui.Normal
	switch {
	case app.Quiet:
		verbosity = ui.Quiet
	case app.Verbose:
		verbosity = ui.Verbose
	}
	// JSON output must never be polluted by progress rendering.
	if app.JSON {
		verbosity = ui.Quiet
	}
	ui.Init(app.NoColor || app.JSON, false, verbosity)

	if app.Help {
		printCommandHelp(cmd)
		return ExitOK
	}

	if app.Jobs <= 0 {
		app.Jobs = defaultJobs()
	}
	if app.Jobs > 32 {
		app.Jobs = 32
	}

	layout, err := core.NewLayout(app.Root)
	if err != nil {
		ui.Err("%v", err)
		return ExitError
	}
	app.Layout = layout

	// Started here, before the command's own work, so its network request
	// (bounded to updateCheckTimeout, and throttled to once a day) overlaps
	// with whatever the command does rather than adding to it — notifyUpdate
	// only ever costs whatever time is left over once the real command
	// finishes.
	notifyUpdate := maybeCheckForUpdate(app, cmd.Name)

	// Ctrl-C cancels the transaction. Because nothing is activated until every
	// download has been verified, an interrupt can never leave a broken tree.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app.Ctx = ctx

	err = cmd.Run(app, p.args)

	if err == nil {
		notifyUpdate()
		return ExitOK
	}

	var ue *UsageError
	switch {
	case errors.As(err, &ue):
		ui.Err("%v", err)
		ui.Hint("usage: %s", cmd.Usage)
		return ExitUsage
	case errors.Is(err, context.Canceled):
		ui.Blank()
		ui.Warn("interrupted; nothing was changed")
		return ExitInterupt
	default:
		reportError(app, err)
		return ExitError
	}
}

// reportError renders an engine error with whatever guidance fits it. Good
// error messages are most of what makes a package manager pleasant, so each
// known failure mode gets a tailored hint instead of a bare string.
func reportError(app *App, err error) {
	var unknown *core.UnknownPackageError
	var unsupported *core.UnsupportedPlatformError
	var cycle *core.CycleError
	var txn *core.TransactionError
	var mismatch *core.DigestMismatch
	var httpErr *core.HTTPError

	switch {
	case errors.As(err, &unknown):
		ui.Err("no package named %s", ui.Bold(unknown.Name))
		if len(unknown.Suggest) > 0 {
			ui.Hint("did you mean %s?", ui.List(quoteAll(unknown.Suggest), "or"))
		}
		ui.Hint("search the index with `hop search %s`", unknown.Name)

	case errors.As(err, &unsupported):
		ui.Err("%v", unsupported)
		ui.Hint("this machine is %s", core.CurrentPlatform().Pretty())
		if len(unsupported.Available) > 0 {
			ui.Hint("ask upstream for a build, or try HOP_PLATFORM=%s to test", unsupported.Available[0])
		}

	case errors.As(err, &cycle):
		ui.Err("%v", cycle)
		ui.Hint("this is a bug in the recipe index, not in your command")

	case errors.As(err, &mismatch):
		ui.Err("%v", mismatch)
		ui.Blank()
		ui.Warn("the bytes upstream served are not the bytes this recipe pins.")
		ui.Hint("nothing was installed. If upstream legitimately replaced the")
		ui.Hint("release, refresh the index with `hop update`.")

	case errors.As(err, &txn):
		ui.Err("transaction aborted; nothing was changed")
		for _, f := range txn.Failures {
			ui.Line("    %s  %v", ui.Pkg(f.Name), f.Err)
		}
		ui.Blank()
		ui.Info("Your environment is exactly as it was before this command.")

	case errors.As(err, &httpErr):
		ui.Err("%v", httpErr)
		ui.Hint("check your connection, or run `hop update` if the index is stale")

	default:
		ui.Err("%v", err)
	}
}

func defaultJobs() int {
	n := runtime.NumCPU()
	switch {
	case n < 2:
		return 2
	case n > 8:
		return 8 // more parallel downloads mostly just fight for bandwidth
	default:
		return n
	}
}

func quoteAll(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, ui.Bold(s))
	}
	return out
}

// onPath reports whether hop's bin directory is on the user's PATH, which is
// the single most common reason a fresh install "does not work".
func onPath(l *core.Layout) bool {
	want := filepath.Clean(l.CurrentBin())
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(p) == want {
			return true
		}
	}
	return false
}

// warnPathIfNeeded tells the user how to finish setup, once, after an install.
func warnPathIfNeeded(a *App) {
	if a.JSON || onPath(a.Layout) {
		return
	}
	ui.Blank()
	ui.Warn("%s is not on your PATH, so the commands above are not runnable yet.", a.Layout.CurrentBin())
	ui.Blank()
	ui.Line("  Add this to %s:", ui.Bold(shellProfile()))
	ui.Blank()
	ui.Line("    %s", ui.Cyan(`eval "$(hop shellenv)"`))
	ui.Blank()
	ui.Line("  Then restart your shell, or run it now in this one.")
}

// shellProfile guesses the profile file to edit from $SHELL.
func shellProfile() string {
	sh := filepath.Base(os.Getenv("SHELL"))
	switch sh {
	case "zsh":
		return "~/.zshrc"
	case "bash":
		if runtime.GOOS == "darwin" {
			return "~/.bash_profile"
		}
		return "~/.bashrc"
	case "fish":
		return "~/.config/fish/config.fish"
	default:
		return "your shell profile"
	}
}
