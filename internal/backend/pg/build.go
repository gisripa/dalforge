package pg

import (
	"cmp"
	"slices"
	"strconv"

	"github.com/gisripa/dalforge/internal/check"
	"github.com/gisripa/dalforge/internal/diag"
	"github.com/gisripa/dalforge/internal/ir"
	"github.com/gisripa/dalforge/internal/ir/pgir"
)

// Rule IDs reported by Build.
const (
	RuleIdentifier  = "DAL204" // invalid, over-long or reserved identifier
	RuleVersion     = "DAL207" // target older than required
	RuleCustomType  = "DAL208" // custom_type/go_type pairing, undeclared extension
	RuleTypeMap     = "DAL209" // no or incompatible type mapping
	RuleDuplicate   = "DAL210" // duplicate table or column name
	RuleIndexRef    = "DAL211" // index references an unknown or repeated field
	RuleNullDefault = "DAL212" // info: optional field with a default can't be inserted as NULL
)

// _configPos attributes findings about the deploy target to the config file.
var _configPos = ir.Pos{File: "dalforge.yaml", Line: 1, Col: 1}

// Build turns a core-valid IR (internal/check reports no errors) and its
// Postgres hints into the physical model, reporting the Postgres rules.
func Build(s *ir.Schema, h *pgir.Hints, t Target) (*Schema, diag.List) {
	b := &builder{hints: h, target: t.major(), out: &Schema{}}
	b.version()
	b.extensions()

	tables := map[string]string{} // table name → entity, for DAL210
	for _, e := range s.Entities {
		tbl := b.table(e)
		if prev, ok := tables[tbl.Name]; ok {
			b.diags.Add(RuleDuplicate, diag.Error, e.Pos, "table %q is also used by %s; set a different (dal.v1.table).name", tbl.Name, prev)
		}
		tables[tbl.Name] = e.FullName
		b.out.Tables = append(b.out.Tables, tbl)
	}
	slices.SortFunc(b.out.Tables, func(a, c *Table) int { return cmp.Compare(a.Name, c.Name) })
	b.diags.Sort()
	return b.out, b.diags
}

type builder struct {
	hints  *pgir.Hints
	target int
	out    *Schema
	diags  diag.List
}

func (b *builder) version() {
	if b.target < Floor {
		b.diags.Add(RuleVersion, diag.Error, _configPos, "pg.version %d is older than the oldest supported Postgres (%d)", b.target, Floor)
	}
	for _, path := range sortedKeys(b.hints.Files) {
		if need := int(b.hints.Files[path].MinVersion); need > b.target {
			b.diags.Add(RuleVersion, diag.Error, ir.Pos{File: path, Line: 1, Col: 1},
				"(dal.pg.v1.file).min_version %d is newer than the deploy target pg.version %d", need, b.target)
		}
	}
}

func (b *builder) extensions() {
	for _, f := range b.hints.Files {
		for _, ext := range f.Extensions {
			if !slices.Contains(b.out.Extensions, ext) {
				b.out.Extensions = append(b.out.Extensions, ext)
			}
		}
	}
	slices.Sort(b.out.Extensions)
}

func (b *builder) identifier(pos ir.Pos, what, name string) {
	if why := checkIdentifier(name); why != "" {
		b.diags.Add(RuleIdentifier, diag.Error, pos, "%s %q %s", what, name, why)
	}
}

func (b *builder) table(e *ir.Entity) *Table {
	hints := b.hints.Table(e.FullName)
	tbl := &Table{Name: e.Table, Entity: e.FullName, IndexBudget: hints.IndexBudget, Pos: e.Pos}

	what := "table name"
	if e.TableSource == ir.SourceDefaulted {
		what = "table name (defaulted from the message name; set (dal.v1.table).name)"
	}
	b.identifier(e.Pos, what, tbl.Name)

	columns := map[string]string{} // column name → field, for DAL210
	for _, f := range e.Fields {
		col := b.column(e, f, hints.Column(f.Number))
		if col == nil {
			continue
		}
		if prev, ok := columns[col.Name]; ok {
			b.diags.Add(RuleDuplicate, diag.Error, f.Pos, "column %q of %s is also used by field %q", col.Name, tbl.Name, prev)
		}
		columns[col.Name] = f.Name
		tbl.Columns = append(tbl.Columns, col)
		if f.PrimaryKey {
			tbl.PrimaryKey = append(tbl.PrimaryKey, col.Name)
		}
	}
	for _, idx := range hints.Indexes {
		tbl.Indexes = append(tbl.Indexes, b.index(e, idx))
	}
	return tbl
}

