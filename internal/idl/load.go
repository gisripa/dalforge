package idl

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/linker"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/gisripa/dalforge/internal/ir"
	"github.com/gisripa/dalforge/internal/ir/pgir"
	pgv1 "github.com/gisripa/dalforge/proto/dal/pg/v1"
	dalv1 "github.com/gisripa/dalforge/proto/dal/v1"
)

// Options configures Load.
type Options struct {
	// ImportPaths are searched for the input files and their imports. The
	// dalforge options and the well-known types need no import path.
	ImportPaths []string
}

// Result is a loaded IDL: the core IR plus the Postgres hints.
type Result struct {
	Schema *ir.Schema  `json:"schema"`
	PG     *pgir.Hints `json:"pg"`
}

// Load compiles the given proto files (paths relative to an import path) and
// builds the IR. It reports every structural problem it finds, each prefixed
// with its source position.
func Load(ctx context.Context, files []string, opts Options) (*Result, error) {
	c := protocompile.Compiler{
		Resolver:       NewResolver(opts.ImportPaths...),
		SourceInfoMode: protocompile.SourceInfoStandard,
	}
	compiled, err := c.Compile(ctx, files...)
	if err != nil {
		return nil, err
	}

	reachable, err := registry(compiled)
	if err != nil {
		return nil, err
	}
	l := &loader{
		resolver: reachable,
		schema:   &ir.Schema{},
		pg:       &pgir.Hints{},
		entities: map[protoreflect.FullName]*ir.Entity{},
		proto3:   map[string]bool{},
	}
	var stores []pendingStore
	for _, f := range compiled {
		l.pgFile(f)
		l.messages(f.Messages())
		for i := range f.Services().Len() {
			if s, ok := l.store(f.Services().Get(i)); ok {
				stores = append(stores, s)
			}
		}
	}
	// Queries need every entity, including ones only a later store names.
	for _, s := range stores {
		l.queries(s)
	}
	if len(l.errs) > 0 {
		return nil, errors.Join(l.errs...)
	}

	cmp := func(a, b string) int { return strings.Compare(a, b) }
	slices.SortFunc(l.schema.Entities, func(a, b *ir.Entity) int { return cmp(a.FullName, b.FullName) })
	slices.SortFunc(l.schema.Stores, func(a, b *ir.Store) int { return cmp(a.FullName, b.FullName) })
	return &Result{Schema: l.schema, PG: l.pg}, nil
}

type loader struct {
	resolver *protoregistry.Files
	schema   *ir.Schema
	pg       *pgir.Hints
	entities map[protoreflect.FullName]*ir.Entity
	proto3   map[string]bool // files already checked for proto3 syntax
	errs     []error
}

// registry indexes every file reachable from the compiled ones, so a store
// can name an entity declared in an imported file that wasn't passed to Load.
// (linker.Files.AsResolver only searches the files themselves.)
func registry(files linker.Files) (*protoregistry.Files, error) {
	reg := &protoregistry.Files{}
	seen := map[string]bool{}
	var add func(protoreflect.FileDescriptor) error
	add = func(f protoreflect.FileDescriptor) error {
		if seen[f.Path()] {
			return nil
		}
		seen[f.Path()] = true
		for i := range f.Imports().Len() {
			if err := add(f.Imports().Get(i).FileDescriptor); err != nil {
				return err
			}
		}
		return reg.RegisterFile(f)
	}
	for _, f := range files {
		if err := add(f); err != nil {
			return nil, fmt.Errorf("index %s: %w", f.Path(), err)
		}
	}
	return reg, nil
}

type pendingStore struct {
	desc   protoreflect.ServiceDescriptor
	store  *ir.Store
	entity *ir.Entity
}

func (l *loader) errorf(d protoreflect.Descriptor, format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf("%s: %s", pos(d), fmt.Sprintf(format, args...)))
}

// get returns option xt on d as T, or a nil T when unset (generated getters
// are nil-safe).
func get[T proto.Message](l *loader, d protoreflect.Descriptor, xt protoreflect.ExtensionType) T {
	v, ok, err := option[T](d, xt)
	if err != nil {
		l.errorf(d, "%v", err)
	}
	if !ok {
		var zero T
		return zero
	}
	return v
}

