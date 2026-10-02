package pg

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gisripa/dalforge/internal/check"
	"github.com/gisripa/dalforge/internal/golden"
	"github.com/gisripa/dalforge/internal/idl"
)

// sqlcBinary returns the sqlc on PATH (mise pins it). Without it the test
// skips locally but fails in CI, so a missing tool can't hide a regression.
func sqlcBinary(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("sqlc")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("sqlc not on PATH in CI; it is pinned in mise.toml")
		}
		t.Skip("sqlc not on PATH; run tests through mise")
	}
	return bin
}

var _fixtures = []struct{ name, file string }{
	{name: "orders", file: "orders/v1/orders.proto"},
	{name: "types", file: "types/v1/types.proto"},
	{name: "defaults", file: "defaults/v1/defaults.proto"},
	{name: "shop", file: "shop/v1/stores.proto"},
}

// generate emits a fixture into a temp dir, runs sqlc there, and returns the
// dir. sqlc's Go lands in gen/sqlcdb.
func generate(t *testing.T, bin, file string) string {
	t.Helper()
	res, err := idl.Load(context.Background(), []string{file}, idl.Options{ImportPaths: []string{"../../idl/testdata"}})
	if err != nil {
		t.Fatal(err)
	}
	if d := check.Schema(res.Schema); d.HasErrors() {
		t.Fatalf("core findings:\n%s", d)
	}
	model, d := Build(res.Schema, res.PG, Target{})
	if d.HasErrors() {
		t.Fatalf("pg findings:\n%s", d)
	}
	files, d := Emit(res.Schema, model, Layout{})
	if d.HasErrors() {
		t.Fatalf("emit findings:\n%s", d)
	}

	dir := t.TempDir()
	for path, body := range files {
		write(t, filepath.Join(dir, filepath.FromSlash(path)), body)
	}
	// Both query directories must exist for sqlc, even when empty.
	for _, q := range []string{"generated", "custom"} {
		if err := os.MkdirAll(filepath.Join(dir, "queries", q), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command(bin, "generate")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sqlc generate: %v\n%s", err, out)
	}
	return dir
}

// TestSQLCGenerate runs sqlc on the emitted output of each fixture and pins
// the Go it generates. This is the feedback loop for the SQL side: sqlc must
// accept every generated query and override, and its Go is what the DAL
// builds on. No pgtype may appear (design §7).
func TestSQLCGenerate(t *testing.T) {
	bin := sqlcBinary(t)
	for _, tt := range _fixtures {
		t.Run(tt.name, func(t *testing.T) {
			out := filepath.Join(generate(t, bin, tt.file), "gen", "sqlcdb")
			entries, err := os.ReadDir(out)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string][]byte{}
			for _, e := range entries {
				b, err := os.ReadFile(filepath.Join(out, e.Name()))
				if err != nil {
					t.Fatal(err)
				}
				got[e.Name()] = []byte(stripVersion(string(b)))
				if strings.Contains(string(b), "pgtype") {
					t.Errorf("%s mentions pgtype; a type override is missing or doesn't match", e.Name())
				}
			}
			golden.AssertDir(t, "sqlc/"+tt.name, got)
		})
	}
}

func write(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// stripVersion drops sqlc's "//   sqlc vX.Y.Z" header line so the goldens
// don't churn on sqlc patch upgrades.
func stripVersion(src string) string {
	var lines []string
	for _, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(l, "//   sqlc v") {
			continue
		}
		lines = append(lines, l)
	}
	return strings.Join(lines, "\n")
}
