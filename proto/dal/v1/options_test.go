package dalv1

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
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
		{name: "table", ext: E_Table, wantName: "dal.v1.table", wantNumber: 51000, wantExtendee: "google.protobuf.MessageOptions"},
		{name: "field", ext: E_Field, wantName: "dal.v1.field", wantNumber: 51000, wantExtendee: "google.protobuf.FieldOptions"},
		{name: "store", ext: E_Store, wantName: "dal.v1.store", wantNumber: 51000, wantExtendee: "google.protobuf.ServiceOptions"},
		{name: "query", ext: E_Query, wantName: "dal.v1.query", wantNumber: 51000, wantExtendee: "google.protobuf.MethodOptions"},
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
	const want = "dal/v1/options.proto"
	if got := File_dal_v1_options_proto.Path(); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}
