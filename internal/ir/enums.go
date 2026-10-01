package ir

import "fmt"

// Kind is the logical type of a field.
type Kind int

// Kinds. Message kinds other than Timestamp become KindJSON in entities and
// KindMessage in request/response shapes.
const (
	KindUnspecified Kind = iota
	KindBool
	KindInt32
	KindInt64
	KindUint32
	KindUint64
	KindFloat
	KindDouble
	KindString
	KindBytes
	KindEnum
	KindTimestamp
	KindJSON
	KindMessage
)

var _kindNames = []string{
	"unspecified", "bool", "int32", "int64", "uint32", "uint64", "float",
	"double", "string", "bytes", "enum", "timestamp", "json", "message",
}

func (k Kind) String() string { return enumString(_kindNames, int(k)) }

// MarshalText implements encoding.TextMarshaler.
func (k Kind) MarshalText() ([]byte, error) { return enumText(_kindNames, int(k), "kind") }

// Format refines a field's kind; see dal.v1.Format.
type Format int

// Formats.
const (
	FormatNone Format = iota
	FormatUUID
	FormatDecimal
	FormatJSON
)

var _formatNames = []string{"none", "uuid", "decimal", "json"}

func (f Format) String() string { return enumString(_formatNames, int(f)) }

// MarshalText implements encoding.TextMarshaler.
func (f Format) MarshalText() ([]byte, error) { return enumText(_formatNames, int(f), "format") }

// Role marks a column whose value the generated code manages.
type Role int

// Roles.
const (
	RoleNone Role = iota
	RoleCreateTime
	RoleUpdateTime
	RoleDeleteTime
	RoleVersion
)

var _roleNames = []string{"none", "create_time", "update_time", "delete_time", "version"}

func (r Role) String() string { return enumString(_roleNames, int(r)) }

// MarshalText implements encoding.TextMarshaler.
func (r Role) MarshalText() ([]byte, error) { return enumText(_roleNames, int(r), "role") }

// State is a column's lifecycle state.
type State int

// States. The loader turns an unset state into StateActive.
const (
	StateActive State = iota
	StateDeprecated
)

var _stateNames = []string{"active", "deprecated"}

func (s State) String() string { return enumString(_stateNames, int(s)) }

// MarshalText implements encoding.TextMarshaler.
func (s State) MarshalText() ([]byte, error) { return enumText(_stateNames, int(s), "state") }

// Consistency is the read consistency a query needs. The loader turns an
// unset consistency into ConsistencyEventual.
type Consistency int

// Consistencies.
const (
	ConsistencyEventual Consistency = iota
	ConsistencyStrong
)

var _consistencyNames = []string{"eventual", "strong"}

func (c Consistency) String() string { return enumString(_consistencyNames, int(c)) }

// MarshalText implements encoding.TextMarshaler.
func (c Consistency) MarshalText() ([]byte, error) {
	return enumText(_consistencyNames, int(c), "consistency")
}

// Source records where a value came from, so lint can tell an explicit
// choice from a fallback (e.g. DAL103 fires only on a defaulted sort).
type Source int

// Sources.
const (
	// SourceDeclared means the IDL states the value.
	SourceDeclared Source = iota
	// SourceDefaulted means the loader filled in a core default.
	SourceDefaulted
	// SourceInherited means a backend pass derived it, e.g. a sort inherited
	// from an explicit Postgres index.
	SourceInherited
)

var _sourceNames = []string{"declared", "defaulted", "inherited"}

func (s Source) String() string { return enumString(_sourceNames, int(s)) }

// MarshalText implements encoding.TextMarshaler.
func (s Source) MarshalText() ([]byte, error) { return enumText(_sourceNames, int(s), "source") }

func enumString(names []string, v int) string {
	if v < 0 || v >= len(names) {
		return fmt.Sprintf("unknown(%d)", v)
	}
	return names[v]
}

func enumText(names []string, v int, what string) ([]byte, error) {
	if v < 0 || v >= len(names) {
		return nil, fmt.Errorf("unknown %s %d", what, v)
	}
	return []byte(names[v]), nil
}
