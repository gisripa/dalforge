package playground

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gisripa/dalforge/internal/pipeline"
)

// TestPresets: every preset runs; the clean ones generate without errors,
// and the mistakes preset reports the core rules its comments promise.
func TestPresets(t *testing.T) {
	presets, err := Presets()
	if err != nil {
		t.Fatal(err)
	}
	if len(presets) < 5 {
		t.Fatalf("found %d presets, want at least 5", len(presets))
	}
	s := New("") // dalforge only; sqlc is covered by TestRunWithSQLC
	for _, p := range presets {
		t.Run(p.Name, func(t *testing.T) {
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
						t.Errorf("mistakes preset doesn't report %s; got %v", rule, resp.Diagnostics)
					}
				}
				return
			}
			if len(errs) > 0 {
				t.Fatalf("preset has errors: %+v", resp.Diagnostics)
			}
			if !slices.ContainsFunc(resp.Files, func(f File) bool { return f.Path == "schema/schema.sql" }) {
				t.Errorf("no schema/schema.sql among %d files", len(resp.Files))
			}
			if strings.HasPrefix(p.Title, "//") || p.Title == "" {
				t.Errorf("title %q: want the first comment line without //", p.Title)
			}
		})
	}
}

func TestLoadErrorsArePositioned(t *testing.T) {
	resp, err := New("").Run(context.Background(), "syntax = \"proto3\";\npackage x.v1;\nmessage A {\n  string a = 1\n}\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Diagnostics) == 0 || resp.Diagnostics[0].Line < 4 || resp.Diagnostics[0].Severity != "error" {
		t.Errorf("diagnostics = %+v, want an error at line 4 or later", resp.Diagnostics)
	}
	if len(resp.Files) != 0 {
		t.Errorf("files = %d, want none", len(resp.Files))
	}
}

// TestRunWithSQLC runs every clean preset through sqlc too, when sqlc is
// installed (it is under mise): sqlc must accept what dalforge generates.
func TestRunWithSQLC(t *testing.T) {
	sqlc, err := pipeline.FindSQLC()
	if err != nil {
		t.Skip("sqlc not on PATH")
	}
	presets, err := Presets()
	if err != nil {
		t.Fatal(err)
	}
	s := New(sqlc)
	for _, p := range presets {
		if p.Name == "mistakes" {
			continue
		}
		t.Run(p.Name, func(t *testing.T) {
			t.Parallel()
			resp, err := s.Run(context.Background(), p.Source)
			if err != nil {
				t.Fatal(err)
			}
			if resp.SQLC != "" {
				t.Fatalf("sqlc: %s", resp.SQLC)
			}
			if !slices.ContainsFunc(resp.Files, func(f File) bool { return f.Generator == "sqlc" && f.Path == "gen/sqlcdb/models.go" }) {
				t.Errorf("no sqlc models among %d files", len(resp.Files))
			}
			for _, f := range resp.Files {
				if strings.Contains(f.Content, "pgtype.") {
					t.Errorf("%s mentions pgtype; a type override is missing", f.Path)
				}
			}
		})
	}
}

func TestHTTP(t *testing.T) {
	srv := httptest.NewServer(New(""))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Errorf("GET / = %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}

	for _, asset := range []string{"/static/highlight.min.js", "/static/protobuf.min.js", "/static/github.min.css", "/static/github-dark.min.css"} {
		res, err := http.Get(srv.URL + asset)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", asset, res.StatusCode)
		}
	}

	res, err = http.Get(srv.URL + "/api/presets")
	if err != nil {
		t.Fatal(err)
	}
	var presets []Preset
	if err := json.NewDecoder(res.Body).Decode(&presets); err != nil || len(presets) == 0 {
		t.Errorf("GET /api/presets: %v, %d presets", err, len(presets))
	}
	_ = res.Body.Close()

	body, _ := json.Marshal(Request{Source: presets[0].Source})
	res, err = http.Post(srv.URL+"/api/generate", "application/json", strings.NewReader(string(body)))
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
