package main

// Ticket 20261016 finding 5: `go install .../cmd/quotactl@vX.Y.Z` (no ldflags)
// printed "version dev (commit unknown, built unknown)" — the version a client
// must quote to support was never in the binary. RED on v0.1.9 (no
// resolveBuildInfo: the module version in the build info was ignored).

import (
	"runtime/debug"
	"testing"
)

func TestResolveBuildInfo_GoInstallBuild_20261016(t *testing.T) {
	bi := &debug.BuildInfo{
		Main: debug.Module{Path: "github.com/ab0t-com/ab0t-quota-go", Version: "v0.1.10"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef0123"},
			{Key: "vcs.time", Value: "2026-10-16T00:00:00Z"},
		},
	}
	v, c, tm := resolveBuildInfo("dev", "unknown", "unknown", bi, true)
	if v != "v0.1.10" || c != "0123456789ab" || tm != "2026-10-16T00:00:00Z" {
		t.Fatalf("got %q %q %q", v, c, tm)
	}
}

func TestResolveBuildInfo_LdflagsWinAndDevelStaysDev_20261016(t *testing.T) {
	bi := &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}}
	if v, _, _ := resolveBuildInfo("v0.1.10", "abc", "t", bi, true); v != "v0.1.10" {
		t.Fatalf("ldflags version overridden: %q", v)
	}
	if v, _, _ := resolveBuildInfo("dev", "unknown", "unknown", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true); v != "dev" {
		t.Fatalf("(devel) build: %q, want dev", v)
	}
	if v, _, _ := resolveBuildInfo("dev", "unknown", "unknown", nil, false); v != "dev" {
		t.Fatalf("no build info: %q", v)
	}
}
