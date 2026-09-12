// Package ui provides terminal output primitives: colour, symbols, tables and
// diagnostics. Every writer degrades gracefully when stdout is not a TTY, so
// `hop ... | cat` and CI logs stay readable.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Verbosity levels.
const (
	Quiet = iota - 1
	Normal
	Verbose
)

var (
	mu     sync.Mutex
	out    io.Writer = os.Stdout
	errOut io.Writer = os.Stderr

	colour  bool
	tty     bool
	unicode = true
	level   = Normal
	width   = 80
)

// Init configures the package from the detected environment and user flags.
// noColor and forceColor mirror --no-color / FORCE_COLOR.
func Init(noColor, forceColor bool, verbosity int) {
	mu.Lock()
	defer mu.Unlock()

	level = verbosity
	tty = isTerminal(os.Stdout)

	switch {
	case noColor, os.Getenv("NO_COLOR") != "":
		colour = false
	case forceColor, os.Getenv("FORCE_COLOR") != "":
		colour = true
	default:
		colour = tty && os.Getenv("TERM") != "dumb"
	}

	// exFAT/legacy terminals and LANG=C can mangle box-drawing glyphs.
	lang := os.Getenv("LC_ALL") + os.Getenv("LC_CTYPE") + os.Getenv("LANG")
	unicode = lang == "" || strings.Contains(strings.ToUpper(lang), "UTF")

	if w, _, err := terminalSize(os.Stdout); err == nil && w > 20 {
		width = w
	}
}

// IsTTY reports whether stdout is an interactive terminal. Callers use this to
// decide between live progress rendering and plain sequential logging.
func IsTTY() bool { mu.Lock(); defer mu.Unlock(); return tty }

// Width returns the usable terminal width.
func Width() int { mu.Lock(); defer mu.Unlock(); return width }

// SetOutput redirects normal and error output. Used by tests.
func SetOutput(o, e io.Writer) { mu.Lock(); defer mu.Unlock(); out, errOut = o, e }

// ---------------------------------------------------------------- colour ----

type style string

const (
	reset     style = "\x1b[0m"
	bold      style = "\x1b[1m"
	dim       style = "\x1b[2m"
	italic    style = "\x1b[3m"
	underline style = "\x1b[4m"

	red     style = "\x1b[31m"
	green   style = "\x1b[32m"
	yellow  style = "\x1b[33m"
	blue    style = "\x1b[34m"
	magenta style = "\x1b[35m"
	cyan    style = "\x1b[36m"
	grey    style = "\x1b[90m"
)

func paint(s style, v string) string {
	mu.Lock()
	on := colour
	mu.Unlock()
	if !on || v == "" {
		return v
	}
	return string(s) + v + string(reset)
}

// Colour helpers. These are safe to nest inside format strings.
func Bold(s string) string   { return paint(bold, s) }
func Dim(s string) string    { return paint(dim, s) }
func Italic(s string) string { return paint(italic, s) }
func Under(s string) string  { return paint(underline, s) }
func Red(s string) string    { return paint(red, s) }
func Green(s string) string  { return paint(green, s) }
func Yellow(s string) string { return paint(yellow, s) }
func Blue(s string) string   { return paint(blue, s) }
func Mag(s string) string    { return paint(magenta, s) }
func Cyan(s string) string   { return paint(cyan, s) }
func Grey(s string) string   { return paint(grey, s) }

// Pkg renders a package name, and PkgVer a name@version, consistently
// everywhere so users can scan output for the thing they care about.
func Pkg(name string) string { return Cyan(Bold(name)) }

// PkgVer renders "name 1.2.3" with the version de-emphasised.
func PkgVer(name, version string) string {
	if version == "" {
		return Pkg(name)
	}
	return Pkg(name) + " " + Grey(version)
}

// --------------------------------------------------------------- symbols ----

type glyphs struct{ tick, cross, warn, info, arrow, bullet, dot, spin string }

var (
	uni = glyphs{"✓", "✗", "⚠", "ℹ", "→", "•", "·", "⠁"}
	asc = glyphs{"+", "x", "!", "i", "->", "*", ".", "-"}
)

func g() glyphs {
	mu.Lock()
	defer mu.Unlock()
	if unicode {
		return uni
	}
	return asc
}

// Arrow returns the platform-appropriate right arrow, exported for plan output.
func Arrow() string { return g().arrow }

