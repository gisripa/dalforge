// Package playground serves a local web page for trying the IDL: edit a
// proto file and see the lint findings and everything dalforge and sqlc
// generate from it. Each request runs the same pipeline as `dalforge
// generate`, in a throwaway project directory.
//
// It's meant for localhost: the server binds to the loopback address and runs
// sqlc on whatever the browser sends.
package playground

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gisripa/dalforge/internal/config"
	"github.com/gisripa/dalforge/internal/pipeline"
)

//go:embed index.html
var _index []byte

//go:embed presets/*.proto
var _presets embed.FS

// _static holds the page's vendored assets: highlight.js (BSD-3-Clause, see
// static/LICENSE-highlight.js) with its protobuf grammar and GitHub themes.
// They're embedded rather than loaded from a CDN so the page works offline.
//
//go:embed static
var _static embed.FS

const (
	_maxSource = 256 << 10 // bytes of proto source per request
	_timeout   = 30 * time.Second
	// _protoFile is where the edited source is written in the throwaway
	// project; the proto package inside it decides the DAL package path.
	_protoFile = "proto/playground.proto"
	_config    = "version: 1\nmodule: example.com/playground\nbackend: pg\n"
)

// Server is the playground's HTTP handler.
type Server struct {
	sqlc string // sqlc binary; empty skips sqlc's output
	mux  *http.ServeMux
}

// New returns the playground handler. sqlc is the sqlc binary to run on the
// generated configuration; empty shows dalforge's output only.
func New(sqlc string) *Server {
	s := &Server{sqlc: sqlc, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /{$}", s.index)
	s.mux.Handle("GET /static/", http.FileServerFS(_static))
	s.mux.HandleFunc("GET /api/presets", s.presets)
	s.mux.HandleFunc("POST /api/generate", s.generate)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(_index)
}

// Preset is an example IDL offered in the page.
type Preset struct {
	Name   string `json:"name"`   // file name without the order prefix, e.g. "basics"
	Title  string `json:"title"`  // the first comment line, e.g. "Basics: one entity, …"
	Source string `json:"source"` // the proto file
}

// Presets returns the bundled examples, in their file-name order.
func Presets() ([]Preset, error) {
	names, err := fs.Glob(_presets, "presets/*.proto")
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	out := make([]Preset, 0, len(names))
	for _, n := range names {
		src, err := _presets.ReadFile(n)
		if err != nil {
			return nil, err
		}
		base := strings.TrimSuffix(path.Base(n), ".proto")
		if _, rest, ok := strings.Cut(base, "-"); ok {
			base = rest
		}
		first, _, _ := strings.Cut(string(src), "\n")
		out = append(out, Preset{Name: base, Title: strings.TrimPrefix(first, "// "), Source: string(src)})
	}
	return out, nil
}

func (s *Server) presets(w http.ResponseWriter, _ *http.Request) {
	p, err := Presets()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, p)
}

// Request is what the page sends to generate.
type Request struct {
	Source string `json:"source"`
}

// Diagnostic is one lint finding or load error, positioned in the source.
type Diagnostic struct {
	Line     int    `json:"line"`
	Col      int    `json:"col"`
	Severity string `json:"severity"` // error, warning or info
	Rule     string `json:"rule,omitempty"`
	Message  string `json:"message"`
}

// File is one generated file.
type File struct {
	Path      string `json:"path"`
	Generator string `json:"generator"` // "dalforge" or "sqlc"
	Content   string `json:"content"`
}

// Response is the result of one run.
type Response struct {
	Diagnostics []Diagnostic `json:"diagnostics"`
	Files       []File       `json:"files"`
	// SQLC explains why sqlc's output is missing, when it is: sqlc isn't
	// installed, there are no queries, or sqlc reported an error.
	SQLC string `json:"sqlc,omitempty"`
}

func (s *Server) generate(w http.ResponseWriter, r *http.Request) {
	var req Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, _maxSource+1024)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), _timeout)
	defer cancel()
	resp, err := s.Run(ctx, req.Source)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

// Run generates from one proto source, in a throwaway project directory.
// Problems in the source come back as diagnostics; the error is reserved for
// the playground itself failing.
func (s *Server) Run(ctx context.Context, source string) (*Response, error) {
	if len(source) > _maxSource {
		return &Response{Diagnostics: []Diagnostic{{Line: 1, Col: 1, Severity: "error", Message: "the source is larger than the playground accepts"}}}, nil
	}
	dir, err := os.MkdirTemp("", "dalforge-playground-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := writeFile(filepath.Join(dir, "dalforge.yaml"), _config); err != nil {
		return nil, err
	}
	if err := writeFile(filepath.Join(dir, filepath.FromSlash(_protoFile)), source); err != nil {
		return nil, err
	}
	cfg, err := config.Load(filepath.Join(dir, "dalforge.yaml"))
	if err != nil {
		return nil, err
	}

	resp := &Response{Diagnostics: []Diagnostic{}, Files: []File{}}
	plan, err := pipeline.Build(ctx, cfg)
	if err != nil {
		resp.Diagnostics = loadErrors(err)
		return resp, nil
	}
	for _, d := range plan.Diags {
		resp.Diagnostics = append(resp.Diagnostics, Diagnostic{
			Line: d.Pos.Line, Col: d.Pos.Col, Severity: d.Severity.String(), Rule: d.Rule, Message: d.Message,
		})
	}
	if plan.Diags.HasErrors() {
		return resp, nil
	}
	for _, p := range slices.Sorted(maps.Keys(plan.Files)) {
		resp.Files = append(resp.Files, File{Path: p, Generator: "dalforge", Content: string(plan.Files[p])})
	}

	if s.sqlc == "" {
		resp.SQLC = "sqlc isn't on PATH, so sqlc's Go output (models, queries) isn't shown. Run the playground with `mise run playground`."
		return resp, nil
	}
	sum, err := pipeline.Generate(ctx, cfg, plan, s.sqlc)
	switch {
	case err != nil:
		resp.SQLC = err.Error()
		return resp, nil
	case !sum.RanSQLC:
		resp.SQLC = sum.SQLCNote
		return resp, nil
	}
	sqlcDir := filepath.Join(dir, filepath.FromSlash(cfg.Out.SQLC))
	err = filepath.WalkDir(sqlcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		resp.Files = append(resp.Files, File{Path: filepath.ToSlash(rel), Generator: "sqlc", Content: string(b)})
		return nil
	})
	return resp, err
}

// _loadError matches one structural error from the loader:
// "proto/playground.proto:12:3: message".
var _loadError = regexp.MustCompile(`^(?:[^:]*\.proto):(\d+):(\d+): (.*)$`)

// loadErrors turns the loader's error (one line per problem) into
// diagnostics; lines without a position are reported at the top.
func loadErrors(err error) []Diagnostic {
	var out []Diagnostic
	sc := bufio.NewScanner(strings.NewReader(err.Error()))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		d := Diagnostic{Line: 1, Col: 1, Severity: "error", Message: line}
		if m := _loadError.FindStringSubmatch(line); m != nil {
			d.Line, _ = strconv.Atoi(m[1])
			d.Col, _ = strconv.Atoi(m[2])
			d.Message = m[3]
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		out = append(out, Diagnostic{Line: 1, Col: 1, Severity: "error", Message: err.Error()})
	}
	return out
}

func writeFile(p, content string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "playground: write response:", err)
	}
}