func pos(d protoreflect.Descriptor) ir.Pos {
	f := d.ParentFile()
	loc := f.SourceLocations().ByDescriptor(d)
	return ir.Pos{File: f.Path(), Line: loc.StartLine + 1, Col: loc.StartColumn + 1}
}

// requireProto3 reports, once per file, a dalforge declaration in a file that
// isn't proto3: nullability is defined by proto3's `optional` keyword.
func (l *loader) requireProto3(d protoreflect.Descriptor) {
	f := d.ParentFile()
	if _, seen := l.proto3[f.Path()]; seen {
		return
	}
	l.proto3[f.Path()] = true
	if f.Syntax() != protoreflect.Proto3 {
		l.errorf(d, "dalforge IDL must use syntax = \"proto3\" (file uses %s)", f.Syntax())
	}
}

func (l *loader) messages(mds protoreflect.MessageDescriptors) {
	for i := range mds.Len() {
		md := mds.Get(i)
		if md.IsMapEntry() {
			continue
		}
		if get[*dalv1.Table](l, md, dalv1.E_Table) != nil {
			l.entity(md)
		}
		l.messages(md.Messages())
	}
}

func (l *loader) entity(md protoreflect.MessageDescriptor) *ir.Entity {
	if e, ok := l.entities[md.FullName()]; ok {
		return e
	}
	l.requireProto3(md)

	table := get[*dalv1.Table](l, md, dalv1.E_Table)
	e := &ir.Entity{
		FullName: string(md.FullName()),
		Table:    table.GetName(),
		ShardKey: table.GetShardKey(),
		Pos:      pos(md),
	}
	if e.Table == "" {
		e.Table, e.TableSource = snakeCase(string(md.Name())), ir.SourceDefaulted
	}
	for i := range md.ReservedRanges().Len() {
		r := md.ReservedRanges().Get(i) // [start, end)
		e.Reserved.Numbers = append(e.Reserved.Numbers, [2]int32{int32(r[0]), int32(r[1]) - 1})
	}
	for i := range md.ReservedNames().Len() {
		e.Reserved.Names = append(e.Reserved.Names, string(md.ReservedNames().Get(i)))
	}
	l.entities[md.FullName()] = e
	l.schema.Entities = append(l.schema.Entities, e)

	for i := range md.Fields().Len() {
		if f := l.field(md.Fields().Get(i)); f != nil {
			e.Fields = append(e.Fields, f)
		}
	}
	if len(e.PrimaryKey()) == 0 {
		l.errorf(md, "entity %s has no primary key; mark a field with (dal.v1.field).primary_key", md.FullName())
	}
	l.pgTable(md, e)
	return e
}

func (l *loader) field(fd protoreflect.FieldDescriptor) *ir.Field {
	if o := fd.ContainingOneof(); o != nil && !o.IsSynthetic() {
		l.errorf(fd, "field %s is in oneof %s; oneofs are not supported in entities", fd.Name(), o.Name())
		return nil
	}
	kind, ok := l.kind(fd, true)
	if !ok {
		return nil
	}
	opts := get[*dalv1.Field](l, fd, dalv1.E_Field)
	f := &ir.Field{
		Name:       string(fd.Name()),
		Column:     opts.GetName(),
		Number:     int32(fd.Number()),
		Kind:       kind,
		Format:     ir.Format(opts.GetFormat()),
		Repeated:   fd.IsList(),
		Nullable:   fd.HasOptionalKeyword(),
		PrimaryKey: opts.GetPrimaryKey(),
		Unique:     opts.GetUnique(),
		Role:       ir.Role(opts.GetRole()),
		State:      state(opts.GetState()),
		Pos:        pos(fd),
	}
	if f.Column == "" {
		f.Column = f.Name
	}
	if kind == ir.KindEnum {
		f.Enum = enum(fd.Enum())
	}
	return f
}

