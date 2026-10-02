package pg

import (
	"fmt"
	"slices"
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

	// One dal.Op per rpc, fixed at generation time: retry policies read its
	// Idempotent flag, and tracers its name (docs/design.md, "Errors and retries").
	g.body.WriteString("var (\n")
	for _, m := range ms {
		fmt.Fprintf(&g.body, "\t%s = dal.Op{Entity: %q, Method: %q, Kind: dal.%s, Idempotent: %t}\n",
			opVar(short, m), modelName(t), m.rpc, opKind(m.q), idempotent(m.q, e))
	}
	g.body.WriteString(")\n\n")

	for _, m := range ms {
		fmt.Fprintf(&g.body, "// %s implements %s.%s.\n", m.rpc, st.FullName, m.rpc)
		fmt.Fprintf(&g.body, "func (r *%s) %s%s {\n", recv, m.rpc, g.signature(m))
		switch s := m.q.Spec.(type) {
		case *ir.Create, *ir.Upsert:
			g.createChecks(e, t, m, short)
		case *ir.Update:
			g.updateChecks(e, s, m, short)
		case *ir.List:
			g.listBody(m, short, sqlcPkg)
			g.body.WriteString("}\n\n")
			continue
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
	if m.upsert {
		// The conflicting row is soft-deleted: upsert never revives it, so
		// the conflict update is skipped and no row comes back.
		g.body.WriteString("\t\tif dalpg.IsNoRows(err) {\n\t\t\treturn dal.ErrAlreadyExists\n\t\t}\n")
	}
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

// updateChecks rejects a nil required field before any database call, as
// Create does; nil on an optional field means "set NULL".
func (g *goFile) updateChecks(e *ir.Entity, s *ir.Update, m method, short string) {
	ref := func(field string) string {
		if m.inline {
			return goIdent(field)
		}
		return "p." + camel(field)
	}
	for _, name := range s.Columns.Names {
		if f := e.Field(name); f != nil && !f.Nullable {
			fmt.Fprintf(&g.body, "\tif %s == nil {\n", ref(name))
			fmt.Fprintf(&g.body, "\t\treturn %s{}, dalpg.NewError(%s, &dal.MissingFieldError{Field: %q})\n\t}\n",
				m.model, opVar(short, m), name)
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

// idempotent follows docs/design.md, "Errors and retries": reads, deletes and upserts are; Create isn't;
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

// listBody renders a keyset List: decode the page token (before the retry
// loop: a bad token is the caller's error), run the first-page or cursor
// query for size+1 rows, and build the page and its next token.
func (g *goFile) listBody(m method, short, sqlcPkg string) {
	g.imports[_dalImport] = true
	op := opVar(short, m)
	def, mx := int32(50), int32(500)
	if m.list.DefaultPageSize > 0 {
		def = int32(m.list.DefaultPageSize)
	}
	if m.list.MaxPageSize > 0 {
		mx = int32(m.list.MaxPageSize)
	}
	fmt.Fprintf(&g.body, "\tconst shape = %q\n", m.sqlc+":"+sortKeys(m.path.Keys))
	fmt.Fprintf(&g.body, "\tsize := dal.PageSize(p.PageSize, %d, %d)\n", def, mx)

	filters := listFilterFields(m)
	var fexprs []string
	for _, f := range filters {
		fexprs = append(fexprs, "p."+camel(f.name))
	}
	fmt.Fprintf(&g.body, "\tfilters := []any{%s}\n", strings.Join(fexprs, ", "))

	// Typed cursor values, decoded from the token.
	var afterVars, afterPtrs []string
	for _, k := range m.path.Keys {
		typ := m.fieldType(k.Field)
		typ.Pointer = false
		v := goIdent("after_" + k.Field)
		fmt.Fprintf(&g.body, "\tvar %s %s\n", v, g.use(typ))
		afterVars = append(afterVars, v)
		afterPtrs = append(afterPtrs, "&"+v)
	}
	g.body.WriteString("\tif p.PageToken != \"\" {\n")
	fmt.Fprintf(&g.body, "\t\tif err := dal.DecodeToken(p.PageToken, shape, filters, %s); err != nil {\n", strings.Join(afterPtrs, ", "))
	fmt.Fprintf(&g.body, "\t\t\treturn dal.Page[%s]{}, dalpg.NewError(%s, err)\n\t\t}\n\t}\n", m.model, op)

	// sqlc params: filters, then (After) the cursor, then the limit.
	var firstFields []string
	for _, f := range filters {
		firstFields = append(firstFields, fmt.Sprintf("%s: p.%s", camel(f.name), camel(f.name)))
	}
	afterFields := slices.Clone(firstFields)
	for i, k := range m.path.Keys {
		afterFields = append(afterFields, fmt.Sprintf("%s: %s", camel("after_"+k.Field), afterVars[i]))
	}
	firstFields = append(firstFields, "PageLimit: size + 1")
	afterFields = append(afterFields, "PageLimit: size + 1")
	firstArg := fmt.Sprintf("%s.%sParams{%s}", sqlcPkg, m.sqlc, strings.Join(firstFields, ", "))
	if len(filters) == 0 { // the limit is sqlc's only parameter: passed inline
		firstArg = "size + 1"
	}

	fmt.Fprintf(&g.body, "\tvar rows []%s\n", m.model)
	fmt.Fprintf(&g.body, "\terr := r.run.Read(ctx, %s, %t, func(ctx context.Context, q dalpg.DBTX) error {\n", op, m.strong)
	g.body.WriteString("\t\tvar err error\n\t\tif p.PageToken == \"\" {\n")
	fmt.Fprintf(&g.body, "\t\t\trows, err = %s.New(q).%s(ctx, %s)\n", sqlcPkg, m.sqlc, firstArg)
	g.body.WriteString("\t\t} else {\n")
	fmt.Fprintf(&g.body, "\t\t\trows, err = %s.New(q).%sAfter(ctx, %s.%sAfterParams{%s})\n", sqlcPkg, m.sqlc, sqlcPkg, m.sqlc, strings.Join(afterFields, ", "))
	g.body.WriteString("\t\t}\n\t\treturn err\n\t})\n")
	fmt.Fprintf(&g.body, "\tif err != nil {\n\t\treturn dal.Page[%s]{}, err\n\t}\n", m.model)

	var lastKeys []string
	for _, k := range m.path.Keys {
		expr := "last." + camel(k.Field)
		if m.fieldType(k.Field).Pointer {
			expr = "*" + expr // a nullable range field: the range filter excludes NULLs
		}
		lastKeys = append(lastKeys, expr)
	}
	fmt.Fprintf(&g.body, "\treturn dal.NewPage(rows, size, func(last %s) (string, error) {\n", m.model)
	fmt.Fprintf(&g.body, "\t\treturn dal.EncodeToken(shape, filters, %s)\n\t})\n", strings.Join(lastKeys, ", "))
}

// tx renders the package's transaction API: WithTx and a Tx exposing each
// store's repository, plus sqlc's Queries for custom queries in the same
// transaction.
func (g *goFile) tx(stores []*ir.Store, protoPkg, sqlcPkg string) {
	if len(stores) == 0 {
		return
	}
	pkg := dalPackageName(protoPkg)
	fmt.Fprintf(&g.body, "var _opWithTx = dal.Op{Entity: %q, Method: \"WithTx\", Kind: dal.OpTx, Idempotent: false}\n\n", pkg)
	g.body.WriteString("// Tx gives this package's repositories inside one transaction.\n")
	g.body.WriteString("type Tx struct{ run *dalpg.Runner }\n\n")
	for _, st := range stores {
		short := storeShort(st)
		recv := strings.ToLower(short[:1]) + short[1:] + "Repository"
		fmt.Fprintf(&g.body, "// %s returns the %s repository, bound to the transaction.\n", short, st.FullName)
		fmt.Fprintf(&g.body, "func (tx Tx) %s() %sRepository {\n\treturn &%s{run: tx.run}\n}\n\n", short, short, recv)
	}
	g.body.WriteString("// Queries returns sqlc's queries bound to the transaction, so custom\n")
	g.body.WriteString("// queries (queries/custom) can run in it too.\n")
	fmt.Fprintf(&g.body, "func (tx Tx) Queries() *%s.Queries {\n\treturn %s.New(tx.run.Conn())\n}\n\n", sqlcPkg, sqlcPkg)
	g.body.WriteString("// WithTx runs fn in a transaction on the writer pool and commits if it\n")
	g.body.WriteString("// returns nil. Statements inside aren't retried on their own. The whole\n")
	g.body.WriteString("// transaction is retried, re-running fn, only when Postgres rolled it back\n")
	g.body.WriteString("// (serialization failure, deadlock), never after an ambiguous failure; keep\n")
	g.body.WriteString("// side effects outside the database out of fn.\n")
	g.body.WriteString("func WithTx(ctx context.Context, run *dalpg.Runner, fn func(ctx context.Context, tx Tx) error) error {\n")
	g.body.WriteString("\treturn run.InTx(ctx, _opWithTx, func(ctx context.Context, txRun *dalpg.Runner) error {\n\t\treturn fn(ctx, Tx{run: txRun})\n\t})\n}\n")
}
