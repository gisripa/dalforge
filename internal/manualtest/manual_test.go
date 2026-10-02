// Package manualtest checks the user manual (docs/manual) as a whole: every
// relative link and anchor resolves. The generated parts (rule catalog,
// option reference, type tables) are kept fresh by internal/docgen, and the
// rule catalog is checked against the code by internal/rules.
package manualtest

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const _manual = "../../docs/manual"

var (
	_mdLink     = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	_heading    = regexp.MustCompile(`(?m)^#{1,6} (.+)$`)
	_slugStrip  = regexp.MustCompile(`[^a-z0-9 _-]`)
	_codeFences = regexp.MustCompile("(?s)```.*?```")
)

// TestLinks: every relative link in the manual points at an existing file,
// and every #anchor at an existing heading.
func TestLinks(t *testing.T) {
	pages, err := filepath.Glob(_manual + "/*.md")
	if err != nil || len(pages) == 0 {
		t.Fatalf("no manual pages: %v", err)
	}
	links := 0
	for _, page := range pages {
		text := _codeFences.ReplaceAllString(read(t, page), "")
		for _, m := range _mdLink.FindAllStringSubmatch(text, -1) {
			link := m[1]
			if strings.Contains(link, "://") || strings.HasPrefix(link, "mailto:") {
				continue
			}
			links++
			target, anchor, _ := strings.Cut(link, "#")
			file := page
			if target != "" {
				file = filepath.Join(filepath.Dir(page), target)
			}
			info, err := os.Stat(file)
			if err != nil {
				t.Errorf("%s: link %q: %v", filepath.Base(page), link, err)
				continue
			}
			if anchor == "" || info.IsDir() || !strings.HasSuffix(file, ".md") {
				continue
			}
			if !slices.Contains(anchors(read(t, file)), anchor) {
				t.Errorf("%s: link %q: no heading with anchor #%s in %s", filepath.Base(page), link, anchor, filepath.Base(file))
			}
		}
	}
	if links < 50 {
		t.Errorf("found only %d relative links; is the link pattern broken?", links)
	}
}

// anchors returns GitHub's anchor for each heading of a Markdown document.
func anchors(doc string) []string {
	doc = _codeFences.ReplaceAllString(doc, "")
	var out []string
	for _, m := range _heading.FindAllStringSubmatch(doc, -1) {
		h := strings.ToLower(strings.ReplaceAll(m[1], "`", ""))
		h = _slugStrip.ReplaceAllString(h, "")
		out = append(out, strings.ReplaceAll(h, " ", "-"))
	}
	return out
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
