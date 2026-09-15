package cmds

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/samuelbanapour/hopcli/internal/ui"
)

func init() {
	register(&Command{
		Name:    "menu",
		Aliases: []string{"interactive"},
		Group:   "General",
		Usage:   "hop menu",
		Short:   "Pick what to do from a menu instead of typing a command",
		Long: `Browse hop's commands by choice instead of by name.

Everything you can do by typing a full command (` + "`hop install ripgrep`" + `)
you can also reach here by picking it from a numbered list — useful if you
don't remember a command's exact name, or just prefer arrow-key-free menus
over memorizing flags. It runs the exact same code either way: the menu just
builds the command line for you and hands it to hop like you'd typed it
yourself.

A bare ` + "`hop`" + ` with no arguments opens this menu automatically when
run at a real terminal. Anywhere hop's output isn't going to a person
(scripts, pipes, CI) that still prints plain help text instead, since there
would be nobody there to answer a prompt.`,
		Run: runMenu,
	})
}

// menuGroupOrder mirrors groupOrder but drops "Shell", whose commands
// (shellenv, completions, version) are plumbing meant for scripts and shell
// profiles, not something you'd browse to from a menu.
var menuGroupOrder = []string{"Packages", "Inspect", "Projects", "History", "Maintenance"}

func runMenu(a *App, _ []string) error {
	if !ui.Interactive() {
		ui.Info("the menu needs a real terminal on both stdin and stdout")
		ui.Hint("run `hop help` to see every command directly")
		return nil
	}

	in := bufio.NewReader(os.Stdin)

	for {
		ui.Blank()
		ui.Line("%s", ui.Bold("hop — pick a group, or 'q' to quit"))
		ui.Blank()
		groups := menuGroupsInUse()
		for i, g := range groups {
			ui.Line("  %s  %s", ui.Cyan(fmt.Sprintf("%d)", i+1)), g)
		}
		ui.Line("  %s  %s", ui.Cyan("c)"), "run any command directly (advanced)")
		ui.Blank()

		choice, ok := readLine(in, "> ")
		if !ok || isQuit(choice) {
			return nil
		}
		choice = strings.TrimSpace(choice)
		if choice == "" {
			continue
		}

		if strings.EqualFold(choice, "c") {
			runRawCommand(in)
			continue
		}

		idx, ok := parseChoice(choice, len(groups))
		if !ok {
			ui.Warn("type a number from the list, 'c', or 'q'")
			continue
		}

		if !runGroupMenu(in, groups[idx]) {
			return nil
		}
	}
}

// menuGroupsInUse returns menuGroupOrder filtered to groups that actually
// have at least one command registered, so the menu never shows an empty
// section.
func menuGroupsInUse() []string {
	have := map[string]bool{}
	for _, c := range registry {
		have[c.Group] = true
	}
	var out []string
	for _, g := range menuGroupOrder {
		if have[g] {
			out = append(out, g)
		}
	}
	return out
}

// runGroupMenu lists the commands in one group and runs whichever the user
// picks. Returns false if the user asked to quit hop entirely (rather than
// just backing out of this submenu).
func runGroupMenu(in *bufio.Reader, group string) bool {
	var cmds []*Command
	for _, c := range registry {
		if c.Group == group && !strings.HasPrefix(c.Name, "__") {
			cmds = append(cmds, c)
		}
	}

	for {
		ui.Blank()
		ui.Line("%s", ui.Bold(group))
		ui.Blank()
		for i, c := range cmds {
			ui.Line("  %s  %-14s %s", ui.Cyan(fmt.Sprintf("%d)", i+1)), c.Name, ui.Grey(c.Short))
		}
		ui.Line("  %s  %s", ui.Cyan("b)"), "back")
		ui.Blank()

		choice, ok := readLine(in, "> ")
		if !ok || isQuit(choice) {
			return false
		}
		choice = strings.TrimSpace(choice)
		if choice == "" {
			continue
		}
		if strings.EqualFold(choice, "b") {
			return true
		}

		idx, ok := parseChoice(choice, len(cmds))
		if !ok {
			ui.Warn("type a number from the list, 'b', or 'q'")
			continue
		}

		runChosenCommand(in, cmds[idx])
	}
}

// runChosenCommand shows a command's usage, asks for any arguments and
// flags on one line, and dispatches it through runArgv exactly as if it had
// been typed on the real command line.
func runChosenCommand(in *bufio.Reader, c *Command) {
	ui.Blank()
	ui.Line("%s  %s", ui.Bold(c.Name), ui.Grey(c.Short))
	ui.Line("%s", ui.Grey("usage: "+c.Usage))

	line, ok := readLine(in, "arguments (blank for none, 'b' to cancel): ")
	if !ok {
		return
	}
	line = strings.TrimSpace(line)
	if strings.EqualFold(line, "b") {
		return
	}

	argv := append([]string{c.Name}, splitArgs(line)...)
	ui.Blank()
	runArgv(argv)
	readLine(in, "press Enter to continue... ")
}

// runRawCommand is the menu's escape hatch: type a full command line, same
// as at a shell prompt, without leaving the menu.
func runRawCommand(in *bufio.Reader) {
	line, ok := readLine(in, "hop ")
	if !ok {
		return
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	argv := splitArgs(line)
	if len(argv) == 0 {
		return
	}
	ui.Blank()
	runArgv(argv)
	readLine(in, "press Enter to continue... ")
}

// readLine prints prompt and reads one line of input. ok is false on EOF
// (Ctrl-D) or a read error, which the caller treats as "quit".
func readLine(in *bufio.Reader, prompt string) (string, bool) {
	ui.Prompt("%s", prompt)
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		ui.Blank()
		return "", false
	}
	return strings.TrimRight(line, "\r\n"), true
}

func isQuit(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "q" || s == "quit" || s == "exit"
}

// parseChoice turns a typed number into a zero-based index, accepting only
// values that are actually on the list shown.
func parseChoice(s string, n int) (int, bool) {
	var i int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &i); err != nil {
		return 0, false
	}
	if i < 1 || i > n {
		return 0, false
	}
	return i - 1, true
}

// splitArgs tokenizes a typed line the way a shell would for hop's purposes:
// whitespace-separated, with '...' and "..." kept as single arguments so a
// path or name with spaces can still be typed. No escapes, no globbing —
// this is a menu prompt, not a shell.
func splitArgs(line string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	inField := false

	flush := func() {
		if inField {
			out = append(out, cur.String())
			cur.Reset()
			inField = false
		}
	}

	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inField = true
		case r == ' ' || r == '\t':
			flush()
		default:
			inField = true
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}
