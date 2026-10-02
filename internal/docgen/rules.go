package docgen

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/gisripa/dalforge/internal/rules"
)

//go:embed lint-rules.head.md
var _rulesHead string

// RuleCatalog renders docs/manual/lint-rules.md from internal/rules.
func RuleCatalog() string {
	var b strings.Builder
	b.WriteString(_header + "\n\n")
	b.WriteString(_rulesHead)
	all := rules.All()
	section := func(title, scope string) {
		fmt.Fprintf(&b, "\n## %s\n", title)
		for _, r := range all {
			if r.Scope != scope || r.Planned {
				continue
			}
			fmt.Fprintf(&b, "\n### %s\n\n**%s: %s**\n", r.ID, r.Severity, r.Title)
			if r.Body != "" {
				b.WriteString("\n" + r.Body + "\n")
			}
		}
	}
	section("Backend-neutral rules", "core")
	section("Postgres rules", "pg")

	b.WriteString("\n## Planned\n\nThese rules arrive with generated migrations (see\n[Changing the schema](schema-changes.md)):\n\n")
	b.WriteString("| Rule | Severity | Catches |\n|---|---|---|\n")
	for _, r := range all {
		if r.Planned {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", r.ID, r.Severity, cell(strings.TrimSuffix(r.Title, ".")))
		}
	}
	return b.String()
}
