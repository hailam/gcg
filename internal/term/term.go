// Package term provides terminal helpers for gcg's CLI: a stoppable
// spinner with rotating messages, a rolling thinking viewport, TTY
// detection, and ANSI color wrappers. Color, spinner, and viewport
// output are silently suppressed when the target writer is not an
// interactive terminal, so piped output stays clean.
package term

import (
	"fmt"
	"io"
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
	AnsiRed    = "\033[31m"
	AnsiGreen  = "\033[32m"
	AnsiYellow = "\033[33m"
	AnsiCyan   = "\033[36m"
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

// Dim, Cyan, Green, Yellow, Red return text wrapped in the corresponding
// ANSI color when w is an interactive terminal.
func Dim(w io.Writer, text string) string    { return colorize(w, AnsiDim, text) }
func Cyan(w io.Writer, text string) string   { return colorize(w, AnsiCyan, text) }
func Green(w io.Writer, text string) string  { return colorize(w, AnsiGreen, text) }
func Yellow(w io.Writer, text string) string { return colorize(w, AnsiYellow, text) }
func Red(w io.Writer, text string) string    { return colorize(w, AnsiRed, text) }

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
		"visnanee",       // dv: thinking
		"visnamun",       // dv: while pondering
		"hithah aranee",  // dv: it's coming to mind
		"loa hulhuvanee", // dv: opening the eyes (taking it in)
	}
	MsgsToolReading = []string{
		"peeking at the file",
		"reading the source",
		"checking callers",
		"following the trail",
		"kiyanee",    // dv: reading
		"balaalanee", // dv: taking a look
	}
	MsgsToolListing = []string{
		"listing siblings",
		"surveying the layout",
		"mapping the package",
		"looking around",
		"hoadhamun", // dv: while searching
		"balanee",   // dv: looking
	}
	MsgsStructuring = []string{
		"structuring the response",
		"polishing the subject",
		"shaping JSON",
		"tightening the wording",
		"liyamun",     // dv: while writing
		"nimmanee",    // dv: finishing up
		"saafkuranee", // dv: tidying up
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
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	t := time.NewTicker(80 * time.Millisecond)
	defer t.Stop()

	// Rotate to the next message every ~1.5s so a long call cycles
	// through several verbs without flickering.
	const ticksPerMessage = 18

	render := func(frame, msg string) {
		fmt.Fprintf(s.w, "\r\033[K%s", Dim(s.w, frame+" "+msg+"…"))
	}

	frameIdx, msgIdx, tickCount := 0, 0, 0
	render(frames[frameIdx], s.msgs[msgIdx])
	for {
		select {
		case <-s.stop:
			fmt.Fprint(s.w, "\r\033[K")
			return
		case <-t.C:
			frameIdx = (frameIdx + 1) % len(frames)
			tickCount++
			if tickCount >= ticksPerMessage && len(s.msgs) > 1 {
				tickCount = 0
				msgIdx = (msgIdx + 1) % len(s.msgs)
			}
			render(frames[frameIdx], s.msgs[msgIdx])
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

// Viewport renders streamed text into a fixed-height rolling window
// using ANSI cursor controls so each Write redraws the same lines in
// place. Token-sized chunks from the LLM produce a typewriter feel on
// the live partial line; completed lines scroll up out of view.
//
// On a non-terminal writer Write becomes a no-op so piped output stays
// clean. Not safe to use concurrently with another writer to the same
// stream — Stop the viewport before printing other content.
type Viewport struct {
	w        io.Writer
	height   int
	finished []string
	current  string
	rendered bool
	mu       sync.Mutex
}

// NewViewport creates a height-line viewport on w. height < 1 falls
// back to 4.
func NewViewport(w io.Writer, height int) *Viewport {
	if height < 1 {
		height = 4
	}
	return &Viewport{w: w, height: height}
}

// Write feeds streamed text into the viewport, splitting on '\n' and
// redrawing after each chunk so partial lines update character-by-
// character as tokens stream in.
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
	if len(v.finished) > v.height {
		v.finished = v.finished[len(v.finished)-v.height:]
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

func (v *Viewport) draw() {
	width := v.termWidth()
	if v.rendered {
		fmt.Fprintf(v.w, "\033[%dF\033[J", v.height)
	}

	visible := append([]string{}, v.finished...)
	if v.current != "" {
		visible = append(visible, v.current)
	}
	if len(visible) > v.height {
		visible = visible[len(visible)-v.height:]
	}
	// Pad with blanks so the block is always exactly `height` lines —
	// the next redraw can rewind by a fixed number of rows.
	for len(visible) < v.height {
		visible = append(visible, "")
	}

	const prefix = "▎ "
	maxLine := width - utf8.RuneCountInString(prefix) - 1
	for _, line := range visible {
		fmt.Fprintln(v.w, Dim(v.w, prefix+truncate(line, maxLine)))
	}
	v.rendered = true
}

// Stop clears the viewport region. Safe to call multiple times.
func (v *Viewport) Stop() {
	if !IsTerminal(v.w) {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.rendered {
		return
	}
	fmt.Fprintf(v.w, "\033[%dF\033[J", v.height)
	v.rendered = false
	v.finished = nil
	v.current = ""
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
