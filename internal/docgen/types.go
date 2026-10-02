package docgen

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/gisripa/dalforge/internal/backend/pg"
	"github.com/gisripa/dalforge/internal/diag"
	"github.com/gisripa/dalforge/internal/idl"
	pgv1 "github.com/gisripa/dalforge/proto/dal/pg/v1"
)

// _probe lists the rows of the default type-mapping table: how each kind of
// proto field is written, and the label to show. The Postgres and Go types
// are not listed here: they come from building these fields through the real
// loader and backend, so the table can't drift from the code.
var _probe = []struct {
	label, decl string // decl: "<type> %s = %d<options>"
	canOptional bool
}{
	{"`string`", "string %s = %d", true},
	{"`string` + `FORMAT_UUID`", "string %s = %d [(dal.v1.field).format = FORMAT_UUID]", true},
	{"`string` + `FORMAT_DECIMAL`", "string %s = %d [(dal.v1.field).format = FORMAT_DECIMAL]", true},
	{"`string` + `FORMAT_JSON`", "string %s = %d [(dal.v1.field).format = FORMAT_JSON]", true},
	{"`bool`", "bool %s = %d", true},
	{"`int32`", "int32 %s = %d", true},
	{"`sint32`", "sint32 %s = %d", true},
	{"`sfixed32`", "sfixed32 %s = %d", true},
	{"`int64`", "int64 %s = %d", true},
	{"`sint64`", "sint64 %s = %d", true},
	{"`sfixed64`", "sfixed64 %s = %d", true},
	{"`uint32`", "uint32 %s = %d", true},
	{"`fixed32`", "fixed32 %s = %d", true},
	{"`uint64`", "uint64 %s = %d", true},
	{"`fixed64`", "fixed64 %s = %d", true},
	{"`float`", "float %s = %d", true},
	{"`double`", "double %s = %d", true},
	{"`bytes`", "bytes %s = %d", true},
	{"`google.protobuf.Timestamp`", "google.protobuf.Timestamp %s = %d", true},
	{"an enum", "Color %s = %d", true},
	{"`repeated string`", "repeated string %s = %d", false},
	{"`repeated int64`", "repeated int64 %s = %d", false},
	{"`repeated google.protobuf.Timestamp`", "repeated google.protobuf.Timestamp %s = %d", false},
	{"a message", "Nested %s = %d", true},
	{"`repeated` message", "repeated Nested %s = %d", false},
	{"`map<…>`", "map<string, string> %s = %d", false},
	{"`google.protobuf.Struct`", "google.protobuf.Struct %s = %d", true},
	{"`google.protobuf.Value`", "google.protobuf.Value %s = %d", true},
	{"`google.protobuf.ListValue`", "google.protobuf.ListValue %s = %d", true},
}

// _overrideKinds are the proto types the override table covers.
var _overrideKinds = []struct{ label, decl string }{
	{"`string`", "string"},
	{"`int32`", "int32"},
	{"`int64`", "int64"},
	{"`uint32`", "uint32"},
	{"`float`", "float"},
	{"`double`", "double"},
	{"`bool`", "bool"},
	{"`bytes`", "bytes"},
	{"`Timestamp`", "google.protobuf.Timestamp"},
	{"an enum", "Color"},
}

const _probeHeader = `syntax = "proto3";

package probe;

import "dal/pg/v1/options.proto";
import "dal/v1/options.proto";
import "google/protobuf/struct.proto";
import "google/protobuf/timestamp.proto";

enum Color {
  COLOR_UNSPECIFIED = 0;
}

message Nested {
  string s = 1;
}
`

