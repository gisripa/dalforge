package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/gisripa/dalforge/internal/config"
	"github.com/gisripa/dalforge/internal/diag"
	"github.com/gisripa/dalforge/internal/idl"
	"github.com/gisripa/dalforge/internal/ir"
	"github.com/gisripa/dalforge/internal/lock"
)

// RuleVendored reports a vendored copy of the dal options that differs from
// the ones bundled in the binary (which dalforge always uses).
const RuleVendored = "DAL116"

// Version is this binary's version: the release stamped by `go install
// …@v0.3.1`, or lock.Devel for anything else.
func Version() string {
	if _version != "" {
		return releaseVersion(_version)
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return lock.Devel
	}
	return releaseVersion(info.Main.Version)
}

// _version is set by release builds (GoReleaser:
// -X github.com/gisripa/dalforge/internal/pipeline._version=v1.2.3). Builds
// without it fall back to the module version Go stamps from build info.
var _version string

// _pseudo matches the commit stamp of a Go pseudo-version, e.g.
// "v0.0.0-20261002040003-851063fe8fa3", optionally "+dirty".
var _pseudo = regexp.MustCompile(`\d{14}-[0-9a-f]{12}(\+dirty)?$`)

// releaseVersion keeps a real release and maps everything else to
// lock.Devel: "(devel)", and the pseudo-versions Go stamps on builds from a
// source checkout (which change with every commit, so pinning them would
// make the lock churn).
func releaseVersion(v string) string {
	if v == "" || v == "(devel)" || strings.HasSuffix(v, "+dirty") || _pseudo.MatchString(v) {
		return lock.Devel
	}
	return v
}

// CurrentLock describes the running toolchain. sqlc is the sqlc binary to
// ask for its version; empty skips it (lint doesn't need sqlc).
func CurrentLock(ctx context.Context, sqlc string) (lock.Lock, error) {
	l := lock.Lock{Dalforge: Version(), Options: idl.BundledHashes(), Tools: map[string]string{}}
	if sqlc != "" {
		out, err := exec.CommandContext(ctx, sqlc, "version").Output()
		if err != nil {
			return lock.Lock{}, fmt.Errorf("sqlc version: %w", err)
		}
		l.Tools["sqlc"] = strings.TrimSpace(string(out))
	}
	return l, nil
}

// LockStatus compares the project's dalforge.lock with the running
// toolchain.
type LockStatus struct {
	Exists bool
	Diffs  []string
	// Lenient is true when the running dalforge is a local build: mismatches
	// are then warnings, so working on dalforge itself isn't blocked.
	Lenient bool
}

// CheckLock compares dalforge.lock with current.
func CheckLock(cfg *config.Config, current lock.Lock) (LockStatus, error) {
	pinned, ok, err := lock.Read(cfg.Path(lock.File))
	if err != nil || !ok {
		return LockStatus{}, err
	}
	return LockStatus{Exists: true, Diffs: lock.Diff(pinned, current), Lenient: current.Dalforge == lock.Devel}, nil
}

// WriteLock writes current as the project's dalforge.lock.
func WriteLock(cfg *config.Config, current lock.Lock) error {
	return os.WriteFile(cfg.Path(lock.File), current.Marshal(), 0o644)
}

// vendored reports vendored copies of the bundled options under the proto
// roots that differ from the bundled ones (DAL116, a warning: the bundled
// options are always the ones used).
func vendored(ctx context.Context, cfg *config.Config) (diag.List, error) {
	var out diag.List
	bundled := idl.BundledHashes()
	for _, root := range cfg.Proto.Roots {
		for _, p := range idl.BundledPaths() {
			file := filepath.Join(cfg.Path(root), filepath.FromSlash(p))
			if _, err := os.Stat(file); err != nil {
				continue
			}
			h, err := idl.VendoredHash(ctx, p, cfg.Path(root))
			if err != nil {
				return nil, err
			}
			if h != bundled[p] {
				out.Add(RuleVendored, diag.Warning, ir.Pos{File: filepath.ToSlash(filepath.Join(root, p)), Line: 1, Col: 1},
					"this copy of %s differs from the one bundled in dalforge %s, which is what dalforge uses; refresh or remove the copy", p, Version())
			}
		}
	}
	return out, nil
}
