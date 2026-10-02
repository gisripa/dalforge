package pg

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/gisripa/dalforge/internal/diag"
	"github.com/gisripa/dalforge/internal/ir"
)

// emitQueries renders one sqlc query file per table, covering every store of
// that table's entity. Parameters are named after proto fields (@account_id),
// which is also what request messages use (DAL107).
func emitQueries(s *ir.Schema, m *Schema) (map[string][]byte, diag.List) {
	var diags diag.List
	files := map[string]*strings.Builder{}
	names := map[string]string{} // sqlc query name → "Store.Method", for DAL205

	stores := slices.Clone(s.Stores)
	slices.SortFunc(stores, func(a, b *ir.Store) int { return cmp.Compare(a.FullName, b.FullName) })
	for _, st := range stores {
		e := s.Entity(st.Entity)
		t := m.table(st.Entity)
		if e == nil || t == nil {
			continue // Build reported why
		}
		b, ok := files[t.Name]
		if !ok {
			b = &strings.Builder{}
			b.WriteString(_header)
			fmt.Fprintf(b, "-- Queries on %s. Write your own in the custom queries directory, never here.\n", t.Name)
			files[t.Name] = b
		}

		short := strings.TrimSuffix(st.FullName[strings.LastIndex(st.FullName, ".")+1:], "Store")
		for _, q := range st.Queries {
			name := short + q.Method
			origin := fmt.Sprintf("%s.%s", st.FullName, q.Method)
			if prev, dup := names[name]; dup {
				diags.Add(RuleQueryName, diag.Error, q.Pos, "rpc %s and %s both generate the sqlc query %s; rename one rpc or store", origin, prev, name)
				continue
			}
			names[name] = origin

			fmt.Fprintf(b, "\n-- %s (%s)\n", origin, q.Spec.Kind())
			qb := queryBuilder{e: e, t: t, q: q, path: m.Paths[storeShortName(st)+"."+q.Method]}
			if _, ok := q.Spec.(*ir.List); ok {
				// The cursor variant is a second sqlc query.
				after := name + "After"
				if prev, dup := names[after]; dup {
					diags.Add(RuleQueryName, diag.Error, q.Pos, "rpc %s generates the sqlc query %s, which %s also generates; rename one rpc", origin, after, prev)
					continue
				}
				names[after] = origin
			}
			sql, err := qb.render(name)
			if err != nil {
				diags.Add(_ruleInternal, diag.Error, q.Pos, "rpc %s: %v (Emit needs a model Build reported no errors for)", origin, err)
				continue
			}
			b.WriteString(sql)
		}

		// A versioned update that changes zero rows is either a missing row
		// or a stale version; the repository tells them apart with this
		// existence check, run only on that failure path.
		if needsExists(st, e) {
			name := short + "Exists"
			if prev, dup := names[name]; dup {
				diags.Add(RuleQueryName, diag.Error, st.Pos, "the internal sqlc query %s (version-conflict check of %s) collides with %s; rename the rpc", name, st.FullName, prev)
				continue
			}
			names[name] = st.FullName + " (internal exists check)"
			qb := queryBuilder{e: e, t: t}
			conds, err := qb.match(e.PrimaryKey())
			if err != nil {
				diags.Add(_ruleInternal, diag.Error, st.Pos, "%v", err)
				continue
			}
			fmt.Fprintf(b, "\n-- %s: existence check, used to tell a stale version from a missing row\n", st.FullName)
			fmt.Fprintf(b, "-- name: %s :one\nSELECT EXISTS (\n  SELECT 1 FROM %s\n  %s\n);\n", name, t.Name, strings.ReplaceAll(where(conds), "\n", "\n  "))
		}
	}

	out := make(map[string][]byte, len(files))
	for table, b := range files {
		out[table] = []byte(b.String())
	}
	return out, diags
}

func (m *Schema) table(entity string) *Table {
	for _, t := range m.Tables {
		if t.Entity == entity {
			return t
		}
	}
	return nil
}

type queryBuilder struct {
	e    *ir.Entity
	t    *Table
	q    *ir.Query
	path *AccessPath // List only
}

