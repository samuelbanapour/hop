package ui

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// repaintInterval is slow enough to stay cheap over SSH and fast enough to
// feel live.
const repaintInterval = 70 * time.Millisecond

// barState tracks a bar's lifecycle. Finished bars graduate out of the live
// region and become permanent scrollback.
type barState int

const (
	barActive barState = iota
	barDone
	barFailed
	barSkipped
)

// Bar is a single unit of concurrent work, typically one package download.
// Every method is safe to call from the worker goroutine that owns it.
type Bar struct {
	label string
	total atomic.Int64
	cur   atomic.Int64

	mu      sync.Mutex
	state   barState
	note    string // replaces the rate column once finished
	started time.Time

	// Rate is smoothed over a short window; raw deltas jitter badly enough to
	// be useless on a real connection.
	lastAt    time.Time
	lastBytes int64
	rate      float64
}

// Add records n more bytes (or units) of progress.
func (b *Bar) Add(n int64) { b.cur.Add(n) }

// Write lets a Bar be used directly as an io.Writer tee, so the downloader
// reports progress without a wrapper type.
func (b *Bar) Write(p []byte) (int, error) { b.Add(int64(len(p))); return len(p), nil }

// SetTotal updates the expected size, for servers that only reveal
// Content-Length after the response headers arrive.
func (b *Bar) SetTotal(n int64) { b.total.Store(n) }

// Done marks the work complete with a short trailing note.
func (b *Bar) Done(note string) { b.finish(barDone, note) }

// Fail marks the work failed; the reason is shown in red.
func (b *Bar) Fail(reason string) { b.finish(barFailed, reason) }

// Skip marks the work unnecessary, e.g. an artifact already in the store.
func (b *Bar) Skip(note string) { b.finish(barSkipped, note) }

func (b *Bar) finish(s barState, note string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != barActive {
		return // first outcome wins; a late Fail must not overwrite Done
	}
	b.state, b.note = s, note
	if t := b.total.Load(); s == barDone && t > 0 {
		b.cur.Store(t) // snap to 100% so the bar never ends at 99%
	}
}

func (b *Bar) snapshot() (barState, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state, b.note
}

// observeRate updates the smoothed transfer rate. Called only by the renderer.
func (b *Bar) observeRate(now time.Time) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	cur := b.cur.Load()
	if b.lastAt.IsZero() {
		b.lastAt, b.lastBytes = now, cur
		return 0
	}
	dt := now.Sub(b.lastAt).Seconds()
	if dt < 0.2 {
		return b.rate
	}
	inst := float64(cur-b.lastBytes) / dt
	if b.rate == 0 {
		b.rate = inst
	} else {
		b.rate = 0.7*b.rate + 0.3*inst // exponential smoothing
	}
	b.lastAt, b.lastBytes = now, cur
	return b.rate
}

// Progress renders a set of concurrent Bars. On a TTY it repaints a live
// region in place; otherwise it logs one line per completed bar, which keeps
// CI output and `hop install ... | tee` legible.
type Progress struct {
	mu        sync.Mutex
	bars      []*Bar
	live      []*Bar
	lastLines int
	stopped   bool
	done      chan struct{}
	tty       bool
	maxLive   int
}

// NewProgress starts a renderer. Always pair it with a deferred Stop.
func NewProgress() *Progress {
	p := &Progress{done: make(chan struct{}), tty: IsTTY() && level >= Normal, maxLive: 12}
	if _, h, err := terminalSize(os.Stdout); err == nil && h > 6 && h-4 < p.maxLive {
		p.maxLive = h - 4
	}
	if p.tty {
		fmt.Fprint(os.Stdout, "\x1b[?25l") // hide cursor
		go p.loop()
	}
	return p
}

// Bar registers a new unit of work. total may be 0 if not yet known.
func (p *Progress) Bar(label string, total int64) *Bar {
	b := &Bar{label: label, started: time.Now()}
	b.total.Store(total)

	p.mu.Lock()
	p.bars = append(p.bars, b)
	p.live = append(p.live, b)
	p.mu.Unlock()
	return b
}

