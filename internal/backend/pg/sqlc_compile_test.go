//go:build integration

package pg

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSQLCOutputCompiles builds sqlc's generated Go for each fixture in a
// throwaway module, with the real dependencies the overrides import (pgx,
// uuid, pgvector-go). It needs the network for the first module download, so
// it lives in the integration suite.
func TestSQLCOutputCompiles(t *testing.T) {
	bin := sqlcBinary(t)
	for _, tt := range _fixtures {
		t.Run(tt.name, func(t *testing.T) {
			dir := generate(t, bin, tt.file)
			write(t, filepath.Join(dir, "go.mod"), []byte("module example.com/dalforgefixture\n\ngo 1.26.0\n"))
			for _, args := range [][]string{{"mod", "tidy"}, {"build", "./..."}, {"vet", "./..."}} {
				cmd := exec.Command("go", args...)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("go %v: %v\n%s", args, err, out)
				}
			}
		})
	}
}
