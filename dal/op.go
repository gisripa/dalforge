package dal

import "fmt"

// OpKind is the access pattern of an operation.
type OpKind int

// Operation kinds.
const (
	OpGet OpKind = iota + 1
	OpList
	OpCreate
	OpUpdate
	OpDelete
	OpUpsert
	OpTx
)

var _opKindNames = map[OpKind]string{
	OpGet: "get", OpList: "list", OpCreate: "create", OpUpdate: "update",
	OpDelete: "delete", OpUpsert: "upsert", OpTx: "tx",
}

func (k OpKind) String() string {
	if n, ok := _opKindNames[k]; ok {
		return n
	}
	return fmt.Sprintf("opkind(%d)", int(k))
}

// Op describes a repository operation. Generated code fills it at generation
// time, including whether retrying the operation is idempotent (design §7).
type Op struct {
	Entity     string // e.g. "Order"
	Method     string // e.g. "ListByAccount"
	Kind       OpKind
	Idempotent bool
}

func (o Op) String() string {
	return fmt.Sprintf("%s.%s (%s)", o.Entity, o.Method, o.Kind)
}
