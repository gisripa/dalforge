// Package check validates the core IR: the backend-neutral rules that must
// pass before any emitter runs. Every rule reports through diag.
//
//	DAL107  request/response messages match the declared access pattern
//	DAL110  shard_key names existing, active fields
//	DAL117  query references name existing, active fields, once each
//	DAL118  update/upsert never set key or role-managed fields
package check

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gisripa/dalforge/internal/diag"
	"github.com/gisripa/dalforge/internal/ir"
)

// Rule IDs.
const (
	RuleShape      = "DAL107"
	RuleShardKey   = "DAL110"
	RuleReference  = "DAL117"
	RuleWriteField = "DAL118"
)

// Schema runs every core rule and returns the findings, sorted.
func Schema(s *ir.Schema) diag.List {
	var l diag.List
	for _, e := range s.Entities {
		shardKey(&l, e)
	}
	for _, st := range s.Stores {
		e := s.Entity(st.Entity)
		if e == nil {
			continue // the loader guarantees this; be defensive
		}
		for _, q := range st.Queries {
			references(&l, e, q)
			writeFields(&l, e, q)
			shapes(&l, e, q)
		}
	}
	l.Sort()
	return l
}

func shardKey(l *diag.List, e *ir.Entity) {
	refs(l, RuleShardKey, e.Pos, e, "shard_key", e.ShardKey)
}

func references(l *diag.List, e *ir.Entity, q *ir.Query) {
	label := func(what string) string { return fmt.Sprintf("rpc %s %s.%s", q.Method, q.Spec.Kind(), what) }
	switch s := q.Spec.(type) {
	case *ir.Get:
		refs(l, RuleReference, q.Pos, e, label("by"), s.By.Names)
	case *ir.List:
		refs(l, RuleReference, q.Pos, e, label("eq"), s.Eq)
		if s.Range != "" {
			refs(l, RuleReference, q.Pos, e, label("range"), []string{s.Range})
		}
		keys := make([]string, len(s.OrderBy.Keys))
		for i, k := range s.OrderBy.Keys {
			keys[i] = k.Field
		}
		refs(l, RuleReference, q.Pos, e, label("order_by"), keys)
	case *ir.Update:
		refs(l, RuleReference, q.Pos, e, label("columns"), s.Columns.Names)
	case *ir.Upsert:
		refs(l, RuleReference, q.Pos, e, label("conflict_on"), s.ConflictOn.Names)
		refs(l, RuleReference, q.Pos, e, label("columns"), s.Columns.Names)
	}
}

// refs reports names that are unknown, deprecated or repeated.
func refs(l *diag.List, rule string, pos ir.Pos, e *ir.Entity, what string, names []string) {
	seen := map[string]bool{}
	for _, name := range names {
		switch f := e.Field(name); {
		case f == nil:
			l.Add(rule, diag.Error, pos, "%s: %s has no field %q%s", what, e.FullName, name, suggest(e, name))
		case f.State == ir.StateDeprecated:
			l.Add(rule, diag.Error, pos, "%s: field %q is deprecated; deprecated fields are excluded from generated reads and writes", what, name)
		case seen[name]:
			l.Add(rule, diag.Error, pos, "%s: field %q is listed more than once", what, name)
		}
		seen[name] = true
	}
}

// suggest names the field stored in a column called name, since references
// use proto field names, not column names.
func suggest(e *ir.Entity, name string) string {
	return ColumnHint(e, name)
}

// ColumnHint returns a hint when name is a column name rather than a field
// name, or "". Backend rules reuse it for their own references.
func ColumnHint(e *ir.Entity, name string) string {
	if f := e.FieldByColumn(name); f != nil && f.Name != name {
		return fmt.Sprintf(" (%q is the column of field %q; references use field names)", name, f.Name)
	}
	return ""
}

func writeFields(l *diag.List, e *ir.Entity, q *ir.Query) {
	var names []string
	switch s := q.Spec.(type) {
	case *ir.Update:
		names = s.Columns.Names
	case *ir.Upsert:
		names = s.Columns.Names
	default:
		return
	}
	for _, name := range names {
		f := e.Field(name)
		switch {
		case f == nil:
			// reported by DAL117
		case f.PrimaryKey:
			l.Add(RuleWriteField, diag.Error, q.Pos, "rpc %s: %s.columns sets key field %q; keys identify the row and never change", q.Method, q.Spec.Kind(), name)
		case f.Role != ir.RoleNone:
			l.Add(RuleWriteField, diag.Error, q.Pos, "rpc %s: %s.columns sets %q, which the generated code manages (role %s)", q.Method, q.Spec.Kind(), name, f.Role)
		}
	}
}

// want is an expected field of a request or response message.
type want struct {
	name     string
	kind     ir.Kind
	repeated bool
	enum     string // enum full name, when kind is KindEnum
	message  string // message full name, when kind is KindMessage
}

func (w want) String() string {
	var t string
	switch w.kind {
	case ir.KindEnum:
		t = w.enum
	case ir.KindMessage:
		t = w.message
	default:
		t = w.kind.String()
	}
	if w.repeated {
		t = "repeated " + t
	}
	return fmt.Sprintf("%s %s", t, w.name)
}