// Log prints a permanent line without tearing the live region.
func (p *Progress) Log(format string, a ...any) {
	if level < Normal {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.tty {
		fmt.Fprintf(os.Stdout, format+"\n", a...)
		return
	}
	p.clear()
	fmt.Fprintf(os.Stdout, format+"\n", a...)
	p.paint(time.Now())
}

// Stop finishes rendering, leaving all completed bars in scrollback.
func (p *Progress) Stop() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	tty := p.tty
	p.mu.Unlock()

	if tty {
		close(p.done)
		p.mu.Lock()
		p.clear()
		// Graduate every remaining bar so nothing silently disappears.
		for _, b := range p.live {
			fmt.Fprintln(os.Stdout, p.render(b, time.Now(), true))
		}
		p.live, p.lastLines = nil, 0
		fmt.Fprint(os.Stdout, "\x1b[?25h") // restore cursor
		p.mu.Unlock()
		return
	}

	// Non-TTY: emit the summary lines we never streamed.
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.live {
		fmt.Fprintln(os.Stdout, p.render(b, time.Now(), true))
	}
	p.live = nil
}

func (p *Progress) loop() {
	t := time.NewTicker(repaintInterval)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case now := <-t.C:
			p.mu.Lock()
			if !p.stopped {
				p.clear()
				p.paint(now)
			}
			p.mu.Unlock()
		}
	}
}

// clear rewinds over the live region. Caller must hold p.mu.
func (p *Progress) clear() {
	if p.lastLines == 0 {
		return
	}
	// Move up and erase each line, then park at the region's top-left.
	fmt.Fprintf(os.Stdout, "\x1b[%dA\r", p.lastLines)
	for i := 0; i < p.lastLines; i++ {
		fmt.Fprint(os.Stdout, "\x1b[2K")
		if i < p.lastLines-1 {
			fmt.Fprint(os.Stdout, "\x1b[1B")
		}
	}
	if p.lastLines > 1 {
		fmt.Fprintf(os.Stdout, "\x1b[%dA", p.lastLines-1)
	}
	fmt.Fprint(os.Stdout, "\r")
	p.lastLines = 0
}

// paint writes finished bars as permanent lines, then the live region.
// Caller must hold p.mu.
func (p *Progress) paint(now time.Time) {
	var still []*Bar
	var b strings.Builder

	for _, bar := range p.live {
		if st, _ := bar.snapshot(); st != barActive {
			b.WriteString(p.render(bar, now, true) + "\n") // graduates to scrollback
			continue
		}
		still = append(still, bar)
	}
	p.live = still

	shown := still
	hidden := 0
	if len(shown) > p.maxLive {
		hidden = len(shown) - p.maxLive
		shown = shown[:p.maxLive]
	}
	for _, bar := range shown {
		b.WriteString(p.render(bar, now, false) + "\n")
	}
	if hidden > 0 {
		b.WriteString(Grey(fmt.Sprintf("  … %d more queued", hidden)) + "\n")
	}

	fmt.Fprint(os.Stdout, b.String())
	p.lastLines = len(shown)
	if hidden > 0 {
		p.lastLines++
	}
}

// labelWidth keeps every bar's label column aligned. Caller must hold p.mu.
func (p *Progress) labelWidth() int {
	w := 8
	for _, b := range p.bars {
		if n := DisplayWidth(b.label); n > w {
			w = n
		}
	}
	return min(w, 28)
}

