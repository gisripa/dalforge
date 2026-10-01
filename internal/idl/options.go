package idl

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// option returns the value of the dalforge option xt on d, decoded into its
// generated Go type T, and whether it is set.
//
// protocompile stores custom option values as dynamicpb messages, even when
// the extension comes from a bundled descriptor, so proto.GetExtension with
// the generated extension type would panic. Round-tripping the options
// through the wire format against the global type registry, where the
// dalv1/pgv1 bindings register themselves, yields the typed values.
func option[T proto.Message](d protoreflect.Descriptor, xt protoreflect.ExtensionType) (T, bool, error) {
	var zero T
	opts := d.Options()
	if opts == nil {
		return zero, false, nil
	}

	b, err := proto.Marshal(opts)
	if err != nil {
		return zero, false, fmt.Errorf("encode options of %s: %w", d.FullName(), err)
	}
	typed := opts.ProtoReflect().New().Interface()
	if err := (proto.UnmarshalOptions{Resolver: protoregistry.GlobalTypes}).Unmarshal(b, typed); err != nil {
		return zero, false, fmt.Errorf("decode options of %s: %w", d.FullName(), err)
	}

	if !proto.HasExtension(typed, xt) {
		return zero, false, nil
	}
	v, ok := proto.GetExtension(typed, xt).(T)
	if !ok {
		return zero, false, fmt.Errorf("option %s on %s is %T, want %T",
			xt.TypeDescriptor().FullName(), d.FullName(), proto.GetExtension(typed, xt), zero)
	}
	return v, true, nil
}