func (b *builder) column(e *ir.Entity, f *ir.Field, hint *pgir.Column) *Column {
	b.identifier(f.Pos, "column name", f.Column)
	col := &Column{
		Name:    f.Column,
		Field:   f.Name,
		Number:  f.Number,
		NotNull: !f.Nullable,
		Default: hint.Default,
		Unique:  f.Unique,
	}
	if f.Nullable && col.Default != "" && f.Role == ir.RoleNone {
		b.diags.Add(RuleNullDefault, diag.Info, f.Pos,
			"field %q is optional and has default %s: a nil insert parameter means \"use the default\", so Create can't insert NULL (Update can)",
			f.Name, col.Default)
	}
	if col.Default == "" {
		switch f.Role {
		case ir.RoleCreateTime, ir.RoleUpdateTime:
			col.Default = "now()"
		case ir.RoleVersion:
			col.Default = "1"
		}
	}

	switch {
	case hint.CustomType != "":
		col.Type = hint.CustomType
		b.customType(e, f, hint, col)
	case hint.GoType != "":
		b.diags.Add(RuleCustomType, diag.Error, f.Pos, "field %q sets go_type without custom_type; go_type only applies to custom types", f.Name)
		return nil
	case hint.Type != "":
		if err := checkExplicitType(f, hint.Type); err != nil {
			b.diags.Add(RuleTypeMap, diag.Error, f.Pos, "field %q: %v", f.Name, err)
			return nil
		}
		col.Type = arrayOf(f, hint.Type)
		col.GoType = goType(f)
	default:
		typ, err := sqlType(f)
		if err != nil {
			b.diags.Add(RuleTypeMap, diag.Error, f.Pos, "field %q: %v", f.Name, err)
			return nil
		}
		col.Type = arrayOf(f, typ)
		col.GoType = goType(f)
	}

	col.GoType = withNull(col.GoType, col.NotNull)

	if need, ok := _minVersion[customBase(col.Type)]; ok && need > b.target {
		b.diags.Add(RuleVersion, diag.Error, f.Pos, "field %q: type %s needs Postgres %d, but pg.version is %d", f.Name, col.Type, need, b.target)
	}
	return col
}

// arrayOf makes a repeated scalar a Postgres array. Repeated messages are
// already a single jsonb value.
func arrayOf(f *ir.Field, typ string) string {
	if f.Repeated && f.Kind != ir.KindJSON {
		return typ + "[]"
	}
	return typ
}

func (b *builder) customType(e *ir.Entity, f *ir.Field, hint *pgir.Column, col *Column) {
	if hint.GoType == "" {
		b.diags.Add(RuleCustomType, diag.Error, f.Pos, "field %q: custom_type %s needs go_type, the Go type sqlc should use", f.Name, hint.CustomType)
	} else if g, err := parseGoType(hint.GoType); err != nil {
		b.diags.Add(RuleCustomType, diag.Error, f.Pos, "field %q: %v", f.Name, err)
	} else {
		col.GoType = g
	}

	ext, ok := _extensionTypes[customBase(hint.CustomType)]
	if !ok {
		return // unknown custom types are trusted
	}
	var declared []string
	if file, ok := b.hints.Files[e.Pos.File]; ok {
		declared = file.Extensions
	}
	if !slices.Contains(declared, ext) {
		b.diags.Add(RuleCustomType, diag.Error, f.Pos,
			"field %q: custom_type %s comes from the %q extension; declare it with option (dal.pg.v1.file) = {extensions: [%q]} in %s",
			f.Name, hint.CustomType, ext, ext, e.Pos.File)
	}
}

func (b *builder) index(e *ir.Entity, idx *pgir.Index) *Index {
	out := &Index{Name: idx.Name, Where: idx.Where, Unique: idx.Unique}
	if idx.Name != "" {
		b.identifier(e.Pos, "index name", idx.Name)
	}
	label := "index"
	if idx.Name != "" {
		label = "index " + strconv.Quote(idx.Name)
	}

	seen := map[string]bool{}
	resolve := func(what, name string) (string, bool) {
		f := e.Field(name)
		switch {
		case f == nil:
			b.diags.Add(RuleIndexRef, diag.Error, e.Pos, "%s %s: %s has no field %q%s", label, what, e.FullName, name, check.ColumnHint(e, name))
		case seen[name]:
			b.diags.Add(RuleIndexRef, diag.Error, e.Pos, "%s %s: field %q is listed more than once", label, what, name)
		default:
			seen[name] = true
			return f.Column, true
		}
		return "", false
	}
	for _, k := range idx.Columns {
		if col, ok := resolve("columns", k.Field); ok {
			out.Columns = append(out.Columns, IndexColumn{Column: col, Desc: k.Desc})
		}
	}
	for _, name := range idx.Include {
		if col, ok := resolve("include", name); ok {
			out.Include = append(out.Include, col)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