// kind maps a proto field to its logical kind. In entities, messages other
// than Timestamp are stored as JSON; in request/response shapes they stay
// KindMessage.
func (l *loader) kind(fd protoreflect.FieldDescriptor, entity bool) (ir.Kind, bool) {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return ir.KindBool, true
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return ir.KindInt32, true
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return ir.KindInt64, true
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return ir.KindUint32, true
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return ir.KindUint64, true
	case protoreflect.FloatKind:
		return ir.KindFloat, true
	case protoreflect.DoubleKind:
		return ir.KindDouble, true
	case protoreflect.StringKind:
		return ir.KindString, true
	case protoreflect.BytesKind:
		return ir.KindBytes, true
	case protoreflect.EnumKind:
		return ir.KindEnum, true
	case protoreflect.MessageKind:
		if fd.IsMap() {
			return ir.KindJSON, true
		}
		switch name := fd.Message().FullName(); {
		case name == "google.protobuf.Timestamp":
			return ir.KindTimestamp, true
		case name == "google.protobuf.Struct", name == "google.protobuf.Value", name == "google.protobuf.ListValue":
			return ir.KindJSON, true
		case strings.HasPrefix(string(name), "google.protobuf."):
			l.errorf(fd, "field %s: %s is not supported (supported well-known types: Timestamp, Struct, Value, ListValue)", fd.Name(), name)
			return 0, false
		case entity:
			return ir.KindJSON, true
		default:
			return ir.KindMessage, true
		}
	default:
		l.errorf(fd, "field %s: %s fields are not supported", fd.Name(), fd.Kind())
		return 0, false
	}
}

func enum(ed protoreflect.EnumDescriptor) *ir.Enum {
	e := &ir.Enum{FullName: string(ed.FullName())}
	for i := range ed.Values().Len() {
		e.Values = append(e.Values, string(ed.Values().Get(i).Name()))
	}
	return e
}

func state(s dalv1.State) ir.State {
	if s == dalv1.State_STATE_DEPRECATED {
		return ir.StateDeprecated
	}
	return ir.StateActive
}

func consistency(c dalv1.Consistency) ir.Consistency {
	if c == dalv1.Consistency_CONSISTENCY_STRONG {
		return ir.ConsistencyStrong
	}
	return ir.ConsistencyEventual
}

func (l *loader) store(sd protoreflect.ServiceDescriptor) (pendingStore, bool) {
	st := get[*dalv1.Store](l, sd, dalv1.E_Store)
	if st == nil {
		return pendingStore{}, false
	}
	l.requireProto3(sd)

	name := st.GetEntity()
	if name == "" {
		l.errorf(sd, "store %s must name its entity: option (dal.v1.store) = {entity: \"...\"}", sd.Name())
		return pendingStore{}, false
	}
	full := protoreflect.FullName(name)
	if !strings.Contains(name, ".") {
		full = sd.ParentFile().Package().Append(protoreflect.Name(name))
	}
	d, err := l.resolver.FindDescriptorByName(full)
	md, ok := d.(protoreflect.MessageDescriptor)
	if err != nil || !ok {
		l.errorf(sd, "store %s: entity %q not found (looked for message %s)", sd.Name(), name, full)
		return pendingStore{}, false
	}

	e := l.entity(md)
	s := &ir.Store{FullName: string(sd.FullName()), Entity: e.FullName, Pos: pos(sd)}
	l.schema.Stores = append(l.schema.Stores, s)
	return pendingStore{desc: sd, store: s, entity: e}, true
}

func (l *loader) queries(p pendingStore) {
	methods := p.desc.Methods()
	for i := range methods.Len() {
		if q := l.query(methods.Get(i), p.entity); q != nil {
			p.store.Queries = append(p.store.Queries, q)
		}
	}
}

