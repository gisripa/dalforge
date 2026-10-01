package diag

import (
	"testing"

	"github.com/gisripa/dalforge/internal/ir"
)

func TestListSortAndString(t *testing.T) {
	var l List
	l.Add("DAL117", Error, ir.Pos{File: "b.proto", Line: 1, Col: 1}, "second file")
	l.Add("DAL110", Warning, ir.Pos{File: "a.proto", Line: 9, Col: 1}, "later line")
	l.Add("DAL117", Error, ir.Pos{File: "a.proto", Line: 2, Col: 5}, "same pos, rule %s", "B")
	l.Add("DAL107", Error, ir.Pos{File: "a.proto", Line: 2, Col: 5}, "same pos, rule A")
	l.Sort()

	want := `a.proto:2:5: error DAL107: same pos, rule A
a.proto:2:5: error DAL117: same pos, rule B
a.proto:9:1: warning DAL110: later line
b.proto:1:1: error DAL117: second file
`
	if got := l.String(); got != want {
		t.Errorf("String() =\n%s\nwant\n%s", got, want)
	}
}

func TestHasErrors(t *testing.T) {
	tests := []struct {
		name string
		sevs []Severity
		want bool
	}{
		{name: "empty", want: false},
		{name: "warnings and info only", sevs: []Severity{Warning, Info}, want: false},
		{name: "one error", sevs: []Severity{Info, Error}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var l List
			for _, s := range tt.sevs {
				l.Add("DAL000", s, ir.Pos{}, "x")
			}
			if got := l.HasErrors(); got != tt.want {
				t.Errorf("HasErrors() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSeverityString(t *testing.T) {
	for sev, want := range map[Severity]string{Error: "error", Warning: "warning", Info: "info", Severity(7): "severity(7)"} {
		if got := sev.String(); got != want {
			t.Errorf("Severity(%d).String() = %q, want %q", int(sev), got, want)
		}
	}
}
