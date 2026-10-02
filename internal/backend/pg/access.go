package pg

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gisripa/dalforge/internal/diag"
	"github.com/gisripa/dalforge/internal/ir"
)

// Rules for access paths and indexes (design §5).
const (
	RulePKFallback  = "DAL103" // warning: list falls back to a key that isn't time-ordered
	RuleUniqueBy    = "DAL104" // get.by / upsert.conflict_on not backed by a unique constraint
	RuleBudget      = "DAL201" // warning: index budget exceeded
	RuleIndexClash  = "DAL202" // order_by contradicts an explicit index on the same eq fields
	RuleMixedSort   = "DAL203" // warning: mixed sort directions
	RuleShareHint   = "DAL206" // info: two lists could share one index
	_defaultBudget  = 5
	_primaryKeyName = "primary key"
)

// accessPaths resolves each List's keyset (inheriting a sort from an
// explicit index when the list declares none), derives the indexes the lists
// need, merges redundant ones, and reports the access-path rules. It may set
// a List's OrderBy (Source: inherited) on the IR.
func (b *builder) accessPaths(s *ir.Schema) {
	b.out.Paths = map[string]*AccessPath{}
	b.clashed = map[*ir.Query]bool{}
	tables := map[string]*Table{}
	for _, t := range b.out.Tables {
		tables[t.Entity] = t
	}
	byTable := map[*Table][]*Index{}

	for _, st := range s.Stores {
		e, t := s.Entity(st.Entity), tables[st.Entity]
		if e == nil || t == nil {
			continue
		}
		for _, q := range st.Queries {
			switch spec := q.Spec.(type) {
			case *ir.Get:
				b.uniqueBy(e, t, q, "get.by", spec.By)
			case *ir.Upsert:
				b.uniqueBy(e, t, q, "upsert.conflict_on", spec.ConflictOn)
			case *ir.List:
				path, idx := b.listPath(e, t, q, spec)
				if path == nil {
					continue
				}
				b.out.Paths[storeShortName(st)+"."+q.Method] = path
				idx.For = []string{q.Method}
				byTable[t] = append(byTable[t], idx)
			}
		}
	}

	for _, t := range b.out.Tables {
		b.mergeIndexes(t, byTable[t])
		b.budget(t)
	}
	b.shareHints(s)
}

func storeShortName(st *ir.Store) string { return st.FullName[strings.LastIndex(st.FullName, ".")+1:] }

// listPath builds one list's keyset and the index it needs.
func (b *builder) listPath(e *ir.Entity, t *Table, q *ir.Query, s *ir.List) (*AccessPath, *Index) {
	if s.OrderBy.Source == ir.SourceDefaulted && s.Range == "" {
		b.inherit(t, s)
	}
	if s.OrderBy.Source == ir.SourceDefaulted && s.Range == "" && !timeOrdered(e, s.Eq) {
		b.diags.Add(RulePKFallback, diag.Warning, q.Pos, "rpc %s has no order_by and sorts by the primary key %v, which isn't time-ordered; pages are stable but in no meaningful order. Declare order_by",
			q.Method, e.PrimaryKey())
	}
	if s.OrderBy.Source == ir.SourceDeclared {
		b.clash(t, q, s)
	}

	// The keyset: the sort keys, then key fields not already pinned or
	// sorted, as tie-breakers in the last key's direction so one row
	// comparison covers them.
	keys := slices.Clone(s.OrderBy.Keys)
	desc := len(keys) > 0 && keys[len(keys)-1].Desc
	for _, pk := range e.PrimaryKey() {
		if !slices.Contains(s.Eq, pk) && !slices.ContainsFunc(keys, func(k ir.SortKey) bool { return k.Field == pk }) {
			keys = append(keys, ir.SortKey{Field: pk, Desc: desc})
		}
	}
	path := &AccessPath{Eq: s.Eq, Range: s.Range, Keys: keys}
	for _, k := range keys {
		if k.Desc != keys[0].Desc {
			path.Mixed = true
		}
	}
	if path.Mixed {
		b.diags.Add(RuleMixedSort, diag.Warning, q.Pos, "rpc %s sorts in mixed directions %s; the keyset predicate can't use a single row comparison, so it is expanded into ORs, which Postgres plans less efficiently",
			q.Method, sortKeys(keys))
	}

	idx := &Index{Derived: true, eq: len(s.Eq)}
	for _, f := range s.Eq {
		if c := column(t, f); c != nil {
			idx.Columns = append(idx.Columns, IndexColumn{Column: c.Name})
		}
	}
	for _, k := range keys {
		if c := column(t, k.Field); c != nil {
			idx.Columns = append(idx.Columns, IndexColumn{Column: c.Name, Desc: k.Desc})
		}
	}
	idx.sorted = len(s.Eq) + len(s.OrderBy.Keys)
	var preds []string
	if soft := roleColumn(e, t, ir.RoleDeleteTime); soft != "" {
		preds = append(preds, soft+" IS NULL")
	}
	if f := e.Field(s.Range); f != nil && f.Nullable {
		preds = append(preds, column(t, s.Range).Name+" IS NOT NULL")
	}
	idx.Where = strings.Join(preds, " AND ")
	return path, idx
}

