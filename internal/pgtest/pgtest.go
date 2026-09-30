// Package pgtest gives integration tests an isolated Postgres database on the
// local compose server (see compose.yaml and `mise run db:up`).
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvDatabaseURL names the environment variable holding the admin connection
// URL. mise.toml sets it to the compose Postgres.
const EnvDatabaseURL = "DALFORGE_TEST_DATABASE_URL"

// New creates a fresh database for t, returns a pool connected to it, and
// drops the database when t finishes. Tests can therefore run in parallel
// without sharing state.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()

	adminURL := os.Getenv(EnvDatabaseURL)
	if adminURL == "" {
		t.Fatalf("%s is not set; run integration tests with `mise run test:integration`", EnvDatabaseURL)
	}

	// Cleanup runs after t's context is cancelled, so use a detached one.
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to %s: %v (is Postgres up? `mise run db:up`)", EnvDatabaseURL, err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })

	name := "dalforge_test_" + randomSuffix(t)
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
	})

	cfg, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvDatabaseURL, err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to database %s: %v", name, err)
	}
	t.Cleanup(pool.Close) // registered last, so it runs before the drop
	return pool
}

func randomSuffix(t testing.TB) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random database name: %v", err)
	}
	return hex.EncodeToString(b)
}
