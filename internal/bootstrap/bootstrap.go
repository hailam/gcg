// Package bootstrap performs the shared startup work that every entry point
// (cmd/gcg, future cmd/server, etc.) needs: load YAML config,
// decode into a Config. Entry points call Init and then build whatever
// service-specific clients they need on top of the returned App.
package bootstrap

import (
	"fmt"
	"log/slog"

	"github.com/hailam/play-commit/internal/config"
)

type App struct {
	Cfg *config.Config
}

type Option func(*options)

type options struct {
	cfgOpts []config.Option
}

// WithConfigOptions forwards loader options (e.g. WithConfigFile) through
// to config.Load.
func WithConfigOptions(o ...config.Option) Option {
	return func(opt *options) { opt.cfgOpts = append(opt.cfgOpts, o...) }
}

// Init loads configuration and returns an App ready for use.
func Init(opts ...Option) (*App, error) {
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}

	var cfg config.Config
	if err := config.Load(&cfg, o.cfgOpts...); err != nil {
		slog.Error("error loading config", "error", err)
		return nil, fmt.Errorf("bootstrap: %w", err)
	}
	return &App{Cfg: &cfg}, nil
}
