package pg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gisripa/dalforge/internal/check"
	"github.com/gisripa/dalforge/internal/golden"
	"github.com/gisripa/dalforge/internal/idl"
)

// TestEmitGolden pins the full SQL-side output (schema, query files,
// sqlc.yaml) for the loader's valid fixtures.
func TestEmitGolden(t *testing.T) {
	for _, tt := range []struct{ name, file string }{
		{name: "orders", file: "orders/v1/orders.proto"},
		{name: "types", file: "types/v1/types.proto"},
		{name: "defaults", file: "defaults/v1/defaults.proto"},
		{name: "shop", file: "shop/v1/stores.proto"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res, err := idl.Load(context.Background(), []string{tt.file}, idl.Options{ImportPaths: []string{"../../idl/testdata"}})
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
			if len(d) > 0 {
				t.Fatalf("emit findings:\n%s", d)
			}
			golden.AssertDir(t, "emit/"+tt.name, files)
		})
	}
}

func TestQueryNameCollision(t *testing.T) {
	dir := t.TempDir()
	src := `syntax = "proto3";
package c;
import "dal/v1/options.proto";
message A { string id = 1 [(dal.v1.field).primary_key = true]; }
// "OrderStore" trims to "Order", the same as a store named "Order".
service OrderStore { option (dal.v1.store) = {entity: "A"};
  rpc Get(A) returns (A) { option (dal.v1.query) = {get: {}}; } }
service Order { option (dal.v1.store) = {entity: "A"};
  rpc Get(A) returns (A) { option (dal.v1.query) = {get: {}}; } }
// "A" + "BGet" and "AB" + "Get" both concatenate to "ABGet".
service AStore { option (dal.v1.store) = {entity: "A"};
  rpc BGet(A) returns (A) { option (dal.v1.query) = {get: {}}; } }
service ABStore { option (dal.v1.store) = {entity: "A"};
  rpc Get(A) returns (A) { option (dal.v1.query) = {get: {}}; } }
`
	if err := os.WriteFile(filepath.Join(dir, "c.proto"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := idl.Load(context.Background(), []string{"c.proto"}, idl.Options{ImportPaths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	model, _ := Build(res.Schema, res.PG, Target{})
	_, d := Emit(res.Schema, model, Layout{})
	got := d.String()
	for _, want := range []string{"both generate the sqlc query OrderGet", "both generate the sqlc query ABGet"} {
		if !strings.Contains(got, want) {
			t.Errorf("findings =\n%s\nwant one containing %q", got, want)
		}
	}
	if len(d) != 2 {
		t.Errorf("got %d findings, want 2", len(d))
	}
}

func TestIdentName(t *testing.T) {
	if got := identName("orders", "status", "key"); got != "orders_status_key" {
		t.Errorf("identName short = %q", got)
	}
	long := identName(strings.Repeat("t", 40), strings.Repeat("c", 40), "idx")
	if len(long) != _maxIdentifier || checkIdentifier(long) != "" {
		t.Errorf("identName long = %q (%d bytes), want a valid %d-byte identifier", long, len(long), _maxIdentifier)
	}
	if long == identName(strings.Repeat("t", 40), strings.Repeat("d", 40), "idx") {
		t.Error("identName: different long names must not collide after shortening")
	}
}

func TestParamTypeConflict(t *testing.T) {
	m := &Schema{Tables: []*Table{{
		Name: "t",
		Columns: []*Column{
			{Name: "a", Type: "numeric", GoType: GoType{Name: "string"}},
			{Name: "b", Type: "numeric(12, 2)", GoType: GoType{Import: "github.com/shopspring/decimal", Name: "Decimal"}},
			{Name: "c", Type: "uuid", GoType: GoType{Import: "github.com/google/uuid", Name: "UUID"}},
		},
	}}}
	_, conflicts := nullableTypes(m)
	if len(conflicts) != 1 || !strings.Contains(conflicts[0], "t.b: SQL type numeric maps to *decimal.Decimal here but *string elsewhere") {
		t.Errorf("conflicts = %q, want one about t.b", conflicts)
	}
}