// inherit gives a list without order_by the sort of an explicit index whose
// leading columns are exactly its eq fields (design §5, sort resolution 3).
func (b *builder) inherit(t *Table, s *ir.List) {
	eqCols := columnsOf(t, s.Eq)
	for _, idx := range t.Indexes {
		if idx.Derived || len(idx.Columns) <= len(eqCols) || !sameSet(leading(idx, len(eqCols)), eqCols) {
			continue
		}
		var keys []ir.SortKey
		for _, c := range idx.Columns[len(eqCols):] {
			if col := columnByName(t, c.Column); col != nil {
				keys = append(keys, ir.SortKey{Field: col.Field, Desc: c.Desc})
			}
		}
		s.OrderBy = ir.Sort{Keys: keys, Source: ir.SourceInherited}
		return
	}
}

// clash reports a declared order_by that contradicts an explicit index on
// the same equality fields.
func (b *builder) clash(t *Table, q *ir.Query, s *ir.List) {
	eqCols := columnsOf(t, s.Eq)
	for _, idx := range t.Indexes {
		if idx.Derived || len(idx.Columns) <= len(eqCols) || !sameSet(leading(idx, len(eqCols)), eqCols) {
			continue
		}
		rest := idx.Columns[len(eqCols):]
		var want []IndexColumn
		for _, k := range s.OrderBy.Keys {
			if c := column(t, k.Field); c != nil {
				want = append(want, IndexColumn{Column: c.Name, Desc: k.Desc})
			}
		}
		if !prefixMatch(want, rest, 0) && !prefixMatch(rest, want, 0) {
			name := idx.Name
			if name == "" {
				name = "an explicit index"
			}
			b.clashed[q] = true
			b.diags.Add(RuleIndexClash, diag.Error, q.Pos, "rpc %s: order_by %s contradicts %s %s on the same eq fields; align them, or the list needs a second index",
				q.Method, sortKeys(s.OrderBy.Keys), name, indexColumns(idx))
		}
	}
}

// uniqueBy requires a non-key lookup or conflict target to be backed by a
// unique constraint: the key, a unique field, or an explicit unique index.
func (b *builder) uniqueBy(e *ir.Entity, t *Table, q *ir.Query, what string, refs ir.FieldRefs) {
	if refs.Source != ir.SourceDeclared || sameSet(refs.Names, e.PrimaryKey()) {
		return
	}
	if len(refs.Names) == 1 {
		if f := e.Field(refs.Names[0]); f != nil && f.Unique {
			return
		}
	}
	for _, idx := range t.Indexes {
		if idx.Unique && sameSet(columnNames(idx), columnsOf(t, refs.Names)) {
			return
		}
	}
	b.diags.Add(RuleUniqueBy, diag.Error, q.Pos, "rpc %s: %s %v isn't backed by a unique constraint, so it could match several rows; mark the field unique, add a unique index, or make it a list",
		q.Method, what, refs.Names)
}

