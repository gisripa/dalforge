// Package lock reads and writes dalforge.lock, which pins the toolchain that
// produced a project's generated code: the dalforge version, hashes of the
// bundled options, and the sqlc version (design §2). It is committed, like a
// go.sum, so a teammate or CI with a different toolchain notices instead of
// silently regenerating different code.
//
// The file is a small, fixed subset of TOML written and read here, so dalforge
// takes no TOML dependency.
package lock

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// File is the lock file name, next to dalforge.yaml.
const File = "dalforge.lock"

// Devel is the version a local (non-released) dalforge build reports.
const Devel = "(devel)"

// Lock is the content of dalforge.lock.
type Lock struct {
	Dalforge string            // e.g. "v0.3.1", or Devel
	Options  map[string]string // import path → "sha256:…"
	Tools    map[string]string // e.g. "sqlc" → "v1.31.1"
}

const _header = "# dalforge.lock: maintained by dalforge, do not edit. Commit it.\n# Update it with `dalforge lock -upgrade` after changing dalforge or sqlc.\n"

// Marshal renders the lock deterministically.
func (l Lock) Marshal() []byte {
	var b bytes.Buffer
	b.WriteString(_header)
	b.WriteString("lock_version = 1\n")
	fmt.Fprintf(&b, "dalforge = %s\n", strconv.Quote(l.Dalforge))
	section := func(name string, m map[string]string) {
		fmt.Fprintf(&b, "\n[%s]\n", name)
		for _, k := range sortedKeys(m) {
			fmt.Fprintf(&b, "%s = %s\n", strconv.Quote(k), strconv.Quote(m[k]))
		}
	}
	section("options", l.Options)
	section("tools", l.Tools)
	return b.Bytes()
}

// Parse reads a lock written by Marshal.
func Parse(data []byte) (Lock, error) {
	l := Lock{Options: map[string]string{}, Tools: map[string]string{}}
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = line[1 : len(line)-1]
			if section != "options" && section != "tools" {
				return Lock{}, fmt.Errorf("line %d: unknown section [%s]", n, section)
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return Lock{}, fmt.Errorf("line %d: want key = value", n)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if unq, err := strconv.Unquote(key); err == nil {
			key = unq
		}
		val, err := strconv.Unquote(value)
		if err != nil && (section != "" || key != "lock_version") { // lock_version is a bare number
			return Lock{}, fmt.Errorf("line %d: value must be a quoted string", n)
		}
		switch section {
		case "":
			switch key {
			case "lock_version":
				if value != "1" {
					return Lock{}, fmt.Errorf("line %d: lock_version %s is not supported (want 1); upgrade dalforge", n, value)
				}
			case "dalforge":
				l.Dalforge = val
			default:
				return Lock{}, fmt.Errorf("line %d: unknown key %q", n, key)
			}
		case "options":
			l.Options[key] = val
		case "tools":
			l.Tools[key] = val
		}
	}
	return l, sc.Err()
}

// Read loads a lock file. ok is false when it doesn't exist.
func Read(path string) (l Lock, ok bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Lock{}, false, nil
	}
	if err != nil {
		return Lock{}, false, err
	}
	l, err = Parse(data)
	if err != nil {
		return Lock{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return l, true, nil
}

// Diff lists how current differs from the pinned lock, one line per
// difference. Tools missing from current (e.g. sqlc not consulted by lint)
// are not compared.
func Diff(pinned, current Lock) []string {
	var out []string
	if pinned.Dalforge != current.Dalforge {
		out = append(out, fmt.Sprintf("dalforge: locked %s, running %s", pinned.Dalforge, current.Dalforge))
	}
	for _, k := range union(pinned.Options, current.Options) {
		if pinned.Options[k] != current.Options[k] {
			out = append(out, fmt.Sprintf("options %s: locked %s, running %s", k, orNone(pinned.Options[k]), orNone(current.Options[k])))
		}
	}
	for _, k := range sortedKeys(current.Tools) {
		if pinned.Tools[k] != current.Tools[k] {
			out = append(out, fmt.Sprintf("%s: locked %s, running %s", k, orNone(pinned.Tools[k]), current.Tools[k]))
		}
	}
	return out
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func union(a, b map[string]string) []string {
	keys := sortedKeys(a)
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