func (l *loader) query(md protoreflect.MethodDescriptor, e *ir.Entity) *ir.Query {
	if md.IsStreamingClient() || md.IsStreamingServer() {
		l.errorf(md, "rpc %s: streaming rpcs are not supported", md.Name())
		return nil
	}
	q := get[*dalv1.Query](l, md, dalv1.E_Query)
	if q == nil {
		l.errorf(md, "rpc %s has no (dal.v1.query) option; every rpc of a store declares one access pattern", md.Name())
		return nil
	}

	var spec ir.Spec
	switch k := q.GetKind().(type) {
	case *dalv1.Query_Get:
		spec = &ir.Get{
			By:          refsOr(k.Get.GetBy(), e.PrimaryKey()),
			Consistency: consistency(k.Get.GetConsistency()),
		}
	case *dalv1.Query_List:
		spec = l.list(md, k.List, e)
	case *dalv1.Query_Create:
		spec = &ir.Create{}
	case *dalv1.Query_Update:
		spec = &ir.Update{Columns: refsOr(k.Update.GetColumns(), writable(e))}
	case *dalv1.Query_Delete:
		spec = &ir.Delete{}
	case *dalv1.Query_Upsert:
		spec = &ir.Upsert{
			ConflictOn: refsOr(k.Upsert.GetConflictOn(), e.PrimaryKey()),
			Columns:    refsOr(k.Upsert.GetColumns(), writable(e)),
		}
	default:
		l.errorf(md, "rpc %s: (dal.v1.query) sets no access pattern; set one of get, list, create, update, delete, upsert", md.Name())
		return nil
	}
	if spec == nil {
		return nil
	}
	return &ir.Query{
		Method:   string(md.Name()),
		Request:  l.shape(md.Input()),
		Response: l.shape(md.Output()),
		Spec:     spec,
		Pos:      pos(md),
	}
}

func (l *loader) list(md protoreflect.MethodDescriptor, list *dalv1.List, e *ir.Entity) ir.Spec {
	s := &ir.List{
		Eq:              list.GetEq(),
		Range:           list.GetRange(),
		DefaultPageSize: list.GetDefaultPageSize(),
		MaxPageSize:     list.GetMaxPageSize(),
		Consistency:     consistency(list.GetConsistency()),
	}
	switch {
	case len(list.GetOrderBy()) > 0:
		cols, ok := l.sortKeys(md, list.GetOrderBy())
		if !ok {
			return nil
		}
		s.OrderBy = ir.Sort{Keys: cols, Source: ir.SourceDeclared}
	case s.Range != "":
		// A range implies sorting by the range field (design §5).
		s.OrderBy = ir.Sort{Keys: []ir.SortKey{{Field: s.Range}}, Source: ir.SourceDefaulted}
	default:
		// No order_by and no range: the primary key, minus fields the
		// equality filters already pin (sorting by them is a no-op), e.g.
		// eq [tenant_id] on key (tenant_id, id) sorts by id. A backend pass
		// may replace this with a sort inherited from an explicit index.
		keys := []ir.SortKey{}
		for _, name := range e.PrimaryKey() {
			if !slices.Contains(s.Eq, name) {
				keys = append(keys, ir.SortKey{Field: name})
			}
		}
		s.OrderBy = ir.Sort{Keys: keys, Source: ir.SourceDefaulted}
	}
	return s
}

// sortKeys parses entries like "created_at" or "created_at DESC".
func (l *loader) sortKeys(d protoreflect.Descriptor, entries []string) ([]ir.SortKey, bool) {
	cols := make([]ir.SortKey, 0, len(entries))
	for _, entry := range entries {
		c, err := parseSortKey(entry)
		if err != nil {
			l.errorf(d, "%v", err)
			return nil, false
		}
		cols = append(cols, c)
	}
	return cols, true
}

func parseSortKey(s string) (ir.SortKey, error) {
	parts := strings.Fields(s)
	switch {
	case len(parts) == 1:
		return ir.SortKey{Field: parts[0]}, nil
	case len(parts) == 2 && strings.EqualFold(parts[1], "ASC"):
		return ir.SortKey{Field: parts[0]}, nil
	case len(parts) == 2 && strings.EqualFold(parts[1], "DESC"):
		return ir.SortKey{Field: parts[0], Desc: true}, nil
	default:
		return ir.SortKey{}, fmt.Errorf("invalid sort %q: want \"field\" or \"field ASC|DESC\"", s)
	}
}

// refsOr returns the declared field names, or the default when none are
// declared.
func refsOr(declared, def []string) ir.FieldRefs {
	if len(declared) > 0 {
		return ir.FieldRefs{Names: declared, Source: ir.SourceDeclared}
	}
	return ir.FieldRefs{Names: def, Source: ir.SourceDefaulted}
}