// TypeTables renders the default type-mapping table and the table of
// allowed (dal.pg.v1.column).type overrides.
func TypeTables(ctx context.Context) (defaults, overrides string, err error) {
	// One entity per table: the default mapping (each row required, then
	// optional) and the overrides (each proto type × each Type value).
	var src strings.Builder
	src.WriteString(_probeHeader)
	src.WriteString("\nmessage Defaults {\n  option (dal.v1.table) = {};\n  int64 id = 1 [(dal.v1.field).primary_key = true];\n")
	n := 2
	for i, p := range _probe {
		fmt.Fprintf(&src, "  "+p.decl+";\n", fmt.Sprintf("r%d", i), n)
		n++
		if p.canOptional {
			fmt.Fprintf(&src, "  optional "+p.decl+";\n", fmt.Sprintf("o%d", i), n)
			n++
		}
	}
	src.WriteString("}\n\nmessage Overrides {\n  option (dal.v1.table) = {};\n  int64 id = 1 [(dal.v1.field).primary_key = true];\n")
	types := pgv1.Type(0).Descriptor().Values()
	n = 2
	for k, kind := range _overrideKinds {
		for t := 1; t < types.Len(); t++ {
			fmt.Fprintf(&src, "  %s k%d_t%d = %d [(dal.pg.v1.column).type = %s];\n", kind.decl, k, t, n, types.Get(t).Name())
			n++
		}
	}
	src.WriteString("}\n")

	dir, err := os.MkdirTemp("", "dalforge-docgen")
	if err != nil {
		return "", "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "probe.proto"), []byte(src.String()), 0o644); err != nil {
		return "", "", err
	}
	res, err := idl.Load(ctx, []string{"probe.proto"}, idl.Options{ImportPaths: []string{dir}})
	if err != nil {
		return "", "", fmt.Errorf("load probe: %w", err)
	}
	schema, diags := pg.Build(res.Schema, res.PG, pg.Target{Major: pg.Floor})

	columns := map[string]*pg.Column{}
	for _, t := range schema.Tables {
		for _, c := range t.Columns {
			columns[t.Entity+"."+c.Field] = c
		}
	}
	return defaultTable(columns, diags), overrideTable(columns, types), nil
}

func defaultTable(columns map[string]*pg.Column, diags diag.List) string {
	type result struct{ pg, goType, goOptional string }
	var labels [][]string
	var results []result
	for i, p := range _probe {
		r := result{goOptional: "(can't be `optional`)"}
		name := fmt.Sprintf("r%d", i)
		c := columns["probe.Defaults."+name]
		if c == nil {
			// No default mapping: the backend reports DAL209 for the field.
			r.pg, r.goType, r.goOptional = "none", "", ""
			if slices.ContainsFunc(diags, func(d diag.Diagnostic) bool {
				return d.Rule == pg.RuleTypeMap && strings.Contains(d.Message, fmt.Sprintf("%q", name))
			}) {
				r.pg = "none: needs a `custom_type` ([DAL209](lint-rules.md#dal209))"
			}
		} else {
			r.pg, r.goType = "`"+c.Type+"`", "`"+c.GoType.String()+"`"
			if o := columns[fmt.Sprintf("probe.Defaults.o%d", i)]; o != nil {
				r.goOptional = "`" + o.GoType.String() + "`"
			}
		}
		// Merge rows that map identically, keeping the first one's place.
		if j := slices.Index(results, r); j >= 0 {
			labels[j] = append(labels[j], p.label)
			continue
		}
		results = append(results, r)
		labels = append(labels, []string{p.label})
	}

	var b strings.Builder
	b.WriteString("| Proto (+ `format`) | Postgres | Go | Go when `optional` |\n|---|---|---|---|\n")
	for i, r := range results {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", strings.Join(labels[i], ", "), r.pg, r.goType, r.goOptional)
	}
	return b.String()
}

func overrideTable(columns map[string]*pg.Column, types protoreflect.EnumValueDescriptors) string {
	var b strings.Builder
	b.WriteString("| Proto | Allowed `type` values |\n|---|---|\n")
	for k, kind := range _overrideKinds {
		var allowed []string
		for t := 1; t < types.Len(); t++ {
			if columns[fmt.Sprintf("probe.Overrides.k%d_t%d", k, t)] != nil {
				allowed = append(allowed, "`"+string(types.Get(t).Name())+"`")
			}
		}
		if len(allowed) == 0 {
			allowed = []string{"none"}
		}
		fmt.Fprintf(&b, "| %s | %s |\n", kind.label, strings.Join(allowed, ", "))
	}
	return b.String()
}
