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
			qb := queryBuilder{e: e, t: t, q: q}
			sql, err := qb.render(name)
			if err != nil {
				diags.Add(_ruleInternal, diag.Error, q.Pos, "rpc %s: %v (Emit needs a model Build reported no errors for)", origin, err)
				continue
			}
			b.WriteString(sql)
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
	e *ir.Entity
	t *Table
	q *ir.Query
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
	case *ir.List, *ir.Upsert:
		// Keyset List (2.3) and Upsert (2.6) arrive in phase 2.
		return fmt.Sprintf("-- %s: not generated yet (%s queries arrive in phase 2).\n", name, s.Kind()), nil
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
		sets = append(sets, fmt.Sprintf("%s = @%s", c.Name, f))
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
