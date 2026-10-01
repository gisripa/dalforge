// Package idl loads DALForge IDL files: protobuf sources annotated with the
// dal.v1 and dal.pg.v1 options.
package idl

import (
	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/reflect/protoreflect"

	pgv1 "github.com/gisripa/dalforge/proto/dal/pg/v1"
	dalv1 "github.com/gisripa/dalforge/proto/dal/v1"
)

// _bundled holds the options files served from the binary, keyed by the
// import path users write. They come from the compiled Go bindings, so the
// options a user's IDL is checked against are exactly the ones this binary
// reads, and options values decode into the typed dalv1/pgv1 messages.
var _bundled = map[string]protoreflect.FileDescriptor{
	dalv1.File_dal_v1_options_proto.Path():   dalv1.File_dal_v1_options_proto,
	pgv1.File_dal_pg_v1_options_proto.Path(): pgv1.File_dal_pg_v1_options_proto,
}

// NewResolver returns a resolver for compiling user IDL. Imports resolve in
// this order:
//
//  1. the bundled dalforge options (dal/v1/options.proto, dal/pg/v1/options.proto),
//     which always win, so a stale vendored copy on an import path is never used;
//  2. sources under importPaths;
//  3. the protobuf well-known types (google/protobuf/*.proto).
func NewResolver(importPaths ...string) protocompile.Resolver {
	sources := &protocompile.SourceResolver{ImportPaths: importPaths}
	return protocompile.WithStandardImports(protocompile.ResolverFunc(func(path string) (protocompile.SearchResult, error) {
		if fd, ok := _bundled[path]; ok {
			return protocompile.SearchResult{Desc: fd}, nil
		}
		// Delegate directly rather than through CompositeResolver, which would
		// report the first resolver's error and hide the source lookup's.
		return sources.FindFileByPath(path)
	}))
}