// render produces one bar's line. final selects the completed form.
func (p *Progress) render(b *Bar, now time.Time, final bool) string {
	lw := p.labelWidth()
	label := Truncate(b.label, lw)
	label += strings.Repeat(" ", lw-DisplayWidth(label))

	st, note := b.snapshot()
	cur, total := b.cur.Load(), b.total.Load()

	if final || st != barActive {
		var mark, tail string
		switch st {
		case barDone:
			mark = Green(g().tick)
			tail = Grey(note)
			if note == "" {
				tail = Grey(Duration(now.Sub(b.started)))
			}
		case barFailed:
			mark, tail = Red(g().cross), Red(note)
		case barSkipped:
			mark, tail = Grey(g().dot), Grey(note)
		default:
			mark, tail = Yellow(g().warn), Yellow("interrupted")
		}
		size := ""
		if total > 0 && st == barDone {
			size = Grey(Bytes(total))
		}
		return fmt.Sprintf("  %s %s  %s  %s", mark, Pkg(label), size, tail)
	}

	// Active: bar + percent + transferred + rate.
	rate := b.observeRate(now)
	rateStr := ""
	if rate > 1024 {
		rateStr = Grey(Bytes(int64(rate)) + "/s")
	}

	// Reserve room for the fixed columns, then give the rest to the bar.
	fixed := 2 + 2 + lw + 2 + 5 + 2 + 20 + 2 + 12
	barW := Width() - fixed
	if barW < 8 {
		barW = 8
	}
	if barW > 32 {
		barW = 32
	}

	var meter, pct, amount string
	if total > 0 {
		frac := float64(cur) / float64(total)
		if frac > 1 {
			frac = 1
		}
		filled := int(frac * float64(barW))
		full, empty := "█", "░"
		if !unicode {
			full, empty = "#", "-"
		}
		meter = Cyan(strings.Repeat(full, filled)) + Grey(strings.Repeat(empty, barW-filled))
		pct = fmt.Sprintf("%3.0f%%", frac*100)
		amount = fmt.Sprintf("%s/%s", Bytes(cur), Bytes(total))
	} else {
		// Unknown size: sweep an indeterminate block across the track.
		full, empty := "█", "░"
		if !unicode {
			full, empty = "#", "-"
		}
		span := max(barW/5, 2)
		pos := int(now.UnixMilli()/90) % (barW + span)
		var sb strings.Builder
		for i := 0; i < barW; i++ {
			if i >= pos-span && i < pos {
				sb.WriteString(full)
			} else {
				sb.WriteString(empty)
			}
		}
		meter = Cyan(sb.String())
		pct = "   ?"
		amount = Bytes(cur)
	}

	amount += strings.Repeat(" ", max(0, 20-DisplayWidth(amount)))
	return fmt.Sprintf("  %s %s  %s  %s  %s", Grey(g().bullet), Pkg(label), meter+" "+Grey(pct), amount, rateStr)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------- spinner ----

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
var spinASCII = []string{"|", "/", "-", "\\"}

// Spinner reports an indeterminate operation such as fetching the index.
type Spinner struct {
	mu      sync.Mutex
	msg     string
	done    chan struct{}
	stopped bool
	tty     bool
	started time.Time
}

// StartSpinner begins an indeterminate indicator. Pair with Succeed or Stop.
func StartSpinner(format string, a ...any) *Spinner {
	s := &Spinner{
		msg:     fmt.Sprintf(format, a...),
		done:    make(chan struct{}),
		tty:     IsTTY() && level >= Normal,
		started: time.Now(),
	}
	if !s.tty {
		if level >= Normal {
			Step("%s", s.msg)
		}
		return s
	}
	fmt.Fprint(os.Stdout, "\x1b[?25l")
	go func() {
		t := time.NewTicker(80 * time.Millisecond)
		defer t.Stop()
		frames := spinFrames
		if !unicode {
			frames = spinASCII
		}
		for i := 0; ; i++ {
			select {
			case <-s.done:
				return
			case <-t.C:
				s.mu.Lock()
				if !s.stopped {
					fmt.Fprintf(os.Stdout, "\r\x1b[2K%s %s", Cyan(frames[i%len(frames)]), s.msg)
				}
				s.mu.Unlock()
			}
		}
	}()
	return s
}

// Update changes the message in place.
func (s *Spinner) Update(format string, a ...any) {
	s.mu.Lock()
	s.msg = fmt.Sprintf(format, a...)
	s.mu.Unlock()
}

// Succeed stops the spinner and leaves a tick and message behind.
func (s *Spinner) Succeed(format string, a ...any) {
	s.halt()
	if level >= Normal {
		write(out, fmt.Sprintf("%s %s %s\n",
			Green(g().tick), fmt.Sprintf(format, a...), Grey(Duration(time.Since(s.started)))))
	}
}

// Stop clears the spinner without a verdict.
func (s *Spinner) Stop() { s.halt() }

func (s *Spinner) halt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true
	if s.tty {
		close(s.done)
		fmt.Fprint(os.Stdout, "\r\x1b[2K\x1b[?25h")
	}
}
