//go:build integration

package integration

import (
	"testing"

	"github.com/gisripa/dalforge/internal/pgtest"
)

// TestNumericAsString checks the FORMAT_DECIMAL mapping: pgx reads and
// writes Postgres numeric as a Go string, losslessly.
func TestNumericAsString(t *testing.T) {
	pool := pgtest.New(t)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, "CREATE TABLE prices (id int PRIMARY KEY, amount numeric, exact numeric(30, 10))"); err != nil {
		t.Fatal(err)
	}
	const big = "12345678901234567890.0123456789" // beyond float64 precision
	if _, err := pool.Exec(ctx, "INSERT INTO prices VALUES (1, $1, $2)", "0.1", big); err != nil {
		t.Fatalf("insert strings into numeric: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO prices VALUES (2, $1, NULL)", nil); err != nil {
		t.Fatalf("insert NULL: %v", err)
	}

	var amount, exact string
	if err := pool.QueryRow(ctx, "SELECT amount, exact FROM prices WHERE id = 1").Scan(&amount, &exact); err != nil {
		t.Fatalf("scan numeric into string: %v", err)
	}
	if amount != "0.1" || exact != big {
		t.Errorf("round trip = %q, %q; want 0.1, %s", amount, exact, big)
	}

	var maybe *string
	if err := pool.QueryRow(ctx, "SELECT amount FROM prices WHERE id = 2").Scan(&maybe); err != nil || maybe != nil {
		t.Errorf("NULL numeric into *string = %v, %v; want nil", maybe, err)
	}
}
