package cli

import (
	"bytes"
	"context"
	"encoding/json"
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

// run runs dalforge and returns its exit code and everything it printed,
// stdout and stderr together.
func run(args ...string) (int, string) {
	var out bytes.Buffer
	code := Run(context.Background(), args, &out, &out)
	return code, out.String()
}

// runJSON runs dalforge with -json and decodes the report on stdout.
func runJSON(t *testing.T, args ...string) (int, Report, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), append(args, "-json"), &stdout, &stderr)
	var rep Report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("stdout isn't a JSON report: %v\n%s", err, stdout.String())
	}
	return code, rep, stderr.String()
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

// TestJSON pins the -json reports: one JSON object on stdout whatever the
// outcome, with nothing for people on stderr.
func TestJSON(t *testing.T) {
	code, rep, stderr := runJSON(t, "lint", "-config", project(t, nil))
	if code != ExitOK || len(rep.Diagnostics) != 0 || rep.Error != "" || rep.Generated != nil || stderr != "" {
		t.Errorf("clean lint = %d, %+v, stderr %q", code, rep, stderr)
	}

	typo := project(t, func(s string) string { return strings.Replace(s, `eq: ["account_id"]`, `eq: ["acount_id"]`, 1) })
	code, rep, _ = runJSON(t, "lint", "-config", typo)
	if code != ExitError || len(rep.Diagnostics) != 1 {
		t.Fatalf("lint with a typo = %d, %+v", code, rep)
	}
	if d := rep.Diagnostics[0]; d.Rule != "DAL117" || d.Severity != "error" || d.File != "orders/v1/orders.proto" || d.Line == 0 || d.Col == 0 {
		t.Errorf("finding = %+v; want a positioned DAL117 error in orders/v1/orders.proto", d)
	}

	broken := project(t, func(s string) string {
		return strings.Replace(s, "message Order {", "message Order {\n  string oops = 99", 1)
	})
	code, rep, _ = runJSON(t, "lint", "-config", broken)
	if code != ExitError || len(rep.Diagnostics) == 0 || rep.Diagnostics[0].Rule != "" || rep.Diagnostics[0].Line == 0 {
		t.Errorf("syntax error = %d, %+v; want positioned findings without a rule", code, rep)
	}

	code, rep, _ = runJSON(t, "lint", "-config", "/nonexistent/dalforge.yaml")
	if code != ExitError || !strings.Contains(rep.Error, "no such file") {
		t.Errorf("missing config = %d, %+v; want the failure in error", code, rep)
	}

	if _, err := exec.LookPath("sqlc"); err != nil {
		t.Skip("sqlc not on PATH; run tests through mise")
	}
	code, rep, stderr = runJSON(t, "generate", "-config", project(t, nil))
	if code != ExitOK || rep.Generated == nil || !rep.Generated.RanSQLC || !rep.Generated.LockCreated || rep.Generated.Written == 0 || stderr != "" {
		t.Errorf("generate = %d, %+v (generated %+v), stderr %q", code, rep, rep.Generated, stderr)
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
	r := &session{name: "generate", s: streams{out: &stderr, err: &stderr}}
	newer := pinned
	newer.Dalforge = "v0.4.0"
	if code, _ := r.verifyLock(cfg, newer); code != ExitError ||
		!strings.Contains(stderr.String(), "dalforge: locked v0.3.1, running v0.4.0") || !strings.Contains(stderr.String(), "lock -upgrade") {
		t.Errorf("release mismatch = %d, %q; want an error naming the versions and the fix", code, stderr.String())
	}

	stderr.Reset()
	devel := pinned
	devel.Dalforge = lock.Devel
	if code, ok := r.verifyLock(cfg, devel); code != ExitOK || !ok || !strings.Contains(stderr.String(), "warning:") {
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
