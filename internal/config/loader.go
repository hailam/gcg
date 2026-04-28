// Package config loads gcg's YAML config into a target struct. YAML values
// may use ${VAR|default} placeholders that are resolved against the process
// environment at load time.
//
// Lookup order (first hit wins):
//  1. The path passed via WithConfigFile (explicit override).
//  2. $GCG_CONFIG, if set and the file exists.
//  3. $PWD/config/app.yaml, if it exists (project-local override).
//  4. $XDG_CONFIG_HOME/gcg/app.yaml (or ~/.config/gcg/app.yaml).
//  5. The default YAML embedded into the binary at build time.
//
// (5) is what makes `gcg` work after `go install` from any working
// directory — the binary always carries a usable default. Every value in
// the default is a ${VAR|fallback} placeholder, so env vars are the
// expected override mechanism for installed users.
package config

import (
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	gconfig "github.com/gookit/config/v2"
	"github.com/gookit/config/v2/yamlv3"
)

//go:embed default.yaml
var embeddedDefaultYAML []byte

type Option func(*options)

type options struct {
	configPath string
}

// WithConfigFile forces a specific YAML path. Skips the search chain.
func WithConfigFile(p string) Option { return func(o *options) { o.configPath = p } }

// Load reads the YAML config and decodes it into out.
func Load(out any, opts ...Option) error {
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}

	// WithTagName("yaml") tells gookit's decoder to use `yaml:` tags. Default
	// is "mapstructure", which would silently fail to map snake_case YAML
	// keys (e.g. max_bytes) onto camelCase Go fields (MaxBytes).
	// ParseTime adds a hook so `${TIMEOUT|10s}`-style strings decode into
	// time.Duration.
	c := gconfig.NewWithOptions("app", gconfig.ParseEnv, gconfig.ParseTime, gconfig.WithTagName("yaml"))
	c.AddDriver(yamlv3.Driver)

	if err := loadFirstAvailable(c, o.configPath); err != nil {
		return err
	}

	if err := c.Decode(out); err != nil {
		slog.Error("error decoding config", "error", err)
		return fmt.Errorf("config: decode: %w", err)
	}
	return nil
}

func loadFirstAvailable(c *gconfig.Config, explicit string) error {
	// 1. Explicit path: must exist; failure here is a real error.
	if explicit != "" {
		slog.Debug("loading config from explicit path", "path", explicit)
		if err := c.LoadFiles(explicit); err != nil {
			return fmt.Errorf("config: load %s: %w", explicit, err)
		}
		return nil
	}

	for _, candidate := range candidatePaths() {
		if _, err := os.Stat(candidate); err != nil {
			continue
		}
		slog.Debug("loading config from candidate path", "path", candidate)
		if err := c.LoadFiles(candidate); err != nil {
			return fmt.Errorf("config: load %s: %w", candidate, err)
		}
		return nil
	}

	// Fallback: embedded default. Always present, always loadable.
	slog.Debug("no config file found; using embedded default")
	if err := c.LoadSources(yamlv3.Driver.Name(), embeddedDefaultYAML); err != nil {
		return fmt.Errorf("config: load embedded default: %w", err)
	}
	return nil
}

func candidatePaths() []string {
	var paths []string
	if p := os.Getenv("GCG_CONFIG"); p != "" {
		paths = append(paths, p)
	}
	if wd, err := os.Getwd(); err == nil {
		paths = append(paths, filepath.Join(wd, "config", "app.yaml"))
	}
	if userConfig := userConfigDir(); userConfig != "" {
		paths = append(paths, filepath.Join(userConfig, "gcg", "app.yaml"))
	}
	return paths
}

func userConfigDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return xdg
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config")
	}
	return ""
}
