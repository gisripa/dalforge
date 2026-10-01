package idl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	pgv1 "github.com/gisripa/dalforge/proto/dal/pg/v1"
	dalv1 "github.com/gisripa/dalforge/proto/dal/v1"
)

const _ordersPath = "orders/v1/orders.proto"

func compile(t *testing.T, path string, importPaths ...string) protoreflect.FileDescriptor {
	t.Helper()
	c := protocompile.Compiler{Resolver: NewResolver(importPaths...)}
	files, err := c.Compile(context.Background(), path)
	if err != nil {
		t.Fatalf("Compile(%q) error = %v", path, err)
	}
	return files.FindFileByPath(path)
}

func writeProto(t *testing.T, dir, path, src string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBundledImports compiles the example with only its own directory on the
// import path: the dal options must come from the binary.
func TestBundledImports(t *testing.T) {
	fd := compile(t, _ordersPath, "testdata")

	imports := map[string]protoreflect.FileDescriptor{}
	for i := range fd.Imports().Len() {
		imp := fd.Imports().Get(i)
		imports[imp.Path()] = imp.FileDescriptor
	}
	tests := []struct {
		path string
		want protoreflect.FileDescriptor
	}{
		{path: "dal/v1/options.proto", want: dalv1.File_dal_v1_options_proto},
		{path: "dal/pg/v1/options.proto", want: pgv1.File_dal_pg_v1_options_proto},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got, ok := imports[tt.path]
			if !ok {
				t.Fatalf("%s not imported", tt.path)
			}
			if got != tt.want {
				t.Errorf("%s resolved to %v, want the bundled descriptor", tt.path, got)
			}
		})
	}
}

// mustOption reads a dalforge option that the test fixture is known to set.
func mustOption[T proto.Message](t *testing.T, d protoreflect.Descriptor, xt protoreflect.ExtensionType) T {
	t.Helper()
	v, ok, err := option[T](d, xt)
	if err != nil {
		t.Fatalf("option(%s, %s) error = %v", d.FullName(), xt.TypeDescriptor().FullName(), err)
	}
	if !ok {
		t.Fatalf("option(%s, %s) not set", d.FullName(), xt.TypeDescriptor().FullName())
	}
	return v
}

// TestTypedOptions checks that option values decode into the typed binding
// messages, which is what the IDL loader reads.
func TestTypedOptions(t *testing.T) {
	fd := compile(t, _ordersPath, "testdata")
	order := fd.Messages().ByName("Order")
	store := fd.Services().ByName("OrderStore")

	table := mustOption[*dalv1.Table](t, order, dalv1.E_Table)
	id := mustOption[*dalv1.Field](t, order.Fields().ByName("id"), dalv1.E_Field)
	list := mustOption[*dalv1.Query](t, store.Methods().ByName("ListByAccount"), dalv1.E_Query).GetList()
	entity := mustOption[*dalv1.Store](t, store, dalv1.E_Store).GetEntity()
	pgFile := mustOption[*pgv1.File](t, fd, pgv1.E_File)
	status := mustOption[*pgv1.Column](t, order.Fields().ByName("status"), pgv1.E_Column)

	tests := []struct {
		name      string
		got, want any
	}{
		{name: "table name", got: table.GetName(), want: "orders"},
		{name: "shard key", got: strings.Join(table.GetShardKey(), ","), want: "account_id"},
		{name: "id format", got: id.GetFormat(), want: dalv1.Format_FORMAT_UUID},
		{name: "id primary key", got: id.GetPrimaryKey(), want: true},
		{name: "list eq", got: strings.Join(list.GetEq(), ","), want: "account_id"},
		{name: "list order_by", got: strings.Join(list.GetOrderBy(), ","), want: "created_at DESC"},
		{name: "store entity", got: entity, want: "Order"},
		{name: "pg min_version", got: pgFile.GetMinVersion(), want: uint32(16)},
		{name: "pg column default", got: status.GetDefault(), want: "'pending'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %v, want %v", tt.got, tt.want)
			}
		})
	}
}

// TestOptionUnset distinguishes "not set" from a zero value.
func TestOptionUnset(t *testing.T) {
	fd := compile(t, _ordersPath, "testdata")
	note := fd.Messages().ByName("Order").Fields().ByName("note") // has no options

	tests := []struct {
		name string
		d    protoreflect.Descriptor
		xt   protoreflect.ExtensionType
	}{
		{name: "field without options", d: note, xt: dalv1.E_Field},
		{name: "message without pg table", d: fd.Messages().ByName("GetByIdRequest"), xt: pgv1.E_Table},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok, err := option[proto.Message](tt.d, tt.xt)
			if err != nil {
				t.Fatalf("option() error = %v", err)
			}
			if ok {
				t.Errorf("option() ok = true, want false")
			}
		})
	}
}

// TestBundledWinsOverVendoredCopy puts a stale copy of dal/v1/options.proto
// first on the import path. If it were used, the example would not compile:
// the stale Table has no shard_key.
func TestBundledWinsOverVendoredCopy(t *testing.T) {
	vendor := t.TempDir()
	writeProto(t, vendor, "dal/v1/options.proto", `syntax = "proto3";
package dal.v1;
import "google/protobuf/descriptor.proto";
extend google.protobuf.MessageOptions { Table table = 51000; }
message Table { string name = 1; }
`)

	fd := compile(t, _ordersPath, vendor, "testdata")

	for i := range fd.Imports().Len() {
		imp := fd.Imports().Get(i)
		if imp.Path() != "dal/v1/options.proto" {
			continue
		}
		if imp.FileDescriptor != dalv1.File_dal_v1_options_proto {
			t.Errorf("dal/v1/options.proto resolved to the vendored copy, want the bundled descriptor")
		}
		return
	}
	t.Fatal("dal/v1/options.proto not imported")
}

// TestMissingImport keeps the source resolver's error, which names the file.
func TestMissingImport(t *testing.T) {
	dir := t.TempDir()
	writeProto(t, dir, "broken.proto", `syntax = "proto3";
package broken;
import "nope/missing.proto";
`)

	c := protocompile.Compiler{Resolver: NewResolver(dir)}
	_, err := c.Compile(context.Background(), "broken.proto")
	if err == nil {
		t.Fatal("Compile() error = nil, want a missing-import error")
	}
	if !strings.Contains(err.Error(), "nope/missing.proto") {
		t.Errorf("Compile() error = %v, want it to name nope/missing.proto", err)
	}
}
