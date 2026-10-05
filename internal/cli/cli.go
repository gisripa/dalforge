// Package cli implements the dalforge command-line interface.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

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

// streams are where a command writes: results that tools read (JSON reports,
// the version) go to out; findings, progress and errors for people go to err.
type streams struct {
	out, err io.Writer
}

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string, s streams) int
}

var _commands = []command{
	{name: "generate", summary: "generate schema, sqlc queries and the Go DAL from proto IDL", run: generate},
	{name: "lint", summary: "check access patterns, indexes and migrations for unsafe shapes", run: lint},
	{name: "lock", summary: "show or update dalforge.lock, the pinned dalforge/options/sqlc versions", run: lockCmd},
	{name: "migrate", summary: "diff the IDL against the schema snapshot and emit migrations", run: notImplemented},
	{name: "version", summary: "print the dalforge version", run: version},
}

// Run executes dalforge with the given arguments (excluding the program name)
// and returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	s := streams{out: stdout, err: stderr}
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
			return c.run(ctx, fs.Args()[1:], s)
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

// fprintf writes on a best-effort basis; there is nowhere left to report a
// failed write to stdout or stderr.
func fprintf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

func version(_ context.Context, args []string, s streams) int {
	if len(args) > 0 {
		fprintf(s.err, "dalforge version: unexpected arguments %q\n", args)
		return ExitUsage
	}
	fprintf(s.out, "dalforge %s\n", pipeline.Version())
	return ExitOK
}

func notImplemented(_ context.Context, _ []string, s streams) int {
	fprintf(s.err, "dalforge migrate: %v\n", ErrNotImplemented)
	return ExitError
}

// Report is what `dalforge lint -json` and `dalforge generate -json` print on
// stdout: one JSON object, whatever the outcome. Its field names are a stable
// interface for tools such as editors, CI annotations and the playground.
type Report struct {
	// Diagnostics are the lint findings, plus problems that stop the IDL from
	// loading at all (those have no rule).
	Diagnostics []Finding `json:"diagnostics"`
	// Generated describes what generate wrote; it's absent for lint and when
	// generate stopped early.
	Generated *Generated `json:"generated,omitempty"`
	// Error is a failure that isn't about the IDL: the config, the lock, or
	// sqlc. The exit code is 1 whenever Error is set or a finding is an error.
	Error string `json:"error,omitempty"`
}

// Finding is one diagnostic. File is relative to its proto root, as in the
// text output.
type Finding struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Col      int    `json:"col"`
	Severity string `json:"severity"` // error, warning or info
	Rule     string `json:"rule,omitempty"`
	Message  string `json:"message"`
}

// Generated summarises a successful generate.
type Generated struct {
	Written     int    `json:"written"`
	Unchanged   int    `json:"unchanged"`
	Removed     int    `json:"removed"`
	RanSQLC     bool   `json:"ran_sqlc"`
	SQLCNote    string `json:"sqlc_note,omitempty"` // why sqlc didn't run
	LockCreated bool   `json:"lock_created,omitempty"`
}

// session is one lint or generate run. It prints for people on stderr, or,
// with -json, collects a Report and prints it on stdout when the run ends.
type session struct {
	name string
	s    streams
	json bool
	rep  Report
}

// finish ends the run with code, printing the JSON report if there is one.
func (r *session) finish(code int) int {
	if r.json {
		enc := json.NewEncoder(r.s.out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r.rep); err != nil {
			fprintf(r.s.err, "dalforge %s: %v\n", r.name, err)
			return ExitError
		}
	}
	return code
}

// fail ends the run with a failure that isn't a finding.
func (r *session) fail(err error) int {
	if r.json {
		r.rep.Error = err.Error()
	} else {
		fprintf(r.s.err, "dalforge %s: %v\n", r.name, err)
	}
	return r.finish(ExitError)
}

// say prints a progress line for people; JSON reports leave it out.
func (r *session) say(format string, a ...any) {
	if !r.json {
		fprintf(r.s.err, format, a...)
	}
}

