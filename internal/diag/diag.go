// Package diag holds lint and validation findings: a rule ID, a severity, a
// source position and a message. Every checker reports through it, so the CLI
// can sort, filter and print findings uniformly.
package diag

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/gisripa/dalforge/internal/ir"
)

// Severity ranks a finding. Errors block generation; warnings and info don't.
type Severity int

// Severities, most severe first.
const (
	Error Severity = iota
	Warning
	Info
)

var _severityNames = []string{"error", "warning", "info"}

func (s Severity) String() string {
	if s < 0 || int(s) >= len(_severityNames) {
		return fmt.Sprintf("severity(%d)", int(s))
	}
	return _severityNames[s]
}

// Diagnostic is one finding.
type Diagnostic struct {
	Rule     string // e.g. "DAL107"
	Severity Severity
	Pos      ir.Pos
	Message  string
}

// String formats the diagnostic as "file:line:col: severity DAL107: message".
func (d Diagnostic) String() string {
	return fmt.Sprintf("%s: %s %s: %s", d.Pos, d.Severity, d.Rule, d.Message)
}

// List is a set of findings.
type List []Diagnostic

// Add appends a finding.
func (l *List) Add(rule string, sev Severity, pos ir.Pos, format string, args ...any) {
	*l = append(*l, Diagnostic{Rule: rule, Severity: sev, Pos: pos, Message: fmt.Sprintf(format, args...)})
}

// HasErrors reports whether any finding is an error.
func (l List) HasErrors() bool {
	return slices.ContainsFunc(l, func(d Diagnostic) bool { return d.Severity == Error })
}

// Sort orders findings by position, then rule, then message, so output is
// stable regardless of the order checks ran in.
func (l List) Sort() {
	slices.SortStableFunc(l, func(a, b Diagnostic) int {
		return cmp.Or(
			cmp.Compare(a.Pos.File, b.Pos.File),
			cmp.Compare(a.Pos.Line, b.Pos.Line),
			cmp.Compare(a.Pos.Col, b.Pos.Col),
			cmp.Compare(a.Rule, b.Rule),
			cmp.Compare(a.Message, b.Message),
		)
	})
}

// String formats the findings one per line.
func (l List) String() string {
	var b strings.Builder
	for _, d := range l {
		b.WriteString(d.String())
		b.WriteByte('\n')
	}
	return b.String()
}