// writable is the default field set of Update and Upsert: active fields that
// are neither key fields nor managed by a role.
func writable(e *ir.Entity) []string {
	var names []string
	for _, f := range e.Fields {
		if !f.PrimaryKey && f.Role == ir.RoleNone && f.State == ir.StateActive {
			names = append(names, f.Name)
		}
	}
	return names
}

// shape describes an rpc request or response. An entity message is referred
// to by name only.
func (l *loader) shape(md protoreflect.MessageDescriptor) *ir.Message {
	m := &ir.Message{FullName: string(md.FullName())}
	if _, ok := l.entities[md.FullName()]; ok {
		m.Entity = true
		return m
	}
	for i := range md.Fields().Len() {
		fd := md.Fields().Get(i)
		kind, ok := l.kind(fd, false)
		if !ok {
			continue
		}
		f := &ir.MessageField{
			Name:     string(fd.Name()),
			Number:   int32(fd.Number()),
			Kind:     kind,
			Repeated: fd.IsList(),
			Nullable: fd.HasOptionalKeyword(),
		}
		switch kind {
		case ir.KindMessage:
			f.Message = string(fd.Message().FullName())
		case ir.KindEnum:
			f.Enum = string(fd.Enum().FullName())
		}
		m.Fields = append(m.Fields, f)
	}
	return m
}

func (l *loader) pgFile(f protoreflect.FileDescriptor) {
	o := get[*pgv1.File](l, f, pgv1.E_File)
	if o == nil {
		return
	}
	if l.pg.Files == nil {
		l.pg.Files = map[string]*pgir.File{}
	}
	l.pg.Files[f.Path()] = &pgir.File{MinVersion: o.GetMinVersion(), Extensions: o.GetExtensions()}
}

func (l *loader) pgTable(md protoreflect.MessageDescriptor, e *ir.Entity) {
	var t *pgir.Table
	ensure := func() *pgir.Table {
		if t == nil {
			t = &pgir.Table{}
			if l.pg.Tables == nil {
				l.pg.Tables = map[string]*pgir.Table{}
			}
			l.pg.Tables[e.FullName] = t
		}
		return t
	}

	if o := get[*pgv1.Table](l, md, pgv1.E_Table); o != nil {
		ensure().IndexBudget = o.GetIndexBudget()
		for _, idx := range o.GetIndexes() {
			cols, ok := l.sortKeys(md, idx.GetColumns())
			if !ok {
				continue
			}
			t.Indexes = append(t.Indexes, &pgir.Index{
				Name:    idx.GetName(),
				Columns: cols,
				Include: idx.GetInclude(),
				Where:   idx.GetWhere(),
				Unique:  idx.GetUnique(),
			})
		}
	}

	for i := range md.Fields().Len() {
		fd := md.Fields().Get(i)
		o := get[*pgv1.Column](l, fd, pgv1.E_Column)
		if o == nil {
			continue
		}
		tbl := ensure()
		if tbl.Columns == nil {
			tbl.Columns = map[int32]*pgir.Column{}
		}
		tbl.Columns[int32(fd.Number())] = &pgir.Column{
			Type:       pgType(o.GetType()),
			CustomType: o.GetCustomType(),
			Default:    o.GetDefault(),
			GoType:     o.GetGoType(),
		}
	}
}

// pgType turns a dal.pg.v1 Type into its SQL name: TYPE_DOUBLE_PRECISION
// becomes "double precision". Unspecified becomes "" (inferred later).
func pgType(t pgv1.Type) string {
	if t == pgv1.Type_TYPE_UNSPECIFIED {
		return ""
	}
	name := strings.TrimPrefix(t.String(), "TYPE_")
	return strings.ToLower(strings.ReplaceAll(name, "_", " "))
}

// snakeCase converts a message name to a table name: "OrderItem" becomes
// "order_item" and "HTTPRequestLog" becomes "http_request_log".
func snakeCase(s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		if unicode.IsUpper(r) {
			prevLower := i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1]))
			acronymEnd := i > 0 && unicode.IsUpper(runes[i-1]) && i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if prevLower || acronymEnd {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}
