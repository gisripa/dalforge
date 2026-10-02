package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gisripa/dalforge/internal/config"
	"github.com/gisripa/dalforge/internal/idl"
	"github.com/gisripa/dalforge/internal/lock"
	"github.com/gisripa/dalforge/internal/pipeline"
)

func run(args ...string) (int, string) {
	var stderr bytes.Buffer
	code := Run(context.Background(), args, &stderr)
	return code, stderr.String()
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{name: "no args", args: nil, wantCode: ExitUsage, wantStderr: "Usage: dalforge"},
		{name: "help", args: []string{"-h"}, wantCode: ExitOK, wantStderr: "generate"},
		{name: "unknown command", args: []string{"frobnicate"}, wantCode: ExitUsage, wantStderr: `unknown command "frobnicate"`},
		{name: "unknown flag", args: []string{"-nope"}, wantCode: ExitUsage, wantStderr: "flag provided but not defined"},
		{name: "migrate", args: []string{"migrate"}, wantCode: ExitError, wantStderr: "dalforge migrate: not implemented"},
		{name: "lock help", args: []string{"lock", "-h"}, wantCode: ExitOK, wantStderr: "-upgrade"},
		{name: "generate help", args: []string{"generate", "-h"}, wantCode: ExitOK, wantStderr: "-config"},
		{name: "missing config", args: []string{"lint", "-config", "/nonexistent/dalforge.yaml"}, wantCode: ExitError, wantStderr: "no such file"},
		{name: "stray argument", args: []string{"lint", "extra"}, wantCode: ExitUsage, wantStderr: "unexpected arguments"},
		{name: "version", args: []string{"version"}, wantCode: ExitOK, wantStderr: "dalforge (devel)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stderr := run(tt.args...)
			if code != tt.wantCode || !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("Run(%q) = %d, %q; want %d containing %q", tt.args, code, stderr, tt.wantCode, tt.wantStderr)
			}
		})
	}
}

// project writes a dalforge project around the loader's orders fixture and
// returns its config path. edit, if set, rewrites the proto source.
func project(t *testing.T, edit func(string) string) string {
	t.Helper()
	dir := t.TempDir()
	src, err := os.ReadFile("../idl/testdata/orders/v1/orders.proto")
	if err != nil {
		t.Fatal(err)
	}
	proto := string(src)
	if edit != nil {
		proto = edit(proto)
	}
	for path, body := range map[string]string{
		"dalforge.yaml":                "version: 1\nmodule: example.com/shop\n",
		"proto/orders/v1/orders.proto": proto,
	} {
		p := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "dalforge.yaml")
}

func TestLint(t *testing.T) {
	code, stderr := run("lint", "-config", project(t, nil))
	if code != ExitOK || stderr != "" {
		t.Errorf("lint of a clean project = %d, %q; want 0 and no output", code, stderr)
	}

	bad := project(t, func(s string) string { return strings.Replace(s, `eq: ["account_id"]`, `eq: ["acount_id"]`, 1) })
	code, stderr = run("lint", "-config", bad)
	if code != ExitError || !strings.Contains(stderr, "DAL117") || !strings.Contains(stderr, "1 error(s)") || strings.Contains(stderr, "DAL107") {
		t.Errorf("lint with a typo = %d, %q; want 1 with a DAL117 finding", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(bad), "schema")); err == nil {
		t.Error("lint must not write anything")
	}
}

func TestGenerate(t *testing.T) {
	if _, err := exec.LookPath("sqlc"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("sqlc not on PATH in CI")
		}
		t.Skip("sqlc not on PATH; run tests through mise")
	}
	cfg := project(t, nil)
	code, stderr := run("generate", "-config", cfg)
	if code != ExitOK || !strings.Contains(stderr, "file(s) written") || !strings.Contains(stderr, "sqlc generate ok") {
		t.Fatalf("generate = %d, %q", code, stderr)
	}
	for _, rel := range []string{"schema/schema.sql", "sqlc.yaml", "gen/sqlcdb/models.go", "gen/orders/v1/ordersdal/dal.go"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(cfg), filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s not generated: %v", rel, err)
		}
	}

	code, stderr = run("generate", "-config", cfg)
	if code != ExitOK || !strings.Contains(stderr, "0 file(s) written") {
		t.Errorf("second generate = %d, %q; want nothing rewritten", code, stderr)
	}
}

func TestVerifyLock(t *testing.T) {
	cfgPath := project(t, nil)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	pinned := lock.Lock{Dalforge: "v0.3.1", Options: idl.BundledHashes(), Tools: map[string]string{}}
	if err := pipeline.WriteLock(cfg, pinned); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	newer := pinned
	newer.Dalforge = "v0.4.0"
	if code := verifyLock(cfg, newer, "generate", &stderr); code != ExitError ||
		!strings.Contains(stderr.String(), "dalforge: locked v0.3.1, running v0.4.0") || !strings.Contains(stderr.String(), "lock -upgrade") {
		t.Errorf("release mismatch = %d, %q; want an error naming the versions and the fix", code, stderr.String())
	}

	stderr.Reset()
	devel := pinned
	devel.Dalforge = lock.Devel
	if code := verifyLock(cfg, devel, "generate", &stderr); code != ExitOK || !strings.Contains(stderr.String(), "warning:") {
		t.Errorf("local build = %d, %q; want a warning and to continue", code, stderr.String())
	}
}

func TestLockLifecycle(t *testing.T) {
	if _, err := exec.LookPath("sqlc"); err != nil {
		t.Skip("sqlc not on PATH; run tests through mise")
	}
	cfg := project(t, nil)
	lockFile := filepath.Join(filepath.Dir(cfg), "dalforge.lock")

	if code, stderr := run("lock", "-config", cfg); code != ExitOK || !strings.Contains(stderr, "no dalforge.lock yet") {
		t.Errorf("lock before generate = %d, %q", code, stderr)
	}
	if code, stderr := run("generate", "-config", cfg); code != ExitOK || !strings.Contains(stderr, "created dalforge.lock") {
		t.Fatalf("first generate = %d, %q", code, stderr)
	}
	if b, err := os.ReadFile(lockFile); err != nil || !strings.Contains(string(b), `"sqlc" = "v1.`) {
		t.Errorf("lock content = %q, %v; want the sqlc version pinned", b, err)
	}
	if code, stderr := run("lock", "-config", cfg); code != ExitOK || !strings.Contains(stderr, "matches the running toolchain") {
		t.Errorf("lock after generate = %d, %q", code, stderr)
	}
	if code, stderr := run("generate", "-config", cfg); code != ExitOK || strings.Contains(stderr, "created dalforge.lock") {
		t.Errorf("second generate = %d, %q; must not recreate the lock", code, stderr)
	}
	if code, stderr := run("lock", "-upgrade", "-config", cfg); code != ExitOK || !strings.Contains(stderr, "wrote dalforge.lock") {
		t.Errorf("lock -upgrade = %d, %q", code, stderr)
	}
}