// mergeIndexes adds the derived indexes a table still needs: one that is a
// prefix of another index with a compatible predicate (scanned the same way
// or fully backwards) is served by that one instead.
func (b *builder) mergeIndexes(t *Table, derived []*Index) {
	pk := &Index{Name: _primaryKeyName}
	for _, c := range t.PrimaryKey {
		pk.Columns = append(pk.Columns, IndexColumn{Column: c})
	}
	existing := append([]*Index{pk}, t.Indexes...)

	var kept []*Index
	for i, d := range derived {
		var by *Index
		for _, other := range append(slices.Clone(existing), kept...) {
			if covers(other, d) {
				by = other
				break
			}
		}
		if by == nil {
			// A longer derived index still to come may cover it.
			for _, other := range derived[i+1:] {
				if len(other.Columns) > len(d.Columns) && covers(other, d) {
					by = other
					break
				}
			}
		}
		if by != nil {
			by.For = append(by.For, d.For...)
			continue
		}
		kept = append(kept, d)
	}
	for _, d := range kept {
		// Directions are part of the name: (a, b) and (a, b DESC) are
		// different indexes and need different names.
		names := []string{t.Name}
		for _, c := range d.Columns {
			names = append(names, c.Column)
			if c.Desc {
				names = append(names, "desc")
			}
		}
		d.Name = identName(append(names, "idx")...)
		t.Indexes = append(t.Indexes, d)
	}
	b.assignServing(t, append([]*Index{pk}, t.Indexes...))
}

// assignServing records which index serves each path of t.
func (b *builder) assignServing(t *Table, indexes []*Index) {
	for key, p := range b.out.Paths {
		if p.Index != "" {
			continue
		}
		for _, idx := range indexes {
			if slices.ContainsFunc(idx.For, func(m string) bool { return strings.HasSuffix(key, "."+m) }) && servesTable(t, idx) {
				p.Index = idx.Name
				break
			}
		}
	}
}

func servesTable(t *Table, idx *Index) bool {
	return idx.Name == _primaryKeyName || slices.Contains(t.Indexes, idx)
}

// covers reports whether index a can serve every query index d was derived
// for: d's columns are a prefix of a's, in the same or fully reversed
// directions, and a's predicate is absent or the same. An explicit index may
// stop short of d's key tie-breakers: Postgres sorts the few ties
// (incremental sort), which beats keeping a second, near-identical index.
func covers(a, d *Index) bool {
	if a == d || (a.Where != "" && a.Where != d.Where) {
		return false
	}
	n := len(d.Columns)
	if len(a.Columns) < n {
		if a.Derived || len(a.Columns) < d.sorted {
			return false
		}
		n = len(a.Columns)
	}
	return prefixMatch(d.Columns[:n], a.Columns, d.eq)
}

// prefixMatch reports whether p is a prefix of cols, column for column, in
// the same directions or all reversed. The first eq columns of p are
// equality-matched, so their direction doesn't matter.
func prefixMatch(p, cols []IndexColumn, eq int) bool {
	if len(p) > len(cols) {
		return false
	}
	same, reversed := true, true
	for i, c := range p {
		if c.Column != cols[i].Column {
			return false
		}
		if i < eq {
			continue
		}
		same = same && c.Desc == cols[i].Desc
		reversed = reversed && c.Desc != cols[i].Desc
	}
	return same || reversed
}

