package pg

import (
	"fmt"
	"strings"

	"github.com/gisripa/dalforge/internal/ir"
)

// repository writes the implementation of one store's Repository interface:
// each method is one dalpg.Runner call around one sqlc call. The Runner owns
// pool routing, retries, error mapping and per-attempt context; generated code
// only adds what is specific to the access pattern.
func (g *goFile) repository(st *ir.Store, e *ir.Entity, t *Table, ms []method, sqlcPkg string) {
	short := storeShort(st)
	recv := strings.ToLower(short[:1]) + short[1:] + "Repository"

	fmt.Fprintf(&g.body, "// %s implements %sRepository over sqlc's Queries.\n", recv, short)
	fmt.Fprintf(&g.body, "type %s struct{ run *dalpg.Runner }\n\n", recv)
	fmt.Fprintf(&g.body, "var _ %sRepository = (*%s)(nil)\n\n", short, recv)
	fmt.Fprintf(&g.body, "// New%sRepository returns the %s repository. Build run once at the\n", short, st.FullName)
	fmt.Fprintf(&g.body, "// composition root (dalpg.New) and share it; it holds the pools and retry policy.\n")
	fmt.Fprintf(&g.body, "func New%sRepository(run *dalpg.Runner) %sRepository {\n\treturn &%s{run: run}\n}\n\n", short, short, recv)

	// One dal.Op per rpc, fixed at generation time (design §7).
	g.body.WriteString("var (\n")
	for _, m := range ms {
		if m.pending {
			continue
		}
		fmt.Fprintf(&g.body, "\t%s = dal.Op{Entity: %q, Method: %q, Kind: dal.%s, Idempotent: %t}\n",
			opVar(short, m), modelName(t), m.rpc, opKind(m.q), idempotent(m.q, e))
	}
	g.body.WriteString(")\n\n")

	for _, m := range ms {
		if m.pending {
			continue
		}
		fmt.Fprintf(&g.body, "// %s implements %s.%s.\n", m.rpc, st.FullName, m.rpc)
		fmt.Fprintf(&g.body, "func (r *%s) %s%s {\n", recv, m.rpc, g.signature(m))
		switch m.q.Spec.(type) {
		case *ir.Create:
			g.createChecks(e, t, m, short)
		}
		g.call(e, m, short, sqlcPkg)
		g.body.WriteString("}\n\n")
	}
}

// call renders the Runner call around the sqlc query.
func (g *goFile) call(e *ir.Entity, m method, short, sqlcPkg string) {
	arg := "p"
	if m.inline {
		arg = goIdent(m.params[0].name)
	}
	op := opVar(short, m)
	run := fmt.Sprintf("r.run.Write(ctx, %s, ", op)
	if m.read {
		run = fmt.Sprintf("r.run.Read(ctx, %s, %t, ", op, m.strong)
	}

	if m.execRow {
		fmt.Fprintf(&g.body, "\treturn %sfunc(ctx context.Context, q dalpg.DBTX) error {\n", run)
		fmt.Fprintf(&g.body, "\t\tn, err := %s.New(q).%s(ctx, %s)\n", sqlcPkg, m.sqlc, arg)
		g.body.WriteString("\t\tif err == nil && n == 0 {\n\t\t\treturn dal.ErrNotFound\n\t\t}\n\t\treturn err\n\t})\n")
		return
	}

	fmt.Fprintf(&g.body, "\tvar out %s\n", m.model)
	fmt.Fprintf(&g.body, "\terr := %sfunc(ctx context.Context, q dalpg.DBTX) error {\n", run)
	g.body.WriteString("\t\tvar err error\n")
	fmt.Fprintf(&g.body, "\t\tout, err = %s.New(q).%s(ctx, %s)\n", sqlcPkg, m.sqlc, arg)
	if _, ok := m.q.Spec.(*ir.Update); ok && versioned(e) {
		// Zero rows: a missing row or a stale version. The existence check
		// runs only on this failure path, on the same connection.
		g.body.WriteString("\t\tif dalpg.IsNoRows(err) {\n")
		fmt.Fprintf(&g.body, "\t\t\texists, xerr := %s.New(q).%sExists(ctx, %s)\n", sqlcPkg, short, existsArgs(e, sqlcPkg, short))
		g.body.WriteString("\t\t\tif xerr != nil {\n\t\t\t\treturn xerr\n\t\t\t}\n")
		g.body.WriteString("\t\t\tif exists {\n\t\t\t\treturn dal.ErrVersionConflict\n\t\t\t}\n\t\t}\n")
	}
	g.body.WriteString("\t\treturn err\n\t})\n\treturn out, err\n")
}

