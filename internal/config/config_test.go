package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\nmodule: github.com/acme/shop\n"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, got, want string }{
		{"backend", cfg.Backend, "pg"},
		{"pg.version", cfg.PG.Version, "16"},
		{"proto.roots", strings.Join(cfg.Proto.Roots, ","), "proto"},
		{"out.schema", cfg.Out.Schema, "schema/schema.sql"},
		{"out.queries", cfg.Out.Queries, "queries/generated"},
		{"out.custom", cfg.Out.Custom, "queries/custom"},
		{"out.sqlc", cfg.Out.SQLC, "gen/sqlcdb"},
		{"out.dal", cfg.Out.DAL, "gen"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
	if major, _ := cfg.PGMajor(); major != 16 {
		t.Errorf("PGMajor() = %d, want 16", major)
	}
}

func TestParseErrors(t *testing.T) {
	const base = "version: 1\nmodule: m\n"
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{name: "missing version", yaml: "module: m\n", want: "version: got 0, want 1"},
		{name: "missing module", yaml: "version: 1\n", want: "module: required"},
		{name: "unknown key (typo)", yaml: base + "protos:\n  roots: [x]\n", want: "field protos not found"},
		{name: "unsupported backend", yaml: base + "backend: dynamodb\n", want: `backend: "dynamodb" is not supported`},
		{name: "bad pg version", yaml: base + "pg:\n  version: latest\n", want: `pg.version: "latest"`},
		{name: "absolute out path", yaml: base + "out:\n  schema: /tmp/schema.sql\n", want: "out.schema"},
		{name: "escaping root", yaml: base + "proto:\n  roots: [../shared]\n", want: "proto.roots[0]"},
		{name: "custom inside generated", yaml: base + "out:\n  custom: queries/generated/mine\n", want: "out.custom"},
		{name: "custom equals sqlc out", yaml: base + "out:\n  custom: gen/sqlcdb\n", want: "overlaps out.sqlc"},
		{name: "sqlc dir not a package name", yaml: base + "out:\n  sqlc: gen/sqlc-db\n", want: `out.sqlc: the last element "sqlc-db"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse() error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadSetsRoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultFile)
	if err := os.WriteFile(path, []byte("version: 1\nmodule: m\npg:\n  version: \"16.9\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Root != dir || cfg.Path("schema/schema.sql") != filepath.Join(dir, "schema", "schema.sql") {
		t.Errorf("Root = %q, Path = %q", cfg.Root, cfg.Path("schema/schema.sql"))
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("Load of a missing file must fail")
	}
}
