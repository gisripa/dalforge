// Package check validates the core IR: the backend-neutral rules that must
// pass before any emitter runs. Every rule reports through diag.
//
//	DAL107  request/response messages match the declared access pattern
//	DAL110  shard_key names existing fields, once each
//	DAL117  query references name existing fields, once each
//	DAL118  update/upsert never set key or role-managed fields
//	DAL101  a list's range is its leading sort field
//	DAL109  (warning) a list sorts by a field some update can change
//	DAL114  (warning) a list's name says what it filters by
//	DAL115  a nullable field sorts a list only as its range
//	DAL119  role fields have the type the role needs, one field per role
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
	RuleRangeSort  = "DAL101"
	RuleMutableKey = "DAL109"
	RuleListName   = "DAL114"
	RuleNullSort   = "DAL115"
	RuleRole       = "DAL119"
)

// Schema runs every core rule and returns the findings, sorted.
func Schema(s *ir.Schema) diag.List {
	var l diag.List
	clean := map[*ir.Query]bool{} // queries without broken references
	for _, e := range s.Entities {
		shardKey(&l, e)
		roles(&l, e)
	}
	for _, st := range s.Stores {
		e := s.Entity(st.Entity)
		if e == nil {
			continue // the loader guarantees this; be defensive
		}
		for _, q := range st.Queries {
			before := len(l)
			references(&l, e, q)
			// A broken reference makes the expected request shape wrong too;
			// skip DAL107 so only the root cause is reported.
			brokenRefs := len(l) > before
			writeFields(&l, e, q)
			if !brokenRefs {
				clean[q] = true
				shapes(&l, e, q)
				if list, ok := q.Spec.(*ir.List); ok {
					lists(&l, e, q, list)
				}
			}
		}
	}
	mutableSortKeys(&l, s, clean)
	l.Sort()
	return l
}

// lists checks one List's access path (design §5).
func lists(l *diag.List, e *ir.Entity, q *ir.Query, s *ir.List) {
	// A B-tree serves one range, after the equality prefix, on the leading
	// sort column; ranging on X while sorting on Y can't use one index.
	if s.Range != "" && (len(s.OrderBy.Keys) == 0 || s.OrderBy.Keys[0].Field != s.Range) {
		l.Add(RuleRangeSort, diag.Error, q.Pos, "rpc %s: range %q must be the leading order_by field (got %s); one index can't serve a range on one field and a sort on another",
			q.Method, s.Range, sortString(s.OrderBy.Keys))
	}
	// Keyset comparisons treat NULL as unknown, so rows with a NULL sort
	// value would be skipped silently. The range filter excludes NULLs.
	for _, k := range s.OrderBy.Keys {
		if f := e.Field(k.Field); f != nil && f.Nullable && k.Field != s.Range {
			l.Add(RuleNullSort, diag.Error, q.Pos, "rpc %s: order_by %q is optional (nullable); keyset paging would skip rows where it is NULL. Make it required, or make it the list's range",
				q.Method, k.Field)
		}
	}
	// The name should say what the list filters by: List…By<Eq…>And<Range>.
	if want := listNamePart(s); want != "" && !nameMatches(q.Method, s) {
		l.Add(RuleListName, diag.Warning, q.Pos, "rpc %s: the name doesn't say what it filters by; expected it to contain %q, e.g. List%sBy%s",
			q.Method, "By"+want, plural(e), want)
	}
}

// listNamePart is the expected filter part of a list's name: its eq fields
// then its range, CamelCased and joined with "And".
func listNamePart(s *ir.List) string {
	var parts []string
	for _, f := range append(slices.Clone(s.Eq), rangeOf(s)...) {
		parts = append(parts, camelField(f))
	}
	return strings.Join(parts, "And")
}

// nameMatches accepts each field with or without a trailing ID, so
// ListByAccount satisfies eq [account_id].
func nameMatches(method string, s *ir.List) bool {
	var alts [][]string
	for _, f := range append(slices.Clone(s.Eq), rangeOf(s)...) {
		c := camelField(f)
		alts = append(alts, []string{c, strings.TrimSuffix(c, "ID")})
	}
	var walk func(i int, acc string) bool
	walk = func(i int, acc string) bool {
		if i == len(alts) {
			return strings.Contains(method, "By"+acc)
		}
		for _, a := range alts[i] {
			next := a
			if acc != "" {
				next = acc + "And" + a
			}
			if walk(i+1, next) {
				return true
			}
		}
		return false
	}
	return walk(0, "")
}

