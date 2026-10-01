package pgv1

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	// Imported for its init-time registration: the global registry panics if
	// any dal.v1 extension collides with a dal.pg.v1 one on the same extendee.
	_ "github.com/gisripa/dalforge/proto/dal/v1"
)

// TestExtensionsPinned guards the wire contract of the options: every user IDL
// encodes these numbers, so they must never change.
func TestExtensionsPinned(t *testing.T) {
	tests := []struct {
		name         string
		ext          protoreflect.ExtensionType
		wantName     protoreflect.FullName
		wantNumber   protoreflect.FieldNumber
		wantExtendee protoreflect.FullName
	}{
		{name: "file", ext: E_File, wantName: "dal.pg.v1.file", wantNumber: 51010, wantExtendee: "google.protobuf.FileOptions"},
		{name: "table", ext: E_Table, wantName: "dal.pg.v1.table", wantNumber: 51010, wantExtendee: "google.protobuf.MessageOptions"},
		{name: "column", ext: E_Column, wantName: "dal.pg.v1.column", wantNumber: 51010, wantExtendee: "google.protobuf.FieldOptions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := tt.ext.TypeDescriptor()
			if got := d.FullName(); got != tt.wantName {
				t.Errorf("FullName() = %q, want %q", got, tt.wantName)
			}
			if got := d.Number(); got != tt.wantNumber {
				t.Errorf("Number() = %d, want %d", got, tt.wantNumber)
			}
			if got := d.ContainingMessage().FullName(); got != tt.wantExtendee {
				t.Errorf("extendee = %q, want %q", got, tt.wantExtendee)
			}
		})
	}
}

// TestImportPath guards the path users write in `import "..."`.
func TestImportPath(t *testing.T) {
	const want = "dal/pg/v1/options.proto"
	if got := File_dal_pg_v1_options_proto.Path(); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}
