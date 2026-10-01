// Package ir is the backend-neutral model of a DALForge IDL. The loader
// (internal/idl) builds it from protobuf sources; every later stage reads only
// this model, so nothing here may depend on protobuf.
//
// The loader applies defaults that need nothing beyond the core IDL, such as
// Get by primary key, and marks each defaulted value with a Source so lint can
// tell an explicit choice from a fallback. Column references are column-name
// strings; Entity.Column resolves them, and validation guarantees they
// resolve before any emitter runs.
package ir

import "fmt"

// Schema is the whole IDL: every entity and every store.
type Schema struct {
	Entities []*Entity `json:"entities"` // sorted by FullName
	Stores   []*Store  `json:"stores"`   // sorted by FullName
}

// Entity returns the entity with the given full name, or nil.
func (s *Schema) Entity(fullName string) *Entity {
	for _, e := range s.Entities {
		if e.FullName == fullName {
			return e
		}
	}
	return nil
}

// Pos is a source position, 1-based.
type Pos struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

func (p Pos) String() string {
	return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Col)
}

// Entity is a message stored as a table.
type Entity struct {
	FullName    string   `json:"full_name"` // e.g. "orders.v1.Order"
	Table       string   `json:"table"`
	TableSource Source   `json:"table_source"`
	Fields      []*Field `json:"fields"`              // declaration order
	ShardKey    []string `json:"shard_key,omitempty"` // column names
	Reserved    Reserved `json:"reserved"`
	Pos         Pos      `json:"pos"`
}

// Column returns the field stored in the named column, or nil.
func (e *Entity) Column(name string) *Field {
	for _, f := range e.Fields {
		if f.Column == name {
			return f
		}
	}
	return nil
}

// PrimaryKey returns the primary-key column names in declaration order.
func (e *Entity) PrimaryKey() []string {
	var pk []string
	for _, f := range e.Fields {
		if f.PrimaryKey {
			pk = append(pk, f.Column)
		}
	}
	return pk
}

// Reserved holds the field numbers and names an entity has retired. Numbers
// are inclusive ranges.
type Reserved struct {
	Numbers [][2]int32 `json:"numbers,omitempty"`
	Names   []string   `json:"names,omitempty"`
}

// Field is a column of an entity.
type Field struct {
	Column     string `json:"column"`
	Number     int32  `json:"number"` // the column's identity across releases
	Kind       Kind   `json:"kind"`
	Format     Format `json:"format,omitempty"`
	Repeated   bool   `json:"repeated,omitempty"`
	Nullable   bool   `json:"nullable,omitempty"` // declared with proto3 `optional`
	Enum       *Enum  `json:"enum,omitempty"`     // set when Kind is KindEnum
	PrimaryKey bool   `json:"primary_key,omitempty"`
	Unique     bool   `json:"unique,omitempty"`
	Role       Role   `json:"role,omitempty"`
	State      State  `json:"state"`
	Pos        Pos    `json:"pos"`
}

// Enum is the enum type of a field.
type Enum struct {
	FullName string   `json:"full_name"`
	Values   []string `json:"values"` // value names in declaration order
}

// Store is a service declaring the access patterns of one entity.
type Store struct {
	FullName string   `json:"full_name"` // e.g. "orders.v1.OrderStore"
	Entity   string   `json:"entity"`    // entity full name
	Queries  []*Query `json:"queries"`   // declaration order
	Pos      Pos      `json:"pos"`
}

// Query is one rpc of a store: one access pattern.
type Query struct {
	Method   string   `json:"method"`
	Request  *Message `json:"request"`
	Response *Message `json:"response"`
	Spec     Spec     `json:"-"` // see MarshalJSON
	Pos      Pos      `json:"pos"`
}

// Message is the shape of an rpc request or response.
type Message struct {
	FullName string `json:"full_name"`
	// Entity is true when the message is itself an entity; Fields is then
	// omitted, as the entity describes them.
	Entity bool            `json:"entity,omitempty"`
	Fields []*MessageField `json:"fields,omitempty"`
}

// MessageField is a field of a request or response message.
type MessageField struct {
	Name     string `json:"name"`
	Number   int32  `json:"number"`
	Kind     Kind   `json:"kind"`
	Repeated bool   `json:"repeated,omitempty"`
	Nullable bool   `json:"nullable,omitempty"`
	Message  string `json:"message,omitempty"` // full name, when Kind is KindMessage
	Enum     string `json:"enum,omitempty"`    // full name, when Kind is KindEnum
}

// Columns is a set of column names and where it came from.
type Columns struct {
	Names  []string `json:"names"`
	Source Source   `json:"source"`
}

// Sort is an ordered list of sort columns and where it came from.
type Sort struct {
	Columns []SortColumn `json:"columns"`
	Source  Source       `json:"source"`
}

// SortColumn is one column of a sort order.
type SortColumn struct {
	Column string `json:"column"`
	Desc   bool   `json:"desc,omitempty"`
}
