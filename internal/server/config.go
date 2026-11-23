// Package server ties the pieces together: it loads the declarations, runs
// one proxy and one MCP gateway per namespace, starts agent runs and serves
// the external API.
package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/imohiyoko/agent/internal/spec"
)

// Config is nsctl.yaml.
type Config struct {
	NamespacesDir string   `yaml:"namespaces_dir"`
	StateDir      string   `yaml:"state_dir"`
	SecretsFile   string   `yaml:"secrets_file"`
	Listen        string   `yaml:"listen"`
	Admins        []string `yaml:"admins"`
	// MaxRunsPerNamespace limits concurrent runs (default 4).
	MaxRunsPerNamespace int `yaml:"max_runs_per_namespace"`

	// Home is used to expand "~" (not part of the YAML).
	Home string `yaml:"-"`
}

// LoadConfig reads the config file. Relative paths are relative to the
// file's directory.
func LoadConfig(path string) (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	c := &Config{Home: home}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	base, _ := filepath.Abs(filepath.Dir(path))
	c.resolve(base)
	return c, c.Validate()
}

func (c *Config) resolve(base string) {
	fix := func(p, def string) string {
		if p == "" {
			p = def
		}
		if p == "~" {
			return c.Home
		}
		if strings.HasPrefix(p, "~/") {
			return filepath.Join(c.Home, p[2:])
		}
		if !filepath.IsAbs(p) {
			return filepath.Join(base, p)
		}
		return p
	}
	c.NamespacesDir = fix(c.NamespacesDir, "namespaces")
	c.StateDir = fix(c.StateDir, "~/.local/state/nsctl")
	c.SecretsFile = fix(c.SecretsFile, "~/.config/nsctl/secrets.yaml")
	if c.Listen == "" {
		c.Listen = "127.0.0.1:7070"
	}
	if c.MaxRunsPerNamespace == 0 {
		c.MaxRunsPerNamespace = 4
	}
}

// Validate checks the config.
func (c *Config) Validate() error {
	var errs []error
	for _, a := range c.Admins {
		if !spec.IsPrincipalName(a) {
			errs = append(errs, fmt.Errorf("admins: %q is not a valid principal name", a))
		}
	}
	if c.MaxRunsPerNamespace < 1 {
		errs = append(errs, errors.New("max_runs_per_namespace must be at least 1"))
	}
	return errors.Join(errs...)
}

// PrivateDir holds tokens, sockets, logs and audit logs. It is hidden from
// every sandbox, whatever the declarations say.
func (c *Config) PrivateDir() string { return filepath.Join(c.StateDir, "private") }

// PublicDir holds files sandboxes may read (generated MCP configs).
func (c *Config) PublicDir() string { return filepath.Join(c.StateDir, "public") }

// TokensFile is where token hashes are kept.
func (c *Config) TokensFile() string { return filepath.Join(c.PrivateDir(), "tokens.json") }

// AuditDir is where audit logs are written.
func (c *Config) AuditDir() string { return filepath.Join(c.PrivateDir(), "audit") }

// AlwaysHidden are paths no sandbox may read.
func (c *Config) AlwaysHidden() []string {
	return []string{c.PrivateDir(), c.SecretsFile}
}