func shapes(l *diag.List, e *ir.Entity, q *ir.Query) {
	report := func(which string, m *ir.Message, problems []string) {
		if len(problems) > 0 {
			l.Add(RuleShape, diag.Error, q.Pos, "rpc %s %s %s: %s", q.Method, which, m.FullName, strings.Join(problems, "; "))
		}
	}
	isEntity := func(m *ir.Message) bool { return m.Entity && m.FullName == e.FullName }
	entityOnly := func(m *ir.Message) []string {
		if isEntity(m) {
			return nil
		}
		return []string{fmt.Sprintf("must be the entity %s", e.FullName)}
	}
	entityOr := func(m *ir.Message, fields []want) []string {
		if isEntity(m) {
			return nil
		}
		return compare(m, fields)
	}

	switch s := q.Spec.(type) {
	case *ir.Get:
		report("request", q.Request, entityOr(q.Request, fieldsOf(e, s.By.Names)))
		report("response", q.Response, entityOnly(q.Response))
	case *ir.List:
		req := fieldsOf(e, s.Eq)
		if f := e.Field(s.Range); f != nil {
			for _, suffix := range []string{"_from", "_to"} {
				w := wantOf(f)
				w.name = s.Range + suffix
				req = append(req, w)
			}
		}
		req = append(req, want{name: "page_size", kind: ir.KindInt32}, want{name: "page_token", kind: ir.KindString})
		report("request", q.Request, compare(q.Request, req))
		report("response", q.Response, listResponse(e, q.Response))
	case *ir.Create, *ir.Update, *ir.Upsert:
		report("request", q.Request, entityOnly(q.Request))
		report("response", q.Response, entityOnly(q.Response))
	case *ir.Delete:
		report("request", q.Request, entityOr(q.Request, fieldsOf(e, e.PrimaryKey())))
		if !isEntity(q.Response) && q.Response.FullName != "google.protobuf.Empty" {
			report("response", q.Response, []string{fmt.Sprintf("must be the entity %s or google.protobuf.Empty", e.FullName)})
		}
	}
}

// listResponse expects exactly one repeated entity field plus a string
// next_page_token.
func listResponse(e *ir.Entity, m *ir.Message) []string {
	if m.Entity {
		return []string{fmt.Sprintf("must be a page message, not an entity: repeated %s <name> and string next_page_token", e.FullName)}
	}
	var problems []string
	items := 0
	hasToken := false
	for _, f := range m.Fields {
		switch {
		case f.Kind == ir.KindMessage && f.Message == e.FullName && f.Repeated:
			items++
		case f.Name == "next_page_token" && f.Kind == ir.KindString && !f.Repeated:
			hasToken = true
		default:
			problems = append(problems, fmt.Sprintf("unexpected field %s", f.Name))
		}
	}
	if items != 1 {
		problems = append(problems, fmt.Sprintf("want exactly one repeated %s field, found %d", e.FullName, items))
	}
	if !hasToken {
		problems = append(problems, "missing field string next_page_token")
	}
	return problems
}

func wantOf(f *ir.Field) want {
	w := want{name: f.Name, kind: f.Kind, repeated: f.Repeated}
	if f.Enum != nil {
		w.enum = f.Enum.FullName
	}
	return w
}

// fieldsOf returns the expected request fields for entity fields. Unknown
// names are skipped; DAL117 reports them.
func fieldsOf(e *ir.Entity, names []string) []want {
	var ws []want
	for _, name := range names {
		if f := e.Field(name); f != nil {
			ws = append(ws, wantOf(f))
		}
	}
	return ws
}

// compare checks that m has exactly the wanted fields (by name), with
// matching types. Field order and numbers are free.
func compare(m *ir.Message, wants []want) []string {
	if m.Entity {
		var names []string
		for _, w := range wants {
			names = append(names, w.String())
		}
		return []string{fmt.Sprintf("is an entity; want a message with exactly: %s", strings.Join(names, ", "))}
	}

	var problems []string
	got := map[string]*ir.MessageField{}
	for _, f := range m.Fields {
		got[f.Name] = f
	}
	wanted := map[string]bool{}
	for _, w := range wants {
		wanted[w.name] = true
		f, ok := got[w.name]
		if !ok {
			problems = append(problems, "missing field "+w.String())
			continue
		}
		if f.Kind != w.kind || f.Repeated != w.repeated || f.Enum != w.enum || f.Message != w.message {
			gotW := want{name: f.Name, kind: f.Kind, repeated: f.Repeated, enum: f.Enum, message: f.Message}
			problems = append(problems, fmt.Sprintf("field %s is %s, want %s", w.name, strings.TrimSuffix(gotW.String(), " "+f.Name), strings.TrimSuffix(w.String(), " "+w.name)))
		}
	}
	for _, f := range m.Fields {
		if !wanted[f.Name] {
			problems = append(problems, "unexpected field "+f.Name)
		}
	}
	slices.Sort(problems)
	return problems
}