// budget warns when a table carries more indexes than its budget: every
// index slows every insert.
func (b *builder) budget(t *Table) {
	limit := _defaultBudget
	if h := b.hints.Table(t.Entity); h.IndexBudget > 0 {
		limit = int(h.IndexBudget)
	}
	n := 1 + len(t.Indexes) // the primary key
	for _, c := range t.Columns {
		if c.Unique {
			n++
		}
	}
	if n > limit {
		b.diags.Add(RuleBudget, diag.Warning, t.Pos, "table %s has %d indexes (key, unique and list indexes) over its budget of %d; each one adds write cost to every insert. Consolidate list shapes, or raise (dal.pg.v1.table).index_budget",
			t.Name, n, limit)
	}
}

// shareHints notes lists with the same eq fields but different sorts: they
// need separate indexes, and aligning order_by would let them share one.
func (b *builder) shareHints(s *ir.Schema) {
	for _, st := range s.Stores {
		type seen struct {
			method string
			keys   []ir.SortKey
		}
		groups := map[string][]seen{}
		for _, q := range st.Queries {
			p, ok := b.out.Paths[storeShortName(st)+"."+q.Method]
			if !ok || b.clashed[q] { // already an error (DAL202)
				continue
			}
			eq := slices.Clone(p.Eq)
			slices.Sort(eq)
			key := strings.Join(eq, ",")
			// The same sort read backwards shares an index (B-trees scan
			// both ways); hint only when no earlier list can share.
			group := groups[key]
			if len(group) > 0 && !slices.ContainsFunc(group, func(o seen) bool { return sameOrReversed(o.keys, p.Keys) }) {
				other := group[0]
				b.diags.Add(RuleShareHint, diag.Info, q.Pos, "rpc %s and %s filter by the same fields %v but sort differently (%s vs %s), so each needs its own index; aligning order_by would let them share one",
					other.method, q.Method, p.Eq, sortKeys(other.keys), sortKeys(p.Keys))
			}
			groups[key] = append(groups[key], seen{method: q.Method, keys: p.Keys})
		}
	}
}

// timeOrdered reports whether the key fields a list sorts by (those not
// pinned by its eq fields) follow creation order.
func timeOrdered(e *ir.Entity, eq []string) bool {
	for _, name := range e.PrimaryKey() {
		if slices.Contains(eq, name) {
			continue
		}
		f := e.Field(name)
		if f == nil {
			return false
		}
		switch {
		case f.Format == ir.FormatUUID: // dalforge assigns UUIDv7 when the caller doesn't
		case f.Kind == ir.KindInt32 || f.Kind == ir.KindInt64 || f.Kind == ir.KindUint32:
		default:
			return false
		}
	}
	return true
}

func columnsOf(t *Table, fields []string) []string {
	var out []string
	for _, f := range fields {
		if c := column(t, f); c != nil {
			out = append(out, c.Name)
		}
	}
	return out
}

func columnByName(t *Table, name string) *Column {
	for _, c := range t.Columns {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func leading(idx *Index, n int) []string {
	return columnNames(&Index{Columns: idx.Columns[:n]})
}

func columnNames(idx *Index) []string {
	out := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		out[i] = c.Column
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

func sortKeys(keys []ir.SortKey) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k.Field
		if k.Desc {
			parts[i] += " DESC"
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func indexColumns(idx *Index) string {
	parts := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		parts[i] = c.Column
		if c.Desc {
			parts[i] += " DESC"
		}
	}
	return fmt.Sprintf("(%s)", strings.Join(parts, ", "))
}

// sameOrReversed reports whether two keysets are equal, or equal with every
// direction flipped (one index serves both, scanned the other way).
func sameOrReversed(a, b []ir.SortKey) bool {
	if len(a) != len(b) {
		return false
	}
	same, reversed := true, true
	for i := range a {
		if a[i].Field != b[i].Field {
			return false
		}
		same = same && a[i].Desc == b[i].Desc
		reversed = reversed && a[i].Desc != b[i].Desc
	}
	return same || reversed
}
