// Package cli implements the dalforge command-line interface.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// Exit codes returned by Run.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// ErrNotImplemented is returned by subcommands that are not built yet.
var ErrNotImplemented = errors.New("not implemented")

type command struct {
	name    string
	summary string
	run     func(args []string) error
}

var _commands = []command{
	{name: "generate", summary: "generate schema, sqlc queries and the Go DAL from proto IDL", run: notImplemented},
	{name: "lint", summary: "check access patterns, indexes and migrations for unsafe shapes", run: notImplemented},
	{name: "migrate", summary: "diff the IDL against the schema snapshot and emit migrations", run: notImplemented},
}

// Run executes dalforge with the given arguments (excluding the program name),
// writing diagnostics to stderr, and returns the process exit code.
func Run(args []string, stderr io.Writer) int {
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
		if c.name != name {
			continue
		}
		if err := c.run(fs.Args()[1:]); err != nil {
			fprintf(stderr, "dalforge %s: %v\n", name, err)
			return ExitError
		}
		return ExitOK
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
}

// fprintf writes diagnostics on a best-effort basis; there is nowhere left to
// report a failed write to stderr.
func fprintf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

func notImplemented([]string) error {
	return ErrNotImplemented
}
