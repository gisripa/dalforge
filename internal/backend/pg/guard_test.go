package pg

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestGeneratedImports guards design §7's rules on every generated DAL
// package (the emit goldens): the API file (dal.go) never imports a driver
// package, and no generated Go imports protobuf or dalforge internals.
func TestGeneratedImports(t *testing.T) {
	forbiddenEverywhere := []string{"google.golang.org/protobuf", "github.com/gisripa/dalforge/proto", "github.com/gisripa/dalforge/internal"}
	forbiddenInAPI := []string{"github.com/jackc/pgx", "github.com/jackc/pgx/v5/pgtype", "github.com/jackc/pgx/v5/pgconn"}

	checked := 0
	err := filepath.WalkDir("testdata/emit", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go.golden") {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		api := strings.HasSuffix(path, "/dal.go.golden")
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range forbiddenEverywhere {
				if strings.HasPrefix(p, bad) {
					t.Errorf("%s imports %s", path, p)
				}
			}
			for _, bad := range forbiddenInAPI {
				if api && strings.HasPrefix(p, bad) {
					t.Errorf("%s (the DAL API) imports driver package %s", path, p)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 8 { // dal.go + repository.go for each of the 4 fixtures
		t.Errorf("checked only %d generated Go files; the goldens moved?", checked)
	}
}

// TestProtobufStaysInTheLoader: only internal/idl may depend on protobuf.
// The IR, the checks, the backend, and the runtime that generated code
// imports must not.
func TestProtobufStaysInTheLoader(t *testing.T) {
	for _, pkg := range []string{
		"github.com/gisripa/dalforge/internal/ir/...",
		"github.com/gisripa/dalforge/internal/check",
		"github.com/gisripa/dalforge/internal/backend/pg",
		"github.com/gisripa/dalforge/dal/...",
	} {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		for _, dep := range strings.Fields(string(out)) {
			if strings.HasPrefix(dep, "google.golang.org/protobuf") || strings.HasPrefix(dep, "github.com/gisripa/dalforge/proto") {
				t.Errorf("%s depends on %s", pkg, dep)
			}
		}
	}
}
