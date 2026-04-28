package main

import (
	"context"
	"fmt"
	"os"

	"github.com/hailam/play-commit/internal/bootstrap"
	"github.com/hailam/play-commit/internal/gcg"
)

func main() {
	load := func() (*bootstrap.App, error) { return bootstrap.Init() }
	if err := gcg.Run(context.Background(), load); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