func (r *session) findings(diags diag.List) {
	if !r.json {
		report(r.s.err, diags)
		return
	}
	for _, d := range diags {
		r.rep.Diagnostics = append(r.rep.Diagnostics, Finding{
			File: d.Pos.File, Line: d.Pos.Line, Col: d.Pos.Col,
			Severity: d.Severity.String(), Rule: d.Rule, Message: d.Message,
		})
	}
}

// _loadError matches one structural problem the loader reports, e.g.
// "orders/v1/orders.proto:12:3: field x: …".
var _loadError = regexp.MustCompile(`^(.+\.proto):(\d+):(\d+): (.*)$`)

// loadFailed reports an IDL that couldn't be loaded. With -json, each line of
// the loader's error becomes a finding without a rule.
func (r *session) loadFailed(err error) int {
	if !r.json {
		return r.fail(err)
	}
	sc := bufio.NewScanner(strings.NewReader(err.Error()))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		f := Finding{Severity: diag.Error.String(), Message: line}
		if m := _loadError.FindStringSubmatch(line); m != nil {
			f.File, f.Message = m[1], m[4]
			f.Line, _ = strconv.Atoi(m[2])
			f.Col, _ = strconv.Atoi(m[3])
		}
		r.rep.Diagnostics = append(r.rep.Diagnostics, f)
	}
	return r.finish(ExitError)
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

// start parses a lint or generate command's flags and runs the pipeline. ok
// is false when the run is over and the caller should return code.
func start(ctx context.Context, name string, args []string, s streams) (r *session, cfg *config.Config, p *pipeline.Plan, code int, ok bool) {
	fs := flag.NewFlagSet("dalforge "+name, flag.ContinueOnError)
	fs.SetOutput(s.err)
	configPath := fs.String("config", config.DefaultFile, "path to the project config; paths in it are relative to its directory")
	asJSON := fs.Bool("json", false, "print one JSON report on stdout (findings, and for generate what was written) instead of text on stderr")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, nil, nil, ExitOK, false
		}
		return nil, nil, nil, ExitUsage, false
	}
	if fs.NArg() > 0 {
		fprintf(s.err, "dalforge %s: unexpected arguments %q\n", name, fs.Args())
		return nil, nil, nil, ExitUsage, false
	}

	r = &session{name: name, s: s, json: *asJSON, rep: Report{Diagnostics: []Finding{}}}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return nil, nil, nil, r.fail(err), false
	}
	p, err = pipeline.Build(ctx, cfg)
	if err != nil {
		return nil, nil, nil, r.loadFailed(err), false
	}
	r.findings(p.Diags)
	if p.Diags.HasErrors() {
		return nil, nil, nil, r.finish(ExitError), false
	}
	return r, cfg, p, ExitOK, true
}

func lint(ctx context.Context, args []string, s streams) int {
	r, cfg, _, code, ok := start(ctx, "lint", args, s)
	if !ok {
		return code
	}
	current, err := pipeline.CurrentLock(ctx, "") // lint doesn't need sqlc
	if err != nil {
		return r.fail(err)
	}
	if code, ok := r.verifyLock(cfg, current); !ok {
		return code
	}
	return r.finish(ExitOK)
}

