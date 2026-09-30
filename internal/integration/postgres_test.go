//go:build integration

package integration

import (
	"strings"
	"testing"

	"github.com/gisripa/dalforge/internal/pgtest"
)

// _wantMajor is the Postgres major version DALForge targets (Aurora PG 16 LTS).
const _wantMajor = "16."

func TestPostgresVersion(t *testing.T) {
	pool := pgtest.New(t)

	var version string
	if err := pool.QueryRow(t.Context(), "SHOW server_version").Scan(&version); err != nil {
		t.Fatalf("SHOW server_version: %v", err)
	}
	if !strings.HasPrefix(version, _wantMajor) {
		t.Errorf("server_version = %q, want %sx", version, _wantMajor)
	}
}
