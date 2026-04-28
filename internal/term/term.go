// Package term provides terminal helpers for gcg's CLI: a stoppable
// spinner with rotating messages, a rolling thinking viewport, TTY
// detection, and ANSI color wrappers. Color, spinner, and viewport
// output are silently suppressed when the target writer is not an
// interactive terminal, so piped output stays clean.
package term

import (
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"sync"
	"time"
	"unicode/utf8"

	xterm "golang.org/x/term"
)

// ANSI escape codes. Kept exported in case callers want to compose them
// directly, but prefer the helper functions below — they no-op on
// non-TTY writers.
const (
	AnsiReset  = "\033[0m"
	AnsiDim    = "\033[2m"
	AnsiBold   = "\033[1m"
	AnsiRed    = "\033[31m"
	AnsiGreen  = "\033[32m"
	AnsiYellow = "\033[33m"
	AnsiCyan   = "\033[36m"

	// Combined attributes — terminals honor "dim" or "bold" attribute
	// state at the same time as a color, but stacking two separate ANSI
	// wrappers (each with its own reset) drops the outer attribute on
	// many terminals. Use a single CSI to keep both alive.
	ansiBoldCyan = "\033[1;36m"
	ansiDimGreen = "\033[2;32m"
)

// IsTerminal reports whether w is an interactive terminal (a character
// device). When w is a pipe, file, or buffer it returns false so callers
// can skip color and spinner output.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// colorize wraps text in the given ANSI code when w is a terminal.
// Otherwise it returns text unchanged.
func colorize(w io.Writer, code, text string) string {
	if !IsTerminal(w) {
		return text
	}
	return code + text + AnsiReset
}

// Dim, Bold, Cyan, Green, Yellow, Red return text wrapped in the
// corresponding ANSI attribute when w is an interactive terminal.
func Dim(w io.Writer, text string) string    { return colorize(w, AnsiDim, text) }
func Bold(w io.Writer, text string) string   { return colorize(w, AnsiBold, text) }
func Cyan(w io.Writer, text string) string   { return colorize(w, AnsiCyan, text) }
func Green(w io.Writer, text string) string  { return colorize(w, AnsiGreen, text) }
func Yellow(w io.Writer, text string) string { return colorize(w, AnsiYellow, text) }
func Red(w io.Writer, text string) string    { return colorize(w, AnsiRed, text) }

// BoldCyan and DimGreen apply two attributes in a single ANSI sequence.
// Use these instead of nesting Bold(Cyan(...)) — see ansiBoldCyan note.
func BoldCyan(w io.Writer, text string) string { return colorize(w, ansiBoldCyan, text) }
func DimGreen(w io.Writer, text string) string { return colorize(w, ansiDimGreen, text) }

// Phase-tagged status verb pools. Spinner cycles through these so the
// loading line feels alive across long calls without manual
// orchestration. Each pool mixes English with Latinized Dhivehi for a
// little flavor — keep additions short so they fit a single line.
var (
	MsgsThinking = []string{
		"thinking",
		"pondering",
		"mulling it over",
		"chasing implications",
		"reasoning",
		"weighing the options",
		"reading between the lines",
		"connecting the dots",
		"squinting at the diff",
		"musing",
		"parsing intent",
		"tracing logic",
		"brewing thoughts",
		"untangling the prompt",
		"crunching context",
		"visnanee",        // dv: thinking
		"visnamun",        // dv: while pondering
		"funah visnanee",  // dv: thinking deeply / diving deep
		"raavanee",        // dv: planning / structuring the idea
		"hithah aranee",   // dv: it's coming to mind
		"loa hulhuvanee",  // dv: opening the eyes (taking it in)
		"maana hoadhanee", // dv: finding the meaning / interpreting
		"goiy nimmamun",   // dv: while making a decision
	}
	MsgsToolReading = []string{
		"peeking at the file",
		"reading the source",
		"checking callers",
		"following the trail",
		"scanning the text",
		"digging in",
		"inspecting contents",
		"grokking the source",
		"deciphering",
		"parsing the bytes",
		"kiyanee",             // dv: reading
		"balaalanee",          // dv: taking a quick look
		"thafseel balanee",    // dv: checking the details
		"diraasaa kuranee",    // dv: analyzing / studying
		"aslu balanee",        // dv: looking at the root/source
		"etheyreyah fethumun", // dv: while diving inside (the file)
	}
	MsgsToolListing = []string{
		"listing siblings",
		"surveying the layout",
		"mapping the package",
		"looking around",
		"scanning the tree",
		"gathering paths",
		"indexing",
		"scouting the perimeter",
		"walking the tree",
		"hoadhanee",           // dv: while searching
		"balanee",             // dv: looking
		"vashaigen balanee",   // dv: looking all around
		"tharitheeb balanee",  // dv: checking the arrangement/order
		"vashaigen hoadhanee", // dv: searching the surroundings
		"hisaabu balanee",     // dv: checking the area/bounds
	}
	MsgsStructuring = []string{
		"structuring the response",
		"polishing the subject",
		"shaping JSON",
		"tightening the wording",
		"assembling pieces",
		"drafting the reply",
		"crossing the t's",
		"weaving the output",
		"compiling the answer",
		"liyamun",          // dv: while writing
		"nimmanee",         // dv: finishing up
		"saafkuranee",      // dv: tidying up / cleaning
		"furihama kuranee", // dv: perfecting / completing
		"reethi kuranee",   // dv: polishing / making it presentable
		"ekulavaalanee",    // dv: compiling / putting together
		"javaabu raavanee", // dv: structuring the answer
	}
)

