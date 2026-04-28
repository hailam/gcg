// Package config loads ${WORKDIR}/config/app.yaml into a target struct.
// YAML values may use ${VAR|default} placeholders that are resolved against
// the process environment at load time.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	gconfig "github.com/gookit/config/v2"
	"github.com/gookit/config/v2/yamlv3"
)

type Option func(*options)

type options struct {
	configPath string
}

// WithConfigFile overrides the YAML path. Default: ${WORKDIR}/config/app.yaml.
func WithConfigFile(p string) Option { return func(o *options) { o.configPath = p } }

// Load reads the YAML config and decodes it into out.
func Load(out any, opts ...Option) error {
	o := &options{}
	for _, opt := range opts {
		slog.Debug("applying config loader option", "option", fmt.Sprintf("%T", opt))
		opt(o)
	}

	if o.configPath == "" {
		slog.Debug("no config file specified; using default", "path", "config/app.yaml")
		wd, _ := os.Getwd()
		o.configPath = filepath.Join(wd, "config", "app.yaml")
	}

	// WithTagName("yaml") tells gookit's decoder to use `yaml:` tags. Default
	// is "mapstructure", which would silently fail to map snake_case YAML
	// keys (e.g. max_bytes) onto camelCase Go fields (MaxBytes).
	// ParseTime adds a hook so `${TIMEOUT|10s}`-style strings decode into
	// time.Duration.
	c := gconfig.NewWithOptions("app", gconfig.ParseEnv, gconfig.ParseTime, gconfig.WithTagName("yaml"))
	c.AddDriver(yamlv3.Driver)
	if err := c.LoadFiles(o.configPath); err != nil {
		slog.Error("error loading config file", "error", err)
		return fmt.Errorf("config: load %s: %w", o.configPath, err)
	}

	if err := c.Decode(out); err != nil {
		slog.Error("error decoding config", "error", err)
		return fmt.Errorf("config: decode: %w", err)
	}
	return nil
}
