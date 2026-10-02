package idl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// BundledPaths returns the import paths of the options files served from the
// binary, sorted.
func BundledPaths() []string {
	paths := make([]string, 0, len(_bundled))
	for p := range _bundled {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	return paths
}

// BundledHashes returns a content hash ("sha256:…") of each bundled options
// file, keyed by import path. dalforge.lock records them so a different
// binary (with different options) is noticed.
func BundledHashes() map[string]string {
	out := make(map[string]string, len(_bundled))
	for p, fd := range _bundled {
		out[p] = hash(fd)
	}
	return out
}

// VendoredHash compiles a vendored copy of a bundled options file (e.g. one
// kept under a proto root for editor support) and returns its hash, computed
// like BundledHashes, so the two can be compared.
func VendoredHash(ctx context.Context, importPath, dir string) (string, error) {
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: []string{dir}})}
	files, err := c.Compile(ctx, importPath)
	if err != nil {
		return "", fmt.Errorf("compile vendored %s: %w", importPath, err)
	}
	return hash(files.FindFileByPath(importPath)), nil
}

// hash digests a file's descriptor without source info, so a compiled source
// copy and the embedded descriptor of the same file hash equally.
func hash(fd protoreflect.FileDescriptor) string {
	p := protodesc.ToFileDescriptorProto(fd)
	p.SourceCodeInfo = nil
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(p)
	if err != nil {
		panic(fmt.Sprintf("idl: marshal descriptor of %s: %v", fd.Path(), err)) // a valid descriptor always marshals
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