// Spinner is a stoppable Braille indicator that cycles a pool of
// messages so the loading line stays lively. It writes frames to a
// writer, dimmed for visual separation from real content, and clears
// its line on Stop.
type Spinner struct {
	w    io.Writer
	msgs []string
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// NewSpinner starts a spinner with a single static message.
func NewSpinner(w io.Writer, msg string) *Spinner {
	return NewSpinnerPool(w, []string{msg})
}

// NewSpinnerPool starts a spinner that cycles through msgs roughly every
// 1.5s. msgs must be non-empty; the first entry shows immediately.
func NewSpinnerPool(w io.Writer, msgs []string) *Spinner {
	if len(msgs) == 0 {
		msgs = []string{"working"}
	}
	s := &Spinner{
		w:    w,
		msgs: msgs,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go s.loop()
	return s
}

func (s *Spinner) loop() {
	defer close(s.done)
	t := time.NewTicker(80 * time.Millisecond)
	defer t.Stop()

	// Rotate to the next message every ~1.5s so a long call cycles
	// through several verbs without flickering.
	const ticksPerMessage = 18

	render := func(frame, msg string) {
		fmt.Fprintf(s.w, "\r\033[K%s", Dim(s.w, frame+" "+msg+"…"))
	}

	frameIdx, tickCount := 0, 0
	msgIdx := rand.IntN(len(s.msgs))
	render(spinnerFrames[frameIdx], s.msgs[msgIdx])
	for {
		select {
		case <-s.stop:
			fmt.Fprint(s.w, "\r\033[K")
			return
		case <-t.C:
			frameIdx = (frameIdx + 1) % len(spinnerFrames)
			tickCount++
			if tickCount >= ticksPerMessage && len(s.msgs) > 1 {
				tickCount = 0
				msgIdx = pickNextMsg(s.msgs, msgIdx)
			}
			render(spinnerFrames[frameIdx], s.msgs[msgIdx])
		}
	}
}

// Stop halts the spinner and clears its line. Safe to call multiple
// times; only the first call has effect. Blocks until the spinner
// goroutine has actually exited so a subsequent write to the same
// writer can't interleave with a half-printed frame.
func (s *Spinner) Stop() {
	s.once.Do(func() {
		close(s.stop)
		<-s.done
	})
}

// Viewport renders streamed text into a fixed-height rolling window —
// the top (height-1) lines are a thinking transcript that scrolls up as
// new lines arrive; the bottom line is an animated spinner with a
// rotating message pool so the loading indicator stays alive even when
// no chunks are arriving. Token-sized chunks from the LLM produce a
// typewriter feel on the live partial line.
//
// A goroutine ticks the spinner at 80ms regardless of chunk activity;
// Write and tick redraws share a mutex so the screen stays coherent. On
// a non-terminal writer Write becomes a no-op and no goroutine is
// started, so piped output stays clean.
type Viewport struct {
	w      io.Writer
	height int
	msgs   []string

	finished []string
	current  string
	rendered bool

	frameIdx  int
	msgIdx    int
	tickCount int

	mu   sync.Mutex
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// NewViewport creates a height-line viewport on w with msgs cycling in
// the bottom spinner row. height < 2 is bumped to 4 (room for at least
// one thinking line plus the spinner). Empty msgs falls back to a
// generic "working".
func NewViewport(w io.Writer, height int, msgs []string) *Viewport {
	if height < 2 {
		height = 4
	}
	if len(msgs) == 0 {
		msgs = []string{"working"}
	}
	v := &Viewport{
		w:      w,
		height: height,
		msgs:   msgs,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	if IsTerminal(w) {
		go v.tick()
	} else {
		close(v.done)
	}
	return v
}

func (v *Viewport) tick() {
	defer close(v.done)
	t := time.NewTicker(80 * time.Millisecond)
	defer t.Stop()
	const ticksPerMessage = 18

	v.mu.Lock()
	v.msgIdx = rand.IntN(len(v.msgs))
	v.draw()
	v.mu.Unlock()

	for {
		select {
		case <-v.stop:
			return
		case <-t.C:
			v.mu.Lock()
			v.frameIdx = (v.frameIdx + 1) % len(spinnerFrames)
			v.tickCount++
			if v.tickCount >= ticksPerMessage && len(v.msgs) > 1 {
				v.tickCount = 0
				v.msgIdx = pickNextMsg(v.msgs, v.msgIdx)
			}
			v.draw()
			v.mu.Unlock()
		}
	}
}

// Write feeds streamed text into the thinking pane, splitting on '\n'
// and redrawing after each chunk so partial lines update
// character-by-character as tokens stream in.
func (v *Viewport) Write(p []byte) (int, error) {
	if !IsTerminal(v.w) {
		return len(p), nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, ch := range string(p) {
		switch ch {
		case '\n':
			v.pushLine(v.current)
			v.current = ""
		case '\r':
			// ignore stray carriage returns; render is newline-driven
		default:
			v.current += string(ch)
		}
	}
	v.draw()
	return len(p), nil
}

func (v *Viewport) pushLine(line string) {
	v.finished = append(v.finished, line)
	max := v.height - 1
	if len(v.finished) > max {
		v.finished = v.finished[len(v.finished)-max:]
	}
}

func (v *Viewport) termWidth() int {
	f, ok := v.w.(*os.File)
	if !ok {
		return 80
	}
	w, _, err := xterm.GetSize(int(f.Fd()))
	if err != nil || w < 10 {
		return 80
	}
	return w
}

// spinnerFrames is the Braille rotation shared between Spinner.loop and
// Viewport.tick.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// pickNextMsg returns a random index into msgs that differs from prev,
// so the verb visibly changes on each rotation. Falls back to 0 for a
// single-element pool.
func pickNextMsg(msgs []string, prev int) int {
	if len(msgs) <= 1 {
		return 0
	}
	next := rand.IntN(len(msgs) - 1)
	if next >= prev {
		next++
	}
	return next
}

// draw renders the full viewport region. Caller must hold v.mu.
func (v *Viewport) draw() {
	width := v.termWidth()
	if v.rendered {
		fmt.Fprintf(v.w, "\033[%dF\033[J", v.height)
	}

	thinkLines := v.height - 1
	visible := append([]string{}, v.finished...)
	if v.current != "" {
		visible = append(visible, v.current)
	}
	if len(visible) > thinkLines {
		visible = visible[len(visible)-thinkLines:]
	}
	for len(visible) < thinkLines {
		visible = append(visible, "")
	}

	const prefix = "▎ "
	maxLine := width - utf8.RuneCountInString(prefix) - 1
	for _, line := range visible {
		fmt.Fprintln(v.w, Dim(v.w, prefix+truncate(line, maxLine)))
	}
	frame := spinnerFrames[v.frameIdx]
	msg := v.msgs[v.msgIdx]
	fmt.Fprintln(v.w, Dim(v.w, frame+" "+msg+"…"))
	v.rendered = true
}

// Stop halts the spinner ticker and clears the viewport region. Safe to
// call multiple times; only the first call has effect. Blocks until the
// ticker goroutine has exited so a subsequent write to the same writer
// can't interleave with a half-printed frame.
func (v *Viewport) Stop() {
	v.once.Do(func() {
		if IsTerminal(v.w) {
			close(v.stop)
			<-v.done
			v.mu.Lock()
			if v.rendered {
				fmt.Fprintf(v.w, "\033[%dF\033[J", v.height)
				v.rendered = false
			}
			v.finished = nil
			v.current = ""
			v.mu.Unlock()
		}
	})
}

// truncate shortens s to at most width runes, appending an ellipsis when
// content was dropped. width <= 1 returns "".
func truncate(s string, width int) string {
	if width <= 1 {
		return ""
	}
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	limit := width - 1
	out := make([]rune, 0, limit)
	count := 0
	for _, r := range s {
		if count >= limit {
			break
		}
		out = append(out, r)
		count++
	}
	return string(out) + "…"
}
