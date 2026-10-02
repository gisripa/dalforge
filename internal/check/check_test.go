package check

import (
	"context"
	"strings"
	"testing"

	"github.com/gisripa/dalforge/internal/golden"
	"github.com/gisripa/dalforge/internal/idl"
)

func findings(t *testing.T, importPath, file string) string {
	t.Helper()
	res, err := idl.Load(context.Background(), []string{file}, idl.Options{ImportPaths: []string{importPath}})
	if err != nil {
		t.Fatalf("Load(%s) error = %v", file, err)
	}
	return Schema(res.Schema).String()
}

// TestRules pins the findings for one fixture per rule. Each fixture should
// trigger only its own rule; valid controls inside it must stay silent.
func TestRules(t *testing.T) {
	for _, rule := range []string{"dal107", "dal110", "dal117", "dal118", "lists"} {
		t.Run(rule, func(t *testing.T) {
			golden.Assert(t, rule, []byte(findings(t, "testdata", rule+".proto")))
		})
	}
}

// TestValidFixturesAreClean runs the core rules over the loader's fixtures,
// which model correct IDL: any finding there is a false positive.
func TestValidFixturesAreClean(t *testing.T) {
	for _, file := range []string{
		"orders/v1/orders.proto",
		"types/v1/types.proto",
		"defaults/v1/defaults.proto",
		"shop/v1/stores.proto",
	} {
		t.Run(file, func(t *testing.T) {
			if got := findings(t, "../idl/testdata", file); got != "" {
				t.Errorf("unexpected findings:\n%s", got)
			}
		})
	}
}

// TestNoCascadeFromBrokenReference: a typo in a reference reports only the
// reference, not a follow-on request-shape error.
func TestNoCascadeFromBrokenReference(t *testing.T) {
	got := findings(t, "testdata", "cascade.proto")
	if strings.Count(got, "\n") != 1 || !strings.Contains(got, "DAL117") {
		t.Errorf("findings =\n%s\nwant exactly one DAL117", got)
	}
}
