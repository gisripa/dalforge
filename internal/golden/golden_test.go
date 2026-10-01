package golden

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recorder captures failures so the failure paths can be asserted without
// failing the enclosing test. Only the methods golden calls are implemented.
type recorder struct {
	testing.TB
	errs []string
	logs []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recorder) Logf(format string, args ...any) {
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := write(path, []byte(content)); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAssertFile(t *testing.T) {
	tests := []struct {
		name     string
		existing string // golden content on disk; "" means no file
		golden   string
		got      string
		update   bool
		wantErr  string // substring of the single expected error; "" means none
	}{
		{name: "match", existing: "a\nb\n", golden: "x", got: "a\nb\n"},
		{name: "mismatch shows removed line", existing: "a\nb\n", golden: "x", got: "a\nc\n", wantErr: "- b\n"},
		{name: "mismatch shows added line", existing: "a\nb\n", golden: "x", got: "a\nc\n", wantErr: "+ c\n"},
		{name: "mismatch hints at update", existing: "a\n", golden: "x", got: "b\n", wantErr: "mise run test:update"},
		{name: "missing file", golden: "x", got: "a\n", wantErr: "does not exist"},
		{name: "absolute name", golden: "/etc/x", got: "a", wantErr: "must be relative"},
		{name: "escaping name", golden: "../x", got: "a", wantErr: "escapes testdata"},
		{name: "empty name", golden: "", got: "a", wantErr: "empty golden name"},
		{name: "update writes nested file", golden: "sub/dir/x", got: "new\n", update: true},
		{name: "update overwrites", existing: "old\n", golden: "x", got: "new\n", update: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.existing != "" {
				writeFile(t, filepath.Join(root, tt.golden+_suffix), tt.existing)
			}

			r := &recorder{}
			assertFile(r, root, tt.golden, []byte(tt.got), tt.update)

			switch {
			case tt.wantErr == "" && len(r.errs) > 0:
				t.Fatalf("unexpected errors: %q", r.errs)
			case tt.wantErr != "" && (len(r.errs) != 1 || !strings.Contains(r.errs[0], tt.wantErr)):
				t.Fatalf("errors = %q, want one containing %q", r.errs, tt.wantErr)
			}
			if tt.update {
				if got := readFile(t, filepath.Join(root, filepath.FromSlash(tt.golden)+_suffix)); got != tt.got {
					t.Errorf("file after update = %q, want %q", got, tt.got)
				}
			}
		})
	}
}

func TestAssertDir(t *testing.T) {
	tests := []struct {
		name     string
		existing map[string]string
		got      map[string]string
		update   bool
		wantErrs []string // substrings, one per expected error
		wantDisk map[string]string
	}{
		{
			name:     "match",
			existing: map[string]string{"schema.sql": "s\n", "queries/orders.sql": "q\n"},
			got:      map[string]string{"schema.sql": "s\n", "queries/orders.sql": "q\n"},
		},
		{
			name:     "stale file",
			existing: map[string]string{"schema.sql": "s\n", "old.sql": "o\n"},
			got:      map[string]string{"schema.sql": "s\n"},
			wantErrs: []string{"stale file"},
		},
		{
			name:     "missing and mismatched",
			existing: map[string]string{"a.sql": "a\n"},
			got:      map[string]string{"a.sql": "changed\n", "b.sql": "b\n"},
			wantErrs: []string{"a.sql.golden mismatch", "b.sql.golden does not exist"},
		},
		{
			name:     "missing root is empty",
			got:      map[string]string{"a.sql": "a\n"},
			wantErrs: []string{"does not exist"},
		},
		{
			name:     "update rewrites and removes stale",
			existing: map[string]string{"keep.sql": "old\n", "nested/stale.sql": "x\n"},
			got:      map[string]string{"keep.sql": "new\n", "added.sql": "a\n"},
			update:   true,
			wantDisk: map[string]string{"keep.sql": "new\n", "added.sql": "a\n"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "out")
			for name, content := range tt.existing {
				writeFile(t, filepath.Join(root, filepath.FromSlash(name)+_suffix), content)
			}
			got := make(map[string][]byte, len(tt.got))
			for name, content := range tt.got {
				got[name] = []byte(content)
			}

			r := &recorder{}
			assertDir(r, root, got, tt.update)

			if len(r.errs) != len(tt.wantErrs) {
				t.Fatalf("errors = %q, want %d matching %q", r.errs, len(tt.wantErrs), tt.wantErrs)
			}
			for i, want := range tt.wantErrs {
				if !strings.Contains(r.errs[i], want) {
					t.Errorf("error[%d] = %q, want it to contain %q", i, r.errs[i], want)
				}
			}
			if tt.wantDisk == nil {
				return
			}
			onDisk, err := list(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(onDisk) != len(tt.wantDisk) {
				t.Fatalf("files on disk = %q, want %d", onDisk, len(tt.wantDisk))
			}
			for name, want := range tt.wantDisk {
				if got := readFile(t, filepath.Join(root, filepath.FromSlash(name)+_suffix)); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
		})
	}
}

func TestLineDiff(t *testing.T) {
	tests := []struct {
		name      string
		want, got string
		diff      string
	}{
		{name: "equal", want: "a\nb\n", got: "a\nb\n", diff: ""},
		{
			name: "middle change with context",
			want: "1\n2\n3\n4\nold\n6\n7\n8\n9\n",
			got:  "1\n2\n3\n4\nnew\n6\n7\n8\n9\n",
			diff: "@@ line 2 @@\n  2\n  3\n  4\n- old\n+ new\n  6\n  7\n  8\n",
		},
		{
			name: "added lines at end",
			want: "a\n",
			got:  "a\nb\nc\n",
			diff: "@@ line 1 @@\n  a\n+ b\n+ c\n",
		},
		{
			name: "missing trailing newline is visible",
			want: "a\n",
			got:  "a",
			diff: "@@ line 1 @@\n- a\n+ a ⏎ (no newline at end)\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lineDiff([]byte(tt.want), []byte(tt.got)); got != tt.diff {
				t.Errorf("lineDiff() =\n%s\nwant\n%s", got, tt.diff)
			}
		})
	}
}

func TestUpdating(t *testing.T) {
	tests := []struct {
		name       string
		update, ci string
		want       bool
		wantErr    bool
	}{
		{name: "off", want: false},
		{name: "on", update: "1", want: true},
		{name: "refused in CI", update: "1", ci: "true", wantErr: true},
		{name: "CI without update", ci: "true", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvUpdate, tt.update)
			t.Setenv("CI", tt.ci)
			got, err := updating()
			if (err != nil) != tt.wantErr {
				t.Fatalf("updating() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("updating() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAssertExample exercises the public API against this package's own
// testdata, the way generator tests will use it.
func TestAssertExample(t *testing.T) {
	Assert(t, "example", []byte("golden files hold expected output.\n"))
}
