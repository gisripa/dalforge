// Package pg is the Postgres backend: it turns the core IR plus the pg hints
// into a physical model (tables, columns, SQL and Go types, indexes) and
// reports the Postgres rules (DAL2xx). Emitters read only this model.
package pg

import (
	"strings"

	"github.com/gisripa/dalforge/internal/ir"
)

// Floor is the oldest Postgres major version dalforge supports.
const Floor = 16

// Target describes the Postgres the schema is deployed to (dalforge.yaml
// pg.version). The zero value means Floor.
type Target struct {
	Major int
}

func (t Target) major() int {
	if t.Major == 0 {
		return Floor
	}
	return t.Major
}

// Schema is the physical model of an IDL on Postgres.
type Schema struct {
	Extensions []string `json:"extensions,omitempty"` // sorted, deduplicated
	Tables     []*Table `json:"tables"`               // sorted by Name
}

// Table is one entity's table.
type Table struct {
	Name        string    `json:"name"`
	Entity      string    `json:"entity"` // IR entity full name
	Columns     []*Column `json:"columns"`
	PrimaryKey  []string  `json:"primary_key"` // column names
	Indexes     []*Index  `json:"indexes,omitempty"`
	IndexBudget uint32    `json:"index_budget,omitempty"`
	Pos         ir.Pos    `json:"pos"`
}

// Column is one field's column.
type Column struct {
	Name    string `json:"name"`
	Field   string `json:"field"` // proto field name
	Number  int32  `json:"number"`
	Type    string `json:"type"` // SQL type, e.g. "uuid", "text[]", "vector(3)"
	NotNull bool   `json:"not_null,omitempty"`
	Default string `json:"default,omitempty"` // SQL expression
	Unique  bool   `json:"unique,omitempty"`
	GoType  GoType `json:"go_type"`
}

// Index is an index on a table.
type Index struct {
	Name    string        `json:"name,omitempty"`
	Columns []IndexColumn `json:"columns"`
	Include []string      `json:"include,omitempty"` // column names
	Where   string        `json:"where,omitempty"`
	Unique  bool          `json:"unique,omitempty"`
}

// IndexColumn is one key column of an index.
type IndexColumn struct {
	Column string `json:"column"`
	Desc   bool   `json:"desc,omitempty"`
}

// GoType is the Go type of a column in sqlc's models, emitted as a per-column
// sqlc override so no driver (pgtype) type reaches the generated API.
type GoType struct {
	Import  string `json:"import,omitempty"` // e.g. "github.com/google/uuid"; empty for builtins
	Name    string `json:"name"`             // e.g. "UUID", "string", "[]byte"
	Pointer bool   `json:"pointer,omitempty"`
	Slice   bool   `json:"slice,omitempty"`
}

// String renders the type as written in Go source, e.g. "*time.Time" or
// "[]uuid.UUID".
func (g GoType) String() string {
	var b strings.Builder
	if g.Pointer {
		b.WriteByte('*')
	}
	if g.Slice {
		b.WriteString("[]")
	}
	if g.Import != "" {
		b.WriteString(pkgName(g.Import))
		b.WriteByte('.')
	}
	b.WriteString(g.Name)
	return b.String()
}

// pkgName is the identifier a Go file uses for an import: its last path
// element with characters invalid in identifiers replaced by '_', as sqlc
// does ("github.com/pgvector/pgvector-go" → "pgvector_go").
func pkgName(importPath string) string {
	last := importPath[strings.LastIndex(importPath, "/")+1:]
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '.' {
			return '_'
		}
		return r
	}, last)
}
