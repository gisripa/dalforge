// Package golden compares test output with checked-in files under testdata/.
//
// Set DALFORGE_UPDATE_GOLDEN=1 (or run `mise run test:update`) to rewrite the
// files from the current output instead of comparing, then review the diff.
// An environment variable is used rather than a test flag because a flag
// would break `go test ./...` in packages that don't import this one.
package golden

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// EnvUpdate names the environment variable that switches to update mode.
const EnvUpdate = "DALFORGE_UPDATE_GOLDEN"

const (
	_root   = "testdata"
	_suffix = ".golden"
	_hint   = "run `mise run test:update` to accept the new output"
)

// Assert compares got with testdata/<name>.golden. name may contain
// slash-separated subdirectories. In update mode it rewrites the file instead.
func Assert(t testing.TB, name string, got []byte) {
	t.Helper()
	update, err := updating()
	if err != nil {
		t.Errorf("golden: %v", err)
		return
	}
	assertFile(t, _root, name, got, update)
}

// AssertDir compares a set of generated files, keyed by slash-separated
// relative path, with the files under testdata/<dir>. Files present on disk
// but missing from got are reported as stale. In update mode it rewrites the
// directory to match got exactly, deleting stale files.
func AssertDir(t testing.TB, dir string, got map[string][]byte) {
	t.Helper()
	update, err := updating()
	if err != nil {
		t.Errorf("golden: %v", err)
		return
	}
	assertDir(t, filepath.Join(_root, dir), got, update)
}

func updating() (bool, error) {
	if os.Getenv(EnvUpdate) == "" {
		return false, nil
	}
	if os.Getenv("CI") != "" {
		return false, fmt.Errorf("%s is set in CI; golden files must only be updated locally", EnvUpdate)
	}
	return true, nil
}

func assertFile(t testing.TB, root, name string, got []byte, update bool) {
	t.Helper()
	rel, err := cleanRel(name)
	if err != nil {
		t.Errorf("golden: %v", err)
		return
	}
	path := filepath.Join(root, rel+_suffix)

	if update {
		if err := write(path, got); err != nil {
			t.Errorf("golden: %v", err)
			return
		}
		t.Logf("golden: updated %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("golden: %s does not exist; %s", path, _hint)
		return
	}
	if err != nil {
		t.Errorf("golden: %v", err)
		return
	}
	if diff := lineDiff(want, got); diff != "" {
		t.Errorf("golden: %s mismatch (-want +got):\n%s\n%s", path, diff, _hint)
	}
}

func assertDir(t testing.TB, root string, got map[string][]byte, update bool) {
	t.Helper()
	onDisk, err := list(root)
	if err != nil {
		t.Errorf("golden: %v", err)
		return
	}

	wantNames := make(map[string]bool, len(got))
	for name := range got {
		rel, err := cleanRel(name)
		if err != nil {
			t.Errorf("golden: %v", err)
			return
		}
		wantNames[rel] = true
	}

	for _, rel := range onDisk {
		if wantNames[rel] {
			continue
		}
		path := filepath.Join(root, rel+_suffix)
		if update {
			if err := os.Remove(path); err != nil {
				t.Errorf("golden: %v", err)
				continue
			}
			t.Logf("golden: removed stale %s", path)
			continue
		}
		t.Errorf("golden: stale file %s is no longer generated; %s", path, _hint)
	}

	for _, name := range slices.Sorted(maps.Keys(got)) {
		assertFile(t, root, name, got[name], update)
	}
}

// cleanRel validates a golden name and returns it as a clean, OS-specific
// relative path. Names must stay inside the golden root.
func cleanRel(name string) (string, error) {
	if name == "" {
		return "", errors.New("empty golden name")
	}
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
		return "", fmt.Errorf("golden name %q must be relative", name)
	}
	rel := filepath.Clean(filepath.FromSlash(name))
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("golden name %q escapes testdata", name)
	}
	return rel, nil
}

// list returns the golden files under root as relative paths without the
// .golden suffix. A missing root is an empty set.
func list(root string) ([]string, error) {
	var names []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && path == root {
			return fs.SkipAll
		}
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, _suffix) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		names = append(names, strings.TrimSuffix(rel, _suffix))
		return nil
	})
	return names, err
}

func write(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// _context is the number of unchanged lines shown around a change.
const _context = 3

// lineDiff returns a readable diff of the changed region between want and
// got, or "" when they are equal. It trims the common leading and trailing
// lines and prints the rest as one -/+ block with surrounding context. Golden
// diffs are usually localised, so this stays linear on large generated files.
func lineDiff(want, got []byte) string {
	if string(want) == string(got) {
		return ""
	}
	w := strings.SplitAfter(string(want), "\n")
	g := strings.SplitAfter(string(got), "\n")

	prefix := 0
	for prefix < len(w) && prefix < len(g) && w[prefix] == g[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(w)-prefix && suffix < len(g)-prefix && w[len(w)-1-suffix] == g[len(g)-1-suffix] {
		suffix++
	}

	var b strings.Builder
	start := max(0, prefix-_context)
	fmt.Fprintf(&b, "@@ line %d @@\n", start+1)
	for _, l := range w[start:prefix] {
		writeLine(&b, "  ", l)
	}
	for _, l := range w[prefix : len(w)-suffix] {
		writeLine(&b, "- ", l)
	}
	for _, l := range g[prefix : len(g)-suffix] {
		writeLine(&b, "+ ", l)
	}
	for _, l := range w[len(w)-suffix : min(len(w), len(w)-suffix+_context)] {
		writeLine(&b, "  ", l)
	}
	return b.String()
}

// writeLine writes one diff line, marking a missing final newline so that
// whitespace-only differences stay visible.
func writeLine(b *strings.Builder, mark, line string) {
	if line == "" {
		return // the empty element after a trailing newline
	}
	b.WriteString(mark)
	if strings.HasSuffix(line, "\n") {
		b.WriteString(line)
		return
	}
	b.WriteString(line)
	b.WriteString(" ⏎ (no newline at end)\n")
}
