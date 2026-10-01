// Package pgir holds the Postgres hints of an IDL (the dal.pg.v1 options),
// kept apart from the backend-neutral core IR and keyed into it. Like the
// core IR it is plain Go; only the loader touches protobuf.
package pgir

import "github.com/gisripa/dalforge/internal/ir"

// Hints are the Postgres options of a schema. Only files, tables and columns
// that set a dal.pg.v1 option appear.
type Hints struct {
	Files  map[string]*File  `json:"files,omitempty"`  // by proto file path
	Tables map[string]*Table `json:"tables,omitempty"` // by entity full name
}

// Table returns the hints for an entity, or an empty Table.
func (h *Hints) Table(entity string) *Table {
	if t, ok := h.Tables[entity]; ok {
		return t
	}
	return &Table{}
}

// File holds what a file's schema needs from Postgres.
type File struct {
	MinVersion uint32   `json:"min_version,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
}

// Table holds an entity's Postgres hints.
type Table struct {
	Indexes     []*Index          `json:"indexes,omitempty"`
	IndexBudget uint32            `json:"index_budget,omitempty"`
	Columns     map[int32]*Column `json:"columns,omitempty"` // by field number
}

// Column returns the hints for a field number, or an empty Column.
func (t *Table) Column(number int32) *Column {
	if c, ok := t.Columns[number]; ok {
		return c
	}
	return &Column{}
}

// Index is an explicitly declared index.
type Index struct {
	Name    string          `json:"name,omitempty"`
	Columns []ir.SortColumn `json:"columns"`
	Include []string        `json:"include,omitempty"`
	Where   string          `json:"where,omitempty"`
	Unique  bool            `json:"unique,omitempty"`
}

// Column holds a column's Postgres hints. An empty Type and CustomType mean
// the type is inferred from the core kind and format.
type Column struct {
	Type       string `json:"type,omitempty"` // SQL name, e.g. "uuid", "double precision"
	CustomType string `json:"custom_type,omitempty"`
	Default    string `json:"default,omitempty"`
	GoType     string `json:"go_type,omitempty"`
}