// ---------------------------------------------------------------- output ----

func write(w io.Writer, s string) {
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprint(w, s)
}

// Printf writes an unadorned line to stdout, suppressed by --quiet.
func Printf(format string, a ...any) {
	if level < Normal {
		return
	}
	write(out, fmt.Sprintf(format, a...))
}

// Line writes a single unadorned line to stdout.
func Line(format string, a ...any) { Printf(format+"\n", a...) }

// Blank writes an empty separator line.
func Blank() { Printf("\n") }

// Step reports forward progress, e.g. "==> Resolving 3 packages".
func Step(format string, a ...any) {
	if level < Normal {
		return
	}
	write(out, Blue(Bold("==>"))+" "+Bold(fmt.Sprintf(format, a...))+"\n")
}

// Detail reports a sub-step, indented under the most recent Step.
func Detail(format string, a ...any) {
	if level < Normal {
		return
	}
	write(out, "    "+fmt.Sprintf(format, a...)+"\n")
}

// Ok reports a successful action.
func Ok(format string, a ...any) {
	if level < Normal {
		return
	}
	write(out, Green(g().tick)+" "+fmt.Sprintf(format, a...)+"\n")
}

// Warn reports a recoverable problem. Warnings survive --quiet because
// silently dropping them has burned too many package-manager users.
func Warn(format string, a ...any) {
	write(errOut, Yellow(g().warn)+" "+Yellow("warning")+" "+fmt.Sprintf(format, a...)+"\n")
}

// Err reports a fatal problem.
func Err(format string, a ...any) {
	write(errOut, Red(g().cross)+" "+Red(Bold("error"))+" "+fmt.Sprintf(format, a...)+"\n")
}

// Info reports a neutral note.
func Info(format string, a ...any) {
	if level < Normal {
		return
	}
	write(out, Blue(g().info)+" "+fmt.Sprintf(format, a...)+"\n")
}

// Hint offers a next command the user can run. Hints are the main reason
// hop's errors are actionable rather than merely correct.
func Hint(format string, a ...any) {
	if level < Normal {
		return
	}
	write(errOut, "  "+Grey("hint:")+" "+fmt.Sprintf(format, a...)+"\n")
}

// Debug writes only under --verbose.
func Debug(format string, a ...any) {
	if level < Verbose {
		return
	}
	write(errOut, Grey("  debug "+fmt.Sprintf(format, a...))+"\n")
}

// ---------------------------------------------------------------- tables ----

// Table accumulates rows and prints them with aligned, colour-aware columns.
// Alignment measures display width, so colour escapes never skew a column.
type Table struct {
	head  []string
	rows  [][]string
	align []bool // true = right align
}

// NewTable creates a table with the given header. A nil or empty header
// prints no header row.
func NewTable(head ...string) *Table { return &Table{head: head} }

// RightAlign marks the given column indices as right-aligned.
func (t *Table) RightAlign(cols ...int) *Table {
	for _, c := range cols {
		for len(t.align) <= c {
			t.align = append(t.align, false)
		}
		t.align[c] = true
	}
	return t
}

// Row appends a row. Short rows are padded, long rows widen the table.
func (t *Table) Row(cells ...string) *Table { t.rows = append(t.rows, cells); return t }

// Len reports the number of data rows.
func (t *Table) Len() int { return len(t.rows) }

// Render writes the table to stdout.
func (t *Table) Render() {
	if level < Normal {
		return
	}
	cols := len(t.head)
	for _, r := range t.rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 {
		return
	}

	w := make([]int, cols)
	measure := func(cells []string) {
		for i, c := range cells {
			if n := DisplayWidth(c); n > w[i] {
				w[i] = n
			}
		}
	}
	if len(t.head) > 0 {
		measure(t.head)
	}
	for _, r := range t.rows {
		measure(r)
	}

	var b strings.Builder
	emit := func(cells []string, transform func(string) string) {
		for i := 0; i < cols; i++ {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			if transform != nil {
				cell = transform(cell)
			}
			pad := w[i] - DisplayWidth(cell)
			if pad < 0 {
				pad = 0
			}
			right := i < len(t.align) && t.align[i]
			switch {
			case right:
				b.WriteString(strings.Repeat(" ", pad) + cell)
			case i == cols-1:
				b.WriteString(cell) // never pad the final column
			default:
				b.WriteString(cell + strings.Repeat(" ", pad))
			}
			if i < cols-1 {
				b.WriteString("  ")
			}
		}
		b.WriteString("\n")
	}

	if len(t.head) > 0 {
		emit(t.head, func(s string) string { return Grey(Bold(s)) })
	}
	for _, r := range t.rows {
		emit(r, nil)
	}
	write(out, b.String())
}