// verifyLock checks the toolchain against dalforge.lock. A difference is an
// error, or only a warning for a local dalforge build; a missing lock is fine.
// ok is false when the run is over and the caller should return code.
func (r *session) verifyLock(cfg *config.Config, current lock.Lock) (code int, ok bool) {
	st, err := pipeline.CheckLock(cfg, current)
	if err != nil {
		return r.fail(err), false
	}
	if len(st.Diffs) == 0 {
		return ExitOK, true
	}
	if r.json {
		diffs := strings.Join(st.Diffs, "; ")
		if st.Lenient {
			r.rep.Diagnostics = append(r.rep.Diagnostics, Finding{
				File: lock.File, Severity: diag.Warning.String(),
				Message: fmt.Sprintf("%s differs from the running toolchain (local dalforge build: continuing): %s", lock.File, diffs),
			})
			return ExitOK, true
		}
		r.rep.Error = fmt.Sprintf("%s differs from the running toolchain: %s; run `dalforge lock -upgrade` to accept the new toolchain, then review and commit %s", lock.File, diffs, lock.File)
		return r.finish(ExitError), false
	}

	level := "error"
	if st.Lenient {
		level = "warning"
	}
	fprintf(r.s.err, "%s: %s differs from the running toolchain:\n", level, lock.File)
	for _, d := range st.Diffs {
		fprintf(r.s.err, "  %s\n", d)
	}
	if st.Lenient {
		fprintf(r.s.err, "  (local dalforge build: continuing)\n")
		return ExitOK, true
	}
	fprintf(r.s.err, "Run `dalforge lock -upgrade` to accept the new toolchain, then review and commit %s.\n", lock.File)
	return ExitError, false
}

func generate(ctx context.Context, args []string, s streams) int {
	r, cfg, p, code, ok := start(ctx, "generate", args, s)
	if !ok {
		return code
	}
	sqlc, err := pipeline.FindSQLC()
	if err != nil {
		return r.fail(err)
	}
	current, err := pipeline.CurrentLock(ctx, sqlc)
	if err != nil {
		return r.fail(err)
	}
	if code, ok := r.verifyLock(cfg, current); !ok {
		return code
	}
	sum, err := pipeline.Generate(ctx, cfg, p, sqlc)
	if err != nil {
		return r.fail(err)
	}
	gen := &Generated{Written: sum.Written, Unchanged: sum.Unchanged, Removed: sum.Removed, RanSQLC: sum.RanSQLC, SQLCNote: sum.SQLCNote}
	if st, err := pipeline.CheckLock(cfg, current); err == nil && !st.Exists {
		if err := pipeline.WriteLock(cfg, current); err != nil {
			return r.fail(err)
		}
		gen.LockCreated = true
		r.say("dalforge: created %s (commit it)\n", lock.File)
	}
	r.rep.Generated = gen
	r.say("dalforge: %d file(s) written, %d unchanged, %d stale removed", sum.Written, sum.Unchanged, sum.Removed)
	if sum.RanSQLC {
		r.say("; sqlc generate ok\n")
	} else {
		r.say("; %s\n", sum.SQLCNote)
	}
	return r.finish(ExitOK)
}

// lockCmd shows whether dalforge.lock matches the running toolchain, or with
// -upgrade rewrites it to match.
func lockCmd(ctx context.Context, args []string, s streams) int {
	fs := flag.NewFlagSet("dalforge lock", flag.ContinueOnError)
	fs.SetOutput(s.err)
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
		fprintf(s.err, "dalforge lock: %v\n", err)
		return ExitError
	}
	sqlc, _ := pipeline.FindSQLC() // optional here: lock what is available
	current, err := pipeline.CurrentLock(ctx, sqlc)
	if err != nil {
		fprintf(s.err, "dalforge lock: %v\n", err)
		return ExitError
	}
	if *upgrade {
		if err := pipeline.WriteLock(cfg, current); err != nil {
			fprintf(s.err, "dalforge lock: %v\n", err)
			return ExitError
		}
		fprintf(s.err, "dalforge: wrote %s (dalforge %s, sqlc %s); review and commit it\n", lock.File, current.Dalforge, current.Tools["sqlc"])
		return ExitOK
	}
	st, err := pipeline.CheckLock(cfg, current)
	switch {
	case err != nil:
		fprintf(s.err, "dalforge lock: %v\n", err)
		return ExitError
	case !st.Exists:
		fprintf(s.err, "dalforge: no %s yet; `dalforge generate` or `dalforge lock -upgrade` creates it\n", lock.File)
		return ExitOK
	case len(st.Diffs) == 0:
		fprintf(s.err, "dalforge: %s matches the running toolchain\n", lock.File)
		return ExitOK
	}
	for _, d := range st.Diffs {
		fprintf(s.err, "  %s\n", d)
	}
	return ExitError
}
