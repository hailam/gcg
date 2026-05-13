package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/hailam/gcg/internal/bootstrap"
	"github.com/hailam/gcg/internal/gcg"
)

func main() {
	var (
		verbose bool
		noClip  bool
		body    bool
		think   string
	)
	flag.BoolVar(&verbose, "v", false, "enable debug logging (alias for --verbose)")
	flag.BoolVar(&verbose, "verbose", false, "enable debug logging")
	flag.BoolVar(&noClip, "no-clip", false, "do not copy the commit message to the system clipboard")
	flag.BoolVar(&noClip, "disable-clip", false, "alias for --no-clip")
	flag.BoolVar(&body, "body", false, "also generate a bullet-point commit body (defaults think level to high)")
	flag.StringVar(&think, "think", "", "override the Ollama thinking level: true, false, high, medium, low (default: true; high under --body)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `gcg — generate a Conventional Commits message from staged changes.

Usage: gcg [flags]

Flags:
  -v, --verbose         enable debug logging
      --no-clip         do not write the commit message to the clipboard
                        (alias: --disable-clip)
      --body            also generate a bullet-point body; think defaults to high
      --think LEVEL     override think level: true|false|high|medium|low

Output:
  stderr  loading/thinking UI, tool-call lines, and the final pretty preview
  stdout  the clean commit message (subject; with --body also \n\n + body)
          — capture this with a wrapper, e.g.  set msg (gcg --no-clip)

`)
		flag.PrintDefaults()
	}
	flag.Parse()

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	load := func() (*bootstrap.App, error) { return bootstrap.Init() }
	opts := gcg.Options{NoClip: noClip, Body: body, Think: think}
	if err := gcg.Run(context.Background(), load, opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
