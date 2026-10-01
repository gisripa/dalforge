package pg

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gisripa/dalforge/internal/ir"
)

// sqlType is the default SQL type of a non-repeated field (design §4).
func sqlType(f *ir.Field) (string, error) {
	if f.Format != ir.FormatNone && f.Kind != ir.KindString {
		return "", fmt.Errorf("format %s applies only to string fields, not %s", f.Format, f.Kind)
	}
	switch f.Kind {
	case ir.KindString:
		switch f.Format {
		case ir.FormatUUID:
			return "uuid", nil
		case ir.FormatDecimal:
			return "numeric", nil
		case ir.FormatJSON:
			return "jsonb", nil
		}
		return "text", nil
	case ir.KindBool:
		return "boolean", nil
	case ir.KindInt32:
		return "integer", nil
	case ir.KindInt64, ir.KindUint32:
		return "bigint", nil
	case ir.KindUint64:
		return "", fmt.Errorf("uint64 has no Postgres type that holds its full range; set (dal.pg.v1.column).custom_type and go_type, or use int64")
	case ir.KindFloat:
		return "real", nil
	case ir.KindDouble:
		return "double precision", nil
	case ir.KindBytes:
		return "bytea", nil
	case ir.KindTimestamp:
		return "timestamptz", nil
	case ir.KindEnum:
		return "text", nil // stored by value name (design §4)
	case ir.KindJSON:
		return "jsonb", nil
	}
	return "", fmt.Errorf("kind %s has no Postgres mapping", f.Kind)
}

// _compatible lists the explicit (dal.pg.v1.column).type values each kind
// accepts. The Go type always follows the field's kind, so these are the
// types pgx can read into it.
var _compatible = map[ir.Kind][]string{
	ir.KindString:    {"text", "uuid", "numeric", "jsonb", "inet"},
	ir.KindBool:      {"boolean"},
	ir.KindInt32:     {"smallint", "integer", "bigint"},
	ir.KindInt64:     {"bigint"},
	ir.KindUint32:    {"bigint"},
	ir.KindFloat:     {"real", "double precision"},
	ir.KindDouble:    {"double precision"},
	ir.KindBytes:     {"bytea"},
	ir.KindTimestamp: {"timestamptz", "date"},
	ir.KindEnum:      {"text"},
	ir.KindJSON:      {"jsonb"},
}

func checkExplicitType(f *ir.Field, typ string) error {
	if !slices.Contains(_compatible[f.Kind], typ) {
		return fmt.Errorf("type %s doesn't fit field kind %s; use one of: %s", typ, f.Kind, strings.Join(_compatible[f.Kind], ", "))
	}
	return nil
}

// goType is the Go type of a column, from the field's kind and format, before
// nullability (see withNull). It never names a pgx/pgtype type.
func goType(f *ir.Field) GoType {
	var g GoType
	switch f.Kind {
	case ir.KindString, ir.KindEnum:
		g = GoType{Name: "string"}
		switch f.Format {
		case ir.FormatUUID:
			g = GoType{Import: "github.com/google/uuid", Name: "UUID"}
		case ir.FormatJSON:
			g = GoType{Import: "encoding/json", Name: "RawMessage"}
		}
		// FormatDecimal stays string until a decimal type is chosen (design §4).
	case ir.KindBool:
		g = GoType{Name: "bool"}
	case ir.KindInt32:
		g = GoType{Name: "int32"}
	case ir.KindInt64, ir.KindUint32:
		g = GoType{Name: "int64"}
	case ir.KindFloat:
		g = GoType{Name: "float32"}
	case ir.KindDouble:
		g = GoType{Name: "float64"}
	case ir.KindBytes:
		g = GoType{Name: "[]byte"}
	case ir.KindTimestamp:
		g = GoType{Import: "time", Name: "Time"}
	case ir.KindJSON:
		// A repeated message is one jsonb array, not a Postgres array.
		return GoType{Import: "encoding/json", Name: "RawMessage"}
	}
	g.Slice = f.Repeated
	return g
}

// withNull makes g hold NULL when the column is nullable: a pointer, except
// for types where nil already means NULL (slices, []byte, json.RawMessage).
func withNull(g GoType, notNull bool) GoType {
	nilable := g.Slice || g.Name == "[]byte" || (g.Import == "encoding/json" && g.Name == "RawMessage")
	g.Pointer = !notNull && !nilable
	return g
}

// parseGoType parses a go_type option: a builtin ("string") or a qualified
// type ("github.com/pgvector/pgvector-go.Vector").
func parseGoType(s string) (GoType, error) {
	i := strings.LastIndex(s, ".")
	if i < 0 {
		if !isIdent(s) {
			return GoType{}, fmt.Errorf("go_type %q is not a Go type name", s)
		}
		return GoType{Name: s}, nil
	}
	pkg, name := s[:i], s[i+1:]
	if pkg == "" || !isIdent(name) || strings.ContainsAny(pkg, " \t") {
		return GoType{}, fmt.Errorf("go_type %q: want \"import/path.Type\" or a builtin type name", s)
	}
	return GoType{Import: pkg, Name: name}, nil
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		ok := r == '_' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || (i > 0 && '0' <= r && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// _extensionTypes maps custom types to the extension that provides them, so
// DAL208 can require the extension to be declared. Unknown custom types are
// trusted.
var _extensionTypes = map[string]string{
	"vector":    "vector",
	"halfvec":   "vector",
	"sparsevec": "vector",
	"geometry":  "postgis",
	"geography": "postgis",
	"citext":    "citext",
	"hstore":    "hstore",
	"ltree":     "ltree",
}

// customBase returns the base name of a custom type: "vector(1536)" and
// "vector[]" both give "vector".
func customBase(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if i := strings.IndexAny(t, "(["); i >= 0 {
		t = t[:i]
	}
	return strings.TrimSpace(t)
}

// _minVersion is the capability table: the Postgres major version that
// introduced each SQL type dalforge can emit. Types absent from the table are
// available in every supported version. Supporting a newer version adds
// rows here; the IDL never changes (design §4).
var _minVersion = map[string]int{}