func rangeOf(s *ir.List) []string {
	if s.Range == "" {
		return nil
	}
	return []string{s.Range}
}

func camelField(f string) string {
	var b strings.Builder
	for _, part := range strings.Split(f, "_") {
		if part == "" {
			continue
		}
		if part == "id" {
			b.WriteString("ID")
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

func plural(e *ir.Entity) string {
	name := e.FullName[strings.LastIndex(e.FullName, ".")+1:]
	return name + "s"
}

func sortString(keys []ir.SortKey) string {
	var parts []string
	for _, k := range keys {
		p := k.Field
		if k.Desc {
			p += " DESC"
		}
		parts = append(parts, p)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// mutableSortKeys warns when a list sorts by a field some update or upsert of
// the same entity can change: rows would move between pages while a client
// pages through them.
func mutableSortKeys(l *diag.List, s *ir.Schema, clean map[*ir.Query]bool) {
	writers := map[string]map[string]string{} // entity → field → "Store.Method"
	for _, st := range s.Stores {
		for _, q := range st.Queries {
			var cols []string
			switch spec := q.Spec.(type) {
			case *ir.Update:
				cols = spec.Columns.Names
			case *ir.Upsert:
				cols = spec.Columns.Names
			}
			for _, c := range cols {
				if writers[st.Entity] == nil {
					writers[st.Entity] = map[string]string{}
				}
				if _, ok := writers[st.Entity][c]; !ok {
					writers[st.Entity][c] = q.Method
				}
			}
		}
	}
	for _, st := range s.Stores {
		for _, q := range st.Queries {
			list, ok := q.Spec.(*ir.List)
			if !ok || !clean[q] {
				continue
			}
			for _, k := range list.OrderBy.Keys {
				if by, ok := writers[st.Entity][k.Field]; ok {
					l.Add(RuleMutableKey, diag.Warning, q.Pos, "rpc %s sorts by %q, which rpc %s can change; rows can move between pages while a client pages through them",
						q.Method, k.Field, by)
				}
			}
		}
	}
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

// refs reports names that are unknown or repeated.
func refs(l *diag.List, rule string, pos ir.Pos, e *ir.Entity, what string, names []string) {
	seen := map[string]bool{}
	for _, name := range names {
		switch f := e.Field(name); {
		case f == nil:
			l.Add(rule, diag.Error, pos, "%s: %s has no field %q%s", what, e.FullName, name, suggest(e, name))
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
			l.Add(RuleWriteField, diag.Error, q.Pos, "rpc %s: %s.columns sets %q, which the generated code manages (%s)", q.Method, q.Spec.Kind(), name, roleName(f.Role))
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

// roles checks that each role sits on a field the generated SQL can manage:
// timestamps for the time roles, an optional one for soft delete (live rows
// have no deletion time), a required integer for the version, never a key
// field, and one field per role.
func roles(l *diag.List, e *ir.Entity) {
	seen := map[ir.Role]string{}
	for _, f := range e.Fields {
		if f.Role == ir.RoleNone {
			continue
		}
		if prev, dup := seen[f.Role]; dup {
			l.Add(RuleRole, diag.Error, f.Pos, "field %q: %s already has %s; an entity has at most one field per role", f.Name, prev, roleName(f.Role))
			continue
		}
		seen[f.Role] = f.Name
		var want string
		switch {
		case f.PrimaryKey:
			want = "a non-key field (keys identify the row and never change)"
		case f.Repeated:
			want = "a single value, not a repeated field"
		case f.Role == ir.RoleVersion && (f.Kind != ir.KindInt32 && f.Kind != ir.KindInt64 || f.Nullable):
			want = "a required int64 (or int32): updates increment it"
		case f.Role == ir.RoleDeleteTime && (f.Kind != ir.KindTimestamp || !f.Nullable):
			want = "an optional google.protobuf.Timestamp: live rows have no deletion time, so it must be nullable"
		case (f.Role == ir.RoleCreateTime || f.Role == ir.RoleUpdateTime) && f.Kind != ir.KindTimestamp:
			want = "a google.protobuf.Timestamp"
		default:
			continue
		}
		l.Add(RuleRole, diag.Error, f.Pos, "field %q has %s, which needs %s", f.Name, roleName(f.Role), want)
	}
}

// roleName spells a role as the IDL does, e.g. ROLE_VERSION.
func roleName(r ir.Role) string { return "ROLE_" + strings.ToUpper(r.String()) }