func (qb queryBuilder) render(name string) (string, error) {
	switch s := qb.q.Spec.(type) {
	case *ir.Get:
		return qb.get(name, s)
	case *ir.Create:
		return qb.create(name)
	case *ir.Update:
		return qb.update(name, s)
	case *ir.Delete:
		return qb.delete(name)
	case *ir.List:
		return qb.list(name, s)
	case *ir.Upsert:
		return qb.upsert(name, s)
	}
	return "", fmt.Errorf("unsupported access pattern %s", qb.q.Spec.Kind())
}

// columns returns all column names, in table order: generated queries always
// return whole rows, so sqlc maps them to the table's model struct.
func (qb queryBuilder) columns() string {
	names := make([]string, len(qb.t.Columns))
	for i, c := range qb.t.Columns {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

// match renders "col = @field" for each field, plus the live-row filter on
// soft-delete tables.
func (qb queryBuilder) match(fields []string) ([]string, error) {
	conds := make([]string, 0, len(fields)+1)
	for _, f := range fields {
		c := column(qb.t, f)
		if c == nil {
			return nil, fmt.Errorf("field %q has no column", f)
		}
		conds = append(conds, fmt.Sprintf("%s = @%s", c.Name, f))
	}
	if soft := roleColumn(qb.e, qb.t, ir.RoleDeleteTime); soft != "" {
		conds = append(conds, soft+" IS NULL")
	}
	return conds, nil
}

func where(conds []string) string {
	return "WHERE " + strings.Join(conds, "\n  AND ")
}

func (qb queryBuilder) get(name string, s *ir.Get) (string, error) {
	conds, err := qb.match(s.By.Names)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("-- name: %s :one\nSELECT %s\nFROM %s\n%s;\n", name, qb.columns(), qb.t.Name, where(conds)), nil
}

func (qb queryBuilder) create(name string) (string, error) {
	var cols, vals []string
	for _, f := range qb.e.Fields {
		c := column(qb.t, f.Name)
		if c == nil {
			return "", fmt.Errorf("field %q has no column", f.Name)
		}
		switch f.Role {
		case ir.RoleNone:
			// Every insert parameter is nullable (a pointer in Go), so "unset"
			// is representable: nil uses the column default when there is one,
			// is NULL for an optional column, and is rejected by the DAL for a
			// required column without a default (design §7).
			// The cast makes sqlc type the parameter from the cast rather than
			// the column (whose override would drop the nullability); the
			// nullable db_type overrides in sqlc.yaml then map it to a pointer.
			val := fmt.Sprintf("sqlc.narg(%s)::%s", f.Name, c.Type)
			if c.Default != "" {
				val = fmt.Sprintf("COALESCE(%s, %s)", val, c.Default)
			}
			cols, vals = append(cols, c.Name), append(vals, val)
		case ir.RoleCreateTime, ir.RoleUpdateTime:
			cols, vals = append(cols, c.Name), append(vals, "now()")
		case ir.RoleVersion:
			cols, vals = append(cols, c.Name), append(vals, "1")
		case ir.RoleDeleteTime:
			// a new row is live: deleted_at stays NULL
		}
	}
	return fmt.Sprintf("-- name: %s :one\nINSERT INTO %s (%s)\nVALUES (%s)\nRETURNING %s;\n",
		name, qb.t.Name, strings.Join(cols, ", "), strings.Join(vals, ", "), qb.columns()), nil
}

// managed returns the SET clauses for role columns on any write: update time
// is refreshed and the version bumped, so concurrent CAS writers notice.
func (qb queryBuilder) managed() []string {
	var sets []string
	if c := roleColumn(qb.e, qb.t, ir.RoleUpdateTime); c != "" {
		sets = append(sets, c+" = now()")
	}
	if c := roleColumn(qb.e, qb.t, ir.RoleVersion); c != "" {
		sets = append(sets, fmt.Sprintf("%s = %s + 1", c, c))
	}
	return sets
}

func (qb queryBuilder) update(name string, s *ir.Update) (string, error) {
	var sets []string
	for _, f := range s.Columns.Names {
		c := column(qb.t, f)
		if c == nil {
			return "", fmt.Errorf("field %q has no column", f)
		}
		// Like Create: a cast, nullable parameter, so every SET field is a
		// pointer in Go. nil on a required field is rejected by the DAL
		// before this runs; nil on an optional field sets NULL.
		sets = append(sets, fmt.Sprintf("%s = sqlc.narg(%s)::%s", c.Name, f, c.Type))
	}
	sets = append(sets, qb.managed()...)

	conds, err := qb.match(qb.e.PrimaryKey())
	if err != nil {
		return "", err
	}
	// Optimistic locking: with a ROLE_VERSION field every update is a
	// compare-and-swap on the version the caller read.
	for _, f := range qb.e.Fields {
		if c := column(qb.t, f.Name); f.Role == ir.RoleVersion && c != nil {
			conds = append(conds, fmt.Sprintf("%s = @%s", c.Name, f.Name))
		}
	}
	return fmt.Sprintf("-- name: %s :one\nUPDATE %s\nSET %s\n%s\nRETURNING %s;\n",
		name, qb.t.Name, strings.Join(sets, ",\n    "), where(conds), qb.columns()), nil
}

func (qb queryBuilder) delete(name string) (string, error) {
	conds, err := qb.match(qb.e.PrimaryKey())
	if err != nil {
		return "", err
	}
	// Return the row when the rpc returns the entity; otherwise report how
	// many rows changed, so the DAL can map zero to ErrNotFound.
	cmd, returning := ":execrows", ""
	if qb.q.Response.Entity {
		cmd, returning = ":one", "\nRETURNING "+qb.columns()
	}

	if soft := roleColumn(qb.e, qb.t, ir.RoleDeleteTime); soft != "" {
		sets := append([]string{soft + " = now()"}, qb.managed()...)
		return fmt.Sprintf("-- name: %s %s\nUPDATE %s\nSET %s\n%s%s;\n",
			name, cmd, qb.t.Name, strings.Join(sets, ",\n    "), where(conds), returning), nil
	}
	return fmt.Sprintf("-- name: %s %s\nDELETE FROM %s\n%s%s;\n", name, cmd, qb.t.Name, where(conds), returning), nil
}

// needsExists reports whether a store has an update on a versioned entity.
func needsExists(st *ir.Store, e *ir.Entity) bool {
	hasVersion := slices.ContainsFunc(e.Fields, func(f *ir.Field) bool { return f.Role == ir.RoleVersion })
	hasUpdate := slices.ContainsFunc(st.Queries, func(q *ir.Query) bool { return q.Spec.Kind() == "update" })
	return hasVersion && hasUpdate
}

// list renders a keyset-paginated List as two queries (design §6): the
// first page, and the page after a cursor. Two fixed shapes, rather than one
// query with "(@cursor IS NULL OR …)", keep every plan, including Postgres's
// generic plans for prepared statements, able to use the index.
func (qb queryBuilder) list(name string, s *ir.List) (string, error) {
	if qb.path == nil {
		return "", fmt.Errorf("no access path")
	}
	conds, err := qb.match(s.Eq)
	if err != nil {
		return "", err
	}
	if s.Range != "" {
		c := column(qb.t, s.Range)
		if c == nil {
			return "", fmt.Errorf("field %q has no column", s.Range)
		}
		// Half-open, both bounds required (design §5).
		conds = append(conds,
			fmt.Sprintf("%s >= sqlc.arg(%s_from)::%s", c.Name, s.Range, c.Type),
			fmt.Sprintf("%s < sqlc.arg(%s_to)::%s", c.Name, s.Range, c.Type))
	}

	var order []string
	for _, k := range qb.path.Keys {
		c := column(qb.t, k.Field)
		if c == nil {
			return "", fmt.Errorf("field %q has no column", k.Field)
		}
		dir := ""
		if k.Desc {
			dir = " DESC"
		}
		order = append(order, c.Name+dir)
	}
	tail := fmt.Sprintf("ORDER BY %s\nLIMIT sqlc.arg(page_limit)::int", strings.Join(order, ", "))

	first := fmt.Sprintf("-- name: %s :many\nSELECT %s\nFROM %s\n%s\n%s;\n", name, qb.columns(), qb.t.Name, where(conds), tail)
	cursor, err := qb.cursor()
	if err != nil {
		return "", err
	}
	next := fmt.Sprintf("\n-- name: %sAfter :many\nSELECT %s\nFROM %s\n%s\n%s;\n", name, qb.columns(), qb.t.Name, where(append(conds, cursor)), tail)
	return first + next, nil
}

// cursor renders "the rows after the cursor" for the path's keyset: one row
// comparison when all keys share a direction, else the expanded OR form.
func (qb queryBuilder) cursor() (string, error) {
	type key struct{ col, arg string }
	var keys []key
	for _, k := range qb.path.Keys {
		c := column(qb.t, k.Field)
		if c == nil {
			return "", fmt.Errorf("field %q has no column", k.Field)
		}
		keys = append(keys, key{col: c.Name, arg: fmt.Sprintf("sqlc.arg(after_%s)::%s", k.Field, c.Type)})
	}
	op := func(desc bool) string {
		if desc {
			return "<"
		}
		return ">"
	}
	if !qb.path.Mixed {
		cols, args := make([]string, len(keys)), make([]string, len(keys))
		for i, k := range keys {
			cols[i], args[i] = k.col, k.arg
		}
		return fmt.Sprintf("(%s) %s (%s)", strings.Join(cols, ", "), op(qb.path.Keys[0].Desc), strings.Join(args, ", ")), nil
	}
	var ors []string
	for i, k := range keys {
		var ands []string
		for _, prev := range keys[:i] {
			ands = append(ands, prev.col+" = "+prev.arg)
		}
		ands = append(ands, fmt.Sprintf("%s %s %s", k.col, op(qb.path.Keys[i].Desc), k.arg))
		ors = append(ors, "("+strings.Join(ands, " AND ")+")")
	}
	return "(" + strings.Join(ors, "\n    OR ") + ")", nil
}

// upsert inserts a row or, on conflict, updates it: the last writer wins
// (the version is bumped, no compare-and-swap), and a soft-deleted row is
// never revived: the conflict update is skipped, so no row returns and the
// DAL reports ErrAlreadyExists.
func (qb queryBuilder) upsert(name string, s *ir.Upsert) (string, error) {
	insert, err := qb.create(name)
	if err != nil {
		return "", err
	}
	insert = strings.TrimSuffix(insert, ";\n")
	returning := insert[strings.LastIndex(insert, "\nRETURNING "):]
	insert = strings.TrimSuffix(insert, returning)

	var target []string
	for _, f := range s.ConflictOn.Names {
		c := column(qb.t, f)
		if c == nil {
			return "", fmt.Errorf("field %q has no column", f)
		}
		target = append(target, c.Name)
	}
	soft := roleColumn(qb.e, qb.t, ir.RoleDeleteTime)
	conflict := fmt.Sprintf("ON CONFLICT (%s)", strings.Join(target, ", "))
	// A unique non-key field on a soft-delete table is a partial unique
	// index (live rows only); the conflict target must name its predicate.
	if soft != "" && !sameSet(s.ConflictOn.Names, qb.e.PrimaryKey()) {
		conflict += fmt.Sprintf(" WHERE %s IS NULL", soft)
	}

	var sets []string
	for _, f := range s.Columns.Names {
		c := column(qb.t, f)
		if c == nil {
			return "", fmt.Errorf("field %q has no column", f)
		}
		sets = append(sets, fmt.Sprintf("%s = EXCLUDED.%s", c.Name, c.Name))
	}
	for _, m := range qb.managed() {
		// version = version + 1 must name the existing row's version.
		if strings.Contains(m, "+ 1") {
			col := strings.SplitN(m, " ", 2)[0]
			m = fmt.Sprintf("%s = %s.%s + 1", col, qb.t.Name, col)
		}
		sets = append(sets, m)
	}
	out := fmt.Sprintf("%s\n%s DO UPDATE\nSET %s", insert, conflict, strings.Join(sets, ",\n    "))
	if soft != "" {
		out += fmt.Sprintf("\nWHERE %s.%s IS NULL", qb.t.Name, soft)
	}
	return out + returning + ";\n", nil
}
