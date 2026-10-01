package ir

import (
	"encoding/json"
	"fmt"
)

// Spec is the access pattern of a query. It is sealed: exactly one of Get,
// List, Create, Update, Delete and Upsert, mirroring the dal.v1.Query oneof.
type Spec interface {
	// Kind names the access pattern, e.g. "list".
	Kind() string
	spec()
}

// Get fetches at most one row by a unique key.
type Get struct {
	By          FieldRefs   `json:"by"` // defaults to the primary key
	Consistency Consistency `json:"consistency"`
}

// List pages through rows matching equality filters with keyset pagination.
type List struct {
	Eq      []string `json:"eq,omitempty"`
	Range   string   `json:"range,omitempty"`
	OrderBy Sort     `json:"order_by"` // defaults to the range field, else the primary key minus eq
	// Page sizes; zero means the project default.
	DefaultPageSize uint32      `json:"default_page_size,omitempty"`
	MaxPageSize     uint32      `json:"max_page_size,omitempty"`
	Consistency     Consistency `json:"consistency"`
}

// Create inserts one row.
type Create struct{}

// Update modifies one row by primary key.
type Update struct {
	Columns FieldRefs `json:"columns"` // defaults to active, non-key, role-free fields
}

// Delete removes one row by primary key.
type Delete struct{}

// Upsert inserts one row, or updates it on conflict.
type Upsert struct {
	ConflictOn FieldRefs `json:"conflict_on"` // defaults to the primary key
	Columns    FieldRefs `json:"columns"`     // defaults like Update.Columns
}

// Kind implements Spec.
func (*Get) Kind() string { return "get" }

// Kind implements Spec.
func (*List) Kind() string { return "list" }

// Kind implements Spec.
func (*Create) Kind() string { return "create" }

// Kind implements Spec.
func (*Update) Kind() string { return "update" }

// Kind implements Spec.
func (*Delete) Kind() string { return "delete" }

// Kind implements Spec.
func (*Upsert) Kind() string { return "upsert" }

func (*Get) spec()    {}
func (*List) spec()   {}
func (*Create) spec() {}
func (*Update) spec() {}
func (*Delete) spec() {}
func (*Upsert) spec() {}

// MarshalJSON writes the spec under its kind, e.g. {"list": {...}}, so the
// sealed interface stays readable in dumps.
func (q *Query) MarshalJSON() ([]byte, error) {
	type plain Query // drops the method set, avoiding recursion
	if q.Spec == nil {
		return nil, fmt.Errorf("query %s has no spec", q.Method)
	}
	return json.Marshal(struct {
		*plain
		Spec map[string]Spec `json:"spec"`
	}{
		plain: (*plain)(q),
		Spec:  map[string]Spec{q.Spec.Kind(): q.Spec},
	})
}
