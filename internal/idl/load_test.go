package idl

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gisripa/dalforge/internal/golden"
)

func load(t *testing.T, importPath string, files ...string) *Result {
	t.Helper()
	res, err := Load(context.Background(), files, Options{ImportPaths: []string{importPath}})
	if err != nil {
		t.Fatalf("Load(%q) error = %v", files, err)
	}
	return res
}

func dump(t *testing.T, res *Result) []byte {
	t.Helper()
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(b, '\n')
}

// TestLoadGolden pins the IR built from each fixture. Run `mise run
// test:update` after an intended change and review the diff.
func TestLoadGolden(t *testing.T) {
	tests := []struct {
		name  string
		files []string
	}{
		{name: "orders", files: []string{"orders/v1/orders.proto"}},
		{name: "types", files: []string{"types/v1/types.proto"}},
		{name: "defaults", files: []string{"defaults/v1/defaults.proto"}},
		{name: "shop", files: []string{"shop/v1/stores.proto"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			golden.Assert(t, "load/"+tt.name+".json", dump(t, load(t, "testdata", tt.files...)))
		})
	}
}

// TestLoadCrossFileEntity: an entity reached only through a store in another
// file loads the same as when its own file is passed too.
func TestLoadCrossFileEntity(t *testing.T) {
	storeOnly := dump(t, load(t, "testdata", "shop/v1/stores.proto"))
	both := dump(t, load(t, "testdata", "shop/v1/account.proto", "shop/v1/stores.proto"))
	if string(storeOnly) != string(both) {
		t.Errorf("IR differs when account.proto is passed explicitly:\nstore only:\n%s\nboth:\n%s", storeOnly, both)
	}
}

func TestLoadErrors(t *testing.T) {
	const header = `syntax = "proto3";
package e;
import "dal/v1/options.proto";
import "google/protobuf/duration.proto";
`
	tests := []struct {
		name string
		src  string // appended to header (4 lines), so declarations start on line 5
		want []string
	}{
		{
			name: "no primary key",
			src:  `message A { option (dal.v1.table) = {}; string id = 1; }`,
			want: []string{"e.proto:5:1: entity e.A has no primary key"},
		},
		{
			name: "oneof in entity",
			src: `message A { option (dal.v1.table) = {};
  string id = 1 [(dal.v1.field).primary_key = true];
  oneof v { string s = 2; int64 n = 3; } }`,
			want: []string{"e.proto:7:13: field s is in oneof v", "e.proto:7:27: field n is in oneof v"},
		},
		{
			name: "unsupported well-known type",
			src: `message A { option (dal.v1.table) = {};
  string id = 1 [(dal.v1.field).primary_key = true];
  google.protobuf.Duration ttl = 2; }`,
			want: []string{"e.proto:7:3: field ttl: google.protobuf.Duration is not supported"},
		},
		{
			name: "store without entity",
			src:  `service S { option (dal.v1.store) = {}; }`,
			want: []string{"e.proto:5:1: store S must name its entity"},
		},
		{
			name: "store with unknown entity",
			src:  `service S { option (dal.v1.store) = {entity: "Missing"}; }`,
			want: []string{`e.proto:5:1: store S: entity "Missing" not found (looked for message e.Missing)`},
		},
		{
			name: "rpc without query",
			src: `message A { string id = 1 [(dal.v1.field).primary_key = true]; }
service S { option (dal.v1.store) = {entity: "A"};
  rpc Get(A) returns (A); }`,
			want: []string{"e.proto:7:3: rpc Get has no (dal.v1.query) option"},
		},
		{
			name: "empty query",
			src: `message A { string id = 1 [(dal.v1.field).primary_key = true]; }
service S { option (dal.v1.store) = {entity: "A"};
  rpc Get(A) returns (A) { option (dal.v1.query) = {}; } }`,
			want: []string{"e.proto:7:3: rpc Get: (dal.v1.query) sets no access pattern"},
		},
		{
			name: "invalid sort",
			src: `message A { string id = 1 [(dal.v1.field).primary_key = true]; }
service S { option (dal.v1.store) = {entity: "A"};
  rpc L(A) returns (A) { option (dal.v1.query) = {list: {order_by: ["id SIDEWAYS"]}}; } }`,
			want: []string{`e.proto:7:3: invalid sort "id SIDEWAYS"`},
		},
		{
			name: "streaming rpc",
			src: `message A { string id = 1 [(dal.v1.field).primary_key = true]; }
service S { option (dal.v1.store) = {entity: "A"};
  rpc W(stream A) returns (A) { option (dal.v1.query) = {create: {}}; } }`,
			want: []string{"e.proto:7:3: rpc W: streaming rpcs are not supported"},
		},
		{
			name: "all problems reported in one run",
			src: `message A { option (dal.v1.table) = {}; string id = 1; }
message B { option (dal.v1.table) = {}; string id = 1; }`,
			want: []string{"entity e.A has no primary key", "entity e.B has no primary key"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeProto(t, dir, "e.proto", header+tt.src+"\n")

			_, err := Load(context.Background(), []string{"e.proto"}, Options{ImportPaths: []string{dir}})
			if err == nil {
				t.Fatal("Load() error = nil, want errors")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load() error =\n%v\nwant it to contain %q", err, want)
				}
			}
		})
	}
}

func TestLoadRejectsProto2(t *testing.T) {
	dir := t.TempDir()
	writeProto(t, dir, "p2.proto", `syntax = "proto2";
package p2;
import "dal/v1/options.proto";
message A { option (dal.v1.table) = {}; optional string id = 1 [(dal.v1.field).primary_key = true]; }
`)
	_, err := Load(context.Background(), []string{"p2.proto"}, Options{ImportPaths: []string{dir}})
	if err == nil || !strings.Contains(err.Error(), `must use syntax = "proto3"`) {
		t.Errorf("Load() error = %v, want a proto3 requirement", err)
	}
}

func TestParseSortKey(t *testing.T) {
	tests := []struct {
		in       string
		wantCol  string
		wantDesc bool
		wantErr  bool
	}{
		{in: "created_at", wantCol: "created_at"},
		{in: "created_at ASC", wantCol: "created_at"},
		{in: "created_at desc", wantCol: "created_at", wantDesc: true},
		{in: "  created_at   DESC  ", wantCol: "created_at", wantDesc: true},
		{in: "", wantErr: true},
		{in: "a b c", wantErr: true},
		{in: "created_at DOWN", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseSortKey(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseSortKey(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got.Field != tt.wantCol || got.Desc != tt.wantDesc {
				t.Errorf("parseSortKey(%q) = %+v, want {%s %v}", tt.in, got, tt.wantCol, tt.wantDesc)
			}
		})
	}
}

func TestSnakeCase(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Order", "order"},
		{"OrderItem", "order_item"},
		{"HTTPRequestLog", "http_request_log"},
		{"OrderV2", "order_v2"},
		{"APIKey", "api_key"},
		{"already_snake", "already_snake"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := snakeCase(tt.in); got != tt.want {
				t.Errorf("snakeCase(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
