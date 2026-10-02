package rules

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	// RuleShape = "DAL107", or const RuleQueryName = "DAL205".
	_constDecl = regexp.MustCompile(`\b(Rule\w+|_rule\w+)\s*=\s*"(DAL\d{3})"`)
	// diags.Add(RuleShape, diag.Error, … or out.Add(RuleVendored, diag.Warning, …
	_addCall = regexp.MustCompile(`\.Add\(\s*(?:\w+\.)?(Rule\w+),\s*diag\.(Error|Warning|Info)\b`)
)

// _internal are rule IDs for generator bugs, not user mistakes.
var _internal = map[string]bool{"DAL000": true}

// TestCatalogMatchesCode: every rule the code defines has a catalog entry,
// every enforced entry is defined by the code, and every severity the code
// reports a rule with is the one the catalog documents.
func TestCatalogMatchesCode(t *testing.T) {
	ids := map[string]string{}          // constant name → rule ID
	severities := map[string][]string{} // constant name → severities used
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range _constDecl.FindAllStringSubmatch(string(src), -1) {
			ids[m[1]] = m[2]
		}
		for _, m := range _addCall.FindAllStringSubmatch(string(src), -1) {
			severities[m[1]] = append(severities[m[1]], strings.ToLower(m[2]))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	catalog := map[string]Rule{}
	for _, r := range All() {
		catalog[r.ID] = r
	}
	inCode := map[string]bool{}
	for name, id := range ids {
		if _internal[id] {
			continue
		}
		inCode[id] = true
		r, ok := catalog[id]
		switch {
		case !ok:
			t.Errorf("rule %s (%s) has no catalog entry: add internal/rules/catalog/%s.md", id, name, id)
		case r.Planned:
			t.Errorf("rule %s is implemented (%s) but its catalog entry says status: planned", id, name)
		}
		for _, sev := range severities[name] {
			if ok && sev != r.Severity {
				t.Errorf("rule %s is reported as %s, but the catalog says %s", id, sev, r.Severity)
			}
		}
	}
	for id, r := range catalog {
		if !r.Planned && !inCode[id] {
			t.Errorf("catalog entry %s isn't defined in code; mark it status: planned or remove it", id)
		}
	}
	if len(inCode) < 20 {
		t.Errorf("found only %d rule constants; did the declaration style change?", len(inCode))
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct{ name, doc, want string }{
		{"no header", "hello", "missing header"},
		{"unterminated", "---\nseverity: error\n", "unterminated"},
		{"bad severity", "---\nseverity: fatal\nscope: core\ntitle: t\n---\n", "severity"},
		{"bad scope", "---\nseverity: error\nscope: mysql\ntitle: t\n---\n", "scope"},
		{"unknown key", "---\nseverity: error\nscope: core\ntitle: t\nowner: me\n---\n", "unknown header key"},
		{"no title", "---\nseverity: error\nscope: core\n---\n", "no title"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parse("DAL999", tt.doc); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("parse() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLookup(t *testing.T) {
	r, ok := Lookup("DAL115")
	if !ok || r.Severity != "error" || r.Scope != "core" || r.Body == "" {
		t.Errorf("Lookup(DAL115) = %+v, %v", r, ok)
	}
	if _, ok := Lookup("DAL999"); ok {
		t.Error("Lookup(DAL999) found a rule")
	}
}
