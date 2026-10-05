package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// server returns a playground running the dalforge on PATH. The tests drive
// the real CLI and sqlc, as the playground does; `mise run test` builds
// dalforge and puts both on PATH.
func server(t *testing.T) *Server {
	t.Helper()
	for _, bin := range []string{"dalforge", "sqlc"} {
		if _, err := exec.LookPath(bin); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("%s not on PATH in CI", bin)
			}
			t.Skipf("%s not on PATH; run the tests through mise", bin)
		}
	}
	s, err := NewServer(context.Background(), "dalforge")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestPresets: the clean presets generate without findings, sqlc accepts
// their output and it never mentions pgtype; the mistakes preset reports the
// core rules its comments promise.
func TestPresets(t *testing.T) {
	s := server(t)
	presets, err := Presets()
	if err != nil {
		t.Fatal(err)
	}
	if len(presets) < 5 {
		t.Fatalf("found %d presets, want at least 5", len(presets))
	}
	for _, p := range presets {
		t.Run(p.Name, func(t *testing.T) {
			t.Parallel()
			if strings.HasPrefix(p.Title, "//") || p.Title == "" {
				t.Errorf("title %q: want the first comment line without //", p.Title)
			}
			resp, err := s.Run(context.Background(), p.Source)
			if err != nil {
				t.Fatal(err)
			}
			var errs []string
			for _, d := range resp.Diagnostics {
				t.Logf("%d:%d %s %s %s", d.Line, d.Col, d.Severity, d.Rule, d.Message)
				if d.Severity == "error" {
					errs = append(errs, d.Rule)
				}
			}
			if p.Name == "mistakes" {
				for _, rule := range []string{"DAL101", "DAL115", "DAL117", "DAL119"} {
					if !slices.Contains(errs, rule) {
						t.Errorf("mistakes preset doesn't report %s", rule)
					}
				}
				return
			}
			if len(resp.Diagnostics) > 0 || resp.Note != "" {
				t.Fatalf("diagnostics %+v, note %q; want a clean run", resp.Diagnostics, resp.Note)
			}
			for _, want := range []string{"schema/schema.sql", "sqlc.yaml", "gen/sqlcdb/models.go"} {
				if !slices.ContainsFunc(resp.Files, func(f File) bool { return f.Path == want }) {
					t.Errorf("%s not among the generated files", want)
				}
			}
			for _, f := range resp.Files {
				if f.Generator == "sqlc" && !strings.HasPrefix(f.Path, "gen/sqlcdb/") {
					t.Errorf("%s attributed to sqlc", f.Path)
				}
				if strings.Contains(f.Content, "pgtype.") {
					t.Errorf("%s mentions pgtype; a type override is missing", f.Path)
				}
			}
		})
	}
}

func TestSyntaxErrorIsPositioned(t *testing.T) {
	resp, err := server(t).Run(context.Background(), "syntax = \"proto3\";\npackage x.v1;\nmessage A {\n  string a = 1\n}\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Diagnostics) == 0 || resp.Diagnostics[0].Line < 4 || resp.Diagnostics[0].Severity != "error" || len(resp.Files) != 0 {
		t.Errorf("diagnostics %+v, %d files; want an error at line 4 or later and no files", resp.Diagnostics, len(resp.Files))
	}
}

func TestHTTP(t *testing.T) {
	srv := httptest.NewServer(server(t))
	defer srv.Close()

	get := func(path string) *http.Response {
		t.Helper()
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = res.Body.Close() })
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", path, res.StatusCode)
		}
		return res
	}
	if ct := get("/").Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET / content type %q", ct)
	}
	for _, asset := range []string{"/static/highlight.min.js", "/static/protobuf.min.js", "/static/github.min.css", "/static/github-dark.min.css"} {
		get(asset)
	}
	var info map[string]string
	if err := json.NewDecoder(get("/api/info").Body).Decode(&info); err != nil || !strings.HasPrefix(info["dalforge"], "dalforge ") {
		t.Errorf("GET /api/info = %v, %v", info, err)
	}
	var presets []Preset
	if err := json.NewDecoder(get("/api/presets").Body).Decode(&presets); err != nil || len(presets) == 0 {
		t.Fatalf("GET /api/presets: %v, %d presets", err, len(presets))
	}

	body, _ := json.Marshal(Request{Source: presets[0].Source})
	res, err := http.Post(srv.URL+"/api/generate", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	var out Response
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || len(out.Files) == 0 {
		t.Errorf("POST /api/generate: %v, %d files", err, len(out.Files))
	}
	_ = res.Body.Close()

	res, err = http.Post(srv.URL+"/api/generate", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("bad JSON: status %d, want 400", res.StatusCode)
	}
}
