package pg

import "fmt"

// _maxIdentifier is NAMEDATALEN-1: Postgres silently truncates longer names.
const _maxIdentifier = 63

// _reserved holds Postgres keywords that can't be table or column names
// unquoted: the "reserved" and "reserved (can be function or type)" columns
// of the SQL Key Words appendix (PostgreSQL 16).
var _reserved = map[string]bool{}

func init() {
	for _, w := range []string{
		"all", "analyse", "analyze", "and", "any", "array", "as", "asc",
		"asymmetric", "authorization", "binary", "both", "case", "cast",
		"check", "collate", "collation", "column", "concurrently",
		"constraint", "create", "cross", "current_catalog", "current_date",
		"current_role", "current_schema", "current_time", "current_timestamp",
		"current_user", "default", "deferrable", "desc", "distinct", "do",
		"else", "end", "except", "false", "fetch", "for", "foreign", "freeze",
		"from", "full", "grant", "group", "having", "ilike", "in", "initially",
		"inner", "intersect", "into", "is", "isnull", "join", "lateral",
		"leading", "left", "like", "limit", "localtime", "localtimestamp",
		"natural", "not", "notnull", "null", "offset", "on", "only", "or",
		"order", "outer", "overlaps", "placing", "primary", "references",
		"returning", "right", "select", "session_user", "similar", "some",
		"symmetric", "system_user", "table", "tablesample", "then", "to",
		"trailing", "true", "union", "unique", "user", "using", "variadic",
		"verbose", "when", "where", "window", "with",
	} {
		_reserved[w] = true
	}
}

// checkIdentifier reports why name can't be used unquoted as a Postgres
// table, column or index name, or "" if it can. dalforge never quotes
// identifiers, so names must also work in hand-written custom queries.
func checkIdentifier(name string) string {
	if name == "" {
		return "is empty"
	}
	if len(name) > _maxIdentifier {
		return fmt.Sprintf("is %d bytes; Postgres silently truncates names to %d", len(name), _maxIdentifier)
	}
	for i, r := range name {
		ok := r == '_' || ('a' <= r && r <= 'z') || (i > 0 && ('0' <= r && r <= '9' || r == '$'))
		if !ok {
			return "must be lowercase letters, digits and underscores, starting with a letter or underscore"
		}
	}
	if _reserved[name] {
		return "is a reserved SQL keyword"
	}
	return ""
}
