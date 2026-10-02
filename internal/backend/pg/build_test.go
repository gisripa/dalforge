package pg

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gisripa/dalforge/internal/check"
	"github.com/gisripa/dalforge/internal/diag"
	"github.com/gisripa/dalforge/internal/golden"
	"github.com/gisripa/dalforge/internal/idl"
)

// build loads a fixture, requires it to be core-valid (so each fixture
// exercises only Postgres rules), and builds the physical model.
func build(t *testing.T, importPath, file string, target Target) (*Schema, diag.List) {
	t.Helper()
	res, err := idl.Load(context.Background(), []string{file}, idl.Options{ImportPaths: []string{importPath}})
	if err != nil {
		t.Fatalf("Load(%s) error = %v", file, err)
	}
	if core := check.Schema(res.Schema); core.HasErrors() {
		t.Fatalf("fixture %s is not core-valid:\n%s", file, core)
	}
	return Build(res.Schema, res.PG, target)
}

// TestModelGolden pins the physical model of the loader's valid fixtures.
func TestModelGolden(t *testing.T) {
	for _, tt := range []struct{ name, file string }{
		{name: "orders", file: "orders/v1/orders.proto"},
		{name: "types", file: "types/v1/types.proto"},
		{name: "defaults", file: "defaults/v1/defaults.proto"},
		{name: "shop", file: "shop/v1/stores.proto"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			model, diags := build(t, "../../idl/testdata", tt.file, Target{})
			if len(diags) > 0 {
				t.Fatalf("unexpected findings:\n%s", diags)
			}
			b, err := json.MarshalIndent(model, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden.Assert(t, "model/"+tt.name+".json", append(b, '\n'))
		})
	}
}

// TestRules pins the findings for one fixture per rule. Valid controls inside
// each fixture must stay silent.
func TestRules(t *testing.T) {
	for _, rule := range []string{"dal204", "dal207", "dal208", "dal209", "dal210", "dal211", "dal212"} {
		t.Run(rule, func(t *testing.T) {
			_, diags := build(t, "testdata", rule+".proto", Target{Major: 16})
			for _, d := range diags {
				if !strings.EqualFold(d.Rule, rule) {
					t.Errorf("fixture %s also triggered %s: %s", rule, d.Rule, d)
				}
			}
			golden.Assert(t, "rules/"+rule, []byte(diags.String()))
		})
	}
}

func TestTargetBelowFloor(t *testing.T) {
	_, diags := build(t, "../../idl/testdata", "shop/v1/stores.proto", Target{Major: 15})
	if len(diags) != 1 || diags[0].Rule != RuleVersion || !strings.Contains(diags[0].Message, "older than the oldest supported") {
		t.Errorf("findings = %v, want one DAL207 about the floor", diags)
	}
}

func TestCheckIdentifier(t *testing.T) {
	tests := []struct {
		name string
		want string // substring of the reason; "" means valid
	}{
		{name: "orders"},
		{name: "_tmp"},
		{name: "col_1$"},
		{name: "", want: "empty"},
		{name: "order", want: "reserved"},
		{name: "user", want: "reserved"},
		{name: "system_user", want: "reserved"}, // new in PG16
		{name: "Orders", want: "lowercase"},
		{name: "1st", want: "lowercase"},
		{name: "a-b", want: "lowercase"},
		{name: strings.Repeat("a", 63)},
		{name: strings.Repeat("a", 64), want: "truncates"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkIdentifier(tt.name)
			if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
				t.Errorf("checkIdentifier(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestGoType(t *testing.T) {
	tests := []struct {
		in      string
		want    string // GoType.String(), or "" for a parse error
		wantImp string
	}{
		{in: "string", want: "string"},
		{in: "github.com/pgvector/pgvector-go.Vector", want: "pgvector-go.Vector", wantImp: "github.com/pgvector/pgvector-go"},
		{in: "net/netip.Prefix", want: "netip.Prefix", wantImp: "net/netip"},
		{in: "not a type"},
		{in: "pkg."},
		{in: ".Type"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			g, err := parseGoType(tt.in)
			if tt.want == "" {
				if err == nil {
					t.Errorf("parseGoType(%q) = %v, want an error", tt.in, g)
				}
				return
			}
			if err != nil || g.String() != tt.want || g.Import != tt.wantImp {
				t.Errorf("parseGoType(%q) = %+v (%s), %v; want %s from %q", tt.in, g, g, err, tt.want, tt.wantImp)
			}
		})
	}

	ptr := GoType{Import: "time", Name: "Time", Pointer: true}
	slice := GoType{Import: "github.com/google/uuid", Name: "UUID", Slice: true}
	if ptr.String() != "*time.Time" || slice.String() != "[]uuid.UUID" {
		t.Errorf("String() = %s, %s; want *time.Time, []uuid.UUID", ptr, slice)
	}
}

func TestCustomBase(t *testing.T) {
	for in, want := range map[string]string{
		"vector(1536)":          "vector",
		"geometry(Point, 4326)": "geometry",
		"CITEXT":                "citext",
		"vector[]":              "vector",
		"my_domain":             "my_domain",
	} {
		if got := customBase(in); got != want {
			t.Errorf("customBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWithNull(t *testing.T) {
	tests := []struct {
		g       GoType
		notNull bool
		want    string
	}{
		{g: GoType{Name: "string"}, notNull: true, want: "string"},
		{g: GoType{Name: "string"}, want: "*string"},
		{g: GoType{Import: "time", Name: "Time"}, want: "*time.Time"},
		{g: GoType{Name: "[]byte"}, want: "[]byte"},
		{g: GoType{Name: "string", Slice: true}, want: "[]string"},
		{g: GoType{Import: "encoding/json", Name: "RawMessage"}, want: "json.RawMessage"},
	}
	for _, tt := range tests {
		if got := withNull(tt.g, tt.notNull).String(); got != tt.want {
			t.Errorf("withNull(%s, notNull=%v) = %s, want %s", tt.g, tt.notNull, got, tt.want)
		}
	}
}
