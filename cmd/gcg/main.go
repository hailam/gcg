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
	var verbose bool
	flag.BoolVar(&verbose, "v", false, "enable debug logging (alias for --verbose)")
	flag.BoolVar(&verbose, "verbose", false, "enable debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	load := func() (*bootstrap.App, error) { return bootstrap.Init() }
	if err := gcg.Run(context.Background(), load); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
