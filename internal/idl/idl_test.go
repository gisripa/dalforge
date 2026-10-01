// Package idl will load DALForge IDL files. For now it only holds a smoke test
// that keeps the draft dal.v1 and dal.pg.v1 options compiling.
package idl

import (
	"context"
	"testing"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestOptionsCompile(t *testing.T) {
	compiler := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			ImportPaths: []string{"../../proto", "testdata"},
		}),
	}
	files, err := compiler.Compile(context.Background(), "orders/v1/orders.proto")
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	fd := files.FindFileByPath("orders/v1/orders.proto")
	if fd == nil {
		t.Fatal("orders/v1/orders.proto not in compiled files")
	}

	tests := []struct {
		name string
		opts protoreflect.ProtoMessage
		ext  protoreflect.FullName
	}{
		{name: "table", opts: fd.Messages().ByName("Order").Options(), ext: "dal.v1.table"},
		{name: "field", opts: fd.Messages().ByName("Order").Fields().ByName("id").Options(), ext: "dal.v1.field"},
		{name: "store", opts: fd.Services().ByName("OrderStore").Options(), ext: "dal.v1.store"},
		{name: "query", opts: fd.Services().ByName("OrderStore").Methods().ByName("ListByAccount").Options(), ext: "dal.v1.query"},
		{name: "pg file", opts: fd.Options(), ext: "dal.pg.v1.file"},
		{name: "pg table", opts: fd.Messages().ByName("Order").Options(), ext: "dal.pg.v1.table"},
		{name: "pg column", opts: fd.Messages().ByName("Order").Fields().ByName("status").Options(), ext: "dal.pg.v1.column"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !hasExtension(tt.opts.ProtoReflect(), tt.ext) {
				t.Errorf("option %s not set", tt.ext)
			}
		})
	}
}

func hasExtension(m protoreflect.Message, name protoreflect.FullName) bool {
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		found = fd.IsExtension() && fd.FullName() == name
		return !found
	})
	return found
}
