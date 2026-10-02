// Package config loads dalforge.yaml, the project configuration. Paths in it
// are relative to the file's directory, the project root.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// DefaultFile is the config file name dalforge looks for.
const DefaultFile = "dalforge.yaml"

// Config is dalforge.yaml.
type Config struct {
	Version int    `yaml:"version"`
	Module  string `yaml:"module"`  // Go module path of the project, e.g. github.com/acme/shop
	Backend string `yaml:"backend"` // "pg", the only supported backend
	Proto   Proto  `yaml:"proto"`
	PG      PG     `yaml:"pg"`
	Out     Out    `yaml:"out"`

	// Root is the directory holding the config file; every relative path
	// resolves against it. Not part of the file.
	Root string `yaml:"-"`
}

// Proto selects the IDL files.
type Proto struct {
	Roots []string `yaml:"roots"` // import paths; default ["proto"]
	Files []string `yaml:"files"` // globs relative to a root; default: every .proto under the roots
}

// PG configures the Postgres backend.
type PG struct {
	Version string `yaml:"version"` // deploy target, e.g. "16.9"; default "16"
}

// Out is where generated files go, relative to Root.
type Out struct {
	Schema  string `yaml:"schema"`  // default schema/schema.sql
	Queries string `yaml:"queries"` // default queries/generated
	Custom  string `yaml:"custom"`  // default queries/custom; user-owned, never written
	SQLC    string `yaml:"sqlc"`    // default gen/sqlcdb
	DAL     string `yaml:"dal"`     // default gen
}

// Load reads and validates the config at path, filling defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.Root = filepath.Dir(abs)
	return cfg, nil
}

// Parse decodes and validates a config, filling defaults. Unknown keys are
// errors, so a typo can't silently do nothing.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	cfg.defaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) defaults() {
	set := func(v *string, def string) {
		if *v == "" {
			*v = def
		}
	}
	set(&c.Backend, "pg")
	set(&c.PG.Version, "16")
	if len(c.Proto.Roots) == 0 {
		c.Proto.Roots = []string{"proto"}
	}
	set(&c.Out.Schema, "schema/schema.sql")
	set(&c.Out.Queries, "queries/generated")
	set(&c.Out.Custom, "queries/custom")
	set(&c.Out.SQLC, "gen/sqlcdb")
	set(&c.Out.DAL, "gen")
}

func (c *Config) validate() error {
	var errs []error
	if c.Version != 1 {
		errs = append(errs, fmt.Errorf("version: got %d, want 1", c.Version))
	}
	if c.Module == "" {
		errs = append(errs, errors.New("module: required (the Go module path of this project, e.g. github.com/acme/shop)"))
	}
	if c.Backend != "pg" {
		errs = append(errs, fmt.Errorf("backend: %q is not supported (only \"pg\")", c.Backend))
	}
	if _, err := c.PGMajor(); err != nil {
		errs = append(errs, err)
	}

	paths := map[string]string{
		"out.schema": c.Out.Schema, "out.queries": c.Out.Queries, "out.custom": c.Out.Custom,
		"out.sqlc": c.Out.SQLC, "out.dal": c.Out.DAL,
	}
	for _, key := range []string{"out.schema", "out.queries", "out.custom", "out.sqlc", "out.dal"} {
		if err := relative(key, paths[key]); err != nil {
			errs = append(errs, err)
		}
	}
	for i, r := range c.Proto.Roots {
		if err := relative(fmt.Sprintf("proto.roots[%d]", i), r); err != nil {
			errs = append(errs, err)
		}
	}
	// sqlc's output directory names its Go package.
	if pkg := path.Base(c.Out.SQLC); !goPackageName(pkg) {
		errs = append(errs, fmt.Errorf("out.sqlc: the last element %q becomes a Go package name; use lowercase letters and digits", pkg))
	}
	// The user-owned directory must not overlap anything dalforge writes.
	for _, key := range []string{"out.queries", "out.sqlc", "out.dal"} {
		if overlaps(c.Out.Custom, paths[key]) {
			errs = append(errs, fmt.Errorf("out.custom %q overlaps %s %q; custom queries must live outside generated directories", c.Out.Custom, key, paths[key]))
		}
	}
	return errors.Join(errs...)
}

// PGMajor returns the major version of pg.version ("16.9" → 16).
func (c *Config) PGMajor() (int, error) {
	major, _, _ := strings.Cut(c.PG.Version, ".")
	n, err := strconv.Atoi(major)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("pg.version: %q is not a Postgres version like \"16.9\"", c.PG.Version)
	}
	return n, nil
}

// Path resolves a config-relative path.
func (c *Config) Path(rel string) string {
	return filepath.Join(c.Root, filepath.FromSlash(rel))
}

func relative(key, p string) error {
	clean := path.Clean(filepath.ToSlash(p))
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%s: %q must be a path inside the project", key, p)
	}
	return nil
}

func overlaps(a, b string) bool {
	a, b = path.Clean(filepath.ToSlash(a)), path.Clean(filepath.ToSlash(b))
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func goPackageName(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
