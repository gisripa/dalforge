package ir

import (
	"encoding/json"
	"strings"
	"testing"
)

func testEntity() *Entity {
	return &Entity{
		FullName: "shop.v1.Item",
		Fields: []*Field{
			{Column: "tenant_id", PrimaryKey: true},
			{Column: "name"},
			{Column: "id", PrimaryKey: true},
		},
	}
}

func TestEntityLookups(t *testing.T) {
	e := testEntity()
	if got := e.Column("name"); got == nil || got.Column != "name" {
		t.Errorf("Column(name) = %v, want the name field", got)
	}
	if got := e.Column("missing"); got != nil {
		t.Errorf("Column(missing) = %v, want nil", got)
	}
	if got := strings.Join(e.PrimaryKey(), ","); got != "tenant_id,id" {
		t.Errorf("PrimaryKey() = %q, want declaration order tenant_id,id", got)
	}

	s := &Schema{Entities: []*Entity{e}}
	if s.Entity("shop.v1.Item") != e || s.Entity("shop.v1.Other") != nil {
		t.Error("Schema.Entity lookup is wrong")
	}
}

func TestEnumText(t *testing.T) {
	tests := []struct {
		name string
		v    interface{ MarshalText() ([]byte, error) }
		want string
	}{
		{name: "kind", v: KindTimestamp, want: "timestamp"},
		{name: "format", v: FormatUUID, want: "uuid"},
		{name: "role", v: RoleDeleteTime, want: "delete_time"},
		{name: "state", v: StateDeprecated, want: "deprecated"},
		{name: "consistency", v: ConsistencyStrong, want: "strong"},
		{name: "source", v: SourceInherited, want: "inherited"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.v.MarshalText()
			if err != nil || string(got) != tt.want {
				t.Errorf("MarshalText() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}

	if _, err := Kind(99).MarshalText(); err == nil {
		t.Error("Kind(99).MarshalText() error = nil, want an error")
	}
	if got := Role(99).String(); got != "unknown(99)" {
		t.Errorf("Role(99).String() = %q, want unknown(99)", got)
	}
}

func TestQueryJSON(t *testing.T) {
	q := &Query{
		Method: "ListByAccount",
		Spec: &List{
			Eq:      []string{"account_id"},
			OrderBy: Sort{Columns: []SortColumn{{Column: "created_at", Desc: true}}, Source: SourceDeclared},
		},
	}
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	want := `"spec":{"list":{"eq":["account_id"],"order_by":{"columns":[{"column":"created_at","desc":true}],"source":"declared"},"consistency":"eventual"}}`
	if !strings.Contains(string(b), want) {
		t.Errorf("json = %s\nwant it to contain %s", b, want)
	}

	if _, err := json.Marshal(&Query{Method: "Broken"}); err == nil {
		t.Error("marshal of a query without spec: error = nil, want an error")
	}
}

func TestSpecKinds(t *testing.T) {
	specs := []Spec{&Get{}, &List{}, &Create{}, &Update{}, &Delete{}, &Upsert{}}
	want := []string{"get", "list", "create", "update", "delete", "upsert"}
	for i, s := range specs {
		if got := s.Kind(); got != want[i] {
			t.Errorf("%T.Kind() = %q, want %q", s, got, want[i])
		}
	}
}

func TestPosString(t *testing.T) {
	if got := (Pos{File: "a/b.proto", Line: 3, Col: 7}).String(); got != "a/b.proto:3:7" {
		t.Errorf("Pos.String() = %q", got)
	}
}
