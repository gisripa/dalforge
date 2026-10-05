package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
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
	// _protoFile is where the edited source goes in the throwaway project;
	// the proto package inside it decides the DAL package path.
	_protoFile = "proto/playground.proto"
	_config    = "version: 1\nmodule: example.com/playground\nbackend: pg\n"
	// _sqlcOut is sqlc's output directory under the default dalforge.yaml.
	_sqlcOut = "gen/sqlcdb/"
)

// Server is the playground's HTTP handler. It runs the dalforge CLI, exactly
// as a user's project would, so the page shows real output and can never
// drift from the released tool.
type Server struct {
	dalforge string // the dalforge binary
	version  string // its `dalforge version` output
	mux      *http.ServeMux
}

// NewServer returns the playground handler, running the given dalforge
// binary. dalforge finds sqlc on PATH itself.
func NewServer(ctx context.Context, dalforge string) (*Server, error) {
	out, err := exec.CommandContext(ctx, dalforge, "version").Output()
	if err != nil {
		return nil, fmt.Errorf("run %s version: %w", dalforge, err)
	}
	s := &Server{dalforge: dalforge, version: strings.TrimSpace(string(out)), mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /{$}", s.index)
	s.mux.Handle("GET /static/", http.FileServerFS(_static))
	s.mux.HandleFunc("GET /api/info", s.info)
	s.mux.HandleFunc("GET /api/presets", s.presets)
	s.mux.HandleFunc("POST /api/generate", s.generate)
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(_index)
}

func (s *Server) info(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"dalforge": s.version})
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

// Diagnostic is one lint finding or load error in the source.
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
	// Note explains a failure that isn't about the source, such as sqlc
	// missing or rejecting the generated SQL.
	Note string `json:"note,omitempty"`
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

// cliReport is the part of `dalforge generate -json`'s report the playground
// reads (see the CLI's Report type).
type cliReport struct {
	Diagnostics []struct {
		File     string `json:"file"`
		Line     int    `json:"line"`
		Col      int    `json:"col"`
		Severity string `json:"severity"`
		Rule     string `json:"rule"`
		Message  string `json:"message"`
	} `json:"diagnostics"`
	Generated *struct {
		RanSQLC  bool   `json:"ran_sqlc"`
		SQLCNote string `json:"sqlc_note"`
	} `json:"generated"`
	Error string `json:"error"`
}

// Run generates from one proto source: it writes a throwaway project, runs
// `dalforge generate -json` in it, and returns the findings and every file
// generated. Problems in the source come back as diagnostics; the error is
// reserved for the playground itself failing.
func (s *Server) Run(ctx context.Context, source string) (*Response, error) {
	resp := &Response{Diagnostics: []Diagnostic{}, Files: []File{}}
	if len(source) > _maxSource {
		resp.Diagnostics = append(resp.Diagnostics, Diagnostic{Line: 1, Col: 1, Severity: "error", Message: "the source is larger than the playground accepts"})
		return resp, nil
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

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, s.dalforge, "generate", "-json", "-config", filepath.Join(dir, "dalforge.yaml"))
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var rep cliReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		// Not a report: dalforge itself failed (or isn't a version with -json).
		return nil, fmt.Errorf("dalforge generate: %v: %s", errors.Join(runErr, err), strings.TrimSpace(stderr.String()))
	}
	for _, d := range rep.Diagnostics {
		resp.Diagnostics = append(resp.Diagnostics, Diagnostic{Line: max(d.Line, 1), Col: max(d.Col, 1), Severity: d.Severity, Rule: d.Rule, Message: d.Message})
	}
	resp.Note = rep.Error
	if rep.Generated == nil {
		return resp, nil
	}
	if !rep.Generated.RanSQLC {
		resp.Note = "sqlc didn't run: " + rep.Generated.SQLCNote
	}
	files, err := generatedFiles(dir)
	if err != nil {
		return nil, err
	}
	resp.Files = files
	return resp, nil
}

// generatedFiles reads every file dalforge and sqlc wrote into dir, skipping
// the inputs and the lock.
func generatedFiles(dir string) ([]File, error) {
	var out []File
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "dalforge.yaml" || rel == "dalforge.lock" || strings.HasPrefix(rel, "proto/") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		gen := "dalforge"
		if strings.HasPrefix(rel, _sqlcOut) {
			gen = "sqlc"
		}
		out = append(out, File{Path: rel, Generator: gen, Content: string(b)})
		return nil
	})
	slices.SortFunc(out, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	return out, err
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
