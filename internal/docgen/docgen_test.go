package docgen

import (
	"context"
	"os"
	"strings"
	"testing"
)

const _manual = "../../docs/manual/"

// TestDocsFresh fails when a generated page or region in docs/manual is
// stale. With DALFORGE_UPDATE_GOLDEN=1 (`mise run docs`) it rewrites them.
func TestDocsFresh(t *testing.T) {
	ctx := context.Background()
	options, err := OptionReference(ctx, "../../proto")
	if err != nil {
		t.Fatal(err)
	}
	defaults, overrides, err := TypeTables(ctx)
	if err != nil {
		t.Fatal(err)
	}

	pages := map[string]func(old string) (string, error){
		"lint-rules.md": func(string) (string, error) { return RuleCatalog(), nil },
		"options.md":    func(string) (string, error) { return options, nil },
		"types.md": func(old string) (string, error) {
			doc, err := Splice(old, "type-defaults", _header+"\n\n"+defaults)
			if err != nil {
				return "", err
			}
			return Splice(doc, "type-overrides", _header+"\n\n"+overrides)
		},
	}
	update := os.Getenv("DALFORGE_UPDATE_GOLDEN") != ""
	for name, render := range pages {
		path := _manual + name
		old, err := os.ReadFile(path)
		if err != nil && (!update || !os.IsNotExist(err)) {
			t.Fatal(err)
		}
		want, err := render(string(old))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(old) == want {
			continue
		}
		if update {
			if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("updated docs/manual/%s", name)
			continue
		}
		t.Errorf("docs/manual/%s is stale; run `mise run docs` and commit the result", name)
	}
}

func TestSplice(t *testing.T) {
	doc := "a\n<!-- generated:x:begin -->\nold\n<!-- generated:x:end -->\nb\n"
	got, err := Splice(doc, "x", "new\n")
	if err != nil || got != "a\n<!-- generated:x:begin -->\nnew\n<!-- generated:x:end -->\nb\n" {
		t.Errorf("Splice = %q, %v", got, err)
	}
	if _, err := Splice(doc, "y", "z"); err == nil {
		t.Error("Splice of a missing region succeeded")
	}
	if _, err := Splice(doc+doc, "x", "z"); err == nil {
		t.Error("Splice of a duplicated region succeeded")
	}
}

// TestTypeTablesShape guards the probe itself: a broken probe would
// silently produce an empty or all-"none" table.
func TestTypeTablesShape(t *testing.T) {
	defaults, overrides, err := TypeTables(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| `text` | `string` | `*string` |",
		"| `string` + `FORMAT_UUID` | `uuid` | `uuid.UUID` | `*uuid.UUID` |",
		"`uint64`, `fixed64` | none: needs a `custom_type`",
		"| `google.protobuf.Timestamp` | `timestamptz` | `time.Time` | `*time.Time` |",
	} {
		if !strings.Contains(defaults, want) {
			t.Errorf("default table lacks %q:\n%s", want, defaults)
		}
	}
	if !strings.Contains(overrides, "| `Timestamp` | `TYPE_TIMESTAMPTZ`, `TYPE_DATE` |") {
		t.Errorf("override table:\n%s", overrides)
	}
}
