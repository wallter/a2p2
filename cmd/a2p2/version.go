package main

import "runtime/debug"

func init() { version = resolveVersion(version, debug.ReadBuildInfo) }

// resolveVersion keeps a version given by -ldflags. Without one (a plain
// `go install github.com/wallter/a2p2/cmd/a2p2@v1.2.3` or `go build`), it
// is the module version the go command recorded in the binary: the tag
// for an installed release, a pseudo-version for a commit.
func resolveVersion(ldflags string, read func() (*debug.BuildInfo, bool)) string {
	if ldflags != "dev" {
		return ldflags
	}
	if bi, ok := read(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return ldflags
}