// KV prints an aligned key/value block, used by `hop info` and `hop doctor`.
func KV(pairs ...[2]string) {
	if level < Normal {
		return
	}
	kw := 0
	for _, p := range pairs {
		if n := DisplayWidth(p[0]); n > kw {
			kw = n
		}
	}
	var b strings.Builder
	for _, p := range pairs {
		if p[1] == "" {
			continue
		}
		b.WriteString(Grey(p[0]+":") + strings.Repeat(" ", kw-DisplayWidth(p[0])+1) + p[1] + "\n")
	}
	write(out, b.String())
}

// ---------------------------------------------------------------- format ----

// DisplayWidth returns the printable width of s, ignoring ANSI escapes and
// counting runes rather than bytes.
func DisplayWidth(s string) int {
	n, inEsc := 0, false
	for _, r := range s {
		switch {
		case inEsc:
			// Consume until the final byte of a CSI/SGR sequence.
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			n++
		}
	}
	return n
}

// Truncate shortens s to at most n display columns, adding an ellipsis.
// It is escape-aware only to the extent that it refuses to cut mid-escape.
func Truncate(s string, n int) string {
	if n <= 0 || DisplayWidth(s) <= n {
		return s
	}
	if n == 1 {
		return "."
	}
	cut := make([]rune, 0, n)
	count := 0
	for _, r := range s {
		if count >= n-1 {
			break
		}
		cut = append(cut, r)
		count++
	}
	return string(cut) + "…"
}

// Bytes formats a byte count with binary-ish units at a fixed width, so
// columns of sizes line up without a table.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n)
	units := []string{"KB", "MB", "GB", "TB"}
	for _, u := range units {
		f /= unit
		if f < unit {
			if f < 10 {
				return fmt.Sprintf("%.1f %s", f, u)
			}
			return fmt.Sprintf("%.0f %s", f, u)
		}
	}
	return fmt.Sprintf("%.1f PB", f/unit)
}

// Duration formats a wall-clock duration for the run summary.
func Duration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%dus", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.2fs", d.Seconds())
	default:
		m := int(d.Minutes())
		return fmt.Sprintf("%dm%02ds", m, int(d.Seconds())-m*60)
	}
}

// Ago formats a timestamp as a compact relative age, e.g. "3h ago".
func Ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	if d < 0 {
		return "just now"
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

// Plural returns the singular or plural word for n.
func Plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Count renders "3 packages" with correct agreement.
func Count(n int, one, many string) string {
	return fmt.Sprintf("%d %s", n, Plural(n, one, many))
}

// List joins items with commas and a trailing conjunction.
func List(items []string, conj string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " " + conj + " " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " " + conj + " " + items[len(items)-1]
	}
}

// Indent prefixes every line of s with n spaces.
func Indent(s string, n int) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}

// Rule draws a full-width horizontal separator.
func Rule() {
	if level < Normal {
		return
	}
	ch := "─"
	if !unicode {
		ch = "-"
	}
	write(out, Grey(strings.Repeat(ch, min(Width(), 60)))+"\n")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ------------------------------------------------------------- interaction ----

// Confirm asks a yes/no question. It returns def when stdin is not a terminal,
// so scripted use never hangs waiting for an answer nobody will type.
func Confirm(prompt string, def bool) bool {
	if !isTerminal(os.Stdin) || !tty {
		return def
	}
	suffix := " [y/N] "
	if def {
		suffix = " [Y/n] "
	}
	write(out, Bold(prompt)+Grey(suffix))

	var answer string
	if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil {
		// Bare Enter (or EOF) accepts the default.
		write(out, "\n")
		return def
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default:
		return def
	}
}

// TrimZeroWidth strips characters that would corrupt width accounting if a
// recipe description contained them.
func TrimZeroWidth(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' || !utf8.ValidRune(r) {
			return ' '
		}
		return r
	}, s)
}
