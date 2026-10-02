package docgen

import (
	"context"
	"fmt"
	"strings"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// _optionFiles are the options protos, as imported by users.
var _optionFiles = []string{"dal/v1/options.proto", "dal/pg/v1/options.proto"}

// OptionReference renders docs/manual/options.md from the options protos
// under protoDir, using their comments as the descriptions. The protos'
// comments are therefore the source of truth for what each option means.
func OptionReference(ctx context.Context, protoDir string) (string, error) {
	c := protocompile.Compiler{
		Resolver:       protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: []string{protoDir}}),
		SourceInfoMode: protocompile.SourceInfoStandard,
	}
	files, err := c.Compile(ctx, _optionFiles...)
	if err != nil {
		return "", fmt.Errorf("compile options: %w", err)
	}

	var b strings.Builder
	b.WriteString(_header + "\n\n# Option reference\n\n")
	b.WriteString("Every option dalforge reads, generated from the comments in its options\n")
	b.WriteString("protos. [Writing the IDL](idl.md) explains how they fit together.\n")
	for _, fd := range files {
		file := protoreflect.FileDescriptor(fd)
		fmt.Fprintf(&b, "\n## `%s`\n\n`import \"%s\";`\n", file.Package(), file.Path())

		// Extensions first: they're what users write, e.g. (dal.v1.table).
		exts := file.Extensions()
		if exts.Len() > 0 {
			b.WriteString("\n| Option | On | Type |\n|---|---|---|\n")
			for i := range exts.Len() {
				x := exts.Get(i)
				fmt.Fprintf(&b, "| `(%s)` | %s | %s |\n", x.FullName(), target(x.ContainingMessage().FullName()), typeRef(x))
			}
		}
		msgs := file.Messages()
		for i := range msgs.Len() {
			message(&b, msgs.Get(i))
		}
		enums := file.Enums()
		for i := range enums.Len() {
			enum(&b, enums.Get(i))
		}
	}
	return b.String(), nil
}

func message(b *strings.Builder, m protoreflect.MessageDescriptor) {
	fmt.Fprintf(b, "\n### %s\n", m.FullName())
	if doc := comment(m); doc != "" {
		b.WriteString("\n" + doc + "\n")
	}
	fields := m.Fields()
	if fields.Len() == 0 {
		b.WriteString("\nNo fields.\n")
		return
	}
	b.WriteString("\n| Field | Type | Description |\n|---|---|---|\n")
	for i := range fields.Len() {
		f := fields.Get(i)
		desc := comment(f)
		if o := f.ContainingOneof(); o != nil && !o.IsSynthetic() {
			desc = strings.TrimSpace(fmt.Sprintf("One of `%s`. %s", o.Name(), desc))
		}
		fmt.Fprintf(b, "| `%s` | %s | %s |\n", f.Name(), typeRef(f), cell(desc))
	}
}

func enum(b *strings.Builder, e protoreflect.EnumDescriptor) {
	fmt.Fprintf(b, "\n### %s\n", e.FullName())
	if doc := comment(e); doc != "" {
		b.WriteString("\n" + doc + "\n")
	}
	values := e.Values()
	described := false
	for i := 1; i < values.Len(); i++ {
		described = described || comment(values.Get(i)) != ""
	}
	if !described { // self-describing values, e.g. TYPE_TIMESTAMPTZ
		var names []string
		for i := range values.Len() {
			if values.Get(i).Number() != 0 {
				names = append(names, "`"+string(values.Get(i).Name())+"`")
			}
		}
		b.WriteString("\nValues: " + strings.Join(names, ", ") + ". Zero (`" + string(values.Get(0).Name()) + "`) means unset.\n")
		return
	}
	b.WriteString("\n| Value | Description |\n|---|---|\n")
	for i := range values.Len() {
		v := values.Get(i)
		desc := comment(v)
		if desc == "" && v.Number() == 0 {
			desc = "Unset."
		}
		fmt.Fprintf(b, "| `%s` | %s |\n", v.Name(), cell(desc))
	}
}

// typeRef renders a field's type, linking message and enum types to their
// section.
func typeRef(f protoreflect.FieldDescriptor) string {
	var t string
	switch {
	case f.Message() != nil:
		t = link(f.Message().FullName())
	case f.Enum() != nil:
		t = link(f.Enum().FullName())
	default:
		t = "`" + f.Kind().String() + "`"
	}
	if f.IsList() {
		t = "repeated " + t
	}
	return t
}

func link(name protoreflect.FullName) string {
	return fmt.Sprintf("[%s](#%s)", name.Name(), anchor(string(name)))
}

func target(name protoreflect.FullName) string {
	switch name {
	case "google.protobuf.FileOptions":
		return "file"
	case "google.protobuf.MessageOptions":
		return "message"
	case "google.protobuf.FieldOptions":
		return "field"
	case "google.protobuf.ServiceOptions":
		return "service"
	case "google.protobuf.MethodOptions":
		return "rpc"
	}
	return string(name)
}

// comment is a declaration's leading comment, with the proto line breaks
// joined into Markdown paragraphs.
func comment(d protoreflect.Descriptor) string {
	loc := d.ParentFile().SourceLocations().ByDescriptor(d)
	raw := strings.TrimSpace(loc.LeadingComments)
	if raw == "" {
		return ""
	}
	var paras []string
	for p := range strings.SplitSeq(raw, "\n\n") {
		var lines []string
		for l := range strings.SplitSeq(p, "\n") {
			lines = append(lines, strings.TrimSpace(l))
		}
		paras = append(paras, strings.Join(lines, " "))
	}
	return strings.Join(paras, "\n\n")
}
