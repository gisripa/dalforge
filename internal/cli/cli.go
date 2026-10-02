// Package cli implements the dalforge command-line interface.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/gisripa/dalforge/internal/config"
	"github.com/gisripa/dalforge/internal/diag"
	"github.com/gisripa/dalforge/internal/lock"
	"github.com/gisripa/dalforge/internal/pipeline"
)

// Exit codes returned by Run.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// ErrNotImplemented is returned by subcommands that are declared in the CLI but
// have no implementation in this build.
var ErrNotImplemented = errors.New("not implemented")

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string, stderr io.Writer) int
}

var _commands = []command{
	{name: "generate", summary: "generate schema, sqlc queries and the Go DAL from proto IDL", run: generate},
	{name: "lint", summary: "check access patterns, indexes and migrations for unsafe shapes", run: lint},
	{name: "lock", summary: "show or update dalforge.lock, the pinned dalforge/options/sqlc versions", run: lockCmd},
	{name: "migrate", summary: "diff the IDL against the schema snapshot and emit migrations", run: notImplemented},
	{name: "version", summary: "print the dalforge version", run: version},
}

func version(_ context.Context, args []string, stderr io.Writer) int {
	if len(args) > 0 {
		fprintf(stderr, "dalforge version: unexpected arguments %q\n", args)
		return ExitUsage
	}
	fprintf(stderr, "dalforge %s\n", pipeline.Version())
	return ExitOK
}

// Run executes dalforge with the given arguments (excluding the program name),
// writing diagnostics to stderr, and returns the process exit code.
func Run(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("dalforge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stderr) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}

	if fs.NArg() == 0 {
		usage(stderr)
		return ExitUsage
	}

	name := fs.Arg(0)
	for _, c := range _commands {
		if c.name == name {
			return c.run(ctx, fs.Args()[1:], stderr)
		}
	}

	fprintf(stderr, "dalforge: unknown command %q\n\n", name)
	usage(stderr)
	return ExitUsage
}

func usage(w io.Writer) {
	fprintf(w, "Usage: dalforge <command> [flags]\n\nCommands:\n")
	for _, c := range _commands {
		fprintf(w, "  %-10s %s\n", c.name, c.summary)
	}
	fprintf(w, "\nRun dalforge <command> -h for the command's flags.\n")
}

// fprintf writes diagnostics on a best-effort basis; there is nowhere left to
// report a failed write to stderr.
func fprintf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

func notImplemented(_ context.Context, _ []string, stderr io.Writer) int {
	fprintf(stderr, "dalforge migrate: %v\n", ErrNotImplemented)
	return ExitError
}

// plan parses the command's flags, loads the config and runs the pipeline,
// printing findings. ok is false when the caller should exit with code.
func plan(ctx context.Context, name string, args []string, stderr io.Writer) (cfg *config.Config, p *pipeline.Plan, code int, ok bool) {
	fs := flag.NewFlagSet("dalforge "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", config.DefaultFile, "path to the project config; paths in it are relative to its directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, nil, ExitOK, false
		}
		return nil, nil, ExitUsage, false
	}
	if fs.NArg() > 0 {
		fprintf(stderr, "dalforge %s: unexpected arguments %q\n", name, fs.Args())
		return nil, nil, ExitUsage, false
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fprintf(stderr, "dalforge %s: %v\n", name, err)
		return nil, nil, ExitError, false
	}
	p, err = pipeline.Build(ctx, cfg)
	if err != nil {
		fprintf(stderr, "dalforge %s: %v\n", name, err)
		return nil, nil, ExitError, false
	}
	report(stderr, p.Diags)
	if p.Diags.HasErrors() {
		return nil, nil, ExitError, false
	}
	return cfg, p, ExitOK, true
}

func report(w io.Writer, diags diag.List) {
	if len(diags) == 0 {
		return
	}
	fprintf(w, "%s", diags)
	counts := map[diag.Severity]int{}
	for _, d := range diags {
		counts[d.Severity]++
	}
	fprintf(w, "%d error(s), %d warning(s), %d info\n", counts[diag.Error], counts[diag.Warning], counts[diag.Info])
}

func lint(ctx context.Context, args []string, stderr io.Writer) int {
	cfg, _, code, ok := plan(ctx, "lint", args, stderr)
	if !ok {
		return code
	}
	current, err := pipeline.CurrentLock(ctx, "") // lint doesn't need sqlc
	if err != nil {
		fprintf(stderr, "dalforge lint: %v\n", err)
		return ExitError
	}
	return verifyLock(cfg, current, "lint", stderr)
}

