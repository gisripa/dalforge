package pipeline

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gisripa/dalforge/internal/idl"
	"github.com/gisripa/dalforge/internal/lock"
)

func TestCheckLock(t *testing.T) {
	cfg := project(t, _yaml, "orders/v1/orders.proto")
	release := lock.Lock{Dalforge: "v0.3.1", Options: idl.BundledHashes(), Tools: map[string]string{"sqlc": "v1.31.1"}}

	if st, err := CheckLock(cfg, release); err != nil || st.Exists {
		t.Fatalf("no lock yet: %+v, %v", st, err)
	}
	if err := WriteLock(cfg, release); err != nil {
		t.Fatal(err)
	}
	if st, _ := CheckLock(cfg, release); !st.Exists || len(st.Diffs) != 0 {
		t.Errorf("same toolchain: %+v, want no diffs", st)
	}

	newer := release
	newer.Dalforge = "v0.4.0"
	if st, _ := CheckLock(cfg, newer); len(st.Diffs) != 1 || st.Lenient {
		t.Errorf("different release: %+v, want one strict diff", st)
	}

	devel := release
	devel.Dalforge = lock.Devel
	if st, _ := CheckLock(cfg, devel); len(st.Diffs) != 1 || !st.Lenient {
		t.Errorf("local build: %+v, want a lenient diff", st)
	}
}

func TestVendoredOptions(t *testing.T) {
	cfg := project(t, _yaml, "orders/v1/orders.proto")
	src, err := os.ReadFile("../../proto/dal/v1/options.proto")
	if err != nil {
		t.Fatal(err)
	}
	vendor := cfg.Path("proto/dal/v1/options.proto")

	write(t, vendor, string(src)) // an identical copy: silent
	plan, err := Build(context.Background(), cfg)
	if err != nil || strings.Contains(plan.Diags.String(), RuleVendored) {
		t.Errorf("identical copy: %v\n%s", err, plan.Diags)
	}

	write(t, vendor, strings.Replace(string(src), "bool unique = 4;", "bool unique = 4;\n  bool frozen = 7;", 1))
	plan, err = Build(context.Background(), cfg)
	if err != nil || !strings.Contains(plan.Diags.String(), "warning DAL116") || len(plan.Files) == 0 {
		t.Errorf("stale copy: want a DAL116 warning and generation to continue; %v\n%s", err, plan.Diags)
	}
}

func TestReleaseVersion(t *testing.T) {
	for in, want := range map[string]string{
		"v0.3.1":                             "v0.3.1",
		"v1.2.0-rc.1":                        "v1.2.0-rc.1",
		"(devel)":                            lock.Devel,
		"":                                   lock.Devel,
		"v0.0.0-20261002040003-851063fe8fa3": lock.Devel,
		"v0.0.0-20261002040003-851063fe8fa3+dirty": lock.Devel,
		"v0.3.2-0.20261002040003-851063fe8fa3":     lock.Devel,
		"v0.3.1+dirty":                             lock.Devel,
	} {
		if got := releaseVersion(in); got != want {
			t.Errorf("releaseVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
