package main

import (
	"runtime/debug"
	"testing"
)

func TestVersionFallsBackToTheModuleVersionWithoutLdflags(t *testing.T) {
	info := func(v string) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Path: "github.com/wallter/a2p2", Version: v}}, true
		}
	}
	none := func() (*debug.BuildInfo, bool) { return nil, false }
	for _, c := range []struct {
		name, ldflags string
		read          func() (*debug.BuildInfo, bool)
		want          string
	}{
		{"ldflags win", "v1.2.3-4-gabc", info("v9.9.9"), "v1.2.3-4-gabc"},
		{"go install @tag", "dev", info("v0.3.0"), "v0.3.0"},
		{"go install @commit", "dev", info("v0.0.0-20261124000000-abcdef123456"), "v0.0.0-20261124000000-abcdef123456"},
		{"built without vcs", "dev", info("(devel)"), "dev"},
		{"empty", "dev", info(""), "dev"},
		{"no build info", "dev", none, "dev"},
	} {
		if got := resolveVersion(c.ldflags, c.read); got != c.want {
			t.Errorf("%s: resolveVersion(%q) = %q, want %q", c.name, c.ldflags, got, c.want)
		}
	}
}
