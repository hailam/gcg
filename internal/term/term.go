// Package term provides terminal helpers for gcg's CLI: a small
// stoppable spinner, TTY detection, and ANSI color wrappers. Color and
// spinner output are silently suppressed when the target writer is not
// an interactive terminal, so piped output stays clean.
package term

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
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

// Spinner is a tiny stoppable Braille-frame indicator. It writes frames
// to a writer at a fixed interval, dimmed for visual separation from
// real content, and clears its line on Stop.
type Spinner struct {
	w    io.Writer
	msg  string
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// NewSpinner starts a spinner writing to w with the given message.
// Caller is responsible for calling Stop when the underlying work
// finishes (or as soon as the first real content arrives, so the
// spinner doesn't overlap streamed output).
func NewSpinner(w io.Writer, msg string) *Spinner {
	s := &Spinner{
		w:    w,
		msg:  msg,
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

	render := func(frame string) {
		fmt.Fprintf(s.w, "\r\033[K%s", Dim(s.w, frame+" "+s.msg))
	}

	render(frames[0])
	i := 0
	for {
		select {
		case <-s.stop:
			fmt.Fprint(s.w, "\r\033[K")
			return
		case <-t.C:
			i = (i + 1) % len(frames)
			render(frames[i])
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