// createChecks validates required fields before any database call, and gives
// a nil UUID primary key a UUIDv7 once, before the retry loop, so retries
// reuse the same ID.
func (g *goFile) createChecks(e *ir.Entity, t *Table, m method, short string) {
	ref := func(field string) string {
		if m.inline {
			return goIdent(field)
		}
		return "p." + camel(field)
	}
	for _, f := range e.Fields {
		c := column(t, f.Name)
		if c == nil || f.Role != ir.RoleNone {
			continue
		}
		switch {
		case f.PrimaryKey && f.Format == ir.FormatUUID:
			fmt.Fprintf(&g.body, "\tif %s == nil {\n", ref(f.Name))
			g.imports["github.com/google/uuid"] = true
			g.body.WriteString("\t\tid, err := uuid.NewV7()\n\t\tif err != nil {\n")
			fmt.Fprintf(&g.body, "\t\t\treturn %s{}, dalpg.NewError(%s, err)\n\t\t}\n", m.model, opVar(short, m))
			fmt.Fprintf(&g.body, "\t\t%s = &id\n\t}\n", ref(f.Name))
		case !f.Nullable && c.Default == "":
			fmt.Fprintf(&g.body, "\tif %s == nil {\n", ref(f.Name))
			fmt.Fprintf(&g.body, "\t\treturn %s{}, dalpg.NewError(%s, &dal.MissingFieldError{Field: %q})\n\t}\n",
				m.model, opVar(short, m), f.Name)
		}
	}
}

// existsArgs renders the argument of the <Store>Exists query from the
// update's params: the key inline, or sqlc's params struct for a composite
// key.
func existsArgs(e *ir.Entity, sqlcPkg, short string) string {
	pk := e.PrimaryKey()
	if len(pk) == 1 {
		return "p." + camel(pk[0])
	}
	fields := make([]string, len(pk))
	for i, f := range pk {
		fields[i] = fmt.Sprintf("%s: p.%s", camel(f), camel(f))
	}
	return fmt.Sprintf("%s.%sExistsParams{%s}", sqlcPkg, short, strings.Join(fields, ", "))
}

func opVar(short string, m method) string { return "_op" + short + m.rpc }

func opKind(q *ir.Query) string {
	switch q.Spec.(type) {
	case *ir.Get:
		return "OpGet"
	case *ir.List:
		return "OpList"
	case *ir.Create:
		return "OpCreate"
	case *ir.Update:
		return "OpUpdate"
	case *ir.Delete:
		return "OpDelete"
	case *ir.Upsert:
		return "OpUpsert"
	}
	return "OpGet"
}

// idempotent follows design §7: reads, deletes and upserts are; Create isn't;
// Update is unless it is a compare-and-swap (a retry after an ambiguous
// success would report the caller's own write as a conflict).
func idempotent(q *ir.Query, e *ir.Entity) bool {
	switch q.Spec.(type) {
	case *ir.Create:
		return false
	case *ir.Update:
		return !versioned(e)
	}
	return true
}

func versioned(e *ir.Entity) bool {
	for _, f := range e.Fields {
		if f.Role == ir.RoleVersion {
			return true
		}
	}
	return false
}
