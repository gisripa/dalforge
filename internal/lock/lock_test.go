package lock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gisripa/dalforge/internal/golden"
)

var _sample = Lock{
	Dalforge: "v0.3.1",
	Options:  map[string]string{"dal/v1/options.proto": "sha256:aa", "dal/pg/v1/options.proto": "sha256:bb"},
	Tools:    map[string]string{"sqlc": "v1.31.1"},
}

func TestMarshalGolden(t *testing.T) {
	golden.Assert(t, "dalforge.lock", _sample.Marshal())
}

func TestRoundTrip(t *testing.T) {
	got, err := Parse(_sample.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(_sample, got); len(d) > 0 || got.Dalforge != _sample.Dalforge {
		t.Errorf("round trip changed the lock: %v", d)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct{ name, src, want string }{
		{name: "future version", src: "lock_version = 2\n", want: "lock_version 2 is not supported"},
		{name: "unknown key", src: "lock_version = 1\nfoo = \"x\"\n", want: `unknown key "foo"`},
		{name: "unknown section", src: "lock_version = 1\n[extras]\n", want: "unknown section [extras]"},
		{name: "unquoted value", src: "lock_version = 1\ndalforge = v1\n", want: "quoted string"},
		{name: "no equals", src: "lock_version = 1\ndalforge\n", want: "key = value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.src)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDiff(t *testing.T) {
	current := Lock{
		Dalforge: "v0.4.0",
		Options:  map[string]string{"dal/v1/options.proto": "sha256:aa", "dal/pg/v1/options.proto": "sha256:cc"},
		Tools:    map[string]string{"sqlc": "v1.32.0"},
	}
	got := strings.Join(Diff(_sample, current), "\n")
	want := "dalforge: locked v0.3.1, running v0.4.0\n" +
		"options dal/pg/v1/options.proto: locked sha256:bb, running sha256:cc\n" +
		"sqlc: locked v1.31.1, running v1.32.0"
	if got != want {
		t.Errorf("Diff =\n%s\nwant\n%s", got, want)
	}

	// Tools current didn't consult (lint doesn't run sqlc) aren't compared.
	noTools := Lock{Dalforge: _sample.Dalforge, Options: _sample.Options}
	if d := Diff(_sample, noTools); len(d) != 0 {
		t.Errorf("Diff without tools = %v, want none", d)
	}
}

func TestRead(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := Read(filepath.Join(dir, File)); ok || err != nil {
		t.Errorf("missing lock: ok=%v err=%v, want not ok and no error", ok, err)
	}
	p := filepath.Join(dir, File)
	if err := os.WriteFile(p, _sample.Marshal(), 0o644); err != nil {
		t.Fatal(err)
	}
	l, ok, err := Read(p)
	if !ok || err != nil || l.Tools["sqlc"] != "v1.31.1" {
		t.Errorf("Read = %+v, %v, %v", l, ok, err)
	}
}