// verifyLock reports a toolchain that differs from dalforge.lock: an error,
// or a warning for a local dalforge build. A missing lock is fine.
func verifyLock(cfg *config.Config, current lock.Lock, name string, stderr io.Writer) int {
	st, err := pipeline.CheckLock(cfg, current)
	if err != nil {
		fprintf(stderr, "dalforge %s: %v\n", name, err)
		return ExitError
	}
	if len(st.Diffs) == 0 {
		return ExitOK
	}
	level := "error"
	if st.Lenient {
		level = "warning"
	}
	fprintf(stderr, "%s: %s differs from the running toolchain:\n", level, lock.File)
	for _, d := range st.Diffs {
		fprintf(stderr, "  %s\n", d)
	}
	if st.Lenient {
		fprintf(stderr, "  (local dalforge build: continuing)\n")
		return ExitOK
	}
	fprintf(stderr, "Run `dalforge lock -upgrade` to accept the new toolchain, then review and commit %s.\n", lock.File)
	return ExitError
}

func generate(ctx context.Context, args []string, stderr io.Writer) int {
	cfg, p, code, ok := plan(ctx, "generate", args, stderr)
	if !ok {
		return code
	}
	sqlc, err := pipeline.FindSQLC()
	if err != nil {
		fprintf(stderr, "dalforge generate: %v\n", err)
		return ExitError
	}
	current, err := pipeline.CurrentLock(ctx, sqlc)
	if err != nil {
		fprintf(stderr, "dalforge generate: %v\n", err)
		return ExitError
	}
	if code := verifyLock(cfg, current, "generate", stderr); code != ExitOK {
		return code
	}
	sum, err := pipeline.Generate(ctx, cfg, p, sqlc)
	if err != nil {
		fprintf(stderr, "dalforge generate: %v\n", err)
		return ExitError
	}
	if st, err := pipeline.CheckLock(cfg, current); err == nil && !st.Exists {
		if err := pipeline.WriteLock(cfg, current); err != nil {
			fprintf(stderr, "dalforge generate: %v\n", err)
			return ExitError
		}
		fprintf(stderr, "dalforge: created %s (commit it)\n", lock.File)
	}
	fprintf(stderr, "dalforge: %d file(s) written, %d unchanged, %d stale removed", sum.Written, sum.Unchanged, sum.Removed)
	if sum.RanSQLC {
		fprintf(stderr, "; sqlc generate ok\n")
	} else {
		fprintf(stderr, "; %s\n", sum.SQLCNote)
	}
	return ExitOK
}

// lockCmd shows whether dalforge.lock matches the running toolchain, or with
// -upgrade rewrites it to match.
func lockCmd(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("dalforge lock", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", config.DefaultFile, "path to the project config")
	upgrade := fs.Bool("upgrade", false, "rewrite dalforge.lock to the running dalforge, options and sqlc")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fprintf(stderr, "dalforge lock: %v\n", err)
		return ExitError
	}
	sqlc, _ := pipeline.FindSQLC() // optional here: lock what is available
	current, err := pipeline.CurrentLock(ctx, sqlc)
	if err != nil {
		fprintf(stderr, "dalforge lock: %v\n", err)
		return ExitError
	}
	if *upgrade {
		if err := pipeline.WriteLock(cfg, current); err != nil {
			fprintf(stderr, "dalforge lock: %v\n", err)
			return ExitError
		}
		fprintf(stderr, "dalforge: wrote %s (dalforge %s, sqlc %s); review and commit it\n", lock.File, current.Dalforge, current.Tools["sqlc"])
		return ExitOK
	}
	st, err := pipeline.CheckLock(cfg, current)
	switch {
	case err != nil:
		fprintf(stderr, "dalforge lock: %v\n", err)
		return ExitError
	case !st.Exists:
		fprintf(stderr, "dalforge: no %s yet; `dalforge generate` or `dalforge lock -upgrade` creates it\n", lock.File)
		return ExitOK
	case len(st.Diffs) == 0:
		fprintf(stderr, "dalforge: %s matches the running toolchain\n", lock.File)
		return ExitOK
	}
	for _, d := range st.Diffs {
		fprintf(stderr, "  %s\n", d)
	}
	return ExitError
}
