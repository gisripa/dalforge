package idl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVendoredCopyHashesLikeBundled: the repository's own .proto sources,
// compiled, hash the same as the descriptors embedded in the binary. That is
// what makes DAL116 (a vendored copy differs) free of false alarms.
func TestVendoredCopyHashesLikeBundled(t *testing.T) {
	bundled := BundledHashes()
	for _, p := range BundledPaths() {
		got, err := VendoredHash(context.Background(), p, "../../proto")
		if err != nil {
			t.Fatal(err)
		}
		if got != bundled[p] || !strings.HasPrefix(got, "sha256:") {
			t.Errorf("%s: source hashes to %s, bundled to %s", p, got, bundled[p])
		}
	}
}

func TestVendoredCopyThatDiffers(t *testing.T) {
	dir := t.TempDir()
	src, err := os.ReadFile("../../proto/dal/v1/options.proto")
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(src), "bool unique = 4;", "bool unique = 4;\n  bool frozen = 7;", 1)
	p := filepath.Join(dir, "dal", "v1", "options.proto")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := VendoredHash(context.Background(), "dal/v1/options.proto", dir)
	if err != nil {
		t.Fatal(err)
	}
	if got == BundledHashes()["dal/v1/options.proto"] {
		t.Error("an edited copy must hash differently")
	}
}
